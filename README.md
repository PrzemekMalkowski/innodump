# 🗄️ ibd-extractor

`ibd-extractor` is an offline reader for a single MySQL InnoDB tablespace
file (`table.ibd`, file-per-table mode) — MySQL 8.0/8.4, MySQL 5.6/5.7 or
MariaDB given the table's `.frm` file alongside it (see "MySQL 5.6/5.7
(.frm) support" and "MariaDB support", below). Given `table.ibd`, it
produces:

- `table-schema.sql` — a best-effort `CREATE TABLE` statement, reconstructed
  from the table's own embedded dictionary information (SDI), or its `.frm`
  file for a pre-8.0 table.
- `table-data.sql` — one `INSERT INTO ... VALUES (...);` statement per live
  row, decoded by walking the table's clustered index (its B+tree) directly
  in the file.

It reads the file directly and does not connect to a running server, so
it's safe to point at a copy of a `.ibd` file from a node that is up or
down (as long as the copy is consistent — e.g. taken while the server was
stopped, or via `FLUSH TABLES ... FOR EXPORT`).

## How it works (brief)

Since MySQL 8.0, every InnoDB tablespace carries a compressed JSON copy of
its own table definition — the Serialized Dictionary Information (SDI) —
in a small hidden index inside the file itself. `ibd-extractor` locates
that index, decompresses the JSON, and uses it to learn every column's
type, nullability, and physical storage layout, plus which page starts the
table's clustered (PRIMARY KEY) index.

It then walks that index's B+tree from the root to the leftmost leaf page,
across every leaf page via the sibling-page pointers, decoding each row's
NULL bitmap and variable-length field list exactly as InnoDB itself does.
Columns stored off-page (long BLOB/TEXT/JSON values) are followed through
MySQL 8.0's LOB page format to recover the full value.

## Build

Requires Go 1.21+.

```sh
go build -o ibd-extractor .
```

## Usage

```
ibd-extractor --file /path/to/table.ibd [options]
```

| Flag | Description |
|------|-------------|
| `--file PATH` | Path to the `.ibd` file (required). |
| `--out-dir DIR` | Directory for the two output files (default: the current directory). |
| `--table NAME` | Which table to extract, if the file's SDI unexpectedly holds more than one. |
| `--limit N` | Stop after N rows (0 = all). |
| `--ddl-only` | Only write the schema file; skip walking the table's data entirely. |
| `--skip-corrupted` | On a corrupted leaf page, note it and carry on from the next page instead of stopping. |
| `--no-progress` | Never draw the progress bar on stderr (also honored via `NO_PROGRESS=1`). |
| `--verbose` | Print extra file/table details: page size, FSP flags, the MySQL version and dictionary/SDI versions that wrote the file, and index/column counts. |
| `--debug` | Print each record's decoded field byte-ranges as they're read. |
| `--dump-page N` | Hex-dump one page and its record chain, then exit. |
| `--version` | Print version and exit. |

## Scope (v1)

Supported: MySQL **8.0.16+ and 8.4.x** tablespaces, `ROW_FORMAT=DYNAMIC`,
`COMPACT`, `REDUNDANT`, or `COMPRESSED`, non-partitioned, file-per-table
`.ibd` files — including tables with `INSTANT ADD COLUMN` / `INSTANT DROP
COLUMN` history, both MySQL's original (8.0.12-8.0.28) mechanism and the
row-versioning one that superseded it in 8.0.29+ (see "INSTANT ADD/DROP
COLUMN", below) — plus **MySQL 5.6/5.7**, given the table's `.frm` file
alongside its `.ibd` (see "MySQL 5.6/5.7 (.frm) support", below).
`ROW_FORMAT=REDUNDANT` uses the pre-5.0.3 "old-style" record layout (a
per-field cumulative-offset array instead of a NULL bitmap + variable-length
list), decoded by `redundant.go`; it is what a 5.6/5.7 table (or an
8.0+ one never rebuilt since an upgrade) will still have. Row
versioning is not combined with `REDUNDANT` in practice (a table can't
gain `INSTANT ADD/DROP COLUMN` history without also being converted off
`REDUNDANT` first), so that specific — vanishingly rare — combination is
detected and rejected rather than guessed at, and the same goes for
`COMPRESSED` combined with either instant mechanism. Each of the
remaining exclusions is checked explicitly — the tool refuses the table
with a specific reason rather than guessing.

`ROW_FORMAT=COMPRESSED` (any `KEY_BLOCK_SIZE`) is decoded by
`zipdecompress.go`: every `FIL_PAGE_INDEX`/`FIL_PAGE_SDI` page is
reconstructed to its full logical size before anything else in this tool
ever sees it, in the same three passes InnoDB itself uses to read one -
inflate the page's zlib-compressed image, replay its modification log
(the delta patch InnoDB appends instead of recompressing on every write -
in practice this is where *most* of a compressed page's real row content
usually lives, not the image), then restore `DB_TRX_ID`/`DB_ROLL_PTR`,
node pointers, and BLOB references from the page's own dedicated storage
area. One thing this doesn't cover: a `COMPRESSED` table's off-page
(external) column values use a different, separate on-disk format
(`FIL_PAGE_TYPE_ZBLOB`/`ZBLOB2`) that this tool doesn't read - a row with
one is skipped with a warning, exactly like any other row this tool can't
decode, rather than the whole table being refused.

**Not supported in v1** (all detected and reported, not silently
mis-decoded):

- Encrypted tablespaces.
- Partitioned tables.
- **MariaDB** tablespaces using the classic (non-`full_crc32`)
  `innodb_checksum_algorithm` values (`innodb`/`crc32`/`none` -
  effectively any MariaDB server older than 10.4, or one that has
  explicitly turned `full_crc32` off), and MariaDB's own
  `page_compression` feature (an orthogonal, page-level compression
  scheme unrelated to `ROW_FORMAT=COMPRESSED`). See "MariaDB support",
  below, for what *is* covered.
- Full-text and spatial indexes are rendered into the DDL as best-effort
  `FULLTEXT KEY`/`SPATIAL KEY` clauses, but are never walked for data —
  only the clustered index is (all the data lives there regardless).

## MySQL 5.6/5.7 (`.frm`) support

MySQL didn't introduce SDI until 8.0 — a 5.6/5.7 tablespace carries no
embedded schema at all. Point this tool at a pre-8.0 `.ibd` file and it
looks for a `.frm` file with the same base name in the same directory
(`table.ibd` → `table.frm`) and parses that instead (`frm.go`), ported
from `open_binary_frm()`/`make_field_from_frm()` in the mysql-server
source and cross-checked byte-for-byte against real `.frm` files from
live MySQL 5.6.51 and 5.7.44 servers. Once parsed, it's turned into
exactly the same internal model the SDI path builds, so every other part
of this tool (the record decoder, the corruption handling, `--verbose`,
DDL generation) runs completely unchanged.

A `.frm` carries no InnoDB-internal physical detail (that lived in the
pre-8.0 internal data dictionary, inside `ibdata1`, which this tool has
no access to and no need for) — notably, no clustered index root page
number. This relies instead on a well-known InnoDB convention, confirmed
empirically: a fresh file-per-table tablespace's first-created index —
always the clustered index — gets root page 3. If that page isn't
actually an index page, extraction fails with a clear error rather than
guessing further; this is the one place a `.frm`-based extraction could
run into a table layout this convention doesn't hold for.

**Not supported for the `.frm` path** (on top of the exclusions above,
none of which are 5.6/5.7-specific): generated/virtual columns (5.7.6+;
rejected explicitly, since decoding them needs a section of the `.frm`
this tool doesn't read), views, and a `.frm` old enough to need a
"legacy field-position" reconciliation (`find_field()` in the original
source) — vanishingly rare for anything actually created on a live
5.6/5.7 server. Partitioned tables are *not* explicitly excluded here:
each partition is its own ordinary single-table tablespace file sharing
the one `.frm`, and pointing this tool at one partition's `.ibd` file
directly works the same way it would for any other table.

## MariaDB support

MariaDB is InnoDB under the hood but has no SDI, and its default
checksum format (`innodb_checksum_algorithm=full_crc32`, the default
since MariaDB 10.4) repurposes the very same `FSP_SPACE_FLAGS` bits
MySQL uses for `KEY_BLOCK_SIZE`/`zip_ssize` - a MariaDB tablespace with
those bits set is not actually compressed at all. `page.go` checks a
marker bit in the flags first and, when it's set, decodes the page size
from a different (and differently-special-cased: `ssize=5` means 16KiB,
not `ssize=0` as in classic mode) formula, and verifies pages with
MariaDB's own checksum scheme instead of MySQL's - a single CRC-32C over
the whole page except its own last 4 bytes, stored as a plain big-endian
`uint32` in those last 4 bytes (rather than split across the start and
end of the page, as classic MySQL checksums are), with the LSN
consistency check shifted to compare against the second-to-last 4 bytes
instead of the very last 4.

Once the page/checksum layer is sorted out, a MariaDB tablespace's
on-disk page and record format is identical to standard InnoDB's, so
none of the record-decoding logic needed any change. The `.frm` path
(MariaDB has never had SDI, so every MariaDB table goes through `.frm`
parsing, not just 5.6/5.7-style ones) also needed no MariaDB-specific
code: MariaDB's `.frm` wraps an "extra2" tagged section right after the
64-byte header - conceptually a different mechanism from MySQL's own
legacy names-table for locating the form-info block - but the field this
tool already reads to find that block (a 4-byte pointer immediately
after the header-adjacent blob whose length is `head[4:6]`) resolves to
the exact same offset under both schemes, so `frm.go`'s existing
MySQL-5.6/5.7-oriented logic carries over unmodified. Every other byte
offset downstream (form-info's own fields, the field-definition layout,
the key-info block) is unchanged between the two servers as well.

MariaDB also introduced a new family of collation IDs (`utf8mb4_uca1400_*`
and siblings, MariaDB's own default since 10.4, starting at ID 2048 in
256-ID blocks per charset) that aren't in the classic MySQL collation
table this tool otherwise relies on for CHAR/VARCHAR byte-width and
`DEFAULT CHARSET=` decisions; `collations.go` recognizes the range and
resolves it to the right charset (and thus the right column byte-width),
though - since a numeric ID alone can't say which of the many
language/pad-mode variants within a charset's block it is - the
generated DDL states only `DEFAULT CHARSET=...` for these, not the exact
`COLLATE=...`.

Validated end-to-end (extraction, then reload onto a real server, diffed
row-for-row) against MariaDB 11.6.2, uncompressed tables, `full_crc32`
checksums - both handwritten test tables and a real customer table
reported against this tool. Not yet validated: older MariaDB checksum
formats, and MariaDB's own `page_compression` feature (see "Scope (v1)",
above).

## INSTANT ADD/DROP COLUMN

A row's physical layout depends on which schema version it was written
under — a row inserted before a later `ADD COLUMN` simply doesn't have that
column's bytes at all, and one written before a later `DROP COLUMN` still
does. Each record's header carries enough information (see `instant.go`) to
work out, field by field, whether it's genuinely present, not yet added (use
the column's recorded instant default, or `NULL`), or already dropped
(entirely absent, contributing no bytes and no NULL-bitmap bit) — mirroring
`Record::GetInsertState`/`InitColumnOffsetsCompactLeaf` in `ibdNinja`.

A dropped column is naturally excluded from both the generated `CREATE
TABLE` and every `INSERT`, exactly as `SELECT *` on the live table would be.
- Pre-8.0 tablespaces (no embedded SDI to read at all).

## Output notes

- **DDL is best-effort.** Column types (including `unsigned`/`zerofill`),
  nullability, `AUTO_INCREMENT`, comments, generated-column expressions,
  the `PRIMARY KEY`, secondary indexes (`KEY`/`UNIQUE KEY`/`FULLTEXT
  KEY`/`SPATIAL KEY`, including prefix lengths, `DESC` order, and
  `INVISIBLE`), and the table's default charset/collation are reproduced
  from the SDI. `DEFAULT` clauses and other table-level options are not —
  add them by hand if you need an exact round-trip DDL, or restore
  alongside the original `SHOW CREATE TABLE` output if you have it.
- **`AUTO_INCREMENT=N`** is *not* in the SDI at all — the server tracks
  that counter separately, outside any single tablespace file. It's
  approximated as the highest value seen in the auto-increment column
  across the table's live rows, plus one. This is exact as long as nothing
  was ever deleted from the high end of the column (the common case); if
  rows near the current max were deleted, or the counter was bumped ahead
  manually (e.g. `ALTER TABLE ... AUTO_INCREMENT=N`) without inserting up
  to it, the real counter can be higher than this tool can recover from
  the file alone.
- **String columns** are emitted as their raw stored bytes inside a quoted
  SQL string literal. For `utf8mb3`/`utf8mb4` columns (the common case)
  this is already correct UTF-8. For any other character set, the bytes
  are still correct for reloading with a session in that same character
  set, but may not display correctly in a plain text editor.
- **BLOB/BINARY/VARBINARY/GEOMETRY** columns are emitted as `X'...'` hex
  literals, which is always safe and lossless regardless of content.
- **JSON** columns are decoded from MySQL's internal binary JSON
  representation back into JSON text.
- **ENUM/SET** columns are rendered using their real label text (not the
  internal integer), decoded from the SDI's own element list.
- A row that fails to decode (corrupt data, or an unsupported nested case)
  is skipped with a warning printed to stderr and a `-- skipped a row ...`
  comment in the data file, rather than aborting the whole extraction.

## Corrupted pages

Every leaf page is checksum-verified (the same CRC-32C algorithm and LSN
consistency check the server itself uses — see `page.go`'s `checkPage`)
before its records are read, and rejected if its page type or index id
doesn't match what's expected either. By default a corrupted page is fatal:
extraction stops immediately with the page number, index name/id, and the
specific reason (checksum mismatch, wrong page type, etc.) — silently
skipping past unreadable pages by default would make an incomplete dump
look like a complete one.

Pass `--skip-corrupted` to keep going instead: each corrupted page is noted
(a warning on stderr, and a `-- skipped corrupted page ...` comment with
the page number, table, index name/id, and reason in the data file) and the
walk tries to carry on using that page's own "next page" pointer, since
corruption is often localized to the page body and leaves the header
intact. If that pointer is itself missing or unusable, the walk ends there
(not as an error — you keep every row decoded up to that point). The final
summary line reports how many pages were skipped this way.

Both the `crc32` checksum algorithm (the default since MySQL 5.7, and
MariaDB's own `full_crc32` format - see "MariaDB support", above) and the
legacy `innodb` algorithm (MySQL 5.6's own default, a custom hash
predating `crc32` entirely) are verified. A `ROW_FORMAT=COMPRESSED` page's
checksum uses a different algorithm this tool doesn't implement, so only
the cheaper "is this an unallocated, all-zero page" check runs for one —
a genuinely corrupted compressed page will likely surface as a decode
error instead of a clean "checksum mismatch" one.

## Progress

When stderr is a real terminal, a spinner shows on it once an extraction
has been running long enough to be worth the flicker (fast runs finish
before it ever appears) — a percentage bar keyed off the tablespace's page
count, a running row count, and elapsed time. It's a rough proxy only: not
every page in the file belongs to the table being extracted, so the bar
may not reach 100% on a small table sharing a big tablespace, and it never
touches stdout or the output files either way. Pass `--no-progress` (or
set `NO_PROGRESS=1`) to suppress it, e.g. when running under something
that doesn't want carriage-return redraws in its log capture.

## Validation

This was cross-checked end to end against real MySQL 8.0.19, 8.0.46, and
8.4.11 servers: for every test table (plain types, every scalar type
including `DECIMAL`/`BIT`/`ENUM`/`SET`/`JSON`/all temporal types, off-page
BLOB/TEXT values up to 32 KB spanning multiple LOB pages, tables with no
explicit `PRIMARY KEY`, composite-key tables, a table with `AUTO_INCREMENT`
+ secondary indexes + deleted rows, an 8.0.19 table with the old
instant-add-only mechanism across two `ALTER TABLE ADD COLUMN`s, an
8.0.46 table combining `ADD` and `DROP COLUMN` across five row versions,
`ROW_FORMAT=REDUNDANT` tables both plain and with an off-page BLOB column,
and `ROW_FORMAT=COMPRESSED` tables at `KEY_BLOCK_SIZE=8` covering NULLs, a
table with about half its rows deleted (exercising the free-list/dense-
directory path), a table updated enough times in place to leave a
non-empty modification log, and a 3,000-row/multi-page table spanning a
non-leaf level whose root page's own content lives entirely in its
modification log rather than its compressed image), reloading the
generated `*-schema.sql` + `*-data.sql` into a fresh database and diffing
every row against the original table (`SELECT * FROM orig EXCEPT SELECT *
FROM reloaded`, both directions) came back empty. Also cross-checked
against the standard Sakila sample database's `rental` table (16,044 rows,
`ROW_FORMAT=COMPRESSED`, real production-shaped data) on Percona Server
8.4.8 — a real file surfaced a bug the synthetic fixtures hadn't (a
compressed leaf record's field-info-block entries don't map one-to-one to
its physical columns; see `tableZipShape`'s doc comment in
`zipdecompress.go`), fixed and reconfirmed against it and every existing
fixture.

The `.frm` path was validated the same way, against real MySQL 5.6.51 and
5.7.44 servers: `ROW_FORMAT=COMPACT`, `DYNAMIC`, and `COMPRESSED` tables
covering `INT`/`VARCHAR`/`DECIMAL`/`DATETIME`/`TINYINT`/`TEXT`/`ENUM`/`SET`,
a composite `PRIMARY KEY`, `UNIQUE`/plain secondary indexes, an
`AUTO_INCREMENT` column, `FLOAT UNSIGNED`, `NULL`s, and multi-byte
(`utf8mb4`) string data. Reload-and-diff came back byte-for-byte identical
in every case, including a real Percona Server 5.6.47 table using the
legacy `innodb` checksum algorithm (5.6's own default, and the first real
file this tool hand-verified that algorithm's two hash formulas against —
see "Corrupted pages").

Corruption handling was validated by flipping a byte in a real leaf page of
a 2,000-row, 72-leaf-page table: the default run stopped with the exact
page/index/reason, and `--skip-corrupted` recovered exactly the 1,972 rows
outside that one page (a single contiguous gap matching the corrupted
page's rows, confirmed against the original data), with no false positives
across any of the other real pages/tables tested.

## Acknowledgements

The on-disk page, record, SDI, and LOB formats were cross-referenced
against the public `mysql-server` source (`storage/innobase`) and against
[KernelMaker/ibdNinja](https://github.com/KernelMaker/ibdNinja) (GPL-3.0),
which was used throughout development to validate these layouts —
particularly the SDI B+tree walk and the modern LOB page format. This
project is not affiliated with Oracle or with the ibdNinja project.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).

Copyright (C) 2026 Przemysław Malkowski

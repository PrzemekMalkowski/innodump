# 🗄️ innodump

`innodump` is an offline reader for MySQL/MariaDB InnoDB tablespace files
(`table.ibd`, file-per-table mode, or a shared/common tablespace holding
several tables — see `--table` below) — MySQL 26.x, 9.7, 8.0/8.4, MySQL
5.6/5.7, or MariaDB given the table's `.frm` file alongside it (see "MySQL
5.6/5.7 (.frm) support" and "MariaDB support", below). Given a table's
tablespace file or a data directory, it produces, under
`./sqldump_<timestamp>/` by default:

- `schema.table-schema.sql` — a best-effort `CREATE TABLE` statement,
  reconstructed from the table's own embedded dictionary information (SDI),
  or its `.frm` file for a pre-8.0 table.
- `schema.table-data.sql` — one `INSERT INTO ... VALUES (...);` statement
  per live row, decoded by walking the table's clustered index (its B+tree)
  directly in the file.

Output files are named after the table's own schema and name (as recorded
in its dictionary information, not the `.ibd` file's own filename) since a
shared/common tablespace's single `.ibd` file can hold more than one
table — extracting `mysql.ibd`'s `user` table produces `mysql.user-schema.sql`
/`mysql.user-data.sql`, not `mysql-schema.sql`/`mysql-data.sql`.

Pass `--format tsv` instead to produce a MySQL Shell `util.loadDump()`
-compatible dump directory (schema/table DDL plus tab-separated data
files and metadata JSON) rather than SQL `INSERT` statements — see "TSV
output" below. Pass `--source-dir DIR` instead of `--file` to recursively
scan a directory for every `.ibd` file it holds and extract every table
each one contains, rather than a single table from a single file — see
"Bulk extraction" below. Point `--file` at `ibdata1` itself (with
`--table schema.table`) to extract a table that has no `.ibd` of its own
at all - the shared/system tablespace every table lives in together when
`innodb_file_per_table=0` - see "Shared/system tablespace", below.

It reads the file directly and does not connect to a running server, so
it's safe to point at a copy of a `.ibd` file from a node that is up or
down (as long as the copy is consistent — e.g. taken while the server was
stopped, or via `FLUSH TABLES ... FOR EXPORT`).

> **This is not a backup tool.** `innodump` reads whatever bytes happen to
> be in the file(s) it's pointed at — it has no notion of a transactionally
> consistent snapshot, doesn't coordinate with the server (no
> `FLUSH TABLES ... FOR EXPORT`, no binlog/GTID position, no locking), and
> can't tell you whether what it read is a moment-in-time-consistent view
> across tables or even within one table. Point it at a copy taken while
> the server was stopped, or at a proper consistent snapshot/backup (e.g.
> `FLUSH TABLES ... FOR EXPORT`, `mariabackup`/`xtrabackup`, a filesystem
> snapshot) — never at a live, running instance's own datadir — and treat
> its output as a best-effort reconstruction for inspection, recovery, or
> migration, not as a substitute for real backup/replication tooling or as
> a guarantee that it reflects the most recent committed data.

## How it works (brief)

Since MySQL 8.0, every InnoDB tablespace carries a compressed JSON copy of
its own table definition — the Serialized Dictionary Information (SDI) —
in a small hidden index inside the file itself. `innodump` locates
that index, decompresses the JSON, and uses it to learn every column's
type, nullability, and physical storage layout, plus which page starts the
table's clustered (PRIMARY KEY) index.

It then walks that index's B+tree from the root to the leftmost leaf page,
across every leaf page via the sibling-page pointers, decoding each row's
NULL bitmap and variable-length field list exactly as InnoDB itself does.
Columns stored off-page (long BLOB/TEXT/JSON values) are followed through
MySQL 8.0's LOB page format to recover the full value.

## Example runs

```
$ innodump --file instant_t.ibd --skip-corrupted
=== innodump 0.7.6 ===
Table:       db1.instant_t (row_format=DYNAMIC)
Columns:     29 output (31 total incl. system/hidden)
warning: corrupted page 100 (index "PRIMARY", id 943): LSN low bytes at the start and end of the page disagree
Schema file: sqldump_2026-09-08_15.43/db1.instant_t-schema.sql
Data file:   sqldump_2026-09-08_15.43/db1.instant_t-data.sql (100680 row(s) written, 1 corrupted page(s) skipped - see warnings above)
```
```
$ innodump --file child_1.ibd --skip-corrupted
=== innodump 0.7.6 ===
Table:       db1.child_1 (row_format=DYNAMIC)
Columns:     2 output (4 total incl. system/hidden)
warning: corrupted page 1996 (index "PRIMARY", id 693): page 1996 is past the end of the file (it holds only 1920 page(s)) - the file looks truncated (not fully copied), rather than corrupted
Schema file: sqldump_2026-09-08_15.44/db1.child_1-schema.sql
Data file:   sqldump_2026-09-08_15.44/db1.child_1-data.sql (3721 row(s) written, 1 corrupted page(s) skipped - see warnings above)
Note:        the source file looks truncated (not fully copied) - recovered every row up to where it ends; anything stored after that point is missing.
```
```
$ innodump --source-dir /data/ --format tsv
=== innodump 0.7.6 ===
Following /data/ recursively — recognized as a single instance's datadir (found ibdata1), so every InnoDB table found inside (file-per-table .ibd files, plus any table found only in ibdata1's own shared tablespace) will be dumped.
Source:      /data/ (8 .ibd file(s), 129 .frm-only table(s) to resolve via ibdata1 found)
  db1.orders_and_vectors: 0 row(s)
  db1.t1: 3 row(s)
  my_db_1.t2: 3 row(s)
  my_db_1.test9: 2 row(s)
Files:       8/8 processed
Tables:      4 extracted
Skipped:     4 system-schema table(s) skipped (mysql/sys/performance_schema/information_schema/ndbinfo) - pass --include-system-schemas to include them
Note:        129 .frm file(s) with no matching .ibd had no InnoDB entry in ibdata1's own dictionary either - likely a non-InnoDB table, a VIEW, or a general tablespace this tool can't reach - skipped
Rows:        8 written
Output:      tsvdump_2026-09-08_15.59
```
```
$ innodump --file /data/sandboxes/msb_8_4_3/data/sbtest/sbtest1.ibd --deleted-only --verbose
File:        /data/sandboxes/msb_8_4_3/data/sbtest/sbtest1.ibd (9437184 bytes)
Space ID:    71355
Pages:       576
Page size:   16384 bytes
FSP flags:   0x00004021 (post_antelope=true atomic_blobs=true data_dir=false shared=false temporary=false encryption=false sdi=true)
SDI:         present (written by MySQL 8.4.3, dd_version 80300, sdi_version 80019)
Tables in SDI: 1
Row format:  DYNAMIC
Clustered index: "PRIMARY" (id 71531), root page 4
Secondary indexes: 1
Physical fields: 6 (4 output)
=== innodump 0.7.7 ===
Table:       sbtest.sbtest1 (row_format=DYNAMIC)
Columns:     4 output (6 total incl. system/hidden)
Schema file: sqldump_2026-09-08_22.17/sbtest.sbtest1-schema.sql
Data file:   sqldump_2026-09-08_22.17/sbtest.sbtest1-data-deleted.sql (2100 delete-marked row(s) written)
```

## Build

Requires Go 1.21+. Pulls in one dependency,
[klauspost/compress](https://github.com/klauspost/compress) (pure Go, used
only for `--compression=zstd` — see "TSV output" below), fetched
automatically on first build.

```sh
go build -o innodump .
```

Linux and macOS (amd64/arm64) only for now — the progress bar's TTY
detection uses a POSIX `ioctl` (see `progress.go`) that has no Windows
equivalent yet, so a Windows build isn't offered on the
[releases page](https://github.com/PrzemekMalkowski/innodump/releases).

## Usage

```
innodump --file /path/to/table.ibd [options]
```

| Flag | Description |
|------|-------------|
| `--file PATH` | Path to the `.ibd` file (required, unless `--source-dir` is given instead). |
| `--source-dir DIR` | Recursively scan DIR for `.ibd` files and extract every table each one holds, instead of a single `--file`. Cannot be combined with `--file` or `--table` (see "Bulk extraction", below). Refuses to scan a DIR that isn't a single instance's datadir or a single database directory - see `--force-scan`. |
| `--force-scan` | With `--source-dir`, scan DIR even if it doesn't look like a single instance's datadir or a single database directory (see "Bulk extraction", below). |
| `--include-system-schemas` | With `--source-dir --format=tsv`, also dump the server's own housekeeping schemas (`mysql`, `sys`, `performance_schema`, `information_schema`, `ndbinfo`) - left out by default (see "Bulk extraction", below). |
| `--out-dir DIR` | Directory for the output (default: `./sqldump_<timestamp>` for `--format=sql`, `./tsvdump_<timestamp>` for `--format=tsv` - `<timestamp>` is this run's own start time, `date +%Y-%m-%d_%H.%M`, so repeated runs never collide; created if it doesn't exist). |
| `--format sql\|tsv` | `sql` (default): a `schema.table-{schema,data}.sql` pair per table. `tsv`: a MySQL Shell `util.loadDump()`-compatible dump directory (see "TSV output", below). |
| `--compression none\|zstd` | `--format=tsv` only: compress each table's data file with zstd (default `none`) - see "TSV output" below. |
| `--compression-level N` | zstd compression level, 1-22 (default `1`, matching `util.dumpSchemas()`'s own lightest/fastest setting); only used with `--compression=zstd`. |
| `--table NAME` | Which table to extract, if the file holds more than one - always required for a shared/common tablespace. Not usable with `--source-dir`, which always extracts every table it finds. |
| `--limit N` | Stop after N rows (0 = all). |
| `--ddl-only` | Only write the schema file(s); skip walking any table's data entirely. |
| `--deleted-only` | Dump only rows still marked as delete-marked (deleted, but not yet purged/overwritten by InnoDB) instead of live rows - for forensic recovery of recently deleted data (see "Recovering deleted rows", below). The output data file's name gets a `-deleted` suffix, so it never collides with a normal dump of the same table. |
| `--yes` | Overwrite existing output without prompting (also honored via `YES=1`). Without it: for `--format=sql`, an existing output file triggers an interactive y/N prompt, or - with no terminal to prompt on - a refusal naming the file(s) in question; for `--format=tsv`, the same, but checked only against the dump directory's own root metadata file (`@.json`), not every individual file it may end up writing. |
| `--skip-corrupted` | On a corrupted leaf page, note it and carry on from the next page instead of stopping. |
| `--no-progress` | Never draw the progress bar on stderr (also honored via `NO_PROGRESS=1`). |
| `--verbose` | Print extra file/table details: page size, FSP flags, the MySQL version and dictionary/SDI versions that wrote the file, and index/column counts. Single `--file` only. |
| `--debug` | Print each record's decoded field byte-ranges as they're read. |
| `--dump-page N` | Hex-dump one page and its record chain, then exit. Requires `--file`. |
| `--version` | Print version and exit. |

Status output (not the generated SQL/TSV files themselves, which are never
colored) uses a little colour when stdout is a real terminal - a bold
banner line, dim field labels next to a differently-colored value
(`--verbose`'s `Space ID:    8`, say), and green/yellow/red for
success/skipped/error counts. `--help`'s per-flag listing is colored the
same way (flag name vs. its description). Disable all of it with
`NO_COLOR=1` (https://no-color.org) or by piping/redirecting output, which
always gets the same plain text either way.

## TSV output (MySQL Shell-compatible dumps)

`--format tsv` produces a dump directory that MySQL Shell's own
`util.loadDump()` can load directly, as if it had been produced by
`util.dumpSchemas()`/`util.dumpTables()` — one `.ibd` file's worth of data
becomes one self-contained dump directory (or, with `--source-dir`, many
`.ibd` files' tables all land in the one dump directory together). This was
reverse-engineered from MySQL Shell's own source
(`modules/util/dump/{dumper,text_dump_writer}.cc`, `modules/util/common/
dump/*`, and `modules/util/load/dump_reader.cc` for what a loader actually
requires) and confirmed end-to-end (MySQL Shell 8.4.8, dump format version
2.0.1): every scalar type this tool decodes, `NULL`s, and a `BLOB` large
enough to need multi-byte base64 padding, dumped with `--format tsv` and
loaded back with `util.loadDump()`, byte-for-byte matched the original
table (`SELECT * FROM orig EXCEPT SELECT * FROM reloaded`, both directions,
came back empty) — including a `ROW_FORMAT=REDUNDANT` table and a
`GEOMETRY` column round-tripped through `ST_AsText()`.

For one `table.ibd`, this produces (under `--out-dir`, `./tsvdump_<timestamp>`
by default):

- `@.json` / `@.done.json` — the dump directory's root metadata: which
  schemas/tables it holds, and that it's complete (`util.loadDump()` waits
  for more data if `@.done.json` is missing, since a dump directory can
  also be loaded while still being written to — not something this tool
  ever needs, so it always writes `@.done.json` immediately).
- `<schema>.sql` / `<schema>.json` — a `CREATE DATABASE IF NOT EXISTS` and
  which tables that schema holds.
- `<schema>@<table>.sql` — that table's `CREATE TABLE` (identical to
  `--format=sql`'s schema file, less its explanatory comments).
- `<schema>@<table>.json` — column list, TSV dialect, and the other
  per-table options `util.loadDump()`'s importer needs.
- `<schema>@<table>.tsv` (or `<schema>@<table>.tsv.zst` — see
  "Compression", below) — the table's data, one field per column separated
  by a tab, one row per line, in MySQL Shell's own default TSV dialect
  (`FIELDS TERMINATED BY '\t' ESCAPED BY '\\' LINES TERMINATED BY '\n'`, no
  `FIELDS ENCLOSED BY` - the same dialect `LOAD DATA INFILE` itself
  defaults to): `NULL` is `\N`; the escape character, tab, newline,
  carriage return, ASCII NUL, backspace, and Ctrl+Z each become a two-byte
  backslash sequence in a string/JSON/ENUM/SET value; `BLOB`/`BINARY`/
  `VARBINARY`/`BIT`/`GEOMETRY`/`VECTOR` columns are base64-encoded (with a
  matching `"decodeColumns": {"col": "FROM_BASE64"}` entry in that table's
  `.json`), matching `util.dumpSchemas()`'s own default encoding for
  those "CSV-unsafe" types.

Schema/table names are percent-encoded into filenames exactly the way
MySQL Shell's own dumper does it (every ASCII byte outside
`[A-Za-z0-9._~-]` becomes `%XX`; a name is otherwise used as-is), so a
schema or table name with unusual characters still produces a loadable
dump.

### Compression

Pass `--compression zstd` to compress each table's data file (`--compression
none`, the default, leaves it plain) - `util.dumpSchemas()`/`dumpTables()`
themselves default to zstd, but a single-table extraction tool producing a
surprise `.zst` file nobody asked for seemed like the wrong default here.
The result is fully `util.loadDump()`-compatible either way: the data file
is named `<schema>@<table>.tsv.zst`, and its own metadata JSON records
`"extension": "tsv.zst"` and `"compression": "zstd"`, exactly like a real
compressed dump.

`--compression-level N` (1-22, default `1`) is the same real zstd
compression level `util.dumpSchemas()`'s own `compressionLevel` option
takes. This tool uses a pure-Go zstd encoder
([klauspost/compress](https://github.com/klauspost/compress)) rather than
linking the real `libzstd`, so the file it writes is a completely standard,
valid zstd frame — any zstd decoder, including the one `util.loadDump()`
itself uses, reads it identically to one `libzstd` produced — but
`--compression-level`'s number only selects the nearest of this encoder's
four internal speed/ratio presets, rather than that exact numbered level;
this affects the resulting compression ratio/speed trade-off, never
whether the file loads correctly.

Deliberately not reproduced, since a loader doesn't require any of it (see
`tsvdump.go`'s package comment for chapter and verse): the checksum/`.idx`
sidecar files, partitions, views, triggers, and histograms (this tool
doesn't extract any of those regardless of output format), and a
schema-level `DEFAULT CHARACTER SET`/
`COLLATE` in `<schema>.sql` (this tool has no access to
`information_schema.SCHEMATA`, only to the `.ibd` file itself — every
table's own `DEFAULT CHARSET`/`COLLATE`, which is what actually governs
its stored data, still comes through in full on each table's own `CREATE
TABLE`, exactly as it does for `--format=sql`).

## Bulk extraction (`--source-dir`)

`--source-dir DIR` recursively scans `DIR` for every `.ibd` file it can
find (matching by extension only, so a MySQL/MariaDB `datadir` copied
as-is works directly) and extracts every table each one holds — unlike a
single `--file`, which extracts one table (or, for a shared/common
tablespace holding several, errors out asking `--table` to pick one),
`--source-dir` always extracts every table it finds, since asking
interactively doesn't fit a recursive scan across possibly many files.
It combines with either `--format`:

If `DIR` also holds an `ibdata1` (a pre-8.0/MariaDB instance with
`innodb_file_per_table=0`, or one mixing the two), `--source-dir` doesn't
stop at file-per-table `.ibd` files: every `.frm` file it finds with no
`.ibd` of its own is looked up in `ibdata1`'s own internal dictionary
(the same `SYS_TABLES`/`SYS_INDEXES` lookup `--table schema.table` uses
against a single shared tablespace file — see "Shared/system tablespace",
below — done once for every such `.frm` found, rather than one lookup per
table), and every one found there is extracted from `ibdata1` the same as
any other table. A `.frm` with no InnoDB entry there at all (most
commonly a MyISAM/CSV/Aria table, or a `VIEW` — both perfectly normal
things to find `.frm` files for outside a table's own schema) is simply
left out — not an error, and not warned about individually, since a real
`mysql` schema alone typically holds well over a hundred of them; the
summary reports how many were left out this way as a single line.

```sh
# One schema.table-{schema,data}.sql pair per table, all in --out-dir.
innodump --source-dir /var/lib/mysql --out-dir sqldump

# One MySQL Shell-loadable dump directory holding every table found.
innodump --source-dir /var/lib/mysql --format tsv --out-dir dump
```

A `.ibd` file this tool can't extract (missing `.frm`, an unsupported
`ROW_FORMAT`, a corrupted page with `--skip-corrupted` not given, …) is
skipped with a warning printed to stderr rather than stopping the whole
scan; the final summary line reports how many of the files found were
processed successfully, and the tool exits non-zero if any weren't. Since
many tables can land in one flat `--out-dir` (`--format=sql`) or one dump
directory (`--format=tsv`), a schema+table name collision is possible if
`DIR` holds copies of tables from more than one distinct MySQL instance
that happen to share both a schema and a table name — not something a
single real instance's own `datadir` can produce, since schema+table names
are already unique there.

### Guardrail: `DIR` must look like one instance or one database

Before scanning anything, `--source-dir` checks that `DIR` itself is
either a single instance's own datadir (it holds `ibdata1` directly) or a
single database directory (it holds `.ibd`/other files directly and no
subdirectories of its own — a real schema directory never nests another
one inside itself). If it's neither — most often because `DIR` is a
parent folder holding several instances' datadirs side by side (a sandbox
root, say) or a pile of old backups — the scan is refused outright rather
than silently extracting far more than intended:

```
Error: /data/sandboxes doesn't look like a single instance's datadir (no
ibdata1 found directly inside it) or a single database directory (it holds
subdirectories of its own, which a real one never does) - ...
```

Point `--source-dir` at the specific datadir or database directory you
actually want, or - if scanning a directory shaped like that really is
what you want - pass `--force-scan` to skip this check.

### System schemas excluded by default (`--format=tsv`)

`--source-dir --format=tsv` is the "dump this whole instance" case, so —
matching MySQL Shell's own `util.dumpInstance()` default — it leaves out
the server's own housekeeping schemas (`mysql`, `sys`,
`performance_schema`, `information_schema`, `ndbinfo`) unless
`--include-system-schemas` is given. The final summary reports how many
tables were left out this way (`Skipped: N system-schema table(s)
skipped ...`). This filtering only applies to that combination: a single
`--file`/`--table` naming a table in one of those schemas explicitly is
always dumped (it was asked for by name), and `--source-dir --format=sql`
dumps everything it finds, same as before this option existed — `sql`'s
one-file-pair-per-table output has no per-run metadata to omit an entry
from, so there was no equivalent MySQL Shell behavior to match there.

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

## Shared/system tablespace (`innodb_file_per_table=0`)

Before MySQL 5.6.6 defaulted it on (and still today, for any server or
table with `innodb_file_per_table=0`, common on older MariaDB/MySQL
5.1-5.7 installs), a table's data doesn't live in its own `table.ibd` at
all - every such table's rows sit together inside the shared/system
tablespace, `ibdata1`. Point `--file` at `ibdata1` itself and pass
`--table schema.table` (or `schema/table`, InnoDB's own internal form,
exactly as `INFORMATION_SCHEMA.INNODB_SYS_TABLES.NAME` shows it) to
extract one:

```sh
innodump --file /var/lib/mysql/ibdata1 --table sbtest.sbtest1
```

Omit `--table` (or misspell it) and the error lists every table this
tablespace's own dictionary actually holds, the same way a `.ibd` file
whose SDI holds more than one table does (see `--table`'s own
description).

`--source-dir` never requires naming a table this way - pointed at a
datadir that holds `ibdata1`, it looks up every `.frm` file it finds with
no `.ibd` of its own against this same dictionary automatically, on top
of every ordinary file-per-table `.ibd` it finds - see "Bulk extraction",
above.

This reads InnoDB's own classic internal data dictionary - `SYS_TABLES`
and `SYS_INDEXES`, still visible today via
`INFORMATION_SCHEMA.INNODB_SYS_TABLES`/`INNODB_SYS_INDEXES` on a running
MariaDB server (MySQL 8.0 replaced this dictionary with the SDI this tool
otherwise relies on, so this path only ever runs for a pre-8.0-style,
SDI-less tablespace) - to find which tablespace and root page a table's
clustered index actually lives at, since the "root page 3" convention a
table's own dedicated file always follows doesn't hold inside a shared
one. The table's own SQL-level schema (column names/types/nullability,
secondary indexes) still comes from its `.frm` file exactly as for any
other pre-8.0 table (see "MySQL 5.6/5.7 (.frm) support" and "MariaDB
support", above) - expected at `<schema>/<table>.frm`, a sibling of
`ibdata1` itself, precisely where the server itself always keeps it
regardless of where that table's InnoDB data physically lives. See
`sysdict.go` for exactly which fields of `SYS_TABLES`/`SYS_INDEXES` this
reads and how, cross-checked against MariaDB 10.6's own
`storage/innobase/include/dict0boot.h`/`dict0crea.cc`, and validated
end-to-end (a fresh MariaDB 11.8.6 instance, `innodb_file_per_table=0`,
reload-and-diff came back empty) - including a table with a secondary
index, and detecting a table whose dictionary entry points at a
*different* tablespace (its own separate file, most likely) rather than
silently reading the wrong bytes.

**A consistent copy matters here even more than for a single table's
`.ibd`, and `FLUSH TABLES` does not provide it.** A file-per-table
tablespace can be safely copied live via `FLUSH TABLES ... FOR EXPORT`;
there's no equivalent for the shared tablespace itself, and (confirmed
against a real server) plain `FLUSH TABLES` does nothing to help -
copying `ibdata1` off a *running* server without stopping it first (or
without a proper hot-backup tool that replays the redo log, e.g.
`mariabackup`/`xtrabackup`) can capture a stale, partially checkpointed
snapshot: a table created or altered even minutes earlier can be entirely
missing from `SYS_TABLES` in the copy, or its data out of date, simply
because InnoDB's own background page cleaner hadn't gotten around to
writing those particular dictionary/data pages back to the file yet -
nothing this tool does is wrong in that case, and the same table becomes
visible immediately once that write-back happens.

Two ways to get a consistent copy:

- **Stop the server first** (a normal/clean shutdown always flushes every
  dirty page before exiting) - simplest, but not always practical on a
  server you need to keep running.
- **Force a flush without stopping it.** Note the current
  `innodb_max_dirty_pages_pct`/`_lwm`, then:
  ```sql
  SET GLOBAL innodb_max_dirty_pages_pct = 0;
  SET GLOBAL innodb_max_dirty_pages_pct_lwm = 0;
  -- wait for this to reach 0 (a couple of seconds on a small/idle instance):
  SHOW GLOBAL STATUS LIKE 'Innodb_buffer_pool_pages_dirty';
  -- then restore what you noted above - leaving both at 0 forces the
  -- server to flush far more aggressively than normal from then on:
  SET GLOBAL innodb_max_dirty_pages_pct = 90;      -- example
  SET GLOBAL innodb_max_dirty_pages_pct_lwm = 10;  -- example
  ```
  Confirmed on a real MariaDB 11.8.6 instance: a table created moments
  earlier was invisible to this tool (`Innodb_buffer_pool_pages_dirty >
  0`, unmoved by `FLUSH TABLES`) until this forced it to 0, with no
  restart needed.

`innodump` itself never connects to a server to do any of this (it's
strictly an offline file reader, by design) - if a table isn't where
`SYS_TABLES` says it should be, the error suggests exactly this.

Only the system tablespace itself (space id 0) is read this way - a table
recorded in `SYS_TABLES` with a different `SPACE` lives in its own
file-per-table `.ibd` (use `--file` on that file directly) or in a
separate general tablespace file this tool has no way to reach from here
(the dictionary itself only ever lives in the system tablespace); either
case is detected and reported by name/id rather than silently
mis-decoded.

`--skip-corrupted` only ever applies once a table has actually been
found: a corrupted `SYS_TABLES`/`SYS_INDEXES` page is always fatal (there
being only one way to reach the specific row `--table` names, unlike an
ordinary table's own leaf-page chain, corruption there can't be skipped
past any more than a corrupted root page can - see "Corrupted pages",
above). Confirmed harmless on a real, heavily damaged `ibdata1` (50% of
its pages randomly corrupted): every failure mode above - a corrupted
dictionary header, `SYS_TABLES`, `SYS_INDEXES`, or the target table's own
data - produced one of these same clear errors (or, for the table's own
data specifically, a normal `--skip-corrupted` partial recovery), never a
crash or a hang.

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
  `INVISIBLE`), `FOREIGN KEY` constraints (`ON DELETE`/`ON UPDATE`
  included, omitted only when they're the implicit `NO ACTION` default,
  exactly like `SHOW CREATE TABLE` itself), and the table's — and, where a
  column's own differs, that column's own — default charset/collation are
  reproduced from the SDI, matching `SHOW CREATE TABLE`'s own rules for
  when a column needs an explicit `CHARACTER SET`/`COLLATE` (including a
  column whose collation was simply named explicitly at `CREATE`/`ALTER
  TABLE` time even though it happens to equal the table's own default, and
  the special-cased `utf8mb4_0900_ai_ci` default collation - see
  `sqlout.go`'s `GenerateDDL`, `resolveCollation`, and
  `collations.go`'s `primaryCollationIDs` for chapter and verse, sourced
  directly from `mysql-server`'s own `sql_show.cc`). `DEFAULT` clauses and
  other table-level options are not reproduced — add them by hand if you
  need an exact round-trip DDL, or restore alongside the original `SHOW
  CREATE TABLE` output if you have it.
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

## Recovering deleted rows

A `DELETE` doesn't remove a row from its leaf page right away. InnoDB
first just flips that record's delete-mark bit and leaves the bytes in
place; later, in the background, once no open transaction could still
need the old version (InnoDB's MVCC undo/purge mechanism), a purge thread
physically unlinks it from the page's live record chain - but even then
it doesn't erase the bytes, it just adds that slot to the page's own
*free list* so a future insert can reuse the space. And if enough of a
page's records get purged, InnoDB can go a step further and merge what's
left into a sibling page, deallocating the emptied one entirely -
unlinking the whole page from the table's live leaf-page chain, again
without erasing or reformatting it until it's claimed for something else.
By default `innodump` skips all of this and only decodes live rows,
exactly like a normal `SELECT` would. Pass `--deleted-only` to flip that
around: it walks the same clustered index the same way, but instead
recovers every row still recognizable as deleted, from any of three
sources:

- a **delete-marked** record still on the live record chain, on a page
  still linked into the table's own live leaf level - deleted, but purge
  hasn't gotten to it yet;
- a **purged** record still sitting on such a page's own free list -
  physically removed from the live record chain, but not yet overwritten
  by a later insert;
- either of the above, sitting on a page that's since been merged away
  and deallocated - no longer linked into the table's live leaf level at
  all, but not yet reformatted for something else.

Either way the row's actual field bytes are untouched by the delete/purge
itself (only a header pointer used for chain-linking gets rewritten), so
a recovered row decodes with the exact same fidelity as a live one. Rows
recovered from more than one of these sources can turn out byte-identical
(a page split copies a record's bytes verbatim to the new page, so its
old copy and its later, separately-deleted copy can both still be lying
around) - `innodump` drops any such exact repeat itself, so the output
only ever holds distinct rows; two rows that share a primary key but
whose other columns differ are two genuinely different historical
versions of that row, and both are kept.

```
$ innodump --file table.ibd --deleted-only
=== innodump 0.7.9 ===
Table:       sbtest.sbtest1 (row_format=DYNAMIC)
Columns:     4 output (6 total incl. system/hidden)
Schema file: sqldump_2026-09-08_21.04/sbtest.sbtest1-schema.sql
Data file:   sqldump_2026-09-08_21.04/sbtest.sbtest1-data-deleted.sql (1000 delete-marked row(s) written)
```

The output data file's name always gets a `-deleted` suffix (`...-data-
deleted.sql` for `--format=sql`, `<schema>@<table>-deleted.tsv` - and
every other file `innodump` writes for that table - for `--format=tsv`),
so a `--deleted-only` run never collides with, or gets collided into by,
a normal dump of the same table written to the same `--out-dir`. The
schema/DDL file itself is unaffected (a table's structure doesn't depend
on whether you're after its live or its deleted rows).

This only recovers what the file still happens to hold, and only for as
long as it still holds it: a delete-marked-but-not-yet-purged record can
be purged at any time, a purged record's slot (or a merged-away page's
whole extent) can be reused for something else at any time, and InnoDB is
also free to reorganize a page's free space wholesale (compacting or
discarding what's on its free list) independently of any single record
being reused - so there's no guaranteed recovery window, on any of the
three sources above; the sooner you copy the file after the delete, the
better the odds. A busy shared/system tablespace (`ibdata1`, see below)
tends to reclaim space faster than a dedicated per-table file simply
because more unrelated activity is competing for it. It's still bound by
everything else in this README: it needs the same schema/SDI or `.frm`
this tool always needs to make sense of a page's bytes, it's subject to
the same corrupted/truncated-page handling (a deleted record on a page
`--skip-corrupted` falls back to the same out-of-order full-file scan a
badly truncated file does - see "Truncated files", below - and is
recovered exactly the same way a live one would be), and - like every
other mode - it's read-only best-effort extraction, not a substitute for
a real backup (see the caveat at the top of this README).

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

**Heavy corruption is handled the same way, just more of it.** Tested
against a real MariaDB 11.8.6 `ibdata1` with 15% and 50% of its pages
each randomly damaged (1-10 flipped bytes apiece, scattered across the
whole file - the internal dictionary and both `mysql`'s own and user
tables' data alike): every run still finished promptly with no crash or
hang, recovering every row on every page that survived intact and
reporting a clear reason for every one that didn't. A table whose *root*
page itself is corrupted recovers nothing (there's no way to find its
data at all without it - same as a `--file` given a `.frm` whose page 3
isn't actually an index page), and a corrupted `SYS_TABLES`/`SYS_INDEXES`
page (see "Shared/system tablespace", below) is always fatal regardless
of `--skip-corrupted` for the same reason - both already-documented,
`--skip-corrupted` only ever helps with an ordinary leaf page.

## Truncated files

A file that simply ends early - most commonly an interrupted copy, rather
than corruption - is detected specifically as that, not reported as a
generic read error: **without** `--skip-corrupted`, extraction stops with
`page N is past the end of the file (it holds only M page(s)) - the file
looks truncated (not fully copied), rather than corrupted`. **With**
`--skip-corrupted`, it's treated as a corrupted page with that same
reason (so it shows up in the usual warning/`-- skipped corrupted page
...` places) and the walk ends there - there's no page header to recover
a "next page" pointer from, so unlike an ordinary corrupted page in the
middle of an otherwise-intact file, this always means "nothing further
exists to read," never "skip this one and keep going." Either way, the
final summary adds one more line making this specific reason hard to
miss:

```
Data file:   sqldump_.../schema.table-data.sql (11078 row(s) written, 1 corrupted page(s) skipped - see warnings above)
Note:        the source file looks truncated (not fully copied) - recovered every row up to where it ends; anything stored after that point is missing.
```

`--source-dir` reports the same per file in its own per-table summary
line (`... [source file truncated]`) and once more in its final tally
(`N source file(s) looked truncated ...`) if any were.

### When even finding the leftmost leaf isn't possible

The above covers a file that ends partway through the ordinary leaf-level
walk, once it's under way. A more severely truncated file can end before
that walk even starts: finding the correct starting point means
navigating down from the root through the index's non-leaf levels first,
and if *that* hits a page number past where the file ends (the root
itself, or any page on the way down), there's no way to know which
leaf page is the real leftmost one - so with `--skip-corrupted`,
`innodump` instead falls back to reading every page the file actually
has and decoding any leaf page it finds belonging to this table's index,
in physical page order rather than key order:

```
warning: corrupted page 4 (index "PRIMARY", id 1586): can't navigate from the root to the leftmost leaf (page 1702109297 is past the end of the file (it holds only 54 page(s)) - the file looks truncated (not fully copied), rather than corrupted) - falling back to an out-of-order scan of every leaf page the file does hold
Data file:   sqldump_.../schema.table-data.sql (1000 row(s) written, 1 corrupted page(s) skipped - see warnings above)
Note:        the source file looks truncated (not fully copied) - recovered every row up to where it ends; anything stored after that point is missing.
```

This is a best-effort scan, not a validated walk: rows come out in
whatever order the surviving leaf pages happen to sit in the file, not
primary-key order, and there's no way to know whether a leaf page that
should exist but doesn't survive belongs at the start, middle, or end of
that order - all this tool can promise is that every row still present
in the truncated file's own surviving pages is recovered. **Without**
`--skip-corrupted`, this situation is still a hard failure, same as any
other truncation.

## Performance

Reading and validating each leaf page, and following its `FIL_PAGE_NEXT`
pointer to the next one, happen strictly in order, exactly as if this ran
on a single core - that's what gives `--skip-corrupted` a well-defined
"resume from the next page" and makes every row (and, for a corrupted
page, every `-- skipped corrupted page ...`/`-- skipped a row ...`
comment) land in the output in exactly the same order it always has. What
*is* parallel is the CPU-bound part sitting behind that walk - decoding
each row's columns and rendering them as SQL or TSV - which runs across
every available core (`GOMAXPROCS`, normally one per CPU) in batches of up
to that many pages at a time, before results reach the output file in the
original order. Output itself is buffered (rather than one `write(2)`
syscall per row, as versions before 0.5.0 did), which matters at least as
much on a multi-million-row table - see `btree.go`'s `WalkRows` and
`decodeBatch` for the details.

## Progress

When stderr is a real terminal, a spinner shows on it once an extraction
has been running long enough to be worth the flicker (fast runs finish
before it ever appears) — a percentage bar keyed off the tablespace's page
count, a running row count, and elapsed time. It's a rough proxy only: not
every page in the file belongs to the table being extracted, so the bar
may not reach 100% on a small table sharing a big tablespace, and it never
touches stdout or the output files either way. A `--skip-corrupted`
warning printed mid-run (a corrupted page, or a row that failed to
decode) clears the bar's own line first, so it always lands cleanly on
its own line rather than splicing into whatever the bar had last drawn -
the bar itself then picks back up on the next redraw. Pass
`--no-progress` (or set `NO_PROGRESS=1`) to suppress it entirely, e.g.
when running under something that doesn't want carriage-return redraws
in its log capture.

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

## Trademark notice

`innodump` is an independent open-source project and is not affiliated
with, endorsed by, or sponsored by Oracle Corporation. MySQL and InnoDB
are registered trademarks of Oracle Corporation and/or its affiliates.
MariaDB is a registered trademark of MariaDB Foundation. Use of these
names here is solely to describe compatibility and does not imply any
affiliation with or endorsement by their respective trademark owners.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).

Copyright (C) 2026 Przemysław Malkowski

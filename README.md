# 🗄️ innodump

`innodump` is an offline reader for MySQL/MariaDB table files. Given a
table's file, or a whole data directory, it writes out the table's
definition and rows without a running server:

- **InnoDB** `.ibd` files: MySQL 8.0/8.4, 9.7 and 26.x (schema from the
  file's embedded SDI); MySQL 5.5–5.7 and MariaDB (schema from the table's
  `.frm` - see "MySQL 5.5–5.7 (`.frm`) support" and "MariaDB support").
  Also shared tablespaces: a general tablespace holding several tables (pick
  one with `--table`) and the system tablespace `ibdata1` (see
  "Shared/system tablespace").
- **MyISAM** `.MYD` files, schema from the `.sdi` (8.0+) or `.frm`
  (pre-8.0, MariaDB) - see "MyISAM support". `--myisam-to-innodb` writes
  their DDL as `ENGINE=InnoDB` for migrations.
- **Partitioned** InnoDB tables, extracted as one table from all their
  partition files (see "Partitioned tables").
- **MyRocks** (`ENGINE=ROCKSDB`) tables, read straight from the datadir's
  RocksDB files (`.rocksdb/`: SST files, write-ahead logs, MANIFEST), schema
  from the `.sdi` (Percona Server 8.0, 8.4, 9.7) or `.frm` (Percona Server
  5.7, MariaDB) - including damaged and crashed ones; see "MyRocks support".
  `--rocksdb-to-innodb` writes their DDL as `ENGINE=InnoDB` for migrations.

By default it writes, under `./sqldump_<timestamp>/`:

- `schema.table-schema.sql` - a best-effort `CREATE TABLE`, reconstructed
  from the SDI or `.frm`.
- `schema.table-data.sql` - one `INSERT` per live row, decoded by walking
  the table's clustered index directly in the file.

Files are named after the table's schema and name as its dictionary
records them, not after the source file - `mysql.ibd`'s `user` table gives
`mysql.user-schema.sql`.

`--format tsv` writes a MySQL Shell `util.loadDump()`-compatible dump
directory instead (see "TSV output"). `--source-dir DIR` extracts every
table in a datadir or database directory, InnoDB, MyISAM and MyRocks
alike, into one combined dump (see "Bulk extraction").

> **This is not a backup tool.** `innodump` reads whatever bytes are in
> the files it's given. It doesn't coordinate with a server (no locking, no
> binlog/GTID position) and can't tell whether what it read is consistent,
> across tables or even within one. Point it at a copy taken while the
> server was stopped, at a `FLUSH TABLES ... FOR EXPORT` copy, or at a
> prepared `mariabackup`/`xtrabackup` backup or filesystem snapshot - never
> at a running instance's own datadir. Treat the output as a best-effort
> reconstruction for inspection, recovery or migration, not as a guarantee
> that it holds the most recent committed data.

## How it works (brief)

Since MySQL 8.0, every InnoDB tablespace carries a compressed JSON copy of
its table definition - the Serialized Dictionary Information (SDI) - in a
small hidden index inside the file. `innodump` reads it to learn every
column's type, nullability and physical layout, and which page is the root
of the clustered (PRIMARY KEY) index. For older servers the same
information comes from the `.frm` file (and, for `ibdata1`, InnoDB's
internal dictionary).

It then walks the clustered index from the root down to the leftmost leaf
and across every leaf page via the sibling pointers, decoding each record's
NULL bitmap and variable-length field list the way InnoDB does. Off-page
BLOB/TEXT/JSON values are followed through the LOB pages to recover the
full value.

MyRocks tables are read differently - from the datadir's RocksDB files,
merging SST files and write-ahead logs into the current state of every
key - see "MyRocks support".

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
```
$ innodump --file /backup/rocks2/rocks/sbtest15_673.sdi
warning: RocksDB: 1 prepared transaction(s) have no XID event in the binary log - rolled back, as crash recovery would
=== innodump 0.8.0 ===
Table:       rocks.sbtest15 (engine=ROCKSDB)
Columns:     4 output (4 total incl. system/hidden)
Schema file: sqldump_2026-10-13_20.07/rocks.sbtest15-schema.sql
Data file:   sqldump_2026-10-13_20.07/rocks.sbtest15-data.sql (10000 row(s) written)
```

## Build

Requires Go 1.21+. Two dependencies, both pure Go, are fetched on first
build: [klauspost/compress](https://github.com/klauspost/compress) (zstd,
for `--compression=zstd` and for reading zstd-compressed RocksDB blocks)
and [zeebo/xxh3](https://github.com/zeebo/xxh3) (the XXH3 checksum newer
RocksDB versions put on every block).

```sh
go build -o innodump .
```

Linux and macOS (amd64/arm64) only for now: the progress bar's TTY
detection uses a POSIX `ioctl` (`progress.go`), so there's no Windows build
on the [releases page](https://github.com/PrzemekMalkowski/innodump/releases).

## Usage

```
innodump --file /path/to/table.ibd [options]
```

| Flag | Description |
|------|-------------|
| `--file PATH` | The `.ibd` file, or a MyISAM table's `.MYD`/`.MYI` file. Required unless `--source-dir` is given. Any one partition's file (`table#p#p0.ibd`) extracts the whole partitioned table. For a MyRocks table: the datadir (or its `.rocksdb` directory) with `--table`, or the table's `.sdi`/`.frm` (see "MyRocks support"). |
| `--source-dir DIR` | Recursively scan DIR for `.ibd` and `.MYD` files, plus a MyRocks `.rocksdb`/`#rocksdb` directory, and extract every table they hold. Not combinable with `--file` or `--table`. Refuses a DIR that isn't one instance's datadir or one database directory - see `--force-scan`. |
| `--force-scan` | With `--source-dir`, scan DIR even if it doesn't look like a single datadir or database directory. |
| `--include-system-schemas` | With `--source-dir --format=tsv`, also dump `mysql`, `sys`, `performance_schema`, `information_schema` and `ndbinfo` (left out by default). |
| `--out-dir DIR` | Output directory (default `./sqldump_<timestamp>` for `sql`, `./tsvdump_<timestamp>` for `tsv`, timestamped `date +%Y-%m-%d_%H.%M`; created if missing). |
| `--format sql\|tsv` | `sql` (default): a `schema.table-{schema,data}.sql` pair per table. `tsv`: a `util.loadDump()`-compatible dump directory. |
| `--compression none\|zstd` | `--format=tsv` only: compress each data file with zstd (default `none`). |
| `--compression-level N` | zstd level 1-22 (default `1`, `util.dumpSchemas()`'s own default); only with `--compression=zstd`. |
| `--table NAME` | Which table to extract when the file holds more than one - required for a shared tablespace. Not usable with `--source-dir`. |
| `--limit N` | Stop after N rows (0 = all). |
| `--ddl-only` | Only write the schema file(s); don't read any row data. |
| `--myisam-to-innodb` | Write MyISAM tables' DDL as `ENGINE=InnoDB`, adding a `KEY` where InnoDB would reject the `AUTO_INCREMENT` column. Row data is unchanged. |
| `--rocksdb-to-innodb` | Write MyRocks tables' DDL as `ENGINE=InnoDB`, without the index `COMMENT`s MyRocks uses for column family placement. Row data is unchanged. |
| `--rocksdb-dir DIR` | MyRocks: the RocksDB directory, when it isn't the datadir's own `.rocksdb/` (`rocksdb_datadir` pointed elsewhere). |
| `--rocksdb-index N` | MyRocks recovery: read `--table`'s rows from index number N, whatever MyRocks' own dictionary says - for when it has lost track of the table (see "When the MyRocks dictionary is damaged"). `--file` and `--table` only. |
| `--deleted-only` | Dump only deleted rows that are still recoverable - delete-marked, purged-but-not-overwritten, or on freed pages (for MyRocks: rows whose deletion hasn't been compacted away yet) - instead of live rows (see "Recovering deleted rows"). The data file gets a `-deleted` suffix. |
| `--yes` | Overwrite existing output without asking (also `YES=1`). Otherwise an existing output file triggers a y/N prompt, or a refusal when there's no terminal; for `tsv` only the dump's `@.json` is checked. |
| `--skip-corrupted` | On a corrupted page or record, note it and carry on where possible instead of stopping. |
| `--no-progress` | Never draw the progress bar (also `NO_PROGRESS=1`). |
| `--verbose` | Print file/table details: page size, FSP flags, the MySQL and dictionary/SDI versions that wrote the file, index/column counts. Single `--file` only. |
| `--debug` | Print each record's decoded field byte-ranges. |
| `--dump-page N` | Hex-dump one page and its record chain, then exit. Requires `--file`. |
| `--version` | Print version and exit. |

Status output uses a little colour when stdout is a terminal; the
generated files never do. `NO_COLOR=1`, or piping the output, turns it off.

## TSV output (MySQL Shell-compatible dumps)

`--format tsv` writes a dump directory that MySQL Shell's `util.loadDump()`
loads as if `util.dumpSchemas()`/`util.dumpTables()` had written it (with
`--source-dir`, every table goes into the one directory). The format was
worked out from MySQL Shell's source (`modules/util/dump/`,
`modules/util/load/dump_reader.cc`) and confirmed with MySQL Shell 8.4.8
(dump format 2.0.1): every scalar type this tool decodes, `NULL`s, a
`BLOB` needing multi-byte base64 padding, a `ROW_FORMAT=REDUNDANT` table
and a `GEOMETRY` column all loaded back identical to the original (`SELECT
* FROM orig EXCEPT SELECT * FROM reloaded`, both directions, empty).

For one table it writes:

- `@.json` / `@.done.json` - the dump's root metadata and its "complete"
  marker (`util.loadDump()` waits for more data without it).
- `<schema>.sql` / `<schema>.json` - `CREATE DATABASE IF NOT EXISTS`, and
  the schema's table list.
- `<schema>@<table>.sql` - the `CREATE TABLE` (as in `--format=sql`,
  without its comments).
- `<schema>@<table>.json` - column list, TSV dialect and the other options
  the importer needs.
- `<schema>@<table>.tsv` (or `.tsv.zst`) - the rows, in MySQL Shell's
  default dialect (`FIELDS TERMINATED BY '\t' ESCAPED BY '\\' LINES
  TERMINATED BY '\n'`, the `LOAD DATA` default): `NULL` is `\N`;
  backslash, tab, newline, CR, NUL, backspace and Ctrl+Z are
  backslash-escaped in string/JSON/ENUM/SET values;
  `BLOB`/`BINARY`/`VARBINARY`/`BIT`/`GEOMETRY`/`VECTOR` values are
  base64-encoded, with a matching `"decodeColumns"` entry in the table's
  `.json`, as `util.dumpSchemas()` does.

Schema and table names are percent-encoded into file names the way MySQL
Shell does it (every ASCII byte outside `[A-Za-z0-9._~-]` becomes `%XX`).

### Compression

`--compression zstd` compresses each data file into `<schema>@<table>.tsv.zst`
and records `"compression": "zstd"` in its metadata, like a real compressed
dump. It's off by default, although `util.dumpSchemas()` defaults to it,
so a single-table extraction doesn't produce a `.zst` file nobody asked
for.

The encoder is pure Go (klauspost/compress), not `libzstd`. Its output is
standard zstd that any decoder reads, but `--compression-level` only picks
the nearest of its four speed/ratio presets rather than that exact level.
This affects size and speed, never whether the file loads.

Not written, since the loader doesn't need them (see `tsvdump.go`):
checksum/`.idx` sidecar files, per-partition data files (a partitioned
table's rows go into one file), views, triggers and histograms (never
extracted in any format), and a schema-level `DEFAULT CHARACTER SET` in
`<schema>.sql` (the files don't record it; each table's own `DEFAULT
CHARSET`/`COLLATE` is still in its `CREATE TABLE`).

## Bulk extraction (`--source-dir`)

`--source-dir DIR` recursively finds every `.ibd` and `.MYD` file under
`DIR` (by extension, so a copied datadir works as-is) and extracts every
table they hold into one output or dump directory, with either `--format`.
Unlike `--file`, which asks for `--table` when a file holds several tables,
it always extracts all of them.

If `DIR` also holds an `ibdata1` (a pre-8.0 or MariaDB instance with
`innodb_file_per_table=0`, or one mixing both), every `.frm` without an
`.ibd` of its own is looked up in `ibdata1`'s internal dictionary (one pass
for all of them) and extracted from there - see "Shared/system
tablespace". A `.frm` with no InnoDB entry there (usually a
MyISAM/CSV/Aria table or a `VIEW`) is left out without a warning, since a
`mysql` schema alone has over a hundred; the summary counts them in one
line. A MyISAM table's `.frm` is read as part of its `.MYD`, never looked
up in `ibdata1`.

```sh
# One schema.table-{schema,data}.sql pair per table, all in --out-dir.
innodump --source-dir /var/lib/mysql --out-dir sqldump

# One MySQL Shell-loadable dump directory holding every table found.
innodump --source-dir /var/lib/mysql --format tsv --out-dir dump
```

A table that can't be extracted (missing `.frm`, unsupported format, a
corrupted page without `--skip-corrupted`, ...) is skipped with a warning
rather than stopping the scan; the summary reports how many files
succeeded, and the exit code is non-zero if any failed. Tables from two
different instances that share a schema and table name would collide in
one output directory - a single real datadir can't produce that.

### Guardrail: `DIR` must look like one instance or one database

`--source-dir` first checks that `DIR` is either an instance's datadir (it
holds `ibdata1`) or a single database directory (files, but no
subdirectories). Anything else - typically a parent folder of several
datadirs, or a pile of backups - is refused rather than silently extracting
far more than intended:

```
Error: /data/sandboxes doesn't look like a single instance's datadir (no
ibdata1 found directly inside it) or a single database directory (it holds
subdirectories of its own, which a real one never does) - ...
```

Point it at the specific directory you want, or pass `--force-scan`.

### System schemas excluded by default (`--format=tsv`)

Like MySQL Shell's `util.dumpInstance()`, `--source-dir --format=tsv`
leaves out `mysql`, `sys`, `performance_schema`, `information_schema` and
`ndbinfo` unless `--include-system-schemas` is given; the summary counts
what was skipped. A table named explicitly with `--file`/`--table` is
always dumped, and `--source-dir --format=sql` dumps everything.

## Scope (v1)

Supported:

- MySQL 8.0.16+ tablespaces (including 8.4), `ROW_FORMAT=DYNAMIC`,
  `COMPACT`, `REDUNDANT` or `COMPRESSED`, including `INSTANT ADD/DROP
  COLUMN` history under both the 8.0.12–8.0.28 mechanism and 8.0.29+ row
  versioning (see "INSTANT ADD/DROP COLUMN").
- MySQL 5.5–5.7 and MariaDB tables, via their `.frm` (see below).
- Partitioned InnoDB tables, MyISAM tables, and MyRocks tables (see their
  sections).

Transparent page compression (`CREATE TABLE ... COMPRESSION='zlib'` or
`'lz4'`, MySQL 5.7+) is undone page by page as the server's own I/O layer
does it - restore the compressed body after the FIL header, then check the
page's checksum - so every page type (index, SDI, LOB, allocation) reads
as usual, and the DDL keeps the table's `COMPRESSION=` option. A page that
won't decompress is reported like any corrupted page. (MySQL's own
`ibd2sdi` can't read the SDI of such a tablespace; `innodump` can.)
Checked against Percona Server 8.0.35/8.0.46 zlib and LZ4 tables, reloaded
and compared row for row.

`ROW_FORMAT=REDUNDANT`'s older record layout (a per-field offset array
instead of a NULL bitmap and length list) is decoded by `redundant.go`.
`ROW_FORMAT=COMPRESSED` pages (any `KEY_BLOCK_SIZE`) are rebuilt to full
size by `zipdecompress.go` the way InnoDB reads them: inflate the
compressed image, replay the page's modification log (where much of a
page's row content usually lives), then restore the system columns, node
pointers and BLOB references from the page's trailer. A `COMPRESSED`
table's off-page values use a separate format (`ZBLOB`/`ZBLOB2`) that isn't
read; such a row is skipped with a warning.

**Not supported in v1.** These are checked for and refused with a specific
reason rather than guessed at:

- Encrypted tablespaces.
- Row versioning combined with `REDUNDANT`, or either instant mechanism
  combined with `COMPRESSED`.
- Partitioned MyISAM tables, and partitions stored in a shared or general
  tablespace.
- MariaDB tablespaces using the classic (non-`full_crc32`) checksums -
  effectively MariaDB older than 10.4, or with `full_crc32` turned off -
  and MariaDB's `page_compression`.
- MyISAM `ROW_FORMAT=COMPRESSED` (`myisampack`) tables and MyISAM `BIT`
  columns.

`FULLTEXT` and `SPATIAL` indexes appear in the DDL but are never read; only
the clustered index is, and it holds all the data.

The explicit checks cover the cases above. A decoding bug on an unusual
table can still fail in a misleading way - past bugs have shown up as
false "file looks truncated" reports - so an unexpected error on a file you
believe is intact is worth reporting.

## MySQL 5.5–5.7 (`.frm`) support

Before 8.0 a tablespace carries no embedded schema. For a pre-8.0 `.ibd`,
`innodump` reads the `.frm` with the same base name next to it (`frm.go`,
ported from `open_binary_frm()`/`make_field_from_frm()` and checked
byte-for-byte against real `.frm` files from MySQL 5.6.51 and 5.7.44). It
builds the same internal model as the SDI path, so everything downstream
works the same way.

A `.frm` doesn't record the clustered index's root page. For a
file-per-table `.ibd`, `innodump` relies on InnoDB's convention that the
first index created - always the clustered one - has its root at page 3,
and fails with a clear error if page 3 isn't an index page. (Tables inside
`ibdata1` are located through InnoDB's own dictionary instead - see
"Shared/system tablespace".)

The clustered index is the `PRIMARY KEY`, or else the first `UNIQUE` key
whose columns are all `NOT NULL` and indexed in full (InnoDB promotes it,
as `open_binary_frm()` does), or else InnoDB's hidden `DB_ROW_ID`. The DDL
still shows a promoted key as `UNIQUE KEY`, like `SHOW CREATE TABLE`. Table
and schema names come from the file path, with MySQL's `@XXXX` filename
encoding decoded (`TABLE@002075.frm` is `` `TABLE 75` ``); the rarer
two-character form used for accented and non-Latin letters is left as-is.

Tables created on MySQL 5.5 or earlier and never rebuilt keep the
pre-5.6.4 temporal formats, which are decoded too: `DATETIME` (an 8-byte
`YYYYMMDDhhmmss` integer), `TIME` (3-byte signed `±hhmmss`), `TIMESTAMP`
(4-byte Unix seconds) and the pre-5.0 4-byte `DATE`, none with fractional
seconds. MyISAM stores them little-endian; InnoDB big-endian with the
sign-bit flip (except the unsigned `TIMESTAMP`). This was checked against
InnoDB `COMPACT`/`REDUNDANT` and MyISAM `FIXED`/`DYNAMIC` tables written by
MariaDB with `mysql56_temporal_format=OFF` - zero dates, range limits,
negative `TIME`s and `NULL`s all matched the server's `SELECT` - and
against a real MySQL 5.5.62 datadir. The 4-byte `DATE` follows the same
rules but is untested, since no available server still writes it. The DDL
uses the plain type names, so the target server creates the columns in its
current format.

**Not supported for the `.frm` path:** generated/virtual columns (5.7.6+;
refused explicitly), views, and a `.frm` old enough to need the legacy
field-position reconciliation (`find_field()`). Partitioned tables are
supported through their shared `.frm` - see "Partitioned tables".

## MariaDB support

MariaDB has no SDI, so every MariaDB table goes through the `.frm` path.
Its `.frm` adds an "extra2" section after the header, but the pointer
`frm.go` follows to the form-info block lands in the same place, and every
offset after that is the same as MySQL's, so no MariaDB-specific parsing
was needed. The record format is standard InnoDB too.

The difference is at the page level. MariaDB's default
`innodb_checksum_algorithm=full_crc32` (10.4+) reuses the `FSP_SPACE_FLAGS`
bits MySQL uses for `KEY_BLOCK_SIZE`, so such a tablespace isn't actually
compressed. `page.go` checks the `full_crc32` marker bit and then decodes
the page size with MariaDB's formula and verifies pages with its checksum
(one CRC-32C over the page except the last 4 bytes, which hold it), with
the LSN check moved to the second-to-last 4 bytes.

MariaDB's newer collation IDs (`utf8mb4_uca1400_*` and siblings, from ID
2048 up) aren't in the MySQL collation table this tool uses. `collations.go`
maps them to the right charset, and so the right column byte width, but an
ID alone doesn't say which language/pad variant it is, so the DDL states
only `DEFAULT CHARSET=...` for them, not `COLLATE=...`.

Validated end-to-end (extract, reload, diff row by row) against MariaDB
11.6.2: uncompressed tables with `full_crc32` checksums, both handwritten
test tables and a real customer table. Not validated: the older MariaDB
checksum formats and `page_compression` (see "Scope (v1)").

## MyISAM support

Point `--file` at a table's `.MYD` (or its `.MYI`, which resolves to the
`.MYD` beside it):

```sh
innodump --file /var/lib/mysql/mydb/mytable.MYD
```

The schema comes from a file next to the `.MYD`:

- **MySQL 8.0+**: the `<table>_<id>.sdi` file written for every table (the
  `<id>` varies per install, so whichever single `<table>_<digits>.sdi` is
  there is used) - the same JSON as an InnoDB tablespace's SDI.
- **Pre-8.0 MySQL, and MariaDB**: the table's `.frm`, read by the same
  parser as the InnoDB path, name decoding included. `FULLTEXT` and
  `SPATIAL` keys come through. The row format (`FIXED`, `DYNAMIC`, or
  `myisampack`-compressed) comes from the `.MYI` header if present - packing
  a table doesn't rewrite its `.frm` - otherwise from the `.frm`.

Column types, nullability, `AUTO_INCREMENT`, comments, the `PRIMARY KEY`
and secondary/`UNIQUE`/`FULLTEXT` indexes go into the DDL as for InnoDB.

Rows are read by scanning the `.MYD` from the start, the way MyISAM's own
unindexed full scan (`mi_scan`) does. **The `.MYI` isn't needed.** When
it's present, only its header is read:

- to cross-check the record layout derived from the schema (record length,
  field count, and for `DYNAMIC` the flag-bitmap size) - a mismatch fails
  the table with a clear error rather than decoding garbage;
- for each column's storage type as MyISAM recorded it at `CREATE TABLE`
  time, which takes precedence over what the schema suggests. Tables
  created by older servers can differ here (a `TINYINT` stored plain rather
  than zero-packed, with an identical `.frm`), and only the `.MYI` knows;
- for a `FIXED` table's exact record slot length, and the row count
  `--verbose` shows as a sanity check.

A missing or damaged `.MYI` never stops recovery; without it the layout is
derived from the schema alone, which is right for tables created by
current servers.

Both `FIXED` and `DYNAMIC` rows are supported, including `DYNAMIC` records
that later `UPDATE`s split across several blocks, and `CHECKSUM=1` tables.
`ROW_FORMAT=COMPRESSED` (`myisampack`) is detected and refused, and `BIT`
columns aren't decoded yet; every other type supported for InnoDB is
supported here. `CHAR` values are written without trailing padding, as
`SELECT` returns them.

`--skip-corrupted` works as for InnoDB. By default a `.MYD` whose block
structure doesn't parse is a fatal error naming where and why. With
`--skip-corrupted` the spot is noted (a warning, plus a `-- skipped
corrupted record ...` comment in the data file) and the scan resumes at the
next position that looks like a valid record header. A `.MYD` has no page
checksums to confirm that, so a damaged file can yield a few garbled rows
around the damage as well as losing the ones inside it.

Cross-checked against a MyISAM table served by MySQL 8.0+/MariaDB
(`DYNAMIC`, every scalar type decoded for MyISAM, a nullable `TEXT`,
`PRIMARY KEY`/`UNIQUE`/plain/`FULLTEXT` indexes, 10,000 rows):
field-by-field against `myisamchk -dvv`'s per-column layout, and
row-for-row against the server's `SELECT *` - all 10,000 rows matched.

The `.frm` path was checked against a real MySQL 5.6.47 server reading the
same datadir: every MyISAM table in it (`mysql` and an application schema),
and a purpose-built schema covering every scalar type decoded for MyISAM
(`NULL`s, zero dates, negative fractional `TIME`s, `SET`s wider than 3
bytes, utf8 and latin1 `CHAR`/`VARCHAR`, `BINARY`/`VARBINARY`), `FIXED` and
`DYNAMIC` tables, deleted rows, fragmented records, `CHECKSUM=1`,
prefix/`FULLTEXT` keys and a filename-encoded table name. Each was dumped,
reloaded into the same server, and matched the original's `CHECKSUM
TABLE`.

## Migrating MyISAM tables to InnoDB (`--myisam-to-innodb`)

`--myisam-to-innodb` writes every MyISAM table's DDL as `ENGINE=InnoDB`,
so an old datadir's MyISAM tables come out of the same run as its InnoDB
ones, ready to load onto a new server (`util.loadDump()` for
`--format=tsv`, or the `.sql` files). Row data is unchanged; only the
`CREATE TABLE` differs:

- `ENGINE=MyISAM` becomes `ENGINE=InnoDB`, and a `ROW_FORMAT=FIXED`/
  `DYNAMIC` clause is dropped, leaving the target's InnoDB default.
- MyISAM allows an `AUTO_INCREMENT` column in any position of a
  multi-column key (`PRIMARY KEY (grp, id)` gives each `grp` its own
  sequence). InnoDB refuses that unless the column also leads an index, so
  a plain `KEY` on it is added. The counter then becomes table-wide:
  existing rows load unchanged, but new rows are numbered differently than
  MyISAM would have.
- The DDL file's header comments note both changes, and the summary counts
  them (`Converted:   9 MyISAM table(s) written as ENGINE=InnoDB
  (--myisam-to-innodb)`, plus how many got the extra `KEY`).

`FULLTEXT` (InnoDB 5.6+) and `SPATIAL` (5.7+) keys carry over as-is, and
every MyISAM key (1000-byte limit) fits InnoDB's 3072-byte one. What isn't
checked is InnoDB's in-page row size limit (about 8KB with 16KB pages): a
table with many long `CHAR`/`VARCHAR` columns can hit `Row size too large
(> 8126)` under `innodb_strict_mode`, as some old `COMPACT` InnoDB tables
do when loaded onto 8.0 (see "Validation"). The `mysql` schema's MyISAM
tables are converted too, but they're for inspection, not for loading over
a new server's own `mysql` schema (`--format=tsv` leaves them out unless
`--include-system-schemas` is given).

## MyRocks support

MyRocks keeps every `ENGINE=ROCKSDB` table of an instance in one RocksDB
database: the datadir's `.rocksdb/` directory (`#rocksdb/` on MariaDB).
`innodump` reads that directory itself - no RocksDB library, no server:

- the **MANIFEST** (named by `CURRENT`), replayed to find each column
  family's live SST files and the oldest write-ahead log still needed;
- every live **SST file** (RocksDB's block-based table format), each block
  checksum-verified before use;
- the **write-ahead logs** (`NNNNNN.log`) - writes not yet flushed to an
  SST file, which on a crashed or still-running server can be a lot -
  replayed the way RocksDB's own recovery does, including MyRocks'
  two-phase-commit `PREPARE`/`COMMIT` markers (see below);

and merges them into one view: for each key, the newest version wins, and
deletions (point and range) hide what's beneath them, exactly as a
RocksDB read does.

MyRocks' own data dictionary (kept in RocksDB itself, in the `__system__`
column family) maps each table to its index numbers; a table's rows are
its primary key's keys (or its hidden primary key's, for a table without
one). The column definitions come from the table's `.sdi` file (Percona
Server 8.0+, the same file as for MyISAM) or `.frm` (Percona Server 5.7,
MariaDB). Each row is then decoded the way MyRocks' own `Rdb_converter`
and `Rdb_key_def` do it: the value holds the non-key columns in MySQL's
record format, and the primary key columns are decoded back out of the
key's mem-comparable form - integers, floating point, `DECIMAL`, temporal
types, `ENUM`/`SET`/`BIT`, binary strings, `_bin` collations, 8-bit
collations like `latin1_swedish_ci` (with the extra "unpack info" MyRocks
keeps in the value), and, for other collations, the copy MyRocks stores
there. The rules for which key columns are decodable differ between the
5.7-era and 8.0 MyRocks, and `innodump` follows whichever wrote the table.

```sh
# One table: point --file at the datadir (or its .rocksdb directory) ...
innodump --file /var/lib/mysql --table shop.orders
# ... or at the table's own schema file.
innodump --file /var/lib/mysql/shop/orders_1234.sdi
# Every table - InnoDB, MyISAM and MyRocks alike.
innodump --source-dir /var/lib/mysql --format tsv
```

```
$ innodump --file . --table w1.big
=== innodump 0.8.0 ===
Table:       w1.big (engine=ROCKSDB)
Columns:     3 output (3 total incl. system/hidden)
Schema file: out/w1.big-schema.sql
Data file:   out/w1.big-data.sql (47999 row(s) written)
```

`--rocksdb-dir` names the RocksDB directory when `rocksdb_datadir` put it
somewhere else. `--verbose` lists the column families, their live SST
files and unflushed WAL entries, and the table's index number, column
family and `kv_format_version`.

The DDL keeps MyRocks' index `COMMENT`s (`cfname=...`, which place an index
in a column family) and the table `COMMENT` (where `ttl_duration` lives),
and its `AUTO_INCREMENT` is MyRocks' own persisted counter (or the highest
value in the data plus one, if that's higher). Rows come out in primary
key order (descending, for a reverse-ordered `rev:` column family). Partitioned tables come out as one table, partition by
partition, as for InnoDB.

### What's supported

- Percona Server 8.0, 8.4 and 9.7 (`.sdi`), Percona Server 5.7 and
  MariaDB 10.x (`.frm`, including 5.6-style generic partitioning).
- SST `format_version` 0-6 (2-6 tested against real files); checksums
  CRC32c, xxHash, xxHash64, XXH3, or none; block compression none, LZ4,
  LZ4HC, ZSTD (with or without a dictionary), Zlib, Snappy and BZip2 (the
  last two not tested against real files - the Percona build used has
  neither - though the Snappy decoder was checked against a reference
  encoder); every index type (binary search, hash, two-level/partitioned,
  with first key) and the data block hash index.
- Bulk-loaded (`rocksdb_bulk_load`, ingested) SST files, reverse-ordered
  (`rev:`) and custom column families, range deletions, TTL tables
  (rows past their TTL are left out, as the server's own reads hide them),
  `INSTANT ADD COLUMN`, hidden primary keys, and a `UNIQUE NOT NULL` key
  the server promoted to the clustered index.
- `--deleted-only`: rows whose newest version is a deletion but whose
  previous version is still there - until compaction drops both.

### Two-phase commit and the binary log

With the binlog enabled, MyRocks commits every transaction in two phases:
its writes go into the WAL as a `PREPARE` section, the transaction is
written to the binlog, and only then is a `COMMIT` marker added to the
WAL - which MyRocks doesn't even flush straight away, since the binlog is
enough to decide. So a WAL copied from a crashed or running server
routinely ends with prepared sections whose `COMMIT` never made it to the
file. On restart the server commits those whose XID is in the binlog and
rolls back the rest; `innodump` does the same, reading the XID events of
the binlog files its `.index` lists in the datadir. Without them, it
leaves such transactions out and says how many there were.

### Damaged and crashed instances

A copy of a crashed server's datadir - unflushed WAL, prepared
transactions and all - reads like a clean one. Validated on a Percona
Server that aborted mid-flush and then refused to start (`ROCKSDB_CORRUPTED`):
every row came out, identical to what the server recovered once forced
to start.

Every block is checked before it's used. As for InnoDB, damage is fatal by
default (stopping with the file, offset and reason) and skipped with
`--skip-corrupted`:

```
$ innodump --file . --table bk.bl
...
Error: walking table rows: 000133.sst: data block (offset 5588759, size 12974): block checksum mismatch (XXH3, stored 0x5a8e7b08, computed 0x0dd8bed3) (pass --skip-corrupted to skip damaged blocks and carry on)
$ innodump --file . --table bk.bl --skip-corrupted
...
warning: corrupted RocksDB file 000133.sst: data block (offset 5588759, size 12974): block checksum mismatch (XXH3, stored 0x5a8e7b08, computed 0x0dd8bed3)
Data file:   out/bk.bl-data.sql (199601 row(s) written, 1 corrupted block(s) skipped - see warnings above)
```

- **A damaged data block** loses the rows stored in it (299 of 199,900
  above). Skipping it can surface an *older* version of a key from another
  file, or a deleted row from before the deletion; rows in the key range
  of a skipped block are counted in a closing note.
- **A damaged index, metaindex or footer** - or an SST file **cut short**
  by an incomplete copy - doesn't lose the rest of the file: its data
  blocks are found by scanning it from the start (where each block ends
  is worked out from its own structure, and only a block whose checksum
  matches is used). A file whose index alone is damaged comes back
  complete, with just a warning; a truncated one loses only what came
  after the cut, and is flagged `[source file truncated]`.
- **A missing SST file** the MANIFEST lists is reported for each table
  whose key range it overlapped.
- **A damaged WAL record** loses the transaction (WriteBatch) it held;
  reading resumes at the next 32KB log block. A torn last record (a crash
  mid-write) is normal and only noted.
- **A missing or damaged MANIFEST** (or `CURRENT`): every SST file present
  is treated as live, with a warning that rows deleted long ago could
  reappear if a crash left obsolete files behind.

Tested by flipping bytes in data blocks, destroying an index block,
truncating an SST file to 70% of its length, deleting SST files, the
MANIFEST and `CURRENT`, damaging WALs, and damaging 2% of every SST
file's bytes at random: each case recovered what the damage left intact
and reported the rest.

### When the MyRocks dictionary is damaged

If the `__system__` column family's own blocks are lost, a table can drop
out of MyRocks' dictionary while its rows, in other files, are intact.
For a table whose `.sdi`/`.frm` is present but which the dictionary
doesn't list, `innodump` looks through the index numbers holding data
that no dictionary entry claims, in the column family its primary key's
`COMMENT` names, and decodes a sample of each one's rows with the table's
own layout: an index whose rows all decode with every byte accounted for
fits. A table only one such index fits - and which no other table also
fits - is extracted from it, with a warning. When several fit (tables of
the same shape, or an older copy of the table, e.g. from an `ALTER TABLE`,
not yet compacted away) it isn't guessed at: the candidates are listed,
and `--file <datadir> --table <schema.table> --rocksdb-index <N>` reads
the one you pick (newer tables get higher numbers).

### Migrating to InnoDB (`--rocksdb-to-innodb`)

`--rocksdb-to-innodb` writes every MyRocks table's DDL as `ENGINE=InnoDB`
(partitions included) and leaves out the index `COMMENT`s MyRocks uses for
column family placement; rows are unchanged. It's also the only way to
load a `--format=tsv` dump of MyRocks tables with MySQL Shell's
`util.loadDump()`, which loads data at `READ UNCOMMITTED` - an isolation
level MyRocks refuses (`--format=sql` dumps load into MyRocks fine).

### Limitations

- `wal_compression` WALs, BlobDB blob files, wide-column
  entities and user-defined timestamps aren't read (MyRocks uses none of
  them by default); RocksDB's PlainTable/CuckooTable SST formats aren't
  either (MyRocks never writes them).
- Only primary keys are read - secondary indexes appear in the DDL only.
  A table whose primary key the dictionary lost *and* which is
  partitioned can't be matched to its rows (see above).
- `--dump-page` and `--debug` apply to InnoDB only.

### Validation

Checked end to end, as for InnoDB: each table dumped (from a copy of a
live datadir, a cleanly stopped one, and a crashed one), reloaded, and
compared row for row with the original (`mysqldump --hex-blob
--order-by-primary` of both, identical):

- **Percona Server 8.4.11**: every scalar type (including `JSON`, `BIT`,
  `ENUM`/`SET`, all temporal types with fractional seconds, `BLOB`s up to
  5MB, stored/virtual generated and invisible columns); a primary key over
  every decodable type and collation family (`utf8mb4_0900_ai_ci`,
  `utf8mb4_general_ci`, `utf8mb3_general_ci`, `utf8mb4_bin`, `utf8mb3_bin`,
  `latin1_bin`, `latin1_swedish_ci`, binary, prefix keys, composite keys);
  hidden and promoted-`UNIQUE` primary keys; RANGE/HASH/subpartitioned
  tables; reverse and custom column families; TTL and `INSTANT ADD
  COLUMN` tables; 250,000 bulk-loaded rows; tables after full manual
  compaction; and six column families covering ZSTD (with a trained
  dictionary), Zlib, LZ4, LZ4HC and no compression, all four checksum
  types plus none, `format_version` 2-6 and all four index types.
  `--format=tsv --rocksdb-to-innodb` dumps were loaded with
  `util.loadDump()` and matched as well.
- **Percona Server 9.7.2**: MyRocks tables, a partitioned one among them.
- **Percona Server 8.0.35**: the MyRocks tables of a real 8.0 datadir.
- **Percona Server 5.7.44** and **MariaDB 10.6.28** (`.frm`, older
  `kv_format_version`): the same approach over every type, `latin1`,
  `utf8`, `utf8mb4_bin` and binary keys, hidden keys, partitioned and
  reverse-ordered tables.

So MyRocks extraction is tested on Percona Server 5.7, 8.0, 8.4 and 9.7,
and on MariaDB 10.6.

A RocksDB directory whose dictionary reads fine but lists no tables, with
no data outside `__system__` (an instance where MyRocks was enabled but
never used), is reported as holding no MyRocks tables rather than as
damaged.

Speed: about 600,000 rows/s for a 3-million-row table on 22 cores (the
RocksDB merge itself is sequential; decoding and rendering rows runs in
parallel, as for InnoDB).

## Partitioned tables

Each partition (or subpartition) of an InnoDB table is its own tablespace
file with its own clustered index: `table#p#p0.ibd`, `table#p#p0#sp#s0.ibd`
(5.7 writes `#P#`/`#SP#`, and 8.0+ lower-cases partition names; matching
is case-insensitive). `--source-dir` groups these files by table, and
`--file` on any one partition's file extracts the whole table from the
partition files next to it. The result is one `CREATE TABLE ... PARTITION
BY ...` and one data file with every partition's rows in partition order,
so reloading recreates the same partitions with the same rows. `--verbose`
lists each partition's file, space id, page count and root page.

Where the definition comes from:

- **MySQL 8.0+**: only the first partition's file holds the table's SDI
  (`sdi_tablespace::store_tbl_sdi`), listing every partition and
  subpartition with its root page, index id and tablespace id. Partitions
  are matched to files by tablespace id, not file name. `PARTITION BY` is
  rebuilt following `SHOW CREATE TABLE`'s `generate_partition_syntax`
  (checked against MySQL 8.4's output): `RANGE`, `LIST`, `HASH`, `KEY`
  (including `ALGORITHM = 1`), `LINEAR` variants, `RANGE COLUMNS`/`LIST
  COLUMNS`, `PARTITIONS N`, explicit or default subpartitions, and
  per-partition `COMMENT`s. `DATA DIRECTORY`, `TABLESPACE` and
  `MAX_ROWS`/`MIN_ROWS` partition options aren't reproduced, like the
  table-level options.
- **MySQL 5.6/5.7**: the table's `table.frm` holds the columns, keys and
  the server's own `PARTITION BY` text, used verbatim. The partition order
  comes from 5.6's `.par` file, or - for 5.7's native partitioning, which
  writes none - from the clause itself, with the server's default names for
  unnamed partitions and subpartitions (`p0`, `p1`, ...; `p0sp0`, ...).
  Each partition's root is at page 3, as for any 5.6/5.7 file-per-table
  tablespace.

A partition whose file is missing is reported (`partition p1's tablespace
file wasn't found - its rows are missing from this dump`) and the rest is
still extracted - except an 8.0+ table's first partition, whose SDI is
needed for the columns, so the table fails without it. A file named like a
partition that none of the table's partitions claims (a leftover copy, say)
is reported and left out; `ALTER TABLE`'s `#tmp` files are never picked up.
Warnings name the partition they come from.

Validated against MySQL 5.6.47 (`ha_partition`, `.par` files), 5.7.44
(native partitioning) and 8.4.11 (SDI), with the same ten tables on each:
`RANGE` with `MAXVALUE` and a partition comment, `LIST` with `NULL`,
negative values and a filename-encoded partition name,
`HASH`/`KEY`/`LINEAR HASH`, two-column `RANGE COLUMNS`/`LIST COLUMNS`,
`RANGE` subpartitioned by `HASH` (named) and `KEY` (default names), a table
without a primary key, and one clustered on a promoted `UNIQUE` key, with
multi-page partitions and off-page `TEXT`. Each was dumped, reloaded, and
matched the original's `CHECKSUM TABLE`, per-partition row counts and
`information_schema.PARTITIONS` definitions.

## Shared/system tablespace (`innodb_file_per_table=0`)

With `innodb_file_per_table=0` (the default before MySQL 5.6.6, and common
on older installs), tables have no `.ibd` of their own; their rows live in
the system tablespace, `ibdata1`. Point `--file` at it and name the table
with `--table schema.table` (or InnoDB's own `schema/table` form):

```sh
innodump --file /var/lib/mysql/ibdata1 --table sbtest.sbtest1
```

Without `--table` (or with a wrong one) the error lists every table the
dictionary holds. `--source-dir` does this lookup automatically for every
`.frm` without an `.ibd` (see "Bulk extraction").

The table and its clustered index root are found through InnoDB's classic
internal dictionary, `SYS_TABLES` and `SYS_INDEXES` (`sysdict.go`, checked
against MariaDB 10.6's `dict0boot.h`/`dict0crea.cc`), since the "root page
3" convention doesn't hold in a shared tablespace. The column definitions
still come from `<schema>/<table>.frm` next to `ibdata1`. Validated
end-to-end on MariaDB 11.8.6 with `innodb_file_per_table=0`
(reload-and-diff empty), including a table with a secondary index, and a
table whose dictionary entry points at a different tablespace, which is
reported rather than read from the wrong place. A MySQL 5.5.62 `ibdata1`
has also been read successfully.

Only the system tablespace itself (space 0) is read this way. A table whose
`SYS_TABLES` entry has another space id lives in its own `.ibd` (use
`--file` on it) or in a general tablespace this path can't reach; either
is reported by name and id.

**A consistent copy matters even more here, and `FLUSH TABLES` doesn't
give one.** There's no `FOR EXPORT` for the system tablespace, and plain
`FLUSH TABLES` doesn't write InnoDB's dirty pages (confirmed on a real
server). An `ibdata1` copied off a running server can be stale: a table
created or altered minutes earlier can be missing from `SYS_TABLES` in the
copy, or its data out of date, because those pages hadn't been written back
yet. Ways to get a consistent copy:

- **Stop the server** cleanly first (a clean shutdown flushes every dirty
  page), or use a hot-backup tool that applies the redo log
  (`mariabackup`/`xtrabackup`, then prepare the backup).
- **Force a flush on the running server.** Note the current
  `innodb_max_dirty_pages_pct` and `_lwm`, then:
  ```sql
  SET GLOBAL innodb_max_dirty_pages_pct = 0;
  SET GLOBAL innodb_max_dirty_pages_pct_lwm = 0;
  -- wait for this to reach 0 (seconds on a small/idle instance):
  SHOW GLOBAL STATUS LIKE 'Innodb_buffer_pool_pages_dirty';
  -- then restore the values you noted - leaving both at 0 makes the
  -- server flush far more aggressively from then on:
  SET GLOBAL innodb_max_dirty_pages_pct = 90;      -- example
  SET GLOBAL innodb_max_dirty_pages_pct_lwm = 10;  -- example
  ```
  On MariaDB 11.8.6, a table created moments earlier was invisible to this
  tool until this brought the dirty-page count to 0 (`FLUSH TABLES` didn't),
  with no restart needed.

If a table isn't where `SYS_TABLES` says, the error suggests this.

`--skip-corrupted` only applies once the table is found: a corrupted
`SYS_TABLES`/`SYS_INDEXES` page is always fatal, since there's no other way
to reach the table (as with a corrupted root page - see "Corrupted pages").
On a real `ibdata1` with 50% of its pages randomly damaged, every failure
(dictionary header, `SYS_TABLES`, `SYS_INDEXES`, or the table's own data)
produced a clear error or, for the table's data, a normal
`--skip-corrupted` partial recovery - never a crash or hang.

## INSTANT ADD/DROP COLUMN

A row's physical layout depends on the schema version it was written
under: a row inserted before a later `ADD COLUMN` has no bytes for that
column, and one written before a later `DROP COLUMN` still has them. Each
record's header says enough (see `instant.go`) to tell, per field, whether
it's present, not yet added (use the column's recorded instant default, or
`NULL`), or dropped (no bytes and no NULL-bitmap bit) - mirroring
`Record::GetInsertState`/`InitColumnOffsetsCompactLeaf` in `ibdNinja`.
Dropped columns are left out of the `CREATE TABLE` and the `INSERT`s, as
`SELECT *` would.

## Output notes

- **DDL is best-effort.** Reproduced: column types (with
  `unsigned`/`zerofill`), nullability, `AUTO_INCREMENT`, comments,
  generated-column expressions (SDI only), the `PRIMARY KEY`, secondary
  indexes (`KEY`/`UNIQUE`/`FULLTEXT`/`SPATIAL`, with prefix lengths, `DESC`
  and `INVISIBLE`), `FOREIGN KEY`s (with `ON DELETE`/`ON UPDATE` unless
  they're the implicit `NO ACTION`), and the table's and columns' charsets
  and collations, following `SHOW CREATE TABLE`'s rules for when a column
  needs an explicit `CHARACTER SET`/`COLLATE` (see `sqlout.go`'s
  `GenerateDDL` and `collations.go`). **`DEFAULT` clauses and other
  table-level options are not reproduced** - add them by hand, or use the
  original `SHOW CREATE TABLE` output if you have it.
- **Index prefix lengths** are written in characters, as `SHOW CREATE
  TABLE` does (the dictionary stores bytes); a key part covering its whole
  column gets no prefix, and a `BLOB`/`TEXT` key part always keeps its own.
- **`AUTO_INCREMENT=N`** isn't in the SDI; it's approximated as the highest
  value in the column plus one. That's exact unless rows at the top of the
  range were deleted or the counter was moved ahead with `ALTER TABLE ...
  AUTO_INCREMENT=N`, in which case the real counter is higher.
- **String columns** are written as their raw stored bytes in a quoted
  literal. For `utf8mb3`/`utf8mb4` columns that's valid UTF-8. The `.sql`
  files don't set a client character set, so for other character sets load
  them with a session character set matching the data; the text may also
  not display correctly in an editor.
- **BLOB/BINARY/VARBINARY/GEOMETRY** values are written as `X'...'` hex
  literals, which is lossless.
- **JSON** is decoded from MySQL's binary JSON format back to text.
- **ENUM/SET** values are written as their labels, not their internal
  numbers.
- A row that fails to decode (corrupt data, or an unsupported nested case)
  is skipped with a warning and a `-- skipped a row ...` comment in the
  data file, rather than aborting the extraction.

## Recovering deleted rows

A `DELETE` doesn't remove a row's bytes right away. InnoDB first sets the
record's delete mark. Later, once no transaction can still need it, purge
unlinks it from the page's record chain and puts its space on the page's
free list, without erasing it. If enough of a page empties out, InnoDB can
merge the page into a neighbour and free it, again without erasing it
until the space is reused.

By default `innodump` only decodes live rows, like `SELECT`.
`--deleted-only` instead recovers rows still recognizable as deleted, from
three places:

- **delete-marked** records still on a live page's record chain (not yet
  purged);
- **purged** records still on a live page's free list (not yet
  overwritten);
- either of the above on a page that has since been **freed** (merged
  away, but not yet reused).

Deleting and purging normally rewrite only a record's header links, not
its field bytes, so such rows usually decode completely. A reused slot or
page can leave a partial or garbled row, though, and an off-page BLOB/TEXT
value of a deleted row may already be freed. Exact duplicate rows (a page
split copies a record verbatim, so two copies can both survive) are
dropped; rows sharing a primary key but differing elsewhere are all kept,
since they may be different historical versions of the same row.

```
$ innodump --file table.ibd --deleted-only
=== innodump 0.7.8 ===
Table:       sbtest.sbtest1 (row_format=DYNAMIC)
Columns:     4 output (6 total incl. system/hidden)
Schema file: sqldump_2026-09-08_21.04/sbtest.sbtest1-schema.sql
Data file:   sqldump_2026-09-08_21.04/sbtest.sbtest1-data-deleted.sql (1000 delete-marked row(s) written)
```

The data file gets a `-deleted` suffix (`...-data-deleted.sql`, or
`<schema>@<table>-deleted.tsv` and its companion files for `tsv`), so it
never collides with a normal dump in the same `--out-dir`. The schema file
is the same as for a normal dump.

There's no guaranteed recovery window: purge can run at any time, freed
space can be reused at any time, and InnoDB can reorganize a page's free
space independently of any single record. The sooner the file is copied
after the delete, the better; a busy `ibdata1` tends to reuse space faster
than a dedicated `.ibd`. Everything else in this README still applies - the
same schema requirements, the same corruption and truncation handling, and
the same "not a backup" caveat.

## Corrupted pages

Every page is checked before its records are read - checksum (the
server's own CRC-32C, or the legacy `innodb` hash used by MySQL 5.6, and
MariaDB's `full_crc32`), LSN consistency, page type and index id (see
`page.go`'s `checkPage`). By default a corrupted page is fatal: extraction
stops with the page number, index and reason, because silently skipping it
would make an incomplete dump look complete.

With `--skip-corrupted`, each corrupted page is noted (a warning, and a
`-- skipped corrupted page ...` comment in the data file) and the walk
continues from that page's own "next page" pointer, since damage often
leaves the header intact. If that pointer is unusable (null, looping, or
past the end of the file), the walk ends there: rows decoded so far are
kept, but **intact pages further along the chain are not reached**. The
summary counts the skipped pages.

`ROW_FORMAT=COMPRESSED` pages use a checksum this tool doesn't implement,
so only the "unallocated all-zero page" check runs for them; a corrupted
compressed page will more likely show up as a decode error.

Tested against a real MariaDB 11.8.6 `ibdata1` with 15% and 50% of its
pages randomly damaged (1-10 flipped bytes each, across the dictionary and
the `mysql` and user tables): every run finished promptly with no crash or
hang, and every skipped page was reported with a reason. Recovery is
limited as described above - a corrupted root page leaves nothing to walk,
a corrupted `SYS_TABLES`/`SYS_INDEXES` page is always fatal, and a broken
next-page pointer ends the walk - so heavy damage loses more than just the
damaged pages.

## Truncated files

A page number beyond the end of the file is reported as `page N is past
the end of the file (it holds only M page(s)) - the file looks truncated
(not fully copied), rather than corrupted`. That wording is a diagnosis,
not a certainty: an interrupted copy is the usual cause, but a corrupted
pointer - or a decoding bug in this tool, as has happened in earlier
builds - reads the same way. Check the source file's size if it matters.

Without `--skip-corrupted` this is fatal. With it, it's reported like a
corrupted page and the walk ends there (there's no page to read a "next"
pointer from), and the summary adds a note:

```
Data file:   sqldump_.../schema.table-data.sql (11078 row(s) written, 1 corrupted page(s) skipped - see warnings above)
Note:        the source file looks truncated (not fully copied) - recovered every row up to where it ends; anything stored after that point is missing.
```

`--source-dir` marks such tables `[source file truncated]` in its
per-table lines and counts them in its final tally.

### When even finding the leftmost leaf isn't possible

The walk starts by descending from the root through the non-leaf levels to
the leftmost leaf. If that descent hits a page past the end of the file,
there's no way to know which leaf comes first, so with `--skip-corrupted`
`innodump` falls back to reading every page in the file and decoding every
leaf page that carries this index's id, in physical order:

```
warning: corrupted page 4 (index "PRIMARY", id 1586): can't navigate from the root to the leftmost leaf (page 1702109297 is past the end of the file (it holds only 54 page(s)) - the file looks truncated (not fully copied), rather than corrupted) - falling back to an out-of-order scan of every leaf page the file does hold
Data file:   sqldump_.../schema.table-data.sql (1000 row(s) written, 1 corrupted page(s) skipped - see warnings above)
Note:        the source file looks truncated (not fully copied) - recovered every row up to where it ends; anything stored after that point is missing.
```

This is a best-effort scan, not a validated walk:

- rows come out in file order, not primary-key order;
- a page that's missing from the file leaves an unknown gap;
- **freed leaf pages of the same index that were never reused are decoded
  too**, so the output can contain stale rows, including several rows with
  the same primary key. Loading it into a table with that key will fail on
  duplicates; deduplicate first, or treat it as raw recovered data.

Without `--skip-corrupted`, this is a hard failure like any other
truncation.

## Performance

Reading and validating leaf pages and following their next-page pointers
happens strictly in order, which keeps `--skip-corrupted`'s behaviour and
the output order (rows and `-- skipped ...` comments alike) deterministic.
Decoding rows and rendering them as SQL or TSV runs in parallel across
`GOMAXPROCS` cores, in batches of pages, and the results are written in the
original order through a buffered writer (see `btree.go`'s `WalkRows` and
`decodeBatch`).

## Progress

When stderr is a terminal, a progress line appears once an extraction has
run long enough to be worth showing: a percentage based on the
tablespace's page count, a row count, and elapsed time. It's approximate -
not every page belongs to the table being extracted, so it may not reach
100% for a small table in a big shared tablespace. Warnings printed
mid-run clear the line first so they aren't spliced into it. It never
touches stdout or the output files. `--no-progress` or `NO_PROGRESS=1`
turns it off.

## Validation

**MySQL 8.0/8.4.** Checked end to end against MySQL 8.0.19, 8.0.46 and
8.4.11: each test table was dumped, the `*-schema.sql` and `*-data.sql`
reloaded into a fresh database, and diffed against the original (`SELECT *
FROM orig EXCEPT SELECT * FROM reloaded`, both directions, empty). The
tables covered every scalar type (including `DECIMAL`, `BIT`, `ENUM`,
`SET`, `JSON` and all temporal types), off-page BLOB/TEXT values up to 32
KB across several LOB pages, tables without an explicit `PRIMARY KEY`,
composite keys, `AUTO_INCREMENT` with secondary indexes and deleted rows,
an 8.0.19 table with two old-style instant `ADD COLUMN`s, an 8.0.46 table
with `ADD`/`DROP COLUMN` across five row versions, `REDUNDANT` tables with
and without off-page BLOBs, and `COMPRESSED` tables at `KEY_BLOCK_SIZE=8`
(NULLs, about half the rows deleted, a non-empty modification log, and a
3,000-row table whose root page content lived entirely in its modification
log). Also checked against the Sakila `rental` table (16,044 rows,
`COMPRESSED`) on Percona Server 8.4.8. MySQL 9.x and 26.x files are read
by the same code, but aren't part of this reload-and-diff validation.

**MySQL 5.6/5.7 (`.frm`).** Validated the same way against MySQL 5.6.51
and 5.7.44: `COMPACT`, `DYNAMIC` and `COMPRESSED` tables covering
`INT`/`VARCHAR`/`DECIMAL`/`DATETIME`/`TINYINT`/`TEXT`/`ENUM`/`SET`, a
composite `PRIMARY KEY`, `UNIQUE` and plain secondary indexes,
`AUTO_INCREMENT`, `FLOAT UNSIGNED`, `NULL`s and `utf8mb4` data, plus a
Percona Server 5.6.47 table using the legacy `innodb` checksum. All reloaded
byte-for-byte identical.

**A whole 5.6.47 datadir.** Two application schemas plus `mysql` - 121
`.ibd` files (including a partitioned `ha_partition` table) and 30 MyISAM
tables, 16.3M rows - were dumped with `--source-dir --format tsv
--myisam-to-innodb` and loaded into MySQL 8.0.46 with `util.loadDump()`.
Every table came through as InnoDB. Every MyISAM table and a selection of
the InnoDB ones were compared with the original 5.6 server and matched row
for row (the partitioned table via `CHECKSUM TABLE`). The only two
tables 8.0 refused were original `COMPACT` InnoDB tables with rows over its
8126-byte in-page limit, which 5.6 had allowed without strict mode;
`sessionInitSql: ["SET SESSION innodb_strict_mode=0"]` loads them.

**Corruption.** Flipping a byte in one leaf page of a 2,000-row, 72-leaf
table: the default run stopped with the exact page, index and reason, and
`--skip-corrupted` recovered exactly the 1,972 rows outside that page. See
"Corrupted pages" for the heavier random-damage tests.

## Acknowledgements

The RocksDB formats (SST, WAL, MANIFEST) and MyRocks' key/value encoding
were worked out from the RocksDB and MyRocks source Percona Server 8.4
bundles (`storage/rocksdb`), with Percona Server 5.7's and MariaDB's
MyRocks for their older rules, and checked against RocksDB's own
`sst_dump` and `ldb` tools. This project is not affiliated with Meta,
Percona or the RocksDB project.

The on-disk page, record, SDI and LOB formats were cross-referenced
against the public `mysql-server` source (`storage/innobase`) and against
[KernelMaker/ibdNinja](https://github.com/KernelMaker/ibdNinja) (GPL-3.0),
which was used throughout development to validate these layouts,
particularly the SDI B+tree walk and the modern LOB format. This project is
not affiliated with Oracle or with the ibdNinja project.

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

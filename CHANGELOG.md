## 0.8.2	26-Oct-2026

## NEW FEATURES

* The generated `CREATE TABLE` now has each column's `DEFAULT` and `ON
  UPDATE` clauses, as `SHOW CREATE TABLE` writes them: literals,
  `DEFAULT NULL`, `CURRENT_TIMESTAMP[(n)]`, `BIT` defaults and expression
  defaults. They come from the SDI (MySQL 8.0+), or are decoded from a
  `.frm` file's default-values record (MySQL 5.5-5.7, MariaDB, including
  MariaDB's expression defaults). Checked by recreating test tables from
  the generated DDL on MySQL 5.5, 5.7, 8.0 and MariaDB 10.11 and
  comparing `SHOW CREATE TABLE` output.
* `INVISIBLE` columns (MySQL 8.0.23+) are marked `/*!80023 INVISIBLE */` in
  the generated `CREATE TABLE`, as `SHOW CREATE TABLE` writes them - which
  includes the `my_row_id` column of a table with a generated invisible
  primary key (`sql_generate_invisible_primary_key`). Their data was
  already dumped (every `INSERT` and `LOAD DATA` names its columns).
* `CHECK` constraints (MySQL 8.0.16+) are in the generated `CREATE TABLE`,
  in the order `SHOW CREATE TABLE` lists them (by name), with `NOT
  ENFORCED` ones marked `/*!80016 NOT ENFORCED */`. They were left out.
* MyISAM tables with `BIT` columns can be extracted: MyISAM keeps a
  `BIT(n)` column's `n%8` leftover bits in the record's null bitmap (and a
  `BIT(1)`-`BIT(7)` has no field in the record at all), which is now
  decoded. Such a table used to be refused with a "record length ...
  doesn't match ... .MYI" error.
* A spatial column's `SRID` attribute (MySQL 8.0.3+) is kept in the
  generated `CREATE TABLE`, as `/*!80003 SRID n */`, where `SHOW CREATE
  TABLE` writes it. Without it, the recreated column accepted geometries in
  any spatial reference system, and a `SPATIAL` index on it was ignored by
  the optimizer.

## BUGS FIXED

* A `FULLTEXT` index's auxiliary tables (`fts_..._index_1.ibd` ...
  `fts_..._config.ibd`, `FTS_..._CONFIG.ibd` on 5.6/5.7) were taken for
  ordinary tables, each failing with a warning ("SDI is missing the
  DB_TRX_ID/DB_ROLL_PTR system columns", or a missing `.frm`). They're
  InnoDB-internal and rebuilt by the server from the table's `FULLTEXT
  KEY`, so `--source-dir` now skips them, saying how many, and `--file` on
  one explains what it is.
* `FULLTEXT` and `SPATIAL` keys of a MySQL 5.x InnoDB table (read from its
  `.frm`) were left out of the generated `CREATE TABLE`, so the recreated
  table had no such index. A 5.x `.frm` `GEOMETRY` subtype (`point`,
  `polygon`, ...) also came out as plain `geometry`.
* `FLOAT(M,D)`/`DOUBLE(M,D)` columns read from a MySQL 5.x `.frm` lost
  their `(M,D)` in the generated `CREATE TABLE`, so the recreated column
  no longer rounded values to `D` decimals.
* A functional index (`KEY k ((a + b))`, MySQL 8.0.13+) was written as an
  extra `!hidden!k!0!0` column with a plain index on it; it's now written
  as the index's own expression, `KEY k (((a + b)))`.

## 0.8.1	13-Oct-2026

## NEW FEATURES

* `--examine`: a read-only health check of a datadir (`--source-dir`) or a
  single table (`--file`). Every table is read and every row decoded
  exactly as an extraction would, but nothing is written; it carries on
  past damage to count all of it. One line per table (engine, rows, OK /
  DAMAGED / UNREADABLE), then a summary per engine and a list of every
  table with problems and the first problem found; exits non-zero if
  there are any. `--verbose` lists every damaged page/block and row.
* `--direct-io` and `--io-limit RATE`, for extracting next to a busy server
  without page-cache pressure or saturating its disks: source files are
  read with `O_DIRECT` (`F_NOCACHE` on macOS; `POSIX_FADV_DONTNEED` where
  the filesystem refuses `O_DIRECT`), output is flushed and dropped from
  the cache as it's written, and a token bucket caps the bytes read and
  written per second. Applies to every source and output file.

## BUGS FIXED

* A tablespace whose page 0 is blank was reported as "no SDI (a pre-8.0
  file)"; it's now reported as a damaged tablespace header.

## 0.8.0	13-Oct-2026

## NEW FEATURES

* MyRocks (`ENGINE=ROCKSDB`) support: tables are read straight from the
  datadir's RocksDB files (`.rocksdb/`, `#rocksdb/` on MariaDB) - MANIFEST,
  SST files and write-ahead logs, merged into each key's current state -
  with no RocksDB library or server. Schema from the `.sdi` (Percona Server
  8.0+) or `.frm` (Percona Server 5.7, MariaDB). Use `--file <datadir>
  --table schema.table`, `--file` on the table's `.sdi`/`.frm`, or
  `--source-dir`, which now picks up MyRocks tables alongside InnoDB and
  MyISAM ones. Tested on Percona Server 5.7, 8.0, 8.4 and 9.7 (9.7.2,
  including a partitioned table), and MariaDB 10.6. See the README's
  "MyRocks support".
* SST format_version 0-6, every checksum type (CRC32c, xxHash, xxHash64,
  XXH3), LZ4/LZ4HC/ZSTD (with dictionaries)/Zlib/Snappy/BZip2 blocks, every
  index type; bulk-loaded (ingested) files, range deletions, reverse and
  custom column families, partitioned tables, hidden and promoted-`UNIQUE`
  primary keys, TTL tables, `INSTANT ADD COLUMN`.
* Two-phase-commit transactions left prepared in the WAL are resolved
  against the binlog's XID events, as the server's crash recovery does.
* Damage tolerance: damaged data blocks are skipped with
  `--skip-corrupted`; SST files with a damaged index or footer, or cut short,
  are salvaged by scanning them block by block; missing SST files, damaged
  WAL records and a lost MANIFEST are reported and worked around. Tables
  that drop out of a damaged MyRocks dictionary are matched to their rows
  when unambiguous; `--rocksdb-index N` picks one by hand. A RocksDB
  directory with no MyRocks tables at all is reported as such, not as
  damaged.
* `--deleted-only` recovers MyRocks rows whose deletion hasn't been
  compacted away yet.
* `--rocksdb-to-innodb` writes MyRocks tables' DDL as `ENGINE=InnoDB`
  (needed for MySQL Shell's `util.loadDump()`, which can't load into
  MyRocks). `--rocksdb-dir` names a RocksDB directory outside the datadir.
* InnoDB transparent page compression (`COMPRESSION='zlib'` or `'lz4'`):
  pages are decompressed as the server's own I/O layer does it before
  their checksum is checked, so the SDI, rows, LOB pages, `--deleted-only`
  and `--skip-corrupted` all work on such tables, and the DDL keeps the
  table's `COMPRESSION=` option. Verified against Percona Server
  8.0.35/8.0.46 zlib and LZ4 tables.

## BUGS FIXED

* A page-compressed InnoDB tablespace failed with `reading SDI: corrupt
  SDI page 3: expected infimum` (see above; MySQL's own `ibd2sdi` fails on
  these files too).
* Generated column expressions were written with backslash-escaped quotes
  (`_utf8mb4\'x\'`), which isn't valid SQL.
* STORED generated columns were included in `INSERT`s, which the server
  rejects on reload.
* MyISAM `JSON` and `GEOMETRY` columns were read with a 2-byte length
  prefix instead of 4.
* A `FLOAT` holding +/-FLT_MAX was written as a value the server rejects
  as out of range.
* A deleted row whose off-page BLOB purge already freed (`--deleted-only`)
  is now reported as such, instead of `reading page 4294967295: EOF`.
* Table and index `COMMENT`s are now kept in the generated DDL.

## DEPENDENCIES

* Added `github.com/zeebo/xxh3` (pure Go) for RocksDB's XXH3 block
  checksums.

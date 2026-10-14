## Unreleased

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

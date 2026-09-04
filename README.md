# 🗄️ ibd-extractor

`ibd-extractor` is an offline reader for a single MySQL 8.0/8.4 InnoDB
tablespace file (`table.ibd`, file-per-table mode). Given `table.ibd`, it
produces:

- `table-schema.sql` — a best-effort `CREATE TABLE` statement, reconstructed
  from the table's own embedded dictionary information (SDI).
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
| `--out-dir DIR` | Directory for the two output files (default: alongside `--file`). |
| `--table NAME` | Which table to extract, if the file's SDI unexpectedly holds more than one. |
| `--limit N` | Stop after N rows (0 = all). |
| `--debug` | Print each record's decoded field byte-ranges as they're read. |
| `--dump-page N` | Hex-dump one page and its record chain, then exit. |
| `--version` | Print version and exit. |

## Scope (v1)

Supported: MySQL **8.0.16+ and 8.4.x** tablespaces, `ROW_FORMAT=DYNAMIC` or
`COMPACT`, uncompressed, non-partitioned, file-per-table `.ibd` files whose
columns have never been through an `INSTANT ADD/DROP COLUMN` (each of these
is checked explicitly — the tool refuses the table with a specific reason
rather than guessing).

**Not supported in v1** (all detected and reported, not silently
mis-decoded):

- `ROW_FORMAT=COMPRESSED` and page-compressed tablespaces.
- `ROW_FORMAT=REDUNDANT` (pre-5.0.3 record format).
- Encrypted tablespaces.
- Partitioned tables.
- Tables with `INSTANT ADD COLUMN` / `INSTANT DROP COLUMN` history (MySQL
  8.0.12+). This is common in practice for long-lived tables that have had
  `ALTER TABLE ... ADD COLUMN` run on them — a v2 goal is to support it
  (see `schema.go` and `record.go`'s package comments for exactly what
  additional bookkeeping that needs).
- Full-text and spatial indexes (irrelevant here: only the clustered index
  is ever walked).
- Pre-8.0 tablespaces (no embedded SDI to read at all).

## Output notes

- **DDL is best-effort.** Column types (including `unsigned`/`zerofill`),
  nullability, comments, generated-column expressions, and the `PRIMARY
  KEY` are reproduced from the SDI. `DEFAULT` clauses, `AUTO_INCREMENT`
  counters, secondary indexes, and table-level options are not — add them
  by hand if you need an exact round-trip DDL, or restore alongside the
  original `SHOW CREATE TABLE` output if you have it.
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

## Validation

This was cross-checked end to end against real MySQL 8.0.46 and 8.4.11
servers: for every test table (plain types, every scalar type including
`DECIMAL`/`BIT`/`ENUM`/`SET`/`JSON`/all temporal types, off-page
BLOB/TEXT values up to 32 KB spanning multiple LOB pages, tables with no
explicit `PRIMARY KEY`, and composite-key tables), reloading the generated
`*-schema.sql` + `*-data.sql` into a fresh database and diffing every row
against the original table (`SELECT * FROM orig EXCEPT SELECT * FROM
reloaded`, both directions) came back empty.

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

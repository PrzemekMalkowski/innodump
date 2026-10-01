// tsvdump.go: writes a dump directory compatible with MySQL Shell's
// util.loadDump() - the same layout util.dumpSchemas()/dumpTables() produce,
// using that utility's default TSV dialect for the data files themselves
// (FIELDS TERMINATED BY '\t' ESCAPED BY '\\' LINES TERMINATED BY '\n', no
// FIELDS ENCLOSED BY - see values.go's escapeTSVField).
//
// This was reverse-engineered from MySQL Shell's own source
// (modules/util/dump/{dumper,text_dump_writer}.cc and modules/util/common/
// dump/*, and modules/util/load/dump_reader.cc for what a loader actually
// requires) and confirmed against a real util.dumpTables()/util.loadDump()
// round trip (MySQL Shell 8.4.8, dump format version 2.0.1) covering every
// scalar type this tool decodes, NULLs, and a BLOB large enough to need
// multi-byte base64 padding.
//
// A dump directory holds, per MySQL Shell's own naming:
//   - "@.json"            - root metadata: which schemas/tables the dump
//     holds and their basenames (percent-encoded schema/table names - see
//     encodeBasenamePart - since a name can contain bytes unsafe for a
//     filename).
//   - "@.done.json"       - marks the dump complete; util.loadDump() waits
//     for more data if this is missing (it's meant to support dumping
//     while a load is already in progress against the same directory).
//   - "<schema>.sql"       - one "CREATE DATABASE IF NOT EXISTS" per schema.
//   - "<schema>.json"      - which tables/views that schema holds.
//   - "<schema>@<table>.sql"  - that table's CREATE TABLE (reuses
//     GenerateDDL verbatim - see sqlout.go).
//   - "<schema>@<table>.json" - column list, TSV dialect, and the other
//     per-table options util.loadDump()'s chunk importer needs (this is
//     fed to it almost directly - see Dump_reader::Table_info::
//     update_metadata upstream).
//   - "<schema>@<table>.tsv" (or "...tsv.zst" - see compress.go) - the
//     table's data. This tool never splits a table across multiple chunk
//     files, so - matching "chunking": false in the table's own metadata
//     (below) - this is the plain, un-chunked filename; MySQL Shell
//     reserves "<basename>@@<index>.<ext>" (a double "@@" marking the last
//     chunk) for when chunking is turned on.
//
// Deliberately not reproduced: compression (dumps are written uncompressed
// - MySQL Shell's loader handles both fine), the checksum/.idx sidecar
// files (both optional - see Dump_reader::open/parse_done_metadata
// upstream, which only warn if either is missing), partitions, views,
// triggers, and histograms (this tool doesn't extract any of those), and a
// schema-level DEFAULT CHARACTER SET/COLLATE in "<schema>.sql" (this tool
// has no access to information_schema.SCHEMATA, only to the .ibd file
// itself - every table's own DEFAULT CHARSET/COLLATE, which is what
// actually governs its stored data, still comes through in full on each
// table's own CREATE TABLE).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// dumpFormatVersion is the dump-metadata format version this tool claims in
// "@.json" - not to be confused with this tool's own --version. 2.0.1 is
// the version MySQL Shell 8.0.34+/8.4.x/9.x itself writes; a loader refuses
// a dump whose major.minor exceeds what it was built to support, so this
// must never be bumped ahead of what's actually been validated (see
// validate_dumper_version in MySQL Shell's modules/util/common/dump/
// dump_version.cc).
const dumpFormatVersion = "2.0.1"

// encodeBasenamePart percent-encodes name the same way MySQL Shell's dump
// utility does when turning a schema/table name into a filesystem-safe
// basename (modules/util/common/dump/utils.cc's hexencode): every ASCII
// byte outside [A-Za-z0-9._~-] becomes "%XX" (uppercase hex); a raw
// (multi-byte UTF-8) byte 0x80-0xFF passes through unescaped.
func encodeBasenamePart(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c >= 0x80:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func encodeSchemaBasename(schema string) string { return encodeBasenamePart(schema) }

func encodeTableBasename(schema, table string) string {
	return encodeSchemaBasename(schema) + "@" + encodeBasenamePart(table)
}

// columnNeedsBase64 reports whether c is one of the types MySQL Shell's dump
// utility treats as "CSV-unsafe" (Instance_cache::Column::csv_unsafe
// upstream: BLOB/BINARY/VARBINARY, BIT, GEOMETRY, VECTOR) and so encodes as
// base64 rather than writing its raw bytes straight into the TSV field.
func columnNeedsBase64(c *Column) bool {
	switch c.DDType {
	case ddBit, ddGeometry, ddVector:
		return true
	case ddVarchar, ddVarString, ddString, ddTinyBlob, ddMediumBlob, ddBlob, ddLongBlob:
		return c.IsBinary()
	default:
		return false
	}
}

// --- metadata JSON shapes (field names/casing match MySQL Shell's own) ---

type tsvRootMeta struct {
	Dumper               string            `json:"dumper"`
	Version              string            `json:"version"`
	Origin               string            `json:"origin"`
	Schemas              []string          `json:"schemas"`
	Basenames            map[string]string `json:"basenames"`
	DefaultCharacterSet  string            `json:"defaultCharacterSet"`
	TzUtc                bool              `json:"tzUtc"`
	BytesPerChunk        uint64            `json:"bytesPerChunk"`
	Consistent           bool              `json:"consistent"`
	CompatibilityOptions []string          `json:"compatibilityOptions"`
	Capabilities         []any             `json:"capabilities"`
	Checksum             bool              `json:"checksum"`
	Begin                string            `json:"begin"`
	ServerVersion        string            `json:"serverVersion"`
}

type tsvDoneMeta struct {
	End            string                       `json:"end"`
	DataBytes      uint64                       `json:"dataBytes"`
	TableDataBytes map[string]map[string]uint64 `json:"tableDataBytes"`
	TableRows      map[string]map[string]uint64 `json:"tableRows"`
	ChunkFileBytes map[string]uint64            `json:"chunkFileBytes"`
}

type tsvSchemaMeta struct {
	Schema           string            `json:"schema"`
	IncludesDdl      bool              `json:"includesDdl"`
	IncludesViewsDdl bool              `json:"includesViewsDdl"`
	IncludesData     bool              `json:"includesData"`
	Tables           []string          `json:"tables"`
	Views            []string          `json:"views"`
	Basenames        map[string]string `json:"basenames"`
}

type tsvTableOptions struct {
	Schema                   string            `json:"schema"`
	Table                    string            `json:"table"`
	Columns                  []string          `json:"columns"`
	DecodeColumns            map[string]string `json:"decodeColumns,omitempty"`
	DefaultCharacterSet      string            `json:"defaultCharacterSet"`
	FieldsTerminatedBy       string            `json:"fieldsTerminatedBy"`
	FieldsEnclosedBy         string            `json:"fieldsEnclosedBy"`
	FieldsOptionallyEnclosed bool              `json:"fieldsOptionallyEnclosed"`
	FieldsEscapedBy          string            `json:"fieldsEscapedBy"`
	LinesTerminatedBy        string            `json:"linesTerminatedBy"`
}

type tsvTableMeta struct {
	Options      tsvTableOptions   `json:"options"`
	Triggers     []string          `json:"triggers"`
	IncludesData bool              `json:"includesData"`
	IncludesDdl  bool              `json:"includesDdl"`
	Extension    string            `json:"extension"`
	Chunking     bool              `json:"chunking"`
	Compression  string            `json:"compression"`
	PrimaryIndex []string          `json:"primaryIndex"`
	Partitions   []string          `json:"partitions"`
	Basenames    map[string]string `json:"basenames"`
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}

// tsvSchemaState is one schema's accumulated bookkeeping across every table
// AddTable has written for it so far - a dump directory can span many .ibd
// files (see --source-dir), so a schema's own table list is only final once
// every input file has been processed (Finish writes it then).
type tsvSchemaState struct {
	basename       string
	tables         []string // first-seen order
	tableBasenames map[string]string
}

// TSVDump accumulates a MySQL-Shell-compatible dump directory across one or
// more calls to AddTable (one per extracted table, however many .ibd files
// they came from), and writes the directory-level metadata files once every
// table has been added - see Finish.
type TSVDump struct {
	dir         string
	schemas     map[string]*tsvSchemaState
	schemaOrder []string

	totalDataBytes uint64
	tableDataBytes map[string]map[string]uint64
	tableRows      map[string]map[string]uint64
	chunkFileBytes map[string]uint64

	// sourceVersionID is the highest mysqld_version_id (MMmmpp, e.g. 80046 -
	// see formatMySQLVersion) seen across every table added so far, read
	// from each 8.0+ table's own embedded SDI (0 for a pre-8.0/.frm-derived
	// table, which carries no such version at all). util.loadDump() refuses
	// a dump whose "serverVersion" looks more than one major release behind
	// the target server, so Finish falls back to a plausible pre-8.0
	// placeholder rather than leaving it unset entirely - see Finish.
	sourceVersionID uint32
}

// noteSourceVersion records src (a table's own SDI mysqld_version_id, or 0
// if it has none) as this dump's source-version hint, keeping the highest
// one seen so far.
func (d *TSVDump) noteSourceVersion(src uint32) {
	if src > d.sourceVersionID {
		d.sourceVersionID = src
	}
}

// NewTSVDump creates (or reuses) dir as a dump directory's root.
func NewTSVDump(dir string) (*TSVDump, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating output directory %s: %w", dir, err)
	}
	return &TSVDump{
		dir:            dir,
		schemas:        map[string]*tsvSchemaState{},
		tableDataBytes: map[string]map[string]uint64{},
		tableRows:      map[string]map[string]uint64{},
		chunkFileBytes: map[string]uint64{},
	}, nil
}

// looksLikeExistingDump reports whether dir already holds a dump's root
// metadata file, for confirmOverwriteDir's use.
func looksLikeExistingDump(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "@.json"))
	return err == nil
}

// confirmOverwriteDir is confirmOverwrite's TSV-mode counterpart: a dump
// directory can end up with dozens of files across many tables, so rather
// than enumerate every one of them up front (as the SQL path's
// confirmOverwrite does), this only checks for the root metadata file that
// marks dir as an existing dump.
func confirmOverwriteDir(dir string, yes bool) error {
	if yes || !looksLikeExistingDump(dir) {
		return nil
	}
	if !isTTY(os.Stdin) {
		return fmt.Errorf("output directory %s already holds a dump (pass --yes, or set YES=1, to overwrite)", dir)
	}
	fmt.Printf("The output directory %s already holds a dump.\n", dir)
	fmt.Printf("Overwrite? [y/N]: ")
	var answer string
	fmt.Scanln(&answer)
	answer = strings.TrimSpace(answer)
	if answer != "y" && answer != "Y" && !strings.EqualFold(answer, "yes") {
		return fmt.Errorf("not overwriting existing dump directory: %s", dir)
	}
	return nil
}

func (d *TSVDump) ensureSchema(schema string) (*tsvSchemaState, error) {
	if s, ok := d.schemas[schema]; ok {
		return s, nil
	}
	s := &tsvSchemaState{basename: encodeSchemaBasename(schema), tableBasenames: map[string]string{}}
	d.schemas[schema] = s
	d.schemaOrder = append(d.schemaOrder, schema)

	ddl := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s;\n", backquote(schema))
	if err := os.WriteFile(filepath.Join(d.dir, s.basename+".sql"), []byte(ddl), 0644); err != nil {
		return nil, fmt.Errorf("writing %s.sql: %w", s.basename, err)
	}
	return s, nil
}

// AddTable extracts one table's DDL and (unless ddlOnly) data into this
// dump's directory, and folds it into the directory-level bookkeeping that
// Finish later writes out. progressLabel is passed straight to newProg
// (progress.go). sourceVersionID is the table's own SDI mysqld_version_id
// (0 if it has none - see noteSourceVersion). compression/compressionLevel
// select the data file's on-disk compression - see compress.go. With
// deletedOnly, every file this writes for the table (DDL, options JSON, and
// data) gets a "-deleted" suffix on its basename - the schema/root metadata
// still points at that basename (see schemaState.tableBasenames), so the
// dump directory stays internally consistent and util.loadDump()-loadable,
// while never colliding with a normal dump of the same table written to the
// same directory. Returns the same row/page/truncated-file counts
// WalkRows' caller in main.go already tracks for the SQL path (see
// writeSQLTable's own comment on truncated).
func (d *TSVDump) AddTable(rw RowWalker, t *Table, outCols []*Column, ddlOnly bool, limitRows int,
	skipCorrupted, deletedOnly bool, progressLabel string, sourceVersionID uint32,
	compression compressionKind, compressionLevel int) (nOK, nErr, nCorruptPages int, truncated bool, err error) {
	d.noteSourceVersion(sourceVersionID)
	schemaState, err := d.ensureSchema(t.SchemaRef)
	if err != nil {
		return 0, 0, 0, false, err
	}
	tableBasename := encodeTableBasename(t.SchemaRef, t.Name)
	if deletedOnly {
		tableBasename += "-deleted"
	}
	schemaState.tables = append(schemaState.tables, t.Name)
	schemaState.tableBasenames[t.Name] = tableBasename

	writeTableDDL := func() error {
		return os.WriteFile(filepath.Join(d.dir, tableBasename+".sql"), []byte(GenerateDDL(t)), 0644)
	}
	if err := writeTableDDL(); err != nil {
		return 0, 0, 0, false, fmt.Errorf("writing %s.sql: %w", tableBasename, err)
	}

	ext := compression.extension()
	var dataBytes uint64
	if !ddlOnly {
		dataPath := filepath.Join(d.dir, tableBasename+"."+ext)
		f, ferr := os.Create(dataPath)
		if ferr != nil {
			return 0, 0, 0, false, fmt.Errorf("writing %s: %w", dataPath, ferr)
		}
		defer f.Close()
		dw, dwErr := newDataWriter(f, compression, compressionLevel)
		if dwErr != nil {
			return 0, 0, 0, false, dwErr
		}

		autoIncIdx := -1
		for i, c := range outCols {
			if c == t.AutoIncrementCol {
				autoIncIdx = i
			}
		}
		var maxAutoInc uint64

		pr := newProg(progressLabel, rw.ProgressTotal)
		onCorrupt := func(cp CorruptPage) {
			nCorruptPages++
			if cp.Truncated {
				truncated = true
			}
			// pr.Warnf (not a plain Fprintf) clears the bar's own
			// in-progress line first, so this can't land mid-redraw and
			// splice into it - see progress.go.
			pr.Warnf("%s corrupted %s: %s\n", warn("warning:"), t.corruptUnitLabel(cp.PageNo), cp.Reason)
		}
		onRow := func(roe RowOrError) bool {
			pr.set(roe.ProgressBase + int64(roe.PageNo))
			if roe.Err != nil {
				nErr++
				pr.Warnf("%s %s: %v\n", warn("warning:"), t.corruptRowLabel(roe.PageNo, roe.RecOff), roe.Err)
				return limitRows == 0 || nOK+nErr < limitRows
			}
			line := strings.Join(roe.Row.Values, "\t") + "\n"
			n, _ := io.WriteString(dw, line)
			dataBytes += uint64(n) // uncompressed size, regardless of compression
			if autoIncIdx >= 0 {
				if v, perr := strconv.ParseUint(roe.Row.Values[autoIncIdx], 10, 64); perr == nil && v > maxAutoInc {
					maxAutoInc = v
				}
			}
			nOK++
			pr.setExtra(int64(nOK))
			return limitRows == 0 || nOK < limitRows
		}
		if deletedOnly {
			// The same historical row content can genuinely turn up more
			// than once (see dedupDeletedRows) - drop exact repeats so
			// nOK, and the data file itself, only ever reflect distinct
			// recovered rows.
			onRow = dedupDeletedRows(onRow)
		}
		walkErr := rw.Walk(onCorrupt, onRow)
		pr.finish()
		if ferr := dw.Finish(); ferr != nil && walkErr == nil {
			walkErr = ferr
		}
		if walkErr != nil {
			return nOK, nErr, nCorruptPages, truncated, fmt.Errorf("walking table rows: %w", walkErr)
		}

		if t.AutoIncrementCol != nil {
			next := maxAutoInc + 1
			t.AutoIncrementNext = &next
			if err := writeTableDDL(); err != nil { // re-render now that AUTO_INCREMENT is known
				return nOK, nErr, nCorruptPages, truncated, fmt.Errorf("rewriting %s.sql: %w", tableBasename, err)
			}
		}

		if d.tableDataBytes[t.SchemaRef] == nil {
			d.tableDataBytes[t.SchemaRef] = map[string]uint64{}
			d.tableRows[t.SchemaRef] = map[string]uint64{}
		}
		d.tableDataBytes[t.SchemaRef][t.Name] = dataBytes
		d.tableRows[t.SchemaRef][t.Name] = uint64(nOK)
		d.chunkFileBytes[tableBasename+"."+ext] = dataBytes
		d.totalDataBytes += dataBytes
	}

	decodeCols := map[string]string{}
	colNames := make([]string, len(outCols))
	for i, c := range outCols {
		colNames[i] = c.Name
		if columnNeedsBase64(c) {
			decodeCols[c.Name] = "FROM_BASE64"
		}
	}
	if len(decodeCols) == 0 {
		decodeCols = nil
	}

	var primaryIndex []string
	if t.HasExplicitPK {
		for _, f := range t.PKFields {
			primaryIndex = append(primaryIndex, f.Col.Name)
		}
	}
	if primaryIndex == nil {
		primaryIndex = []string{}
	}

	charset := "utf8mb4"
	if cl, ok := collationTable[t.CollationID]; ok {
		charset = charsetOf(cl.name)
	} else if c, ok := uca1400Charset(t.CollationID); ok {
		charset = c.name
	}
	// Every string field is written as its column's own raw stored bytes,
	// in that column's own charset - so when some column's charset differs
	// from the table's default (a latin1 column in a utf8mb3 table, say),
	// no single text charset describes the whole file, and loading it as
	// the table's own would reject or mangle the other columns' bytes
	// ("Invalid utf8mb3 character string"). LOAD DATA ... CHARACTER SET
	// binary instead stores every field's bytes into its column exactly as
	// they are, which is precisely what they already are.
	for _, c := range outCols {
		if !columnHasCharset(c.DDType) || c.IsBinary() {
			continue
		}
		if colCharset, _, _, ok := resolveCollation(c.CollationID); ok && colCharset != charset {
			charset = "binary"
			break
		}
	}

	tm := tsvTableMeta{
		Options: tsvTableOptions{
			Schema: t.SchemaRef, Table: t.Name, Columns: colNames, DecodeColumns: decodeCols,
			DefaultCharacterSet: charset, FieldsTerminatedBy: "\t", FieldsEnclosedBy: "",
			FieldsOptionallyEnclosed: false, FieldsEscapedBy: `\`, LinesTerminatedBy: "\n",
		},
		Triggers: []string{}, IncludesData: !ddlOnly, IncludesDdl: true,
		Extension: compression.extension(), Chunking: false, Compression: compression.metadataName(),
		PrimaryIndex: primaryIndex, Partitions: []string{}, Basenames: map[string]string{},
	}
	if err := writeJSONFile(filepath.Join(d.dir, tableBasename+".json"), tm); err != nil {
		return nOK, nErr, nCorruptPages, truncated, fmt.Errorf("writing %s.json: %w", tableBasename, err)
	}

	return nOK, nErr, nCorruptPages, truncated, nil
}

// Finish writes the dump directory's schema- and root-level metadata, once
// every table has been added. Call it exactly once, after the last AddTable.
func (d *TSVDump) Finish() error {
	schemas := make([]string, 0, len(d.schemaOrder))
	basenames := map[string]string{}
	for _, name := range d.schemaOrder {
		s := d.schemas[name]
		schemas = append(schemas, name)
		basenames[name] = s.basename

		sm := tsvSchemaMeta{
			Schema: name, IncludesDdl: true, IncludesViewsDdl: true, IncludesData: true,
			Tables: s.tables, Views: []string{}, Basenames: s.tableBasenames,
		}
		if err := writeJSONFile(filepath.Join(d.dir, s.basename+".json"), sm); err != nil {
			return fmt.Errorf("writing %s.json: %w", s.basename, err)
		}
	}

	// util.loadDump() refuses a dump whose recorded server version is more
	// than one major release behind the target's (see dump_loader.cc's
	// major_difference check) - fall back to a plausible pre-8.0 version
	// when every table came from a .frm (no SDI carries no version at all)
	// rather than leaving this at "0.0.0", which would always trip that
	// check regardless of the target server.
	serverVersion := "5.7.44"
	if d.sourceVersionID > 0 {
		serverVersion = formatMySQLVersion(d.sourceVersionID)
	}

	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	root := tsvRootMeta{
		Dumper: fmt.Sprintf("%s %s", appName, version), Version: dumpFormatVersion, Origin: appName,
		Schemas: schemas, Basenames: basenames, DefaultCharacterSet: "utf8mb4", TzUtc: true,
		BytesPerChunk: 0, Consistent: false, CompatibilityOptions: []string{}, Capabilities: []any{},
		Checksum: false, Begin: now, ServerVersion: serverVersion,
	}
	if err := writeJSONFile(filepath.Join(d.dir, "@.json"), root); err != nil {
		return fmt.Errorf("writing @.json: %w", err)
	}

	done := tsvDoneMeta{
		End: now, DataBytes: d.totalDataBytes, TableDataBytes: d.tableDataBytes,
		TableRows: d.tableRows, ChunkFileBytes: d.chunkFileBytes,
	}
	if err := writeJSONFile(filepath.Join(d.dir, "@.done.json"), done); err != nil {
		return fmt.Errorf("writing @.done.json: %w", err)
	}
	return nil
}

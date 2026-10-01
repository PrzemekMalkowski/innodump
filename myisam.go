// myisam.go: offline schema+data extraction for a MyISAM table, given its
// .MYD (data) file - the counterpart to schema.go/btree.go's InnoDB path.
//
// MyISAM has no tablespace file to embed a dictionary in the way InnoDB's
// SDI does, so its schema instead comes from one of two places, exactly
// mirroring what a real server keeps next to the table's own data file:
//
//   - MySQL 8.0+: a standalone "<table>_<id>.sdi" JSON file (findStandaloneSDI)
//   - the same dictionary JSON shape as an InnoDB tablespace's embedded
//     SDI (schema.go's ddTableJSON unmarshals either one identically), just
//     written to its own file instead of a hidden index inside a .ibd.
//   - Pre-8.0 MySQL/MariaDB: a sibling .frm file, read by the same frm.go
//     parser InnoDB's own 5.6/5.7 path uses (parseMyISAMFRM) - that .frm's
//     columns and keys, plus a row format taken from the .MYI header when
//     there is one (readMYIHeader), or else from the .frm's own create
//     options.
//
// Once the schema is resolved into the same ddTableJSON/Column model
// schema.go uses, BuildMyISAMTable turns it into a Table exactly like
// BuildTable does for InnoDB - but without any of InnoDB's own physical
// record layout (no clustered index, no DB_TRX_ID/DB_ROLL_PTR system
// columns, no INSTANT ADD/DROP COLUMN): a MyISAM table's dd columns ARE
// its physical columns, in CREATE TABLE order.
//
// Row data itself is read directly off the .MYD file with no dependency on
// the .MYI index file - not even to find the data file's own length or row
// count, both taken from the .MYD file's own size on disk instead. (The
// .MYI's small fixed header is read if it's there, only to learn the row
// format of a .frm-described table and to cross-check the record length
// this file computes - see readMYIHeader - and is simply skipped if it's
// missing or unreadable.) This is deliberate: it means a missing or
// corrupted .MYI never stops this tool from recovering whatever the .MYD
// itself still holds, and it
// mirrors what a real, unindexed full table scan (MyISAM's own mi_scan/
// _mi_read_rnd_dynamic_record) does internally - walk the data file
// sequentially from the start, rather than via any key.
//
// The on-disk row formats themselves (buildMyISAMLayout's field
// classification, parseMIBlockInfo's block header layout, and
// unpackMyISAMRecord's packing rules) are ported from mysql-server's
// storage/myisam/{mi_dynrec,mi_statrec}.c and ha_myisam.cc's table2myisam
// (which decides each column's packing kind), and cross-checked against a
// real MySQL 8.0+ MyISAM table both via myisamchk -dvv (which prints the
// exact same per-field offset/length/packing-kind table this file
// computes independently from the SDI alone) and via a full reload-and-
// diff through a real MariaDB server.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// isMyISAMDataFile reports whether path names a MyISAM data or index file
// (by extension only, matching how --source-dir recognizes a .ibd file) -
// main.go's run() uses this to route --file to runMyISAMFile instead of
// the InnoDB tablespace path.
func isMyISAMDataFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".myd", ".myi":
		return true
	}
	return false
}

// myisamBaseName strips path's directory and .MYD/.MYI extension, giving
// the table's own on-disk basename (e.g. "cms_articles").
func myisamBaseName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

// myisamDataPath turns a --file argument that may point at either half of
// a MyISAM table (its .MYD or its .MYI) into the .MYD path this file
// always reads data from.
func myisamDataPath(path string) string {
	if strings.EqualFold(filepath.Ext(path), ".myi") {
		return strings.TrimSuffix(path, filepath.Ext(path)) + ".MYD"
	}
	return path
}

// --- schema discovery ---

// standaloneSDIFile is the top-level shape of a standalone "<table>_<id>.sdi"
// file - identical to the wrapper an embedded SDI record's JSON payload
// already has (see sdi.go/schema.go), just written to its own file instead
// of a compressed row inside a tablespace.
type standaloneSDIFile struct {
	MySQLVersionID uint32          `json:"mysqld_version_id"`
	DDObjectType   string          `json:"dd_object_type"`
	DDObject       json.RawMessage `json:"dd_object"`
}

// findStandaloneSDI looks in dir for the one "<base>_<digits>.sdi" file
// describing the table named base (MySQL 8.0+ writes this suffix from the
// table's own internal dictionary id, which varies per install - hence the
// glob rather than a fixed name). Returns "" (no error) if none is found,
// so the caller can fall back to a legacy .frm.
func findStandaloneSDI(dir, base string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	prefix := base + "_"
	var matches []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".sdi") {
			continue
		}
		stem := name[:len(name)-len(".sdi")]
		if len(stem) <= len(prefix) || !strings.EqualFold(stem[:len(prefix)], prefix) {
			continue
		}
		if _, err := strconv.ParseUint(stem[len(prefix):], 10, 64); err != nil {
			continue // doesn't end in "_<digits>" - not this table's SDI
		}
		matches = append(matches, name)
	}
	if len(matches) == 0 {
		return "", nil
	}
	sort.Strings(matches) // deterministic if, somehow, more than one matches
	return filepath.Join(dir, matches[0]), nil
}

// loadStandaloneSDI reads and parses one standalone .sdi file into the same
// ddTableJSON model schema.go's embedded-SDI path builds.
func loadStandaloneSDI(path string) (*ddTableJSON, uint32, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	var f standaloneSDIFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, 0, fmt.Errorf("parsing %s: %w", path, err)
	}
	if f.DDObjectType != "Table" {
		return nil, 0, fmt.Errorf("%s does not describe a table (dd_object_type=%q)", path, f.DDObjectType)
	}
	var t ddTableJSON
	if err := json.Unmarshal(f.DDObject, &t); err != nil {
		return nil, 0, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &t, f.MySQLVersionID, nil
}

// findMyISAMSchema locates and parses the dictionary information for the
// MyISAM table mydPath belongs to - see this file's own package comment
// for the two places that can live. mysqlVersionID is 0 for a
// .frm-described table (a .frm records no server version).
func findMyISAMSchema(mydPath string) (*ddTableJSON, uint32, error) {
	dir := filepath.Dir(mydPath)
	base := myisamBaseName(mydPath)

	sdiPath, err := findStandaloneSDI(dir, base)
	if err != nil {
		return nil, 0, fmt.Errorf("looking for %s's .sdi file: %w", base, err)
	}
	if sdiPath != "" {
		raw, versionID, err := loadStandaloneSDI(sdiPath)
		if err != nil {
			return nil, 0, err
		}
		return raw, versionID, nil
	}

	if strings.Contains(strings.ToLower(base), "#p#") {
		return nil, 0, fmt.Errorf("%s is one partition of a partitioned MyISAM table, which isn't supported (v1 limitation) - "+
			"only partitioned InnoDB tables are", filepath.Base(mydPath))
	}
	frmPath := filepath.Join(dir, base+".frm")
	if _, err := os.Stat(frmPath); err != nil {
		return nil, 0, fmt.Errorf("no schema found for %s - expected a standalone %s_<id>.sdi (MySQL 8.0+) or a %s.frm (pre-8.0 MySQL/MariaDB) next to it",
			filepath.Base(mydPath), base, base)
	}
	raw, err := parseMyISAMFRM(frmPath)
	if err != nil {
		return nil, 0, fmt.Errorf("reading %s: %w", filepath.Base(frmPath), err)
	}
	// The .frm's own create options say FIXED vs DYNAMIC, but only the
	// .MYI records what the table's data file actually holds right now -
	// notably, whether myisampack has since compressed it (a .frm is never
	// rewritten for that).
	if hdr, ok := readMYIHeader(mydPath); ok {
		switch {
		case hdr.Options&miOptionCompressRecord != 0:
			raw.RowFormat = rowFormatCompressed
		case hdr.Options&haOptionPackRecord != 0:
			raw.RowFormat = rowFormatDynamic
		default:
			raw.RowFormat = rowFormatFixed
		}
	}
	return raw, 0, nil
}

// myisamConvertAddsKey reports whether --myisam-to-innodb adds an extra
// KEY for mydPath's table's AUTO_INCREMENT column (see sqlout.go's
// myisamToInnoDBAutoIncKey) - for --source-dir's closing summary.
func myisamConvertAddsKey(mydPath string) bool {
	raw, _, err := findMyISAMSchema(mydPath)
	if err != nil {
		return false
	}
	t, err := BuildMyISAMTable(raw)
	return err == nil && myisamToInnoDBAutoIncKey(t) != ""
}

// miOptionCompressRecord is HA_OPTION_COMPRESS_RECORD (my_base.h): set in
// a .MYI header's options once myisampack has compressed the table.
const miOptionCompressRecord = 4

// myiHeader is the handful of .MYI header fields this tool uses - see
// readMYIHeader.
type myiHeader struct {
	Options       uint32 // HA_OPTION_* bits (mi_state_info's options)
	Records       uint64 // mi_state_info's state.records: live rows, as of the table's last clean close
	RecLength     uint32 // MI_BASE_INFO.reclength: the unpacked record's length
	PackRecLength uint32 // MI_BASE_INFO.pack_reclength: a FIXED-format record's own slot length in the .MYD
	Fields        uint32 // MI_BASE_INFO.fields: physical fields, the leading null-bitmap one included
	PackBits      uint32 // MI_BASE_INFO.pack_bits: a DYNAMIC record's flag-bitmap bytes
	// Columns is the table's own per-field MI_COLUMNDEF array (recinfo),
	// in physical order, or nil if it couldn't be located - see
	// readMYIColumns.
	Columns []miColumnDef
}

// miColumnDef is one MI_COLUMNDEF entry: the en_fieldtype MyISAM itself
// packs this field with, fixed for good when the table was created (and
// so reflecting whatever rules the server that created it followed -
// which is not always what table2myisam would pick for the same
// .frm today), and the field's length in the unpacked record.
type miColumnDef struct {
	Type   int16
	Length uint32
}

// MyISAM's own en_fieldtype codes (myisampack.h) this tool decodes.
const (
	miFieldNormal       = 0
	miFieldSkipEndspace = 1
	miFieldSkipPrespace = 2
	miFieldSkipZero     = 3
	miFieldBlob         = 4
	miFieldVarchar      = 8
)

// Sizes of the fixed-length .MYI header sections that sit between the
// base info block and the recinfo array (mi_open.c's own *_SIZE macros).
const (
	miKeyDefSize    = 12 // MI_KEYDEF_SIZE
	miKeySegSize    = 18 // HA_KEYSEG_SIZE
	miUniqueDefSize = 4  // MI_UNIQUEDEF_SIZE
	miColumnDefSize = 7  // MI_COLUMNDEF_SIZE
)

// readMYIColumns reads the recinfo array mi_open.c reads with
// mi_recinfo_read: fields MI_COLUMNDEF entries (type int16, length uint16,
// null_bit uint8, null_pos uint16, all big-endian) right after the key
// definitions (keys MI_KEYDEF + key_parts HA_KEYSEG), and the unique
// definitions (uniques MI_UNIQUEDEF + unique_key_parts HA_KEYSEG) that
// follow the base info block - ending exactly at header_length. Returns
// nil if the counts in head don't line up that way.
func readMYIColumns(f *os.File, head []byte, basePos int64, fields uint32) []miColumnDef {
	headerLen := int64(miBEUint(head[6:8]))
	baseInfoLen := int64(miBEUint(head[10:12]))
	keyParts := int64(miBEUint(head[14:16]))
	uniqueKeyParts := int64(miBEUint(head[16:18]))
	keys, uniques := int64(head[18]), int64(head[19])
	off := basePos + baseInfoLen + keys*miKeyDefSize + keyParts*miKeySegSize +
		uniques*miUniqueDefSize + uniqueKeyParts*miKeySegSize
	if off+int64(fields)*miColumnDefSize != headerLen {
		return nil
	}
	buf := make([]byte, int64(fields)*miColumnDefSize)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil
	}
	cols := make([]miColumnDef, fields)
	for i := range cols {
		e := buf[i*miColumnDefSize:]
		cols[i] = miColumnDef{Type: int16(miBEUint(e[0:2])), Length: uint32(miBEUint(e[2:4]))}
	}
	return cols
}

// readMYIHeader reads the fixed header at the start of mydPath's sibling
// .MYI file - mi_state_info_read/mi_base_info_read in mi_open.c: a
// 24-byte state header (magic, options, header lengths, base_pos) followed
// by the table's state (whose row count sits 4 bytes in), then, at
// base_pos, the base info block (reclength/pack_reclength 44/48 bytes in,
// fields 64, pack_bits 76). Every field is big-endian. ok is false if there's
// no .MYI, or it doesn't look like one - never an error, since nothing
// here is required (see this file's package comment).
func readMYIHeader(mydPath string) (myiHeader, bool) {
	myiPath := strings.TrimSuffix(mydPath, filepath.Ext(mydPath)) + ".MYI"
	f, err := os.Open(myiPath)
	if err != nil {
		if f, err = os.Open(strings.TrimSuffix(mydPath, filepath.Ext(mydPath)) + ".myi"); err != nil {
			return myiHeader{}, false
		}
	}
	defer f.Close()
	head := make([]byte, 24)
	if _, err := io.ReadFull(f, head); err != nil {
		return myiHeader{}, false
	}
	if head[0] != 0xfe || head[1] != 0xfe || head[3] != 0x01 {
		return myiHeader{}, false // not a MyISAM index file (myisam_file_magic)
	}
	h := myiHeader{Options: uint32(miBEUint(head[4:6]))}
	state := make([]byte, 12)
	if _, err := f.ReadAt(state, 24); err != nil {
		return myiHeader{}, false
	}
	h.Records = miBEUint(state[4:12])
	basePos := int64(miBEUint(head[12:14]))
	base := make([]byte, 78)
	if _, err := f.ReadAt(base, basePos); err != nil {
		return myiHeader{}, false
	}
	h.RecLength = uint32(miBEUint(base[44:48]))
	h.PackRecLength = uint32(miBEUint(base[48:52]))
	h.Fields = uint32(miBEUint(base[64:68]))
	h.PackBits = uint32(miBEUint(base[76:78]))
	h.Columns = readMYIColumns(f, head, basePos, h.Fields)
	return h, true
}

// --- table model ---

// myisamDefaultRowFormat is the ROW_FORMAT MyISAM picks on its own when
// none is given explicitly at CREATE TABLE time: DYNAMIC if the table has
// any variable-length column (VARCHAR/BLOB/TEXT-family/JSON/GEOMETRY/
// VECTOR), FIXED otherwise. GenerateDDL (sqlout.go) only ever states an
// explicit ROW_FORMAT= clause when the table's real one differs from this,
// matching SHOW CREATE TABLE's own rule.
func myisamDefaultRowFormat(t *Table) uint32 {
	for _, c := range t.Columns {
		if c.IsVirtual {
			continue
		}
		switch c.DDType {
		case ddVarchar, ddTinyBlob, ddMediumBlob, ddBlob, ddLongBlob, ddJSON, ddVector, ddGeometry:
			return rowFormatDynamic
		}
	}
	return rowFormatFixed
}

// BuildMyISAMTable resolves a raw SDI/.frm dd_object into the model this
// file's row walker needs - the MyISAM counterpart to schema.go's
// BuildTable. Unlike InnoDB, a MyISAM table has no clustered index, no
// DB_TRX_ID/DB_ROLL_PTR system columns, and no INSTANT ADD/DROP COLUMN
// history: its dd columns already list exactly its physical columns, in
// CREATE TABLE order, so this is considerably shorter than BuildTable.
func BuildMyISAMTable(raw *ddTableJSON) (*Table, error) {
	if raw.Engine != "" && raw.Engine != "MyISAM" {
		return nil, fmt.Errorf("this .sdi describes a %s table, not MyISAM - point --file at its own tablespace/.ibd file instead", raw.Engine)
	}
	if raw.PartitionType != 0 {
		return nil, fmt.Errorf("partitioned tables are not supported (v1 limitation)")
	}
	switch raw.RowFormat {
	case rowFormatFixed, rowFormatDynamic:
	case rowFormatCompressed:
		return nil, fmt.Errorf("ROW_FORMAT=COMPRESSED MyISAM tables (myisampack, read-only archives) are not supported (v1 limitation)")
	default:
		return nil, fmt.Errorf("ROW_FORMAT %s is not supported for MyISAM (v1 limitation)", rowFormatName(raw.RowFormat))
	}

	colsByOpx := make([]*Column, len(raw.Columns))
	for i, rc := range raw.Columns {
		c, err := buildColumn(rc)
		if err != nil {
			return nil, err
		}
		colsByOpx[i] = c
	}

	t := &Table{
		Name:        raw.Name,
		SchemaRef:   raw.SchemaRef,
		Engine:      "MyISAM",
		RowFormat:   raw.RowFormat,
		SePrivateID: raw.SePrivateID,
		Comment:     raw.Comment,
		CollationID: raw.CollationID,
	}
	for _, c := range colsByOpx {
		if c.IsAutoIncrement {
			t.AutoIncrementCol = c
			break
		}
	}
	t.Columns = append([]*Column(nil), colsByOpx...)
	sort.SliceStable(t.Columns, func(i, j int) bool {
		return t.Columns[i].raw.OrdinalPosition < t.Columns[j].raw.OrdinalPosition
	})

	// Unlike InnoDB (where indexes[0] is always the clustered index, real
	// PRIMARY KEY or not - see BuildTable), a MyISAM table's index list
	// only ever starts with a PRIMARY KEY entry when one was actually
	// declared; a table with no PK simply omits it; nothing takes its
	// place as index 0.
	startSecondary := 0
	if len(raw.Indexes) > 0 && raw.Indexes[0].Type == ddIndexPrimary {
		t.HasExplicitPK = true
		t.IndexName = raw.Indexes[0].Name
		for _, el := range raw.Indexes[0].Elements {
			if el.Hidden {
				break
			}
			if int(el.ColumnOpx) >= len(colsByOpx) {
				return nil, fmt.Errorf("PRIMARY KEY element references an out-of-range column")
			}
			col := colsByOpx[el.ColumnOpx]
			field := &IndexField{Col: col}
			if el.Length < col.ColLen || (isBlobFamily(col.DDType) && el.Length > 0) {
				switch col.Mtype {
				case dataInt, dataFloat, dataDouble, dataDecimal:
				default:
					field.PrefixLen = el.Length
				}
			}
			t.PKFields = append(t.PKFields, field)
		}
		startSecondary = 1
	}

	for _, idx := range raw.Indexes[startSecondary:] {
		if idx.Hidden {
			continue
		}
		si := &SecondaryIndex{
			Name:     idx.Name,
			Unique:   idx.Type == ddIndexUnique,
			Fulltext: idx.Type == ddIndexFulltext,
			Spatial:  idx.Type == ddIndexSpatial,
			Visible:  idx.IsVisible,
		}
		for _, el := range idx.Elements {
			if el.Hidden {
				continue
			}
			if int(el.ColumnOpx) >= len(colsByOpx) {
				return nil, fmt.Errorf("index %q references an out-of-range column", idx.Name)
			}
			col := colsByOpx[el.ColumnOpx]
			sic := SecondaryIndexColumn{Col: col, Desc: el.Order == ddOrderDesc}
			// A BLOB/TEXT key part is always a prefix, however its length
			// compares to the column's own (tiny) in-record pack length.
			if el.Length < col.ColLen || (isBlobFamily(col.DDType) && el.Length > 0) {
				sic.PrefixLen = el.Length
			}
			si.Columns = append(si.Columns, sic)
		}
		if len(si.Columns) > 0 {
			t.SecondaryIndexes = append(t.SecondaryIndexes, si)
		}
	}
	// MySQL refuses to create a FOREIGN KEY on a non-InnoDB table, so a
	// MyISAM table's own dd_object never carries any - nothing to build.
	return t, nil
}

// --- physical row layout ---

// myisamFieldKind is one column's on-disk packing scheme in a DYNAMIC-
// format MyISAM row - MI_COLUMNDEF's own "type" (en_fieldtype in
// mysql-server), as decided by ha_myisam.cc's table2myisam for every
// column depending on its SQL type (see classifyMyISAMField).
type myisamFieldKind int

const (
	mfNormal       myisamFieldKind = iota // stored verbatim, in full, always (includes the leading null-bitmap bytes)
	mfSkipZero                            // stored in full unless every byte is 0 (a single flag bit then means "all zero")
	mfSkipEndspace                        // trailing spaces optionally stripped (CHAR)
	mfSkipPrespace                        // leading spaces optionally stripped (TIMESTAMP2's own bug-compat packing - see classifyMyISAMField)
	mfVarchar                             // 1-2 byte length prefix + exactly that many bytes, always (true VARCHAR)
	mfBlob                                // size_length-byte length prefix + that many content bytes, unless a flag bit says "empty"
)

// myisamField is one physical field (or, at index 0 when NullBytes > 0, the
// synthetic leading null-bitmap gap) in a MyISAM table's own native
// "unpacked" record layout - see buildMyISAMLayout.
type myisamField struct {
	Col      *Column // nil for the null-bitmap gap
	Kind     myisamFieldKind
	Offset   uint32 // byte offset in the unpacked record
	Length   uint32 // pack length in the unpacked record (packLength()) - for mfBlob, includes its own length-prefix bytes
	NullByte int    // absolute byte offset of this column's null bit, or -1 if it's never NULL
	NullMask byte
}

// myisamLayout is one table's full physical row layout, derived purely
// from its dictionary information (column types/nullability/order) - no
// .MYI is read for any of this; see this file's own package comment.
type myisamLayout struct {
	Fields    []myisamField // in physical (== CREATE TABLE) order, virtual columns excluded (they occupy no storage)
	NullBytes int
	RecLength uint32 // total unpacked record length - sum of every field's Length
	// SlotLength is a FIXED-format record's own on-disk slot length in the
	// .MYD - RecLength, rounded up to the minimum a deleted slot needs for
	// its own free-list link (see minFixedSlotLength), or exactly what the
	// .MYI header says it is when there is one (see applyMYIHeader).
	SlotLength uint32
	PackBits   int  // bytes of packed-record flag bitmap (DYNAMIC only - see unpackMyISAMRecord)
	Fixed      bool // ROW_FORMAT=FIXED (static): every field is mfNormal, one record every SlotLength bytes
	// MYIRecords is the live row count the .MYI header recorded (HaveMYI
	// false if there was no usable .MYI) - reported alongside --verbose's
	// own detail, as a sanity check against the rows a scan finds.
	MYIRecords uint64
	HaveMYI    bool
}

// classifyMyISAMField decides one non-virtual column's myisamFieldKind,
// mirroring table2myisam's own decision tree in ha_myisam.cc (mysql-server):
// BLOB-family columns and true VARCHAR are always special-cased first message
// (regardless of ZEROFILL/length); TIMESTAMP2 is force-fit into
// FIELD_SKIP_PRESPACE (a long-standing, deliberately-preserved bug in real
// MySQL/MariaDB - see the comment table2myisam itself carries); every other
// type follows Field::zero_pack() (true for numeric/temporal types unless
// ZEROFILL, and for SET - Field_set overrides it back to true; false for
// CHAR/BINARY, which fall to FIELD_SKIP_ENDSPACE once their pack length
// exceeds 3 bytes, and for ENUM, whose 1-2 byte pack length always stays
// FIELD_NORMAL). Cross-checked field-by-field against a real table's own
// myisamchk -dvv output - see this file's package comment - and, for SET,
// against a real 5.6 mysql.proc row (a 4-byte sql_mode SET holding 0
// stores only its flag bit, no length byte, which rules out both
// FIELD_SKIP_ENDSPACE and FIELD_SKIP_PRESPACE).
func classifyMyISAMField(c *Column) (myisamFieldKind, error) {
	switch c.DDType {
	case ddTinyBlob, ddMediumBlob, ddBlob, ddLongBlob, ddJSON, ddVector, ddGeometry:
		return mfBlob, nil
	case ddVarchar:
		return mfVarchar, nil
	case ddTimestamp2:
		return mfSkipPrespace, nil
	case ddTimestamp:
		// Pre-5.6.4 TIMESTAMP: Field_timestamp always carries ZEROFILL_FLAG
		// internally, so it's FIELD_NORMAL - confirmed against real 5.5
		// mysql.proc/event/tables_priv .MYI recinfo (old DATETIME/TIME fall
		// to the default FIELD_SKIP_ZERO below, which those confirm too).
		return mfNormal, nil
	case ddSet:
		return mfSkipZero, nil
	case ddString, ddVarString, ddEnum:
		pl, err := packLength(&c.raw)
		if err != nil {
			return 0, err
		}
		if pl <= 3 || c.IsZerofill {
			return mfNormal, nil
		}
		return mfSkipEndspace, nil
	default:
		if c.IsZerofill {
			return mfNormal, nil
		}
		return mfSkipZero, nil
	}
}

// varcharLenBytes returns how many bytes col's own embedded VARCHAR length
// prefix takes on disk: 1 up to 255 *characters* declared, 2 above that -
// Field_varstring's length_bytes (field.h), which - unlike packLength's
// CharLength (already multiplied by the charset's own max bytes/char, e.g.
// 1000 for a utf8mb4 VARCHAR(250)) - is decided from the plain declared
// character count (250), not the byte capacity. Cross-checked against a
// real utf8mb4 VARCHAR(250) MyISAM column, which myisamchk confirms packs
// with a single-byte length prefix even though its byte capacity (1000)
// alone would suggest two.
func varcharLenBytes(col *Column) uint32 {
	declaredChars := col.raw.CharLength
	if info, ok := collationInfoFor(col.CollationID); ok && info.max > 1 {
		declaredChars /= uint32(info.max)
	}
	if declaredChars >= 256 {
		return 2
	}
	return 1
}

// buildMyISAMLayout derives t's full physical row layout purely from its
// already-resolved column list - see myisamLayout's own comment.
func buildMyISAMLayout(t *Table) (*myisamLayout, error) {
	lay := &myisamLayout{Fixed: t.RowFormat == rowFormatFixed}

	var physCols []*Column
	nNullable := 0
	for _, c := range t.Columns {
		if c.IsVirtual {
			continue // a virtual generated column is computed on read, never stored
		}
		physCols = append(physCols, c)
		if c.IsNullable {
			nNullable++
		}
	}
	// A FIXED-format table (no HA_OPTION_PACK_RECORD) reserves the null
	// bitmap's very first bit as its own "this slot is live" marker -
	// always 1 in a real record, so a deleted slot (whose first byte is
	// overwritten with 0) can be told apart from it: open_binary_frm's
	// null_bit_pos = 1 in table.cc, and _mi_read_rnd_static_record's
	// `if (!*record)` check in mi_statrec.c. Column null bits start right
	// after it, and the table always has at least one null byte for it,
	// even with no nullable column at all. Confirmed against the .MYI
	// headers' own reclength for real 5.6 FIXED tables (mysql.time_zone:
	// INT + ENUM, both NOT NULL = 1 + 4 + 1 = 6 bytes).
	reservedBits := 0
	if lay.Fixed {
		reservedBits = 1
	}
	lay.NullBytes = (nNullable + reservedBits + 7) / 8

	var offset uint32
	if lay.NullBytes > 0 {
		lay.Fields = append(lay.Fields, myisamField{Kind: mfNormal, Offset: 0, Length: uint32(lay.NullBytes), NullByte: -1})
		offset = uint32(lay.NullBytes)
	}

	nullIdx := reservedBits
	packedFields := 0
	for _, c := range physCols {
		pl, err := packLength(&c.raw)
		if err != nil {
			return nil, err
		}
		f := myisamField{Col: c, Offset: offset, Length: pl, NullByte: -1}
		if c.IsNullable {
			f.NullByte = nullIdx / 8
			f.NullMask = 1 << uint(nullIdx%8)
			nullIdx++
		}
		if lay.Fixed {
			f.Kind = mfNormal
		} else {
			kind, err := classifyMyISAMField(c)
			if err != nil {
				return nil, err
			}
			f.Kind = kind
			if kind != mfNormal && kind != mfVarchar {
				packedFields++
			}
		}
		lay.Fields = append(lay.Fields, f)
		offset += pl
	}
	lay.RecLength = offset
	lay.PackBits = (packedFields + 7) / 8
	lay.SlotLength = lay.RecLength
	if lay.Fixed && lay.SlotLength < minFixedSlotLength {
		lay.SlotLength = minFixedSlotLength
	}
	return lay, nil
}

// minFixedSlotLength is the shortest a FIXED-format record's slot in the
// .MYD ever is: a deleted slot is overwritten with a 0 marker byte plus
// a link to the next deleted slot (rec_reflength bytes - 6 with the
// server's default myisam_data_pointer_size), so a record shorter than
// that is still padded out to it (mi_create.c's pack_reclength). Only a
// fallback: applyMYIHeader uses the .MYI's own exact value when there is
// one, which also covers a non-default data pointer size or a
// CHECKSUM=1 table's extra trailing byte.
const minFixedSlotLength = 1 + 6

// applyMYIHeader cross-checks lay against mydPath's .MYI header, if there
// is one (readMYIHeader): the unpacked record length, physical field
// count, and (DYNAMIC only) flag-bitmap size this file derived from the
// schema alone must all match what MyISAM itself recorded - a mismatch
// means the schema (.sdi/.frm) doesn't describe this data file, or
// describes it in a way this tool misreads, and decoding would only
// produce garbage - and a FIXED table's slot length is taken from it
// as-is. No .MYI (or an unreadable one) is not an error.
func applyMYIHeader(mydPath string, lay *myisamLayout) error {
	hdr, ok := readMYIHeader(mydPath)
	if !ok {
		return nil
	}
	mismatch := func(what string, ours, theirs uint32) error {
		return fmt.Errorf("%s derived from the table's schema (%d) doesn't match the %d its own .MYI header records - "+
			"the .sdi/.frm doesn't describe this data file as this tool reads it", what, ours, theirs)
	}
	if hdr.RecLength != lay.RecLength {
		return mismatch("record length", lay.RecLength, hdr.RecLength)
	}
	if hdr.Fields != uint32(len(lay.Fields)) {
		return mismatch("physical field count", uint32(len(lay.Fields)), hdr.Fields)
	}
	if !lay.Fixed && hdr.Columns != nil {
		if err := adoptMYIFieldKinds(lay, hdr.Columns); err != nil {
			return err
		}
	}
	if !lay.Fixed && hdr.PackBits != uint32(lay.PackBits) {
		return mismatch("packed-record flag bitmap size", uint32(lay.PackBits), hdr.PackBits)
	}
	if lay.Fixed && hdr.PackRecLength >= lay.RecLength {
		lay.SlotLength = hdr.PackRecLength
	}
	lay.MYIRecords = hdr.Records
	lay.HaveMYI = true
	return nil
}

// adoptMYIFieldKinds replaces a DYNAMIC layout's schema-derived field
// kinds with the ones the table's own .MYI recinfo records, then
// recomputes PackBits to match. The recinfo is what MyISAM itself packs
// and unpacks every row with, fixed when the table was created - so a
// table created by an older server can store, say, a TINYINT as
// FIELD_NORMAL where table2myisam would pick FIELD_SKIP_ZERO for the very
// same .frm today (seen on real 5.6 tables whose .frm pack_flag is
// identical for both). Only swaps among the fixed-length kinds (normal,
// skip-zero, skip-endspace, skip-prespace) are taken as-is: those all
// unpack into the same field slot, so the value decodes the same either
// way. A disagreement over VARCHAR/BLOB, a field's length, or a type this
// tool doesn't decode still means the schema doesn't describe this file.
func adoptMYIFieldKinds(lay *myisamLayout, cols []miColumnDef) error {
	fixedLen := func(k myisamFieldKind) bool {
		return k == mfNormal || k == mfSkipZero || k == mfSkipEndspace || k == mfSkipPrespace
	}
	packedFields := 0
	for i := range lay.Fields {
		f := &lay.Fields[i]
		c := cols[i]
		var kind myisamFieldKind
		switch c.Type {
		case miFieldNormal:
			kind = mfNormal
		case miFieldSkipEndspace:
			kind = mfSkipEndspace
		case miFieldSkipPrespace:
			kind = mfSkipPrespace
		case miFieldSkipZero:
			kind = mfSkipZero
		case miFieldBlob:
			kind = mfBlob
		case miFieldVarchar:
			kind = mfVarchar
		default:
			return fmt.Errorf("field %d is stored with MyISAM field type %d, which this tool doesn't decode", i+1, c.Type)
		}
		if c.Length != f.Length {
			return fmt.Errorf("field %d's length derived from the table's schema (%d) doesn't match the %d its own .MYI header records - "+
				"the .sdi/.frm doesn't describe this data file as this tool reads it", i+1, f.Length, c.Length)
		}
		if kind != f.Kind && !(fixedLen(kind) && fixedLen(f.Kind)) {
			return fmt.Errorf("field %d's storage type derived from the table's schema (%d) doesn't match the %d its own .MYI header records - "+
				"the .sdi/.frm doesn't describe this data file as this tool reads it", i+1, f.Kind, kind)
		}
		f.Kind = kind
		if kind != mfNormal && kind != mfVarchar {
			packedFields++
		}
	}
	lay.PackBits = (packedFields + 7) / 8
	return nil
}

// --- DYNAMIC-format block headers ---

const (
	miBlockHeaderLen = 20 // MI_BLOCK_INFO_HEADER_LENGTH
	// miDynAlignSize is MI_DYN_ALIGN_SIZE - the resync step used once a
	// block header fails validation (see scanMyISAMDynamic's fail/resync
	// loop). It is NOT a universal invariant of every block's own length
	// (only block types 3/4/9/10 round up to it via a trailing padding
	// byte; types 1/2/7/8 are an exact fit with none at all).
	miDynAlignSize = 4
)

// miBlockInfo is one dynamic-record block's parsed header - mirrors
// MI_BLOCK_INFO/_mi_get_block_info in mi_dynrec.c closely enough to
// reproduce its exact byte layout; see parseMIBlockInfo.
type miBlockInfo struct {
	Deleted     bool
	First, Last bool
	RecLen      uint32 // whole record's length - only meaningful on its first block
	DataLen     uint32 // this block's own share of the record
	BlockLen    uint32 // this block's total on-disk length (>= DataLen; the difference is slack/alignment padding)
	NextFilePos uint64 // next block's file offset, when !Last
	HeaderLen   int    // bytes of the 20-byte header this block type actually uses - the rest already holds prefetched row data
	// Continuation is true for a non-first block of some record (types
	// 7-12) found where a record's own first block was expected - see
	// parseMIBlockInfo.
	Continuation bool
}

// miBEUint reads b as a big-endian unsigned integer of len(b) bytes - MyISAM's
// own "mi_" store order for every field in a block header (see
// myisampack.h's "Storing of values in high byte first order" comment) -
// unlike the row data those blocks carry, which is plain little-endian.
func miBEUint(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

// parseMIBlockInfo decodes one dynamic-record block header from a
// miBlockHeaderLen-byte buffer - mirrors _mi_get_block_info in
// mi_dynrec.c. secondRead is true when this block continues a record
// already begun by an earlier (First) block - a genuine MyISAM data file
// never puts a First/Deleted-shaped header there, so this doubles as the
// same sync-error check the real reader makes. The reverse - a
// continuation block (types 7-12) met while scanning for the next
// record's first block - is perfectly normal, though: a record updated to
// a longer length gets extra blocks allocated wherever free space is,
// often further along the file than its own first block, so a sequential
// scan runs into them on its own. The real reader flags those
// BLOCK_SYNC_ERROR and skips each one by its own length
// (_mi_read_rnd_dynamic_record's skip_deleted_blocks branch); this sets
// Continuation instead, for scanMyISAMDynamic to do the same.
func parseMIBlockInfo(header []byte, secondRead bool) (miBlockInfo, error) {
	if len(header) < miBlockHeaderLen {
		return miBlockInfo{}, fmt.Errorf("only %d byte(s) available, need %d for a block header", len(header), miBlockHeaderLen)
	}
	typ := header[0]
	var bi miBlockInfo
	if secondRead {
		if typ <= 6 || typ == 13 {
			return miBlockInfo{}, fmt.Errorf("expected a continuation block, found type %d", typ)
		}
	} else if typ > 6 && typ != 13 {
		bi.Continuation = true
	}
	switch typ {
	case 0:
		bi.Deleted = true
		bi.BlockLen = uint32(miBEUint(header[1:4]))
		bi.HeaderLen = miBlockHeaderLen // the whole header is delete-link bookkeeping, not row data
	case 1:
		v := uint32(miBEUint(header[1:3]))
		bi.RecLen, bi.DataLen, bi.BlockLen = v, v, v
		bi.First, bi.Last, bi.HeaderLen = true, true, 3
	case 2:
		v := uint32(miBEUint(header[1:4]))
		bi.RecLen, bi.DataLen, bi.BlockLen = v, v, v
		bi.First, bi.Last, bi.HeaderLen = true, true, 4
	case 3:
		v := uint32(miBEUint(header[1:3]))
		bi.RecLen, bi.DataLen = v, v
		bi.BlockLen = v + uint32(header[3])
		bi.First, bi.Last, bi.HeaderLen = true, true, 4
	case 4:
		v := uint32(miBEUint(header[1:4]))
		bi.RecLen, bi.DataLen = v, v
		bi.BlockLen = v + uint32(header[4])
		bi.First, bi.Last, bi.HeaderLen = true, true, 5
	case 5:
		bi.RecLen = uint32(miBEUint(header[1:3]))
		bi.DataLen = uint32(miBEUint(header[3:5]))
		bi.BlockLen = bi.DataLen
		bi.NextFilePos = miBEUint(header[5:13])
		bi.First, bi.HeaderLen = true, 13
	case 6:
		bi.RecLen = uint32(miBEUint(header[1:4]))
		bi.DataLen = uint32(miBEUint(header[4:7]))
		bi.BlockLen = bi.DataLen
		bi.NextFilePos = miBEUint(header[7:15])
		bi.First, bi.HeaderLen = true, 15
	case 13:
		bi.RecLen = uint32(miBEUint(header[1:5]))
		bi.DataLen = uint32(miBEUint(header[5:8]))
		bi.BlockLen = bi.DataLen
		bi.NextFilePos = miBEUint(header[8:16])
		bi.First, bi.HeaderLen = true, 16
	case 7:
		bi.DataLen = uint32(miBEUint(header[1:3]))
		bi.BlockLen = bi.DataLen
		bi.Last, bi.HeaderLen = true, 3
	case 8:
		bi.DataLen = uint32(miBEUint(header[1:4]))
		bi.BlockLen = bi.DataLen
		bi.Last, bi.HeaderLen = true, 4
	case 9:
		bi.DataLen = uint32(miBEUint(header[1:3]))
		bi.BlockLen = bi.DataLen + uint32(header[3])
		bi.Last, bi.HeaderLen = true, 4
	case 10:
		bi.DataLen = uint32(miBEUint(header[1:4]))
		bi.BlockLen = bi.DataLen + uint32(header[4])
		bi.Last, bi.HeaderLen = true, 5
	case 11:
		bi.DataLen = uint32(miBEUint(header[1:3]))
		bi.BlockLen = bi.DataLen
		bi.NextFilePos = miBEUint(header[3:11])
		bi.HeaderLen = 11
	case 12:
		bi.DataLen = uint32(miBEUint(header[1:4]))
		bi.BlockLen = bi.DataLen
		bi.NextFilePos = miBEUint(header[4:12])
		bi.HeaderLen = 12
	default:
		return miBlockInfo{}, fmt.Errorf("unrecognized block type %d", typ)
	}
	return bi, nil
}

// --- unpacking a DYNAMIC-format record's assembled bytes ---

// myisamRecord is one decoded row's physical bytes, laid out exactly like
// myisamLayout describes: Buf holds every field's own fixed slot (for a
// VARCHAR, its length prefix followed by only its actual content; for a
// BLOB, just its own length-prefix bytes) and Blob carries each BLOB-kind
// field's actual content bytes separately (parallel to lay.Fields; nil for
// every non-blob field, and for a blob that's empty or SQL NULL).
type myisamRecord struct {
	Buf  []byte
	Blob [][]byte
}

// unpackMyISAMRecord expands one DYNAMIC-format record's packed on-disk
// bytes (already assembled across every block it spans - see
// scanMyISAMDynamic) into a myisamRecord, mirroring _mi_rec_unpack in
// mi_dynrec.c field by field.
func unpackMyISAMRecord(packed []byte, lay *myisamLayout) (*myisamRecord, error) {
	if len(packed) < lay.PackBits {
		return nil, fmt.Errorf("packed record (%d byte(s)) shorter than its own %d-byte flag bitmap", len(packed), lay.PackBits)
	}
	flagBytes := packed[:lay.PackBits]
	from := packed[lay.PackBits:]
	bitIdx := 0
	testBit := func() bool {
		byteIdx, bit := bitIdx/8, uint(bitIdx%8)
		bitIdx++
		if byteIdx >= len(flagBytes) {
			return false
		}
		return flagBytes[byteIdx]&(1<<bit) != 0
	}
	consume := func(n uint32) ([]byte, error) {
		if uint64(len(from)) < uint64(n) {
			return nil, fmt.Errorf("packed record ends mid-field (need %d more byte(s))", n)
		}
		b := from[:n]
		from = from[n:]
		return b, nil
	}

	rec := &myisamRecord{Buf: make([]byte, lay.RecLength), Blob: make([][]byte, len(lay.Fields))}
	for fi, f := range lay.Fields {
		switch f.Kind {
		case mfVarchar:
			// The PACKED on-disk length prefix is NOT simply lenBytes
			// worth of little-endian bytes once lenBytes==2 (a >255-
			// character VARCHAR): it's my_compare.h's own variable-width
			// get_key_length/store_key_length_inc encoding instead - one
			// byte for a length under 255, or a 0xFF marker followed by a
			// big-endian ("mi_") 2-byte length for 255 and above. Only
			// when lenBytes==1 does the packed form match the unpacked
			// one byte-for-byte. Either way, rec.Buf itself always ends up
			// holding the plain little-endian lenBytes-byte length
			// Field_varstring itself expects (matching int2store on the
			// real unpack side), regardless of which packed form was read.
			lenBytes := varcharLenBytes(f.Col)
			var l uint32
			if lenBytes == 1 {
				lb, err := consume(1)
				if err != nil {
					return nil, fmt.Errorf("column %q: %w", f.Col.Name, err)
				}
				l = uint32(lb[0])
				rec.Buf[f.Offset] = lb[0]
			} else {
				b0, err := consume(1)
				if err != nil {
					return nil, fmt.Errorf("column %q: %w", f.Col.Name, err)
				}
				if b0[0] != 0xff {
					l = uint32(b0[0])
				} else {
					b12, err := consume(2)
					if err != nil {
						return nil, fmt.Errorf("column %q: %w", f.Col.Name, err)
					}
					l = uint32(miBEUint(b12))
				}
				rec.Buf[f.Offset] = byte(l)
				rec.Buf[f.Offset+1] = byte(l >> 8)
			}
			maxData := f.Length - lenBytes
			if l > maxData {
				return nil, fmt.Errorf("column %q: VARCHAR length %d exceeds its own max %d", f.Col.Name, l, maxData)
			}
			data, err := consume(l)
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", f.Col.Name, err)
			}
			copy(rec.Buf[f.Offset+lenBytes:], data)

		case mfBlob:
			if testBit() {
				continue // empty/NULL - length prefix stays zeroed, Blob[fi] stays nil
			}
			sizeLen := f.Length - 8 // portable_sizeof_char_ptr on any 64-bit build (see packLength)
			lb, err := consume(sizeLen)
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", f.Col.Name, err)
			}
			blobLen := decodeUnsignedLE(lb)
			data, err := consume(uint32(blobLen))
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", f.Col.Name, err)
			}
			copy(rec.Buf[f.Offset:f.Offset+sizeLen], lb)
			rec.Blob[fi] = data

		case mfSkipZero:
			if testBit() {
				continue // all-zero - Buf's slot stays zeroed, which decodes to 0 either way
			}
			data, err := consume(f.Length)
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", f.Col.Name, err)
			}
			copy(rec.Buf[f.Offset:], data)

		case mfSkipEndspace, mfSkipPrespace:
			if !testBit() {
				data, err := consume(f.Length)
				if err != nil {
					return nil, fmt.Errorf("column %q: %w", f.Col.Name, err)
				}
				copy(rec.Buf[f.Offset:], data)
				continue
			}
			var l uint32
			if f.Length > 255 {
				b0, err := consume(1)
				if err != nil {
					return nil, fmt.Errorf("column %q: %w", f.Col.Name, err)
				}
				if b0[0]&0x80 != 0 {
					b1, err := consume(1)
					if err != nil {
						return nil, fmt.Errorf("column %q: %w", f.Col.Name, err)
					}
					l = uint32(b0[0]&0x7f) | uint32(b1[0])<<7
				} else {
					l = uint32(b0[0])
				}
			} else {
				b0, err := consume(1)
				if err != nil {
					return nil, fmt.Errorf("column %q: %w", f.Col.Name, err)
				}
				l = uint32(b0[0])
			}
			if l > f.Length {
				return nil, fmt.Errorf("column %q: packed length %d exceeds its own max %d", f.Col.Name, l, f.Length)
			}
			data, err := consume(l)
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", f.Col.Name, err)
			}
			if f.Kind == mfSkipEndspace {
				copy(rec.Buf[f.Offset:], data)
				for i := f.Offset + uint32(len(data)); i < f.Offset+f.Length; i++ {
					rec.Buf[i] = ' '
				}
			} else {
				pad := f.Length - l
				for i := f.Offset; i < f.Offset+pad; i++ {
					rec.Buf[i] = ' '
				}
				copy(rec.Buf[f.Offset+pad:], data)
			}

		default: // mfNormal, including the leading null-bitmap gap
			data, err := consume(f.Length)
			if err != nil {
				name := "<null-bitmap>"
				if f.Col != nil {
					name = f.Col.Name
				}
				return nil, fmt.Errorf("column %q: %w", name, err)
			}
			copy(rec.Buf[f.Offset:], data)
		}
	}
	return rec, nil
}

// unpackMyISAMFixedRecord is the FIXED (static) row format's trivial
// counterpart to unpackMyISAMRecord: every field is stored verbatim, at
// its own fixed offset, with no packing at all - buf IS the record.
func unpackMyISAMFixedRecord(buf []byte, lay *myisamLayout) *myisamRecord {
	return &myisamRecord{Buf: buf, Blob: make([][]byte, len(lay.Fields))}
}

// --- scanning the .MYD file ---

// maxMyISAMBlockChain bounds how many blocks a single record's chain may
// span before this tool gives up and reports it as corrupt - a genuine
// MyISAM row, however large, chains through only as many blocks as its
// length needs (never more than a few thousand for any realistic
// configuration); a much longer chain only happens if a corrupted
// next-block pointer has looped back on itself or wandered into unrelated
// bytes that happen to parse as another plausible-looking block header.
const maxMyISAMBlockChain = 1 << 20

// WalkMyISAMRows scans mydPath's own data file directly, decoding every row
// it can find and streaming it to fn - the MyISAM counterpart to WalkRows
// (btree.go), sharing the same RowOrError/CorruptPage contract closely
// enough that writeSQLTable/TSVDump.AddTable consume either engine's rows
// identically (see RowWalker). There is no notion of a "page" here: each
// RowOrError/CorruptPage's PageNo instead carries the record's own byte
// offset into the .MYD file (truncated to 32 bits - a known v1 limitation
// for a MyISAM data file over 4GB), and RecOff is always 0.
func WalkMyISAMRows(mydPath string, t *Table, lay *myisamLayout, outCols []*Column, format outputFormat,
	skipCorrupted bool, onCorrupt func(CorruptPage), fn func(RowOrError) bool) error {
	f, err := os.Open(mydPath)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()

	fieldIdx := make(map[*Column]int, len(lay.Fields))
	for i, mf := range lay.Fields {
		if mf.Col != nil {
			fieldIdx[mf.Col] = i
		}
	}
	decode := func(rec *myisamRecord, offset int64) RowOrError {
		row, err := decodeMyISAMRow(rec, lay, fieldIdx, outCols, format)
		return RowOrError{PageNo: uint32(offset), Row: row, Err: err}
	}

	if lay.Fixed {
		return scanMyISAMFixed(f, size, lay, skipCorrupted, decode, onCorrupt, fn)
	}
	return scanMyISAMDynamic(f, size, lay, skipCorrupted, decode, onCorrupt, fn)
}

// scanMyISAMFixed walks a FIXED (static) row-format .MYD file: one
// lay.SlotLength-byte slot per record, sequentially from offset 0, with a
// deleted slot marked by a leading zero byte (mirrors
// _mi_read_rnd_static_record's own `if (!*record)` check in mi_statrec.c).
func scanMyISAMFixed(f *os.File, size int64, lay *myisamLayout, skipCorrupted bool,
	decode func(*myisamRecord, int64) RowOrError, onCorrupt func(CorruptPage), fn func(RowOrError) bool) error {
	if lay.SlotLength == 0 {
		return fmt.Errorf("table has a zero-length record - nothing to scan")
	}
	recLen := int64(lay.SlotLength)
	buf := make([]byte, lay.SlotLength)
	for pos := int64(0); pos+recLen <= size; pos += recLen {
		if _, err := io.ReadFull(io.NewSectionReader(f, pos, recLen), buf); err != nil {
			return fmt.Errorf("reading record at offset %d: %w", pos, err)
		}
		if buf[0] == 0 {
			continue // deleted slot
		}
		if !fn(decode(unpackMyISAMFixedRecord(append([]byte(nil), buf[:lay.RecLength]...), lay), pos)) {
			return nil
		}
	}
	if rem := size % recLen; rem != 0 {
		reason := fmt.Sprintf("file size %d isn't a multiple of the fixed record length %d (%d trailing byte(s)) - "+
			"the file looks truncated (not fully copied), rather than corrupted", size, recLen, rem)
		if !skipCorrupted {
			return fmt.Errorf("%s", reason)
		}
		if onCorrupt != nil {
			onCorrupt(CorruptPage{PageNo: uint32(size - rem), Reason: reason, Truncated: true})
		}
	}
	return nil
}

// scanMyISAMDynamic walks a DYNAMIC row-format .MYD file block by block
// from offset 0, exactly the physical order a real unindexed full table
// scan visits records in (_mi_read_rnd_dynamic_record in mi_dynrec.c) -
// see this file's own package comment for why this never needs the .MYI.
//
// By default a structurally-invalid block (bad type byte, a length that
// doesn't fit what's left of the file, a chain that runs off the end or
// loops) is fatal, matching InnoDB's own default-fatal corrupted-page
// behavior. With skipCorrupted, this instead reports it once (onCorrupt)
// and resynchronizes by advancing MI_DYN_ALIGN_SIZE bytes at a time -
// every real block starts on that alignment - until it finds another
// position whose header parses as a plausible record start, silently
// skipping the bytes in between rather than reporting one warning per
// failed alignment step.
func scanMyISAMDynamic(f *os.File, size int64, lay *myisamLayout, skipCorrupted bool,
	decode func(*myisamRecord, int64) RowOrError, onCorrupt func(CorruptPage), fn func(RowOrError) bool) error {
	header := make([]byte, miBlockHeaderLen)
	pos := int64(0)
	inBadStretch := false

	fail := func(pos int64, reason string, truncated bool) error {
		if !skipCorrupted {
			return fmt.Errorf("offset %d: %s", pos, reason)
		}
		if !inBadStretch && onCorrupt != nil {
			onCorrupt(CorruptPage{PageNo: uint32(pos), Reason: reason, Truncated: truncated})
		}
		inBadStretch = true
		return nil
	}

	for pos < size {
		n, err := f.ReadAt(header, pos)
		if err != nil && err != io.EOF {
			return fmt.Errorf("reading block header at offset %d: %w", pos, err)
		}
		if n < miBlockHeaderLen {
			return fail(pos, fmt.Sprintf("only %d byte(s) left, not enough for a block header - "+
				"the file looks truncated (not fully copied), rather than corrupted", n), true)
		}
		first, err := parseMIBlockInfo(header, false)
		// Only types 3/4/9/10 pad their own block_len up to an aligned
		// allocation (via their trailing "extra" header byte); types
		// 1/2/7/8 are an exact fit with no padding at all, so block_len
		// itself is NOT reliably a multiple of MI_DYN_ALIGN_SIZE across
		// every type - the one universal invariant is that the block's
		// full on-disk footprint (header + block_len) must fit in the file.
		// A deleted block (type 0) is the exception: its block_len already
		// covers its own 20-byte header (mi_dynrec.c's delete_dynamic_record
		// stores the whole freed length there), so adding HeaderLen on top
		// would push a free block ending exactly at EOF 20 bytes past it.
		footprint := int64(first.HeaderLen) + int64(first.BlockLen)
		if first.Deleted {
			footprint = int64(first.BlockLen)
		}
		if err != nil || first.BlockLen == 0 || pos+footprint > size {
			reason := "block header failed validation"
			pastEOF := false
			if err != nil {
				reason = err.Error()
			} else if pos+footprint > size {
				reason = fmt.Sprintf("block claims to span %d byte(s), past the end of the file - "+
					"the file looks truncated (not fully copied), rather than corrupted", footprint)
				pastEOF = true
			}
			if e := fail(pos, reason, pastEOF); e != nil {
				return e
			}
			pos += miDynAlignSize
			continue
		}
		inBadStretch = false

		if first.Deleted {
			pos += footprint
			continue
		}
		if first.Continuation {
			// Some other record's later block - already read (or to be
			// read) via that record's own chain; see parseMIBlockInfo.
			pos += footprint
			continue
		}

		// Gather this record's full packed length across every block it
		// spans, mirroring _mi_read_rnd_dynamic_record's own chain walk.
		recBuf := make([]byte, 0, first.RecLen)
		left := first.RecLen
		curPos := pos
		cur := first
		blockNo := 0
		recordStart := pos
		var chainErr error
		for {
			dataStart := curPos + int64(cur.HeaderLen)
			var prefetch []byte
			if blockNo == 0 {
				prefetch = header[cur.HeaderLen:]
			} else {
				// header still holds this (later) block's own header read
				// just below, so the same slicing applies.
				prefetch = header[cur.HeaderLen:]
			}
			need := cur.DataLen
			if uint32(len(prefetch)) > need {
				prefetch = prefetch[:need]
			}
			recBuf = append(recBuf, prefetch...)
			remaining := need - uint32(len(prefetch))
			if remaining > 0 {
				extra := make([]byte, remaining)
				if _, err := f.ReadAt(extra, dataStart+int64(len(prefetch))); err != nil {
					chainErr = fmt.Errorf("reading record data at offset %d: %w", dataStart+int64(len(prefetch)), err)
					break
				}
				recBuf = append(recBuf, extra...)
			}
			if left < cur.DataLen {
				chainErr = fmt.Errorf("block chain data length exceeds the record's own declared length")
				break
			}
			left -= cur.DataLen
			if cur.Last || left == 0 {
				break
			}
			blockNo++
			if blockNo > maxMyISAMBlockChain {
				chainErr = fmt.Errorf("record spans more than %d blocks - its block chain is almost certainly corrupted (a loop, or a pointer into unrelated data)", maxMyISAMBlockChain)
				break
			}
			if cur.NextFilePos == 0 || int64(cur.NextFilePos) <= 0 || int64(cur.NextFilePos) >= size {
				chainErr = fmt.Errorf("block chain continues at offset %d, past the end of the file", cur.NextFilePos)
				break
			}
			curPos = int64(cur.NextFilePos)
			if n, err := f.ReadAt(header, curPos); err != nil && err != io.EOF || n < miBlockHeaderLen {
				chainErr = fmt.Errorf("reading continuation block header at offset %d", curPos)
				break
			}
			cur, err = parseMIBlockInfo(header, true)
			if err != nil {
				chainErr = fmt.Errorf("continuation block at offset %d: %w", curPos, err)
				break
			}
		}

		// The NEXT record search always resumes right after this record's
		// own FIRST block (matches info->nextpos = block_info.filepos +
		// block_info.block_len in _mi_read_rnd_dynamic_record) - later
		// blocks of a fragmented record can physically sit anywhere, even
		// interleaved with other records' own blocks, so scanning must not
		// skip past them. block_info.filepos is the block's DATA start
		// (recordStart+HeaderLen), not its own start - BlockLen alone
		// covers only the data-plus-padding area after the header.
		pos = recordStart + int64(first.HeaderLen) + int64(first.BlockLen)

		if chainErr != nil {
			if e := fail(recordStart, chainErr.Error(), false); e != nil {
				return e
			}
			continue
		}

		rec, uerr := unpackMyISAMRecord(recBuf, lay)
		if uerr != nil {
			if !fn(RowOrError{PageNo: uint32(recordStart), Err: fmt.Errorf("unpacking record: %w", uerr)}) {
				return nil
			}
			continue
		}
		if !fn(decode(rec, recordStart)) {
			return nil
		}
	}
	return nil
}

// --- rendering decoded fields ---

// decodeMyISAMRow renders every outCols entry for one already-unpacked
// MyISAM record as a *Row (btree.go), the same shape WalkRows produces for
// an InnoDB table - see decodeMyISAMField for the actual per-column
// literal/TSV-field rendering.
func decodeMyISAMRow(rec *myisamRecord, lay *myisamLayout, fieldIdx map[*Column]int, outCols []*Column, format outputFormat) (row *Row, err error) {
	defer func() {
		if r := recover(); r != nil {
			row, err = nil, fmt.Errorf("panic decoding row: %v", r)
		}
	}()
	values := make([]string, len(outCols))
	for i, col := range outCols {
		fi, ok := fieldIdx[col]
		if !ok {
			return nil, fmt.Errorf("column %q has no physical field in this table's own layout (internal error)", col.Name)
		}
		f := lay.Fields[fi]
		if f.NullByte >= 0 && rec.Buf[f.NullByte]&f.NullMask != 0 {
			values[i] = nullLiteral(format)
			continue
		}
		var raw []byte
		switch f.Kind {
		case mfBlob:
			raw = rec.Blob[fi]
		case mfVarchar:
			lenBytes := varcharLenBytes(col)
			var l uint32
			if lenBytes == 1 {
				l = uint32(rec.Buf[f.Offset])
			} else {
				l = uint32(rec.Buf[f.Offset]) | uint32(rec.Buf[f.Offset+1])<<8
			}
			raw = rec.Buf[f.Offset+lenBytes : f.Offset+lenBytes+l]
		default:
			raw = rec.Buf[f.Offset : f.Offset+f.Length]
		}
		lit, ferr := decodeMyISAMField(col, raw, format)
		if ferr != nil {
			return nil, fmt.Errorf("column %q: %w", col.Name, ferr)
		}
		values[i] = lit
	}
	return &Row{Values: values}, nil
}

// decodeMyISAMField renders one non-NULL column's raw MyISAM-native bytes
// as a SQL literal or TSV field - the MyISAM counterpart to values.go's
// decodeField. MyISAM (like MEMORY, and row-based binlog images) stores
// fixed-width integers and ENUM/SET indexes as plain little-endian values
// with no InnoDB-style sign flip, so only those decoders differ; every
// other type (FLOAT/DOUBLE, NEWDECIMAL, the "new" 5.6+ temporal formats,
// JSON, and plain string/binary data) already uses a storage-engine-
// agnostic on-disk format, so this calls straight into the same decimal.go/
// temporal.go/jsonb.go/values.go helpers decodeField itself uses.
func decodeMyISAMField(col *Column, raw []byte, format outputFormat) (lit string, err error) {
	defer func() {
		if r := recover(); r != nil {
			lit, err = "", fmt.Errorf("panic decoding column %q: %v", col.Name, r)
		}
	}()
	switch col.DDType {
	case ddTiny, ddShort, ddInt24, ddLong, ddLonglong:
		if col.IsUnsigned {
			return strconv.FormatUint(decodeUnsignedLE(raw), 10), nil
		}
		return strconv.FormatInt(decodeSignedLE(raw), 10), nil

	case ddYear:
		return decodeYear(raw), nil
	case ddNewdate:
		return dateLiteral(decodeMyISAMDate(raw), format), nil
	case ddDatetime2:
		return dateLiteral(decodeDatetime2(raw, int(col.DatetimePrec)), format), nil
	case ddTimestamp2:
		return dateLiteral(decodeTimestamp2(raw, int(col.DatetimePrec)), format), nil
	case ddTime2:
		return dateLiteral(decodeTime2(raw, int(col.DatetimePrec)), format), nil
	case ddDatetime:
		return dateLiteral(decodeOldDatetime(decodeSignedLE(raw)), format), nil
	case ddTime:
		return dateLiteral(decodeOldTime(decodeSignedLE(raw)), format), nil
	case ddDate:
		return dateLiteral(decodeOldDate(decodeSignedLE(raw)), format), nil
	case ddTimestamp:
		return dateLiteral(decodeOldTimestamp(uint32(decodeUnsignedLE(raw))), format), nil

	case ddFloat:
		if len(raw) < 4 {
			return "", fmt.Errorf("column %q: float value truncated", col.Name)
		}
		return strconv.FormatFloat(float64(math.Float32frombits(binary.LittleEndian.Uint32(raw))), 'g', -1, 32), nil
	case ddDouble:
		if len(raw) < 8 {
			return "", fmt.Errorf("column %q: double value truncated", col.Name)
		}
		return strconv.FormatFloat(math.Float64frombits(binary.LittleEndian.Uint64(raw)), 'g', -1, 64), nil

	case ddNewdecimal:
		s, err := decodeDecimal(raw, int(col.NumPrec), int(col.NumScale))
		if err != nil {
			return "", fmt.Errorf("column %q: %w", col.Name, err)
		}
		return s, nil
	case ddDecimal:
		return "", fmt.Errorf("column %q uses the legacy (pre-5.0) DECIMAL storage format, not supported", col.Name)

	case ddEnum:
		idx := decodeUnsignedLE(raw)
		if idx == 0 {
			return quoteText("", format), nil
		}
		if int(idx) > len(col.Elements) {
			return "", fmt.Errorf("column %q: enum index %d out of range", col.Name, idx)
		}
		return quoteText(col.Elements[idx-1], format), nil
	case ddSet:
		bits := decodeUnsignedLE(raw)
		var parts []string
		for i := 0; i < len(col.Elements); i++ {
			if bits&(1<<uint(i)) != 0 {
				parts = append(parts, col.Elements[i])
			}
		}
		return quoteText(strings.Join(parts, ","), format), nil
	case ddBit:
		return "", fmt.Errorf("column %q: BIT columns on MyISAM are not supported yet (v1 limitation)", col.Name)

	case ddJSON:
		return quoteText(jsonBinaryToText(raw), format), nil
	case ddVector:
		return binaryField(raw, format), nil
	case ddGeometry:
		return binaryField(raw, format), nil

	case ddVarString, ddString:
		if col.IsBinary() {
			return binaryField(raw, format), nil // BINARY(N): its 0x00 padding is part of the value
		}
		// A CHAR(N) is stored space-padded to its full width; the server
		// strips that padding again on every read (unless
		// PAD_CHAR_TO_FULL_LENGTH), so do the same - for every charset
		// whose space is the single byte 0x20, i.e. all but the UCS-2/
		// UTF-16/UTF-32 family, which are left as-is.
		if !isWideCharset(col.CollationID) {
			raw = bytes.TrimRight(raw, " ")
		}
		return quoteText(string(raw), format), nil
	case ddVarchar:
		if col.IsBinary() {
			return binaryField(raw, format), nil
		}
		return quoteText(string(raw), format), nil
	case ddTinyBlob, ddMediumBlob, ddBlob, ddLongBlob:
		if col.IsBinary() {
			return binaryField(raw, format), nil
		}
		return quoteText(string(raw), format), nil

	default:
		return "", fmt.Errorf("column %q has an unsupported type code %d", col.Name, col.DDType)
	}
}

// decodeMyISAMDate renders a DATE (MYSQL_TYPE_NEWDATE) column's 3 bytes as
// MyISAM stores them: a plain little-endian int3store of
// year*512 + month*32 + day (Field_newdate::store in field.cc) - unlike
// InnoDB, which stores the same integer big-endian with its sign bit
// flipped (temporal.go's decodeNewDate).
func decodeMyISAMDate(raw []byte) string {
	if len(raw) < 3 {
		return "'0000-00-00'"
	}
	v := uint32(raw[0]) | uint32(raw[1])<<8 | uint32(raw[2])<<16
	return fmt.Sprintf("'%04d-%02d-%02d'", v>>9, (v>>5)&0xf, v&0x1f)
}

// isWideCharset reports whether collation id belongs to a charset whose
// space character isn't the single byte 0x20 (ucs2/utf16/utf16le/utf32) -
// see decodeMyISAMField's CHAR case.
func isWideCharset(id uint64) bool {
	charset, _, _, ok := resolveCollation(id)
	if !ok {
		return false
	}
	switch charset {
	case "ucs2", "utf16", "utf16le", "utf32":
		return true
	}
	return false
}

// myisamRowWalker adapts WalkMyISAMRows into a RowWalker (see btree.go).
func myisamRowWalker(mydPath string, t *Table, lay *myisamLayout, outCols []*Column, format outputFormat, skipCorrupted bool) RowWalker {
	var total int64
	if fi, err := os.Stat(mydPath); err == nil {
		total = fi.Size()
	}
	return RowWalker{
		ProgressTotal: total,
		Walk: func(onCorrupt func(CorruptPage), fn func(RowOrError) bool) error {
			return WalkMyISAMRows(mydPath, t, lay, outCols, format, skipCorrupted, onCorrupt, fn)
		},
	}
}

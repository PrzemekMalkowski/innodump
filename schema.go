// schema.go: turns the raw SDI "dd_object" JSON for a Table into the
// InnoDB-layer model needed to actually decode rows - column storage types
// (mtype/fixed-vs-variable length) and, crucially, the *physical* column
// order of the clustered index's row layout, which is NOT the same as the
// CREATE TABLE column order.
//
// The physical layout (see Index::FillSeIndex in ibdNinja's Index.cc, used
// as the reference) is always:
//
//	[explicit PRIMARY KEY columns, in key order]
//	  (or, if the table has no explicit PK: [DB_ROW_ID])
//	+ [DB_TRX_ID] + [DB_ROLL_PTR]
//	+ [every remaining non-virtual, non-SE-hidden column, in CREATE TABLE order]
//
// v1 does not support tables whose columns carry INSTANT ADD/DROP COLUMN
// history (detected via the "version_added"/"version_dropped"/"instant_col"
// se_private_data markers): the physical layout and NULL-bitmap size then
// depend on which schema version a given *row* was inserted under, which
// needs a lot more machinery (see Record::GetInsertState in ibdNinja) than
// a first prototype warrants. Such tables are reported, not mis-decoded.
package main

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
)

// enum_column_types (Column.h in the DD; values are part of the on-disk SDI
// format, not implementation detail, so hard-coding them here is safe).
const (
	ddDecimal    = 1
	ddTiny       = 2
	ddShort      = 3
	ddLong       = 4
	ddFloat      = 5
	ddDouble     = 6
	ddTypeNull   = 7
	ddTimestamp  = 8
	ddLonglong   = 9
	ddInt24      = 10
	ddDate       = 11
	ddTime       = 12
	ddDatetime   = 13
	ddYear       = 14
	ddNewdate    = 15
	ddVarchar    = 16
	ddBit        = 17
	ddTimestamp2 = 18
	ddDatetime2  = 19
	ddTime2      = 20
	ddNewdecimal = 21
	ddEnum       = 22
	ddSet        = 23
	ddTinyBlob   = 24
	ddMediumBlob = 25
	ddLongBlob   = 26
	ddBlob       = 27
	ddVarString  = 28
	ddString     = 29
	ddGeometry   = 30
	ddJSON       = 31
	ddVector     = 32
)

// Column::enum_hidden_type
const (
	hiddenVisible = 1
	hiddenSE      = 2
	hiddenSQL     = 3
	hiddenUser    = 4
)

// Table::enum_row_format
const (
	rowFormatFixed      = 1
	rowFormatDynamic    = 2
	rowFormatCompressed = 3
	rowFormatRedundant  = 4
	rowFormatCompact    = 5
	rowFormatPaged      = 6
)

// InnoDB mtype (DATA_*), storage/innobase/include/data0type.h.
const (
	dataVarchar   = 1
	dataChar      = 2
	dataFixbinary = 3
	dataBinary    = 4
	dataBlob      = 5
	dataInt       = 6
	dataSys       = 8
	dataFloat     = 9
	dataDouble    = 10
	dataDecimal   = 11
	dataVarmysql  = 12
	dataMysql     = 13
	dataGeometry  = 14
	dataPoint     = 15
	dataVarPoint  = 16
)

const dictMaxFixedColLen = 768

// --- raw SDI JSON shapes (only the fields this tool needs) ---

type ddElementJSON struct {
	Name  string `json:"name"` // base64-encoded label text (ENUM/SET)
	Index uint32 `json:"index"`
}

type ddColumnJSON struct {
	Name                     string          `json:"name"`
	Type                     uint32          `json:"type"`
	IsNullable               bool            `json:"is_nullable"`
	IsZerofill               bool            `json:"is_zerofill"`
	IsUnsigned               bool            `json:"is_unsigned"`
	IsAutoIncrement          bool            `json:"is_auto_increment"`
	IsVirtual                bool            `json:"is_virtual"`
	Hidden                   uint32          `json:"hidden"`
	OrdinalPosition          uint32          `json:"ordinal_position"`
	CharLength               uint32          `json:"char_length"`
	NumericPrecision         uint32          `json:"numeric_precision"`
	NumericScale             uint32          `json:"numeric_scale"`
	DatetimePrecision        uint32          `json:"datetime_precision"`
	HasNoDefault             bool            `json:"has_no_default"`
	DefaultValueUtf8Null     bool            `json:"default_value_utf8_null"`
	DefaultValueUtf8         string          `json:"default_value_utf8"`
	Comment                  string          `json:"comment"`
	GenerationExpressionUtf8 string          `json:"generation_expression_utf8"`
	SePrivateData            string          `json:"se_private_data"`
	ColumnTypeUtf8           string          `json:"column_type_utf8"`
	Elements                 []ddElementJSON `json:"elements"`
	CollationID              uint64          `json:"collation_id"`
	IsExplicitCollation      bool            `json:"is_explicit_collation"`
}

type ddIndexElementJSON struct {
	OrdinalPosition uint32 `json:"ordinal_position"`
	Length          uint32 `json:"length"`
	Order           uint32 `json:"order"`
	Hidden          bool   `json:"hidden"`
	ColumnOpx       uint32 `json:"column_opx"`
}

type ddIndexJSON struct {
	Name          string               `json:"name"`
	Hidden        bool                 `json:"hidden"`
	IsVisible     bool                 `json:"is_visible"`
	SePrivateData string               `json:"se_private_data"`
	Type          uint32               `json:"type"`
	Elements      []ddIndexElementJSON `json:"elements"`
}

type ddTableJSON struct {
	Name          string         `json:"name"`
	Hidden        uint32         `json:"hidden"`
	Columns       []ddColumnJSON `json:"columns"`
	SchemaRef     string         `json:"schema_ref"`
	SePrivateID   uint64         `json:"se_private_id"`
	Comment       string         `json:"comment"`
	SePrivateData string         `json:"se_private_data"`
	RowFormat     uint32         `json:"row_format"`
	PartitionType uint32         `json:"partition_type"`
	CollationID   uint64         `json:"collation_id"`
	Indexes       []ddIndexJSON  `json:"indexes"`
}

// Index::enum_index_type (Index.h in the DD).
const (
	ddIndexPrimary  = 1
	ddIndexUnique   = 2
	ddIndexMultiple = 3
	ddIndexFulltext = 4
	ddIndexSpatial  = 5
)

// IndexColumn::enum_index_element_order (Column.h in the DD).
const ddOrderDesc = 3

// --- resolved (InnoDB-layer) model ---

type Column struct {
	raw ddColumnJSON

	Name            string
	DDType          uint32
	IsNullable      bool
	IsUnsigned      bool
	IsZerofill      bool
	IsAutoIncrement bool
	IsVirtual       bool
	Hidden          uint32
	CharLength      uint32
	NumPrec         uint32
	NumScale        uint32
	DatetimePrec    uint32
	TypeText        string // column_type_utf8, e.g. "varchar(255)"
	CollationID     uint64
	Comment         string
	GenExpr         string
	Elements        []string // decoded ENUM/SET labels, 1-indexed by position
	HasDefault      bool
	DefaultText     string

	Mtype    uint32
	ColLen   uint32 // storage byte length (pack length)
	FixedLen uint32 // 0 => variable-length in the record
	IsSystem bool   // DB_ROW_ID / DB_TRX_ID / DB_ROLL_PTR
}

func (c *Column) IsBinary() bool { return c.CollationID == 63 } // my_charset_bin

// isBigCol matches Column::IsBigCol: columns whose length list entry can
// need the 2-byte/external encoding.
func (c *Column) isBigCol() bool {
	return c.ColLen > 255 || c.Mtype == dataBlob || c.Mtype == dataVarPoint || c.Mtype == dataGeometry
}

type IndexField struct {
	Col         *Column
	PrefixLen   uint32 // 0 = full column
	EffFixedLen uint32 // 0 = variable-length in the record
}

// SecondaryIndexColumn is one column of a non-clustered index, for DDL
// output only (v1 never walks a secondary index for data - the clustered
// index already holds every column).
type SecondaryIndexColumn struct {
	Col       *Column
	PrefixLen uint32 // 0 = no prefix
	Desc      bool
}

// SecondaryIndex is a non-PRIMARY index, for DDL output only.
type SecondaryIndex struct {
	Name              string
	Unique            bool
	Fulltext, Spatial bool
	Visible           bool
	Columns           []SecondaryIndexColumn
}

type Table struct {
	Name, SchemaRef string
	RowFormat       uint32
	SePrivateID     uint64
	Comment         string
	CollationID     uint64 // table's default collation, for DEFAULT CHARSET/COLLATE

	// All columns, in ordinal (CREATE TABLE) order - used for DDL output.
	// Includes SE-hidden system columns; callers filter with Hidden/IsSystem.
	Columns []*Column

	HasExplicitPK  bool
	PKFields       []*IndexField
	PhysicalFields []*IndexField // full clustered-index row layout, in order
	NNullable      int

	SecondaryIndexes []*SecondaryIndex

	// AUTO_INCREMENT: the SDI carries no persisted "next value" (the server
	// tracks that separately, outside any single tablespace file), so main.go
	// fills this in from the highest value actually seen while walking rows.
	AutoIncrementCol  *Column
	AutoIncrementNext *uint64

	RootPage uint32
	IndexID  uint64
}

// decodeElements base64-decodes the ENUM/SET label list from the SDI.
func decodeElements(els []ddElementJSON) []string {
	if len(els) == 0 {
		return nil
	}
	out := make([]string, len(els))
	for _, e := range els {
		label, err := base64.StdEncoding.DecodeString(e.Name)
		idx := e.Index
		if idx < 1 || int(idx) > len(out) {
			continue
		}
		if err != nil {
			out[idx-1] = e.Name
		} else {
			out[idx-1] = string(label)
		}
	}
	return out
}

// sePropString parses the "key1=val1;key2=val2;" se_private_data / options
// encoding used throughout the SDI (see Properties::InsertValues).
func sePropString(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ";") {
		if part == "" {
			continue
		}
		if i := strings.IndexByte(part, '='); i >= 0 {
			out[part[:i]] = part[i+1:]
		}
	}
	return out
}

// packLength mirrors Column::PackLength(): the InnoDB row-storage byte
// length of a fixed-size type, or the VARCHAR field's byte capacity
// (including its own 1/2-byte length prefix - callers of packLength for
// VARCHAR subtract that back off, matching PackLength()'s own caller).
func packLength(c *ddColumnJSON) (uint32, error) {
	dig2bytes := [10]uint32{0, 1, 1, 2, 2, 3, 3, 4, 4, 4}
	switch c.Type {
	case ddVarString, ddString:
		return c.CharLength, nil
	case ddVarchar:
		lenBytes := uint32(1)
		if c.CharLength >= 256 {
			lenBytes = 2
		}
		return lenBytes + c.CharLength, nil
	case ddBlob, ddGeometry, ddJSON, ddVector:
		return 2 + 8, nil
	case ddMediumBlob:
		return 3 + 8, nil
	case ddTinyBlob:
		return 1 + 8, nil
	case ddLongBlob:
		return 4 + 8, nil
	case ddEnum:
		if len(c.Elements) < 256 {
			return 1, nil
		}
		return 2, nil
	case ddSet:
		n := (uint32(len(c.Elements)) + 7) / 8
		if n > 4 {
			return 8, nil
		}
		return n, nil
	case ddDecimal:
		return c.CharLength, nil
	case ddNewdecimal:
		precision, scale := int(c.NumericPrecision), int(c.NumericScale)
		if precision <= 0 || scale < 0 || scale > precision {
			return 0, fmt.Errorf("column %q has an invalid decimal(%d,%d)", c.Name, precision, scale)
		}
		intg := precision - scale
		intg0, frac0 := intg/9, scale/9
		intg0x, frac0x := intg-intg0*9, scale-frac0*9
		return uint32(intg0)*4 + dig2bytes[intg0x] + uint32(frac0)*4 + dig2bytes[frac0x], nil
	case ddFloat:
		return 4, nil
	case ddDouble:
		return 8, nil
	case ddTiny:
		return 1, nil
	case ddShort:
		return 2, nil
	case ddInt24:
		return 3, nil
	case ddLong:
		return 4, nil
	case ddLonglong:
		return 8, nil
	case ddTimestamp:
		return c.CharLength, nil
	case ddTimestamp2:
		return 4 + (c.DatetimePrecision+1)/2, nil
	case ddYear:
		return 1, nil
	case ddNewdate:
		return 3, nil
	case ddTime:
		return 3, nil
	case ddTime2:
		return 3 + (c.DatetimePrecision+1)/2, nil
	case ddDatetime:
		return 8, nil
	case ddDatetime2:
		return 5 + (c.DatetimePrecision+1)/2, nil
	case ddTypeNull:
		return 0, nil
	case ddBit:
		return (c.CharLength + 7) / 8, nil
	default:
		return 0, fmt.Errorf("column %q has an unsupported type code %d", c.Name, c.Type)
	}
}

// fieldType2SeType mirrors Column::FieldType2SeType(): the InnoDB mtype for
// a column, derived from its DD type and binary-ness.
func fieldType2SeType(c *ddColumnJSON, isBinary bool) uint32 {
	if c.Type == ddEnum || c.Type == ddSet {
		return dataInt
	}
	switch c.Type {
	case ddVarString, ddVarchar:
		if isBinary {
			return dataBinary
		} else if c.CollationID == 8 { // latin1_swedish_ci
			return dataVarchar
		}
		return dataVarmysql
	case ddBit, ddString:
		if isBinary {
			return dataFixbinary
		} else if c.CollationID == 8 {
			return dataChar
		}
		return dataMysql
	case ddNewdecimal:
		return dataFixbinary
	case ddLong, ddLonglong, ddTiny, ddShort, ddInt24, ddDate, ddYear, ddNewdate:
		return dataInt
	case ddTime, ddDatetime, ddTimestamp:
		return dataInt
	case ddTime2, ddDatetime2, ddTimestamp2:
		return dataFixbinary
	case ddFloat:
		return dataFloat
	case ddDouble:
		return dataDouble
	case ddDecimal:
		return dataDecimal
	case ddGeometry:
		return dataGeometry
	case ddTinyBlob, ddMediumBlob, ddBlob, ddLongBlob, ddJSON, ddVector:
		return dataBlob
	default:
		return 0
	}
}

// getFixedSize mirrors Column::GetFixedSize(): whether this column occupies
// a fixed number of bytes in the record (returns 0 for variable-length).
func getFixedSize(mtype uint32, colLen uint32, collationID uint64, isBinary bool) uint32 {
	switch mtype {
	case dataSys, dataChar, dataFixbinary, dataInt, dataFloat, dataDouble, dataPoint:
		return colLen
	case dataMysql:
		if isBinary {
			return colLen
		}
		if cl, ok := collationTable[collationID]; ok && cl.min == cl.max {
			return colLen
		}
		return 0
	default: // dataVarchar, dataBinary, dataDecimal, dataVarmysql, dataVarPoint, dataGeometry, dataBlob
		return 0
	}
}

// System-column storage lengths (data0type.h DATA_ROW_ID_LEN/DATA_TRX_ID_LEN/
// DATA_ROLL_PTR_LEN). The SDI records these columns under placeholder DD
// types (e.g. DB_TRX_ID as INT24, DB_ROLL_PTR as LONGLONG) whose PackLength
// does NOT match their real on-disk size, so - like ibdNinja's
// Table::InitSeTable - their length is hard-coded by name instead of
// computed from the DD type.
var systemColLen = map[string]uint32{"DB_ROW_ID": 6, "DB_TRX_ID": 6, "DB_ROLL_PTR": 7}

// buildColumn resolves one SDI column into the InnoDB-layer model.
func buildColumn(raw ddColumnJSON) (*Column, error) {
	isSystem := raw.Name == "DB_ROW_ID" || raw.Name == "DB_TRX_ID" || raw.Name == "DB_ROLL_PTR"
	var pl uint32
	if isSystem {
		pl = systemColLen[raw.Name]
	} else {
		var err error
		pl, err = packLength(&raw)
		if err != nil {
			return nil, err
		}
	}
	c := &Column{
		raw:             raw,
		Name:            raw.Name,
		DDType:          raw.Type,
		IsNullable:      raw.IsNullable,
		IsUnsigned:      raw.IsUnsigned,
		IsZerofill:      raw.IsZerofill,
		IsAutoIncrement: raw.IsAutoIncrement,
		IsVirtual:       raw.IsVirtual,
		Hidden:          raw.Hidden,
		CharLength:      raw.CharLength,
		NumPrec:         raw.NumericPrecision,
		NumScale:        raw.NumericScale,
		DatetimePrec:    raw.DatetimePrecision,
		TypeText:        raw.ColumnTypeUtf8,
		CollationID:     raw.CollationID,
		Comment:         raw.Comment,
		GenExpr:         raw.GenerationExpressionUtf8,
		Elements:        decodeElements(raw.Elements),
		HasDefault:      !raw.HasNoDefault && !raw.DefaultValueUtf8Null,
		DefaultText:     raw.DefaultValueUtf8,
		IsSystem:        isSystem,
	}
	if isSystem {
		c.Mtype = dataSys
		c.ColLen = pl
		c.FixedLen = pl
		return c, nil
	}
	isBinary := c.CollationID == 63
	c.Mtype = fieldType2SeType(&raw, isBinary)
	if c.DDType == ddVarchar {
		lenBytes := uint32(1)
		if c.CharLength >= 256 {
			lenBytes = 2
		}
		c.ColLen = pl - lenBytes
	} else {
		c.ColLen = pl
	}
	c.FixedLen = getFixedSize(c.Mtype, c.ColLen, c.CollationID, isBinary)
	if c.DDType == ddBit {
		// BIT is always physically fixed-width (ceil(N/8) bytes) - unlike a
		// real CHAR(N), there's no trailing-space trimming to make a
		// variable-length encoding meaningful. The DD nonetheless assigns
		// it the table's default (often multi-byte) collation alongside
		// "treat_bit_as_char", which would otherwise make getFixedSize
		// treat it as variable like a genuine multi-byte CHAR.
		c.FixedLen = c.ColLen
	}
	return c, nil
}

// hasInstantHistory reports whether the raw column carries any
// instant-ADD/DROP-COLUMN or row-versioning marker.
func hasInstantHistory(sep string) bool {
	p := sePropString(sep)
	_, added := p["version_added"]
	_, dropped := p["version_dropped"]
	return added || dropped
}

// BuildTable resolves a raw SDI dd_object into the model record.go and
// btree.go need, or returns a descriptive error for a v1-unsupported table
// (compressed/redundant row format, partitioning, or instant ADD/DROP
// COLUMN history).
func BuildTable(raw *ddTableJSON) (*Table, error) {
	if raw.PartitionType != 0 {
		return nil, fmt.Errorf("partitioned tables are not supported (v1 limitation)")
	}
	if raw.RowFormat != rowFormatDynamic && raw.RowFormat != rowFormatCompact {
		return nil, fmt.Errorf("ROW_FORMAT %s is not supported; only DYNAMIC and COMPACT are (v1 limitation)", rowFormatName(raw.RowFormat))
	}
	if p := sePropString(raw.SePrivateData); p["instant_col"] != "" {
		return nil, fmt.Errorf("table has INSTANT ADD/DROP COLUMN history, which v1 does not decode (see limitations)")
	}

	colsByOpx := make([]*Column, len(raw.Columns)) // original SDI array order
	for i, rc := range raw.Columns {
		if hasInstantHistory(rc.SePrivateData) {
			return nil, fmt.Errorf("column %q has INSTANT ADD/DROP COLUMN history, which v1 does not decode (see limitations)", rc.Name)
		}
		c, err := buildColumn(rc)
		if err != nil {
			return nil, err
		}
		colsByOpx[i] = c
	}

	t := &Table{
		Name:        raw.Name,
		SchemaRef:   raw.SchemaRef,
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
	// CREATE TABLE / ordinal order, for DDL and for the "everything else"
	// pass below - stable-sorted defensively even though the SDI already
	// stores columns in this order.
	t.Columns = append([]*Column(nil), colsByOpx...)
	sort.SliceStable(t.Columns, func(i, j int) bool {
		return t.Columns[i].raw.OrdinalPosition < t.Columns[j].raw.OrdinalPosition
	})

	if len(raw.Indexes) == 0 {
		return nil, fmt.Errorf("table has no indexes in its SDI")
	}
	clust := raw.Indexes[0] // Index::FillSeIndex: ind==0 is always the clustered index
	t.HasExplicitPK = !clust.Hidden
	t.IndexID = 0
	if p := sePropString(clust.SePrivateData); p["id"] != "" {
		fmt.Sscanf(p["id"], "%d", &t.IndexID)
	}
	if p := sePropString(clust.SePrivateData); p["root"] != "" {
		fmt.Sscanf(p["root"], "%d", &t.RootPage)
	} else {
		return nil, fmt.Errorf("clustered index has no root page in its SDI")
	}

	for _, el := range clust.Elements {
		if el.Hidden {
			break // the leading run of non-hidden elements is the real PK
		}
		if int(el.ColumnOpx) >= len(colsByOpx) {
			return nil, fmt.Errorf("PRIMARY KEY element references an out-of-range column")
		}
		col := colsByOpx[el.ColumnOpx]
		field := &IndexField{Col: col}
		if col.isBigCol() || el.Length < col.ColLen {
			switch col.Mtype {
			case dataInt, dataFloat, dataDouble, dataDecimal: // no prefix keys on these
			default:
				field.PrefixLen = el.Length
			}
		}
		field.EffFixedLen = effectiveFixedLen(col, field.PrefixLen)
		t.PKFields = append(t.PKFields, field)
	}

	var rowIDCol, trxIDCol, rollPtrCol *Column
	for _, c := range colsByOpx {
		switch c.Name {
		case "DB_ROW_ID":
			rowIDCol = c
		case "DB_TRX_ID":
			trxIDCol = c
		case "DB_ROLL_PTR":
			rollPtrCol = c
		}
	}
	if trxIDCol == nil || rollPtrCol == nil {
		return nil, fmt.Errorf("SDI is missing the DB_TRX_ID/DB_ROLL_PTR system columns")
	}
	if !t.HasExplicitPK && rowIDCol == nil {
		return nil, fmt.Errorf("table has no PRIMARY KEY and no DB_ROW_ID in its SDI")
	}

	fields := append([]*IndexField(nil), t.PKFields...)
	placed := map[*Column]bool{}
	for _, f := range fields {
		placed[f.Col] = true
	}
	addSys := func(c *Column) {
		f := &IndexField{Col: c, EffFixedLen: c.FixedLen}
		fields = append(fields, f)
		placed[c] = true
	}
	if !t.HasExplicitPK {
		addSys(rowIDCol)
	}
	addSys(trxIDCol)
	addSys(rollPtrCol)
	for _, c := range colsByOpx {
		if c.IsSystem || c.IsVirtual || c.Hidden == hiddenSE || placed[c] {
			continue
		}
		fields = append(fields, &IndexField{Col: c, EffFixedLen: c.FixedLen})
		placed[c] = true
	}
	t.PhysicalFields = fields
	for _, f := range fields {
		if f.Col.IsNullable {
			t.NNullable++
		}
	}

	// Secondary indexes are only ever rendered into the DDL, never walked
	// for data (the clustered index already carries every column), so this
	// only needs each index's own explicit (non-hidden) key parts - not the
	// full SE-layer physical-field machinery used for the clustered index.
	for _, idx := range raw.Indexes[1:] {
		if idx.Hidden {
			continue // e.g. an auto-generated helper index
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
			if el.Length < col.ColLen {
				sic.PrefixLen = el.Length
			}
			si.Columns = append(si.Columns, sic)
		}
		if len(si.Columns) > 0 {
			t.SecondaryIndexes = append(t.SecondaryIndexes, si)
		}
	}
	return t, nil
}

func effectiveFixedLen(col *Column, prefixLen uint32) uint32 {
	fixed := col.FixedLen
	if prefixLen > 0 && fixed > prefixLen {
		fixed = prefixLen
	}
	if fixed > dictMaxFixedColLen {
		fixed = 0
	}
	return fixed
}

// charsetOf derives a charset name from its default collation name (e.g.
// "utf8mb4_0900_ai_ci" -> "utf8mb4"): every charset name in collationTable
// is itself free of underscores, so splitting on the first one is exact.
func charsetOf(collationName string) string {
	if i := strings.IndexByte(collationName, '_'); i >= 0 {
		return collationName[:i]
	}
	return collationName
}

func rowFormatName(rf uint32) string {
	switch rf {
	case rowFormatFixed:
		return "FIXED"
	case rowFormatDynamic:
		return "DYNAMIC"
	case rowFormatCompressed:
		return "COMPRESSED"
	case rowFormatRedundant:
		return "REDUNDANT"
	case rowFormatCompact:
		return "COMPACT"
	case rowFormatPaged:
		return "PAGED"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", rf)
	}
}

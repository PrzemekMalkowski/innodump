// frm.go: legacy (MySQL 5.6/5.7) schema discovery. A pre-8.0 tablespace
// carries no SDI at all - the table definition lived in a separate .frm
// file (deleted once MySQL 8.0's data dictionary took over), so reading
// one of these tables needs its .frm file alongside the .ibd (see
// findLegacyFRM in main.go for how that file is located).
//
// Ported from open_binary_frm()/make_field_from_frm() in the mysql-server
// source (the 5.7 branch: sql/table.cc), cross-checked byte-for-byte
// against real .frm files from live MySQL 5.6.51 and 5.7.44 servers before
// trusting any of the offsets below - see this tool's own development
// history for the field-by-field trace that confirmed each one.
//
// Deliberately out of scope, consistent with the rest of this tool's
// "detect and report" philosophy: generated/virtual columns (5.7.6+;
// rejected explicitly rather than mis-parsed - decoding them needs the
// separate gcol_screen section this file doesn't read), views, and any
// .frm not immediately identifiable as a plain InnoDB table (wrong magic,
// too old a version, wrong storage engine, or a "legacy field-position"
// scheme old enough to need the expensive find_field() reconciliation
// open_binary_frm falls back to - vanishingly rare for anything created on
// an actual 5.6/5.7 server). Partitioned tables are NOT explicitly
// rejected: each partition is its own ordinary single-table tablespace
// file sharing the one .frm, and this file's approach happens to still
// work correctly against one of those directly.
//
// Unlike the SDI path (schema.go), a .frm carries no InnoDB-internal
// physical detail at all - notably, no clustered index root page number
// (that lived in the pre-8.0 internal data dictionary, inside ibdata1,
// which this tool has no access to and no need for). This file instead
// relies on a well-known, empirically-confirmed InnoDB convention: a fresh
// file-per-table tablespace's first-created index - the clustered index,
// always created before any secondary index - gets root page 3 (pages
// 0-2 are always the FSP header, the insert buffer bitmap, and the first
// inode page). See guessLegacyRoot.
package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	frmHeaderSize  = 64
	frmFormInfoLen = 288
	// field_pack_length for new_frm_ver>=3 (any .frm from MySQL 4.1
	// onward - always true for a 5.6/5.7 file).
	frmFieldPackLength = 17
	// FRM_VER (field.h) + the "true VARCHAR" version bump (+4) that every
	// 4.1+ file has: the version byte itself (head[2]) is checked against
	// this, and new_frm_ver = head[2] - frmVerBase.
	frmVerBase         = 6
	frmVerTrueVarchar  = frmVerBase + 4
	legacyDBTypeInnoDB = 12
	fieldGeneratedFlag = 0x80 // Field::GENERATED_FIELD, in unireg_type
	fieldNextNumber    = 15   // Field::NEXT_NUMBER, in unireg_type & 0x7f
	fieldMaybeNull     = 0x8000
	fieldDecimalFlag   = 0x0001 // FIELDFLAG_DECIMAL/BINARY - "not unsigned" for numerics
	fieldZerofillFlag  = 0x0004
	fieldDecShift      = 8
	fieldDecMask       = 0x1f
	haNoSame           = 1    // unique
	haFulltext         = 128  // HA_FULLTEXT
	haSpatial          = 1024 // HA_SPATIAL
	haUsesComment      = 4096
	haUsesParser       = 16384
	fieldNrMask        = 16383
	maxTimeWidth       = 10 // sql_const.h
	maxDatetimeWidth   = 19
)

// oldFieldTypeToDD maps a .frm field_type byte (the old, sparse
// enum_field_types) to this tool's ddXxx constants (schema.go), which are
// the DD's own dense renumbering of the very same type list - confirmed
// empirically (see the file comment) rather than assumed from the two
// enums merely looking similar.
var oldFieldTypeToDD = map[byte]uint32{
	0: ddDecimal, 1: ddTiny, 2: ddShort, 3: ddLong, 4: ddFloat, 5: ddDouble,
	6: ddTypeNull, 7: ddTimestamp, 8: ddLonglong, 9: ddInt24, 10: ddDate,
	11: ddTime, 12: ddDatetime, 13: ddYear, 14: ddNewdate, 15: ddVarchar,
	16: ddBit, 17: ddTimestamp2, 18: ddDatetime2, 19: ddTime2,
	245: ddJSON, 246: ddNewdecimal, 247: ddEnum, 248: ddSet,
	249: ddTinyBlob, 250: ddMediumBlob, 251: ddLongBlob, 252: ddBlob,
	253: ddVarString, 254: ddString, 255: ddGeometry,
}

func frmU16(b []byte) uint32 { return uint32(b[0]) | uint32(b[1])<<8 }
func frmU24(b []byte) uint32 { return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 }
func frmU32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// frmKeyPart is one column of one key, in on-disk order.
type frmKeyPart struct {
	fieldnr uint32 // 1-based
	length  uint32 // prefix length in bytes, or the full column's storage length
}

type frmKey struct {
	name                    string
	unique                  bool
	fulltext, spatial       bool
	usesComment, usesParser bool
	parts                   []frmKeyPart
}

// parseFRMKeys reads the key (index) definition block starting at
// data[keyOff:keyOff+keyLen] - see open_binary_frm's "Read keyinformation"
// section. Every 5.6/5.7 file uses the new_frm_ver>=3 (8-byte header,
// 9-byte part) encoding.
func parseFRMKeys(data []byte, keyOff, keyLen uint32) ([]frmKey, error) {
	if int(keyOff+keyLen) > len(data) || keyLen < 6 {
		return nil, fmt.Errorf(".frm file is truncated (key info)")
	}
	buf := data[keyOff : keyOff+keyLen]
	var keys, keyParts uint32
	if buf[0]&0x80 != 0 {
		keys = (uint32(buf[1]) << 7) | uint32(buf[0]&0x7f)
		keyParts = frmU16(buf[2:4])
	} else {
		keys = uint32(buf[0])
		keyParts = uint32(buf[1])
	}
	_ = keyParts
	pos := uint32(6)
	result := make([]frmKey, keys)
	for i := uint32(0); i < keys; i++ {
		if pos+8 > uint32(len(buf)) {
			return nil, fmt.Errorf(".frm file is truncated (key %d header)", i)
		}
		flags := frmU16(buf[pos:pos+2]) ^ haNoSame
		nParts := uint32(buf[pos+4])
		pos += 8
		k := frmKey{
			unique:      flags&haNoSame != 0,
			fulltext:    flags&haFulltext != 0,
			spatial:     flags&haSpatial != 0,
			usesComment: flags&haUsesComment != 0,
			usesParser:  flags&haUsesParser != 0,
		}
		for j := uint32(0); j < nParts; j++ {
			if pos+9 > uint32(len(buf)) {
				return nil, fmt.Errorf(".frm file is truncated (key %d part %d)", i, j)
			}
			fieldnr := frmU16(buf[pos:pos+2]) & fieldNrMask
			length := frmU16(buf[pos+7 : pos+9])
			pos += 9
			k.parts = append(k.parts, frmKeyPart{fieldnr: fieldnr, length: length})
		}
		result[i] = k
	}
	// Key names: one sep-delimited TYPELIB, same encoding as fieldnames
	// (see frmTypelib) - but note the C source copies this whole
	// remaining blob verbatim first (my_stpcpy), so it always runs to a
	// double terminator; frmTypelib's stopping condition already handles
	// that correctly without needing that intermediate copy.
	names, next, err := frmTypelib(buf, pos, 1)
	if err != nil {
		return nil, fmt.Errorf(".frm file is truncated (key names): %w", err)
	}
	for i := range result {
		if i < len(names[0]) {
			result[i].name = names[0][i]
		}
	}
	pos = next
	// Per-key comments, only present when usesComment is set.
	for i := range result {
		if !result[i].usesComment {
			continue
		}
		if pos+2 > uint32(len(buf)) {
			break
		}
		l := frmU16(buf[pos : pos+2])
		pos += 2 + l
	}
	return result, nil
}

// frmTypelib reads `count` sep-delimited string groups starting at
// buf[pos:], mirroring fix_type_pointers in table.cc: each group starts
// with a one-byte separator (0 means an empty group), and its members are
// delimited (and terminated) by that same byte, e.g. "\xffPRIMARY\xff" for
// a single-entry group with separator 0xff - confirmed empirically (see
// the file comment) rather than assumed to be a fixed separator byte.
func frmTypelib(buf []byte, pos uint32, count int) ([][]string, uint32, error) {
	out := make([][]string, count)
	for i := 0; i < count; i++ {
		if pos >= uint32(len(buf)) {
			return nil, 0, fmt.Errorf("ran off the end of the buffer")
		}
		sep := buf[pos]
		if sep == 0 {
			pos++
			continue
		}
		p := pos + 1
		var vals []string
		for {
			// strchr(p, sep): a C-string search, implicitly bounded by
			// the first NUL byte - NOT a plain scan of the rest of the
			// buffer. Getting this wrong (as an earlier version of this
			// function did) makes a group with a trailing "<sep>\x00"
			// terminator - the common case - run straight past it into
			// whatever TYPELIB group happens to follow, since that one
			// often reuses the very same separator byte.
			idx := -1
			for k := p; k < uint32(len(buf)); k++ {
				if buf[k] == sep {
					idx = int(k - p)
					break
				}
				if buf[k] == 0 {
					break
				}
			}
			if idx < 0 {
				break
			}
			vals = append(vals, string(buf[p:p+uint32(idx)]))
			p += uint32(idx) + 1
		}
		out[i] = vals
		pos = p + 1
	}
	return out, pos, nil
}

// frmField is one column's decoded field-definition entry (make_field_from_frm).
type frmField struct {
	name          string
	fieldType     byte
	length        uint32 // field_length
	packFlag      uint32
	unireg        byte // low 7 bits of unireg_type
	generated     bool
	csid          uint32
	comment       string
	intervalIndex uint32 // 1-based; 0 = none
}

// parseFRMFields reads share->fields entries of frmFieldPackLength bytes
// each, plus the trailing names/intervals/comments blobs, from
// data[fieldsOff:] - see open_binary_frm's main field loop.
func parseFRMFields(data []byte, fieldsOff, numFields, nLength, intervalCount, intLength, comLength uint32) ([]frmField, [][]string, error) {
	need := fieldsOff + numFields*frmFieldPackLength + nLength + intLength + comLength
	if int(need) > len(data) {
		return nil, nil, fmt.Errorf(".frm file is truncated (field definitions)")
	}
	namesBlobOff := fieldsOff + numFields*frmFieldPackLength
	namesBlob := data[namesBlobOff : namesBlobOff+nLength+intLength]
	commentsBlob := data[namesBlobOff+nLength+intLength : namesBlobOff+nLength+intLength+comLength]

	fieldNames, next, err := frmTypelib(namesBlob, 0, 1)
	if err != nil {
		return nil, nil, fmt.Errorf(".frm file is truncated (field names): %w", err)
	}
	if len(fieldNames[0]) != int(numFields) {
		return nil, nil, fmt.Errorf(".frm file's field name count (%d) doesn't match its field count (%d)", len(fieldNames[0]), numFields)
	}
	var intervals [][]string
	if intervalCount > 0 {
		intervals, _, err = frmTypelib(namesBlob, next, int(intervalCount))
		if err != nil {
			return nil, nil, fmt.Errorf(".frm file is truncated (interval/enum-set values): %w", err)
		}
	}

	fields := make([]frmField, numFields)
	commentPos := uint32(0)
	for i := uint32(0); i < numFields; i++ {
		fp := data[fieldsOff+i*frmFieldPackLength : fieldsOff+(i+1)*frmFieldPackLength]
		length := frmU16(fp[3:5])
		packFlag := frmU16(fp[8:10])
		unireg := fp[10]
		intervalNr := uint32(fp[12])
		fieldType := fp[13]
		commentLen := frmU16(fp[15:17])

		var csid uint32
		if fieldType != 255 { // MYSQL_TYPE_GEOMETRY shares this byte with geom_type instead
			csid = uint32(fp[14]) + uint32(fp[11])<<8
		}
		var comment string
		if commentLen > 0 {
			if commentPos+commentLen > uint32(len(commentsBlob)) {
				return nil, nil, fmt.Errorf(".frm file is truncated (field comment)")
			}
			comment = string(commentsBlob[commentPos : commentPos+commentLen])
			commentPos += commentLen
		}
		fields[i] = frmField{
			name:          fieldNames[0][i],
			fieldType:     fieldType,
			length:        length,
			packFlag:      packFlag,
			unireg:        unireg & 0x7f,
			generated:     unireg&fieldGeneratedFlag != 0,
			csid:          csid,
			comment:       comment,
			intervalIndex: intervalNr,
		}
	}
	return fields, intervals, nil
}

// buildFRMColumn turns one decoded frmField into this tool's ddColumnJSON
// shape (schema.go), which flows straight into buildColumn()/BuildTable()
// unchanged - see the file comment on why a .frm's on-disk byte lengths
// (field_length) can be handed to those functions' CharLength/NumericXxx
// fields directly, with no unit conversion, for every type this tool
// supports.
func buildFRMColumn(f frmField, ordinal uint32, intervals [][]string) (ddColumnJSON, error) {
	ddType, ok := oldFieldTypeToDD[f.fieldType]
	if !ok {
		return ddColumnJSON{}, fmt.Errorf("column %q has an unrecognized .frm type code %d", f.name, f.fieldType)
	}
	if f.generated {
		return ddColumnJSON{}, fmt.Errorf("column %q is a generated column, which the .frm path does not decode (v1 limitation)", f.name)
	}
	unsigned := f.packFlag&fieldDecimalFlag == 0
	c := ddColumnJSON{
		Name:            f.name,
		Type:            ddType,
		IsNullable:      f.packFlag&fieldMaybeNull != 0,
		IsZerofill:      f.packFlag&fieldZerofillFlag != 0,
		IsUnsigned:      unsigned,
		IsAutoIncrement: f.unireg == fieldNextNumber,
		OrdinalPosition: ordinal,
		CollationID:     uint64(f.csid),
		Comment:         f.comment,
		HasNoDefault:    true, // .frm DEFAULT clauses are not reproduced - see the DDL output note in the README
	}
	if c.CollationID == 0 {
		c.CollationID = 63 // my_charset_bin
	}
	decimals := (f.packFlag >> fieldDecShift) & fieldDecMask

	switch ddType {
	case ddVarchar, ddVarString, ddString:
		c.CharLength = f.length
	case ddNewdecimal:
		sign, point := uint32(0), uint32(0)
		if !unsigned {
			sign = 1
		}
		if decimals > 0 {
			point = 1
		}
		if f.length < sign+point {
			return ddColumnJSON{}, fmt.Errorf("column %q has an implausible DECIMAL display width %d", f.name, f.length)
		}
		c.NumericPrecision = f.length - sign - point
		c.NumericScale = decimals
	case ddFloat, ddDouble:
		c.NumericScale = decimals
	case ddTimestamp, ddDatetime, ddTime, ddDate:
		return ddColumnJSON{}, fmt.Errorf("column %q uses a pre-5.6.4 temporal storage format that v1 does not decode (ALTER TABLE ... FORCE would upgrade it)", f.name)
	case ddTimestamp2:
		if f.length > maxDatetimeWidth {
			c.DatetimePrecision = f.length - 1 - maxDatetimeWidth
		}
	case ddDatetime2:
		if f.length > maxDatetimeWidth {
			c.DatetimePrecision = f.length - 1 - maxDatetimeWidth
		}
	case ddTime2:
		if f.length > maxTimeWidth {
			c.DatetimePrecision = f.length - 1 - maxTimeWidth
		}
	case ddBit:
		c.CharLength = f.length // packLength's ddBit case reads CharLength directly
	case ddEnum, ddSet:
		if f.intervalIndex == 0 || int(f.intervalIndex) > len(intervals) {
			return ddColumnJSON{}, fmt.Errorf("column %q is missing its ENUM/SET value list", f.name)
		}
		vals := intervals[f.intervalIndex-1]
		c.Elements = make([]ddElementJSON, len(vals))
		for i, v := range vals {
			c.Elements[i] = ddElementJSON{Name: base64.StdEncoding.EncodeToString([]byte(v)), Index: uint32(i + 1)}
		}
	}
	c.ColumnTypeUtf8 = frmColumnTypeText(&c, f)
	return c, nil
}

// frmColumnTypeText renders column_type_utf8 (e.g. "varchar(50)", "int(11)
// unsigned zerofill") from a decoded column - the DD computes this once and
// stores it directly in the SDI (schema.go's buildColumn just copies it),
// but a .frm carries only the raw pieces, so this tool has to render it the
// same way MySQL itself would for GenerateDDL to print (sqlout.go copies
// this field verbatim into the CREATE TABLE line).
func frmColumnTypeText(c *ddColumnJSON, f frmField) string {
	isBinary := c.CollationID == 63
	numSuffix := func(base string) string {
		s := base
		if c.IsUnsigned {
			s += " unsigned"
		}
		if c.IsZerofill {
			s += " zerofill"
		}
		return s
	}
	charLen := func() uint32 {
		maxBytes := 1
		if cl, ok := collationInfoFor(c.CollationID); ok && cl.max > 0 {
			maxBytes = cl.max
		}
		return c.CharLength / uint32(maxBytes)
	}
	switch c.Type {
	case ddTiny:
		return numSuffix(fmt.Sprintf("tinyint(%d)", f.length))
	case ddShort:
		return numSuffix(fmt.Sprintf("smallint(%d)", f.length))
	case ddInt24:
		return numSuffix(fmt.Sprintf("mediumint(%d)", f.length))
	case ddLong:
		return numSuffix(fmt.Sprintf("int(%d)", f.length))
	case ddLonglong:
		return numSuffix(fmt.Sprintf("bigint(%d)", f.length))
	case ddFloat:
		return numSuffix("float")
	case ddDouble:
		return numSuffix("double")
	case ddNewdecimal:
		return numSuffix(fmt.Sprintf("decimal(%d,%d)", c.NumericPrecision, c.NumericScale))
	case ddYear:
		return "year(4)"
	case ddNewdate:
		return "date"
	case ddTime2:
		if c.DatetimePrecision > 0 {
			return fmt.Sprintf("time(%d)", c.DatetimePrecision)
		}
		return "time"
	case ddDatetime2:
		if c.DatetimePrecision > 0 {
			return fmt.Sprintf("datetime(%d)", c.DatetimePrecision)
		}
		return "datetime"
	case ddTimestamp2:
		if c.DatetimePrecision > 0 {
			return fmt.Sprintf("timestamp(%d)", c.DatetimePrecision)
		}
		return "timestamp"
	case ddVarchar:
		return fmt.Sprintf("varchar(%d)", charLen())
	case ddVarString, ddString:
		if isBinary {
			return fmt.Sprintf("binary(%d)", c.CharLength)
		}
		return fmt.Sprintf("char(%d)", charLen())
	case ddBit:
		return fmt.Sprintf("bit(%d)", f.length)
	case ddTinyBlob:
		if isBinary {
			return "tinyblob"
		}
		return "tinytext"
	case ddMediumBlob:
		if isBinary {
			return "mediumblob"
		}
		return "mediumtext"
	case ddLongBlob:
		if isBinary {
			return "longblob"
		}
		return "longtext"
	case ddBlob:
		if isBinary {
			return "blob"
		}
		return "text"
	case ddEnum:
		return "enum(" + frmQuoteElements(c.Elements) + ")"
	case ddSet:
		return "set(" + frmQuoteElements(c.Elements) + ")"
	case ddGeometry:
		return "geometry"
	case ddJSON:
		return "json"
	default:
		return ""
	}
}

func frmQuoteElements(els []ddElementJSON) string {
	parts := make([]string, len(els))
	for _, e := range els {
		if e.Index < 1 || int(e.Index) > len(parts) {
			continue
		}
		label, err := base64.StdEncoding.DecodeString(e.Name)
		if err != nil {
			label = []byte(e.Name)
		}
		parts[e.Index-1] = "'" + strings.ReplaceAll(string(label), "'", "''") + "'"
	}
	return strings.Join(parts, ",")
}

// legacyRootGuess is InnoDB's own well-known convention for a fresh
// file-per-table tablespace - see the file comment.
const legacyRootGuess = 3

// guessLegacyRootPage returns page 3's own page number and index id, after
// confirming it actually is an INDEX page (the one real check this
// convention affords - if it's wrong, this is where it'll show).
func guessLegacyRootPage(sp *Space) (pageNo uint32, indexID uint64, err error) {
	page, err := sp.ReadPage(legacyRootGuess)
	if err != nil {
		return 0, 0, err
	}
	if filType(page) != filPageIndex {
		return 0, 0, fmt.Errorf("page %d (the conventional root page for a table's first index) has type %d, not INDEX - this .ibd doesn't match the layout this tool assumes for a pre-8.0 file", legacyRootGuess, filType(page))
	}
	return legacyRootGuess, pageGetIndexID(page), nil
}

// legacyRowFormat determines the table's ROW_FORMAT from the tablespace
// itself - not from the .frm's own (possibly stale, or simply
// ROW_TYPE_DEFAULT/unspecified) row_type byte - the same way every other
// format decision in this tool is made: by looking at what's actually on
// disk. REDUNDANT/COMPACT is the page's own "new record format" bit;
// DYNAMIC vs COMPACT (both share that bit) is FSP_FLAGS' atomic_blobs bit,
// the actual on-disk difference between the two (whether off-page columns
// keep a fixed inline prefix).
func legacyRowFormat(sp *Space, rootPage []byte) uint32 {
	if sp.Compressed {
		return rowFormatCompressed
	}
	if !pageIsCompact(rootPage) {
		return rowFormatRedundant
	}
	if sp.Flags.atomicBlobs {
		return rowFormatDynamic
	}
	return rowFormatCompact
}

// parseFRM reads path (a MySQL 5.6/5.7 .frm file) and sp's guessed root
// page, and returns the same *ddTableJSON shape LoadSDITables would, so it
// flows into BuildTable completely unchanged - see the file comment.
func parseFRM(path string, sp *Space) (*ddTableJSON, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < frmHeaderSize {
		return nil, fmt.Errorf(".frm file is too short to be valid")
	}
	head := data[:frmHeaderSize]
	if head[0] != 0xFE || head[1] != 1 {
		return nil, fmt.Errorf(".frm file has an unrecognized header (not a MySQL table definition)")
	}
	if bytesHasPrefix(data, []byte("TYPE=VIEW")) {
		return nil, fmt.Errorf(".frm file is a VIEW, not a table")
	}
	frmVer := head[2]
	if frmVer == frmVerTrueVarchar-1 && head[33] == 5 {
		// A 4.1 file whose CHAR columns should still display as CHAR
		// rather than VARCHAR (open_binary_frm's own frm_version bump) -
		// this is the true-VARCHAR format either way; only the reported
		// version byte is one lower.
		frmVer = frmVerTrueVarchar
	}
	if frmVer < frmVerTrueVarchar {
		return nil, fmt.Errorf(".frm file version %d predates MySQL 4.1 VARCHAR support, which v1 does not decode", frmVer)
	}
	newFrmVer := int(frmVer) - frmVerBase
	if newFrmVer < 3 {
		return nil, fmt.Errorf(".frm file format (version %d) is not supported (v1 limitation)", frmVer)
	}
	if head[3] != legacyDBTypeInnoDB {
		return nil, fmt.Errorf(".frm file's storage engine (legacy type %d) is not InnoDB", head[3])
	}
	if head[27] <= 1 {
		return nil, fmt.Errorf(".frm file predates a field-position feature this tool relies on; an ALTER TABLE ... FORCE on the original server would rewrite it")
	}

	namesLen := frmU16(head[4:6])
	namesCount := frmU16(head[8:10])
	if namesCount == 0 {
		return nil, fmt.Errorf(".frm file has no form position (possibly a VIEW)")
	}
	formPosOff := frmHeaderSize + namesLen
	if int(formPosOff+4) > len(data) {
		return nil, fmt.Errorf(".frm file is truncated (form position table)")
	}
	formPos := frmU32(data[formPosOff : formPosOff+4])
	if int(formPos+frmFormInfoLen) > len(data) {
		return nil, fmt.Errorf(".frm file is truncated (form info)")
	}
	forminfo := data[formPos : formPos+frmFormInfoLen]

	numFields := frmU16(forminfo[258:260])
	screensLen := frmU16(forminfo[260:262])
	nLength := frmU16(forminfo[268:270])
	intervalCount := frmU16(forminfo[270:272])
	intLength := frmU16(forminfo[274:276])
	comLength := frmU16(forminfo[284:286])

	var tableComment string
	if forminfo[46] != 255 {
		tableComment = string(forminfo[47 : 47+uint32(forminfo[46])])
	}

	keyInfoOffset := frmU16(head[6:8])
	keyInfoLength := frmU16(head[28:30])
	keys, err := parseFRMKeys(data, keyInfoOffset, keyInfoLength)
	if err != nil {
		return nil, err
	}

	fieldsOff := formPos + frmFormInfoLen + screensLen
	fields, intervals, err := parseFRMFields(data, fieldsOff, numFields, nLength, intervalCount, intLength, comLength)
	if err != nil {
		return nil, err
	}

	// Neither the table name nor its schema are recorded inside the .frm
	// itself - both come from its own path, mirroring MySQL's own
	// directory-per-database, file-per-table convention
	// (datadir/dbname/tablename.frm) that this .frm/.ibd pair must live
	// under for findLegacyFRM to have found it there in the first place.
	tableName := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	schemaRef := filepath.Base(filepath.Dir(path))
	rawTable := &ddTableJSON{Name: tableName, SchemaRef: schemaRef, Comment: tableComment, CollationID: uint64(tableCharsetID(head))}
	colsByOpx := make([]ddColumnJSON, 0, numFields+3)
	for i, f := range fields {
		c, err := buildFRMColumn(f, uint32(i), intervals)
		if err != nil {
			return nil, err
		}
		colsByOpx = append(colsByOpx, c)
	}

	// PRIMARY key detection: a key literally named "PRIMARY" (case-
	// insensitive) - the overwhelming common case. Anything else (a
	// UNIQUE NOT NULL key silently promoted to PK, no explicit PK at all)
	// falls back to InnoDB's own hidden DB_ROW_ID, matching BuildTable's
	// existing "!HasExplicitPK" path.
	pkIdx := -1
	for i, k := range keys {
		if strings.EqualFold(k.name, "PRIMARY") {
			pkIdx = i
			break
		}
	}

	clust := ddIndexJSON{Name: "PRIMARY", Type: ddIndexPrimary}
	if pkIdx >= 0 {
		k := keys[pkIdx]
		for _, p := range k.parts {
			if p.fieldnr == 0 || int(p.fieldnr) > len(colsByOpx) {
				return nil, fmt.Errorf("PRIMARY KEY references an out-of-range column")
			}
			col := colsByOpx[p.fieldnr-1]
			el := ddIndexElementJSON{ColumnOpx: p.fieldnr - 1}
			fullLen, err := packLength(&col)
			if err == nil && p.length < fullLen {
				el.Length = p.length
			}
			clust.Elements = append(clust.Elements, el)
		}
	} else {
		clust.Hidden = true
		clust.Name = "GEN_CLUST_INDEX"
	}

	rootPage, indexID, err := guessLegacyRootPage(sp)
	if err != nil {
		return nil, err
	}
	clust.SePrivateData = fmt.Sprintf("root=%d;id=%d;", rootPage, indexID)
	rawTable.Indexes = append(rawTable.Indexes, clust)

	for i, k := range keys {
		if i == pkIdx || k.fulltext || k.spatial {
			continue // fulltext/spatial rendering isn't attempted for the .frm path (v1 limitation)
		}
		idx := ddIndexJSON{Name: k.name, IsVisible: true}
		if k.unique {
			idx.Type = ddIndexUnique
		} else {
			idx.Type = ddIndexMultiple
		}
		for _, p := range k.parts {
			if p.fieldnr == 0 || int(p.fieldnr) > len(colsByOpx) {
				continue
			}
			col := colsByOpx[p.fieldnr-1]
			el := ddIndexElementJSON{ColumnOpx: p.fieldnr - 1}
			if fullLen, err := packLength(&col); err == nil && p.length < fullLen {
				el.Length = p.length
			}
			idx.Elements = append(idx.Elements, el)
		}
		rawTable.Indexes = append(rawTable.Indexes, idx)
	}

	// InnoDB's own system columns are never described in a .frm at all
	// (pre-8.0, InnoDB tracked them internally, invisible even to the SQL
	// layer) - added here exactly as the SDI always lists them, so
	// BuildTable's system-column lookups (by name) succeed unchanged.
	nextOpx := uint32(len(colsByOpx))
	addSys := func(name string) {
		colsByOpx = append(colsByOpx, ddColumnJSON{Name: name, Hidden: hiddenSE, OrdinalPosition: nextOpx})
		nextOpx++
	}
	if pkIdx < 0 {
		addSys("DB_ROW_ID")
	}
	addSys("DB_TRX_ID")
	addSys("DB_ROLL_PTR")

	rawTable.Columns = colsByOpx
	rootRaw, err := sp.ReadPage(rootPage)
	if err != nil {
		return nil, err
	}
	rawTable.RowFormat = legacyRowFormat(sp, rootRaw)
	return rawTable, nil
}

// tableCharsetID reads the table's own default charset/collation id
// (share->table_charset in open_binary_frm), used only for the
// DEFAULT CHARSET clause in GenerateDDL's output.
func tableCharsetID(head []byte) uint32 {
	if head[32] != 0 {
		return 63 // pre-3.23 frm: no reliable charset info at all
	}
	return uint32(head[41])<<8 | uint32(head[38])
}

func bytesHasPrefix(b, prefix []byte) bool {
	return len(b) >= len(prefix) && string(b[:len(prefix)]) == string(prefix)
}

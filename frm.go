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
// The same parser (readFRMDefinition) also serves a pre-8.0 MyISAM table's
// .frm (parseMyISAMFRM, used by myisam.go) - everything up to the column
// and key definitions is engine-independent; only what each engine then
// needs on top (InnoDB's clustered index and system columns, MyISAM's row
// format) differs.
//
// Deliberately out of scope, consistent with the rest of this tool's
// "detect and report" philosophy: generated/virtual columns (5.7.6+;
// rejected explicitly rather than mis-parsed - decoding them needs the
// separate gcol_screen section this file doesn't read), views, and any
// .frm not immediately identifiable as a plain InnoDB/MyISAM table (wrong magic,
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
		// Pre-5.6.4 temporal storage (see temporal.go's decodeOldXxx):
		// no fractional seconds, so nothing beyond the type itself to carry.
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
	case ddNewdate, ddDate:
		return "date"
	case ddTime:
		return "time"
	case ddDatetime:
		return "datetime"
	case ddTimestamp:
		return "timestamp"
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
		if isBinary {
			return fmt.Sprintf("varbinary(%d)", c.CharLength)
		}
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

// frmDefinition is the engine-independent part of one parsed .frm file:
// the table's name, schema, comment, default collation, and columns
// (already converted to ddColumnJSON), plus its raw key list and the two
// header fields a caller needs to decide what to do with it - which
// storage engine it describes, and its db_create_options bits. parseFRM
// (InnoDB) and parseMyISAMFRM (MyISAM) each turn this into their own
// engine's ddTableJSON shape.
type frmDefinition struct {
	table         *ddTableJSON // Indexes left empty - engine-specific, see parseFRM/parseMyISAMFRM
	keys          []frmKey
	legacyDBType  byte   // head[3], enum legacy_db_type
	createOptions uint32 // head[30:32], db_create_options (HA_OPTION_* bits)
	// partitionClause is a partitioned table's own "PARTITION BY ..."
	// clause text, exactly as the server stored it (see
	// frmPartitionClause); "" for a table that isn't partitioned.
	partitionClause string
	// partDBType is head[61], default_part_db_type: the storage engine of
	// a partitioned table's partitions, which legacyDBType can't say
	// itself when the table uses the generic partitioning engine (5.6's
	// ha_partition, legacyDBTypePartitioned).
	partDBType byte
}

// engineIsInnoDB reports whether def describes an InnoDB table - a plain
// one, a 5.7 natively partitioned one (legacy type InnoDB, plus a
// partition clause), or a 5.6-style one whose partitions are InnoDB
// tables under the generic partitioning engine.
func (def *frmDefinition) engineIsInnoDB() bool {
	return def.legacyDBType == legacyDBTypeInnoDB ||
		(def.legacyDBType == legacyDBTypePartitioned && def.partDBType == legacyDBTypeInnoDB)
}

// frmPartitionClause reads the partition clause text out of a .frm's
// "extra data segment" - open_binary_frm in sql/table.cc (5.7): the
// segment, uint4korr(head+55) bytes long, sits right after the default
// record (at uint2korr(head+6) + the form position length, plus
// reclength), and holds a 2-byte-length connect string, a 2-byte-length
// storage engine name ("partition" for 5.6-style partitioning), then a
// 4-byte-length partition clause - " PARTITION BY RANGE (...) (PARTITION
// p0 VALUES LESS THAN (...) ENGINE = InnoDB, ...)", exactly as the server
// itself generated it. "" if there's none (not partitioned).
func frmPartitionClause(data []byte) string {
	head := data[:frmHeaderSize]
	extraLen := frmU32(head[55:59])
	if extraLen == 0 {
		return ""
	}
	recordOffset := frmU16(head[6:8])
	if frmU16(head[14:16]) == 0xffff {
		recordOffset += frmU32(head[47:51])
	} else {
		recordOffset += frmU16(head[14:16])
	}
	start := uint64(recordOffset) + uint64(frmU16(head[16:18]))
	end := start + uint64(extraLen)
	if end > uint64(len(data)) {
		return ""
	}
	seg := data[start:end]
	pos := 0
	skip2 := func() bool { // one 2-byte-length-prefixed string
		if pos+2 > len(seg) {
			return false
		}
		pos += 2 + int(frmU16(seg[pos:pos+2]))
		return pos <= len(seg)
	}
	if !skip2() || !skip2() { // connect string, engine name
		return ""
	}
	if pos+5 > len(seg) {
		return ""
	}
	n := int(frmU32(seg[pos : pos+4]))
	if n == 0 || pos+4+n > len(seg) {
		return ""
	}
	return string(seg[pos+4 : pos+4+n])
}

// Legacy (pre-8.0) .frm storage engine codes - enum legacy_db_type in
// mysql-server's sql/handler.h. Only the ones worth naming in an error
// message are listed; any engine loaded as a plugin gets a dynamic code
// (42 and up) instead.
const (
	legacyDBTypeHeap        = 6
	legacyDBTypeMyISAM      = 9
	legacyDBTypeMrgMyISAM   = 10
	legacyDBTypeArchive     = 16
	legacyDBTypeCSV         = 17
	legacyDBTypeFederated   = 18
	legacyDBTypeBlackhole   = 19
	legacyDBTypePartitioned = 20
)

// legacyDBTypeName names a legacy_db_type code for error messages.
func legacyDBTypeName(t byte) string {
	switch t {
	case legacyDBTypeHeap:
		return "MEMORY"
	case legacyDBTypeMyISAM:
		return "MyISAM"
	case legacyDBTypeMrgMyISAM:
		return "MRG_MYISAM"
	case legacyDBTypeInnoDB:
		return "InnoDB"
	case legacyDBTypeArchive:
		return "ARCHIVE"
	case legacyDBTypeCSV:
		return "CSV"
	case legacyDBTypeFederated:
		return "FEDERATED"
	case legacyDBTypeBlackhole:
		return "BLACKHOLE"
	case legacyDBTypePartitioned:
		return "a partitioned table"
	}
	return fmt.Sprintf("legacy type %d", t)
}

// decodeMySQLFilename reverses the one part of MySQL's filename-safe
// identifier encoding (my_charset_filename, used for every database and
// table directory/file name since 5.1) that real-world names actually
// hit: "@" followed by four hex digits encodes one character by its own
// Unicode code point - "@0020" for a space, "@002d" for "-", and so on -
// so a table created as `TABLE 75` lives in "TABLE@002075.frm". The
// encoding's other, two-character "@xy" form (accented Latin, Greek,
// Cyrillic, full-width letters) is left as-is.
func decodeMySQLFilename(name string) string {
	if !strings.Contains(name, "@") {
		return name
	}
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] == '@' && i+5 <= len(name) {
			var cp rune
			ok := true
			for _, h := range name[i+1 : i+5] {
				switch {
				case h >= '0' && h <= '9':
					cp = cp<<4 | (h - '0')
				case h >= 'a' && h <= 'f':
					cp = cp<<4 | (h - 'a' + 10)
				case h >= 'A' && h <= 'F':
					cp = cp<<4 | (h - 'A' + 10)
				default:
					ok = false
				}
			}
			if ok {
				b.WriteRune(cp)
				i += 4
				continue
			}
		}
		b.WriteByte(name[i])
	}
	return b.String()
}

// readFRMDefinition parses path (a MySQL 5.6/5.7 or MariaDB .frm file)
// into a frmDefinition, rejecting anything that isn't a plain table
// definition this file knows how to read - but, unlike parseFRM, not
// checking which storage engine it belongs to.
func readFRMDefinition(path string) (*frmDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if bytesHasPrefix(data, []byte("TYPE=VIEW")) {
		return nil, fmt.Errorf(".frm file is a VIEW, not a table")
	}
	if len(data) < frmHeaderSize {
		return nil, fmt.Errorf(".frm file is too short to be valid")
	}
	head := data[:frmHeaderSize]
	if head[0] != 0xFE || head[1] != 1 {
		return nil, fmt.Errorf(".frm file has an unrecognized header (not a MySQL table definition)")
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
	// (datadir/dbname/tablename.frm) - decoded back from MySQL's
	// filename-safe encoding (see decodeMySQLFilename).
	tableName := decodeMySQLFilename(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	schemaRef := decodeMySQLFilename(filepath.Base(filepath.Dir(path)))
	rawTable := &ddTableJSON{Name: tableName, SchemaRef: schemaRef, Comment: tableComment, CollationID: uint64(tableCharsetID(head))}
	rawTable.Columns = make([]ddColumnJSON, 0, numFields+3)
	for i, f := range fields {
		c, err := buildFRMColumn(f, uint32(i), intervals)
		if err != nil {
			return nil, err
		}
		rawTable.Columns = append(rawTable.Columns, c)
	}
	return &frmDefinition{table: rawTable, keys: keys, legacyDBType: head[3], createOptions: frmU16(head[30:32]),
		partitionClause: frmPartitionClause(data), partDBType: head[61]}, nil
}

// frmPrimaryKeyIndex returns the position of keys' PRIMARY KEY, or -1:
// a key literally named "PRIMARY" (case-insensitive) - the overwhelming
// common case.
func frmPrimaryKeyIndex(keys []frmKey) int {
	for i, k := range keys {
		if strings.EqualFold(k.name, "PRIMARY") {
			return i
		}
	}
	return -1
}

// frmPromotedUniqueKey returns the position of the key InnoDB uses as a
// table's clustered index when it has no PRIMARY KEY, or -1 if there is
// none (InnoDB then clusters on a hidden DB_ROW_ID): the first UNIQUE key
// - in the .frm's own key order, which the server already sorted UNIQUE
// NOT NULL keys to the front of - whose every part is a NOT NULL column
// indexed in full, not by a prefix. Mirrors open_binary_frm's own
// primary_key selection in sql/table.cc, which is what InnoDB is handed as
// "the primary key" for such a table.
func frmPromotedUniqueKey(keys []frmKey, cols []ddColumnJSON) int {
	for i, k := range keys {
		if !k.unique || k.fulltext || k.spatial || len(k.parts) == 0 {
			continue
		}
		ok := true
		for _, p := range k.parts {
			if p.fieldnr == 0 || int(p.fieldnr) > len(cols) {
				ok = false
				break
			}
			col := cols[p.fieldnr-1]
			if col.IsNullable || isBlobFamily(col.Type) || p.length != frmKeyLength(&col) {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// frmKeyLength is a column's full key length in bytes (Field::key_length()
// in the server): its pack length, minus a VARCHAR's own length prefix.
func frmKeyLength(col *ddColumnJSON) uint32 {
	pl, err := packLength(col)
	if err != nil {
		return 0
	}
	if col.Type == ddVarchar {
		if col.CharLength >= 256 {
			return pl - 2
		}
		return pl - 1
	}
	return pl
}

// parseFRM reads path (a MySQL 5.6/5.7 .frm file) and returns the same
// *ddTableJSON shape LoadSDITables would, so it flows into BuildTable
// completely unchanged - see the file comment. loc is nil for an ordinary
// file-per-table .ibd, where sp IS that table's own tablespace and its
// clustered index root page is found via the well-known "root page 3"
// convention (guessLegacyRootPage); non-nil when the table's data instead
// lives in a shared/system tablespace (sysdict.go's FindTableLocation),
// giving the actual root page/index id its own SYS_INDEXES row recorded -
// the "page 3" convention only ever held for a table's own dedicated file.
func parseFRM(path string, sp *Space, loc *TableLocation) (*ddTableJSON, error) {
	def, err := readFRMDefinition(path)
	if err != nil {
		return nil, err
	}
	if !def.engineIsInnoDB() {
		engine := legacyDBTypeName(def.legacyDBType)
		if def.legacyDBType == legacyDBTypePartitioned {
			engine = "a partitioned " + legacyDBTypeName(def.partDBType) + " table"
		}
		return nil, fmt.Errorf(".frm file's storage engine (%s) is not InnoDB", engine)
	}
	rawTable, keys := def.table, def.keys
	colsByOpx := rawTable.Columns

	// The clustered index: the PRIMARY KEY (frmPrimaryKeyIndex), else the
	// UNIQUE key InnoDB promotes in its place (frmPromotedUniqueKey), else
	// InnoDB's own hidden DB_ROW_ID, matching BuildTable's existing
	// "!HasExplicitPK" path.
	pkIdx := frmPrimaryKeyIndex(keys)
	clust := ddIndexJSON{Name: "PRIMARY", Type: ddIndexPrimary}
	if pkIdx < 0 {
		if pkIdx = frmPromotedUniqueKey(keys, colsByOpx); pkIdx >= 0 {
			clust = ddIndexJSON{Name: keys[pkIdx].name, Type: ddIndexUnique}
		}
	}
	if pkIdx >= 0 {
		k := keys[pkIdx]
		for _, p := range k.parts {
			if p.fieldnr == 0 || int(p.fieldnr) > len(colsByOpx) {
				return nil, fmt.Errorf("PRIMARY KEY references an out-of-range column")
			}
			col := colsByOpx[p.fieldnr-1]
			el := ddIndexElementJSON{ColumnOpx: p.fieldnr - 1}
			fullLen, err := packLength(&col)
			if err == nil && (p.length < fullLen || isBlobFamily(col.Type)) {
				el.Length = p.length
			}
			clust.Elements = append(clust.Elements, el)
		}
	} else {
		clust.Hidden = true
		clust.Name = "GEN_CLUST_INDEX"
	}

	var rootPage uint32
	var indexID uint64
	if loc != nil {
		rootPage, indexID = loc.RootPage, loc.IndexID
	} else {
		var err error
		rootPage, indexID, err = guessLegacyRootPage(sp)
		if err != nil {
			return nil, err
		}
	}
	clust.SePrivateData = fmt.Sprintf("root=%d;id=%d;", rootPage, indexID)
	rawTable.Indexes = append(rawTable.Indexes, clust)

	for i, k := range keys {
		if i == pkIdx || k.fulltext || k.spatial {
			continue // fulltext/spatial rendering isn't attempted for the InnoDB .frm path (v1 limitation)
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
			// A BLOB/TEXT key part is always a prefix, even though its
			// length exceeds the column's own (tiny) in-record pack length.
			if fullLen, err := packLength(&col); err == nil && (p.length < fullLen || isBlobFamily(col.Type)) {
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

// haOptionPackRecord is HA_OPTION_PACK_RECORD (my_base.h), the
// db_create_options bit a .frm (and a .MYI's own header) sets when a
// MyISAM table uses the DYNAMIC row format rather than FIXED.
const haOptionPackRecord = 1

// parseMyISAMFRM reads path (a pre-8.0 MySQL/MariaDB MyISAM table's .frm)
// into the ddTableJSON shape BuildMyISAMTable expects - the same shape a
// MySQL 8.0+ standalone .sdi unmarshals into (see myisam.go's
// findMyISAMSchema). Unlike parseFRM there's no InnoDB-internal detail to
// add (no clustered index root page, no DB_TRX_ID/DB_ROLL_PTR): a MyISAM
// table's .frm columns are exactly its physical columns, and its keys
// (FULLTEXT/SPATIAL included) come through as-is. RowFormat is set from
// the .frm's own db_create_options (DYNAMIC if HA_OPTION_PACK_RECORD is
// set, FIXED otherwise) - findMyISAMSchema overrides that from the .MYI
// header when one is there, since only the .MYI records whether
// myisampack has since compressed the table.
func parseMyISAMFRM(path string) (*ddTableJSON, error) {
	def, err := readFRMDefinition(path)
	if err != nil {
		return nil, err
	}
	if def.legacyDBType != legacyDBTypeMyISAM {
		return nil, fmt.Errorf(".frm file's storage engine (%s) is not MyISAM", legacyDBTypeName(def.legacyDBType))
	}
	rawTable, keys := def.table, def.keys
	rawTable.Engine = "MyISAM"
	rawTable.RowFormat = rowFormatFixed
	if def.createOptions&haOptionPackRecord != 0 {
		rawTable.RowFormat = rowFormatDynamic
	}

	// BuildMyISAMTable expects a PRIMARY KEY (if any) as Indexes[0], same
	// as an SDI lists it; every element's Length is the key part's own
	// byte length, which BuildMyISAMTable compares against the column's
	// own to tell a prefix key from a full-column one - also exactly as
	// an SDI records it.
	pkIdx := frmPrimaryKeyIndex(keys)
	order := make([]int, 0, len(keys))
	if pkIdx >= 0 {
		order = append(order, pkIdx)
	}
	for i := range keys {
		if i != pkIdx {
			order = append(order, i)
		}
	}
	for _, i := range order {
		k := keys[i]
		idx := ddIndexJSON{Name: k.name, IsVisible: true}
		switch {
		case i == pkIdx:
			idx.Type = ddIndexPrimary
		case k.fulltext:
			idx.Type = ddIndexFulltext
		case k.spatial:
			idx.Type = ddIndexSpatial
		case k.unique:
			idx.Type = ddIndexUnique
		default:
			idx.Type = ddIndexMultiple
		}
		for _, p := range k.parts {
			if p.fieldnr == 0 || int(p.fieldnr) > len(rawTable.Columns) {
				return nil, fmt.Errorf("index %q references an out-of-range column", k.name)
			}
			el := ddIndexElementJSON{ColumnOpx: p.fieldnr - 1, Length: p.length}
			if k.fulltext || k.spatial {
				el.Length = 0 // never a prefix - see BuildMyISAMTable
			}
			idx.Elements = append(idx.Elements, el)
		}
		rawTable.Indexes = append(rawTable.Indexes, idx)
	}
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

// frm_default.go: a .frm column's DEFAULT / ON UPDATE, for GenerateDDL.
//
// Unlike the SDI, which keeps each column's default as text, a .frm keeps
// a whole row of them - the "default values record", in the server's own
// record format (the one MyISAM's FIXED rows also use, so myisam.go's
// decodeMyISAMField reads most of its fields) - plus three bits of
// per-field metadata: pack_flag's FIELDFLAG_NO_DEFAULT (a NOT NULL column
// declared without a DEFAULT, whose record bytes are just zeros),
// unireg_type's TIMESTAMP_DN/UN/DNUN (DEFAULT and/or ON UPDATE
// CURRENT_TIMESTAMP, which no stored value can express), and the record's
// own null bitmap (DEFAULT NULL). frmApplyDefault turns all of that into
// the same ddColumnJSON fields the SDI fills in, so columnDefaultClause
// (sqlout.go) renders both paths' defaults identically.
//
// Expression defaults don't exist before MySQL 8.0.13, so a MySQL .frm
// never has one, but MariaDB 10.2+ has them (and stores a BLOB/TEXT/JSON
// column's literal default the same way): its .frm version 11 adds an
// expressions section after the field comments - see
// frmDefaultExpressions.
package main

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// frmRecordOffset is where a .frm's default-values record starts - right
// after the key definitions (open_binary_frm's record_offset).
func frmRecordOffset(head []byte) uint64 {
	off := uint64(frmU16(head[6:8]))
	if frmU16(head[14:16]) == 0xffff {
		off += uint64(frmU32(head[47:51]))
	} else {
		off += uint64(frmU16(head[14:16]))
	}
	return off
}

// frmDefaultRecord returns a .frm's default-values record (reclength
// bytes), or nil if the file is too short to hold it.
func frmDefaultRecord(data []byte) []byte {
	start := frmRecordOffset(data[:frmHeaderSize])
	end := start + uint64(frmU16(data[16:18]))
	if end > uint64(len(data)) {
		return nil
	}
	return data[start:end]
}

// frmVerExpressions is the .frm version byte (FRM_VER_EXPRESSSIONS in
// MariaDB's table.h) that MariaDB 10.2+ writes, adding the expressions
// section; MySQL's own .frm files stop at frmVerTrueVarchar.
const frmVerExpressions = frmVerTrueVarchar + 1

// frmDefaultExpressions reads a MariaDB .frm's expressions section - a
// 16-byte header, then one entry per expression: a type byte, the field
// number (2 bytes, 0-based), the expression's length (2 bytes), a name's
// length (1 byte), the name, and the expression text (parse_vcol_defs in
// MariaDB's table.cc) - and returns the DEFAULT expressions (type 2,
// VCOL_DEFAULT) by field number. Generated columns and CHECK constraints
// share the section but aren't defaults.
func frmDefaultExpressions(sec []byte) map[uint32]string {
	const baseSize, headerSize, vcolDefault = 16, 6, 2
	out := map[uint32]string{}
	for pos := baseSize; pos+headerSize <= len(sec); {
		typ := sec[pos]
		fieldNr := frmU16(sec[pos+1 : pos+3])
		exprLen := int(frmU16(sec[pos+3 : pos+5]))
		nameLen := int(sec[pos+5])
		exprStart := pos + headerSize + nameLen
		if exprStart+exprLen > len(sec) {
			break
		}
		if typ == vcolDefault {
			out[fieldNr] = string(sec[exprStart : exprStart+exprLen])
		}
		pos = exprStart + exprLen
	}
	return out
}

// frmApplyDefault fills c's default fields from field f's slice of the
// default-values record rec, or from expr, its MariaDB DEFAULT expression
// if it has one. nullBit is the field's bit in the record's
// null bitmap (-1 if it's NOT NULL); bitHiPos/bitHiLen locate a BIT
// column's leftover high bits, which live in that same bitmap. A default
// this can't decode (an unreadable record, the pre-5.0 DECIMAL format) is
// left out of the DDL, as every .frm default was before this existed.
func frmApplyDefault(c *ddColumnJSON, f frmField, rec []byte, expr string, nullBit, bitHiPos, bitHiLen int) {
	c.DefaultOption = expr
	switch f.unireg {
	case fieldTimestampDN, fieldTimestampDNUN, fieldTimestampOld:
		c.DefaultOption = frmCurrentTimestamp(c)
	}
	switch f.unireg {
	case fieldTimestampUN, fieldTimestampDNUN, fieldTimestampOld:
		c.UpdateOption = frmCurrentTimestamp(c)
	}
	if c.DefaultOption != "" {
		c.DefaultValueUtf8 = c.DefaultOption
		return
	}
	if !c.IsNullable && f.packFlag&fieldNoDefault != 0 {
		c.HasNoDefault, c.DefaultValueUtf8Null = true, true
		return
	}
	if nullBit >= 0 && frmRecordBit(rec, nullBit) {
		c.DefaultValueUtf8Null = true
		return
	}
	switch c.Type {
	case ddTinyBlob, ddMediumBlob, ddBlob, ddLongBlob, ddJSON, ddGeometry:
		// No literal DEFAULT is possible; the record holds only a
		// length+pointer placeholder.
		c.HasNoDefault, c.DefaultValueUtf8Null = !c.IsNullable, true
		return
	}
	if err := frmDecodeDefault(c, f, rec, bitHiPos, bitHiLen); err != nil {
		c.HasNoDefault = true
	}
}

// frmCurrentTimestamp spells CURRENT_TIMESTAMP with c's own fractional-
// seconds precision, as SHOW CREATE TABLE does.
func frmCurrentTimestamp(c *ddColumnJSON) string {
	if c.DatetimePrecision > 0 {
		return fmt.Sprintf("CURRENT_TIMESTAMP(%d)", c.DatetimePrecision)
	}
	return "CURRENT_TIMESTAMP"
}

// frmRecordBit reports bit n of rec, counting from bit 0 of byte 0 - the
// null bitmap's own order (get_rec_bits reads it little-endian).
func frmRecordBit(rec []byte, n int) bool {
	return n/8 < len(rec) && rec[n/8]&(1<<uint(n%8)) != 0
}

// frmNotFixedDec is NOT_FIXED_DEC: a FLOAT/DOUBLE's "decimals" when it was
// declared without a scale.
const frmNotFixedDec = 31

// frmDecodeDefault decodes field f's non-NULL default value out of rec into
// c.DefaultValueUtf8 - or, for a binary-charset string, c.DefaultValue (its
// raw record image, as the SDI keeps it).
func frmDecodeDefault(c *ddColumnJSON, f frmField, rec []byte, bitHiPos, bitHiLen int) error {
	pl, err := packLength(c)
	if err != nil {
		return err
	}
	if c.Type == ddBit && f.packFlag&fieldBitAsChar == 0 {
		pl = f.length / 8 // the rest is bitHiLen bits in the null bitmap
	}
	off := uint64(f.recpos) - 1
	if f.recpos == 0 || off+uint64(pl) > uint64(len(rec)) {
		return fmt.Errorf("default value lies outside the default-values record")
	}
	raw := rec[off : off+uint64(pl)]

	switch c.Type {
	case ddBit:
		var v uint64
		for i := 0; i < bitHiLen; i++ {
			if frmRecordBit(rec, bitHiPos+i) {
				v |= 1 << uint(i)
			}
		}
		for _, b := range raw {
			v = v<<8 | uint64(b)
		}
		c.DefaultValueUtf8 = "b'" + strconv.FormatUint(v, 2) + "'"
		return nil
	case ddEnum:
		idx := decodeUnsignedLE(raw)
		if idx == 0 {
			c.DefaultValueUtf8 = ""
			return nil
		}
		labels := decodeElements(c.Elements)
		if int(idx) > len(labels) {
			return fmt.Errorf("enum default index %d out of range", idx)
		}
		c.DefaultValueUtf8 = labels[idx-1]
		return nil
	case ddSet:
		bits := decodeUnsignedLE(raw)
		labels := decodeElements(c.Elements)
		var parts []string
		for i, l := range labels {
			if bits&(1<<uint(i)) != 0 {
				parts = append(parts, l)
			}
		}
		c.DefaultValueUtf8 = strings.Join(parts, ",")
		return nil
	case ddString, ddVarString, ddVarchar:
		if c.CollationID == 63 {
			c.DefaultValue = base64.StdEncoding.EncodeToString(raw)
			return nil
		}
		v := raw
		if c.Type == ddVarchar {
			lenBytes := pl - c.CharLength
			n := uint32(raw[0])
			if lenBytes == 2 {
				n |= uint32(raw[1]) << 8
			}
			if lenBytes+n > pl {
				return fmt.Errorf("varchar default length %d exceeds the column", n)
			}
			v = raw[lenBytes : lenBytes+n]
		} else if !isWideCharset(c.CollationID) {
			v = []byte(strings.TrimRight(string(v), " "))
		}
		if s, ok := frmStringToUTF8(c.CollationID, v); ok {
			c.DefaultValueUtf8 = s
		} else {
			c.DefaultValueUtf8 = "0x" + strings.ToUpper(fmt.Sprintf("%x", v))
			c.DefaultIsLiteral = true
		}
		return nil
	case ddFloat, ddDouble:
		var v float64
		if c.Type == ddFloat {
			v = float64(math.Float32frombits(binary.LittleEndian.Uint32(raw)))
		} else {
			v = math.Float64frombits(binary.LittleEndian.Uint64(raw))
		}
		var s string
		switch {
		case c.NumericScale < frmNotFixedDec:
			s = strconv.FormatFloat(v, 'f', int(c.NumericScale), 64)
		case c.Type == ddFloat:
			s = formatFloat32(float32(v))
		default:
			s = strconv.FormatFloat(v, 'g', -1, 64)
		}
		c.DefaultValueUtf8 = frmZerofill(c, f, s)
		return nil
	}

	// Integers, DECIMAL, YEAR and every temporal type: the same
	// record-format decoding a MyISAM row uses. TSV format renders a
	// temporal value without the quotes an SQL literal would add.
	col, err := buildColumn(*c)
	if err != nil {
		return err
	}
	s, err := decodeMyISAMField(col, raw, formatTSV)
	if err != nil {
		return err
	}
	c.DefaultValueUtf8 = frmZerofill(c, f, s)
	return nil
}

// frmZerofill pads a ZEROFILL numeric default to the column's display
// width, as SHOW CREATE TABLE prints it (Field_num::prepend_zeros).
func frmZerofill(c *ddColumnJSON, f frmField, s string) string {
	if !c.IsZerofill || uint32(len(s)) >= f.length {
		return s
	}
	return strings.Repeat("0", int(f.length)-len(s)) + s
}

// frmStringToUTF8 converts a string default from its column's charset to
// UTF-8 for the DDL file: as-is for the UTF-8 family (and for plain ASCII
// in any other byte-oriented charset), through MySQL's own latin1 mapping
// (cp1252's, with its five undefined bytes as the matching C1 controls)
// for latin1. ok is false for anything else, which the caller instead
// writes as a 0x... hex literal - MySQL reads that as raw bytes in the
// column's own charset, so it's exact whatever the charset.
func frmStringToUTF8(collationID uint64, b []byte) (string, bool) {
	charset, _, _, _ := resolveCollation(collationID)
	switch charset {
	case "utf8", "utf8mb3", "utf8mb4":
		return string(b), utf8.Valid(b)
	case "latin1":
		var sb strings.Builder
		for _, x := range b {
			if x >= 0x80 && x < 0xa0 {
				sb.WriteRune(cp1252High[x-0x80])
			} else {
				sb.WriteRune(rune(x))
			}
		}
		return sb.String(), true
	}
	if isWideCharset(collationID) {
		return "", false
	}
	for _, x := range b {
		if x >= 0x80 {
			return "", false
		}
	}
	return string(b), true
}

// cp1252High maps latin1 bytes 0x80-0x9F to Unicode, as MySQL's latin1
// (really cp1252) does - see ctype-latin1.cc's cs_to_uni.
var cp1252High = [32]rune{
	0x20AC, 0x0081, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0x008D, 0x017D, 0x008F,
	0x0090, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0x009D, 0x017E, 0x0178,
}

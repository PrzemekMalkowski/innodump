// redundant.go: decodes REDUNDANT ("old-style") records - InnoDB's original,
// pre-5.0.3 physical row format. Still creatable today via
// ROW_FORMAT=REDUNDANT, and was the default before MySQL 5.5, so real 5.1/5.5
// /5.6/5.7 data (or anything explicitly created REDUNDANT since) needs it.
// Cross-checked against rem0rec.cc/rem0rec.ic in the mysql-server source and
// validated empirically against a real MySQL 8.0.46 ROW_FORMAT=REDUNDANT
// table (which still writes exactly this format).
//
// Where COMPACT/DYNAMIC use a NULL bitmap plus a variable-length list,
// REDUNDANT stores one CUMULATIVE END OFFSET per field (from the record
// origin), immediately before the header, ordered so field 0's entry sits
// closest to the header (mirrors rec_1_get_prev_field_end_info /
// rec_2_get_prev_field_end_info in rem0rec.ic: calling either with field
// index n=i+1 gives field i's end offset). Each entry is 1 byte (whole
// record's data fits in 127 bytes and it has no external column) or 2 bytes,
// decided once per record when it was written (the REC_OLD_SHORT header
// bit), never per field:
//
//	1 byte : bit 7  = SQL NULL,                    bits 0-6  = end offset
//	2 bytes: bit 15 = SQL NULL, bit 14 = external,  bits 0-13 = end offset
//
// A NULL *fixed*-length column still physically occupies its full width
// (dummy filler bytes InnoDB never reads back - dtype_get_sql_null_size in
// data0type.ic); the stored end offset already reflects this, so field i's
// START is simply field (i-1)'s stored end offset (0 for i=0) regardless of
// NULL-ness - only a NULL *variable*-length column leaves the offset
// unchanged from the previous field. Either way, this tool never needs to
// know which case applies: the stored offsets already encode it, so - unlike
// COMPACT - decoding a field here needs no fixed-vs-variable distinction at
// all, and no separate NULL-bitmap pass.
//
// REDUNDANT does not support INSTANT ADD/DROP COLUMN in the versions this
// tool targets in any form BuildTable lets through (see its comment), so
// there is no old-format equivalent of instant.go here.
package main

import "encoding/binary"

// recOldIs1ByteOffs reports whether recOff's field-offset array uses 1-byte
// (true) or 2-byte (false) entries - the REC_OLD_SHORT header bit.
func recOldIs1ByteOffs(page []byte, recOff uint32) bool {
	return page[recOff-3]&0x01 != 0
}

// decodeFieldRangesOld decodes a REDUNDANT record's fields, given the
// physical field list (t.PhysicalFields for a leaf record, or a PK/DB_ROW_ID
// prefix plus a synthetic child-pointer field for a non-leaf one - see
// btree.go's nonLeafChildPage).
func decodeFieldRangesOld(page []byte, recOff uint32, fields []*IndexField) []fieldRange {
	is1Byte := recOldIs1ByteOffs(page, recOff)
	out := make([]fieldRange, len(fields))
	var prevEnd uint32 // offset relative to the record origin
	for i := range fields {
		n := uint32(i + 1)
		var raw uint32
		var isNull, isExternal bool
		if is1Byte {
			b := page[recOff-uint32(recNOldExtraBytes)-n]
			isNull = b&0x80 != 0
			raw = uint32(b & 0x7f)
		} else {
			pos := recOff - uint32(recNOldExtraBytes) - 2*n
			v := binary.BigEndian.Uint16(page[pos : pos+2])
			isNull = v&0x8000 != 0
			isExternal = v&0x4000 != 0
			raw = uint32(v & 0x3fff)
		}
		start := recOff + prevEnd
		if isNull {
			out[i] = fieldRange{Null: true, Start: start, End: start}
		} else {
			out[i] = fieldRange{Start: start, End: recOff + raw, External: isExternal}
		}
		prevEnd = raw
	}
	return out
}

// record.go: decodes one clustered-index leaf record (COMPACT/DYNAMIC,
// "no instant/no row-version" layout - see schema.go) into a byte range per
// physical field, following the exact algorithm InnoDB itself uses to
// interpret a record's NULL bitmap and variable-length list
// (Record::InitColumnOffsetsCompactLeaf in ibdNinja's Record.cc, itself a
// port of rem0rec.cc's rec_init_offsets_comp_ordinary).
//
// Record layout, walking backward from the record's origin (recOff):
//
//	[... variable-length list ...][NULL bitmap][5-byte header] REC_ORIGIN [field 0][field 1]...
//
// The variable-length list has one entry per non-NULL, non-fixed-length
// physical field (in physical field order), each 1 byte, or 2 bytes for a
// "big" column (over 255 bytes, or BLOB-family) whose length has the high
// bit set - see decodeLenByte.
package main

import (
	"encoding/binary"
	"fmt"
)

// fieldRange describes where one physical field's bytes sit within a
// record, or that it is NULL/absent/not-applicable-to-this-row (Default,
// Dropped: see instant.go).
type fieldRange struct {
	Start, End uint32 // byte offsets within the page, valid only if !Null
	Null       bool
	External   bool // the last 20 bytes of [Start,End) are a BTR_EXTERN reference
	Default    bool // column didn't exist yet when this row was written; use its instant default
	Dropped    bool // column was already instant-dropped by the time this row was written
}

// decodeRecordFields returns one fieldRange per entry in t.PhysicalFields,
// for the ordinary leaf record at recOff on page. REDUNDANT records (see
// redundant.go) have no separate "status" field or instant/version history
// to dispatch on - the field-offset array alone determines everything, so
// they're handled first and entirely separately. Everything else dispatches
// on the record's own info-bit flags and the table's INSTANT ADD/DROP COLUMN
// state - see instant.go's package comment for the four resulting cases
// (this mirrors Record::GetInsertState in ibdNinja).
func decodeRecordFields(page []byte, recOff uint32, t *Table) ([]fieldRange, error) {
	if !pageIsCompact(page) {
		return decodeFieldRangesOld(page, recOff, t.PhysicalFields), nil
	}
	if recStatus(page, recOff) != recStatusOrdinary {
		return nil, fmt.Errorf("not an ordinary record (status=%d)", recStatus(page, recOff))
	}
	info := recInfoBits(page, recOff)
	isVersioned := info&recInfoVersionFlag != 0
	isInstant := info&recInfoInstantFlag != 0

	switch {
	case isVersioned:
		if !t.HasRowVersions && !t.HasOldInstantCols {
			return nil, fmt.Errorf("record has the row-version header flag set, but the table's SDI shows no instant/row-version history (corrupt?)")
		}
		return decodeFieldRangesVersioned(page, recOff, t, page[recOff-6], 1)
	case isInstant:
		if !t.HasOldInstantCols {
			return nil, fmt.Errorf("record has the legacy instant-add header flag set, but the table's SDI shows no such history (corrupt?)")
		}
		n, length, err := readOldInstantFieldCount(page, recOff)
		if err != nil {
			return nil, err
		}
		return decodeFieldRangesOldInstant(page, recOff, t, n, uint32(length))
	case t.HasRowVersions:
		// Predates row versioning entirely (no version byte was ever written
		// for it) - treat as if written at version 0.
		return decodeFieldRangesVersioned(page, recOff, t, 0, 0)
	case t.HasOldInstantCols:
		// Predates the table's first-ever instant add.
		return decodeFieldRangesOldInstant(page, recOff, t, t.OldNonDefaultFields, 0)
	default:
		return decodeFieldRanges(page, recOff, t.PhysicalFields, t.NNullable)
	}
}

// decodeFieldRanges is the shared implementation of InnoDB's
// rec_init_offsets_comp_ordinary for a plain (non-instant, non-versioned)
// record: it walks the NULL bitmap and variable-length list once, in
// physical field order, and returns each field's byte range. Used both for
// leaf (row) records and, with a restricted field list, for non-leaf
// (node-pointer) records - see btree.go's nonLeafChildPage - and as the
// inner loop of the INSTANT ADD/DROP-aware decoders in instant.go.
func decodeFieldRanges(page []byte, recOff uint32, fields []*IndexField, nNullable int) ([]fieldRange, error) {
	return decodeFieldRangesSpecial(page, recOff, 0, fields, nNullable, nil)
}

// decodeFieldRangesSpecial is decodeFieldRanges with a per-field hook
// consulted BEFORE the null-bit/length-list decode, for INSTANT ADD/DROP
// COLUMN handling: InnoDB's rec_init_offsets_comp_ordinary resolves a
// dropped-or-not-yet-added field before ever touching the NULL bitmap or
// variable-length list for it, since such a field consumes zero bytes and no
// NULL bit at all (special returning ok=true skips both entirely).
//
// headerExtra is the count of extra bytes an instant/versioned record
// carries between the 5-byte header and the NULL bitmap (the row version
// byte, or the old mechanism's 1-2 byte field count) - the NULL bitmap and
// variable-length list both start that much further back than usual.
func decodeFieldRangesSpecial(page []byte, recOff uint32, headerExtra uint32, fields []*IndexField, nNullable int,
	special func(i int, col *Column) (fieldRange, bool)) ([]fieldRange, error) {
	nullBytes := (nNullable + 7) / 8
	nullsEnd := recOff - 5 - headerExtra // one past the last (rightmost) null-bitmap byte
	lensEnd := nullsEnd - uint32(nullBytes)

	nullPos := 0 // index of the next nullable column to consult
	null := func() bool {
		// nulls are read MSB-first within each byte, walking the bitmap
		// left-to-right the same way InnoDB's null_mask/nulls-- loop does:
		// bit 0 of the byte closest to nullsEnd is the FIRST nullable
		// column, matching InitColumnOffsetsCompactLeaf's null_mask<<=1.
		byteIdx := nullPos / 8
		bit := uint(nullPos % 8)
		nullPos++
		b := page[nullsEnd-1-uint32(byteIdx)]
		return b&(1<<bit) != 0
	}

	lensPos := lensEnd // walks DOWN (toward lower offsets) as bytes are consumed
	out := make([]fieldRange, len(fields))
	offs := recOff // fieldRange.Start/End are absolute page offsets
	if debugFields {
		fmt.Printf("decodeFieldRanges: recOff=0x%x nullBytes=%d nullsEnd=0x%x lensEnd=0x%x\n", recOff, nullBytes, nullsEnd, lensEnd)
	}
	for i, f := range fields {
		col := f.Col
		if special != nil {
			if fr, ok := special(i, col); ok {
				fr.Start, fr.End = offs, offs
				out[i] = fr
				continue
			}
		}
		if col.IsNullable {
			if null() {
				out[i] = fieldRange{Null: true, Start: offs, End: offs}
				continue
			}
		}
		if f.EffFixedLen != 0 {
			start := offs
			offs += f.EffFixedLen
			out[i] = fieldRange{Start: start, End: offs}
			continue
		}
		// Variable-length field: consume 1 or 2 bytes from the length list.
		lensPos--
		b1 := page[lensPos]
		start := offs
		if col.isBigCol() && b1&0x80 != 0 {
			lensPos--
			b2 := page[lensPos]
			v := uint32(b1)<<8 | uint32(b2)
			offs += v & 0x3fff
			out[i] = fieldRange{Start: start, End: offs, External: v&0x4000 != 0}
			continue
		}
		offs += uint32(b1)
		out[i] = fieldRange{Start: start, End: offs}
	}
	if debugFields {
		for i, r := range out {
			fmt.Printf("  field %d %q: [0x%x,0x%x) null=%v ext=%v default=%v dropped=%v\n",
				i, fields[i].Col.Name, r.Start, r.End, r.Null, r.External, r.Default, r.Dropped)
		}
	}
	return out, nil
}

var debugFields = false

// externalRef is a 20-byte BTR_EXTERN_FIELD_REF: a pointer to an
// off-page (LOB) column value.
type externalRef struct {
	SpaceID uint32
	PageNo  uint32
	Version uint32 // BTR_EXTERN_VERSION: repurposed as a LOB version in 8.0+
	Length  uint64 // total length across all off-page storage (36 bits)
}

const btrExternFieldRefSize = 20

func readExternalRef(page []byte, end uint32) (externalRef, error) {
	if end < btrExternFieldRefSize {
		return externalRef{}, fmt.Errorf("external reference runs before the start of the page")
	}
	ref := page[end-btrExternFieldRefSize : end]
	return externalRef{
		SpaceID: binary.BigEndian.Uint32(ref[0:4]),
		PageNo:  binary.BigEndian.Uint32(ref[4:8]),
		Version: binary.BigEndian.Uint32(ref[8:12]),
		Length:  binary.BigEndian.Uint64(ref[12:20]) & 0x1FFFFFFFFF,
	}, nil
}

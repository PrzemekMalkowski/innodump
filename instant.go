// instant.go: decodes records written under INSTANT ADD/DROP COLUMN
// history, cross-checked against ibdNinja's Record.cc/Index.cc/Table.cc and
// validated empirically against real MySQL 8.0.19 (the old mechanism) and
// 8.0.46/8.4.11 (row versioning) servers - see the two mechanisms below.
//
// MySQL has shipped two different on-disk mechanisms for this:
//
//  1. The "old" mechanism (MySQL 8.0.12-8.0.28): INSTANT ADD COLUMN only, no
//     DROP. Every record inserted after the table's first-ever instant add
//     carries the header's REC_INFO_INSTANT_FLAG (0x80) plus a 1-2 byte
//     "how many fields does THIS record have" count immediately before the
//     NULL bitmap (readOldInstantFieldCount); fields at or past that count
//     didn't exist yet when the record was written, and use the column's
//     recorded instant default. A record with neither flag predates the
//     table's first instant add ever, so it uses a fixed field count derived
//     from the table-level "instant_col" SDI marker (the number of user
//     columns that existed at that point) instead of a per-record byte.
//
//  2. Row versioning (MySQL 8.0.29+), which superseded the above and adds
//     INSTANT DROP COLUMN too: every column gets a permanent "physical_pos"
//     slot (so a later drop never disturbs other columns' offsets) and,
//     where relevant, a "version_added"/"version_dropped" pair. A record
//     carries the header's REC_INFO_VERSION_FLAG (0x40) plus a 1-byte row
//     version immediately before the NULL bitmap; a column is absent from a
//     given record if it was dropped at or before that version, or hasn't
//     been added yet as of that version - either way contributing zero bytes
//     and no NULL bit. A record with neither flag predates row versioning
//     entirely and is treated as version 0.
//
// A table uses at most one of the two (a table already using the old
// mechanism switches to row versioning - and stops writing old-style
// records - the moment it receives its first INSTANT DROP COLUMN, or any
// instant operation at all once the server itself has moved on to
// 8.0.29+/8.4.x, which is by far the common case in practice).
package main

import "fmt"

// finishInstantSetup computes the table-level bookkeeping BuildTable needs
// to decode INSTANT ADD/DROP COLUMN records: the per-row-version nullable
// -column counts for row-versioned tables (mirrors Index::FillSeIndex's
// ib_nullables_ array), and the fixed pre-first-instant-add field count for
// old-mechanism tables (mirrors Index::GetNOriginalFields).
func finishInstantSetup(t *Table, raw *ddTableJSON, colsByOpx []*Column) {
	if t.HasRowVersions {
		var maxVersion uint8
		for _, c := range colsByOpx {
			if c.HasVersionAdded && c.VersionAdded > maxVersion {
				maxVersion = c.VersionAdded
			}
			if c.HasVersionDropped && c.VersionDropped > maxVersion {
				maxVersion = c.VersionDropped
			}
		}
		t.CurrentRowVersion = maxVersion
		nullable := make([]int, int(maxVersion)+1)
		for _, f := range t.PhysicalFields {
			c := f.Col
			if c.IsSystem || !c.IsNullable {
				continue
			}
			start := 0
			if c.IsInstantAdded() {
				start = int(c.VersionAdded)
			}
			for v := start; v <= int(maxVersion); v++ {
				nullable[v]++
			}
			if c.IsInstantDropped() {
				for v := int(c.VersionDropped); v <= int(maxVersion); v++ {
					nullable[v]--
				}
			}
		}
		t.NullableInVersion = nullable
	}

	if p := sePropString(raw.SePrivateData); p["instant_col"] != "" {
		t.HasOldInstantCols = true
		originalUserCols := int(parseUintDefault(p["instant_col"], 0))
		n := originalUserCols + 2 // + DB_TRX_ID + DB_ROLL_PTR
		if !t.HasExplicitPK {
			n++ // + DB_ROW_ID
		}
		t.OldNonDefaultFields = n
	}
}

// readOldInstantFieldCount mirrors Record::GetNFieldsInstant: the 1-2 byte
// field count an old-mechanism instant record carries immediately before its
// NULL bitmap (high bit of the first byte set = 2-byte encoding), and how
// many bytes that took (callers must shift the NULL bitmap start back by
// exactly this much - see decodeFieldRangesSpecial's headerExtra).
func readOldInstantFieldCount(page []byte, recOff uint32) (n int, length int, err error) {
	if recOff < 7 {
		return 0, 0, fmt.Errorf("record is too close to the start of the page for an instant field count")
	}
	b1 := page[recOff-6]
	if b1&0x80 == 0 {
		return int(b1), 1, nil
	}
	b2 := page[recOff-7]
	n = (int(b1&0x7f) << 8) | int(b2)
	if n <= 0 || n >= 1024 {
		return 0, 0, fmt.Errorf("instant field count %d is implausible, the record is likely corrupt", n)
	}
	return n, 2, nil
}

// decodeFieldRangesOldInstant decodes a record written under the old
// (8.0.12-8.0.28) instant-add mechanism: fields at or past nonDefaultFields
// didn't exist yet when this record was written. headerExtra is 0 for a
// pristine pre-first-instant-add record (no per-record byte at all) or the
// length readOldInstantFieldCount returned (1 or 2) otherwise.
func decodeFieldRangesOldInstant(page []byte, recOff uint32, t *Table, nonDefaultFields int, headerExtra uint32) ([]fieldRange, error) {
	nNull := 0
	for i, f := range t.PhysicalFields {
		if i >= nonDefaultFields {
			break
		}
		if f.Col.IsNullable {
			nNull++
		}
	}
	return decodeFieldRangesSpecial(page, recOff, headerExtra, t.PhysicalFields, nNull, func(i int, col *Column) (fieldRange, bool) {
		if i >= nonDefaultFields {
			return fieldRange{Default: true}, true
		}
		return fieldRange{}, false
	})
}

// decodeFieldRangesVersioned decodes a record written under row versioning
// (8.0.29+): each column is compared against the record's own row version to
// decide whether it was already dropped, not yet added, or genuinely present.
// headerExtra is 0 for a pristine pre-row-versioning record (rowVersion is
// then always 0, and no per-record byte was written at all) or 1 when the
// record actually carries its own row-version byte.
func decodeFieldRangesVersioned(page []byte, recOff uint32, t *Table, rowVersion uint8, headerExtra uint32) ([]fieldRange, error) {
	if int(rowVersion) >= len(t.NullableInVersion) {
		return nil, fmt.Errorf("record's row version %d exceeds the table's current row version %d (corrupt?)", rowVersion, t.CurrentRowVersion)
	}
	nNull := t.NullableInVersion[rowVersion]
	return decodeFieldRangesSpecial(page, recOff, headerExtra, t.PhysicalFields, nNull, func(i int, col *Column) (fieldRange, bool) {
		if col.IsDroppedInOrBefore(rowVersion) {
			return fieldRange{Dropped: true}, true
		}
		if col.IsAddedAfter(rowVersion) {
			return fieldRange{Default: true}, true
		}
		return fieldRange{}, false
	})
}

// decodeInstantDefault renders an instantly-added column's recorded default
// value (from the SDI, not the record) as a SQL literal, for a field whose
// fieldRange came back Default. It reuses decodeField's full type-decode
// logic by treating the default bytes as if they were a normal inline field.
func decodeInstantDefault(col *Column, format outputFormat) (string, error) {
	if col.InstantDefaultIsNull || col.InstantDefault == nil {
		return nullLiteral(format), nil
	}
	fr := fieldRange{Start: 0, End: uint32(len(col.InstantDefault))}
	return decodeField(nil, col, col.InstantDefault, fr, format)
}

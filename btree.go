// btree.go: walks a clustered index from its root page down to the
// leftmost leaf, then across the whole leaf level via FIL_PAGE_NEXT,
// decoding every non-delete-marked ordinary record into a row.
package main

import (
	"encoding/binary"
	"fmt"
)

// leftmostLeaf descends from root to the leftmost page at level 0, reading
// the child page number out of the first (leftmost) non-leaf record on each
// level via nonLeafChildPage. Unlike the leaf-level walk in WalkRows, a
// corrupt page here is always fatal (regardless of --skip-corrupted): there
// is no well-defined "next page" to fall back to while still finding our way
// down to the correct leftmost leaf.
func leftmostLeaf(sp *Space, t *Table) (uint32, error) {
	page, err := sp.ReadPage(t.RootPage)
	if err != nil {
		return 0, err
	}
	cur := t.RootPage
	for pageGetLevel(page) != 0 {
		if pc := checkPage(page); !pc.OK {
			return 0, fmt.Errorf("page %d (index %q, id %d): %s", cur, t.IndexName, t.IndexID, pc.Reason)
		}
		compact := pageIsCompact(page)
		var recOff uint32
		if compact {
			if recStatus(page, pageNewInfimum) != recStatusInfimum {
				return 0, fmt.Errorf("corrupt index page %d: expected infimum", cur)
			}
			recOff = recNextOffset(page, pageNewInfimum, sp.PageSize)
		} else {
			recOff = recOldNextOffset(page, pageOldInfimum)
		}
		if recOff == 0 {
			return 0, fmt.Errorf("corrupt index page %d: empty non-leaf page", cur)
		}
		child, err := nonLeafChildPage(page, recOff, t, compact)
		if err != nil {
			return 0, fmt.Errorf("page %d: %w", cur, err)
		}
		level := pageGetLevel(page)
		page, err = sp.ReadPage(child)
		if err != nil {
			return 0, err
		}
		if pageGetLevel(page) != level-1 {
			return 0, fmt.Errorf("index is not well-formed at page %d", child)
		}
		cur = child
	}
	return cur, nil
}

// nonLeafChildPage decodes a node-pointer record's key prefix (the PRIMARY
// KEY columns, or DB_ROW_ID if the table has no explicit PK) followed by
// the 4-byte child page number, and returns that child page number. Neither
// row versioning nor a REC_OLD_SHORT-style per-record format choice ever
// applies to a node-pointer record's key columns (only leaf rows can have
// evolved schemas), so this needs none of decodeRecordFields' dispatch -
// just the right one of the two plain field-range decoders for the page's
// own COMPACT flag.
func nonLeafChildPage(page []byte, recOff uint32, t *Table, compact bool) (uint32, error) {
	nUniq := len(t.PKFields)
	if !t.HasExplicitPK {
		nUniq = 1
	}
	if nUniq > len(t.PhysicalFields) {
		return 0, fmt.Errorf("index metadata is inconsistent (n_uniq=%d > %d physical fields)", nUniq, len(t.PhysicalFields))
	}
	keyFields := t.PhysicalFields[:nUniq]
	childField := &IndexField{Col: &Column{FixedLen: 4}, EffFixedLen: 4}
	fields := append(append([]*IndexField(nil), keyFields...), childField)

	var ranges []fieldRange
	if compact {
		nNull := 0
		for _, f := range keyFields {
			if f.Col.IsNullable {
				nNull++
			}
		}
		var err error
		ranges, err = decodeFieldRanges(page, recOff, fields, nNull)
		if err != nil {
			return 0, err
		}
	} else {
		ranges = decodeFieldRangesOld(page, recOff, fields)
	}
	last := ranges[len(ranges)-1]
	if last.Null || int(last.End) > len(page) {
		return 0, fmt.Errorf("corrupt node-pointer record")
	}
	return binary.BigEndian.Uint32(page[last.Start:last.End]), nil
}

// Row is one decoded record: the SQL literal for every output column, in
// t.OutputColumns() order (see sqlout.go).
type Row struct {
	Values []string // "" only ever appears alongside a decode error
}

// RowOrError pairs a decoded row with the page/record it came from, for
// error reporting when a single row fails to decode.
type RowOrError struct {
	PageNo, RecOff uint32
	Row            *Row
	Err            error
}

// CorruptPage describes one leaf page that failed validation.
type CorruptPage struct {
	PageNo uint32
	Reason string
}

// WalkRows decodes every live (non-delete-marked) row in the clustered
// index and streams it to fn. fn returning false stops the walk early.
//
// Every leaf page is validated (checksum/LSN consistency, page type, index
// id) before its records are read. By default a bad page is a fatal error
// (stop and report exactly where and why - the caller can hand that
// straight to the user rather than continuing past silently-wrong data).
// With skipCorrupted, onCorrupt is called instead for each bad page and the
// walk tries to press on: it still trusts that page's own FIL_PAGE_NEXT
// pointer (corruption is often localized to the page body, leaving the
// header intact) to reach the next page, stopping only if that pointer is
// itself missing or unusable. onCorrupt may be nil.
func WalkRows(sp *Space, t *Table, outCols []*Column, skipCorrupted bool, onCorrupt func(CorruptPage), fn func(RowOrError) bool) error {
	leaf, err := leftmostLeaf(sp, t)
	if err != nil {
		return fmt.Errorf("finding the leftmost leaf page: %w", err)
	}
	pageNo := leaf
	for pageNo != filNull {
		page, err := sp.ReadPage(pageNo)
		if err != nil {
			return err
		}
		reason := ""
		if pc := checkPage(page); !pc.OK {
			reason = pc.Reason
		} else if filType(page) != filPageIndex {
			reason = fmt.Sprintf("expected an INDEX page, got type %d", filType(page))
		} else if pageGetIndexID(page) != t.IndexID {
			reason = fmt.Sprintf("belongs to index id %d, not the expected %d", pageGetIndexID(page), t.IndexID)
		}
		if reason != "" {
			if !skipCorrupted {
				return fmt.Errorf("page %d (index %q, id %d): %s", pageNo, t.IndexName, t.IndexID, reason)
			}
			if onCorrupt != nil {
				onCorrupt(CorruptPage{PageNo: pageNo, Reason: reason})
			}
			next := filNextPage(page)
			if next == filNull || next == pageNo || next >= sp.NumPages {
				return nil // no usable way to keep going from here
			}
			pageNo = next
			continue
		}
		cont := true
		walkRecords(page, sp.PageSize, func(recOff uint32) bool {
			if recDeleted(page, recOff) {
				return true
			}
			row, err := decodeOneRow(sp, page, recOff, t, outCols)
			if !fn(RowOrError{PageNo: pageNo, RecOff: recOff, Row: row, Err: err}) {
				cont = false
				return false
			}
			return true
		})
		if !cont {
			return nil
		}
		pageNo = filNextPage(page)
	}
	return nil
}

func decodeOneRow(sp *Space, page []byte, recOff uint32, t *Table, outCols []*Column) (row *Row, err error) {
	defer func() {
		if r := recover(); r != nil {
			row, err = nil, fmt.Errorf("panic decoding record: %v", r)
		}
	}()
	ranges, err := decodeRecordFields(page, recOff, t)
	if err != nil {
		return nil, err
	}
	byCol := make(map[*Column]fieldRange, len(t.PhysicalFields))
	for i, f := range t.PhysicalFields {
		byCol[f.Col] = ranges[i]
	}
	values := make([]string, len(outCols))
	for i, col := range outCols {
		fr, ok := byCol[col]
		if !ok {
			return nil, fmt.Errorf("column %q is not in the clustered index's physical layout", col.Name)
		}
		if fr.Dropped {
			// An output column is never one this table's SDI marks
			// IsColumnDropped, so this would mean the physical layout and
			// the per-record instant/version decode disagree - a bug, not
			// user-facing corruption, so it's worth a specific message.
			return nil, fmt.Errorf("column %q resolved to dropped for this row, but is not itself a dropped column", col.Name)
		}
		var v string
		var err error
		switch {
		case fr.Null:
			v = "NULL"
		case fr.Default:
			v, err = decodeInstantDefault(col)
		default:
			v, err = decodeField(sp, col, page, fr)
		}
		if err != nil {
			return nil, err
		}
		values[i] = v
	}
	return &Row{Values: values}, nil
}

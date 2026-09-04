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
// level via nonLeafChildPage.
func leftmostLeaf(sp *Space, t *Table) (uint32, error) {
	page, err := sp.ReadPage(t.RootPage)
	if err != nil {
		return 0, err
	}
	cur := t.RootPage
	for pageGetLevel(page) != 0 {
		if recStatus(page, pageNewInfimum) != recStatusInfimum {
			return 0, fmt.Errorf("corrupt index page %d: expected infimum", cur)
		}
		recOff := recNextOffset(page, pageNewInfimum, sp.PageSize)
		if recOff == 0 {
			return 0, fmt.Errorf("corrupt index page %d: empty non-leaf page", cur)
		}
		child, err := nonLeafChildPage(page, recOff, t)
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
// the 4-byte child page number, and returns that child page number.
func nonLeafChildPage(page []byte, recOff uint32, t *Table) (uint32, error) {
	nUniq := len(t.PKFields)
	if !t.HasExplicitPK {
		nUniq = 1
	}
	if nUniq > len(t.PhysicalFields) {
		return 0, fmt.Errorf("index metadata is inconsistent (n_uniq=%d > %d physical fields)", nUniq, len(t.PhysicalFields))
	}
	keyFields := t.PhysicalFields[:nUniq]
	nNull := 0
	for _, f := range keyFields {
		if f.Col.IsNullable {
			nNull++
		}
	}
	childField := &IndexField{Col: &Column{FixedLen: 4}, EffFixedLen: 4}
	fields := append(append([]*IndexField(nil), keyFields...), childField)
	ranges, err := decodeFieldRanges(page, recOff, fields, nNull)
	if err != nil {
		return 0, err
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

// WalkRows decodes every live (non-delete-marked) row in the clustered
// index and streams it to fn. fn returning false stops the walk early.
func WalkRows(sp *Space, t *Table, outCols []*Column, fn func(RowOrError) bool) error {
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
		if filType(page) != filPageIndex {
			return fmt.Errorf("page %d: expected an INDEX page, got type %d", pageNo, filType(page))
		}
		if pageGetIndexID(page) != t.IndexID {
			return fmt.Errorf("page %d: belongs to index id %d, not the expected %d (leaf chain is corrupt)", pageNo, pageGetIndexID(page), t.IndexID)
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
		if fr.Null {
			values[i] = "NULL"
			continue
		}
		v, err := decodeField(sp, col, page, fr)
		if err != nil {
			return nil, err
		}
		values[i] = v
	}
	return &Row{Values: values}, nil
}

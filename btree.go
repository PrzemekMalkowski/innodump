// btree.go: walks a clustered index from its root page down to the
// leftmost leaf, then across the whole leaf level via FIL_PAGE_NEXT,
// decoding every non-delete-marked ordinary record into a row.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
)

// truncatedReason returns a "this file is truncated" explanation for
// pageNo if the file doesn't actually hold that many pages at all, or ""
// if it does (a page the file genuinely holds failing to read cleanly, or
// failing validation, means something else - a damaged filesystem, or
// real on-disk corruption - and is handled separately by each caller).
func truncatedReason(sp *Space, pageNo uint32) string {
	if pageNo < sp.NumPages {
		return ""
	}
	return fmt.Sprintf("page %d is past the end of the file (it holds only %d page(s)) - "+
		"the file looks truncated (not fully copied), rather than corrupted", pageNo, sp.NumPages)
}

// errTruncated marks an error produced by truncatedReason - the file
// itself doesn't hold the requested page at all, as opposed to a
// checksum/structural failure on a page it does hold. WalkRows uses
// errors.As to tell the two apart: only this one has a well-defined,
// principled fallback (scanTruncatedLeaves) when leftmostLeaf can't even
// navigate down to the correct starting page.
type errTruncated struct{ reason string }

func (e *errTruncated) Error() string { return e.reason }

// readPageOrTruncated is Space.ReadPage with a clearer error for the
// specific, common case of a page number the file doesn't hold at all
// (see truncatedReason) - used where that's always fatal regardless of
// --skip-corrupted (leftmostLeaf, below); collectBatch handles the same
// condition itself, since there it's instead recoverable.
func readPageOrTruncated(sp *Space, pageNo uint32) ([]byte, error) {
	if reason := truncatedReason(sp, pageNo); reason != "" {
		return nil, &errTruncated{reason}
	}
	return sp.ReadPage(pageNo)
}

// leftmostLeaf descends from root to the leftmost page at level 0, reading
// the child page number out of the first (leftmost) non-leaf record on each
// level via nonLeafChildPage. Unlike the leaf-level walk in WalkRows, a
// corrupt page here is always fatal (regardless of --skip-corrupted): there
// is no well-defined "next page" to fall back to while still finding our way
// down to the correct leftmost leaf.
func leftmostLeaf(sp *Space, t *Table) (uint32, error) {
	shape := tableZipShape(t)
	raw, err := readPageOrTruncated(sp, t.RootPage)
	if err != nil {
		return 0, err
	}
	cur := t.RootPage
	for pageGetLevel(raw) != 0 {
		if pc := checkPage(raw, sp.Compressed, sp.Flags.fullCRC32); !pc.OK {
			return 0, fmt.Errorf("page %d (index %q, id %d): %s", cur, t.IndexName, t.IndexID, pc.Reason)
		}
		// Decompression (a no-op on an uncompressed tablespace) only ever
		// runs after checkPage above has passed - see Decompress's comment.
		page, err := sp.Decompress(raw, shape)
		if err != nil {
			return 0, fmt.Errorf("page %d (index %q, id %d): %s", cur, t.IndexName, t.IndexID, err)
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
		raw, err = readPageOrTruncated(sp, child)
		if err != nil {
			return 0, err
		}
		if pageGetLevel(raw) != level-1 {
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
	PageNo    uint32
	Reason    string
	Truncated bool // pageNo is past the end of the file itself - see collectBatch
}

// dedupDeletedRows wraps fn so that a row whose full set of output values
// is byte-identical to one already seen this walk is silently dropped
// (fn is simply never called for it, and the walk carries straight on)
// instead of passed through a second time.
//
// This only ever matters under --deleted-only, and only because the same
// historical row content can genuinely turn up at more than one (page,
// offset) in the same file: a page split copies a record's bytes to the
// new page verbatim, so if the ORIGINAL copy is later found on its old
// page's free list (see walkFreeRecords) while the row's own later,
// separately-deleted copy is independently recovered from wherever it
// ended up living, both copies decode to the exact same field values.
// Showing the same values twice adds no forensic information, so only the
// first copy is kept; a row whose fields genuinely differ from every copy
// seen so far - even one sharing the same primary key - is always kept,
// since that's a distinct historical version of the row, not a duplicate.
// A row that failed to decode (Err set, Row nil) always passes through
// unfiltered, exactly like every other RowOrError consumer treats it.
func dedupDeletedRows(fn func(RowOrError) bool) func(RowOrError) bool {
	seen := make(map[string]bool)
	return func(roe RowOrError) bool {
		if roe.Err != nil || roe.Row == nil {
			return fn(roe)
		}
		key := strings.Join(roe.Row.Values, "\x00")
		if seen[key] {
			return true // duplicate of an earlier row - skip, keep walking
		}
		seen[key] = true
		return fn(roe)
	}
}

// pageEvent is one leaf page's outcome, as collected by WalkRows' sequential
// batch-building loop: either a corrupt-page report (see CorruptPage) or the
// (possibly still being decoded, in parallel - see decodeBatch) rows found
// on an otherwise-good page.
type pageEvent struct {
	corrupt *CorruptPage
	page    []byte // this page's decompressed bytes; nil for a corrupt event
	rows    []RowOrError
}

// WalkRows decodes every live (non-delete-marked) row in the clustered
// index and streams it to fn - or, with deletedOnly, every row that IS
// still delete-marked instead (InnoDB doesn't overwrite a deleted record's
// bytes until the space is reused, so a still-live delete-marked record on
// a leaf page this walk reaches is recoverable exactly like a live one;
// see recDeleted). fn returning false stops the walk early.
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
//
// Reading and validating a page, and following its FIL_PAGE_NEXT pointer to
// the next one, all happen sequentially, exactly as before (that's what
// makes --skip-corrupted's "resume from the next page" strategy, and the
// exact page/row order onCorrupt and fn see, well-defined). What's
// parallel is the CPU-bound part - decodeOneRow's per-column decode and
// (for --format=sql/tsv) literal/field rendering - which this collects a
// batch of up to GOMAXPROCS pages at a time (decodeBatch) to spread across
// every available core, before handing results to onCorrupt/fn ONE PAGE AT
// A TIME, strictly in the same page and row order the old fully-sequential
// version used - so this is purely a speedup: every ordering guarantee,
// and the exact set of rows/pages onCorrupt and fn are called for, is
// unchanged.
func WalkRows(sp *Space, t *Table, outCols []*Column, format outputFormat, skipCorrupted, deletedOnly bool, onCorrupt func(CorruptPage), fn func(RowOrError) bool) error {
	leaf, err := leftmostLeaf(sp, t)
	if err != nil {
		var te *errTruncated
		if skipCorrupted && errors.As(err, &te) {
			// Even navigating from the root down to the correct leftmost
			// leaf isn't possible - some page along the way (maybe the
			// root itself) is past where this truncated file ends. There's
			// no way to resume the ordinary key-ordered walk from here,
			// but --skip-corrupted's job is still "recover what's
			// possible": fall back to reading every page the file DOES
			// have and picking out whichever ones happen to be leaf pages
			// of this index - see scanUnlinkedLeaves. Nothing has been
			// read yet (leftmostLeaf failed before collectBatch ever ran),
			// so there's no visited set to pass.
			if onCorrupt != nil {
				onCorrupt(CorruptPage{
					PageNo: t.RootPage,
					Reason: fmt.Sprintf("can't navigate from the root to the leftmost leaf (%s) - "+
						"falling back to an out-of-order scan of every leaf page the file does hold", te.reason),
					Truncated: true,
				})
			}
			return scanUnlinkedLeaves(sp, t, outCols, format, deletedOnly, nil, fn)
		}
		return fmt.Errorf("finding the leftmost leaf page: %w", err)
	}
	shape := tableZipShape(t)
	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}

	// visited records every page number the ordinary walk below has
	// already processed. This is always needed, regardless of
	// deletedOnly: it's what lets collectBatch notice a leaf chain that
	// loops back on itself (a corrupted, or maliciously crafted,
	// FIL_PAGE_NEXT pointer aiming back at a page already walked) and stop
	// there, rather than following it around forever, endlessly
	// re-decoding and re-emitting the same rows - see the cycle check in
	// collectBatch. With --deleted-only, the same map doubles as the skip
	// list for the supplementary scan below (scanUnlinkedLeaves), so it
	// never re-reads (and so never double-reports) a page the ordinary
	// walk already covered.
	visited := make(map[uint32]bool, sp.NumPages)

	pageNo := leaf
	for pageNo != filNull {
		batch, fatalErr := collectBatch(sp, t, shape, skipCorrupted, deletedOnly, visited, workers, &pageNo)
		decodeBatch(sp, t, outCols, format, batch)

		for _, ev := range batch {
			if ev.corrupt != nil {
				if onCorrupt != nil {
					onCorrupt(*ev.corrupt)
				}
				continue
			}
			for _, roe := range ev.rows {
				if !fn(roe) {
					return nil // fn asked to stop - not an error
				}
			}
		}
		if fatalErr != nil {
			return fatalErr
		}
	}

	if deletedOnly {
		// A leaf page purge has fully emptied can be merged into a sibling
		// and deallocated - unlinked from the live leaf chain the loop
		// above just followed - without its bytes being erased or its
		// header reformatted until the freed page is claimed for
		// something else. Such a page is invisible to the ordinary walk
		// (nothing live points to it any more), but may still hold exactly
		// the delete-marked/free-list leftovers --deleted-only is after,
		// so it needs its own pass - see scanUnlinkedLeaves.
		return scanUnlinkedLeaves(sp, t, outCols, format, deletedOnly, visited, fn)
	}
	return nil
}

// collectRowOffsets returns, for one already-validated, decompressed leaf
// page, every record offset decodeBatch should decode: by default, every
// live record NOT carrying the delete-mark bit (an ordinary row); with
// deletedOnly, instead every live record that DOES carry it (deleted, but
// not yet purged - see recDeleted/walkRecords) PLUS every record still
// sitting on the page's own free list (deleted AND already purged, but not
// yet overwritten by a later insert - see walkFreeRecords). The two
// deletedOnly sources can never overlap: a free-list record was, by
// definition, already unlinked from the live record chain walkRecords
// traverses.
func collectRowOffsets(page []byte, pageSize uint32, deletedOnly bool) []uint32 {
	var offs []uint32
	walkRecords(page, pageSize, func(recOff uint32) bool {
		if recDeleted(page, recOff) == deletedOnly {
			offs = append(offs, recOff)
		}
		return true
	})
	if deletedOnly {
		walkFreeRecords(page, pageSize, func(recOff uint32) bool {
			offs = append(offs, recOff)
			return true
		})
	}
	return offs
}

// rowOrErrorsFor turns a page's own record-offset list (collectRowOffsets)
// into the []RowOrError shape pageEvent/decodeBatch expect, all still
// carrying pageNo and their own recOff with Row/Err left blank.
func rowOrErrorsFor(pageNo uint32, offs []uint32) []RowOrError {
	rows := make([]RowOrError, len(offs))
	for i, recOff := range offs {
		rows[i] = RowOrError{PageNo: pageNo, RecOff: recOff}
	}
	return rows
}

// collectBatch reads and validates up to maxPages leaf pages starting at
// *pageNo, following FIL_PAGE_NEXT exactly as the original single-threaded
// WalkRows did (see its comment for the corrupt-page/--skip-corrupted
// rules), and returns one pageEvent per page - a live-row-offset list for
// decodeBatch to fill in, or a corrupt-page report ready to hand to
// onCorrupt as-is. *pageNo is left at filNull once the walk has genuinely
// ended (chain exhausted, or an unrecoverable corrupt page with
// --skip-corrupted); otherwise it's the next page collectBatch itself
// hasn't read yet. A non-nil returned error is always the *last* thing
// that happened (a fatal corrupt page) - every event before it in the
// returned slice is still good and must still reach onCorrupt/fn.
func collectBatch(sp *Space, t *Table, shape zipIndexShape, skipCorrupted, deletedOnly bool, visited map[uint32]bool, maxPages int, pageNo *uint32) ([]pageEvent, error) {
	batch := make([]pageEvent, 0, maxPages)
	for len(batch) < maxPages && *pageNo != filNull {
		pn := *pageNo
		if visited[pn] {
			// The leaf chain has looped back to a page this same walk
			// already processed. A genuine InnoDB leaf chain never
			// revisits a page, so this only happens when a FIL_PAGE_NEXT
			// pointer is corrupted (as here: the fallback for a page that
			// failed validation still trusts that page's own next-page
			// pointer to keep going - see below - and a damaged one can
			// easily point back into the chain instead of forward) or the
			// file was deliberately crafted to contain a cycle. Following
			// it anyway would re-decode and re-emit the same rows forever,
			// so - like a chain that runs off the end of the file
			// (truncatedReason, below) - this is always where the walk
			// ends, never something to skip past even with
			// --skip-corrupted: there's no way to make forward progress
			// out of a loop.
			*pageNo = filNull
			reason := fmt.Sprintf("its FIL_PAGE_NEXT pointer loops back to page %d, already visited earlier in this same walk - the leaf chain is corrupted", pn)
			if !skipCorrupted {
				return batch, fmt.Errorf("page %d (index %q, id %d): %s", pn, t.IndexName, t.IndexID, reason)
			}
			cp := CorruptPage{PageNo: pn, Reason: reason}
			batch = append(batch, pageEvent{corrupt: &cp})
			return batch, nil
		}
		visited[pn] = true
		if reason := truncatedReason(sp, pn); reason != "" {
			// The file itself doesn't hold this page at all - not a bad
			// checksum or a wrong page type on a page that IS there, but
			// the file ending before it altogether. Most often this means
			// the copy was interrupted partway through (see the README's
			// "Truncated files" section) rather than genuine corruption;
			// either way there's no header here to read a next-page
			// pointer from, so - unlike an ordinary corrupt page - this is
			// always where the walk ends, never something to skip past.
			*pageNo = filNull
			if !skipCorrupted {
				return batch, fmt.Errorf("page %d (index %q, id %d): %s", pn, t.IndexName, t.IndexID, reason)
			}
			cp := CorruptPage{PageNo: pn, Reason: reason, Truncated: true}
			batch = append(batch, pageEvent{corrupt: &cp})
			return batch, nil
		}
		raw, err := sp.ReadPage(pn)
		if err != nil {
			*pageNo = filNull
			return batch, err
		}
		// Validated against the raw physical bytes throughout, exactly as
		// for an uncompressed tablespace (see Decompress's comment):
		// checksum, then FIL_PAGE_TYPE/index id, both readable straight off
		// the header, which is copied verbatim into the compressed format.
		reason := ""
		var page []byte
		if pc := checkPage(raw, sp.Compressed, sp.Flags.fullCRC32); !pc.OK {
			reason = pc.Reason
		} else if filType(raw) != filPageIndex {
			reason = fmt.Sprintf("expected an INDEX page, got type %d", filType(raw))
		} else if pageGetIndexID(raw) != t.IndexID {
			reason = fmt.Sprintf("belongs to index id %d, not the expected %d", pageGetIndexID(raw), t.IndexID)
		} else if page, err = sp.Decompress(raw, shape); err != nil {
			reason = err.Error()
		}
		if reason != "" {
			if !skipCorrupted {
				*pageNo = filNull
				return batch, fmt.Errorf("page %d (index %q, id %d): %s", pn, t.IndexName, t.IndexID, reason)
			}
			cp := CorruptPage{PageNo: pn, Reason: reason}
			batch = append(batch, pageEvent{corrupt: &cp})
			next := filNextPage(raw)
			if next == filNull || next == pn || next >= sp.NumPages {
				*pageNo = filNull // no usable way to keep going from here
				return batch, nil
			}
			*pageNo = next
			continue
		}

		ev := pageEvent{page: page, rows: rowOrErrorsFor(pn, collectRowOffsets(page, sp.PageSize, deletedOnly))}
		batch = append(batch, ev)
		*pageNo = filNextPage(page)
	}
	return batch, nil
}

// scanUnlinkedLeaves reads every page the file actually has, page 0 through
// sp.NumPages-1, and decodes any leaf page it finds belonging to this index
// (matching FIL_PAGE_TYPE, index id, and level 0 - the same checks
// collectBatch applies to a page it already knows is part of this index's
// leaf chain) that isn't in visited, in physical page-number order rather
// than key order. It serves two different callers, for two different
// reasons a page can be unreachable from the live leaf chain the ordinary
// walk follows:
//
//   - WalkRows' --skip-corrupted fallback, when the file is so badly
//     truncated that even navigating from the root to the correct
//     leftmost leaf isn't possible (see leftmostLeaf/errTruncated): normal
//     key order depends on that navigation, so this is the only way to
//     recover anything at all. visited is nil here (nothing has been read
//     yet at that point).
//   - --deleted-only's own supplement to an otherwise-ordinary walk (see
//     WalkRows): a leaf page purge has fully emptied can be merged into a
//     sibling and deallocated - unlinked from the live chain, though its
//     bytes stay exactly as they were until the freed page is claimed for
//     something else - which makes any delete-marked/free-list leftovers
//     it still holds invisible to the ordinary walk. Here visited is the
//     page numbers that walk already covered, so this never re-reads (and
//     so never double-reports) any of them.
//
// Every other page - non-leaf pages of this index, pages belonging to a
// different index or table sharing this tablespace, FSP/INODE/undo
// bookkeeping pages, a page that fails its own checksum - is silently
// skipped: this is a best-effort scan of whatever the file happens to
// still hold, not a validated walk, so there's no single well-defined
// "corrupt page" to report for any of them the way there is during the
// ordinary walk.
func scanUnlinkedLeaves(sp *Space, t *Table, outCols []*Column, format outputFormat, deletedOnly bool, visited map[uint32]bool, fn func(RowOrError) bool) error {
	shape := tableZipShape(t)
	for pn := uint32(0); pn < sp.NumPages; pn++ {
		if visited[pn] {
			continue
		}
		raw, err := sp.ReadPage(pn)
		if err != nil {
			continue
		}
		if pc := checkPage(raw, sp.Compressed, sp.Flags.fullCRC32); !pc.OK {
			continue
		}
		if filType(raw) != filPageIndex || pageGetIndexID(raw) != t.IndexID || pageGetLevel(raw) != 0 {
			continue
		}
		page, err := sp.Decompress(raw, shape)
		if err != nil {
			continue
		}
		offs := collectRowOffsets(page, sp.PageSize, deletedOnly)
		if len(offs) == 0 {
			continue
		}
		ev := pageEvent{page: page, rows: make([]RowOrError, len(offs))}
		for i, recOff := range offs {
			ev.rows[i] = RowOrError{PageNo: pn, RecOff: recOff}
		}
		decodeBatch(sp, t, outCols, format, []pageEvent{ev})
		for _, roe := range ev.rows {
			if !fn(roe) {
				return nil
			}
		}
	}
	return nil
}

// decodeBatch fills in every rows[i].Row/Err left blank by collectBatch (a
// corrupt-page event has none to fill), one goroutine per page, since each
// page's records only ever read that page's own bytes plus (for an
// off-page BLOB/TEXT column) further pages fetched through sp - and
// Space.ReadPage/ReadAt, and every column decoder, only ever read, never
// share or mutate, state across a call - see values.go/lob.go. Returns
// once every page in the batch has finished decoding.
func decodeBatch(sp *Space, t *Table, outCols []*Column, format outputFormat, batch []pageEvent) {
	var wg sync.WaitGroup
	for i := range batch {
		ev := &batch[i]
		if ev.corrupt != nil || len(ev.rows) == 0 {
			continue
		}
		wg.Add(1)
		go func(ev *pageEvent) {
			defer wg.Done()
			for j := range ev.rows {
				row, err := decodeOneRow(sp, ev.page, ev.rows[j].RecOff, t, outCols, format)
				ev.rows[j].Row, ev.rows[j].Err = row, err
			}
		}(ev)
	}
	wg.Wait()
}

func decodeOneRow(sp *Space, page []byte, recOff uint32, t *Table, outCols []*Column, format outputFormat) (row *Row, err error) {
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
			v = nullLiteral(format)
		case fr.Default:
			v, err = decodeInstantDefault(col, format)
		default:
			v, err = decodeField(sp, col, page, fr, format)
		}
		if err != nil {
			return nil, err
		}
		values[i] = v
	}
	return &Row{Values: values}, nil
}

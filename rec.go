// rec.go: the 5-byte "new" (COMPACT/DYNAMIC) record header that precedes
// every record's origin, and the primitives built on it (next-record
// traversal, status/delete-mark/info-bit decoding). Shared by the SDI
// B+tree walker (sdi.go) and the clustered-index row decoder (record.go).
//
// Header layout (5 bytes at page[recOff-5 : recOff]), all COMPACT/DYNAMIC:
//
//	byte -5        : info_bits (high nibble) | n_owned (low nibble)
//	bytes -4,-3    : big-endian uint16 = heap_no(13 bits, >>3) | status(3 bits)
//	bytes -2,-1    : big-endian uint16, signed relative offset to next record
package main

import "encoding/binary"

const (
	recNextBytes = 2

	recInfoBitsMask = 0xF0
	recNOwnedMask   = 0x0F
	recStatusMask   = 0x07

	recInfoMinRecFlag  = 0x10
	recInfoDeletedFlag = 0x20
	recInfoVersionFlag = 0x40
	recInfoInstantFlag = 0x80

	recStatusOrdinary = 0
	recStatusNodePtr  = 1
	recStatusInfimum  = 2
	recStatusSupremum = 3
)

// recInfoBits returns the high nibble of the header's info byte (rec-5).
func recInfoBits(page []byte, recOff uint32) byte {
	return page[recOff-5] & recInfoBitsMask
}

// recDeleted reports the delete-mark bit, dispatching on the page's own
// COMPACT flag since the info-bits byte sits at a different header offset
// for a REDUNDANT record (recOldInfoBits, below) than for a COMPACT/DYNAMIC
// one (recInfoBits).
func recDeleted(page []byte, recOff uint32) bool {
	if !pageIsCompact(page) {
		return recOldDeleted(page, recOff)
	}
	return recInfoBits(page, recOff)&recInfoDeletedFlag != 0
}

func recStatus(page []byte, recOff uint32) byte {
	return page[recOff-3] & recStatusMask
}

// recNextOffset returns the absolute page offset of the next record in the
// singly-linked record chain, or 0 if the chain terminates or the relative
// offset looks corrupt. Offsets wrap modulo the page size, matching
// InnoDB's ut_align_offset(rec + rel, page_size) (page size is always a
// power of two).
func recNextOffset(page []byte, recOff uint32, pageSize uint32) uint32 {
	rel := int16(binary.BigEndian.Uint16(page[recOff-2 : recOff]))
	if rel == 0 {
		return 0
	}
	return uint32(int64(recOff)+int64(rel)) & (pageSize - 1)
}

func recIsInfimum(pageOffset uint32) bool  { return pageOffset == pageNewInfimum }
func recIsSupremum(pageOffset uint32) bool { return pageOffset == pageNewSupremum }

// recSetNextOffsNew writes recNextOffset's inverse: the signed relative
// offset (wrapping the same way InnoDB's own uint16 arithmetic does, so no
// separate negative-number handling is needed) that makes recOff's next
// pointer resolve to target. Used only when reconstructing a page that was
// never laid out on disk as bytes in the first place - a ROW_FORMAT=
// COMPRESSED page's record chain (see zipdecompress.go), synthesized from
// its dense directory rather than read off a physical next-pointer.
func recSetNextOffsNew(page []byte, recOff, target uint32) {
	binary.BigEndian.PutUint16(page[recOff-2:recOff], uint16(target-recOff))
}

// --- REDUNDANT ("old-style") record header - see redundant.go ---
//
// 6-byte header (page[recOff-6 : recOff]):
//
//	byte -6     : info_bits (high nibble) | n_owned (low nibble)
//	bytes -5,-4 : big-endian uint16, heap_no (13 bits, >>3)
//	bytes -4,-3 : big-endian uint16, n_fields (10 bits, >>1) - byte -4 is
//	              shared between the heap_no and n_fields 16-bit windows
//	bytes -2,-1 : big-endian uint16, ABSOLUTE next-record page offset
//	              (unlike COMPACT, never relative, never wraps)

// recOldInfoBits returns the high nibble of the OLD-format info byte
// (rec-6): same bit meanings as recInfoBits, different byte offset.
func recOldInfoBits(page []byte, recOff uint32) byte {
	return page[recOff-6] & recInfoBitsMask
}

func recOldDeleted(page []byte, recOff uint32) bool {
	return recOldInfoBits(page, recOff)&recInfoDeletedFlag != 0
}

// recOldNextOffset returns the absolute page offset of the next record in an
// old-style (REDUNDANT) record chain, or 0 if the chain terminates.
func recOldNextOffset(page []byte, recOff uint32) uint32 {
	return uint32(binary.BigEndian.Uint16(page[recOff-2 : recOff]))
}

func recIsOldInfimum(pageOffset uint32) bool  { return pageOffset == pageOldInfimum }
func recIsOldSupremum(pageOffset uint32) bool { return pageOffset == pageOldSupremum }

// walkRecords calls fn for every non-infimum/non-supremum record on a single
// page, in next-record chain order, stopping at supremum. It does not follow
// FIL_PAGE_NEXT to a sibling page - callers that need the whole leaf level
// do that themselves (see btree.go), since only they know when to stop.
// Dispatches on the page's own COMPACT flag bit (PAGE_N_HEAP's top bit), not
// the table's nominal ROW_FORMAT, since that's what InnoDB itself checks.
func walkRecords(page []byte, pageSize uint32, fn func(recOff uint32) bool) {
	if !pageIsCompact(page) {
		off := recOldNextOffset(page, pageOldInfimum)
		for off != 0 && !recIsOldSupremum(off) {
			if !fn(off) {
				return
			}
			off = recOldNextOffset(page, off)
		}
		return
	}
	off := recNextOffset(page, pageNewInfimum, pageSize)
	for off != 0 && !recIsSupremum(off) {
		if !fn(off) {
			return
		}
		off = recNextOffset(page, off, pageSize)
	}
}

// walkFreeRecords calls fn for every record on a single page's own free
// list (PAGE_FREE) - InnoDB's singly-linked list of record slots freed by
// purge (an already-committed DELETE whose row has since been physically
// removed from the live record chain, as opposed to a delete-marked record
// still on it - see recDeleted/walkRecords) but not yet reused for a new
// row. A freed slot's own "next free" pointer lives in exactly the header
// field a live record's next-record pointer does (rec.go's package
// comment), so this is structurally identical to walkRecords, just started
// from PAGE_FREE instead of infimum; the rest of the freed record's bytes
// - including every field decodeOneRow reads - are untouched by purge, so
// it decodes with the same fidelity as a live record, for as long as the
// slot hasn't been carved up for a subsequent insert or lost to page
// reorganization.
//
// The iteration cap (this page's own PAGE_N_HEAP - an upper bound on how
// many record slots this page has ever handed out, live or free, so it can
// never legitimately be exceeded) guards against an infinite loop if the
// free list were somehow cyclic; it does not affect any genuine free list,
// which is always shorter than that.
func walkFreeRecords(page []byte, pageSize uint32, fn func(recOff uint32) bool) {
	maxIter := int(pageGetNHeap(page))
	if !pageIsCompact(page) {
		off := uint32(pageGetFree(page))
		for off != 0 && maxIter > 0 {
			if !fn(off) {
				return
			}
			off = recOldNextOffset(page, off)
			maxIter--
		}
		return
	}
	off := uint32(pageGetFree(page))
	for off != 0 && maxIter > 0 {
		if !fn(off) {
			return
		}
		off = recNextOffset(page, off, pageSize)
		maxIter--
	}
}

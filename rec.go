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

func recDeleted(page []byte, recOff uint32) bool {
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

// walkRecords calls fn for every non-infimum/non-supremum record on a single
// page, in next-record chain order, stopping at supremum. It does not follow
// FIL_PAGE_NEXT to a sibling page - callers that need the whole leaf level
// do that themselves (see btree.go), since only they know when to stop.
func walkRecords(page []byte, pageSize uint32, fn func(recOff uint32) bool) {
	off := recNextOffset(page, pageNewInfimum, pageSize)
	for off != 0 && !recIsSupremum(off) {
		if !fn(off) {
			return
		}
		off = recNextOffset(page, off, pageSize)
	}
}

// lob.go: fetches the current value of an externally-stored (off-page)
// column, in whichever of the two formats its first page turns out to be:
//
//   - The modern LOB format (FIL_PAGE_TYPE_LOB_FIRST / LOB_DATA pages,
//     fetchLOB) - the straightforward "walk the current index-entry list
//     front to back" path. It deliberately does not implement LOB *version*
//     selection (JSON partial-update history), which this tool has no use
//     for since it only ever wants the row's current value. Ported from
//     ibdNinja's FetchModernUncompLob (ibdNinja.cc).
//   - The older, much simpler chain of FIL_PAGE_TYPE_BLOB pages (fetchOldBlob,
//     built on the same fetchSimpleBlobChain helper sdi.go uses for
//     FIL_PAGE_SDI_BLOB pages - it's the same on-disk structure, just a
//     different FIL_PAGE_TYPE and no system-page reservation to guard
//     against).
//
// Which one a given tablespace uses is a property of the tablespace itself
// (whether it was ever laid out in the pre-5.6 "Antelope" file format, i.e.
// FSP_FLAGS' atomic_blobs bit), not of the table's declared ROW_FORMAT: a
// ROW_FORMAT=REDUNDANT table in a modern (any 5.6+, definitely any 8.0+)
// per-table tablespace still gets the modern LOB format for its overflow
// pages - confirmed empirically against a real MySQL 8.0.46 REDUNDANT table.
// So rather than trust either the row format or the FSP flag, fetchExternal
// just looks at the actual page it lands on and believes that instead.
package main

import (
	"encoding/binary"
	"fmt"
)

// fetchExternal fetches an externally-stored column's value, detecting
// which off-page format it uses from its first page's own FIL_PAGE_TYPE.
func fetchExternal(sp *Space, ref externalRef) ([]byte, error) {
	first, err := sp.ReadPage(ref.PageNo)
	if err != nil {
		return nil, err
	}
	switch filType(first) {
	case filPageTypeLOBFirst:
		return fetchLOB(sp, ref)
	case filPageTypeBlob:
		return fetchOldBlob(sp, ref)
	default:
		return nil, fmt.Errorf("page %d has type %d, neither a LOB_FIRST nor a BLOB page", ref.PageNo, filType(first))
	}
}

// fetchOldBlob follows a FIL_PAGE_TYPE_BLOB chain (the pre-5.6 "Antelope"
// off-page storage format) to recover a column value stored there.
func fetchOldBlob(sp *Space, ref externalRef) ([]byte, error) {
	return fetchSimpleBlobChain(sp, ref.PageNo, int(ref.Length), filPageTypeBlob, 0)
}

// fetchSimpleBlobChain walks a chain of pages sharing the oldest, simplest
// InnoDB off-page format: an 8-byte header (4-byte part length, 4-byte next
// page number) at FIL_PAGE_DATA, followed immediately by that many data
// bytes. Used for both FIL_PAGE_TYPE_BLOB (REDUNDANT's off-page columns,
// lob.go) and FIL_PAGE_SDI_BLOB (the SDI's own overflow storage, sdi.go).
// minPageNo rejects a chain that (corrupt or not) points back into the
// tablespace's reserved system pages (0..3 for SDI; unused, i.e. 0, for a
// plain BLOB chain, which is free to start as low as page 4 like any other
// allocated page).
func fetchSimpleBlobChain(sp *Space, firstPage uint32, want int, wantType uint16, minPageNo uint32) ([]byte, error) {
	const hdrPartLen = 0
	const hdrNextPage = 4
	const hdrSize = 8
	out := make([]byte, 0, want)
	pageNo := firstPage
	for len(out) < want {
		page, err := sp.ReadPage(pageNo)
		if err != nil {
			return nil, err
		}
		if filType(page) != wantType {
			return nil, fmt.Errorf("expected page type %d at page %d, got %d", wantType, pageNo, filType(page))
		}
		partLen := binary.BigEndian.Uint32(page[filPageData+hdrPartLen:])
		start := filPageData + hdrSize
		end := start + int(partLen)
		if end > len(page)-filPageDataEnd || len(out)+int(partLen) > want {
			return nil, fmt.Errorf("corrupt blob page %d: part length out of range", pageNo)
		}
		out = append(out, page[start:end]...)
		next := binary.BigEndian.Uint32(page[filPageData+hdrNextPage:])
		if len(out) >= want {
			break
		}
		if next == filNull || next <= minPageNo {
			return nil, fmt.Errorf("truncated blob chain at page %d", pageNo)
		}
		pageNo = next
	}
	return out, nil
}

const (
	lobFirstPageDataLen    = 16
	lobFirstPageIndexList  = 26
	lobFirstPageIndexBegin = 58
	lobFirstPageNEntries   = 10
	lobIndexEntrySize      = 60

	lobEntryNext    = 6
	lobEntryPageNo  = 48
	lobEntryDataLen = 52

	lobDataPageDataBegin = 11

	lobMaxFetchSize    = 64 << 20 // generous cap against corrupt length fields
	lobMaxPagesVisited = 1 << 20
)

// filAddr is a {page_no, byte_offset} file address; a null address has
// page_no == filNull.
type filAddr struct {
	pageNo uint32
	offset uint16
}

func (a filAddr) isNull() bool { return a.pageNo == filNull }

func readFilAddr(b []byte) filAddr {
	return filAddr{pageNo: binary.BigEndian.Uint32(b[0:4]), offset: binary.BigEndian.Uint16(b[4:6])}
}

// fetchLOB returns the current value of an externally-stored column given
// its BTR_EXTERN reference (page.go/record.go's externalRef).
func fetchLOB(sp *Space, ref externalRef) ([]byte, error) {
	wantLen := ref.Length
	if wantLen > lobMaxFetchSize {
		return nil, fmt.Errorf("external value is %d bytes, over this tool's %d-byte cap", ref.Length, uint64(lobMaxFetchSize))
	}
	page, err := sp.ReadPage(ref.PageNo)
	if err != nil {
		return nil, err
	}
	if filType(page) != filPageTypeLOBFirst {
		return nil, fmt.Errorf("expected a LOB_FIRST page at %d, got type %d (compressed/REDUNDANT LOB formats are not supported)", ref.PageNo, filType(page))
	}
	// lobFirstPageIndexList points at a FlstBaseNode (4-byte length, then
	// the 6-byte "first" FilAddr we actually want - LOB_ENTRY_NEXT below is
	// a bare FilAddr instead, with no such length prefix to skip).
	indexListFirst := readFilAddr(page[filPageData+lobFirstPageIndexList+4:])
	firstPageDataOff := filPageData + lobFirstPageIndexBegin + lobFirstPageNEntries*lobIndexEntrySize

	out := make([]byte, 0, wantLen)
	cur := indexListFirst
	cachedPageNo := ref.PageNo
	visited := 0
	for !cur.isNull() && uint64(len(out)) < wantLen {
		visited++
		if visited > lobMaxPagesVisited {
			return nil, fmt.Errorf("LOB index chain did not terminate within %d pages", lobMaxPagesVisited)
		}
		if cur.pageNo != cachedPageNo {
			page, err = sp.ReadPage(cur.pageNo)
			if err != nil {
				return nil, err
			}
			cachedPageNo = cur.pageNo
		}
		entOff := int(cur.offset)
		if entOff+lobIndexEntrySize > len(page) {
			return nil, fmt.Errorf("LOB index entry at page %d offset %d is out of bounds", cur.pageNo, entOff)
		}
		entry := page[entOff : entOff+lobIndexEntrySize]
		next := readFilAddr(entry[lobEntryNext:])
		dataPageNo := binary.BigEndian.Uint32(entry[lobEntryPageNo:])
		dataLen := uint64(binary.BigEndian.Uint16(entry[lobEntryDataLen:]))
		if uint64(len(out))+dataLen > wantLen {
			dataLen = wantLen - uint64(len(out))
		}

		var src []byte
		var srcOff int
		if dataPageNo == ref.PageNo {
			firstPage := page
			if cachedPageNo != ref.PageNo {
				firstPage, err = sp.ReadPage(ref.PageNo)
				if err != nil {
					return nil, err
				}
			}
			src, srcOff = firstPage, firstPageDataOff
		} else {
			dp, err := sp.ReadPage(dataPageNo)
			if err != nil {
				return nil, err
			}
			if filType(dp) != filPageTypeLOBData {
				return nil, fmt.Errorf("expected a LOB_DATA page at %d, got type %d", dataPageNo, filType(dp))
			}
			src, srcOff = dp, filPageData+lobDataPageDataBegin
		}
		avail := uint64(0)
		if pageEnd := len(src) - filPageDataEnd; pageEnd > srcOff {
			avail = uint64(pageEnd - srcOff)
		}
		if dataLen > avail {
			dataLen = avail
		}
		out = append(out, src[srcOff:srcOff+int(dataLen)]...)
		cur = next
	}
	return out, nil
}

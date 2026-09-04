// lob.go: fetches the current value of an externally-stored (off-page)
// column through MySQL 8.0's "modern" LOB storage format (FIL_PAGE_TYPE_
// LOB_FIRST / LOB_DATA pages). This is the straightforward "walk the
// current index-entry list front to back" path; it deliberately does not
// implement LOB *version* selection (JSON partial-update history), which
// this tool has no use for since it only ever wants the row's current
// value. Ported from ibdNinja's FetchModernUncompLob (ibdNinja.cc).
//
// v1 supports only this modern, uncompressed LOB format (DYNAMIC/COMPACT
// row formats without ROW_FORMAT=COMPRESSED, which is the whole of v1's
// supported scope per schema.go). The older simple BLOB-page chain used by
// REDUNDANT tables (FIL_PAGE_TYPE_BLOB, no LOB_FIRST framing) is not
// implemented.
package main

import (
	"encoding/binary"
	"fmt"
)

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

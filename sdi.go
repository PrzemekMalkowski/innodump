// sdi.go: locate and extract the Serialized Dictionary Information (SDI)
// that MySQL 8.0+ embeds in every tablespace. The SDI is itself stored as
// rows of a small hidden B+tree index (the "SDI index"): each row holds one
// dictionary object (a Table or a Tablespace) as zlib-compressed JSON.
//
// Steps (mirrors ibdNinja's ibdNinja.cc, which was used as the reference for
// this layout):
//  1. Page 0's FSP header is followed by the XDES (extent descriptor) array;
//     right after that sits a small FSEG-header-shaped pointer whose second
//     4-byte field is the SDI index's root page number.
//  2. Walk from the root down to the leftmost leaf page (level 0) via the
//     first user record's node-pointer child, same as any InnoDB B+tree.
//  3. Walk every record on the leaf level (following FIL_PAGE_NEXT across
//     pages), decoding each SDI row's fixed header (type, id, compressed/
//     uncompressed length) and then its one variable-length column (the
//     zlib-compressed JSON), which may spill into FIL_PAGE_SDI_BLOB pages.
//  4. zlib-inflate each row's payload to recover the dictionary JSON.
package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// SDI record fixed-header offsets (relative to the record origin).
const (
	sdiOffType      = 0
	sdiTypeLen      = 4
	sdiOffID        = sdiOffType + sdiTypeLen // 4
	sdiIDLen        = 8
	sdiOffTrxID     = sdiOffID + sdiIDLen // 12
	sdiTrxIDLen     = 6
	sdiOffRollPtr   = sdiOffTrxID + sdiTrxIDLen // 18
	sdiRollPtrLen   = 7
	sdiOffUncompLen = sdiOffRollPtr + sdiRollPtrLen // 25
	sdiOffCompLen   = sdiOffUncompLen + 4           // 29
	sdiOffData      = sdiOffCompLen + 4             // 33 (REC_OFF_DATA_VARCHAR)

	sdiAntelopePrefix = 768 // REC_ANTELOPE_MAX_INDEX_COL_LEN: fixed in-page
	// prefix length the SDI index uses for an
	// externally-stored row (0 or exactly this).
	sdiBlobAllowed = 4 // page numbers 0..3 are reserved (FSP hdr/bitmaps/SDI root)
)

// sdiRoot returns the SDI index's root page number, read from the fixed
// slot on page 0 right after the XDES array. Returns 0 if the tablespace's
// FSP flags don't advertise SDI at all (older files / no dictionary copy).
func sdiRoot(page0 []byte, pageSize uint32, hasSDI bool) (uint32, error) {
	off := xdesArrOffset + xdesSize(pageSize)*(pageSize/fspExtentSize(pageSize)) + infoMaxSize
	if int(off)+8 > len(page0) {
		return 0, fmt.Errorf("SDI root pointer offset %d is beyond page 0", off)
	}
	root := binary.BigEndian.Uint32(page0[off+4 : off+8])
	if !hasSDI && root < sdiBlobAllowed {
		return 0, fmt.Errorf("no SDI found in this tablespace (older MySQL version, or not file-per-table)")
	}
	return root, nil
}

// sdiLeftmostLeaf descends from the SDI root to the leftmost leaf page.
func sdiLeftmostLeaf(sp *Space, root uint32) (uint32, error) {
	page, err := sp.ReadPage(root)
	if err != nil {
		return 0, err
	}
	if pageGetNRecs(page) == 0 {
		return 0, fmt.Errorf("SDI index is empty (older MySQL version?)")
	}
	cur := root
	for pageGetLevel(page) != 0 {
		if recStatus(page, pageNewInfimum) != recStatusInfimum {
			return 0, fmt.Errorf("corrupt SDI page %d: expected infimum", cur)
		}
		nextOff := recNextOffset(page, pageNewInfimum, sp.PageSize)
		if nextOff == 0 || int(nextOff)+sdiOffID+sdiIDLen > len(page) {
			return 0, fmt.Errorf("corrupt SDI non-leaf page %d", cur)
		}
		child := binary.BigEndian.Uint32(page[nextOff+sdiOffID+sdiIDLen : nextOff+sdiOffID+sdiIDLen+4])
		if child < sdiBlobAllowed {
			return 0, fmt.Errorf("corrupt SDI node pointer on page %d -> %d", cur, child)
		}
		level := pageGetLevel(page)
		page, err = sp.ReadPage(child)
		if err != nil {
			return 0, err
		}
		if pageGetLevel(page) != level-1 {
			return 0, fmt.Errorf("SDI tree is not well-formed at page %d", child)
		}
		cur = child
	}
	return cur, nil
}

// sdiRow is one decoded (and decompressed) SDI record.
type sdiRow struct {
	Type uint32
	ID   uint64
	JSON []byte
}

// sdiFetchBlob follows a FIL_PAGE_SDI_BLOB chain to recover data stored
// off-page for one SDI row.
func sdiFetchBlob(sp *Space, firstPage uint32, want int) ([]byte, error) {
	const lobHdrPartLen = 0
	const lobHdrNextPage = 4
	const lobHdrSize = 8
	out := make([]byte, 0, want)
	pageNo := firstPage
	for len(out) < want {
		page, err := sp.ReadPage(pageNo)
		if err != nil {
			return nil, err
		}
		if filType(page) != filPageSDIBlob {
			return nil, fmt.Errorf("expected SDI_BLOB page at %d, got type %d", pageNo, filType(page))
		}
		partLen := binary.BigEndian.Uint32(page[filPageData+lobHdrPartLen:])
		start := filPageData + lobHdrSize
		end := start + int(partLen)
		if end > len(page)-filPageDataEnd || len(out)+int(partLen) > want {
			return nil, fmt.Errorf("corrupt SDI_BLOB page %d: part length out of range", pageNo)
		}
		out = append(out, page[start:end]...)
		next := binary.BigEndian.Uint32(page[filPageData+lobHdrNextPage:])
		if len(out) >= want {
			break
		}
		if next <= sdiBlobAllowed {
			return nil, fmt.Errorf("truncated SDI_BLOB chain at page %d", pageNo)
		}
		pageNo = next
	}
	return out, nil
}

// sdiParseRecord decodes one SDI row at recOff on page, resolving any
// off-page portion and zlib-inflating the JSON payload.
func sdiParseRecord(sp *Space, page []byte, recOff uint32) (*sdiRow, error) {
	if int(recOff)+sdiOffData > len(page) {
		return nil, fmt.Errorf("SDI record header runs past the page")
	}
	typ := binary.BigEndian.Uint32(page[recOff+sdiOffType:])
	id := binary.BigEndian.Uint64(page[recOff+sdiOffID:])
	uncompLen := binary.BigEndian.Uint32(page[recOff+sdiOffUncompLen:])
	compLen := binary.BigEndian.Uint32(page[recOff+sdiOffCompLen:])

	lenByte := page[recOff-6]
	var (
		dataLen   uint64
		external  bool
		inPageLen uint32
	)
	switch {
	case lenByte&0x80 == 0:
		dataLen = uint64(lenByte)
	case lenByte&0x40 != 0:
		external = true
		inPageLen = uint32(lenByte&0x3f) << 8
		if inPageLen != 0 && inPageLen != sdiAntelopePrefix {
			return nil, fmt.Errorf("unexpected SDI in-page prefix length %d", inPageLen)
		}
		if int(recOff)+sdiOffData+int(inPageLen)+20 > len(page) {
			return nil, fmt.Errorf("SDI external reference runs past the page")
		}
		extLen := binary.BigEndian.Uint64(page[recOff+sdiOffData+inPageLen+12:]) & 0x1FFFFFFFFF
		dataLen = extLen + uint64(inPageLen)
	default:
		inPageLen = uint32(lenByte&0x3f) << 8
		lenByte2 := page[recOff-7]
		dataLen = uint64(inPageLen) + uint64(lenByte2)
	}
	if dataLen != uint64(compLen) {
		return nil, fmt.Errorf("SDI record corrupt: stored length %d != compressed length %d", dataLen, compLen)
	}
	const sdiMaxLen = 256 << 20
	if dataLen > sdiMaxLen || uncompLen > sdiMaxLen {
		return nil, fmt.Errorf("SDI record has an unreasonable length")
	}

	var raw []byte
	if external {
		if int(recOff)+sdiOffData+int(inPageLen) > len(page) {
			return nil, fmt.Errorf("SDI record data runs past the page")
		}
		raw = append(raw, page[recOff+sdiOffData:recOff+sdiOffData+inPageLen]...)
		extRefOff := recOff + sdiOffData + inPageLen
		firstBlobPage := binary.BigEndian.Uint32(page[extRefOff+4:])
		blob, err := sdiFetchBlob(sp, firstBlobPage, int(dataLen)-int(inPageLen))
		if err != nil {
			return nil, fmt.Errorf("fetching external SDI data: %w", err)
		}
		raw = append(raw, blob...)
	} else {
		if int(recOff)+sdiOffData+int(dataLen) > len(page) {
			return nil, fmt.Errorf("SDI record data runs past the page")
		}
		raw = append(raw, page[recOff+sdiOffData:recOff+sdiOffData+uint32(dataLen)]...)
	}

	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("SDI payload is not valid zlib data: %w", err)
	}
	defer zr.Close()
	jsonData, err := io.ReadAll(io.LimitReader(zr, int64(uncompLen)+1))
	if err != nil {
		return nil, fmt.Errorf("inflating SDI payload: %w", err)
	}
	return &sdiRow{Type: typ, ID: id, JSON: jsonData}, nil
}

// sdiEnvelope is the outer wrapper MySQL puts around every SDI JSON object.
type sdiEnvelope struct {
	DDObjectType string          `json:"dd_object_type"`
	DDObject     json.RawMessage `json:"dd_object"`
}

// LoadSDITables walks the whole SDI index and returns the raw dd_object
// JSON for every dictionary object of type "Table".
func LoadSDITables(sp *Space) ([]json.RawMessage, error) {
	root, err := sdiRoot(sp.f0(), sp.PageSize, sp.Flags.sdi)
	if err != nil {
		return nil, err
	}
	leaf, err := sdiLeftmostLeaf(sp, root)
	if err != nil {
		return nil, err
	}

	var tables []json.RawMessage
	pageNo := leaf
	for pageNo != filNull {
		page, err := sp.ReadPage(pageNo)
		if err != nil {
			return nil, err
		}
		if filType(page) != filPageSDI {
			return nil, fmt.Errorf("expected SDI page at %d, got type %d", pageNo, filType(page))
		}
		var walkErr error
		walkRecords(page, sp.PageSize, func(recOff uint32) bool {
			if recDeleted(page, recOff) {
				return true
			}
			row, err := sdiParseRecord(sp, page, recOff)
			if err != nil {
				walkErr = err
				return false
			}
			var env sdiEnvelope
			if err := json.Unmarshal(row.JSON, &env); err != nil {
				walkErr = fmt.Errorf("parsing SDI JSON: %w", err)
				return false
			}
			if env.DDObjectType == "Table" {
				tables = append(tables, env.DDObject)
			}
			return true
		})
		if walkErr != nil {
			return nil, walkErr
		}
		pageNo = filNextPage(page)
	}
	return tables, nil
}

// f0 reads and caches page 0 (used by both sdiRoot and the caller that
// already needed it for page-size detection).
func (s *Space) f0() []byte {
	if s.page0 == nil {
		s.page0, _ = s.ReadPage(0)
	}
	return s.page0
}

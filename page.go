// ibd-extractor - offline schema+data extraction from a MySQL 8.0/8.4 InnoDB
// .ibd file (one table per file, ROW_FORMAT=DYNAMIC/COMPACT, uncompressed).
//
// page.go: the FIL/FSP layer - page size detection, the common 38-byte FIL
// page header every page starts with, and raw page I/O.
//
// Layout references (byte offsets, all integers big-endian unless noted):
//
//	FIL header (38 bytes, every page):
//	  checksum[4] page_no[4] prev[4] next[4] lsn[8] type[2] flush_lsn[8] space_id[4]
//	FSP header (page 0 only, starts right after the FIL header at byte 38):
//	  space_id[4] unused[4] size[4] free_limit[4] flags[4] frag_n_used[4] ...
//
// Cross-checked against storage/innobase/include/{fil0fil.h,fsp0fsp.h} in the
// mysql-server source and against the ibdNinja project (GPL-3.0), which was
// used as the primary map of these layouts during development.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
)

// FIL header offsets (relative to the start of a page).
const (
	filPageSpaceOrChksum = 0
	filPageOffset        = 4 // page number
	filPagePrev          = 8
	filPageNext          = 12
	filPageLSN           = 16
	filPageType          = 24
	filPageFlushLSN      = 26 // page 0 only
	filPageSpaceID       = 34
	filPageData          = 38 // page body starts here
	filPageDataEnd       = 8  // trailer size (old checksum[4] + low32 LSN[4])
)

// FIL page types we care about (storage/innobase/include/fil0fil.h).
const (
	filPageIndex        = 17855
	filPageSDI          = 17853
	filPageTypeFSPHdr   = 8
	filPageTypeXDES     = 9
	filPageTypeLOBFirst = 24
	filPageTypeLOBData  = 23
	filPageSDIBlob      = 18
)

const filNull = 0xFFFFFFFF

// FSP header, at filPageData (38) on page 0.
const (
	fspHeaderOffset = filPageData
	fspSpaceFlags   = 16 // offset from fspHeaderOffset
)

// FSP_FLAGS bit layout (storage/innobase/include/fsp0fsp.h).
const (
	fspFlagsPosPostAntelope   = 0
	fspFlagsWidthPostAntelope = 1
	fspFlagsPosZipSSize       = fspFlagsPosPostAntelope + fspFlagsWidthPostAntelope
	fspFlagsWidthZipSSize     = 4
	fspFlagsPosAtomicBlobs    = fspFlagsPosZipSSize + fspFlagsWidthZipSSize
	fspFlagsWidthAtomicBlobs  = 1
	fspFlagsPosPageSSize      = fspFlagsPosAtomicBlobs + fspFlagsWidthAtomicBlobs
	fspFlagsWidthPageSSize    = 4
	fspFlagsPosDataDir        = fspFlagsPosPageSSize + fspFlagsWidthPageSSize
	fspFlagsWidthDataDir      = 1
	fspFlagsPosShared         = fspFlagsPosDataDir + fspFlagsWidthDataDir
	fspFlagsWidthShared       = 1
	fspFlagsPosTemporary      = fspFlagsPosShared + fspFlagsWidthShared
	fspFlagsWidthTemporary    = 1
	fspFlagsPosEncryption     = fspFlagsPosTemporary + fspFlagsWidthTemporary
	fspFlagsWidthEncryption   = 1
	fspFlagsPosSDI            = fspFlagsPosEncryption + fspFlagsWidthEncryption
	fspFlagsWidthSDI          = 1
)

func fspFlagsField(flags uint32, pos, width uint32) uint32 {
	mask := ^(^uint32(0) << width)
	return (flags >> pos) & mask
}

// fspFlags is the decoded FSP_SPACE_FLAGS word.
type fspFlags struct {
	raw          uint32
	zipSSize     uint32 // compressed page size selector (0 = not compressed)
	pageSSize    uint32 // logical page size selector (0 = default 16K)
	postAntelope bool
	atomicBlobs  bool
	dataDir      bool
	shared       bool
	temporary    bool
	encryption   bool
	sdi          bool
}

func parseFSPFlags(raw uint32) fspFlags {
	return fspFlags{
		raw:          raw,
		zipSSize:     fspFlagsField(raw, fspFlagsPosZipSSize, fspFlagsWidthZipSSize),
		pageSSize:    fspFlagsField(raw, fspFlagsPosPageSSize, fspFlagsWidthPageSSize),
		postAntelope: fspFlagsField(raw, fspFlagsPosPostAntelope, fspFlagsWidthPostAntelope) != 0,
		atomicBlobs:  fspFlagsField(raw, fspFlagsPosAtomicBlobs, fspFlagsWidthAtomicBlobs) != 0,
		dataDir:      fspFlagsField(raw, fspFlagsPosDataDir, fspFlagsWidthDataDir) != 0,
		shared:       fspFlagsField(raw, fspFlagsPosShared, fspFlagsWidthShared) != 0,
		temporary:    fspFlagsField(raw, fspFlagsPosTemporary, fspFlagsWidthTemporary) != 0,
		encryption:   fspFlagsField(raw, fspFlagsPosEncryption, fspFlagsWidthEncryption) != 0,
		sdi:          fspFlagsField(raw, fspFlagsPosSDI, fspFlagsWidthSDI) != 0,
	}
}

// XDES / extent-descriptor geometry, needed only to compute where the SDI
// root-page pointer sits on page 0 (see sdi.go). Constants from fsp0fsp.h.
const (
	filAddrSize      = 6
	flstBaseNodeSize = 4 + 2*filAddrSize       // 16
	flstNodeSize     = 2 * filAddrSize         // 12
	fspHeaderSize    = 32 + 5*flstBaseNodeSize // 112
	xdesArrOffset    = fspHeaderOffset + fspHeaderSize
	xdesBitmap       = flstNodeSize + 12 // 24
	xdesBitsPerPage  = 2
	// Encryption info block that (potentially) sits between the XDES array
	// and the SDI root pointer on page 0 (INFO_MAX_SIZE in ibdUtils.h).
	infoMaxSize = 115
)

// fspExtentSize returns FSP_EXTENT_SIZE (pages per extent) for a given
// logical page size.
func fspExtentSize(pageSize uint32) uint32 {
	switch {
	case pageSize <= 16384:
		return 1048576 / pageSize
	case pageSize <= 32768:
		return 2097152 / pageSize
	default:
		return 4194304 / pageSize
	}
}

func xdesSize(pageSize uint32) uint32 {
	bits := fspExtentSize(pageSize) * xdesBitsPerPage
	return xdesBitmap + (bits+7)/8
}

// Space wraps an open .ibd file and knows how to fetch individual pages.
type Space struct {
	f        *os.File
	PageSize uint32 // logical page size (== physical: v1 supports uncompressed only)
	NumPages uint32
	SpaceID  uint32
	Flags    fspFlags
	page0    []byte // lazily cached by f0()
}

// OpenSpace opens path and determines its page size from the FSP header on
// page 0. Returns an error for compressed/encrypted/temporary tablespaces,
// which v1 does not support.
func OpenSpace(path string) (*Space, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	// Read a minimal first page (4KB, the smallest legal InnoDB page) to
	// find the real page size before doing anything page-size-dependent.
	head := make([]byte, 4096)
	if _, err := f.ReadAt(head, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("reading page 0 header: %w", err)
	}
	rawFlags := binary.BigEndian.Uint32(head[fspHeaderOffset+fspSpaceFlags:])
	flags := parseFSPFlags(rawFlags)

	var pageSize uint32
	if flags.pageSSize == 0 {
		pageSize = 16384
	} else {
		pageSize = (1024 >> 1) << flags.pageSSize // 512 << ssize
	}
	if pageSize < 4096 || pageSize > 65536 || (pageSize&(pageSize-1)) != 0 {
		f.Close()
		return nil, fmt.Errorf("could not determine a valid page size (flags=0x%08x)", rawFlags)
	}
	if flags.zipSSize != 0 {
		f.Close()
		return nil, fmt.Errorf("ROW_FORMAT=COMPRESSED / page-compressed tablespaces are not supported (v1 limitation)")
	}
	if flags.encryption {
		f.Close()
		return nil, fmt.Errorf("encrypted tablespaces are not supported (v1 limitation)")
	}
	if flags.temporary {
		f.Close()
		return nil, fmt.Errorf("temporary tablespaces are not supported")
	}

	spaceID := binary.BigEndian.Uint32(head[filPageSpaceID:])
	return &Space{
		f:        f,
		PageSize: pageSize,
		NumPages: uint32(fi.Size() / int64(pageSize)),
		SpaceID:  spaceID,
		Flags:    flags,
	}, nil
}

func (s *Space) Close() error { return s.f.Close() }

// ReadPage returns the raw bytes of page pageNo.
func (s *Space) ReadPage(pageNo uint32) ([]byte, error) {
	buf := make([]byte, s.PageSize)
	off := int64(pageNo) * int64(s.PageSize)
	n, err := s.f.ReadAt(buf, off)
	if err != nil && n != len(buf) {
		return nil, fmt.Errorf("reading page %d: %w", pageNo, err)
	}
	return buf, nil
}

// --- Page corruption detection ---
//
// Cross-checked against BlockReporter::is_corrupted (storage/innobase/buf/
// checksum.cc) in the mysql-server source. This implements the two checks
// that cover the overwhelming majority of real installations:
//
//   - the LSN consistency check (always done, independent of checksum
//     algorithm): the low 4 bytes of the FIL_PAGE_LSN field must equal the
//     page's last 4 bytes, which store the same value redundantly.
//   - the "crc32" checksum algorithm (innodb_checksum_algorithm=crc32, the
//     default since 5.7), which is CRC-32C (Castagnoli), NOT the plain CRC-32
//     zlib/gzip use - confirmed empirically against real pages before
//     relying on it here.
//
// Not implemented: the legacy "innodb" checksum algorithm (a custom Fletcher
// -like hash, pre-5.7 default) and page-compressed (zlib) checksums, which
// are out of scope anyway (v1 rejects compressed tablespaces in OpenSpace).
// A tablespace still using innodb_checksum_algorithm=innodb will report
// false corruption here; --skip-corrupted or a real innochecksum run can
// tell the two apart.
const bufNoChecksumMagic = 0xDEADBEEF

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// pageCheck is the result of validating one page's checksum/LSN fields.
type pageCheck struct {
	OK     bool
	Empty  bool // an unallocated, all-zero page - not corrupt, just unused
	Reason string
}

func checkPage(page []byte) pageCheck {
	n := len(page)
	if n < filPageData+filPageDataEnd {
		return pageCheck{Reason: "page is shorter than a valid FIL header+trailer"}
	}
	if !bytes.Equal(page[filPageLSN+4:filPageLSN+8], page[n-4:n]) {
		return pageCheck{Reason: "LSN low bytes at the start and end of the page disagree"}
	}
	f1 := binary.BigEndian.Uint32(page[0:4])
	f2 := binary.BigEndian.Uint32(page[n-8 : n-4])
	lsn8 := binary.BigEndian.Uint64(page[filPageLSN:])
	if f1 == 0 && f2 == 0 && lsn8 == 0 {
		for _, b := range page {
			if b != 0 {
				return pageCheck{Reason: "checksum and LSN fields are zero but the page is not"}
			}
		}
		return pageCheck{OK: true, Empty: true}
	}
	if f1 == bufNoChecksumMagic && f2 == bufNoChecksumMagic {
		return pageCheck{OK: true} // innodb_checksum_algorithm=none
	}
	c1 := crc32.Checksum(page[4:26], crc32cTable)
	c2 := crc32.Checksum(page[38:n-8], crc32cTable)
	if f1 == f2 && f1 == c1^c2 {
		return pageCheck{OK: true}
	}
	return pageCheck{Reason: fmt.Sprintf("checksum mismatch (stored %08x/%08x, computed crc32c %08x)", f1, f2, c1^c2)}
}

// --- FIL header accessors (operate on a single page's bytes) ---

func filType(page []byte) uint16     { return binary.BigEndian.Uint16(page[filPageType:]) }
func filNextPage(page []byte) uint32 { return binary.BigEndian.Uint32(page[filPageNext:]) }
func filPrevPage(page []byte) uint32 { return binary.BigEndian.Uint32(page[filPagePrev:]) }
func filPageNo(page []byte) uint32   { return binary.BigEndian.Uint32(page[filPageOffset:]) }

// --- INDEX page header (PAGE_HEADER, starts at filPageData = 38) ---

const (
	pageHeader     = filPageData
	pageNDirSlots  = pageHeader + 0
	pageHeapTop    = pageHeader + 2
	pageNHeap      = pageHeader + 4
	pageFree       = pageHeader + 6
	pageGarbage    = pageHeader + 8
	pageLastInsert = pageHeader + 10
	pageDirection  = pageHeader + 12
	pageNDirection = pageHeader + 14
	pageNRecs      = pageHeader + 16
	pageMaxTrxID   = pageHeader + 18
	pageLevel      = pageHeader + 26
	pageIndexID    = pageHeader + 28 // 8 bytes
	pageBtrSegLeaf = pageHeader + 36 // 10-byte FSEG header
	pageBtrSegTop  = pageBtrSegLeaf + 10
	pageData       = pageHeader + 36 + 2*10 // 94

	recNNewExtraBytes = 5
	pageNewInfimum    = pageData + recNNewExtraBytes       // 99
	pageNewSupremum   = pageData + 2*recNNewExtraBytes + 8 // 112
)

func pageIsCompact(page []byte) bool {
	return binary.BigEndian.Uint16(page[pageNHeap:])&0x8000 != 0
}
func pageGetLevel(page []byte) uint16 { return binary.BigEndian.Uint16(page[pageLevel:]) }
func pageGetNRecs(page []byte) uint16 { return binary.BigEndian.Uint16(page[pageNRecs:]) }
func pageGetIndexID(page []byte) uint64 {
	return binary.BigEndian.Uint64(page[pageIndexID:])
}

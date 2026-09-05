// ibd-extractor - offline schema+data extraction from a MySQL 8.0/8.4 InnoDB
// .ibd file (one table per file, ROW_FORMAT=DYNAMIC/COMPACT/REDUNDANT,
// uncompressed or COMPRESSED - see zipdecompress.go).
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
	filPageTypeBlob     = 10 // REDUNDANT's off-page storage (simple chain, no LOB_FIRST framing)
	filPageTypeZBlob    = 11 // ROW_FORMAT=COMPRESSED's off-page storage (not supported - see lob.go)
	filPageTypeZBlob2   = 12
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

// MariaDB's "full_crc32" flags encoding (innodb_checksum_algorithm=
// full_crc32, the default since MariaDB 10.4) repurposes the low bits of
// FSP_SPACE_FLAGS entirely: bit 4 is a marker distinguishing this from the
// classic (MySQL-compatible, and MariaDB's own pre-10.4 default) layout
// above, and when set, bits 0-3 alone give the logical page size selector
// - there is no zip_ssize/KEY_BLOCK_SIZE field at all in this encoding
// (ROW_FORMAT=COMPRESSED doesn't exist under full_crc32; MariaDB's
// unrelated page_compression feature is a separate flag this tool doesn't
// need to read for an uncompressed page). Confirmed empirically against a
// real MariaDB 11.6 tablespace - including hand-verifying its checksum
// algorithm (a single CRC-32C over the whole page except its own last 4
// bytes - see checkPage) - before trusting any of this.
const (
	fspFlagsFCRC32PosPageSSize   = 0
	fspFlagsFCRC32WidthPageSSize = 4
	fspFlagsFCRC32PosMarker      = fspFlagsFCRC32PosPageSSize + fspFlagsFCRC32WidthPageSSize
	fspFlagsFCRC32WidthMarker    = 1
)

func fspFlagsField(flags uint32, pos, width uint32) uint32 {
	mask := ^(^uint32(0) << width)
	return (flags >> pos) & mask
}

// fspFlags is the decoded FSP_SPACE_FLAGS word.
type fspFlags struct {
	raw             uint32
	fullCRC32       bool   // MariaDB's full_crc32 flags encoding is in play - see the constants above
	fcrc32PageSSize uint32 // logical page size selector, full_crc32 encoding only
	zipSSize        uint32 // compressed page size selector (0 = not compressed) - classic encoding only
	pageSSize       uint32 // logical page size selector (0 = default 16K) - classic encoding only
	postAntelope    bool
	atomicBlobs     bool
	dataDir         bool
	shared          bool
	temporary       bool
	encryption      bool
	sdi             bool
}

func parseFSPFlags(raw uint32) fspFlags {
	if fullCRC32 := fspFlagsField(raw, fspFlagsFCRC32PosMarker, fspFlagsFCRC32WidthMarker) != 0; fullCRC32 {
		return fspFlags{
			raw:             raw,
			fullCRC32:       true,
			fcrc32PageSSize: fspFlagsField(raw, fspFlagsFCRC32PosPageSSize, fspFlagsFCRC32WidthPageSSize),
		}
	}
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
// For a ROW_FORMAT=COMPRESSED tablespace, PageSize (the *logical* page
// size, always what every other file in this tool means by "the page
// size") and PhysPageSize (the smaller zip_size actually on disk) differ;
// for an uncompressed one they're the same value. See zipdecompress.go.
type Space struct {
	f            *os.File
	PageSize     uint32 // logical page size
	PhysPageSize uint32 // on-disk page size (== PageSize unless Compressed)
	Compressed   bool
	NumPages     uint32
	SpaceID      uint32
	Flags        fspFlags
	page0        []byte // lazily cached by f0()
}

// OpenSpace opens path and determines its page size from the FSP header on
// page 0. Returns an error for encrypted/temporary tablespaces, which v1
// does not support.
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

	var pageSize, physPageSize uint32
	if flags.fullCRC32 {
		// See fspFlagsFCRC32PosMarker's doc comment: no zip_ssize field
		// exists at all in this encoding, so physical == logical always.
		// Unlike the classic encoding, 0 here means "invalid" rather than
		// "default 16K" (ssize 5 means that instead) - the same 512<<ssize
		// formula already rejects it, landing on 512, which the bounds
		// check just below refuses.
		pageSize = (1024 >> 1) << flags.fcrc32PageSSize
		physPageSize = pageSize
	} else {
		if flags.pageSSize == 0 {
			pageSize = 16384
		} else {
			pageSize = (1024 >> 1) << flags.pageSSize // 512 << ssize
		}
		physPageSize = pageSize
		if flags.zipSSize != 0 {
			physPageSize = (1024 >> 1) << flags.zipSSize // 512 << ssize
			if physPageSize < 1024 || physPageSize > pageSize {
				f.Close()
				return nil, fmt.Errorf("could not determine a valid compressed page size (flags=0x%08x)", rawFlags)
			}
		}
	}
	if pageSize < 4096 || pageSize > 65536 || (pageSize&(pageSize-1)) != 0 {
		f.Close()
		return nil, fmt.Errorf("could not determine a valid page size (flags=0x%08x)", rawFlags)
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
		f:            f,
		PageSize:     pageSize,
		PhysPageSize: physPageSize,
		Compressed:   flags.zipSSize != 0,
		NumPages:     uint32(fi.Size() / int64(physPageSize)),
		SpaceID:      spaceID,
		Flags:        flags,
	}, nil
}

func (s *Space) Close() error { return s.f.Close() }

// ReadPage returns the raw, physical, on-disk bytes of page pageNo -
// exactly what's stored there, at its native (zip_size, for a compressed
// tablespace) size. This is what checksum verification always runs
// against (checkPage), and it's the right and only sensible thing to hand
// back for any page type this tool doesn't decompress (FSP_HDR, XDES, a
// BLOB/LOB chain - none of which are laid out any differently just
// because the tablespace happens to be compressed). For a FIL_PAGE_INDEX
// or FIL_PAGE_SDI page in a compressed tablespace, though, callers need
// the *logical*, decompressed page instead - see Decompress.
func (s *Space) ReadPage(pageNo uint32) ([]byte, error) {
	buf := make([]byte, s.PhysPageSize)
	off := int64(pageNo) * int64(s.PhysPageSize)
	n, err := s.f.ReadAt(buf, off)
	if err != nil && n != len(buf) {
		return nil, fmt.Errorf("reading page %d: %w", pageNo, err)
	}
	return buf, nil
}

// Decompress turns a page's raw physical bytes (as returned by ReadPage)
// into the full logical page every other file in this tool expects,
// reconstructing it from its dense directory and compressed record data if
// this is a compressed tablespace's FIL_PAGE_INDEX or FIL_PAGE_SDI page -
// see zipdecompress.go - and returning raw unchanged for anything else
// (an uncompressed tablespace, or any other page type: FSP_HDR, XDES, a
// BLOB/LOB chain - none of which are laid out any differently just
// because the tablespace happens to be compressed).
//
// Every call site validates a page (checkPage, and usually also its
// FIL_PAGE_TYPE/index id) *before* calling this, using the fields that
// are always readable straight off the raw physical bytes because the
// page header is copied verbatim into the compressed format too - so
// decompression is only ever attempted on a page already known to be the
// right type and, where checked, the right index. The recover() here is
// still worth having: shape describes the caller's own expected index, and
// a page that passed every one of those checks yet still doesn't actually
// match it (a corrupt index id byte that happens to collide, say) could
// otherwise turn a bad file into a crash instead of a clean error.
func (s *Space) Decompress(raw []byte, shape zipIndexShape) (page []byte, err error) {
	if !s.Compressed {
		return raw, nil
	}
	switch filType(raw) {
	case filPageIndex, filPageSDI:
	default:
		return raw, nil
	}
	defer func() {
		if r := recover(); r != nil {
			page, err = nil, fmt.Errorf("decompressing page: panic: %v", r)
		}
	}()
	return decompressIndexPage(raw, s.PageSize, shape)
}

// ReadIndexPage is ReadPage+Decompress in one call, for the (few) call
// sites - all in sdi.go - that read a FIL_PAGE_SDI page without any
// checkPage/index-id validation of their own to sequence Decompress after
// (the SDI walk has never been corruption-hardened the way the target
// table's own clustered index is - see btree.go).
func (s *Space) ReadIndexPage(pageNo uint32, shape zipIndexShape) ([]byte, error) {
	raw, err := s.ReadPage(pageNo)
	if err != nil {
		return nil, err
	}
	return s.Decompress(raw, shape)
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

// checkPage validates page's checksum and LSN consistency. compressed
// tells it not to expect the redundant checksum+LSN trailer an
// uncompressed page's last 8 bytes always carry: a ROW_FORMAT=COMPRESSED
// page's last bytes are its own dense page directory instead (see
// zipdecompress.go), and its checksum is computed and verified by a
// different algorithm this tool does not implement - such a page is only
// checked for the "all zero" (unallocated) case, matching how the legacy
// "innodb" checksum algorithm is already left unverified (see this
// function's package doc comment, above checkPage's const block).
func checkPage(page []byte, compressed, fullCRC32 bool) pageCheck {
	n := len(page)
	if n < filPageData+filPageDataEnd {
		return pageCheck{Reason: "page is shorter than a valid FIL header+trailer"}
	}
	if fullCRC32 {
		// MariaDB's full_crc32 format: a single CRC-32C over the whole
		// page except its own last 4 bytes (where it's stored) - much
		// simpler than the classic split-checksum scheme below, and
		// unrelated to it; hand-verified against a real MariaDB 11.6
		// page before trusting it (see the FSP_FLAGS doc comment).
		if !bytes.Equal(page[filPageLSN+4:filPageLSN+8], page[n-8:n-4]) {
			return pageCheck{Reason: "LSN low bytes at the start and near-end of the page disagree"}
		}
		stored := binary.BigEndian.Uint32(page[n-4:])
		if stored == 0 {
			for _, b := range page {
				if b != 0 {
					return pageCheck{Reason: "checksum is zero but the page is not"}
				}
			}
			return pageCheck{OK: true, Empty: true}
		}
		if computed := crc32.Checksum(page[:n-4], crc32cTable); stored != computed {
			return pageCheck{Reason: fmt.Sprintf("checksum mismatch (stored %08x, computed crc32c %08x)", stored, computed)}
		}
		return pageCheck{OK: true}
	}
	if compressed {
		f1 := binary.BigEndian.Uint32(page[0:4])
		lsn8 := binary.BigEndian.Uint64(page[filPageLSN:])
		if f1 == 0 && lsn8 == 0 {
			for _, b := range page {
				if b != 0 {
					return pageCheck{Reason: "checksum and LSN fields are zero but the page is not"}
				}
			}
			return pageCheck{OK: true, Empty: true}
		}
		return pageCheck{OK: true}
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

	// REDUNDANT ("old-style") records use a 6-byte header instead of 5.
	recNOldExtraBytes = 6
	pageOldInfimum    = pageData + 1 + recNOldExtraBytes       // 101
	pageOldSupremum   = pageData + 2 + 2*recNOldExtraBytes + 8 // 116
)

func pageIsCompact(page []byte) bool {
	return binary.BigEndian.Uint16(page[pageNHeap:])&0x8000 != 0
}

// pageGetNHeap returns PAGE_N_HEAP with its high "is compact" bit masked
// off - the true count of heap records ever allocated on this page
// (infimum/supremum plus every user record, live or on the free list).
func pageGetNHeap(page []byte) uint16 {
	return binary.BigEndian.Uint16(page[pageNHeap:]) &^ 0x8000
}
func pageGetLevel(page []byte) uint16 { return binary.BigEndian.Uint16(page[pageLevel:]) }
func pageGetNRecs(page []byte) uint16 { return binary.BigEndian.Uint16(page[pageNRecs:]) }
func pageGetIndexID(page []byte) uint64 {
	return binary.BigEndian.Uint64(page[pageIndexID:])
}

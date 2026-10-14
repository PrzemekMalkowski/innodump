// rocksdb_sst.go: reading RocksDB's block-based table (.sst) files - the
// sorted, immutable files that hold nearly all of a RocksDB database's
// data (see rocksdb_db.go for how they combine with the WAL into one
// view).
//
// An SST file is a run of blocks, each followed by a 5-byte trailer (a
// compression-type byte and a 4-byte checksum of the block plus that byte):
//
//	[data block 0][data block 1]...[filter][index][range_del][properties]
//	[metaindex][footer]
//
// The footer (the last 53 bytes; 48 for format_version 0) locates the
// metaindex block, which maps meta block names ("rocksdb.properties",
// "rocksdb.range_del", and from format_version 6 "rocksdb.index") to their
// handles; the index block maps a separator key per data block to that
// block's handle. Every block (data, index, meta) shares one format: a
// series of prefix-compressed key/value entries followed by an array of
// "restart point" offsets.
//
// A data block's keys are RocksDB "internal keys": the user key followed by
// 8 bytes packing (sequence number << 8 | value type), little-endian.
//
// Corruption: every block's checksum is verified before it is used. A bad
// data block is reported and skipped; the index still says where the next
// one starts. A file whose footer, metaindex or index can't be read at all
// falls back to sstSalvageScan (rocksdb_salvage.go), which finds data
// blocks by walking the file from offset 0.
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

const (
	sstMagicBlockBased       = 0x88e241b785f4cff7
	sstMagicLegacyBlockBased = 0xdb4775248b80fb57
	sstMagicPlain            = 0x8242229663bf9564
	sstMagicLegacyPlain      = 0x4f3418eb7a8f13b8
	sstMagicCuckoo           = 0x926789d0c5f17873

	sstFooterLen   = 1 + 40 + 4 + 8 // kNewVersionsEncodedLength
	sstFooterLenV0 = 40 + 8         // kVersion0EncodedLength
	sstTrailerLen  = 5              // compression type + checksum
)

// RocksDB index types (BlockBasedTableOptions::IndexType).
const (
	sstIndexBinarySearch          = 0
	sstIndexHashSearch            = 1
	sstIndexTwoLevel              = 2
	sstIndexBinarySearchWithFirst = 3
)

type blockHandle struct {
	Off, Size uint64
}

func (h blockHandle) String() string { return fmt.Sprintf("offset %d, size %d", h.Off, h.Size) }

// indexEntry is one data block as the (flattened) index lists it.
type indexEntry struct {
	SepUserKey []byte // >= every user key in the block (comparator order), <= every key in the next
	H          blockHandle
}

// rangeTombstone is one DeleteRange: every key in [Start, End) whose
// sequence number is below Seq is deleted.
type rangeTombstone struct {
	Start, End []byte
	Seq        uint64
}

// sstFile is one opened SST file: its footer and meta blocks are read
// eagerly (they're small), its index only on first use (loadIndex).
type sstFile struct {
	Path   string
	Number uint64
	Size   int64
	f      *srcFile

	Legacy        bool // format_version 0 (legacy magic number)
	FormatVersion uint32
	ChecksumType  byte
	BaseContext   uint32
	metaindex     blockHandle
	indexHandle   blockHandle

	Props             map[string][]byte
	HaveCFID          bool
	CFID              uint32
	CFName            string
	Comparator        string
	IndexType         uint32
	IndexValueDelta   bool
	IndexKeyIsUserKey bool
	NumEntries        uint64
	External          bool   // written by SstFileWriter and ingested (MyRocks bulk load)
	GlobalSeqno       uint64 // for an external file: the seqno every key in it really has
	dict              []byte
	RangeDels         []rangeTombstone

	// OpenErr is set when the footer, metaindex or index can't be read;
	// such a file is only usable through sstSalvageScan.
	OpenErr error

	index     []indexEntry
	indexErr  error
	indexDone bool

	// Reverse is true for a column family using RocksDB's
	// ReverseBytewiseComparator (MyRocks' "rev:" column families).
	Reverse bool
}

// openSST opens path and reads its footer, metaindex, properties and
// range-deletion blocks. A structural problem doesn't fail the open: it's
// recorded in OpenErr (see sstFile), so the caller can still try to
// salvage the file's data blocks.
func openSST(path string) (*sstFile, error) {
	f, err := openSource(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	s := &sstFile{Path: path, Size: fi.Size(), f: f, Props: map[string][]byte{}}
	s.Number = sstNumberFromName(filepath.Base(path))
	if err := s.readFooter(); err != nil {
		s.OpenErr = err
		return s, nil
	}
	if err := s.readMeta(); err != nil {
		s.OpenErr = err
	}
	return s, nil
}

func (s *sstFile) Close() {
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
}

func (s *sstFile) Name() string { return filepath.Base(s.Path) }

// sstNumberFromName parses "000123.sst" (or .ldb) as file number 123.
func sstNumberFromName(name string) uint64 {
	var n uint64
	for _, c := range name {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + uint64(c-'0')
	}
	return n
}

func (s *sstFile) readAt(off int64, n int) ([]byte, error) {
	if off < 0 || n < 0 || off+int64(n) > s.Size {
		return nil, fmt.Errorf("range [%d, %d) is past the end of the file (%d bytes) - the file looks truncated", off, off+int64(n), s.Size)
	}
	buf := make([]byte, n)
	if _, err := s.f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil, err
	}
	return buf, nil
}

// readFooter is Footer::DecodeFrom.
func (s *sstFile) readFooter() error {
	if s.Size < sstFooterLenV0 {
		return fmt.Errorf("file is only %d bytes - too short to be an SST file", s.Size)
	}
	tail, err := s.readAt(s.Size-8, 8)
	if err != nil {
		return err
	}
	magic := binary.LittleEndian.Uint64(tail)
	switch magic {
	case sstMagicBlockBased:
	case sstMagicLegacyBlockBased:
		s.Legacy = true
	case sstMagicPlain, sstMagicLegacyPlain:
		return errors.New("this is a PlainTable-format SST file; only the block-based format (MyRocks' only format) is supported")
	case sstMagicCuckoo:
		return errors.New("this is a CuckooTable-format SST file; only the block-based format (MyRocks' only format) is supported")
	default:
		return fmt.Errorf("no SST footer magic number at the end of the file (found 0x%016x) - truncated or not an SST file", magic)
	}

	if s.Legacy {
		ft, err := s.readAt(s.Size-sstFooterLenV0, sstFooterLenV0)
		if err != nil {
			return err
		}
		s.ChecksumType = rdbCRC32c
		r := rdbReader{b: ft[:40]}
		s.metaindex = blockHandle{r.varint64("metaindex handle"), r.varint64("metaindex handle")}
		s.indexHandle = blockHandle{r.varint64("index handle"), r.varint64("index handle")}
		return r.err
	}

	if s.Size < sstFooterLen {
		return fmt.Errorf("file is only %d bytes - too short to be an SST file", s.Size)
	}
	footerOff := s.Size - sstFooterLen
	ft, err := s.readAt(footerOff, sstFooterLen)
	if err != nil {
		return err
	}
	s.FormatVersion = binary.LittleEndian.Uint32(ft[sstFooterLen-12:])
	if s.FormatVersion > 7 {
		return fmt.Errorf("corrupt or unsupported SST format_version %d", s.FormatVersion)
	}
	s.ChecksumType = ft[0]
	if s.ChecksumType > rdbXXH3 {
		return fmt.Errorf("corrupt or unsupported checksum type %d in the SST footer", s.ChecksumType)
	}
	if s.FormatVersion >= 6 {
		if !bytes.Equal(ft[1:5], []byte{0x3e, 0x00, 0x7a, 0x00}) {
			return fmt.Errorf("bad extended magic number in the SST footer")
		}
		stored := binary.LittleEndian.Uint32(ft[5:9])
		s.BaseContext = binary.LittleEndian.Uint32(ft[9:13])
		metaSize := binary.LittleEndian.Uint32(ft[13:17])
		if s.ChecksumType != rdbNoChecksum {
			cp := append([]byte(nil), ft...)
			binary.LittleEndian.PutUint32(cp[5:9], 0)
			computed := rdbChecksumWhole(s.ChecksumType, cp) + rdbChecksumModifier(s.BaseContext, uint64(footerOff))
			if computed != stored {
				return fmt.Errorf("SST footer checksum mismatch (stored 0x%08x, computed 0x%08x)", stored, computed)
			}
		}
		metaEnd := uint64(footerOff) - sstTrailerLen
		if uint64(metaSize) > metaEnd {
			return fmt.Errorf("SST footer's metaindex size %d is larger than the file", metaSize)
		}
		s.metaindex = blockHandle{metaEnd - uint64(metaSize), uint64(metaSize)}
		return nil
	}
	r := rdbReader{b: ft[1:41]}
	s.metaindex = blockHandle{r.varint64("metaindex handle"), r.varint64("metaindex handle")}
	s.indexHandle = blockHandle{r.varint64("index handle"), r.varint64("index handle")}
	return r.err
}

// readBlock reads, verifies and decompresses the block at h.
func (s *sstFile) readBlock(h blockHandle) ([]byte, error) {
	if h.Size > rdbMaxUncompressedSz {
		return nil, fmt.Errorf("block handle (%s) has an implausible size", h)
	}
	raw, err := s.readAt(int64(h.Off), int(h.Size)+sstTrailerLen)
	if err != nil {
		return nil, err
	}
	return s.decodeBlock(raw, h.Off, int(h.Size))
}

// decodeBlock verifies and decompresses one block whose raw bytes plus
// trailer are raw (block content is raw[:size]), found at file offset off.
func (s *sstFile) decodeBlock(raw []byte, off uint64, size int) ([]byte, error) {
	data, ctype := raw[:size], raw[size]
	if s.ChecksumType != rdbNoChecksum {
		stored := binary.LittleEndian.Uint32(raw[size+1:])
		computed := rdbBlockChecksum(s.ChecksumType, data, ctype)
		if s.FormatVersion >= 6 {
			computed += rdbChecksumModifier(s.BaseContext, off)
		}
		if stored != computed {
			return nil, fmt.Errorf("block checksum mismatch (%s, stored 0x%08x, computed 0x%08x)", rdbChecksumName(s.ChecksumType), stored, computed)
		}
	}
	out, err := rdbUncompress(ctype, data, s.FormatVersion, s.dict)
	if err != nil {
		return nil, fmt.Errorf("can't decompress %s block: %w", rdbCompressionName(ctype), err)
	}
	return out, nil
}

// readMeta reads the metaindex block, then the properties, compression
// dictionary and range-deletion blocks it points to.
func (s *sstFile) readMeta() error {
	mi, err := s.readBlock(s.metaindex)
	if err != nil {
		return fmt.Errorf("metaindex block: %w", err)
	}
	handles := map[string]blockHandle{}
	it := newBlockIter(mi, false)
	for it.next() {
		r := rdbReader{b: it.val}
		h := blockHandle{r.varint64("meta handle"), r.varint64("meta handle")}
		if r.err == nil {
			handles[string(it.key)] = h
		}
	}
	if it.err != nil {
		return fmt.Errorf("metaindex block: %w", it.err)
	}

	if h, ok := handles["rocksdb.properties"]; ok {
		pb, err := s.readBlock(h)
		if err != nil {
			return fmt.Errorf("properties block: %w", err)
		}
		pit := newBlockIter(pb, false)
		for pit.next() {
			s.Props[string(pit.key)] = append([]byte(nil), pit.val...)
		}
		if pit.err != nil {
			return fmt.Errorf("properties block: %w", pit.err)
		}
	} else if h, ok := handles["rocksdb.stats"]; ok { // pre-2014 name
		if pb, err := s.readBlock(h); err == nil {
			pit := newBlockIter(pb, false)
			for pit.next() {
				s.Props[string(pit.key)] = append([]byte(nil), pit.val...)
			}
		}
	}
	s.applyProps()

	if h, ok := handles["rocksdb.compression_dict"]; ok {
		d, err := s.readBlock(h)
		if err != nil {
			return fmt.Errorf("compression dictionary block: %w", err)
		}
		s.dict = d
	}

	if s.FormatVersion >= 6 || s.indexHandle.Size == 0 {
		h, ok := handles["rocksdb.index"]
		if !ok {
			return errors.New("metaindex has no rocksdb.index entry")
		}
		s.indexHandle = h
	}

	if h, ok := handles["rocksdb.range_del"]; ok {
		rb, err := s.readBlock(h)
		if err != nil {
			return fmt.Errorf("range deletion block: %w", err)
		}
		rit := newBlockIter(rb, false)
		for rit.next() {
			uk, seq, typ, ok := splitInternalKey(rit.key)
			if !ok || typ != rdbTypeRangeDeletion {
				continue
			}
			if s.External {
				seq = s.GlobalSeqno
			}
			s.RangeDels = append(s.RangeDels, rangeTombstone{
				Start: append([]byte(nil), uk...),
				End:   append([]byte(nil), rit.val...),
				Seq:   seq,
			})
		}
		if rit.err != nil {
			return fmt.Errorf("range deletion block: %w", rit.err)
		}
	}
	return nil
}

func (s *sstFile) propVarint(name string) (uint64, bool) {
	v, ok := s.Props[name]
	if !ok {
		return 0, false
	}
	x, n := rdbVarint64(v)
	return x, n > 0
}

func (s *sstFile) applyProps() {
	if v, ok := s.propVarint("rocksdb.column.family.id"); ok && v != 0x7FFFFFFF {
		// TablePropertiesCollectorFactory::Context::kUnknownColumnFamily
		// is port::kMaxInt32.
		s.HaveCFID, s.CFID = true, uint32(v)
	}
	s.CFName = string(s.Props["rocksdb.column.family.name"])
	s.Comparator = string(s.Props["rocksdb.comparator"])
	s.Reverse = isReverseComparator(s.Comparator)
	if v, ok := s.Props["rocksdb.block.based.table.index.type"]; ok && len(v) >= 4 {
		s.IndexType = binary.LittleEndian.Uint32(v)
	}
	if v, ok := s.propVarint("rocksdb.index.value.is.delta.encoded"); ok {
		s.IndexValueDelta = v != 0
	}
	if v, ok := s.propVarint("rocksdb.index.key.is.user.key"); ok {
		s.IndexKeyIsUserKey = v != 0
	}
	if v, ok := s.propVarint("rocksdb.num.entries"); ok {
		s.NumEntries = v
	}
	if v, ok := s.Props["rocksdb.external_sst_file.version"]; ok && len(v) >= 4 {
		s.External = true
		if gs, ok := s.Props["rocksdb.external_sst_file.global_seqno"]; ok && len(gs) >= 8 {
			s.GlobalSeqno = binary.LittleEndian.Uint64(gs)
		}
	}
}

// loadIndex reads and flattens the index (both levels of a partitioned
// one) into s.index; cached.
func (s *sstFile) loadIndex() ([]indexEntry, error) {
	if s.indexDone {
		return s.index, s.indexErr
	}
	s.indexDone = true
	if s.OpenErr != nil {
		s.indexErr = s.OpenErr
		return nil, s.indexErr
	}
	top, err := s.readBlock(s.indexHandle)
	if err != nil {
		s.indexErr = fmt.Errorf("index block: %w", err)
		return nil, s.indexErr
	}
	entries, err := s.parseIndexBlock(top)
	if err != nil {
		s.indexErr = fmt.Errorf("index block: %w", err)
		return nil, s.indexErr
	}
	if s.IndexType == sstIndexTwoLevel {
		var flat []indexEntry
		for _, part := range entries {
			pb, err := s.readBlock(part.H)
			if err != nil {
				s.indexErr = fmt.Errorf("index partition (%s): %w", part.H, err)
				return nil, s.indexErr
			}
			pe, err := s.parseIndexBlock(pb)
			if err != nil {
				s.indexErr = fmt.Errorf("index partition (%s): %w", part.H, err)
				return nil, s.indexErr
			}
			flat = append(flat, pe...)
		}
		entries = flat
	}
	s.index = entries
	return entries, nil
}

// parseIndexBlock decodes one index block (or index partition) - see
// IndexBlockIter::DecodeCurrentValue for the value-delta encoding.
func (s *sstFile) parseIndexBlock(b []byte) ([]indexEntry, error) {
	it := newBlockIter(b, s.IndexValueDelta)
	var out []indexEntry
	var prev blockHandle
	havePrev := false
	for it.next() {
		r := rdbReader{b: it.val}
		var h blockHandle
		if s.IndexValueDelta && it.shared > 0 && havePrev {
			delta, n := binary.Varint(r.b)
			if n <= 0 {
				return nil, errors.New("bad delta-encoded index value")
			}
			r.b = r.b[n:]
			h = blockHandle{prev.Off + prev.Size + sstTrailerLen, uint64(int64(prev.Size) + delta)}
		} else {
			h = blockHandle{r.varint64("index handle"), r.varint64("index handle")}
		}
		if s.IndexType == sstIndexBinarySearchWithFirst {
			r.lpSlice("first key")
		}
		if r.err != nil {
			return nil, r.err
		}
		if s.IndexValueDelta {
			it.setValueLen(len(it.val) - len(r.b))
		}
		key := it.key
		if !s.IndexKeyIsUserKey {
			if len(key) < 8 {
				return nil, errors.New("index key shorter than an internal key")
			}
			key = key[:len(key)-8]
		}
		out = append(out, indexEntry{SepUserKey: append([]byte(nil), key...), H: h})
		prev, havePrev = h, true
	}
	if it.err != nil {
		return nil, it.err
	}
	return out, nil
}

// --- block entries ---

// blockIter walks one block's entries in order (Block::NewDataIterator's
// sequential case). valueDelta selects format_version 4+'s index-block
// entry layout, which omits the value length (the caller decodes the
// value and reports its length back with setValueLen).
type blockIter struct {
	data       []byte
	restarts   int // offset of the restart array = end of the entries
	pos        int
	key        []byte
	val        []byte
	shared     int
	vstart     int
	valueDelta bool
	err        error
}

// setValueLen tells a valueDelta iterator how long the entry's value it
// just decoded was, so next() knows where the following entry starts.
func (it *blockIter) setValueLen(n int) {
	it.val = it.data[it.vstart : it.vstart+n]
	it.pos = it.vstart + n
}

func newBlockIter(b []byte, valueDelta bool) *blockIter {
	it := &blockIter{data: b, valueDelta: valueDelta}
	r, err := blockRestartsOffset(b)
	if err != nil {
		it.err = err
		return it
	}
	it.restarts = r
	return it
}

// blockRestartsOffset finds where a block's entries end: its restart
// array, which may be followed by a data block hash index
// (kDataBlockBinaryAndHash) before the final num_restarts word.
func blockRestartsOffset(b []byte) (int, error) {
	if len(b) < 4 {
		return 0, errors.New("block shorter than its own restart count")
	}
	footer := binary.LittleEndian.Uint32(b[len(b)-4:])
	numRestarts := footer
	end := len(b) - 4
	if len(b) <= 1<<16 && footer&(1<<31) != 0 {
		numRestarts = footer & 0x7FFFFFFF
		if end < 2 {
			return 0, errors.New("block too short for its hash index")
		}
		numBuckets := int(binary.LittleEndian.Uint16(b[end-2:]))
		end = end - 2 - numBuckets
		if end < 0 {
			return 0, errors.New("block too short for its hash index")
		}
	}
	r := end - int(numRestarts)*4
	if numRestarts == 0 || r < 0 || r > end {
		return 0, fmt.Errorf("bad restart count %d for a %d-byte block", numRestarts, len(b))
	}
	return r, nil
}

func (it *blockIter) next() bool {
	if it.err != nil || it.pos >= it.restarts {
		return false
	}
	p := it.pos
	lim := it.restarts
	var shared, nonShared, valLen uint32
	var n int
	if shared, n = rdbVarint32(it.data[p:lim]); n == 0 {
		it.err = fmt.Errorf("bad entry header at block offset %d", p)
		return false
	}
	p += n
	if nonShared, n = rdbVarint32(it.data[p:lim]); n == 0 {
		it.err = fmt.Errorf("bad entry header at block offset %d", p)
		return false
	}
	p += n
	if !it.valueDelta {
		if valLen, n = rdbVarint32(it.data[p:lim]); n == 0 {
			it.err = fmt.Errorf("bad entry header at block offset %d", p)
			return false
		}
		p += n
	}
	if int(shared) > len(it.key) || p+int(nonShared) > lim {
		it.err = fmt.Errorf("bad entry at block offset %d (shared %d, non-shared %d)", it.pos, shared, nonShared)
		return false
	}
	key := make([]byte, int(shared)+int(nonShared))
	copy(key, it.key[:shared])
	copy(key[shared:], it.data[p:p+int(nonShared)])
	p += int(nonShared)
	it.key = key
	it.shared = int(shared)
	if it.valueDelta {
		it.val = it.data[p:lim]
		it.pos = lim // until setValueLen says otherwise
		it.vstart = p
		return true
	}
	if p+int(valLen) > lim {
		it.err = fmt.Errorf("entry value at block offset %d runs past the block's entries", p)
		return false
	}
	it.val = it.data[p : p+int(valLen)]
	it.pos = p + int(valLen)
	return true
}

// --- internal keys ---

// RocksDB ValueType (db/dbformat.h).
const (
	rdbTypeDeletion                 = 0x0
	rdbTypeValue                    = 0x1
	rdbTypeMerge                    = 0x2
	rdbTypeLogData                  = 0x3
	rdbTypeCFDeletion               = 0x4
	rdbTypeCFValue                  = 0x5
	rdbTypeCFMerge                  = 0x6
	rdbTypeSingleDeletion           = 0x7
	rdbTypeCFSingleDeletion         = 0x8
	rdbTypeBeginPrepareXID          = 0x9
	rdbTypeEndPrepareXID            = 0xA
	rdbTypeCommitXID                = 0xB
	rdbTypeRollbackXID              = 0xC
	rdbTypeNoop                     = 0xD
	rdbTypeCFRangeDeletion          = 0xE
	rdbTypeRangeDeletion            = 0xF
	rdbTypeCFBlobIndex              = 0x10
	rdbTypeBlobIndex                = 0x11
	rdbTypeBeginPersistedPrepareXID = 0x12
	rdbTypeBeginUnprepareXID        = 0x13
	rdbTypeDeletionWithTimestamp    = 0x14
	rdbTypeCommitXIDAndTimestamp    = 0x15
	rdbTypeWideColumnEntity         = 0x16
	rdbTypeCFWideColumnEntity       = 0x17
	rdbTypeValuePreferredSeqno      = 0x18
	rdbTypeCFValuePreferredSeqno    = 0x19
)

// splitInternalKey splits an internal key into user key, sequence number
// and value type.
func splitInternalKey(ik []byte) (userKey []byte, seq uint64, typ byte, ok bool) {
	if len(ik) < 8 {
		return nil, 0, 0, false
	}
	n := len(ik) - 8
	packed := binary.LittleEndian.Uint64(ik[n:])
	return ik[:n], packed >> 8, byte(packed), true
}

// isReverseComparator recognizes a reverse-bytewise comparator: RocksDB's
// own, or MyRocks' Rdb_rev_comparator ("rev:RocksDB_SE_v3.10"), which
// every "rev:" column family uses.
func isReverseComparator(name string) bool {
	return name == "rocksdb.ReverseBytewiseComparator" || strings.HasPrefix(name, "rev:")
}

// compareUserKeys orders two user keys the way the column family's
// comparator does: plain bytewise, or reversed for a "rev:" column family.
func compareUserKeys(a, b []byte, reverse bool) int {
	c := bytes.Compare(a, b)
	if reverse {
		return -c
	}
	return c
}

// firstBlockFor returns the index of the first data block that can hold a
// key >= from (comparator order): every earlier block's separator is
// below from.
func firstBlockFor(index []indexEntry, from []byte, reverse bool) int {
	if from == nil {
		return 0
	}
	return sort.Search(len(index), func(i int) bool {
		return compareUserKeys(index[i].SepUserKey, from, reverse) >= 0
	})
}

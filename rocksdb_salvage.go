// rocksdb_salvage.go: recovering data blocks from an SST file whose
// footer, metaindex or index can't be used - a truncated copy, an
// overwritten tail, a damaged index block - by walking the file from
// offset 0 instead of following its index.
//
// Data blocks are written back to back from offset 0, each followed by
// its 5-byte trailer, but nothing records a block's length except the
// index. So at each offset the scan works out where a block would have
// to end - for an uncompressed block, where a plausible restart array
// closes its entries; for a compressed one, how much input the
// decompressor consumed producing the uncompressed size the block
// declares - and accepts that end only if the trailer's checksum matches
// (with format_version 6, also the file's per-offset context). Where no
// block can be found (a damaged stretch), it searches forward for the
// next offset where one can. The scan stops at the first block that
// isn't a data block (the index, which lists the blocks already found)
// or at the end of the file.
package main

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"os"
	"sort"
)

const (
	salvageMaxBlock  = 8 << 20 // largest block a candidate may be
	salvageResyncMax = 64 << 20
)

type salvager struct {
	data       []byte
	ckTypes    []byte
	fv         uint32 // format_version guess for compression framing
	fvKnown    bool
	base       uint32 // format_version 6 context base; 0 = unknown
	baseKnown  bool
	ctx        bool // format_version >= 6 (or unknown: try both)
	found      map[uint64]bool
	dict       []byte
	lastCkType byte
}

// salvageIssue is one thing salvaging a file found: a damaged stretch
// (with the key range it may have held), or just the fact that the file
// had to be salvaged.
type salvageIssue struct {
	Lo, Hi    []byte // key range affected (comparator order; nil = unbounded)
	Global    bool   // concerns every key in the file
	Off       uint64
	Reason    string
	Info      bool
	Truncated bool
}

// salvageSST returns every entry recoverable from ref's file, in file
// order (which is key order: data blocks are written sorted), recording
// what it couldn't read in ref.salvageIssues. Done once per file.
func (db *rdbDB) salvageSST(ref *rdbSSTRef, why error) []rdbEntry {
	if ref.salvageDone {
		return ref.salvaged
	}
	ref.salvageDone = true
	s := ref.sst
	issue := func(is salvageIssue) { ref.salvageIssues = append(ref.salvageIssues, is) }
	data, err := os.ReadFile(s.Path)
	if err != nil {
		issue(salvageIssue{Global: true, Reason: fmt.Sprintf("can't use the file's index (%v) and can't read it for salvaging: %v", why, err)})
		return nil
	}
	sv := &salvager{data: data, found: map[uint64]bool{}, dict: s.dict}
	if s.OpenErr == nil || s.ChecksumType != 0 || s.FormatVersion != 0 {
		// The footer was readable (only the index is bad): trust it.
		sv.ckTypes = []byte{s.ChecksumType}
		sv.fv, sv.fvKnown = s.FormatVersion, true
		sv.ctx = s.FormatVersion >= 6
		if sv.ctx {
			sv.base, sv.baseKnown = s.BaseContext, true
		}
	}
	if !sv.fvKnown {
		sv.ckTypes = []byte{rdbXXH3, rdbCRC32c, rdbXXHash64, rdbXXHash, rdbNoChecksum}
		sv.fv = 2
		sv.ctx = true // tried with and without context, see verify
	}
	issue(salvageIssue{Global: true, Info: true, Reason: fmt.Sprintf("can't use the file's index (%v) - recovered its data blocks by scanning the file instead (each one checksum-verified)", why)})

	var out []rdbEntry
	pos := 0
	nBlocks := 0
	lastKey := func() []byte {
		if len(out) > 0 {
			return out[len(out)-1].UserKey
		}
		return nil
	}
	for pos < len(data) {
		blk, end, ok := sv.blockAt(pos)
		if ok {
			ents, isData := sv.dataEntries(blk, uint64(pos), s)
			if !isData {
				break // index or meta block: the data blocks are over
			}
			sv.found[uint64(pos)] = true
			for i := range ents {
				ents[i].Origin = entryOrigin{File: s.Name(), Off: int64(pos), Kind: originSalvaged}
			}
			out = append(out, ents...)
			nBlocks++
			pos = end + sstTrailerLen
			continue
		}
		next := sv.resync(pos + 1)
		var nextData []rdbEntry
		if next >= 0 {
			if b, _, ok := sv.blockAt(next); ok {
				if ents, isData := sv.dataEntries(b, uint64(next), s); isData {
					nextData = ents
				}
			}
		}
		if next < 0 || nextData == nil {
			// Nothing but non-data blocks (or nothing at all) from here on.
			switch {
			case nBlocks == 0:
				issue(salvageIssue{Global: true, Off: uint64(pos), Reason: "no data block could be recovered from this file"})
			case s.OpenErr != nil && !looksLikeTail(data[pos:]):
				// No footer: the file was cut short (or its tail overwritten)
				// somewhere after the last block found.
				issue(salvageIssue{Lo: lastKey(), Off: uint64(pos), Truncated: true,
					Reason: fmt.Sprintf("the file ends (at byte %d) without its index and footer - it looks truncated (not fully copied); rows stored after byte %d are lost", len(data), pos)})
			}
			break
		}
		issue(salvageIssue{Lo: lastKey(), Hi: nextData[0].UserKey, Off: uint64(pos),
			Reason: fmt.Sprintf("bytes %d-%d are damaged (no valid block found there) - rows stored there are lost", pos, next)})
		pos = next
	}
	if !s.External && ref.HaveSeqRange && ref.SmallestSeq == ref.LargestSeq && ref.LargestSeq > 0 {
		// Most likely an ingested (bulk-loaded) file whose properties are
		// gone with its footer: its keys all carry seqno 0, the real one
		// being what the MANIFEST recorded for the whole file.
		allZero := true
		for _, e := range out {
			if e.Seq != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			for i := range out {
				out[i].Seq = ref.LargestSeq
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if c := compareUserKeys(out[i].UserKey, out[j].UserKey, s.Reverse); c != 0 {
			return c < 0
		}
		return out[i].Seq > out[j].Seq
	})
	ref.salvaged = out
	return out
}

// looksLikeTail reports whether b (what's left after the last data block
// found) is plausibly just an SST's filter/index/meta blocks and footer,
// rather than lost data - it ends in a footer magic number.
func looksLikeTail(b []byte) bool {
	if len(b) < 8 {
		return false
	}
	m := binary.LittleEndian.Uint64(b[len(b)-8:])
	return m == sstMagicBlockBased || m == sstMagicLegacyBlockBased
}

// resync finds the next offset at or after from where a block validates.
func (sv *salvager) resync(from int) int {
	limit := len(sv.data)
	if from+salvageResyncMax < limit {
		limit = from + salvageResyncMax
	}
	for p := from; p < limit-sstTrailerLen; p++ {
		if _, _, ok := sv.blockAt(p); ok {
			return p
		}
	}
	return -1
}

// verify checks the trailer at data[end:end+5] for the block data[pos:end].
func (sv *salvager) verify(pos, end int) bool {
	if end+sstTrailerLen > len(sv.data) {
		return false
	}
	blk, ctype := sv.data[pos:end], sv.data[end]
	stored := binary.LittleEndian.Uint32(sv.data[end+1:])
	for _, ck := range sv.ckTypes {
		if ck == rdbNoChecksum {
			if stored == 0 {
				sv.lastCkType = ck
				return true
			}
			continue
		}
		c := rdbBlockChecksum(ck, blk, ctype)
		if !sv.ctx {
			if c == stored {
				sv.lastCkType = ck
				return true
			}
			continue
		}
		if sv.baseKnown {
			if c+rdbChecksumModifier(sv.base, uint64(pos)) == stored {
				sv.lastCkType = ck
				return true
			}
			continue
		}
		// Unknown format_version: accept a context-free match outright;
		// a format_version 6 file's blocks imply a base that must be the
		// same for every block - take the first block's, then insist.
		if c == stored {
			sv.lastCkType = ck
			return true
		}
		// Only the very first data block (offset 0) may establish it.
		implied := (stored - c) ^ (uint32(pos) + uint32(uint64(pos)>>32))
		if pos == 0 && implied != 0 {
			sv.base, sv.baseKnown = implied, true
			sv.lastCkType = ck
			sv.ckTypes = []byte{ck}
			return true
		}
	}
	return false
}

// blockAt tries to find a whole valid block starting at pos, returning
// its uncompressed contents and where its trailer starts.
func (sv *salvager) blockAt(pos int) (blk []byte, end int, ok bool) {
	d := sv.data
	if pos+sstTrailerLen+4 > len(d) {
		return nil, 0, false
	}
	// A compressed block, format_version >= 2: varint32 size prefix.
	if usz, n := rdbVarint32(d[pos:]); n > 0 && usz > 0 && usz <= salvageMaxBlock {
		body := d[pos+n:]
		if len(body) > salvageMaxBlock+64 {
			body = body[:salvageMaxBlock+64]
		}
		// LZ4: decoding stops exactly when usz bytes are produced.
		if out, used, err := lz4DecodeBlock(body, int(usz), sv.dict); err == nil {
			e := pos + n + used
			if e < len(d) && (d[e] == rdbLZ4 || d[e] == rdbLZ4HC) && sv.verify(pos, e) {
				return out, e, true
			}
		}
		// ZSTD: the frame's own block headers say where it ends.
		if fl := zstdFrameLength(body); fl > 0 {
			e := pos + n + fl
			if e < len(d) && (d[e] == rdbZSTD || d[e] == rdbZSTDNotFinal) && sv.verify(pos, e) {
				if out, err := rdbUncompress(d[e], d[pos:e], 2, sv.dict); err == nil {
					return out, e, true
				}
			}
		}
		// Zlib (raw deflate): flate reads byte by byte from a bytes.Reader,
		// so its position after the final block is the stream's end.
		if e, ok := deflateEnd(body, int(usz)); ok {
			e += pos + n
			if e < len(d) && d[e] == rdbZlib && sv.verify(pos, e) {
				if out, err := rdbUncompress(d[e], d[pos:e], 2, sv.dict); err == nil {
					return out, e, true
				}
			}
		}
	}
	// Snappy carries its own length prefix.
	if out, used, err := snappyDecode(d[pos:min(len(d), pos+salvageMaxBlock+64)]); err == nil && len(out) > 0 {
		e := pos + used
		if e < len(d) && d[e] == rdbSnappy && sv.verify(pos, e) {
			return out, e, true
		}
	}
	// Uncompressed: walk entries; after each, see whether a restart array
	// closing the block could start right there.
	return sv.uncompressedAt(pos)
}

func (sv *salvager) uncompressedAt(pos int) ([]byte, int, bool) {
	d := sv.data
	limit := len(d)
	if pos+salvageMaxBlock < limit {
		limit = pos + salvageMaxBlock
	}
	p := pos
	var prevKey []byte
	first := true
	for p < limit {
		// Try: the restart array starts at p.
		if !first {
			if e, ok := restartArrayEnd(d, pos, p, limit); ok && e < len(d) && d[e] == rdbNoCompression && sv.verify(pos, e) {
				return d[pos:e], e, true
			}
		}
		// Otherwise p must be another entry.
		shared, n1 := rdbVarint32(d[p:limit])
		if n1 == 0 {
			return nil, 0, false
		}
		nonShared, n2 := rdbVarint32(d[p+n1 : limit])
		if n2 == 0 {
			return nil, 0, false
		}
		vlen, n3 := rdbVarint32(d[p+n1+n2 : limit])
		if n3 == 0 {
			return nil, 0, false
		}
		if first && shared != 0 || int(shared) > len(prevKey) || nonShared > salvageMaxBlock || vlen > salvageMaxBlock {
			return nil, 0, false
		}
		q := p + n1 + n2 + n3 + int(nonShared) + int(vlen)
		if q > limit {
			return nil, 0, false
		}
		key := append(append([]byte(nil), prevKey[:shared]...), d[p+n1+n2+n3:p+n1+n2+n3+int(nonShared)]...)
		prevKey = key
		first = false
		p = q
	}
	return nil, 0, false
}

// restartArrayEnd checks whether a restart array beginning at p closes a
// block that began at pos, returning where the block ends: offsets
// starting at 0, increasing, below p-pos, then the count itself.
func restartArrayEnd(d []byte, pos, p, limit int) (int, bool) {
	if p+8 > limit {
		return 0, false
	}
	if binary.LittleEndian.Uint32(d[p:]) != 0 {
		return 0, false
	}
	prev := uint32(0)
	for k := 1; p+4*k+4 <= limit; k++ {
		v := binary.LittleEndian.Uint32(d[p+4*k:])
		if v == uint32(k) {
			return p + 4*k + 4, true
		}
		if v <= prev || int(v) >= p-pos {
			return 0, false
		}
		prev = v
	}
	return 0, false
}

// deflateEnd decodes a raw deflate stream from b, returning how many bytes
// it took, if it inflates to exactly want bytes.
func deflateEnd(b []byte, want int) (int, bool) {
	r := bytes.NewReader(b)
	fr := flate.NewReader(r)
	buf := make([]byte, want+1)
	n := 0
	for n < len(buf) {
		k, err := fr.Read(buf[n:])
		n += k
		if err != nil {
			break
		}
	}
	if n != want {
		return 0, false
	}
	return len(b) - r.Len(), true
}

// dataEntries decodes a salvaged block's entries, or reports that it
// isn't a data block (every key must be an internal key with a valid
// value type; an index block - whose values point at blocks already
// found - ends the data area).
func (sv *salvager) dataEntries(blk []byte, off uint64, s *sstFile) ([]rdbEntry, bool) {
	it := newBlockIter(blk, false)
	var out []rdbEntry
	handles, allHandles := 0, true
	for it.next() {
		uk, seq, typ, ok := splitInternalKey(it.key)
		if !ok {
			return nil, false
		}
		switch typ {
		case rdbTypeDeletion, rdbTypeValue, rdbTypeMerge, rdbTypeSingleDeletion, rdbTypeBlobIndex,
			rdbTypeDeletionWithTimestamp, rdbTypeWideColumnEntity, rdbTypeValuePreferredSeqno:
		default:
			return nil, false
		}
		if allHandles {
			if o, n := rdbVarint64(it.val); n > 0 && sv.found[o] {
				if _, n2 := rdbVarint64(it.val[n:]); n2 > 0 && n+n2 == len(it.val) {
					handles++
				} else {
					allHandles = false
				}
			} else {
				allHandles = false
			}
		}
		if s.External {
			seq = s.GlobalSeqno
		}
		out = append(out, rdbEntry{UserKey: append([]byte(nil), uk...), Seq: seq, Type: typ, Value: append([]byte(nil), it.val...)})
	}
	if it.err != nil || len(out) == 0 {
		return nil, false
	}
	if allHandles && handles == len(out) {
		return nil, false // the index block
	}
	return out, true
}

// zipdecompress.go: reconstructs the full logical-size page for one
// ROW_FORMAT=COMPRESSED index page (FIL_PAGE_INDEX or FIL_PAGE_SDI) from
// its on-disk (zip_size) bytes, so every other file in this tool - which
// was written entirely in terms of an ordinary COMPACT/DYNAMIC page - can
// go on operating on it completely unchanged.
//
// Ported from storage/innobase/page/zipdecompress.cc (page_zip_decompress_
// low and its helpers) in the mysql-server source, with one deliberate
// simplification that this tool's read-only, single-index-at-a-time use
// case affords: the real server has to reconstruct a *dummy* dict_index
// purely from the page's own embedded, self-describing field-length list
// (page_zip_fields_decode), because it may not have the real table
// definition at hand (crash recovery, a generic page-dump tool, etc). This
// tool always does - either the SDI index's own fixed, hardcoded layout
// (sdi.go), or the target table's real column list, decoded from its SDI
// long before any of its data pages are ever read (schema.go) - so rather
// than re-derive a dummy index from that field-length list, this code only
// needs the list's *byte length* in the compressed stream (to skip past
// it), and reuses record.go's ordinary NULL-bitmap/variable-length-list
// decoder (decodeFieldRangesSpecial) for everything else, exactly as it's
// used for an uncompressed page.
//
// That equivalence also means every leaf/non-leaf page this tool ever
// actually decompresses is either the SDI index (always effectively
// "clustered": it carries its own DB_TRX_ID/DB_ROLL_PTR like any real
// table) or the target table's own clustered index - this tool never walks
// a secondary index for data - so the secondary-index-leaf variant
// (page_zip_decompress_sec) has no equivalent here at all.
//
// A page is reconstructed in three passes, mirroring page_zip_decompress_
// low exactly (the modification log exists precisely because MySQL avoids
// recompressing a page on every single write - so it is NOT a rare case:
// even a handful of ordinary inserts commonly leave most of a page's real
// row content there rather than in the main compressed image):
//
//  1. Inflate the zlib-compressed image into every record's placement
//     (from the dense directory), leaving DB_TRX_ID/DB_ROLL_PTR, each
//     externally-stored column's last 20 bytes, and (non-leaf) the node
//     pointer as zero placeholders - those are stored apart from both the
//     compressed image and the modification log (see pass 3).
//  2. Replay the modification log (zipApplyModLog), which overwrites
//     specific records' extra bytes and field data - using the exact same
//     placeholder convention - with whatever was written after the page
//     was last fully compressed.
//  3. Restore the real DB_TRX_ID/DB_ROLL_PTR/node-pointer/BLOB-reference
//     bytes every placeholder above was left expecting, from the page's
//     own dedicated storage/externs trailer (zipRestoreStorage) - this
//     has to run after pass 2, since a modification-log "clear" entry
//     zeroes a record's data outright and only the freshest placement
//     (whatever the dense directory - read straight off disk - currently
//     says) determines which records are even still live.
//
// Not implemented: external (off-page) columns, which a compressed
// tablespace stores in an entirely different page format (FIL_PAGE_TYPE_
// ZBLOB/ZBLOB2, lob0zip.h) that this tool doesn't read - see lob.go.
package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
)

// --- compressed-page constants (page0zip.h/.ic) ---
const (
	pageZipDirSlotSize  = 2
	pageZipDirSlotMask  = 0x3fff
	pageZipDirSlotOwned = 0x4000
	pageZipDirSlotDel   = 0x8000

	recNodePtrSize = 4 // child page number on a non-leaf record
	dataTrxIDLen   = 6
	dataRollPtrLen = 7
	trxRollLen     = dataTrxIDLen + dataRollPtrLen // 13: DB_TRX_ID+DB_ROLL_PTR, stored apart from the compressed stream/modification log

	// PAGE_NEW_SUPREMUM_END: one past the (fixed, 8-byte, un-compressed)
	// supremum pseudo-record - where the compressed byte stream's target
	// output position starts.
	pageZipStart = pageNewSupremum + 8
)

var (
	infimumExtra = [3]byte{0x01, 0x00, 0x02} // info_bits=0,n_owned=1; heap_no=0,status=INFIMUM
	infimumData  = [8]byte{0x69, 0x6e, 0x66, 0x69, 0x6d, 0x75, 0x6d, 0x00}
	// heap_no=1,status=SUPREMUM; next=0; "supremum" - written starting one
	// byte into the header (the info_bits/n_owned byte is set separately).
	supremumExtraData = [12]byte{0x00, 0x0b, 0x00, 0x00, 0x73, 0x75, 0x70, 0x72, 0x65, 0x6d, 0x75, 0x6d}
)

// zipIndexShape is everything decompressIndexPage needs to know about one
// index beyond the raw page bytes: see the file comment for why this is
// enough (in place of the server's from-scratch dummy-index reconstruction).
type zipIndexShape struct {
	LeafFields    []*IndexField
	LeafNNullable int
	TrxIDIdx      int           // index into LeafFields of DB_TRX_ID - always clustered, see file comment
	NonLeafFields []*IndexField // key columns + a 4-byte node-pointer field
}

// zipTrxRollField is DB_TRX_ID and DB_ROLL_PTR's field-info-block entry:
// on a compressed page's leaf record, these two system columns are always
// described as a single fused fixed-length(13), not-null field - confirmed
// empirically (a real page's field-info block byte-for-byte matches this,
// and only this - see the file comment), not merely inferred from
// PAGE_ZIP_CLUST_LEAF_SLOT_SIZE's similar-looking pairing elsewhere. This
// is fine for zipFillRecordData/zipRestoreStorage as-is: they only ever
// skip/restore the first trxRollLen bytes of shape.TrxIDIdx's field and let
// the next field's own gap-fill pick up anything past that - which for a
// real table's DB_TRX_ID+DB_ROLL_PTR is nothing (this field is exactly the
// 13 bytes), but for the SDI index specifically (see sdiZipShape) is not.
var zipTrxRollField = &IndexField{Col: &Column{}, EffFixedLen: trxRollLen}

// tableZipShape builds the zipIndexShape for a table's own clustered index.
func tableZipShape(t *Table) zipIndexShape {
	trxIdx := -1
	leaf := make([]*IndexField, 0, len(t.PhysicalFields))
	for _, f := range t.PhysicalFields {
		switch f.Col.Name {
		case "DB_TRX_ID":
			trxIdx = len(leaf)
			leaf = append(leaf, zipTrxRollField)
		case "DB_ROLL_PTR":
			// already folded into zipTrxRollField above
		default:
			leaf = append(leaf, f)
		}
	}
	nUniq := len(t.PKFields)
	if !t.HasExplicitPK {
		nUniq = 1
	}
	keyFields := t.PhysicalFields[:nUniq] // never includes DB_TRX_ID/DB_ROLL_PTR - no fusing needed
	nonLeaf := append(append([]*IndexField(nil), keyFields...),
		&IndexField{Col: &Column{FixedLen: recNodePtrSize}, EffFixedLen: recNodePtrSize})
	return zipIndexShape{
		LeafFields:    leaf,
		LeafNNullable: t.NNullable,
		TrxIDIdx:      trxIdx,
		NonLeafFields: nonLeaf,
	}
}

// sdiZipShape is the SDI index's own fixed layout (see sdi.go's sdiOff*
// constants) expressed as a zipIndexShape. SDI has no nullable columns, so
// its field lengths alone (via IndexField.EffFixedLen/isBigCol) are
// everything decodeFieldRangesSpecial needs - but unlike a real table, its
// field-info block also fuses adjacent not-null fixed-length columns
// beyond just DB_TRX_ID+DB_ROLL_PTR (empirically confirmed: TYPE+ID as one
// 12-byte field, then DB_TRX_ID+DB_ROLL_PTR+UNCOMPRESSED_LEN+COMPRESSED_LEN
// as one 21-byte field - possibly because MySQL's own SDI index definition
// genuinely declares its key as a single binary column rather than the two
// typed ones sdi.go's *uncompressed*-page reading treats it as, which
// produces byte-identical results either way and was never put to the
// test until this - see the file comment for the fixed-point byte trace).
var sdiZipShape = zipIndexShape{
	LeafFields: []*IndexField{
		{Col: &Column{}, EffFixedLen: 12},               // type+id
		{Col: &Column{}, EffFixedLen: 21},               // trx_id+roll_ptr+uncompressed_len+compressed_len (only the first 13 bytes are the placeholder trx_id_col span)
		{Col: &Column{ColLen: 0x7fff, Mtype: dataBlob}}, // data (variable, external-capable)
	},
	LeafNNullable: 0,
	TrxIDIdx:      1,
	NonLeafFields: []*IndexField{
		{Col: &Column{}, EffFixedLen: 12},                                     // type+id
		{Col: &Column{FixedLen: recNodePtrSize}, EffFixedLen: recNodePtrSize}, // child page
	},
}

// fields returns the field list and NULL-bitmap size for a leaf or
// non-leaf record. Confirmed empirically: a non-leaf record's NULL bitmap
// is sized off the *whole index's* nullable-column count (LeafNNullable),
// not off however many of ITS OWN (key + node-pointer) columns happen to
// be nullable (none ever are - PRIMARY KEY columns are always NOT NULL) -
// the encode/decode side reuses one dict_index definition, and hence one
// n_nullable, for both a page's leaf and non-leaf records alike, even
// though the reserved bitmap bits go completely unused on a non-leaf one.
func (s zipIndexShape) fields(isLeaf bool) ([]*IndexField, int) {
	if isLeaf {
		return s.LeafFields, s.LeafNNullable
	}
	return s.NonLeafFields, s.LeafNNullable
}

// skipCount is how many of fields the field-information block actually
// describes: a leaf record's *are* the field-info's own field list, but a
// non-leaf (node-pointer) record's list here always has one extra entry
// appended (the 4-byte child page number - see tableZipShape/sdiZipShape)
// that the field-info block never counts at all (rec_get_offsets_reverse
// derives the node pointer's presence structurally, not from anything
// page_zip_fields_encode wrote for it - confirmed empirically, see the
// file comment).
func (s zipIndexShape) skipCount(isLeaf bool, fields []*IndexField) int {
	if isLeaf {
		return len(fields)
	}
	return len(fields) - 1
}

// zipDenseEnt is one dense-directory entry: its record's placement (== the
// offset the record's decoded fieldRanges are addressed from, one past its
// 5-byte header - same convention as recOff everywhere else in this tool),
// whether it's delete-marked, and (via liveIdx>=0) its position in the live
// chain if it's a currently-live record rather than one on the free list.
type zipDenseEnt struct {
	offs    uint32
	del     bool
	liveIdx int // -1 if this slot is on the free list
}

// zipSkipFieldsBlock reads and discards page_zip_fields_decode's index-
// information block (n+1 variable-length encoded values - see the file
// comment for why this tool never needs to decode what they actually say).
func zipSkipFieldsBlock(r io.ByteReader, n int) error {
	for i := 0; i < n+1; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return fmt.Errorf("index-information block truncated: %w", err)
		}
		if b&0x80 != 0 {
			if _, err := r.ReadByte(); err != nil {
				return fmt.Errorf("index-information block truncated: %w", err)
			}
		}
	}
	return nil
}

// oneByteReader forces every Read to pull at most one byte from the
// wrapped reader. compress/zlib (via compress/flate) buffers its input
// internally and will happily read ahead of whatever the caller actually
// asked for; since this tool needs to know the *exact* byte offset the
// compressed stream ends at (to find the modification log immediately
// following it - see decompressIndexPage), that read-ahead would silently
// eat into the modification log's own bytes. Limiting reads to one byte at
// a time makes flate ask again exactly when (and only when) it needs more,
// so the source reader's position after Close() is always exact. Pages are
// small (a handful of KB), so the extra call overhead is immaterial.
type oneByteReader struct{ r io.Reader }

func (o oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

// decompressIndexPage reconstructs the full logicalSize-byte page for a
// ROW_FORMAT=COMPRESSED FIL_PAGE_INDEX/FIL_PAGE_SDI page, given its raw
// on-disk (physical, zip_size) bytes - see the file comment for the three
// passes involved. The result is a fully ordinary COMPACT-format page as
// far as every other file in this tool is concerned: same header, same
// infimum/supremum, and a live-record chain with correct next-pointers and
// field bytes - see the doc comment on zipDenseEnt for what's deliberately
// NOT reconstructed with full fidelity, because nothing here ever needs to
// read it back: the sparse page directory, n_owned counts, and any record
// on the free list (this tool only ever walks the live chain from
// infimum, so free-list records only need to be correctly *skipped past*
// in the compressed byte stream and modification log, not placed with
// full header fidelity).
func decompressIndexPage(raw []byte, logicalSize uint32, shape zipIndexShape) ([]byte, error) {
	physSize := len(raw)
	if physSize < pageData+8 {
		return nil, fmt.Errorf("compressed page is shorter than a valid header")
	}
	nHeap := int(pageGetNHeap(raw))
	nDense := nHeap - 2
	if nDense < 0 {
		return nil, fmt.Errorf("compressed page: implausible heap count %d", nHeap)
	}
	if nDense*pageZipDirSlotSize >= physSize {
		return nil, fmt.Errorf("compressed page: dense directory (%d slots) too large for a %d-byte page", nDense, physSize)
	}
	nRecs := int(pageGetNRecs(raw))
	if nRecs > nDense {
		return nil, fmt.Errorf("compressed page: n_recs %d > n_dense %d", nRecs, nDense)
	}

	out := make([]byte, logicalSize)
	copy(out[:pageData], raw[:pageData])

	dirSlot := func(i int) uint16 { return binary.BigEndian.Uint16(raw[physSize-pageZipDirSlotSize*(i+1):]) }

	dense := make([]zipDenseEnt, nDense)
	for i := 0; i < nDense; i++ {
		v := dirSlot(i)
		e := zipDenseEnt{offs: uint32(v & pageZipDirSlotMask), del: v&pageZipDirSlotDel != 0, liveIdx: -1}
		if i < nRecs {
			if e.offs < pageZipStart+recNNewExtraBytes {
				return nil, fmt.Errorf("compressed page: dense directory slot %d has an out-of-range offset %d", i, e.offs)
			}
			e.liveIdx = i
		}
		dense[i] = e
	}

	// Wire up the live record chain (dense-dir order == collation/link
	// order for the first n_recs entries) plus infimum/supremum - see
	// zipDenseEnt's doc comment for what's deliberately skipped.
	copy(out[pageNewInfimum-recNNewExtraBytes:], infimumExtra[:])
	copy(out[pageNewInfimum:], infimumData[:])
	if nRecs == 0 {
		recSetNextOffsNew(out, pageNewInfimum, pageNewSupremum)
	} else {
		recSetNextOffsNew(out, pageNewInfimum, dense[0].offs)
	}
	isLeaf := pageGetLevel(raw) == 0
	status := byte(recStatusOrdinary)
	if !isLeaf {
		status = recStatusNodePtr
	}
	for i := 0; i < nRecs; i++ {
		rec := dense[i].offs
		next := uint32(pageNewSupremum)
		if i+1 < nRecs {
			next = dense[i+1].offs
		}
		recSetNextOffsNew(out, rec, next)
		out[rec-3] = status
		if dense[i].del {
			out[rec-5] = recInfoDeletedFlag
		}
	}
	out[pageNewSupremum-recNNewExtraBytes] = 0x01
	copy(out[pageNewSupremum-recNNewExtraBytes+1:], supremumExtraData[:])

	// The compressed byte stream itself is laid out in ascending final-
	// address order (see the file comment on page0zip.ic), which is NOT
	// the same as dense-directory-index order except for a page that has
	// never had a record deleted from it.
	addr := append([]zipDenseEnt(nil), dense...)
	sort.Slice(addr, func(a, b int) bool { return addr[a].offs < addr[b].offs })

	modLogStart, written, err := zipInflateImage(raw, out, addr, isLeaf, shape)
	if err != nil {
		return nil, err
	}
	if modLogStart < physSize && raw[modLogStart] != 0 {
		if err := zipApplyModLog(raw[modLogStart:], out, addr, isLeaf, shape, written); err != nil {
			return nil, err
		}
	}
	for i, e := range addr {
		if e.liveIdx >= 0 && !written[i] {
			return nil, fmt.Errorf("compressed page: record at offset %d was never written by either the compressed image or the modification log", e.offs)
		}
	}
	zipRestoreStorage(raw, out, addr, isLeaf, shape, written)

	return out, nil
}

// zipInflateImage runs pass 1 (see the file comment): inflates the page's
// zlib-compressed image into every record's placement, in ascending
// address order, leaving the fields pass 3 restores as zero placeholders.
// Returns the physical byte offset the (uncompressed) modification log
// begins at, immediately following the zlib stream.
func zipInflateImage(raw []byte, out []byte, addr []zipDenseEnt, isLeaf bool, shape zipIndexShape) (modLogStart int, written []bool, err error) {
	written = make([]bool, len(addr))
	src := bytes.NewReader(raw[pageData:])
	zr, zerr := zlib.NewReader(oneByteReader{src})
	if zerr != nil {
		return 0, written, fmt.Errorf("compressed page: %w", zerr)
	}
	defer zr.Close()
	fields, nNullable := shape.fields(isLeaf)
	if err := zipSkipFieldsBlock(bufByteReader{zr}, shape.skipCount(isLeaf, fields)); err != nil {
		return 0, written, err
	}

	// The compressed image can legitimately end partway through this loop:
	// MySQL doesn't recompress a page on every write, so a record added or
	// rewritten since the last full recompress has no image content at all
	// - its dense-directory slot (read fresh off disk, independently of
	// this image) exists, but every byte of it comes from the modification
	// log instead (pass 2). Once the zlib stream signals a clean end
	// (io.EOF/io.ErrUnexpectedEOF, never partway through a byte the
	// oneByteReader wrapper hands over - so this can't mask a truncated
	// *earlier* record), everything from here on - this record included -
	// is left zeroed for pass 2 to fill in, exactly like page_zip_
	// decompress_clust/_node_ptrs's own Z_STREAM_END case.
	curPos := uint32(pageZipStart)
	streamEnded := false
	read := func(dst []byte) error {
		if streamEnded {
			return io.EOF
		}
		_, err := io.ReadFull(zr, dst)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			streamEnded = true
		}
		return err
	}
recordLoop:
	for i, e := range addr {
		target := e.offs
		if target < curPos+recNNewExtraBytes {
			return 0, written, fmt.Errorf("compressed page: dense directory is not in ascending address order at offset %d", target)
		}
		if err := read(out[curPos : target-recNNewExtraBytes]); err != nil {
			if streamEnded {
				break recordLoop
			}
			return 0, written, fmt.Errorf("compressed page: %w", err)
		}
		newPos, ferr := zipFillRecordData(out, target, target, isLeaf, shape, fields, nNullable, read)
		if ferr != nil {
			if streamEnded {
				break recordLoop
			}
			return 0, written, ferr
		}
		written[i] = true
		curPos = newPos
	}

	// Drain any trailing garbage (a free-list record allocated from
	// originally longer space) up to PAGE_HEAP_TOP, so the zlib stream's
	// read position lands exactly at the end of the compressed data.
	if !streamEnded {
		heapTop := binary.BigEndian.Uint16(out[pageHeapTop:])
		if uint32(heapTop) > curPos {
			if err := read(out[curPos:heapTop]); err != nil && !streamEnded {
				return 0, written, fmt.Errorf("compressed page: %w", err)
			}
		}
	}

	// Closing the zlib reader validates its Adler-32 trailer; afterward,
	// src.Len() reports exactly how much of raw[pageData:] is unread
	// (oneByteReader guarantees flate never read ahead of what it needed),
	// i.e. where the compressed stream ends and the modification log
	// begins.
	if err := zr.Close(); err != nil {
		return 0, written, fmt.Errorf("compressed page: %w", err)
	}
	consumed := len(raw) - pageData - src.Len()
	return pageData + consumed, written, nil
}

// bufByteReader adapts an io.Reader (compress/zlib's Reader doesn't itself
// implement io.ByteReader) to io.ByteReader by reading one byte at a time -
// only ever used for the tiny (a handful of bytes) index-information block.
type bufByteReader struct{ r io.Reader }

func (b bufByteReader) ReadByte() (byte, error) {
	var buf [1]byte
	_, err := io.ReadFull(b.r, buf[:])
	return buf[0], err
}

// zipFillRecordData decodes one record's field ranges (its extra bytes -
// NULL bitmap and variable-length list - must already be in place at
// out[target-5-...:target-5], exactly as decodeFieldRangesSpecial expects)
// and reads its field data via read, leaving DB_TRX_ID/DB_ROLL_PTR, each
// external reference's last 20 bytes, and (non-leaf) the node pointer as
// zero placeholders for zipRestoreStorage to fill in later - mirroring
// page_zip_decompress_clust/_clust_ext/_node_ptrs (this tool never walks a
// secondary index for data, so page_zip_decompress_sec has no equivalent
// here). Returns the output position immediately after this record.
func zipFillRecordData(out []byte, target, curPos uint32, isLeaf bool, shape zipIndexShape, fields []*IndexField, nNullable int, read func([]byte) error) (uint32, error) {
	ranges, err := decodeFieldRangesSpecial(out, target, 0, fields, nNullable, nil)
	if err != nil {
		return 0, err
	}
	dataEnd := ranges[len(ranges)-1].End

	if !isLeaf {
		toRead := dataEnd - recNodePtrSize - curPos
		if err := read(out[curPos : curPos+toRead]); err != nil {
			return 0, fmt.Errorf("compressed page: %w", err)
		}
		return dataEnd, nil
	}

	for i, r := range ranges {
		switch {
		case i == shape.TrxIDIdx:
			if r.Start > curPos {
				if err := read(out[curPos:r.Start]); err != nil {
					return 0, fmt.Errorf("compressed page: %w", err)
				}
			}
			curPos = r.Start + trxRollLen // DB_TRX_ID+DB_ROLL_PTR: placeholder, restored later
		case r.External:
			extStart := r.End - btrExternFieldRefSize
			if extStart > curPos {
				if err := read(out[curPos:extStart]); err != nil {
					return 0, fmt.Errorf("compressed page: %w", err)
				}
			}
			curPos = r.End // the reference's last 20 bytes: placeholder, restored later
		}
	}
	if dataEnd > curPos {
		if err := read(out[curPos:dataEnd]); err != nil {
			return 0, fmt.Errorf("compressed page: %w", err)
		}
		curPos = dataEnd
	}
	return curPos, nil
}

// zipApplyModLog runs pass 2 (see the file comment): replays the
// modification log (log, already sliced to start right after the
// compressed image and known to be non-empty), overwriting specific
// records' extra bytes and field data with whatever was written after the
// page was last fully compressed. Ported from page_zip_apply_log.
//
// A "clear" entry (marking a record delete-marked-and-later-purged) is
// read (to stay correctly positioned in the log) but never needs to be
// applied: the only records this tool ever reads back are the currently-
// live ones (per the dense directory, read fresh off disk independently of
// the log), and a live record's *last* log entry can never be a bare
// clear - InnoDB only clears a record that stays cleared (on the free
// list, which this tool doesn't walk) or one that's about to be reused by
// a subsequent write entry later in the very same log, which fully
// overwrites it anyway. So this only ever needs to apply writes.
func zipApplyModLog(log []byte, out []byte, addr []zipDenseEnt, isLeaf bool, shape zipIndexShape, written []bool) error {
	fields, nNullable := shape.fields(isLeaf)
	pos := 0
	readVal := func() (int, error) {
		if pos >= len(log) {
			return 0, fmt.Errorf("modification log runs past the page")
		}
		v := int(log[pos])
		pos++
		if v&0x80 != 0 {
			if pos >= len(log) {
				return 0, fmt.Errorf("modification log runs past the page")
			}
			v = (v&0x7f)<<8 | int(log[pos])
			pos++
		}
		return v, nil
	}
	for {
		val, err := readVal()
		if err != nil {
			return err
		}
		if val == 0 {
			return nil // end of log
		}
		idx := (val >> 1) - 1
		if idx < 0 || idx >= len(addr) {
			return fmt.Errorf("modification log entry references record %d, out of range for %d dense-directory slots", idx, len(addr))
		}
		if val&1 != 0 {
			continue // clear entry: no payload, see the doc comment
		}
		target := addr[idx].offs

		extraLen, err := zipExtraBytesLen(log[pos:], fields, nNullable)
		if err != nil {
			return fmt.Errorf("modification log: record %d: %w", idx, err)
		}
		if pos+extraLen > len(log) {
			return fmt.Errorf("modification log runs past the page")
		}
		if extraLen > int(target)-pageZipStart {
			return fmt.Errorf("modification log: record %d has an implausible extra-bytes length %d", idx, extraLen)
		}
		// The log stores a record's extra bytes in the same order they'd
		// be read off a real page working backward from its header (see
		// the file comment's cross-reference to rec_get_offsets_reverse):
		// log[0] is the byte immediately preceding the header, log[1] the
		// one before that, and so on.
		dst := out[target-recNNewExtraBytes-uint32(extraLen) : target-recNNewExtraBytes]
		for i, b := range log[pos : pos+extraLen] {
			dst[extraLen-1-i] = b
		}
		pos += extraLen

		curPos := target
		read := func(dst []byte) error {
			if pos+len(dst) > len(log) {
				return fmt.Errorf("modification log runs past the page")
			}
			copy(dst, log[pos:pos+len(dst)])
			pos += len(dst)
			return nil
		}
		if _, err := zipFillRecordData(out, target, curPos, isLeaf, shape, fields, nNullable, read); err != nil {
			return fmt.Errorf("modification log: record %d: %w", idx, err)
		}
		written[idx] = true
	}
}

// zipExtraBytesLen reports how many bytes of buf are one record's extra
// bytes (NULL bitmap + variable-length list, in the same forward order
// zipApplyModLog's caller expects - see its doc comment), by walking the
// exact same NULL-bit/variable-length-list logic decodeFieldRangesSpecial
// uses, just read forward from a flat buffer instead of backward from a
// page offset (the two are equivalent: this tool's own cross-check against
// rec_get_offsets_reverse - see the file comment - confirms the log's
// forward byte order is bit-for-bit the reverse of a page's, which is
// exactly what "read forward instead of backward" already gives here).
func zipExtraBytesLen(buf []byte, fields []*IndexField, nNullable int) (int, error) {
	nullBytes := (nNullable + 7) / 8
	if nullBytes > len(buf) {
		return 0, fmt.Errorf("truncated NULL bitmap")
	}
	nullPos := 0
	isNull := func() bool {
		byteIdx := nullPos / 8
		bit := uint(nullPos % 8)
		nullPos++
		return buf[byteIdx]&(1<<bit) != 0
	}
	pos := nullBytes
	for _, f := range fields {
		col := f.Col
		if col.IsNullable && isNull() {
			continue
		}
		if f.EffFixedLen != 0 {
			continue
		}
		if pos >= len(buf) {
			return 0, fmt.Errorf("truncated variable-length list")
		}
		b1 := buf[pos]
		pos++
		if col.isBigCol() && b1&0x80 != 0 {
			if pos >= len(buf) {
				return 0, fmt.Errorf("truncated variable-length list")
			}
			pos++
		}
	}
	return pos, nil
}

// zipRestoreStorage runs pass 3 (see the file comment): restores DB_TRX_ID/
// DB_ROLL_PTR (leaf) or the node pointer (non-leaf), and any externally-
// stored column's BTR_EXTERN_FIELD_REF, from the page's own dedicated
// storage/externs trailer - which is never touched by the compressed image
// or the modification log - in the same ascending-address order they were
// written in (page_zip_decompress_clust's own final restore loop). A
// free-list (non-live) record's external references are left zeroed rather
// than restored, matching page_zip_dir_find_free's live/free distinction:
// the encoder never reserved externs-trailer entries for them.
func zipRestoreStorage(raw []byte, out []byte, addr []zipDenseEnt, isLeaf bool, shape zipIndexShape, written []bool) {
	fields, nNullable := shape.fields(isLeaf)
	physSize := uint32(len(raw))
	nDense := uint32(len(addr))
	storagePtr := physSize - nDense*pageZipDirSlotSize
	slotSize := uint32(trxRollLen)
	if !isLeaf {
		slotSize = recNodePtrSize
	}
	externsPtr := storagePtr - nDense*slotSize

	for i, e := range addr {
		// storagePtr always advances by exactly one slot per dense-
		// directory entry, live or not - the encoder reserved this trailer
		// space uniformly, regardless of which slots are actually live -
		// so this must run even for a free-list record whose bytes were
		// never validly established (below) to keep every *later* entry's
		// restore correctly aligned.
		storagePtr -= slotSize
		if !written[i] {
			// A free-list record this tool never reads back (WalkRows only
			// ever walks the live chain) and that neither pass 1 nor pass 2
			// wrote a single real byte of: its "extra bytes" are still
			// whatever zero-initialized filler decodeFieldRangesSpecial
			// would decode into a plausible-looking but meaningless field
			// layout, and restoring anything against that would risk
			// scribbling into a genuinely live record's bytes. Skip it -
			// note this never applies to a live record, which zipInflate
			// Image/zipApplyModLog's own bookkeeping already guarantees is
			// always written by the time this pass runs (see
			// decompressIndexPage).
			continue
		}
		ranges, err := decodeFieldRangesSpecial(out, e.offs, 0, fields, nNullable, nil)
		if err != nil {
			continue
		}
		dataEnd := ranges[len(ranges)-1].End

		if !isLeaf {
			copy(out[dataEnd-recNodePtrSize:dataEnd], raw[storagePtr:storagePtr+recNodePtrSize])
			continue
		}
		trxStart := ranges[shape.TrxIDIdx].Start
		copy(out[trxStart:trxStart+trxRollLen], raw[storagePtr:storagePtr+trxRollLen])

		for _, r := range ranges {
			if !r.External {
				continue
			}
			extStart := r.End - btrExternFieldRefSize
			if e.liveIdx >= 0 {
				externsPtr -= btrExternFieldRefSize
				copy(out[extStart:r.End], raw[externsPtr:externsPtr+btrExternFieldRefSize])
			}
		}
	}
}

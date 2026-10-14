// rocksdb_log.go: RocksDB's log file format - shared by the write-ahead
// logs (NNNNNN.log, holding writes not yet flushed into an SST file) and
// the MANIFEST (holding the history of which SST files make up the
// database) - plus the two payloads carried in it: WriteBatch (WAL) and
// VersionEdit (MANIFEST). See db/log_format.h, db/log_reader.cc,
// db/write_batch.cc and db/version_edit.cc.
//
// A log file is a sequence of 32KB blocks; each block holds records with
// a 7-byte header (masked CRC-32C, 2-byte length, type; recyclable logs add
// a 4-byte log number). A logical record too big for the rest of a block
// is split into FIRST/MIDDLE/LAST fragments.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	logBlockSize            = 32768
	logHeaderSize           = 7
	logRecyclableHeaderSize = 11

	logZeroType               = 0
	logFullType               = 1
	logFirstType              = 2
	logMiddleType             = 3
	logLastType               = 4
	logRecyclableFullType     = 5
	logRecyclableFirstType    = 6
	logRecyclableMiddleType   = 7
	logRecyclableLastType     = 8
	logSetCompressionType     = 9
	logUserDefinedTSType      = 10
	logRecyclableUserDefTSTyp = 11
)

// logRecord is one reassembled logical record and where it started.
type logRecord struct {
	Data   []byte
	Offset int64
}

// logProblem is one spot a log reader had to skip or stop at.
type logProblem struct {
	Offset int64
	Reason string
	Tail   bool // at the very end of the file: an unfinished last write, normal after a crash
}

// readLogFile reads every logical record in a WAL or MANIFEST file.
// logNumber is the file's own number, which a recycled WAL's records must
// carry (anything else in it is stale data from the file's previous use).
// Reading resumes at the next 32KB block after a damaged record, so one
// bad record doesn't hide everything after it; each such skip is returned
// in problems.
func readLogFile(path string, logNumber uint64) (recs []logRecord, problems []logProblem, err error) {
	data, err := readSourceFile(path)
	if err != nil {
		return nil, nil, err
	}
	var frag []byte
	fragStart := int64(-1)
	inFrag := false
	compressed := false

	dropFrag := func(why string) {
		if inFrag {
			problems = append(problems, logProblem{Offset: fragStart, Reason: why})
		}
		frag, inFrag, fragStart = nil, false, -1
	}

	pos := 0
	for pos < len(data) {
		blockEnd := (pos/logBlockSize + 1) * logBlockSize
		if blockEnd > len(data) {
			blockEnd = len(data)
		}
		if blockEnd-pos < logHeaderSize {
			pos = blockEnd // trailer padding
			continue
		}
		h := data[pos:]
		typ := h[6]
		length := int(binary.LittleEndian.Uint16(h[4:6]))
		hdr := logHeaderSize
		if typ >= logRecyclableFullType && typ <= logRecyclableLastType || typ == logRecyclableUserDefTSTyp {
			hdr = logRecyclableHeaderSize
			if blockEnd-pos < hdr {
				pos = blockEnd
				continue
			}
		}
		if typ == logZeroType && length == 0 {
			// Preallocated space never written (or a recycled log's zeroed
			// tail): nothing more in this block.
			pos = blockEnd
			continue
		}
		if pos+hdr+length > blockEnd {
			atEnd := blockEnd == len(data)
			problems = append(problems, logProblem{Offset: int64(pos), Tail: atEnd,
				Reason: fmt.Sprintf("record header says %d bytes, but only %d remain in its block", length, blockEnd-pos-hdr)})
			dropFrag("fragmented record cut short")
			pos = blockEnd
			continue
		}
		payload := data[pos+hdr : pos+hdr+length]
		stored := crc32cUnmask(binary.LittleEndian.Uint32(h[0:4]))
		computed := crc32cValue(data[pos+6 : pos+hdr]) // type byte (+ log number)
		computed = crc32cExtend(computed, payload)
		if stored != computed {
			atEnd := blockEnd == len(data) && allZeroOrEnd(data[pos+hdr+length:])
			problems = append(problems, logProblem{Offset: int64(pos), Tail: atEnd, Reason: "record checksum mismatch"})
			dropFrag("fragmented record lost to a damaged fragment")
			pos = blockEnd
			continue
		}
		if hdr == logRecyclableHeaderSize {
			if uint64(binary.LittleEndian.Uint32(h[7:11])) != logNumber&0xFFFFFFFF {
				// Stale data from this recycled file's previous life: the
				// log ends here.
				break
			}
		}
		recStart := int64(pos)
		pos += hdr + length

		switch typ {
		case logFullType, logRecyclableFullType:
			dropFrag("fragmented record never completed")
			recs = append(recs, logRecord{Data: append([]byte(nil), payload...), Offset: recStart})
		case logFirstType, logRecyclableFirstType:
			dropFrag("fragmented record never completed")
			frag, inFrag, fragStart = append([]byte(nil), payload...), true, recStart
		case logMiddleType, logRecyclableMiddleType:
			if inFrag {
				frag = append(frag, payload...)
			} else {
				problems = append(problems, logProblem{Offset: recStart, Reason: "MIDDLE fragment with no FIRST before it"})
			}
		case logLastType, logRecyclableLastType:
			if inFrag {
				frag = append(frag, payload...)
				recs = append(recs, logRecord{Data: frag, Offset: fragStart})
				frag, inFrag, fragStart = nil, false, -1
			} else {
				problems = append(problems, logProblem{Offset: recStart, Reason: "LAST fragment with no FIRST before it"})
			}
		case logSetCompressionType:
			if len(payload) > 0 && payload[0] != rdbNoCompression {
				compressed = true
			}
		case logUserDefinedTSType, logRecyclableUserDefTSTyp:
			// Timestamp-size metadata: MyRocks doesn't use user-defined
			// timestamps.
		default:
			problems = append(problems, logProblem{Offset: recStart, Reason: fmt.Sprintf("unknown record type %d", typ)})
		}
		if compressed {
			return recs, append(problems, logProblem{Offset: recStart,
				Reason: "this WAL uses wal_compression (streaming-compressed records), which isn't supported - records after this point were not read"}), nil
		}
	}
	if inFrag {
		problems = append(problems, logProblem{Offset: fragStart, Tail: true, Reason: "file ends in the middle of a fragmented record"})
	}
	return recs, problems, nil
}

func allZeroOrEnd(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func crc32cUnmask(m uint32) uint32 {
	rot := m - 0xa282ead8
	return (rot >> 17) | (rot << 15)
}

func crc32cExtend(crc uint32, p []byte) uint32 {
	return crcUpdateCastagnoli(crc, p)
}

// --- WriteBatch (WAL payload) ---

// walOp is one key operation decoded from a WAL WriteBatch.
type walOp struct {
	LogNum uint64      // the WAL file it was read from (set by the replayer)
	Origin entryOrigin // likewise
	CF     uint32
	Type   byte // rdbTypeValue, rdbTypeDeletion, rdbTypeSingleDeletion, rdbTypeMerge, rdbTypeRangeDeletion, ...
	Key    []byte
	Value  []byte // the end key for a range deletion
}

// walBatchVisitor receives a WriteBatch's contents in order.
type walBatchVisitor interface {
	start(seq uint64)
	op(o walOp)
	beginPrepare()
	endPrepare(xid []byte)
	commit(xid []byte)
	rollback(xid []byte)
}

// decodeWriteBatch walks one WriteBatch record (WriteBatchInternal::Iterate):
// a 12-byte header (starting sequence number, entry count) followed by
// tagged entries.
func decodeWriteBatch(b []byte, v walBatchVisitor) (seq uint64, err error) {
	if len(b) < 12 {
		return 0, errors.New("WriteBatch shorter than its 12-byte header")
	}
	seq = binary.LittleEndian.Uint64(b)
	v.start(seq)
	r := rdbReader{b: b[12:]}
	for len(r.b) > 0 && r.err == nil {
		tag := r.b[0]
		r.b = r.b[1:]
		var cf uint32
		switch tag {
		case rdbTypeCFValue, rdbTypeCFDeletion, rdbTypeCFSingleDeletion, rdbTypeCFMerge,
			rdbTypeCFRangeDeletion, rdbTypeCFBlobIndex, rdbTypeCFWideColumnEntity, rdbTypeCFValuePreferredSeqno:
			cf = r.varint32("column family id")
		}
		switch tag {
		case rdbTypeValue, rdbTypeCFValue:
			k, val := r.lpSlice("key"), r.lpSlice("value")
			v.op(walOp{CF: cf, Type: rdbTypeValue, Key: k, Value: val})
		case rdbTypeDeletion, rdbTypeCFDeletion:
			v.op(walOp{CF: cf, Type: rdbTypeDeletion, Key: r.lpSlice("key")})
		case rdbTypeSingleDeletion, rdbTypeCFSingleDeletion:
			v.op(walOp{CF: cf, Type: rdbTypeSingleDeletion, Key: r.lpSlice("key")})
		case rdbTypeDeletionWithTimestamp:
			v.op(walOp{CF: cf, Type: rdbTypeDeletion, Key: r.lpSlice("key")})
		case rdbTypeMerge, rdbTypeCFMerge:
			k, val := r.lpSlice("key"), r.lpSlice("value")
			v.op(walOp{CF: cf, Type: rdbTypeMerge, Key: k, Value: val})
		case rdbTypeRangeDeletion, rdbTypeCFRangeDeletion:
			k, end := r.lpSlice("range start"), r.lpSlice("range end")
			v.op(walOp{CF: cf, Type: rdbTypeRangeDeletion, Key: k, Value: end})
		case rdbTypeBlobIndex, rdbTypeCFBlobIndex:
			k, val := r.lpSlice("key"), r.lpSlice("blob index")
			v.op(walOp{CF: cf, Type: rdbTypeBlobIndex, Key: k, Value: val})
		case rdbTypeWideColumnEntity, rdbTypeCFWideColumnEntity:
			k, val := r.lpSlice("key"), r.lpSlice("entity")
			v.op(walOp{CF: cf, Type: rdbTypeWideColumnEntity, Key: k, Value: val})
		case rdbTypeValuePreferredSeqno, rdbTypeCFValuePreferredSeqno:
			k, val := r.lpSlice("key"), r.lpSlice("value")
			v.op(walOp{CF: cf, Type: rdbTypeValue, Key: k, Value: val})
		case rdbTypeLogData:
			r.lpSlice("log data")
		case rdbTypeNoop:
		case rdbTypeBeginPrepareXID, rdbTypeBeginPersistedPrepareXID, rdbTypeBeginUnprepareXID:
			v.beginPrepare()
		case rdbTypeEndPrepareXID:
			v.endPrepare(r.lpSlice("xid"))
		case rdbTypeCommitXID:
			v.commit(r.lpSlice("xid"))
		case rdbTypeCommitXIDAndTimestamp:
			r.lpSlice("commit timestamp")
			v.commit(r.lpSlice("xid"))
		case rdbTypeRollbackXID:
			v.rollback(r.lpSlice("xid"))
		default:
			return seq, fmt.Errorf("unknown WriteBatch entry tag 0x%x", tag)
		}
	}
	return seq, r.err
}

// --- VersionEdit (MANIFEST payload) ---

// manifestFile is one SST file a VersionEdit adds.
type manifestFile struct {
	Level                   int
	Number                  uint64
	Size                    uint64
	Smallest, Largest       []byte // internal keys
	SmallestSeq, LargestSeq uint64
}

type versionEdit struct {
	HasCF        bool
	CF           uint32
	CFAdd        bool
	CFName       string
	CFDrop       bool
	HasLogNum    bool
	LogNumber    uint64
	HasLastSeq   bool
	LastSeq      uint64
	HasMinLog    bool
	MinLogToKeep uint64
	Comparator   string
	NewFiles     []manifestFile
	Deleted      []uint64 // file numbers
}

const (
	veComparator     = 1
	veLogNumber      = 2
	veNextFileNumber = 3
	veLastSequence   = 4
	veCompactCursor  = 5
	veDeletedFile    = 6
	veNewFile        = 7
	vePrevLogNumber  = 9
	veMinLogToKeep   = 10
	veNewFile2       = 100
	veNewFile3       = 102
	veNewFile4       = 103
	veColumnFamily   = 200
	veCFAdd          = 201
	veCFDrop         = 202
	veMaxCF          = 203
	veInAtomicGroup  = 300
	veBlobFileAdd    = 400
	veBlobFileGarb   = 401
	veSafeIgnoreMask = 1 << 13
)

// decodeVersionEdit is VersionEdit::DecodeFrom, keeping only what's needed
// to reconstruct the set of live files per column family.
func decodeVersionEdit(b []byte) (*versionEdit, error) {
	e := &versionEdit{}
	r := rdbReader{b: b}
	for len(r.b) > 0 && r.err == nil {
		tag := r.varint32("tag")
		switch tag {
		case veComparator:
			e.Comparator = string(r.lpSlice("comparator"))
		case veLogNumber:
			e.HasLogNum, e.LogNumber = true, r.varint64("log number")
		case vePrevLogNumber, veNextFileNumber:
			r.varint64("file number")
		case veMinLogToKeep:
			e.HasMinLog, e.MinLogToKeep = true, r.varint64("min log number")
		case veMaxCF:
			r.varint32("max column family")
		case veLastSequence:
			e.HasLastSeq, e.LastSeq = true, r.varint64("last sequence")
		case veCompactCursor:
			r.varint32("level")
			r.lpSlice("compaction cursor")
		case veDeletedFile:
			r.varint32("level")
			e.Deleted = append(e.Deleted, r.varint64("deleted file"))
		case veNewFile, veNewFile2, veNewFile3:
			var f manifestFile
			f.Level = int(r.varint32("level"))
			f.Number = r.varint64("file number")
			if tag == veNewFile3 {
				r.varint32("path id")
			}
			f.Size = r.varint64("file size")
			f.Smallest = append([]byte(nil), r.lpSlice("smallest key")...)
			f.Largest = append([]byte(nil), r.lpSlice("largest key")...)
			f.LargestSeq = ^uint64(0) >> 8
			if tag != veNewFile {
				f.SmallestSeq = r.varint64("smallest seqno")
				f.LargestSeq = r.varint64("largest seqno")
			}
			e.NewFiles = append(e.NewFiles, f)
		case veNewFile4:
			var f manifestFile
			f.Level = int(r.varint32("level"))
			f.Number = r.varint64("file number")
			f.Size = r.varint64("file size")
			f.Smallest = append([]byte(nil), r.lpSlice("smallest key")...)
			f.Largest = append([]byte(nil), r.lpSlice("largest key")...)
			f.SmallestSeq = r.varint64("smallest seqno")
			f.LargestSeq = r.varint64("largest seqno")
			for r.err == nil {
				ct := r.varint32("custom tag")
				if ct == 1 { // kTerminate
					break
				}
				r.lpSlice("custom field") // epoch number, checksums, path id, ...
			}
			f.Number &= 0x3FFFFFFFFFFFFFFF // kFileNumberMask
			e.NewFiles = append(e.NewFiles, f)
		case veColumnFamily:
			e.HasCF, e.CF = true, r.varint32("column family")
		case veCFAdd:
			e.CFAdd, e.CFName = true, string(r.lpSlice("column family name"))
		case veCFDrop:
			e.CFDrop = true
		case veInAtomicGroup:
			r.varint32("atomic group size")
		case veBlobFileAdd, veBlobFileGarb:
			return e, fmt.Errorf("MANIFEST uses BlobDB (integrated blob files), which MyRocks doesn't and this tool doesn't read")
		default:
			if tag&veSafeIgnoreMask != 0 {
				r.lpSlice("ignorable field")
				continue
			}
			return e, fmt.Errorf("unknown VersionEdit tag %d", tag)
		}
	}
	return e, r.err
}

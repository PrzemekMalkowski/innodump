// rocksdb_codec.go: the low-level encodings a RocksDB data directory is
// built from - varints, the four block checksum algorithms, and the block
// compression formats - shared by the SST, WAL and MANIFEST readers
// (rocksdb_sst.go, rocksdb_log.go). See rocksdb_db.go's package comment for
// how they fit together.
//
// Everything here was written against the RocksDB source a Percona Server
// 8.4 build bundles (storage/rocksdb/rocksdb: util/coding.h, util/hash.h,
// util/crc32c.h, util/compression.h, table/format.cc).
package main

import (
	"bytes"
	"compress/bzip2"
	"compress/flate"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/bits"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/zeebo/xxh3"
)

// --- varints ---

// rdbVarint64 decodes one RocksDB varint (the same LEB128 encoding
// encoding/binary uses) from the start of b, returning the value and the
// number of bytes it took, or n == 0 if b doesn't hold a complete one.
func rdbVarint64(b []byte) (v uint64, n int) {
	v, n = binary.Uvarint(b)
	if n <= 0 {
		return 0, 0
	}
	return v, n
}

// rdbVarint32 is rdbVarint64 limited to 5 bytes / 32 bits, as GetVarint32.
func rdbVarint32(b []byte) (v uint32, n int) {
	x, n := rdbVarint64(b)
	if n == 0 || n > 5 || x > 0xFFFFFFFF {
		return 0, 0
	}
	return uint32(x), n
}

// rdbReader is a forward-only cursor over a byte slice with RocksDB's own
// Get* helpers; any decoding failure sets err and makes every later call a
// no-op, so a run of reads only needs one check at the end.
type rdbReader struct {
	b   []byte
	err error
}

func (r *rdbReader) fail(what string) {
	if r.err == nil {
		r.err = fmt.Errorf("truncated or malformed %s", what)
	}
}

func (r *rdbReader) varint64(what string) uint64 {
	if r.err != nil {
		return 0
	}
	v, n := rdbVarint64(r.b)
	if n == 0 {
		r.fail(what)
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *rdbReader) varint32(what string) uint32 {
	if r.err != nil {
		return 0
	}
	v, n := rdbVarint32(r.b)
	if n == 0 {
		r.fail(what)
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *rdbReader) bytes(n int, what string) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.b) {
		r.fail(what)
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

// lpSlice reads a varint32 length followed by that many bytes
// (GetLengthPrefixedSlice).
func (r *rdbReader) lpSlice(what string) []byte {
	n := r.varint32(what)
	return r.bytes(int(n), what)
}

func (r *rdbReader) fixed32(what string) uint32 {
	b := r.bytes(4, what)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (r *rdbReader) fixed64(what string) uint64 {
	b := r.bytes(8, what)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

// --- checksums ---

// RocksDB's ChecksumType (include/rocksdb/table.h).
const (
	rdbNoChecksum = 0
	rdbCRC32c     = 1
	rdbXXHash     = 2
	rdbXXHash64   = 3
	rdbXXH3       = 4
)

func rdbChecksumName(t byte) string {
	switch t {
	case rdbNoChecksum:
		return "none"
	case rdbCRC32c:
		return "crc32c"
	case rdbXXHash:
		return "xxHash"
	case rdbXXHash64:
		return "xxHash64"
	case rdbXXH3:
		return "XXH3"
	}
	return fmt.Sprintf("unknown(%d)", t)
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// crc32cMask is crc32c::Mask: RocksDB (like LevelDB) never stores a raw
// CRC of data that may itself contain CRCs.
func crc32cMask(crc uint32) uint32 {
	return ((crc >> 15) | (crc << 17)) + 0xa282ead8
}

// rdbBlockChecksum is ComputeBuiltinChecksumWithLastByte: the checksum of
// data followed by one more byte (an SST block's compression-type byte, or
// a footer's own last byte for ComputeBuiltinChecksum - see
// rdbChecksumWhole).
func rdbBlockChecksum(typ byte, data []byte, last byte) uint32 {
	switch typ {
	case rdbCRC32c:
		c := crc32.Update(0, castagnoli, data)
		c = crc32.Update(c, castagnoli, []byte{last})
		return crc32cMask(c)
	case rdbXXHash:
		h := newXXH32(0)
		h.write(data)
		h.write([]byte{last})
		return h.sum()
	case rdbXXHash64:
		h := newXXH64(0)
		h.write(data)
		h.write([]byte{last})
		return uint32(h.sum())
	case rdbXXH3:
		return uint32(xxh3.Hash(data)) ^ uint32(last)*0x6b9083d9
	}
	return 0
}

// rdbChecksumWhole is ComputeBuiltinChecksum over all of data.
func rdbChecksumWhole(typ byte, data []byte) uint32 {
	if len(data) == 0 {
		if typ == rdbXXH3 {
			return 0
		}
		return rdbBlockChecksum(typ, nil, 0) // never used in practice
	}
	return rdbBlockChecksum(typ, data[:len(data)-1], data[len(data)-1])
}

// rdbChecksumModifier is ChecksumModifierForContext (table/format.h):
// format_version 6 adds this per-offset value to every block checksum, so
// a block copied to the wrong place in a file fails verification.
func rdbChecksumModifier(base uint32, offset uint64) uint32 {
	if base == 0 {
		return 0
	}
	return base ^ (uint32(offset) + uint32(offset>>32))
}

func crcUpdateCastagnoli(c uint32, p []byte) uint32 { return crc32.Update(c, castagnoli, p) }

// crc32cValue is the plain (unmasked) CRC-32C the WAL/MANIFEST record
// format uses before masking.
func crc32cValue(parts ...[]byte) uint32 {
	var c uint32
	for _, p := range parts {
		c = crc32.Update(c, castagnoli, p)
	}
	return c
}

// xxh32 is a minimal streaming XXH32 (seed 0 only matters here), used by
// RocksDB's kxxHash checksum type.
type xxh32 struct {
	v1, v2, v3, v4 uint32
	seed           uint32
	total          uint64
	buf            [16]byte
	n              int
}

const (
	p32_1 = 2654435761
	p32_2 = 2246822519
	p32_3 = 3266489917
	p32_4 = 668265263
	p32_5 = 374761393
)

func newXXH32(seed uint32) *xxh32 {
	h := &xxh32{seed: seed}
	h.v1 = seed + p32_1 + p32_2
	h.v2 = seed + p32_2
	h.v3 = seed
	h.v4 = seed - p32_1
	return h
}

func xxh32Round(acc, in uint32) uint32 {
	acc += in * p32_2
	acc = bits.RotateLeft32(acc, 13)
	return acc * p32_1
}

func (h *xxh32) write(p []byte) {
	h.total += uint64(len(p))
	if h.n > 0 {
		k := copy(h.buf[h.n:], p)
		h.n += k
		p = p[k:]
		if h.n < 16 {
			return
		}
		h.stripe(h.buf[:])
		h.n = 0
	}
	for len(p) >= 16 {
		h.stripe(p[:16])
		p = p[16:]
	}
	h.n = copy(h.buf[:], p)
}

func (h *xxh32) stripe(b []byte) {
	h.v1 = xxh32Round(h.v1, binary.LittleEndian.Uint32(b[0:]))
	h.v2 = xxh32Round(h.v2, binary.LittleEndian.Uint32(b[4:]))
	h.v3 = xxh32Round(h.v3, binary.LittleEndian.Uint32(b[8:]))
	h.v4 = xxh32Round(h.v4, binary.LittleEndian.Uint32(b[12:]))
}

func (h *xxh32) sum() uint32 {
	var acc uint32
	if h.total >= 16 {
		acc = bits.RotateLeft32(h.v1, 1) + bits.RotateLeft32(h.v2, 7) + bits.RotateLeft32(h.v3, 12) + bits.RotateLeft32(h.v4, 18)
	} else {
		acc = h.seed + p32_5
	}
	acc += uint32(h.total)
	b := h.buf[:h.n]
	for len(b) >= 4 {
		acc += binary.LittleEndian.Uint32(b) * p32_3
		acc = bits.RotateLeft32(acc, 17) * p32_4
		b = b[4:]
	}
	for _, c := range b {
		acc += uint32(c) * p32_5
		acc = bits.RotateLeft32(acc, 11) * p32_1
	}
	acc ^= acc >> 15
	acc *= p32_2
	acc ^= acc >> 13
	acc *= p32_3
	acc ^= acc >> 16
	return acc
}

// xxh64 is a minimal streaming XXH64, used by RocksDB's kxxHash64 checksum
// type (only its low 32 bits are stored).
type xxh64 struct {
	v1, v2, v3, v4 uint64
	seed           uint64
	total          uint64
	buf            [32]byte
	n              int
}

const (
	p64_1 uint64 = 11400714785074694791
	p64_2 uint64 = 14029467366897019727
	p64_3 uint64 = 1609587929392839161
	p64_4 uint64 = 9650029242287828579
	p64_5 uint64 = 2870177450012600261
)

func newXXH64(seed uint64) *xxh64 {
	h := &xxh64{seed: seed}
	h.v1 = seed + p64_1 + p64_2
	h.v2 = seed + p64_2
	h.v3 = seed
	h.v4 = seed - p64_1
	return h
}

func xxh64Round(acc, in uint64) uint64 {
	acc += in * p64_2
	acc = bits.RotateLeft64(acc, 31)
	return acc * p64_1
}

func xxh64Merge(acc, v uint64) uint64 {
	acc ^= xxh64Round(0, v)
	return acc*p64_1 + p64_4
}

func (h *xxh64) write(p []byte) {
	h.total += uint64(len(p))
	if h.n > 0 {
		k := copy(h.buf[h.n:], p)
		h.n += k
		p = p[k:]
		if h.n < 32 {
			return
		}
		h.stripe(h.buf[:])
		h.n = 0
	}
	for len(p) >= 32 {
		h.stripe(p[:32])
		p = p[32:]
	}
	h.n = copy(h.buf[:], p)
}

func (h *xxh64) stripe(b []byte) {
	h.v1 = xxh64Round(h.v1, binary.LittleEndian.Uint64(b[0:]))
	h.v2 = xxh64Round(h.v2, binary.LittleEndian.Uint64(b[8:]))
	h.v3 = xxh64Round(h.v3, binary.LittleEndian.Uint64(b[16:]))
	h.v4 = xxh64Round(h.v4, binary.LittleEndian.Uint64(b[24:]))
}

func (h *xxh64) sum() uint64 {
	var acc uint64
	if h.total >= 32 {
		acc = bits.RotateLeft64(h.v1, 1) + bits.RotateLeft64(h.v2, 7) + bits.RotateLeft64(h.v3, 12) + bits.RotateLeft64(h.v4, 18)
		acc = xxh64Merge(acc, h.v1)
		acc = xxh64Merge(acc, h.v2)
		acc = xxh64Merge(acc, h.v3)
		acc = xxh64Merge(acc, h.v4)
	} else {
		acc = h.seed + p64_5
	}
	acc += h.total
	b := h.buf[:h.n]
	for len(b) >= 8 {
		acc ^= xxh64Round(0, binary.LittleEndian.Uint64(b))
		acc = bits.RotateLeft64(acc, 27)*p64_1 + p64_4
		b = b[8:]
	}
	if len(b) >= 4 {
		acc ^= uint64(binary.LittleEndian.Uint32(b)) * p64_1
		acc = bits.RotateLeft64(acc, 23)*p64_2 + p64_3
		b = b[4:]
	}
	for _, c := range b {
		acc ^= uint64(c) * p64_5
		acc = bits.RotateLeft64(acc, 11) * p64_1
	}
	acc ^= acc >> 33
	acc *= p64_2
	acc ^= acc >> 29
	acc *= p64_3
	acc ^= acc >> 32
	return acc
}

// --- block compression ---

// RocksDB's CompressionType (include/rocksdb/compression_type.h).
const (
	rdbNoCompression     = 0x0
	rdbSnappy            = 0x1
	rdbZlib              = 0x2
	rdbBZip2             = 0x3
	rdbLZ4               = 0x4
	rdbLZ4HC             = 0x5
	rdbXpress            = 0x6
	rdbZSTD              = 0x7
	rdbZSTDNotFinal      = 0x40
	rdbMaxUncompressedSz = 1 << 30 // sanity cap on a single block's decompressed size
)

func rdbCompressionName(t byte) string {
	switch t {
	case rdbNoCompression:
		return "none"
	case rdbSnappy:
		return "Snappy"
	case rdbZlib:
		return "Zlib"
	case rdbBZip2:
		return "BZip2"
	case rdbLZ4:
		return "LZ4"
	case rdbLZ4HC:
		return "LZ4HC"
	case rdbXpress:
		return "Xpress"
	case rdbZSTD, rdbZSTDNotFinal:
		return "ZSTD"
	}
	return fmt.Sprintf("unknown(0x%x)", t)
}

var (
	zstdDecOnce sync.Once
	zstdDec     *zstd.Decoder
	zstdDecErr  error
)

func sharedZstdDecoder() (*zstd.Decoder, error) {
	zstdDecOnce.Do(func() {
		zstdDec, zstdDecErr = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxMemory(rdbMaxUncompressedSz))
	})
	return zstdDec, zstdDecErr
}

var (
	zstdDictMu   sync.Mutex
	zstdDictDecs = map[string]*zstd.Decoder{}
)

// zstdDecoderFor returns a decoder for blocks compressed against dict (an
// SST's "rocksdb.compression_dict" meta block): a trained zstd dictionary
// (magic number 0xEC30A437, carrying its own id) or, with
// use_zstd_dict_trainer=false, raw content. Cached per dictionary.
func zstdDecoderFor(dict []byte) (*zstd.Decoder, error) {
	if len(dict) == 0 {
		return sharedZstdDecoder()
	}
	zstdDictMu.Lock()
	defer zstdDictMu.Unlock()
	if d, ok := zstdDictDecs[string(dict)]; ok {
		return d, nil
	}
	var opt zstd.DOption
	if len(dict) >= 8 && binary.LittleEndian.Uint32(dict) == 0xEC30A437 {
		opt = zstd.WithDecoderDicts(dict)
	} else {
		opt = zstd.WithDecoderDictRaw(0, dict)
	}
	d, err := zstd.NewReader(nil, opt, zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxMemory(rdbMaxUncompressedSz))
	if err != nil {
		return nil, fmt.Errorf("can't load the file's ZSTD dictionary: %w", err)
	}
	zstdDictDecs[string(dict)] = d
	return d, nil
}

// rdbUncompress inflates one SST block's (or a compressed WAL record's)
// contents, as UncompressData does. formatVersion is the SST's own
// format_version: from 2 on (compress_format_version 2) every codec but
// Snappy - which records its own length - prefixes the payload with the
// uncompressed size as a varint32. dict is the file's compression
// dictionary meta block ("rocksdb.compression_dict"), or nil.
func rdbUncompress(typ byte, data []byte, formatVersion uint32, dict []byte) ([]byte, error) {
	switch typ {
	case rdbNoCompression:
		return data, nil
	case rdbSnappy:
		out, _, err := snappyDecode(data)
		return out, err
	}

	var outLen uint32
	if formatVersion >= 2 {
		v, n := rdbVarint32(data)
		if n == 0 {
			return nil, errors.New("bad uncompressed-size prefix")
		}
		outLen, data = v, data[n:]
	} else if typ == rdbLZ4 || typ == rdbLZ4HC {
		if len(data) < 8 {
			return nil, errors.New("LZ4 block too short")
		}
		outLen, data = binary.LittleEndian.Uint32(data), data[8:]
	}
	if outLen > rdbMaxUncompressedSz {
		return nil, fmt.Errorf("implausible uncompressed size %d", outLen)
	}

	switch typ {
	case rdbLZ4, rdbLZ4HC:
		out, _, err := lz4DecodeBlock(data, int(outLen), dict)
		return out, err
	case rdbZlib:
		out, err := inflateZlib(data, int(outLen))
		return out, err
	case rdbBZip2:
		out, err := io.ReadAll(io.LimitReader(bzip2.NewReader(bytes.NewReader(data)), rdbMaxUncompressedSz))
		if err != nil {
			return nil, err
		}
		if formatVersion >= 2 && uint32(len(out)) != outLen {
			return nil, fmt.Errorf("BZip2 block inflated to %d bytes, expected %d", len(out), outLen)
		}
		return out, nil
	case rdbZSTD, rdbZSTDNotFinal:
		d, err := zstdDecoderFor(dict)
		if err != nil {
			return nil, err
		}
		out, err := d.DecodeAll(data, make([]byte, 0, outLen))
		if err != nil {
			return nil, err
		}
		if formatVersion >= 2 && uint32(len(out)) != outLen {
			return nil, fmt.Errorf("ZSTD block inflated to %d bytes, expected %d", len(out), outLen)
		}
		return out, nil
	case rdbXpress:
		return nil, errors.New("Xpress (Windows-only) compression is not supported")
	}
	return nil, fmt.Errorf("unknown compression type 0x%x", typ)
}

// inflateZlib undoes Zlib_Compress: raw deflate by default (RocksDB's
// window_bits defaults to -14), or a zlib-wrapped stream when a positive
// window_bits was configured - tried in that order.
func inflateZlib(data []byte, outLen int) ([]byte, error) {
	out, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(data)), rdbMaxUncompressedSz))
	if err == nil && (outLen == 0 || len(out) == outLen) {
		return out, nil
	}
	zr, zerr := zlib.NewReader(bytes.NewReader(data))
	if zerr == nil {
		out2, err2 := io.ReadAll(io.LimitReader(zr, rdbMaxUncompressedSz))
		if err2 == nil && (outLen == 0 || len(out2) == outLen) {
			return out2, nil
		}
	}
	if err == nil {
		err = fmt.Errorf("Zlib block inflated to %d bytes, expected %d", len(out), outLen)
	}
	return nil, err
}

// lz4DecodeBlock decodes one raw LZ4 block (no frame) into exactly outLen
// bytes, as LZ4_decompress_safe(_continue) does - optionally against a
// preset dictionary - and returns how much of src it consumed (decoding
// stops once outLen bytes are produced, which salvage scanning relies on
// to find where a block ends).
func lz4DecodeBlock(src []byte, outLen int, dict []byte) (out []byte, consumed int, err error) {
	buf := make([]byte, len(dict), len(dict)+outLen)
	copy(buf, dict)
	base := len(dict)
	limit := base + outLen
	i := 0
	for {
		if i >= len(src) {
			return nil, 0, errors.New("LZ4: input ended mid-sequence")
		}
		token := src[i]
		i++
		lit := int(token >> 4)
		if lit == 15 {
			for {
				if i >= len(src) {
					return nil, 0, errors.New("LZ4: truncated literal length")
				}
				b := src[i]
				i++
				lit += int(b)
				if b != 255 {
					break
				}
			}
		}
		if i+lit > len(src) || len(buf)+lit > limit {
			return nil, 0, errors.New("LZ4: literal run overflows input or output")
		}
		buf = append(buf, src[i:i+lit]...)
		i += lit
		if len(buf) == limit {
			return buf[base:], i, nil // the last sequence is literals only
		}
		if i+2 > len(src) {
			return nil, 0, errors.New("LZ4: truncated match offset")
		}
		off := int(src[i]) | int(src[i+1])<<8
		i += 2
		ml := int(token & 15)
		if ml == 15 {
			for {
				if i >= len(src) {
					return nil, 0, errors.New("LZ4: truncated match length")
				}
				b := src[i]
				i++
				ml += int(b)
				if b != 255 {
					break
				}
			}
		}
		ml += 4
		if off == 0 || off > len(buf) {
			return nil, 0, fmt.Errorf("LZ4: bad match offset %d", off)
		}
		if len(buf)+ml > limit {
			return nil, 0, errors.New("LZ4: match overflows output")
		}
		start := len(buf) - off
		for k := 0; k < ml; k++ { // byte by byte: matches may overlap themselves
			buf = append(buf, buf[start+k])
		}
	}
}

// snappyDecode decodes one raw Snappy block (its own varint length prefix
// included) and returns how many bytes of src it consumed.
func snappyDecode(src []byte) (out []byte, consumed int, err error) {
	dLen, n := rdbVarint64(src)
	if n == 0 || dLen > rdbMaxUncompressedSz {
		return nil, 0, errors.New("Snappy: bad length prefix")
	}
	dst := make([]byte, 0, dLen)
	i := n
	for len(dst) < int(dLen) {
		if i >= len(src) {
			return nil, 0, errors.New("Snappy: input ended early")
		}
		tag := src[i]
		var length, offset int
		switch tag & 3 {
		case 0: // literal
			x := int(tag >> 2)
			i++
			switch {
			case x < 60:
			case x == 60:
				if i+1 > len(src) {
					return nil, 0, errors.New("Snappy: truncated literal")
				}
				x = int(src[i])
				i++
			case x == 61:
				if i+2 > len(src) {
					return nil, 0, errors.New("Snappy: truncated literal")
				}
				x = int(src[i]) | int(src[i+1])<<8
				i += 2
			case x == 62:
				if i+3 > len(src) {
					return nil, 0, errors.New("Snappy: truncated literal")
				}
				x = int(src[i]) | int(src[i+1])<<8 | int(src[i+2])<<16
				i += 3
			default:
				if i+4 > len(src) {
					return nil, 0, errors.New("Snappy: truncated literal")
				}
				x = int(binary.LittleEndian.Uint32(src[i:]))
				i += 4
			}
			length = x + 1
			if length <= 0 || i+length > len(src) || len(dst)+length > int(dLen) {
				return nil, 0, errors.New("Snappy: literal overflows input or output")
			}
			dst = append(dst, src[i:i+length]...)
			i += length
			continue
		case 1:
			if i+2 > len(src) {
				return nil, 0, errors.New("Snappy: truncated copy")
			}
			length = 4 + int(tag>>2)&7
			offset = int(tag&0xe0)<<3 | int(src[i+1])
			i += 2
		case 2:
			if i+3 > len(src) {
				return nil, 0, errors.New("Snappy: truncated copy")
			}
			length = 1 + int(tag>>2)
			offset = int(src[i+1]) | int(src[i+2])<<8
			i += 3
		case 3:
			if i+5 > len(src) {
				return nil, 0, errors.New("Snappy: truncated copy")
			}
			length = 1 + int(tag>>2)
			offset = int(binary.LittleEndian.Uint32(src[i+1:]))
			i += 5
		}
		if offset <= 0 || offset > len(dst) || len(dst)+length > int(dLen) {
			return nil, 0, errors.New("Snappy: bad copy")
		}
		start := len(dst) - offset
		for k := 0; k < length; k++ {
			dst = append(dst, dst[start+k])
		}
	}
	return dst, i, nil
}

// zstdFrameLength walks one zstd frame's block headers (without
// decompressing anything) to find where it ends - for salvage scanning,
// which has to find a compressed block's end before its trailer can be
// checked. Returns 0 if src doesn't start with a well-formed frame.
func zstdFrameLength(src []byte) int {
	if len(src) < 6 || binary.LittleEndian.Uint32(src) != 0xFD2FB528 {
		return 0
	}
	fhd := src[4]
	dictIDFlag := fhd & 3
	checksumFlag := fhd>>2&1 != 0
	singleSegment := fhd>>5&1 != 0
	fcsFlag := fhd >> 6
	pos := 5
	if !singleSegment {
		pos++ // window descriptor
	}
	pos += [4]int{0, 1, 2, 4}[dictIDFlag]
	fcs := [4]int{0, 2, 4, 8}[fcsFlag]
	if fcsFlag == 0 && singleSegment {
		fcs = 1
	}
	pos += fcs
	for {
		if pos+3 > len(src) {
			return 0
		}
		bh := uint32(src[pos]) | uint32(src[pos+1])<<8 | uint32(src[pos+2])<<16
		pos += 3
		last := bh&1 != 0
		btype := (bh >> 1) & 3
		bsize := int(bh >> 3)
		switch btype {
		case 0, 2: // raw, compressed
			pos += bsize
		case 1: // RLE
			pos++
		default:
			return 0
		}
		if btype == 2 && bsize == 0 {
			return 0
		}
		if pos > len(src) {
			return 0
		}
		if last {
			break
		}
	}
	if checksumFlag {
		pos += 4
	}
	if pos > len(src) {
		return 0
	}
	return pos
}

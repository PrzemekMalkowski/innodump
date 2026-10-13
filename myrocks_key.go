// myrocks_key.go: decoding a MyRocks primary key back into column values.
//
// MyRocks stores a row as one RocksDB key/value pair: the key is the
// 4-byte index number followed by every PRIMARY KEY column in a
// "mem-comparable" form (so plain bytewise key order is the index order),
// the value the rest of the row (myrocks.go). Whenever a key column can be
// turned back into its original value from that form, MyRocks leaves it
// out of the value entirely - so reading a row means decoding its key too.
//
// What can be decoded, and how, is decided per column exactly as
// Rdb_field_packing::setup (storage/rocksdb/rdb_datadic.cc) decides it:
//
//   - integers: big-endian, sign bit flipped (signed types);
//   - FLOAT/DOUBLE: IEEE bits rearranged to sort;
//   - DECIMAL/DATETIME2/TIMESTAMP2/TIME2/YEAR/BIT/BINARY(n): verbatim;
//   - DATE: its 3 bytes reversed; ENUM/SET: unsigned big-endian;
//   - VARBINARY: 8-byte groups, each followed by a length/flag byte;
//   - strings in a "_bin" collation: the code points (1, 2 or 3 bytes per
//     character), VARCHAR in "space-padded" segments plus a count of
//     trailing spaces kept in the value's unpack_info;
//   - strings in a simple 8-bit collation (latin1_swedish_ci, ...): the
//     collation weights, plus a few bits per character in unpack_info
//     saying which of the bytes sharing that weight it really was;
//   - strings in any other collation (utf8mb4_0900_ai_ci, ...): from
//     kv_format_version 15 on, a verbatim copy in unpack_info; before
//     that, not decodable from the key - such a column is stored in the
//     value instead, like a prefix-indexed one.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MyRocks kv_format_version thresholds (Rdb_key_def in rdb_datadic.h).
const (
	mrPKFormatUpdate2 = 12 // new VARBINARY escape format
	mrPKFormatUpdate3 = 14 // space-pad lead byte
	mrPKFormatColl    = 15 // unknown collations decodable (value copied into unpack_info)
)

type mrStorage int

const (
	mrStoreAll  mrStorage = iota // the value holds the column
	mrStoreSome                  // decoded from the key, with unpack_info
	mrStoreNone                  // decoded from the key alone
)

type mrKeyKind int

const (
	mkNone           mrKeyKind = iota // not decodable: only skipped
	mkInt                             // integer types
	mkFloat                           // FLOAT
	mkDouble                          // DOUBLE
	mkCopy                            // verbatim image (DECIMAL, temporal2, YEAR, BIT, BINARY(n), 8-bit _bin CHAR)
	mkNewdate                         // DATE
	mkEnumSet                         // ENUM/SET
	mkVarbinary                       // VARBINARY (binary charset VARCHAR)
	mkBinVarchar                      // VARCHAR in a _bin collation (space-padded segments)
	mkBinChar                         // CHAR in utf8mb3_bin/utf8mb4_bin (code points)
	mkSimpleVarchar                   // VARCHAR in a simple 8-bit collation
	mkSimpleChar                      // CHAR in a simple 8-bit collation
	mkUnknownVarchar                  // VARCHAR in another collation: value in unpack_info
	mkUnknownChar                     // CHAR in another collation: value in unpack_info
)

type mrSkip int

const (
	skipFixed    mrSkip = iota // ImageLen bytes
	skipVarlen                 // RDB_ESCAPE_LENGTH groups
	skipSpacePad               // space-padded segments
)

// mrKeyPart is one PRIMARY KEY column's packing, as setup() works it out.
type mrKeyPart struct {
	Col      *Column
	FieldIdx int // index into the layout's Fields
	KeyLen   int // m_max_field_bytes: the key part's length in bytes
	Kind     mrKeyKind
	Storage  mrStorage
	Skip     mrSkip
	ImageLen int // fixed image length (skipFixed); -1 if unknown
	Nullable bool

	SegSize        int  // space-pad segment size (9 or 10)
	WeightBytes    int  // bytes per weight in a _bin image: 1, 2 or 3
	SpaceXfrmLen   int  // bytes of strxfrm(" ")
	LeadByte       bool // space-pad encoding starts with a marker byte (kv >= 14)
	TwoByteUnpack  bool // trailing-space count takes 2 bytes in unpack_info
	LegacyVarlen   bool // pre-kv-12 VARBINARY escape format
	StoresValue    bool // m_unpack_info_stores_value
	UsesUnpackInfo bool
	LenBytes       int // VARCHAR length-prefix bytes (unknown-collation copy)
	PackLen        int
	Codec          *simpleCodec
	Unsigned       bool
}

// simpleCodec is Rdb_collation_codec for one simple 8-bit collation.
type simpleCodec struct {
	encSize [256]uint8
	decSize [256]uint8
	decIdx  [][256]byte
}

var simpleCodecs = map[uint64]*simpleCodec{}

// simpleCodecFor builds rdb_init_collation_mapping's reverse mapping for
// collation id: for each weight, the source bytes sharing it, in
// ascending order, and how many bits it takes to pick one.
func simpleCodecFor(id uint64) *simpleCodec {
	if c, ok := simpleCodecs[id]; ok {
		return c
	}
	so, ok := simpleCollationSortOrder[id]
	if !ok {
		return nil
	}
	rev := map[byte][]byte{}
	maxConflict := 0
	for src := 0; src < 256; src++ {
		dst := so[src]
		rev[dst] = append(rev[dst], byte(src))
		if len(rev[dst]) > maxConflict {
			maxConflict = len(rev[dst])
		}
	}
	c := &simpleCodec{decIdx: make([][256]byte, maxConflict)}
	for dst, srcs := range rev {
		bits := uint8(0)
		for (1 << bits) < len(srcs) {
			bits++
		}
		for idx, src := range srcs {
			c.encSize[src] = bits
			c.decSize[dst] = bits
			c.decIdx[idx][dst] = src
		}
	}
	simpleCodecs[id] = c
	return c
}

// collationKind classifies a string column's collation the way
// Rdb_field_packing::setup branches on it.
type collationClass int

const (
	ccBinary  collationClass = iota // my_charset_bin (BINARY/VARBINARY/BLOB)
	ccBin                           // a PAD SPACE "_bin" collation MyRocks special-cases
	ccSimple                        // my_collation_8bit_simple_ci_handler
	ccUnknown                       // anything else
)

const (
	collUtf8mb4Bin = 46
	collUtf8mb3Bin = 83
)

func classifyCollation(id uint64) collationClass {
	switch {
	case id == 63:
		return ccBinary
	case id == collUtf8mb4Bin || id == collUtf8mb3Bin || eightBitBinCollations[id]:
		return ccBin
	case simpleCollationSortOrder[id] != nil:
		return ccSimple
	}
	return ccUnknown
}

// collationNoPad reports a NO PAD collation (all of MySQL 8.0's
// *_0900_* ones, and MariaDB's *_nopad_*).
func collationNoPad(id uint64) bool {
	info, ok := collationInfoFor(id)
	if !ok {
		return false
	}
	return strings.Contains(info.name, "_0900_") || strings.Contains(info.name, "nopad") || strings.HasSuffix(info.name, "_0900_bin")
}

// collationLevels is levels_for_compare: 1 for nearly everything; 2 or 3
// for accent/case-sensitive UCA 9.0.0 collations and the two Czech ones.
func collationLevels(id uint64) int {
	info, ok := collationInfoFor(id)
	if !ok {
		return 1
	}
	switch {
	case strings.Contains(info.name, "_as_cs"):
		return 3
	case strings.Contains(info.name, "_as_ci"):
		return 2
	case info.name == "latin2_czech_cs" || info.name == "cp1250_czech_cs":
		return 2
	}
	return 1
}

// strnxfrmLen is cs->coll->strnxfrmlen(cs, n) - the mem-comparable image
// length of an n-byte CHAR - for the collation families MyRocks tables
// use; ok=false if unknown.
func strnxfrmLen(id uint64, n int) (int, bool) {
	switch classifyCollation(id) {
	case ccBinary, ccSimple:
		return n, true
	case ccBin:
		switch id {
		case collUtf8mb4Bin:
			return (n + 3) / 4 * 3, true
		case collUtf8mb3Bin:
			return n / 3 * 2, true
		}
		return n, true
	}
	info, ok := collationInfoFor(id)
	if !ok {
		return 0, false
	}
	switch {
	case strings.Contains(info.name, "_0900_"):
		// my_strnxfrmlen_uca_900: up to 8 weights of 2 bytes per code
		// point per level, plus a level separator between levels.
		levels := collationLevels(id)
		cps := n / 4
		w := cps * 8 * levels
		return (w + levels - 1) * 2, true
	case strings.HasPrefix(info.name, "utf8"), strings.HasPrefix(info.name, "ucs2"), strings.HasPrefix(info.name, "utf16"), strings.HasPrefix(info.name, "utf32"):
		// my_strnxfrmlen_unicode / UCA 4.0.0 (2-byte weights per char,
		// up to 8 per char for UCA collations: strxfrm_multiply)
		mb := info.max
		if mb == 0 {
			mb = 1
		}
		if strings.Contains(info.name, "_general_") {
			return n / mb * 2, true
		}
		return n / mb * 2 * 8, true
	}
	return 0, false
}

// mrRules selects which MyRocks generation's Rdb_field_packing::setup a
// table's rows were written under: the rules changed between the 5.6/5.7
// MyRocks (tables described by a .frm) and the 8.0 one (by an SDI) in
// ways kv_format_version doesn't record - see setupKeyPart.
type mrRules int

const (
	mrRules80 mrRules = iota // Percona Server 8.0+
	mrRules57                // Percona Server 5.7 / Facebook MySQL 5.6
)

// segmentSize is get_segment_size_from_collation: 10 for the collations
// whose weights are 3 bytes per character (3 per segment plus the marker
// byte), 9 for everything else.
func segmentSize(id uint64) int {
	switch id {
	case collUtf8mb4Bin, 55, 61, 62: // utf8mb4_bin, utf16_bin, utf32_bin, utf16le_bin
		return 10
	}
	return 9
}

// setupKeyPart mirrors Rdb_field_packing::setup for one PRIMARY KEY part
// of a MyRocks PRIMARY index with the given kv_format_version. Under the
// 5.7 rules, ENUM/SET/BIT key columns can't be decoded from the key (so
// they're kept in the value), and only latin1_bin and utf8_bin count as
// binary collations (utf8mb4_bin and the other 8-bit _bin ones don't).
func setupKeyPart(col *Column, keyLen, packLen int, kv int, rules mrRules) (mrKeyPart, error) {
	kp := mrKeyPart{Col: col, KeyLen: keyLen, PackLen: packLen, Nullable: col.IsNullable, Unsigned: col.IsUnsigned,
		Skip: skipFixed, ImageLen: packLen}
	indexOnly := kv >= mrPKFormatColl
	kp.LegacyVarlen = kv < mrPKFormatUpdate2
	kp.LeadByte = kv >= mrPKFormatUpdate3

	switch col.DDType {
	case ddLonglong, ddLong, ddInt24, ddShort, ddTiny:
		kp.Kind = mkInt
	case ddDouble:
		kp.Kind = mkDouble
	case ddFloat:
		kp.Kind = mkFloat
	case ddBit:
		kp.Kind = mkCopy
		if rules == mrRules57 {
			kp.Kind, kp.Storage = mkNone, mrStoreAll
			return kp, nil
		}
	case ddNewdecimal, ddDatetime2, ddTimestamp2, ddTime2, ddYear:
		kp.Kind = mkCopy
	case ddNewdate:
		kp.Kind = mkNewdate
	case ddEnum, ddSet:
		kp.Kind = mkEnumSet
		if rules == mrRules57 {
			kp.Kind, kp.Storage = mkNone, mrStoreAll
			return kp, nil
		}
	case ddVarchar, ddString, ddVarString, ddTinyBlob, ddBlob, ddMediumBlob, ddLongBlob:
		// handled below
	default:
		return kp, fmt.Errorf("column %q: type code %d can't be part of a MyRocks key", col.Name, col.DDType)
	}
	if kp.Kind != mkNone {
		kp.Storage = mrStoreNone
		return kp, nil
	}

	isVar := col.DDType == ddVarchar || isBlobFamily(col.DDType)
	isBlob := isBlobFamily(col.DDType)
	cls := classifyCollation(col.CollationID)
	if rules == mrRules57 && cls == ccBin && col.CollationID != 47 && col.CollationID != collUtf8mb3Bin {
		cls = ccUnknown // only latin1_bin and utf8_bin were special-cased then
	}
	if isVar {
		if col.DDType == ddVarchar {
			kp.LenBytes = 1
			if col.CharLength >= 256 {
				kp.LenBytes = 2
			}
		} else {
			kp.LenBytes = packLen - 8
		}
		kp.Skip = skipVarlen
		kp.TwoByteUnpack = keyLen+8 >= 0x100
	} else {
		il, ok := strnxfrmLen(col.CollationID, keyLen)
		kp.ImageLen = il
		if !ok {
			kp.ImageLen = -1
		}
	}

	switch cls {
	case ccBinary:
		if isVar {
			kp.Kind = mkVarbinary
		} else {
			kp.Kind = mkCopy
			kp.ImageLen = keyLen
		}
	case ccBin:
		if isVar {
			kp.Kind = mkBinVarchar
			kp.Skip = skipSpacePad
			kp.UsesUnpackInfo = true // dummy_make_unpack_info
			kp.SegSize = 9
			switch col.CollationID {
			case collUtf8mb4Bin:
				kp.WeightBytes, kp.SegSize = 3, 10
			case collUtf8mb3Bin:
				kp.WeightBytes = 2
			default:
				kp.WeightBytes = 1
			}
			kp.SpaceXfrmLen = kp.WeightBytes
		} else {
			switch col.CollationID {
			case collUtf8mb4Bin:
				kp.Kind, kp.WeightBytes = mkBinChar, 3
			case collUtf8mb3Bin:
				kp.Kind, kp.WeightBytes = mkBinChar, 2
			default:
				kp.Kind = mkCopy
			}
		}
	default:
		levels := collationLevels(col.CollationID)
		if isVar && levels == 1 {
			if indexOnly && collationNoPad(col.CollationID) {
				kp.Skip = skipVarlen
			} else {
				kp.Skip = skipSpacePad
				kp.SegSize = segmentSize(col.CollationID)
			}
		}
		if cls == ccSimple {
			kp.Codec = simpleCodecFor(col.CollationID)
			kp.SpaceXfrmLen = 1
			kp.UsesUnpackInfo = true
			if isVar {
				kp.Kind = mkSimpleVarchar
			} else {
				kp.Kind = mkSimpleChar
			}
		} else if indexOnly {
			kp.StoresValue = true
			kp.UsesUnpackInfo = true
			if isVar {
				kp.Kind = mkUnknownVarchar
			} else {
				kp.Kind = mkUnknownChar
			}
		} else {
			kp.Kind = mkNone
		}
	}

	// A prefix key part (or any BLOB/TEXT one) can never be decoded.
	fieldLen := int(col.CharLength) // bytes, for CHAR/VARCHAR/BINARY/VARBINARY
	if isBlob || fieldLen != keyLen {
		kp.Kind = mkNone
		kp.UsesUnpackInfo = false
		kp.StoresValue = true
	}

	switch {
	case kp.Kind == mkNone:
		kp.Storage = mrStoreAll
	case kp.UsesUnpackInfo:
		kp.Storage = mrStoreSome
	default:
		kp.Storage = mrStoreNone
	}
	return kp, nil
}

// --- readers ---

type mrReader struct {
	b   []byte
	pos int
}

func (r *mrReader) read(n int) ([]byte, bool) {
	if n < 0 || r.pos+n > len(r.b) {
		return nil, false
	}
	v := r.b[r.pos : r.pos+n]
	r.pos += n
	return v, true
}

func (r *mrReader) remaining() int { return len(r.b) - r.pos }

// mrBitReader is Rdb_bit_reader: bits taken LSB-first from bytes pulled
// lazily from r; a fresh one starts at the next whole byte.
type mrBitReader struct {
	r   *mrReader
	cur byte
	off uint
}

func (br *mrBitReader) read(size uint) (uint, bool) {
	var ret uint
	for size > 0 {
		if br.off == 0 {
			b, ok := br.r.read(1)
			if !ok {
				return 0, false
			}
			br.cur = b[0]
		}
		bits := 8 - br.off
		if size < bits {
			bits = size
		}
		ret <<= bits
		ret |= uint(br.cur>>br.off) & (1<<bits - 1)
		size -= bits
		br.off = (br.off + bits) & 7
	}
	return ret, true
}

const (
	cmpLessThanSpaces    = 1
	cmpEqualToSpaces     = 2
	cmpGreaterThanSpaces = 3
	rdbEscapeLength      = 9
)

var errMRCorrupt = errors.New("malformed key")

// skipPart skips one key part's image (Rdb_convert_to_record_key_decoder::skip
// plus its m_skip_func), including the trailing-space count a space-padded
// part may have left in unpack_info.
func (kp *mrKeyPart) skipPart(r *mrReader, unp *mrReader) error {
	if kp.Nullable {
		b, ok := r.read(1)
		if !ok {
			return errMRCorrupt
		}
		if b[0] == 0 {
			return nil
		}
		if b[0] != 1 {
			return errMRCorrupt
		}
	}
	if err := kp.skipImage(r); err != nil {
		return err
	}
	if kp.Kind == mkUnknownVarchar && unp != nil && kp.LenBytes > 0 {
		lb, ok := unp.read(kp.LenBytes)
		if !ok {
			return errMRCorrupt
		}
		if _, ok := unp.read(int(decodeUnsignedLE(lb))); !ok {
			return errMRCorrupt
		}
	}
	if kp.Skip == skipSpacePad && !kp.StoresValue && unp != nil {
		n := 1
		if kp.TwoByteUnpack {
			n = 2
		}
		unp.read(n)
	}
	return nil
}

func (kp *mrKeyPart) skipImage(r *mrReader) error {
	switch kp.Skip {
	case skipFixed:
		if kp.ImageLen < 0 {
			return fmt.Errorf("column %q: can't tell how long its key image is (collation id %d)", kp.Col.Name, kp.Col.CollationID)
		}
		if _, ok := r.read(kp.ImageLen); !ok {
			return errMRCorrupt
		}
		return nil
	case skipVarlen:
		_, err := kp.readVarlen(r, false)
		return err
	case skipSpacePad:
		if kp.LeadByte {
			b, ok := r.read(1)
			if !ok {
				return errMRCorrupt
			}
			if b[0] == cmpEqualToSpaces {
				return nil
			}
		}
		for {
			seg, ok := r.read(kp.SegSize)
			if !ok {
				return errMRCorrupt
			}
			switch seg[kp.SegSize-1] {
			case cmpEqualToSpaces:
				return nil
			case cmpLessThanSpaces, cmpGreaterThanSpaces:
			default:
				return errMRCorrupt
			}
		}
	}
	return nil
}

// readVarlen decodes pack_variable_format / pack_legacy_variable_format:
// 8-byte groups, each followed by a flag byte.
func (kp *mrKeyPart) readVarlen(r *mrReader, keep bool) ([]byte, error) {
	var out []byte
	for {
		g, ok := r.read(rdbEscapeLength)
		if !ok {
			return nil, errMRCorrupt
		}
		flag := int(g[rdbEscapeLength-1])
		var used int
		done := false
		if kp.LegacyVarlen {
			used = rdbEscapeLength - 1 - (255 - flag)
			if used < 0 || used > rdbEscapeLength-1 {
				return nil, errMRCorrupt
			}
			done = used < rdbEscapeLength-1
		} else {
			if flag > rdbEscapeLength {
				return nil, errMRCorrupt
			}
			if flag < rdbEscapeLength {
				used, done = flag, true
			} else {
				used = rdbEscapeLength - 1
			}
		}
		if keep {
			out = append(out, g[:used]...)
		}
		if done {
			return out, nil
		}
	}
}

// decodePart decodes one key part into the column's raw value in MySQL's
// own record format (little-endian integers etc.; for VARCHAR/BLOB just
// the data bytes) - the same bytes the value would have held - or
// isNull. unp is nil when the row's value carries no unpack_info.
func (kp *mrKeyPart) decodePart(r *mrReader, unp *mrReader) (raw []byte, isNull bool, err error) {
	if kp.Nullable {
		b, ok := r.read(1)
		if !ok {
			return nil, false, errMRCorrupt
		}
		if b[0] == 0 {
			return nil, true, nil
		}
		if b[0] != 1 {
			return nil, false, errMRCorrupt
		}
	}
	if kp.UsesUnpackInfo && unp == nil && kp.Kind != mkSimpleChar && kp.Kind != mkSimpleVarchar {
		return nil, false, fmt.Errorf("column %q: the row has no unpack_info, but this key column needs it", kp.Col.Name)
	}
	switch kp.Kind {
	case mkInt:
		b, ok := r.read(kp.PackLen)
		if !ok {
			return nil, false, errMRCorrupt
		}
		out := make([]byte, len(b))
		for i := range b {
			out[i] = b[len(b)-1-i]
		}
		if !kp.Unsigned {
			out[len(out)-1] ^= 0x80
		}
		return out, false, nil
	case mkFloat, mkDouble:
		size := 8
		expDigits := 11
		if kp.Kind == mkFloat {
			size, expDigits = 4, 8
		}
		b, ok := r.read(size)
		if !ok {
			return nil, false, errMRCorrupt
		}
		return unpackFloatingPoint(b, size, expDigits), false, nil
	case mkCopy:
		n := kp.ImageLen
		if n < 0 {
			n = kp.PackLen
		}
		b, ok := r.read(n)
		if !ok {
			return nil, false, errMRCorrupt
		}
		return append([]byte(nil), b...), false, nil
	case mkNewdate:
		b, ok := r.read(3)
		if !ok {
			return nil, false, errMRCorrupt
		}
		return []byte{b[2], b[1], b[0]}, false, nil
	case mkEnumSet:
		b, ok := r.read(kp.PackLen)
		if !ok {
			return nil, false, errMRCorrupt
		}
		out := make([]byte, len(b))
		for i := range b {
			out[i] = b[len(b)-1-i]
		}
		return out, false, nil
	case mkVarbinary:
		v, err := kp.readVarlen(r, true)
		return v, false, err
	case mkBinChar:
		b, ok := r.read(kp.ImageLen)
		if !ok {
			return nil, false, errMRCorrupt
		}
		out, err := weightsToUTF8(b, kp.WeightBytes)
		if err != nil {
			return nil, false, err
		}
		for len(out) < kp.PackLen {
			out = append(out, ' ')
		}
		return out, false, nil
	case mkBinVarchar, mkSimpleVarchar:
		v, err := kp.decodeSpacePad(r, unp)
		return v, false, err
	case mkSimpleChar:
		b, ok := r.read(kp.ImageLen)
		if !ok {
			return nil, false, errMRCorrupt
		}
		var br *mrBitReader
		if unp != nil {
			br = &mrBitReader{r: unp}
		}
		out, err := kp.Codec.decode(b, br)
		return out, false, err
	case mkUnknownChar:
		if err := kp.skipImage(r); err != nil {
			return nil, false, err
		}
		b, ok := unp.read(kp.PackLen)
		if !ok {
			return nil, false, errMRCorrupt
		}
		return append([]byte(nil), b...), false, nil
	case mkUnknownVarchar:
		if err := kp.skipImage(r); err != nil {
			return nil, false, err
		}
		lb, ok := unp.read(kp.LenBytes)
		if !ok {
			return nil, false, errMRCorrupt
		}
		n := int(decodeUnsignedLE(lb))
		b, ok := unp.read(n)
		if !ok || n > kp.KeyLen {
			return nil, false, errMRCorrupt
		}
		return append([]byte(nil), b...), false, nil
	}
	return nil, false, fmt.Errorf("column %q can't be decoded from the key", kp.Col.Name)
}

// decodeSpacePad is unpack_binary_or_utf8_varlength_space_pad /
// unpack_simple_varlength_space_pad.
func (kp *mrKeyPart) decodeSpacePad(r *mrReader, unp *mrReader) ([]byte, error) {
	if unp == nil {
		return nil, fmt.Errorf("column %q: the row has no unpack_info, but this key column needs it", kp.Col.Name)
	}
	var extra int
	if kp.TwoByteUnpack {
		b, ok := unp.read(2)
		if !ok {
			return nil, errMRCorrupt
		}
		extra = int(binary.BigEndian.Uint16(b))
	} else {
		b, ok := unp.read(1)
		if !ok {
			return nil, errMRCorrupt
		}
		extra = int(b[0])
	}
	padBytes := 0
	if extra <= 8 {
		padBytes = (8 - extra) * kp.SpaceXfrmLen
		extra = 0
	} else {
		extra -= 8
	}
	var br *mrBitReader
	if kp.Kind == mkSimpleVarchar {
		br = &mrBitReader{r: unp}
	}
	var out []byte
	finished := false
	if kp.LeadByte {
		b, ok := r.read(1)
		if !ok {
			return nil, errMRCorrupt
		}
		if b[0] == cmpEqualToSpaces {
			finished = true
		}
	}
	for !finished {
		seg, ok := r.read(kp.SegSize)
		if !ok {
			return nil, errMRCorrupt
		}
		last := seg[kp.SegSize-1]
		used := kp.SegSize - 1
		switch last {
		case cmpEqualToSpaces:
			if padBytes > kp.SegSize-1 {
				return nil, errMRCorrupt
			}
			used = kp.SegSize - 1 - padBytes
			finished = true
		case cmpLessThanSpaces, cmpGreaterThanSpaces:
		default:
			return nil, errMRCorrupt
		}
		chunk := seg[:used]
		switch {
		case kp.Kind == mkSimpleVarchar:
			d, err := kp.Codec.decode(chunk, br)
			if err != nil {
				return nil, err
			}
			out = append(out, d...)
		case kp.WeightBytes > 1:
			d, err := weightsToUTF8(chunk, kp.WeightBytes)
			if err != nil {
				return nil, err
			}
			out = append(out, d...)
		default:
			out = append(out, chunk...)
		}
	}
	for i := 0; i < extra; i++ {
		out = append(out, ' ')
	}
	if len(out) > kp.KeyLen {
		return nil, errMRCorrupt
	}
	return out, nil
}

// decode maps weights back to bytes (rdb_read_unpack_simple).
func (c *simpleCodec) decode(w []byte, br *mrBitReader) ([]byte, error) {
	out := make([]byte, len(w))
	for i, x := range w {
		if c.decSize[x] > 0 {
			if br == nil {
				return nil, errors.New("the row has no unpack_info, but this key column needs it")
			}
			idx, ok := br.read(uint(c.decSize[x]))
			if !ok || int(idx) >= len(c.decIdx) {
				return nil, errMRCorrupt
			}
			out[i] = c.decIdx[idx][x]
		} else {
			out[i] = c.decIdx[0][x]
		}
	}
	return out, nil
}

// weightsToUTF8 turns 2- or 3-byte big-endian code points back into UTF-8
// (wc_mb for utf8mb3/utf8mb4).
func weightsToUTF8(b []byte, n int) ([]byte, error) {
	if len(b)%n != 0 {
		return nil, errMRCorrupt
	}
	out := make([]byte, 0, len(b))
	var buf [4]byte
	for i := 0; i < len(b); i += n {
		var wc rune
		if n == 3 {
			wc = rune(b[i])<<16 | rune(b[i+1])<<8 | rune(b[i+2])
		} else {
			wc = rune(b[i])<<8 | rune(b[i+1])
		}
		k := utf8.EncodeRune(buf[:], wc)
		out = append(out, buf[:k]...)
	}
	return out, nil
}

// unpackFloatingPoint reverses change_double_for_sort / the FLOAT
// equivalent, returning the value's little-endian IEEE bytes.
func unpackFloatingPoint(from []byte, size, expDigits int) []byte {
	zero := true
	if from[0] != 0x80 {
		zero = false
	}
	for _, c := range from[1:] {
		if c != 0 {
			zero = false
		}
	}
	out := make([]byte, size)
	if zero {
		return out
	}
	tmp := append([]byte(nil), from...)
	if tmp[0]&0x80 != 0 {
		exp := uint16(tmp[0])<<8 | uint16(tmp[1])
		exp &= 0x7FFF
		exp -= 1 << (16 - 1 - expDigits)
		tmp[0], tmp[1] = byte(exp>>8), byte(exp)
	} else {
		for i := range tmp {
			tmp[i] ^= 0xFF
		}
	}
	for i := 0; i < size; i++ {
		out[i] = tmp[size-1-i]
	}
	return out
}

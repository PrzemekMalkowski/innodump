// values.go: turns one physical field's raw bytes into a MySQL SQL literal
// suitable for an INSERT statement.
//
// InnoDB stores fixed-width integers big-endian with the sign bit of the
// most-significant byte flipped (so unsigned memcmp gives correct numeric
// ordering for signed values too); everything else (floats, the packed
// temporal types, NEWDECIMAL, JSON, ENUM/SET) has its own encoding, mostly
// shared with MySQL's binlog row-image format (decimal.go/temporal.go) or
// decoded from first principles here (jsonb.go, enum/set/bit).
package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// decodeSignedBE reads a big-endian, sign-flipped n-byte InnoDB integer.
func decodeSignedBE(b []byte) int64 {
	n := len(b)
	buf := make([]byte, n)
	copy(buf, b)
	buf[0] ^= 0x80
	var v uint64
	for _, x := range buf {
		v = v<<8 | uint64(x)
	}
	shift := uint(64 - n*8)
	return int64(v<<shift) >> shift
}

// decodeUnsignedBE reads a plain big-endian n-byte unsigned integer.
func decodeUnsignedBE(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

// decodeField renders one non-NULL physical field as a SQL literal. sp is
// used to fetch externally-stored (off-page) values; it may be nil if fr is
// known not to be External (callers that already checked can skip it).
func decodeField(sp *Space, col *Column, page []byte, fr fieldRange) (lit string, err error) {
	defer func() {
		if r := recover(); r != nil {
			lit, err = "", fmt.Errorf("panic decoding column %q: %v", col.Name, r)
		}
	}()

	raw := page[fr.Start:fr.End]
	full := raw
	if fr.External {
		if len(raw) < btrExternFieldRefSize {
			return "", fmt.Errorf("column %q: external reference shorter than %d bytes", col.Name, btrExternFieldRefSize)
		}
		inline := raw[:len(raw)-btrExternFieldRefSize]
		ref, err := readExternalRef(page, fr.End)
		if err != nil {
			return "", fmt.Errorf("column %q: %w", col.Name, err)
		}
		off, err := fetchLOB(sp, ref)
		if err != nil {
			return "", fmt.Errorf("column %q: fetching external value: %w", col.Name, err)
		}
		full = append(append([]byte(nil), inline...), off...)
	}

	switch col.DDType {
	case ddTiny, ddShort, ddInt24, ddLong, ddLonglong:
		if col.IsUnsigned {
			return strconv.FormatUint(decodeUnsignedBE(full), 10), nil
		}
		return strconv.FormatInt(decodeSignedBE(full), 10), nil

	case ddYear:
		return decodeYear(full), nil
	case ddNewdate:
		return decodeNewDate(full), nil
	case ddDatetime2:
		return decodeDatetime2(full, int(col.DatetimePrec)), nil
	case ddTimestamp2:
		return decodeTimestamp2(full, int(col.DatetimePrec)), nil
	case ddTime2:
		return decodeTime2(full, int(col.DatetimePrec)), nil
	case ddDate, ddTime, ddDatetime, ddTimestamp:
		return "", fmt.Errorf("column %q uses a pre-5.6 temporal storage format that v1 does not decode", col.Name)

	case ddFloat:
		if len(full) < 4 {
			return "", fmt.Errorf("column %q: float value truncated", col.Name)
		}
		v := math.Float32frombits(binary.LittleEndian.Uint32(full))
		return strconv.FormatFloat(float64(v), 'g', -1, 32), nil
	case ddDouble:
		if len(full) < 8 {
			return "", fmt.Errorf("column %q: double value truncated", col.Name)
		}
		v := math.Float64frombits(binary.LittleEndian.Uint64(full))
		return strconv.FormatFloat(v, 'g', -1, 64), nil

	case ddNewdecimal:
		s, err := decodeDecimal(full, int(col.NumPrec), int(col.NumScale))
		if err != nil {
			return "", fmt.Errorf("column %q: %w", col.Name, err)
		}
		return s, nil
	case ddDecimal:
		return "", fmt.Errorf("column %q uses the legacy (pre-5.0) DECIMAL storage format, not supported", col.Name)

	case ddEnum:
		idx := decodeUnsignedBE(full)
		if idx == 0 {
			return "''", nil // MySQL's representation of an invalid/empty enum value
		}
		if int(idx) > len(col.Elements) {
			return "", fmt.Errorf("column %q: enum index %d out of range", col.Name, idx)
		}
		return quoteSQLString(col.Elements[idx-1]), nil
	case ddSet:
		bits := decodeUnsignedBE(full)
		var parts []string
		for i := 0; i < len(col.Elements); i++ {
			if bits&(1<<uint(i)) != 0 {
				parts = append(parts, col.Elements[i])
			}
		}
		return quoteSQLString(strings.Join(parts, ",")), nil
	case ddBit:
		return fmt.Sprintf("b'%s'", strconv.FormatUint(decodeUnsignedBE(full), 2)), nil

	case ddJSON:
		return quoteSQLString(jsonBinaryToText(full)), nil
	case ddVector:
		return hexLiteral(full), nil // MySQL 9.0+ type, out of v1's version scope
	case ddGeometry:
		return hexLiteral(full), nil // WKB payload; not decoded to WKT in v1

	case ddVarchar, ddVarString, ddString:
		if col.IsBinary() {
			return hexLiteral(full), nil
		}
		return quoteSQLString(string(full)), nil
	case ddTinyBlob, ddMediumBlob, ddBlob, ddLongBlob:
		if col.IsBinary() {
			return hexLiteral(full), nil
		}
		return quoteSQLString(string(full)), nil

	default:
		return "", fmt.Errorf("column %q has an unsupported type code %d", col.Name, col.DDType)
	}
}

func hexLiteral(b []byte) string {
	if len(b) == 0 {
		return "X''"
	}
	return fmt.Sprintf("X'%x'", b)
}

// quoteSQLString renders s as a single-quoted SQL string literal, escaping
// the characters MySQL treats specially in the default SQL mode.
func quoteSQLString(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\'':
			b.WriteString(`\'`)
		case '\\':
			b.WriteString(`\\`)
		case 0:
			b.WriteString(`\0`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case 0x1a: // Ctrl+Z, mangled by some Windows tools mid-stream
			b.WriteString(`\Z`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

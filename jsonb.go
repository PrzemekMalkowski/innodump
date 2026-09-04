// jsonb.go: decodes MySQL's internal binary JSON representation (the
// format actually stored in a JSON column, distinct from JSON text) back
// into JSON text. Ported from ibdNinja's JsonBinary.cc (itself referencing
// MySQL's sql-common/json_binary.h), translated to Go.
package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	jsonbSmallObject = 0x00
	jsonbLargeObject = 0x01
	jsonbSmallArray  = 0x02
	jsonbLargeArray  = 0x03
	jsonbLiteral     = 0x04
	jsonbInt16       = 0x05
	jsonbUint16      = 0x06
	jsonbInt32       = 0x07
	jsonbUint32      = 0x08
	jsonbInt64       = 0x09
	jsonbUint64      = 0x0A
	jsonbDouble      = 0x0B
	jsonbString      = 0x0C
	jsonbOpaque      = 0x0F

	jsonbNull  = 0x00
	jsonbTrue  = 0x01
	jsonbFalse = 0x02

	jsonbMaxDepth = 100
)

func readVariableLength(data []byte, pos int) (length uint32, consumed int, ok bool) {
	var v uint32
	for i := 0; i < 5 && pos+i < len(data); i++ {
		b := data[pos+i]
		v |= uint32(b&0x7f) << (7 * i)
		consumed++
		if b&0x80 == 0 {
			return v, consumed, true
		}
	}
	return 0, 0, false
}

func escapeJSONString(s []byte) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func jsonbReadOffsetOrSize(data []byte, large bool) uint32 {
	if large {
		return binary.LittleEndian.Uint32(data)
	}
	return uint32(binary.LittleEndian.Uint16(data))
}

func jsonbInlineable(typ byte, large bool) bool {
	switch typ {
	case jsonbLiteral, jsonbInt16, jsonbUint16:
		return true
	case jsonbInt32, jsonbUint32:
		return large
	default:
		return false
	}
}

func decodeJSONObjectOrArray(data []byte, offset int, isObject, large bool, depth int) string {
	if depth > jsonbMaxDepth {
		return `"<max_depth_exceeded>"`
	}
	offSize := 2
	if large {
		offSize = 4
	}
	if offset+2*offSize > len(data) {
		return `"<truncated>"`
	}
	count := int(jsonbReadOffsetOrSize(data[offset:], large))
	headerSize := 2 * offSize
	keyEntrySize := offSize + 2
	valueEntrySize := 1 + offSize
	keyEntriesOff := offset + headerSize
	valueEntriesOff := keyEntriesOff
	if isObject {
		valueEntriesOff += count * keyEntrySize
	}

	var out strings.Builder
	if isObject {
		out.WriteByte('{')
	} else {
		out.WriteByte('[')
	}
	for i := 0; i < count; i++ {
		if i > 0 {
			out.WriteByte(',')
		}
		if isObject {
			keOff := keyEntriesOff + i*keyEntrySize
			if keOff+keyEntrySize > len(data) {
				out.WriteString(`"<truncated>"`)
				break
			}
			keyOffset := int(jsonbReadOffsetOrSize(data[keOff:], large))
			keyLen := int(binary.LittleEndian.Uint16(data[keOff+offSize:]))
			absKeyOff := offset + keyOffset
			if absKeyOff+keyLen > len(data) || absKeyOff < 0 {
				out.WriteString(`"<truncated>"`)
				break
			}
			out.WriteString(escapeJSONString(data[absKeyOff : absKeyOff+keyLen]))
			out.WriteByte(':')
		}
		veOff := valueEntriesOff + i*valueEntrySize
		if veOff+valueEntrySize > len(data) {
			out.WriteString(`"<truncated>"`)
			break
		}
		valType := data[veOff]
		valOffOrInline := jsonbReadOffsetOrSize(data[veOff+1:], large)
		if jsonbInlineable(valType, large) {
			switch valType {
			case jsonbLiteral:
				switch byte(valOffOrInline) {
				case jsonbTrue:
					out.WriteString("true")
				case jsonbFalse:
					out.WriteString("false")
				default:
					out.WriteString("null")
				}
			case jsonbInt16:
				out.WriteString(strconv.FormatInt(int64(int16(valOffOrInline)), 10))
			case jsonbUint16:
				out.WriteString(strconv.FormatUint(uint64(uint16(valOffOrInline)), 10))
			case jsonbInt32:
				out.WriteString(strconv.FormatInt(int64(int32(valOffOrInline)), 10))
			case jsonbUint32:
				out.WriteString(strconv.FormatUint(uint64(valOffOrInline), 10))
			default:
				out.WriteString("null")
			}
		} else {
			out.WriteString(decodeJSONValue(data, valType, offset+int(valOffOrInline), depth+1))
		}
	}
	if isObject {
		out.WriteByte('}')
	} else {
		out.WriteByte(']')
	}
	return out.String()
}

func decodeJSONValue(data []byte, typ byte, off int, depth int) string {
	switch typ {
	case jsonbSmallObject:
		return decodeJSONObjectOrArray(data, off, true, false, depth)
	case jsonbLargeObject:
		return decodeJSONObjectOrArray(data, off, true, true, depth)
	case jsonbSmallArray:
		return decodeJSONObjectOrArray(data, off, false, false, depth)
	case jsonbLargeArray:
		return decodeJSONObjectOrArray(data, off, false, true, depth)
	case jsonbLiteral:
		if off >= len(data) {
			return "null"
		}
		switch data[off] {
		case jsonbTrue:
			return "true"
		case jsonbFalse:
			return "false"
		default:
			return "null"
		}
	case jsonbInt16:
		if off+2 > len(data) {
			return "0"
		}
		return strconv.FormatInt(int64(int16(binary.LittleEndian.Uint16(data[off:]))), 10)
	case jsonbUint16:
		if off+2 > len(data) {
			return "0"
		}
		return strconv.FormatUint(uint64(binary.LittleEndian.Uint16(data[off:])), 10)
	case jsonbInt32:
		if off+4 > len(data) {
			return "0"
		}
		return strconv.FormatInt(int64(int32(binary.LittleEndian.Uint32(data[off:]))), 10)
	case jsonbUint32:
		if off+4 > len(data) {
			return "0"
		}
		return strconv.FormatUint(uint64(binary.LittleEndian.Uint32(data[off:])), 10)
	case jsonbInt64:
		if off+8 > len(data) {
			return "0"
		}
		return strconv.FormatInt(int64(binary.LittleEndian.Uint64(data[off:])), 10)
	case jsonbUint64:
		if off+8 > len(data) {
			return "0"
		}
		return strconv.FormatUint(binary.LittleEndian.Uint64(data[off:]), 10)
	case jsonbDouble:
		if off+8 > len(data) {
			return "0.0"
		}
		v := math.Float64frombits(binary.LittleEndian.Uint64(data[off:]))
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return "null"
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	case jsonbString:
		strLen, consumed, ok := readVariableLength(data, off)
		if !ok {
			return `"<truncated>"`
		}
		start := off + consumed
		end := start + int(strLen)
		if end > len(data) {
			end = len(data)
		}
		if start > len(data) {
			start = len(data)
		}
		return escapeJSONString(data[start:end])
	case jsonbOpaque:
		if off >= len(data) {
			return `"<opaque:unknown>"`
		}
		mysqlType := data[off]
		if off+1 < len(data) {
			if oplen, _, ok := readVariableLength(data, off+1); ok {
				return fmt.Sprintf(`"<opaque:type=%d, %d bytes>"`, mysqlType, oplen)
			}
		}
		return fmt.Sprintf(`"<opaque:type=%d>"`, mysqlType)
	default:
		return fmt.Sprintf(`"<unknown_type:0x%02x>"`, typ)
	}
}

// jsonBinaryToText decodes MySQL's internal binary JSON representation
// (JSONB) into JSON text.
func jsonBinaryToText(data []byte) string {
	if len(data) == 0 {
		return "null"
	}
	return decodeJSONValue(data, data[0], 1, 0)
}

// decimal.go: decodes InnoDB's binary NEWDECIMAL encoding, the same
// "my_decimal2binary" format MySQL uses everywhere a DECIMAL/NEWDECIMAL
// value is stored on disk or on the wire (binlog row events included), so
// this is a direct port of the equivalent routine already validated in
// gcache-inspector (rows.go's decodeDecimal), not something specific to
// InnoDB row storage.
//
// Digits are packed in base 10^9 (4 bytes per 9 digits), with any leftover
// digits at each end packed into 1-4 bytes depending on count. The whole
// buffer's sign is encoded by flipping the top bit of the first byte (and,
// for a negative number, inverting every byte) so memcmp() sorts correctly.
package main

import (
	"fmt"
	"strings"
)

var dig2bytes = [10]int{0, 1, 1, 2, 2, 3, 3, 4, 4, 4}

// decodeDecimal reads a NEWDECIMAL value of the given precision/scale from
// d[0:], returning its text form (e.g. "-1234.567").
func decodeDecimal(d []byte, precision, scale int) (string, error) {
	if precision <= 0 || scale < 0 || scale > precision {
		return "", fmt.Errorf("invalid decimal(%d,%d)", precision, scale)
	}
	intg := precision - scale
	intg0, frac0 := intg/9, scale/9
	intg0x, frac0x := intg-intg0*9, scale-frac0*9
	total := intg0*4 + dig2bytes[intg0x] + frac0*4 + dig2bytes[frac0x]
	if total == 0 || total > len(d) {
		return "", fmt.Errorf("decimal(%d,%d) needs %d bytes, only %d available", precision, scale, total, len(d))
	}
	buf := make([]byte, total)
	copy(buf, d[:total])

	positive := buf[0]&0x80 != 0
	buf[0] ^= 0x80
	if !positive {
		for i := range buf {
			buf[i] ^= 0xff
		}
	}
	pos := 0
	be := func(n int) uint64 {
		var v uint64
		for i := 0; i < n; i++ {
			v = (v << 8) | uint64(buf[pos+i])
		}
		pos += n
		return v
	}
	var istr strings.Builder
	if intg0x > 0 {
		fmt.Fprintf(&istr, "%d", be(dig2bytes[intg0x]))
	}
	for i := 0; i < intg0; i++ {
		v := be(4)
		if istr.Len() == 0 {
			fmt.Fprintf(&istr, "%d", v)
		} else {
			fmt.Fprintf(&istr, "%09d", v)
		}
	}
	intPart := strings.TrimLeft(istr.String(), "0")
	if intPart == "" {
		intPart = "0"
	}
	var fstr strings.Builder
	for i := 0; i < frac0; i++ {
		fmt.Fprintf(&fstr, "%09d", be(4))
	}
	if frac0x > 0 {
		fmt.Fprintf(&fstr, "%0*d", frac0x, be(dig2bytes[frac0x]))
	}
	sign := ""
	if !positive {
		sign = "-"
	}
	out := sign + intPart
	if scale > 0 {
		out += "." + fstr.String()
	}
	return out, nil
}

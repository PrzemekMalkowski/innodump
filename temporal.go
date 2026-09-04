// temporal.go: decodes the "packed" temporal encodings MySQL has used since
// 5.6 for DATE/DATETIME(N)/TIMESTAMP(N)/TIME(N) columns - NEWDATE,
// DATETIME2, TIMESTAMP2, TIME2. These are stored big-endian both in InnoDB
// row storage and in binlog row events, so the decode formulas here are a
// direct reuse of the (already-validated) logic in gcache-inspector's
// rows.go, adjusted only to read from an arbitrary byte slice rather than a
// binlog event buffer.
package main

import "fmt"

func beUint(d []byte, n int) uint64 {
	var v uint64
	for i := 0; i < n; i++ {
		v = (v << 8) | uint64(d[i])
	}
	return v
}

// decodeNewDate unpacks a 3-byte NEWDATE value (day/month/year bit-packed
// into a plain integer, day/month/year never negative - but still stored as
// a DATA_INT, so InnoDB applies the usual sign-bit flip on the top byte;
// confirmed empirically, since a date decoded 16384 years too high pins the
// error to exactly that bit).
func decodeNewDate(d []byte) string {
	buf := []byte{d[0] ^ 0x80, d[1], d[2]}
	v := beUint(buf, 3)
	day := v & 0x1f
	month := (v >> 5) & 0xf
	year := v >> 9
	if v == 0 {
		return "'0000-00-00'"
	}
	return fmt.Sprintf("'%04d-%02d-%02d'", year, month, day)
}

func fracMicros(d []byte, decimals int) string {
	nb := (decimals + 1) / 2
	if nb == 0 {
		return ""
	}
	v := beUint(d[:nb], nb)
	return fmt.Sprintf("%0*d", decimals, v)
}

func decodeDatetime2(d []byte, decimals int) string {
	raw := beUint(d[:5], 5)
	v := int64(raw) - 0x8000000000
	dpart := v >> 17
	tpart := v & ((1 << 17) - 1)
	day := dpart & 0x1f
	month := (dpart >> 5) % 13
	year := (dpart >> 5) / 13
	sec := tpart & 0x3f
	minute := (tpart >> 6) & 0x3f
	hour := tpart >> 12
	s := fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", year, month, day, hour, minute, sec)
	if f := fracMicros(d[5:], decimals); f != "" {
		s += "." + f
	}
	return "'" + s + "'"
}

func decodeTimestamp2(d []byte, decimals int) string {
	sec := beUint(d[:4], 4)
	days := int64(sec) / 86400
	rem := int64(sec) % 86400
	y, mo, da := civilFromDays(days)
	h := rem / 3600
	mi := (rem % 3600) / 60
	se := rem % 60
	s := fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", y, mo, da, h, mi, se)
	if f := fracMicros(d[4:], decimals); f != "" {
		s += "." + f
	}
	return "'" + s + "'"
}

func decodeTime2(d []byte, decimals int) string {
	raw := beUint(d[:3], 3)
	v := int64(raw) - 0x800000
	neg := v < 0
	if neg {
		v = -v
	}
	hour := (v >> 12) & 0x3ff
	minute := (v >> 6) & 0x3f
	sec := v & 0x3f
	sign := ""
	if neg {
		sign = "-"
	}
	s := fmt.Sprintf("%s%02d:%02d:%02d", sign, hour, minute, sec)
	if f := fracMicros(d[3:], decimals); f != "" {
		s += "." + f
	}
	return "'" + s + "'"
}

func decodeYear(d []byte) string {
	if d[0] == 0 {
		return "0000"
	}
	return fmt.Sprintf("%d", int(d[0])+1900)
}

// civilFromDays converts days-since-Unix-epoch to a civil (y,m,d) date.
// Algorithm from Howard Hinnant's date library (public domain).
func civilFromDays(z int64) (int64, int64, int64) {
	z += 719468
	era := z
	if z < 0 {
		era = z - 146096
	}
	era /= 146097
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := doy - (153*mp+2)/5 + 1
	var m int64
	if mp < 10 {
		m = mp + 3
	} else {
		m = mp - 9
	}
	if m <= 2 {
		y++
	}
	return y, m, d
}

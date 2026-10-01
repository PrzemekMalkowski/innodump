// temporal.go: decodes the "packed" temporal encodings MySQL has used since
// 5.6 for DATE/DATETIME(N)/TIMESTAMP(N)/TIME(N) columns - NEWDATE,
// DATETIME2, TIMESTAMP2, TIME2. These are stored big-endian both in InnoDB
// row storage and in binlog row events, so the decode formulas here are a
// direct reuse of the (already-validated) logic in gcache-inspector's
// rows.go, adjusted only to read from an arbitrary byte slice rather than a
// binlog event buffer.
package main

import (
	"fmt"
	"strings"
)

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

// fracMicros renders the fractional-seconds bytes that follow a
// DATETIME2/TIMESTAMP2/TIME2 value's integer part. They hold an even number
// of digits - hundredths, ten-thousandths or microseconds in 1, 2 or 3
// bytes - so an odd precision (1, 3, 5) still stores one extra trailing
// digit (always 0 for a value the server wrote itself), dropped here so
// the literal has exactly `decimals` digits.
func fracMicros(d []byte, decimals int) string {
	nb := (decimals + 1) / 2
	if nb == 0 {
		return ""
	}
	v := beUint(d[:nb], nb)
	if decimals%2 == 1 {
		v /= 10
	}
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

// decodeTimestamp2 unpacks a TIMESTAMP2 value: seconds since the Unix
// epoch (UTC), big-endian, plus an optional fraction. A stored 0 is not
// the epoch itself but MySQL's zero timestamp, '0000-00-00 00:00:00' - the
// epoch's own first second can't be stored (TIMESTAMP's range starts at
// '1970-01-01 00:00:01' UTC), so the server reserves 0 for it
// (my_timestamp_from_binary / Field_timestampf::get_date_internal).
func decodeTimestamp2(d []byte, decimals int) string {
	sec := beUint(d[:4], 4)
	if sec == 0 {
		s := "0000-00-00 00:00:00"
		if decimals > 0 {
			s += "." + strings.Repeat("0", decimals)
		}
		return "'" + s + "'"
	}
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

// decodeTime2 unpacks a TIME2 value: a sign-biased 3-byte integer part
// (the hour<<12 | minute<<6 | second bitfield) followed by 0-3 bytes of
// fraction. The two together are one signed fixed-point number rather than
// a sign-and-magnitude pair (my_time_packed_to_binary in MySQL's
// my_time.c): for a negative time the integer part is rounded toward
// -infinity and the fraction holds the remainder modulo its own width -
// '-00:00:01.50' is stored as -2 and 206 (0x100 - 50). Undone here exactly
// as my_time_packed_from_binary does.
func decodeTime2(d []byte, decimals int) string {
	intpart := int64(beUint(d[:3], 3)) - 0x800000
	nb := (decimals + 1) / 2
	var frac int64
	if nb > 0 {
		frac = int64(beUint(d[3:3+nb], nb))
	}
	neg := intpart < 0
	if neg {
		if frac != 0 {
			intpart++
			frac = int64(1)<<(8*uint(nb)) - frac
		}
		intpart = -intpart
	}
	hour := (intpart >> 12) & 0x3ff
	minute := (intpart >> 6) & 0x3f
	sec := intpart & 0x3f
	sign := ""
	if neg {
		sign = "-"
	}
	s := fmt.Sprintf("%s%02d:%02d:%02d", sign, hour, minute, sec)
	if decimals > 0 {
		if decimals%2 == 1 {
			frac /= 10 // see fracMicros
		}
		s += fmt.Sprintf(".%0*d", decimals, frac)
	}
	return "'" + s + "'"
}

// --- pre-5.6.4 temporal formats ---
//
// MySQL 5.5 and earlier (and 5.6+ tables never rebuilt since) store
// DATETIME, TIME and TIMESTAMP without fractional seconds, and a pre-5.0
// table may still hold the 4-byte DATE that NEWDATE replaced. Each is a
// plain integer: little-endian in MyISAM; big-endian in InnoDB, with the
// usual DATA_INT sign-bit flip for every one except TIMESTAMP, which
// Field_timestamp flags UNSIGNED ("for 4.0 MYD and 4.0 InnoDB
// compatibility"). The callers undo the engine's byte order and hand these
// the integer itself; see decodeField and decodeMyISAMField.

// decodeOldDatetime renders an old DATETIME, stored as the decimal
// number YYYYMMDDhhmmss (Field_datetime::store's TIME_to_ulonglong_datetime).
func decodeOldDatetime(v int64) string {
	if v < 0 {
		v = -v // never written by the server - keep the digits visible rather than fail
	}
	date, tod := v/1000000, v%1000000
	return fmt.Sprintf("'%04d-%02d-%02d %02d:%02d:%02d'",
		date/10000, date/100%100, date%100, tod/10000, tod/100%100, tod%100)
}

// decodeOldTime renders an old TIME, stored as the signed decimal number
// ±hhmmss (Field_time::store's TIME_to_ulonglong_time, negated for a
// negative value); hours run up to 838.
func decodeOldTime(v int64) string {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("'%s%02d:%02d:%02d'", sign, v/10000, v/100%100, v%100)
}

// decodeOldDate renders the pre-5.0 4-byte DATE, stored as the decimal
// number YYYYMMDD.
func decodeOldDate(v int64) string {
	if v < 0 {
		v = -v
	}
	return fmt.Sprintf("'%04d-%02d-%02d'", v/10000, v/100%100, v%100)
}

// decodeOldTimestamp renders an old TIMESTAMP: whole seconds since the
// Unix epoch (UTC), 0 meaning '0000-00-00 00:00:00' - the same meaning
// TIMESTAMP2 kept, so this just reuses its decoder with no fraction.
func decodeOldTimestamp(sec uint32) string {
	return decodeTimestamp2([]byte{byte(sec >> 24), byte(sec >> 16), byte(sec >> 8), byte(sec)}, 0)
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

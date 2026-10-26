package main

import (
	"bytes"
	"testing"
)

// TestMyISAMBitLayout checks buildMyISAMLayout and myisamBit.value against
// a real MySQL 5.7.44/8.0.40 MyISAM table (bt_dyn's first 6 columns): a
// BIT(n)'s n/8 whole bytes are a field of their own (none at all for
// BIT(1)-BIT(7)), and its n%8 high bits sit in the null bitmap right
// after the column's own null bit - giving the same 3 null bytes and
// field list the table's .MYI recorded.
func TestMyISAMBitLayout(t *testing.T) {
	cols := []ddColumnJSON{
		{Name: "n1", Type: ddLong, IsNullable: true},
		{Name: "b5", Type: ddBit, CharLength: 5, IsNullable: true},
		{Name: "n2", Type: ddLong},
		{Name: "b10", Type: ddBit, CharLength: 10},
		{Name: "b1", Type: ddBit, CharLength: 1, IsNullable: true},
		{Name: "b64", Type: ddBit, CharLength: 64, IsNullable: true},
	}
	tbl := &Table{RowFormat: rowFormatDynamic}
	for _, rc := range cols {
		c, err := buildColumn(rc)
		if err != nil {
			t.Fatal(err)
		}
		tbl.Columns = append(tbl.Columns, c)
	}
	lay, err := buildMyISAMLayout(tbl)
	if err != nil {
		t.Fatal(err)
	}
	// null bits: n1=0, b5=1 (+5 high bits 2-6), b10 high bits 7-8, b1=9 (+1 bit 10), b64=11
	if lay.NullBytes != 2 {
		t.Errorf("NullBytes = %d, want 2", lay.NullBytes)
	}
	var lens []uint32
	for _, f := range lay.Fields {
		lens = append(lens, f.Length)
	}
	if want := []uint32{2, 4, 4, 1, 8}; !equalU32(lens, want) {
		t.Errorf("field lengths = %v, want %v (null bitmap, n1, n2, b10's byte, b64)", lens, want)
	}
	// b5 = 0b10110 in bits 2-6; b10 = 0b11_00000011: low byte in its field, high bits 7-8.
	rec := &myisamRecord{Buf: make([]byte, lay.RecLength)}
	rec.Buf[0] = 0b10110<<2 | 1<<7 // b5's value, b10's high bit 0
	rec.Buf[1] = 1                 // b10's high bit 1
	rec.Buf[lay.Fields[3].Offset] = 0b00000011
	if got := lay.Bits[tbl.Columns[1]].value(rec, lay); !bytes.Equal(got, []byte{0b10110}) {
		t.Errorf("b5 = %08b, want 00010110", got)
	}
	if got := lay.Bits[tbl.Columns[3]].value(rec, lay); !bytes.Equal(got, []byte{0b11, 0b00000011}) {
		t.Errorf("b10 = %08b, want [00000011 00000011]", got)
	}
}

func equalU32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

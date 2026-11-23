package wire

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/meta"
)

// bcol is a column definition for the binary encoder tests.
func bcol(ty uint8, flags uint16, length uint32, decimals uint8) meta.ColumnDef {
	cs := uint16(meta.CharsetBinary)
	switch ty {
	case meta.TypeVarString, meta.TypeString, meta.TypeBlob, meta.TypeJSON, meta.TypeEnum, meta.TypeSet:
		cs = meta.CollationDefault
	}
	return meta.ColumnDef{Name: "c", Type: ty, Flags: flags, Length: length, Decimals: decimals, Charset: cs}
}

// Each type is encoded as its declared type requires, and go-mysql's independent
// decoder reads back the value the text protocol shows.
func TestBinaryRowTypes(t *testing.T) {
	cases := []struct {
		def  meta.ColumnDef
		val  any
		raw  []byte // the value's bytes after the bitmap
		want string // go-mysql's decoded value, as text
	}{
		{bcol(meta.TypeTiny, 0, 4, 0), int64(-5), []byte{0xfb}, "-5"},
		{bcol(meta.TypeTiny, meta.FlagUnsigned, 3, 0), int64(250), []byte{250}, "250"},
		{bcol(meta.TypeShort, 0, 6, 0), int64(-2), []byte{0xfe, 0xff}, "-2"},
		{bcol(meta.TypeYear, meta.FlagUnsigned|meta.FlagZerofill, 4, 0), int64(2024), []byte{0xe8, 0x07}, "2024"},
		{bcol(meta.TypeInt24, 0, 9, 0), int64(70000), []byte{0x70, 0x11, 0x01, 0x00}, "70000"},
		{bcol(meta.TypeLong, 0, 11, 0), int64(-1), []byte{0xff, 0xff, 0xff, 0xff}, "-1"},
		{bcol(meta.TypeLongLong, 0, 20, 0), int64(1 << 40), []byte{0, 0, 0, 0, 0, 1, 0, 0}, "1099511627776"},
		{bcol(meta.TypeLongLong, meta.FlagUnsigned, 20, 0), "18446744073709551615", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, "18446744073709551615"},
		{bcol(meta.TypeFloat, 0, 12, 31), float64(1.5), []byte{0, 0, 0xc0, 0x3f}, "1.5"},
		{bcol(meta.TypeDouble, 0, 22, 31), float64(0.1), []byte{0x9a, 0x99, 0x99, 0x99, 0x99, 0x99, 0xb9, 0x3f}, "0.1"},
		{bcol(meta.TypeNewDecimal, 0, 12, 2), float64(1.5), []byte{4, '1', '.', '5', '0'}, "1.50"},
		{bcol(meta.TypeVarString, 0, 80, 0), "città", append([]byte{6}, "città"...), "città"},
		{bcol(meta.TypeBlob, meta.FlagBinary|meta.FlagBlob, 65535, 0), []byte{0, 0xff, 0x80}, []byte{3, 0, 0xff, 0x80}, "\x00\xff\x80"},
		{bcol(meta.TypeDate, 0, 10, 0), "2024-01-02", []byte{4, 0xe8, 0x07, 1, 2}, "2024-01-02"},
		{bcol(meta.TypeDate, 0, 10, 0), "0000-00-00", []byte{0}, "0000-00-00"},
		{bcol(meta.TypeDatetime, 0, 19, 0), "2024-01-02 10:20:30", []byte{7, 0xe8, 0x07, 1, 2, 10, 20, 30}, "2024-01-02 10:20:30"},
		{bcol(meta.TypeDatetime, 0, 19, 0), "2024-01-02 00:00:00", []byte{4, 0xe8, 0x07, 1, 2}, "2024-01-02 00:00:00"},
		{bcol(meta.TypeDatetime, 0, 26, 6), "2024-01-02 10:20:30.000500", []byte{11, 0xe8, 0x07, 1, 2, 10, 20, 30, 0xf4, 0x01, 0, 0}, "2024-01-02 10:20:30.000500"},
		{bcol(meta.TypeTimestamp, 0, 19, 0), time.Date(2024, 1, 2, 10, 20, 30, 0, time.UTC), []byte{7, 0xe8, 0x07, 1, 2, 10, 20, 30}, "2024-01-02 10:20:30"},
		{bcol(meta.TypeTime, 0, 10, 0), "-26:00:01", []byte{8, 1, 1, 0, 0, 0, 2, 0, 1}, "-26:00:01"},
		{bcol(meta.TypeTime, 0, 10, 0), "00:00:00", []byte{0}, "00:00:00"},
		{bcol(meta.TypeBit, meta.FlagUnsigned, 10, 0), int64(5), []byte{2, 0, 5}, "\x00\x05"},
	}
	for _, c := range cases {
		row, err := binaryRow([]meta.ColumnDef{c.def}, []any{c.val})
		if err != nil {
			t.Errorf("type %#x %v: %v", c.def.Type, c.val, err)
			continue
		}
		want := append([]byte{0x00, 0x00}, c.raw...)
		if !bytes.Equal(row, want) {
			t.Errorf("type %#x %v: bytes % x, want % x", c.def.Type, c.val, row, want)
			continue
		}
		got, err := mysql.RowData(row).ParseBinary(fields([]meta.ColumnDef{c.def}), nil)
		if err != nil {
			t.Errorf("type %#x %v: go-mysql cannot decode: %v", c.def.Type, c.val, err)
			continue
		}
		if s := decoded(got[0]); s != c.want {
			t.Errorf("type %#x %v: go-mysql decodes %q, want %q", c.def.Type, c.val, s, c.want)
		}
	}
}

func decoded(v mysql.FieldValue) string {
	switch x := v.Value().(type) {
	case []byte:
		return string(x)
	case nil:
		return "NULL"
	default:
		return fmt.Sprint(x)
	}
}

// M4 criterion 4: a NULL bitmap over 64+ columns with mixed NULLs (offset 2).
func TestBinaryRowNullBitmap(t *testing.T) {
	const n = 70
	defs := make([]meta.ColumnDef, n)
	vals := make([]any, n)
	for i := range defs {
		defs[i] = bcol(meta.TypeLong, 0, 11, 0)
		defs[i].Name = fmt.Sprintf("c%d", i)
		if i%3 == 0 || i == 61 || i == 62 || i == 69 {
			continue // NULL
		}
		vals[i] = int64(i)
	}
	row, err := binaryRow(defs, vals)
	if err != nil {
		t.Fatal(err)
	}
	bitmapLen := (n + 7 + 2) / 8
	if bitmapLen != 9 || row[0] != 0x00 {
		t.Fatalf("header %#x, bitmap length %d", row[0], bitmapLen)
	}
	for i := 0; i < n; i++ {
		pos := i + 2
		set := row[1+pos/8]&(1<<(pos%8)) != 0
		if set != (vals[i] == nil) {
			t.Errorf("column %d: NULL bit %v", i, set)
		}
	}
	got, err := mysql.RowData(row).ParseBinary(fields(defs), nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range got {
		want := "NULL"
		if vals[i] != nil {
			want = fmt.Sprint(vals[i])
		}
		if s := decoded(v); s != want {
			t.Errorf("column %d: %q, want %q", i, s, want)
		}
	}
	// The last bitmap byte holds bit positions 64-71, columns 62-69: NULLs 62, 63, 66, 69.
	if row[bitmapLen] != 0b10010011 {
		t.Errorf("last bitmap byte %08b", row[bitmapLen])
	}
}

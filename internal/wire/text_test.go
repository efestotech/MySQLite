package wire

import (
	"bytes"
	"testing"
	"time"

	"mysqlite/internal/meta"
)

func col(typ uint8, length uint32, decimals uint8, flags uint16, charset uint16) meta.ColumnDef {
	return meta.ColumnDef{Type: typ, Length: length, Decimals: decimals, Flags: flags, Charset: charset}
}

func TestTextValue(t *testing.T) {
	var (
		intCol      = col(meta.TypeLong, 11, 0, meta.FlagNum, meta.CharsetBinary)
		zerofill    = col(meta.TypeLong, 5, 0, meta.FlagNum|meta.FlagUnsigned|meta.FlagZerofill, meta.CharsetBinary)
		unsignedCol = col(meta.TypeLongLong, 20, 0, meta.FlagNum|meta.FlagUnsigned, meta.CharsetBinary)
		double      = col(meta.TypeDouble, 22, 31, meta.FlagNum, meta.CharsetBinary)
		doubleFixed = col(meta.TypeDouble, 8, 3, meta.FlagNum, meta.CharsetBinary)
		float       = col(meta.TypeFloat, 12, 31, meta.FlagNum, meta.CharsetBinary)
		decimal2    = col(meta.TypeNewDecimal, 12, 2, meta.FlagNum, meta.CharsetBinary)
		decimal0    = col(meta.TypeNewDecimal, 11, 0, meta.FlagNum, meta.CharsetBinary)
		date        = col(meta.TypeDate, 10, 0, meta.FlagBinary, meta.CharsetBinary)
		datetime    = col(meta.TypeDatetime, 19, 0, meta.FlagBinary, meta.CharsetBinary)
		datetime3   = col(meta.TypeDatetime, 23, 3, meta.FlagBinary, meta.CharsetBinary)
		timestamp6  = col(meta.TypeTimestamp, 26, 6, meta.FlagBinary|meta.FlagTimestamp, meta.CharsetBinary)
		timeCol     = col(meta.TypeTime, 10, 0, meta.FlagBinary, meta.CharsetBinary)
		time2       = col(meta.TypeTime, 13, 2, meta.FlagBinary, meta.CharsetBinary)
		year        = col(meta.TypeYear, 4, 0, meta.FlagUnsigned|meta.FlagZerofill|meta.FlagNum, meta.CharsetBinary)
		varchar     = col(meta.TypeVarString, 80, 0, 0, meta.CollationDefault)
		char        = col(meta.TypeString, 40, 0, 0, meta.CollationDefault)
		binary4     = col(meta.TypeString, 4, 0, meta.FlagBinary, meta.CharsetBinary)
		blob        = col(meta.TypeBlob, 65535, 0, meta.FlagBlob|meta.FlagBinary, meta.CharsetBinary)
		bit10       = col(meta.TypeBit, 10, 0, meta.FlagUnsigned, meta.CharsetBinary)
		nullCol     = col(meta.TypeNull, 0, 0, meta.FlagBinary, meta.CharsetBinary)
		degraded    = meta.ColumnDef{Type: meta.TypeVarString, Length: 262140, Charset: meta.CollationDefault, Degraded: true}
	)
	ts := time.Date(2024, 1, 2, 3, 4, 5, 123456789, time.UTC)
	half := time.Date(2024, 1, 2, 3, 4, 5, 500000000, time.UTC)
	// Runtime float64 addition: a constant expression would fold to exactly 0.3.
	tenth, fifth := 0.1, 0.2
	sum := tenth + fifth

	cases := []struct {
		name string
		def  meta.ColumnDef
		in   any
		want string
	}{
		{"int", intCol, int64(42), "42"},
		{"negative int", intCol, int64(-7), "-7"},
		{"integral real in int column", intCol, float64(3), "3"},
		{"zerofill", zerofill, int64(42), "00042"},
		{"unsigned max int64", unsignedCol, int64(9223372036854775807), "9223372036854775807"},
		{"double", double, 0.1, "0.1"},
		{"double sum", double, sum, "0.30000000000000004"},
		{"double integral", double, float64(100), "100"},
		{"double from integer", double, int64(3), "3"},
		{"double large", double, 1e20, "1e20"},
		{"double just below exponent form", double, 123456789012345.0, "123456789012345"},
		{"double exponent form", double, 1e15, "1e15"},
		{"double small", double, -1.5e-7, "-1.5e-7"},
		{"double fixed scale", doubleFixed, 1.5, "1.500"},
		{"float", float, 0.1, "0.1"},
		{"decimal", decimal2, 1.5, "1.50"},
		{"decimal from integer", decimal2, int64(3), "3.00"},
		{"decimal negative", decimal2, -0.5, "-0.50"},
		{"decimal scale 0", decimal0, float64(3), "3"},
		{"date from time", date, ts, "2024-01-02"},
		{"date string", date, "2024-01-02", "2024-01-02"},
		{"zero date", date, "0000-00-00", "0000-00-00"},
		{"datetime from time", datetime, ts, "2024-01-02 03:04:05"},
		{"datetime string", datetime, "2024-01-02 03:04:05", "2024-01-02 03:04:05"},
		{"zero datetime", datetime, "0000-00-00 00:00:00", "0000-00-00 00:00:00"},
		{"datetime fsp 3 from time", datetime3, ts, "2024-01-02 03:04:05.123"},
		{"datetime fsp 3 from string", datetime3, "2024-01-02 03:04:05.1", "2024-01-02 03:04:05.100"},
		{"datetime fsp 3 from long string", datetime3, "2024-01-02 03:04:05.123456", "2024-01-02 03:04:05.123"},
		{"datetime fsp 3 without fraction", datetime3, "2024-01-02 03:04:05", "2024-01-02 03:04:05.000"},
		{"timestamp fsp 6", timestamp6, half, "2024-01-02 03:04:05.500000"},
		{"time", timeCol, "12:34:56", "12:34:56"},
		{"time negative hours", timeCol, "-838:59:59", "-838:59:59"},
		{"time fsp 2", time2, "01:02:03.5", "01:02:03.50"},
		{"year", year, int64(2024), "2024"},
		{"year zero", year, int64(0), "0000"},
		{"varchar", varchar, "città", "città"},
		{"varchar from integer", varchar, int64(5), "5"},
		{"varchar from bytes", varchar, []byte("raw"), "raw"},
		{"char strips trailing spaces", char, "ab  ", "ab"},
		{"binary pads with zero bytes", binary4, []byte("ab"), "ab\x00\x00"},
		{"blob", blob, []byte{0, 1, 0xff}, "\x00\x01\xff"},
		{"blob from text", blob, "x", "x"},
		{"bit from integer", bit10, int64(5), "\x00\x05"},
		{"bit from bytes", bit10, []byte{1, 2}, "\x01\x02"},
		{"degraded time", degraded, ts, "2024-01-02 03:04:05.123456789"},
		{"degraded real", degraded, 1.5, "1.5"},
	}
	for _, tc := range cases {
		got, null, err := textValue(tc.def, tc.in)
		if err != nil || null {
			t.Errorf("%s: unexpected null=%v err=%v", tc.name, null, err)
			continue
		}
		if !bytes.Equal(got, []byte(tc.want)) {
			t.Errorf("%s: want %q, got %q", tc.name, tc.want, got)
		}
	}

	for _, def := range []meta.ColumnDef{intCol, double, decimal2, date, varchar, blob, nullCol} {
		if _, null, err := textValue(def, nil); !null || err != nil {
			t.Errorf("type %d: NULL must be NULL, got null=%v err=%v", def.Type, null, err)
		}
	}
	if _, null, err := textValue(nullCol, int64(1)); !null || err != nil {
		t.Errorf("a NULL-typed column is always NULL, got null=%v err=%v", null, err)
	}
}

// A stored value that MySQL could not hold in the declared type is an error, never a
// different value: MySQL would have coerced or rejected it at insert time.
func TestTextValueRejectsValuesOutsideTheDeclaredType(t *testing.T) {
	intCol := col(meta.TypeLong, 11, 0, meta.FlagNum, meta.CharsetBinary)
	unsignedCol := col(meta.TypeLong, 10, 0, meta.FlagNum|meta.FlagUnsigned, meta.CharsetBinary)
	decimal2 := col(meta.TypeNewDecimal, 12, 2, meta.FlagNum, meta.CharsetBinary)
	date := col(meta.TypeDate, 10, 0, meta.FlagBinary, meta.CharsetBinary)
	datetime := col(meta.TypeDatetime, 19, 0, meta.FlagBinary, meta.CharsetBinary)
	timeCol := col(meta.TypeTime, 10, 0, meta.FlagBinary, meta.CharsetBinary)
	year := col(meta.TypeYear, 4, 0, meta.FlagUnsigned|meta.FlagZerofill|meta.FlagNum, meta.CharsetBinary)

	cases := []struct {
		name string
		def  meta.ColumnDef
		in   any
	}{
		{"fraction in int", intCol, 1.5},
		{"text in int", intCol, "abc"},
		{"negative unsigned", unsignedCol, int64(-1)},
		{"text in decimal", decimal2, "abc"},
		{"integer in date", date, int64(20240102)},
		{"bad month", date, "2024-13-01"},
		{"garbage date", date, "garbage"},
		{"bad hour", datetime, "2024-01-02 24:00:00"},
		{"iso separator", datetime, "2024-01-02T03:04:05Z"},
		{"bad time", timeCol, "12:60:00"},
		{"year out of range", year, int64(1800)},
	}
	for _, tc := range cases {
		if got, _, err := textValue(tc.def, tc.in); err == nil {
			t.Errorf("%s: want an error, got %q", tc.name, got)
		}
	}
}

func TestTextRow(t *testing.T) {
	defs := []meta.ColumnDef{
		col(meta.TypeLong, 11, 0, meta.FlagNum, meta.CharsetBinary),
		col(meta.TypeVarString, 80, 0, 0, meta.CollationDefault),
		col(meta.TypeVarString, 80, 0, 0, meta.CollationDefault),
	}
	row, err := textRow(defs, []any{int64(1), nil, ""})
	if err != nil {
		t.Fatal(err)
	}
	// lenenc "1", NULL marker 0xFB, empty string as a zero length.
	want := []byte{1, '1', 0xfb, 0}
	if !bytes.Equal(row, want) {
		t.Fatalf("want % x, got % x", want, row)
	}
}

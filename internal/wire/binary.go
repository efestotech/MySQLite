package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// Binary protocol row encoding (docs/03-protocol.md section 5): header 0x00, a NULL
// bitmap with an offset of 2 bits, then each non-NULL value in the encoding of its
// declared column type. The value is first formatted by textValue, so both protocols
// send the same value and the binary encoding follows the declared type, never the Go
// type of the scanned value.

// binaryRow encodes one row of a binary result set.
func binaryRow(defs []meta.ColumnDef, vals []any) ([]byte, error) {
	bitmap := make([]byte, (len(defs)+7+2)/8)
	var values []byte
	for i, d := range defs {
		s, null, err := textValue(d, vals[i])
		if errors.Is(err, errIntegerOverflow) {
			return nil, errs.BigintOutOfRange(d.Flags&meta.FlagUnsigned != 0, d.Name)
		}
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", d.Name, err)
		}
		if null {
			pos := i + 2
			bitmap[pos/8] |= 1 << (pos % 8)
			continue
		}
		if values, err = appendBinary(values, d, s); err != nil {
			return nil, fmt.Errorf("column %q: %w", d.Name, err)
		}
	}
	row := make([]byte, 0, 1+len(bitmap)+len(values))
	row = append(row, 0x00)
	row = append(row, bitmap...)
	return append(row, values...), nil
}

// appendBinary appends the binary encoding of the text value s of column d.
func appendBinary(dst []byte, d meta.ColumnDef, s []byte) ([]byte, error) {
	unsigned := d.Flags&meta.FlagUnsigned != 0
	switch d.Type {
	case meta.TypeTiny:
		n, err := parseInteger(s, unsigned, 8)
		return append(dst, byte(n)), err
	case meta.TypeShort, meta.TypeYear:
		n, err := parseInteger(s, unsigned || d.Type == meta.TypeYear, 16)
		return binary.LittleEndian.AppendUint16(dst, uint16(n)), err
	case meta.TypeInt24, meta.TypeLong:
		n, err := parseInteger(s, unsigned, 32)
		return binary.LittleEndian.AppendUint32(dst, uint32(n)), err
	case meta.TypeLongLong:
		n, err := parseInteger(s, unsigned, 64)
		return binary.LittleEndian.AppendUint64(dst, n), err
	case meta.TypeFloat:
		f, err := strconv.ParseFloat(string(s), 32)
		return binary.LittleEndian.AppendUint32(dst, math.Float32bits(float32(f))), err
	case meta.TypeDouble:
		f, err := strconv.ParseFloat(string(s), 64)
		return binary.LittleEndian.AppendUint64(dst, math.Float64bits(f)), err
	case meta.TypeDate, meta.TypeDatetime, meta.TypeTimestamp:
		return appendBinaryDatetime(dst, string(s))
	case meta.TypeTime:
		return appendBinaryTime(dst, string(s))
	}
	// DECIMAL, strings, blobs, BIT, JSON, ENUM, SET: a length-encoded string.
	return append(dst, mysql.PutLengthEncodedString(s)...), nil
}

// parseInteger reads the decimal text of an integer column as the bit pattern the
// binary protocol sends: two's complement for signed columns.
func parseInteger(s []byte, unsigned bool, bits int) (uint64, error) {
	if unsigned {
		return strconv.ParseUint(string(s), 10, bits)
	}
	n, err := strconv.ParseInt(string(s), 10, bits)
	return uint64(n), err
}

// appendBinaryDatetime encodes 'YYYY-MM-DD[ hh:mm:ss[.ffffff]]' in MySQL's shortest
// form: length 0 for the zero date, 4 without a time, 7 without microseconds, else 11.
func appendBinaryDatetime(dst []byte, s string) ([]byte, error) {
	if len(s) < 10 {
		return nil, fmt.Errorf("%q is not a date", s)
	}
	year, month, day := atoi(s[0:4]), atoi(s[5:7]), atoi(s[8:10])
	var hour, minute, second, micro int
	if len(s) >= 19 {
		hour, minute, second = atoi(s[11:13]), atoi(s[14:16]), atoi(s[17:19])
		micro = microseconds(s[19:])
	}
	size := byte(11)
	switch {
	case year == 0 && month == 0 && day == 0 && hour == 0 && minute == 0 && second == 0 && micro == 0:
		return append(dst, 0), nil
	case hour == 0 && minute == 0 && second == 0 && micro == 0:
		size = 4
	case micro == 0:
		size = 7
	}
	dst = append(dst, size)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(year))
	dst = append(dst, byte(month), byte(day))
	if size == 4 {
		return dst, nil
	}
	dst = append(dst, byte(hour), byte(minute), byte(second))
	if micro == 0 {
		return dst, nil
	}
	return binary.LittleEndian.AppendUint32(dst, uint32(micro)), nil
}

// appendBinaryTime encodes '[-]hhh:mm:ss[.ffffff]': length 0 for 00:00:00, 8 without
// microseconds, else 12; hours above 23 become days.
func appendBinaryTime(dst []byte, s string) ([]byte, error) {
	negative := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	parts := strings.Split(whole, ":")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%q is not a time", s)
	}
	hours, minute, second := atoi(parts[0]), atoi(parts[1]), atoi(parts[2])
	micro := 0
	if frac != "" {
		micro = microseconds("." + frac)
	}
	if hours == 0 && minute == 0 && second == 0 && micro == 0 {
		return append(dst, 0), nil
	}
	size := byte(8)
	if micro != 0 {
		size = 12
	}
	sign := byte(0)
	if negative {
		sign = 1
	}
	dst = append(dst, size, sign)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(hours/24))
	dst = append(dst, byte(hours%24), byte(minute), byte(second))
	if micro == 0 {
		return dst, nil
	}
	return binary.LittleEndian.AppendUint32(dst, uint32(micro)), nil
}

// microseconds reads ".ffffff" (up to six digits, right-padded) as microseconds; 0 for
// "".
func microseconds(frac string) int {
	digits := strings.TrimPrefix(frac, ".")
	if digits == "" {
		return 0
	}
	if len(digits) > 6 {
		digits = digits[:6]
	}
	return atoi(digits + strings.Repeat("0", 6-len(digits)))
}

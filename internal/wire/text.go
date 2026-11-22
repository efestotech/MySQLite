package wire

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// Text protocol row encoding (docs/03-protocol.md section 5): every value is a string
// formatted from the column definition, never from the Go type alone, so the declared
// type and the encoding always agree.
//
// A stored value the declared type cannot hold (1.5 in an INT column, a malformed date)
// is an error, not a reformatted value: MySQL would have coerced or rejected it on
// insert, and showing anything else would be a silent difference. Insert-time coercion
// arrives with M3. Formats are oracle pending (ADR 0002 addendum).

// textRow encodes one row: a lenenc string per column, 0xFB for NULL.
func textRow(defs []meta.ColumnDef, vals []any) ([]byte, error) {
	var row []byte
	for i, d := range defs {
		b, null, err := textValue(d, vals[i])
		if errors.Is(err, errIntegerOverflow) {
			return nil, errs.BigintOutOfRange(d.Flags&meta.FlagUnsigned != 0, d.Name)
		}
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", d.Name, err)
		}
		if null {
			row = append(row, 0xfb)
			continue
		}
		row = append(row, mysql.PutLengthEncodedString(b)...)
	}
	return row, nil
}

// textValue formats one scanned driver value (nil, int64, float64, string, []byte or
// time.Time) for column d.
func textValue(d meta.ColumnDef, v any) ([]byte, bool, error) {
	if v == nil || d.Type == meta.TypeNull {
		return nil, true, nil
	}
	if d.Degraded {
		return []byte(anyText(v)), false, nil
	}
	var s string
	var err error
	switch d.Type {
	case meta.TypeTiny, meta.TypeShort, meta.TypeInt24, meta.TypeLong, meta.TypeLongLong:
		s, err = integerText(d, v)
	case meta.TypeYear:
		s, err = yearText(v)
	case meta.TypeFloat, meta.TypeDouble:
		s, err = floatText(d, v)
	case meta.TypeNewDecimal, meta.TypeDecimal:
		s, err = decimalText(d, v)
	case meta.TypeDate, meta.TypeDatetime, meta.TypeTimestamp, meta.TypeTime:
		s, err = temporalText(d, v)
	case meta.TypeBit:
		b, err := bitBytes(d, v)
		return b, false, err
	case meta.TypeString:
		b := bytesOf(v)
		if d.Charset == meta.CharsetBinary {
			// BINARY(n) is right-padded with 0x00 to its length.
			for uint32(len(b)) < d.Length {
				b = append(b, 0)
			}
			return b, false, nil
		}
		// CHAR drops trailing spaces on retrieval (PAD_CHAR_TO_FULL_LENGTH is off).
		return []byte(strings.TrimRight(string(b), " ")), false, nil
	default:
		// VARCHAR, VARBINARY, TEXT, BLOB, JSON, ENUM, SET: the stored bytes.
		return bytesOf(v), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(s), false, nil
}

func bytesOf(v any) []byte {
	switch x := v.(type) {
	case []byte:
		return append([]byte(nil), x...)
	case string:
		return []byte(x)
	}
	return []byte(anyText(v))
}

// anyText is the plain text of a value, for degraded columns and numbers stored in
// string columns.
func anyText(v any) string {
	switch x := v.(type) {
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return shortestFloat(x, 64)
	case string:
		return x
	case []byte:
		return string(x)
	case time.Time:
		return x.Format("2006-01-02 15:04:05.999999999")
	}
	return fmt.Sprint(v)
}

// errIntegerOverflow is an integer column that received a REAL holding a whole number
// outside the 64-bit range: SQLite's integer arithmetic overflowed into floating point,
// where MySQL raises 1690.
var errIntegerOverflow = errors.New("integer arithmetic overflow")

func integerText(d meta.ColumnDef, v any) (string, error) {
	var n int64
	switch x := v.(type) {
	case int64:
		n = x
	case float64:
		// A REAL that holds an integer is the same number.
		if x == math.Trunc(x) && (x < math.MinInt64 || x >= math.MaxInt64) {
			return "", errIntegerOverflow
		}
		if x != math.Trunc(x) {
			return "", fmt.Errorf("%v does not fit an integer column", x)
		}
		n = int64(x)
	case string:
		// An unsigned value above the int64 range is bound as its text (vars.Arg).
		if u, err := strconv.ParseUint(x, 10, 64); err == nil && d.Flags&meta.FlagUnsigned != 0 {
			return strconv.FormatUint(u, 10), nil
		}
		return "", fmt.Errorf("string value %q in an integer column", x)
	default:
		return "", fmt.Errorf("%T value %q in an integer column", v, anyText(v))
	}
	if d.Flags&meta.FlagUnsigned != 0 && n < 0 {
		return "", fmt.Errorf("%d in an UNSIGNED column", n)
	}
	s := strconv.FormatInt(n, 10)
	if d.Flags&meta.FlagZerofill != 0 && uint32(len(s)) < d.Length {
		s = strings.Repeat("0", int(d.Length)-len(s)) + s
	}
	return s, nil
}

func yearText(v any) (string, error) {
	n, ok := v.(int64)
	if !ok || (n != 0 && (n < 1901 || n > 2155)) {
		return "", fmt.Errorf("%q is not a YEAR", anyText(v))
	}
	return fmt.Sprintf("%04d", n), nil
}

func floatText(d meta.ColumnDef, v any) (string, error) {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case int64:
		f = float64(x)
	default:
		return "", fmt.Errorf("%T value %q in a floating-point column", v, anyText(v))
	}
	if d.Decimals < 31 {
		// FLOAT(M,D) / DOUBLE(M,D): a fixed number of decimals.
		return strconv.FormatFloat(f, 'f', int(d.Decimals), 64), nil
	}
	if d.Type == meta.TypeFloat {
		return shortestFloat(f, 32), nil
	}
	return shortestFloat(f, 64), nil
}

// shortestFloat prints the shortest representation that reads back as the same value,
// in plain notation for decimal exponents -6..14 and as "1.5e-7" / "1e20" otherwise.
func shortestFloat(f float64, bits int) string {
	if bits == 32 {
		f = float64(float32(f))
	}
	sci := strconv.FormatFloat(f, 'e', -1, bits)
	mant, expText, _ := strings.Cut(sci, "e")
	exp, _ := strconv.Atoi(expText)
	if exp >= -6 && exp < 15 {
		return strconv.FormatFloat(f, 'f', -1, bits)
	}
	return mant + "e" + strconv.Itoa(exp)
}

// decimalText prints the declared scale. DECIMAL is stored as REAL (docs/05-dialect.md
// section 9, option a), so rounding follows the binary value.
func decimalText(d meta.ColumnDef, v any) (string, error) {
	var s string
	switch x := v.(type) {
	case int64:
		s = strconv.FormatInt(x, 10)
		if d.Decimals > 0 {
			s += "." + strings.Repeat("0", int(d.Decimals))
		}
	case float64:
		s = strconv.FormatFloat(x, 'f', int(d.Decimals), 64)
		if strings.Trim(s, "-0.") == "" {
			s = strings.TrimPrefix(s, "-") // no negative zero
		}
	default:
		return "", fmt.Errorf("%T value %q in a DECIMAL column", v, anyText(v))
	}
	return s, nil
}

var (
	reDate     = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
	reDatetime = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2}) (\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?$`)
	reTime     = regexp.MustCompile(`^-?(\d{2,3}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?$`)
)

// temporalText formats DATE, DATETIME, TIMESTAMP and TIME. The driver returns
// time.Time only for columns declared exactly DATE, DATETIME or TIMESTAMP (trap T1);
// DATETIME(3), zero dates and malformed values arrive as strings, which are validated.
func temporalText(d meta.ColumnDef, v any) (string, error) {
	fsp := int(d.Decimals)
	if t, ok := v.(time.Time); ok {
		switch d.Type {
		case meta.TypeDate:
			return t.Format("2006-01-02"), nil
		case meta.TypeTime:
			return t.Format("15:04:05") + fraction(fmt.Sprintf("%09d", t.Nanosecond()), fsp), nil
		}
		return t.Format("2006-01-02 15:04:05") + fraction(fmt.Sprintf("%09d", t.Nanosecond()), fsp), nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%T value %q in a temporal column", v, anyText(v))
	}
	switch d.Type {
	case meta.TypeDate:
		if m := reDate.FindStringSubmatch(s); m != nil && validDate(m[1], m[2], m[3]) {
			return s, nil
		}
	case meta.TypeTime:
		if m := reTime.FindStringSubmatch(s); m != nil && atoi(m[1]) <= 838 && atoi(m[2]) <= 59 && atoi(m[3]) <= 59 {
			whole, _, _ := strings.Cut(s, ".")
			return whole + fraction(m[4], fsp), nil
		}
	default:
		if m := reDatetime.FindStringSubmatch(s); m != nil && validDate(m[1], m[2], m[3]) &&
			atoi(m[4]) <= 23 && atoi(m[5]) <= 59 && atoi(m[6]) <= 59 {
			return s[:19] + fraction(m[7], fsp), nil
		}
	}
	return "", fmt.Errorf("%q is not a valid value for the column type", s)
}

// validDate accepts a calendar date or the all-zero date MySQL allows to be stored.
func validDate(y, m, d string) bool {
	if y == "0000" && m == "00" && d == "00" {
		return true
	}
	month, day := atoi(m), atoi(d)
	return month >= 1 && month <= 12 && day >= 1 && day <= 31
}

// fraction returns ".ddd" with exactly fsp digits from the given fraction digits,
// truncated or right-padded with zeros; "" when fsp is 0.
func fraction(digits string, fsp int) string {
	if fsp == 0 {
		return ""
	}
	if len(digits) > fsp {
		digits = digits[:fsp]
	}
	return "." + digits + strings.Repeat("0", fsp-len(digits))
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// bitBytes encodes BIT(M) as ceil(M/8) big-endian bytes, as MySQL sends it.
func bitBytes(d meta.ColumnDef, v any) ([]byte, error) {
	size := int(d.Length+7) / 8
	switch x := v.(type) {
	case int64:
		if x < 0 || (size < 8 && x >= 1<<(8*size)) {
			return nil, fmt.Errorf("%d does not fit BIT(%d)", x, d.Length)
		}
		b := make([]byte, size)
		for i := size - 1; i >= 0; i-- {
			b[i] = byte(x)
			x >>= 8
		}
		return b, nil
	case []byte:
		if len(x) > size {
			return nil, fmt.Errorf("%d bytes do not fit BIT(%d)", len(x), d.Length)
		}
		b := make([]byte, size-len(x), size)
		return append(b, x...), nil
	}
	return nil, fmt.Errorf("%T value %q in a BIT column", v, anyText(v))
}

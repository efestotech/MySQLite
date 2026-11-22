package meta

import (
	"fmt"
	"math"
	"math/big"
	"strings"
	"unicode/utf8"

	"mysqlite/internal/errs"
)

// Limits are the value bounds MySQL's strict mode enforces for a column type: integer
// ranges (1264) and string lengths (1406). SQLite enforces neither, so tables created
// by mysqlite carry them as CHECK constraints and INSERT checks its literal rows first,
// which also gives MySQL's row number.
type Limits struct {
	// Min and Max bound an integer type.
	Min, Max *big.Int
	// bigint is BIGINT: SQLite's integers already have its signed range, and its
	// unsigned values above 2^63-1 cannot be stored.
	bigint bool
	// NonNegative is an UNSIGNED DECIMAL, FLOAT or DOUBLE.
	NonNegative bool
	// Chars bounds a CHAR/VARCHAR length in characters; Bytes a BINARY, VARBINARY, TEXT
	// or BLOB length in bytes. 0 means no bound.
	Chars, Bytes int
}

var intRanges = map[uint8][2]int64{
	TypeTiny:  {-1 << 7, 1<<7 - 1},
	TypeShort: {-1 << 15, 1<<15 - 1},
	TypeInt24: {-1 << 23, 1<<23 - 1},
	TypeLong:  {-1 << 31, 1<<31 - 1},
}

// LimitsOf returns the limits of a MySQL column type as the dialect writes it.
func LimitsOf(mysqlType string) Limits {
	ti, ok := ParseType(mysqlType)
	if !ok {
		return Limits{}
	}
	unsigned := ti.Flags&FlagUnsigned != 0
	var l Limits
	switch ti.Type {
	case TypeTiny, TypeShort, TypeInt24, TypeLong:
		r := intRanges[ti.Type]
		l.Min, l.Max = big.NewInt(r[0]), big.NewInt(r[1])
		if unsigned {
			l.Min = big.NewInt(0)
			l.Max = big.NewInt(r[1]*2 + 1)
		}
	case TypeLongLong:
		l.bigint = true
		l.Min, l.Max = big.NewInt(math.MinInt64), big.NewInt(math.MaxInt64)
		if unsigned {
			l.Min, l.Max = big.NewInt(0), new(big.Int).SetUint64(math.MaxUint64)
		}
	case TypeNewDecimal, TypeFloat, TypeDouble:
		l.NonNegative = unsigned
	case TypeString, TypeVarString:
		if ti.Charset == CharsetBinary {
			l.Bytes = int(ti.Length)
		} else {
			l.Chars = int(ti.Length) / maxLen(ti.Charset)
		}
	case TypeBlob:
		upper := strings.ToUpper(mysqlType)
		switch {
		case strings.HasPrefix(upper, "TINY"):
			l.Bytes = 255
		case strings.HasPrefix(upper, "MEDIUM"):
			l.Bytes = 16777215
		case strings.HasPrefix(upper, "LONG"):
		default:
			l.Bytes = 65535
		}
	}
	return l
}

func maxLen(collation uint16) int {
	if collation == CollationUTF8MB3 {
		return 3
	}
	return 4
}

// CheckSQL returns the CHECK constraint that enforces l on column, as SQLite SQL, or
// "" when there is nothing to enforce. The constraint name tells errs which MySQL error
// a violation is.
func (l Limits) CheckSQL(column string) string {
	q := "`" + strings.ReplaceAll(column, "`", "``") + "`"
	name := func(prefix string) string {
		return "CONSTRAINT `" + strings.ReplaceAll(prefix+column, "`", "``") + "` CHECK "
	}
	switch {
	case l.bigint && l.Min.Sign() == 0:
		return name(errs.CheckRangePrefix) + fmt.Sprintf("(%s >= 0)", q)
	case l.bigint:
		return ""
	case l.Min != nil && l.Max != nil:
		return name(errs.CheckRangePrefix) + fmt.Sprintf("(%s BETWEEN %s AND %s)", q, l.Min, l.Max)
	case l.Min != nil || l.NonNegative:
		return name(errs.CheckRangePrefix) + fmt.Sprintf("(%s >= 0)", q)
	case l.Chars > 0:
		return name(errs.CheckLengthPrefix) + fmt.Sprintf("(length(%s) <= %d)", q, l.Chars)
	case l.Bytes > 0:
		return name(errs.CheckLengthPrefix) + fmt.Sprintf("(length(CAST(%s AS BLOB)) <= %d)", q, l.Bytes)
	}
	return ""
}

// CheckLiteral checks one value written into column at a 1-based row: the strict-mode
// range and length errors MySQL raises before storing anything. Conversions between
// types are M3; a literal of another kind is left to them.
func (l Limits) CheckLiteral(column string, v *Literal, row int) *errs.Error {
	if v == nil {
		return nil
	}
	switch v.Kind {
	case LiteralInt:
		n, ok := new(big.Int).SetString(v.Text, 10)
		if !ok {
			return nil
		}
		if l.Min != nil && n.Cmp(l.Min) < 0 || l.Max != nil && n.Cmp(l.Max) > 0 {
			return errs.OutOfRange(column, row)
		}
		if l.bigint && !n.IsInt64() {
			return errs.Unsupported("BIGINT UNSIGNED value above 9223372036854775807", "SQLite integers are signed 64-bit")
		}
		if l.NonNegative && n.Sign() < 0 {
			return errs.OutOfRange(column, row)
		}
	case LiteralDecimal, LiteralFloat:
		if l.NonNegative && strings.HasPrefix(v.Text, "-") {
			return errs.OutOfRange(column, row)
		}
	case LiteralString:
		if l.Chars > 0 && utf8.RuneCountInString(v.Text) > l.Chars ||
			l.Bytes > 0 && len(v.Text) > l.Bytes {
			return errs.DataTooLong(column, row)
		}
	}
	return nil
}

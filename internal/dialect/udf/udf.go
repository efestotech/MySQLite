// Package udf implements the SQL functions mysqlite registers on every SQLite
// connection, for the MySQL functions SQLite has no equivalent of
// (docs/05-dialect.md section 5). The function table (package functions) names them.
package udf

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"mysqlite/internal/collate"
	"mysqlite/internal/dialect/functions"
	"mysqlite/internal/engine"
	"mysqlite/internal/meta"
)

// Extensions are the collations and functions every engine registers.
func Extensions() engine.Extensions {
	return engine.Extensions{Collations: collate.Collations(), Functions: Functions()}
}

// Functions returns the scalar functions, all deterministic.
func Functions() []engine.ScalarFunction {
	return []engine.ScalarFunction{
		{Name: functions.UDFUpper, NumArgs: 1, Deterministic: true, Fn: caseMap(strings.ToUpper)},
		{Name: functions.UDFLower, NumArgs: 1, Deterministic: true, Fn: caseMap(strings.ToLower)},
		{Name: functions.UDFDecimalText, NumArgs: 2, Deterministic: true, Fn: decimalText},
		{Name: functions.UDFDoubleText, NumArgs: 2, Deterministic: true, Fn: doubleText},
		{Name: functions.UDFMod, NumArgs: 2, Deterministic: true, Fn: mod},
	}
}

// caseMap folds a character string by Unicode rules. A binary string is returned
// unchanged, as MySQL's UPPER and LOWER leave binary strings alone.
func caseMap(fold func(string) string) func(args []any) (any, error) {
	return func(args []any) (any, error) {
		switch x := args[0].(type) {
		case nil:
			return nil, nil
		case string:
			return fold(x), nil
		case []byte:
			return x, nil
		case int64:
			return strconv.FormatInt(x, 10), nil
		case float64:
			return meta.FormatDouble(x, 64), nil
		}
		return nil, fmt.Errorf("udf: unexpected %T", args[0])
	}
}

// decimalText(x, scale) is a DECIMAL value's MySQL string form: the declared scale.
func decimalText(args []any) (any, error) {
	if args[0] == nil {
		return nil, nil
	}
	scale, ok := args[1].(int64)
	if !ok {
		return nil, fmt.Errorf("udf: %s scale is %T", functions.UDFDecimalText, args[1])
	}
	if s, ok := meta.FormatDecimal(args[0], int(scale)); ok {
		return s, nil
	}
	return args[0], nil // a string stored in the column is already text
}

// doubleText(x, bits) is a DOUBLE (64) or FLOAT (32) value's MySQL string form.
func doubleText(args []any) (any, error) {
	bits, ok := args[1].(int64)
	if !ok {
		return nil, fmt.Errorf("udf: %s bits is %T", functions.UDFDoubleText, args[1])
	}
	switch x := args[0].(type) {
	case float64:
		return meta.FormatDouble(x, int(bits)), nil
	case int64:
		return meta.FormatDouble(float64(x), int(bits)), nil
	}
	return args[0], nil
}

// mod is MySQL's MOD and %: NULL for a NULL operand or a zero divisor, the sign of the
// dividend, exact for two integers. Strings convert to their leading number, as in MySQL.
func mod(args []any) (any, error) {
	a, b := number(args[0]), number(args[1])
	if a == nil || b == nil {
		return nil, nil
	}
	if x, ok := a.(int64); ok {
		if y, ok := b.(int64); ok {
			if y == 0 {
				return nil, nil
			}
			if y == -1 {
				return int64(0), nil // MinInt64 % -1 would overflow
			}
			return x % y, nil
		}
	}
	x, y := toFloat(a), toFloat(b)
	if y == 0 {
		return nil, nil
	}
	return math.Mod(x, y), nil
}

// number turns an argument into int64 or float64; nil for NULL or a non-number.
func number(v any) any {
	switch x := v.(type) {
	case int64, float64:
		return x
	case string:
		return parseNumber(x)
	case []byte:
		return parseNumber(string(x))
	}
	return nil
}

// parseNumber reads the leading number of s as MySQL does for arithmetic: "12abc" is
// 12, "abc" is 0.
func parseNumber(s string) any {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && strings.IndexByte("+-0123456789.eE", s[end]) >= 0 {
		end++
	}
	for ; end > 0; end-- {
		if n, err := strconv.ParseInt(s[:end], 10, 64); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(s[:end], 64); err == nil {
			return f
		}
	}
	return int64(0)
}

func toFloat(v any) float64 {
	if n, ok := v.(int64); ok {
		return float64(n)
	}
	return v.(float64)
}

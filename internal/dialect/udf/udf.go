// Package udf implements the SQL functions mysqlite registers on every SQLite
// connection, for the MySQL functions SQLite has no equivalent of
// (docs/05-dialect.md section 5). The function table (package functions) names them.
package udf

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"mysqlite/internal/collate"
	"mysqlite/internal/dialect/functions"
	"mysqlite/internal/engine"
	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// Extensions are the collations and functions every engine registers.
func Extensions() engine.Extensions {
	return engine.Extensions{Collations: collate.Collations(), Functions: Functions()}
}

// Functions returns the scalar functions.
func Functions() []engine.ScalarFunction {
	return []engine.ScalarFunction{
		{Name: functions.UDFUpper, NumArgs: 1, Deterministic: true, Fn: caseMap(strings.ToUpper)},
		{Name: functions.UDFLower, NumArgs: 1, Deterministic: true, Fn: caseMap(strings.ToLower)},
		{Name: functions.UDFDecimalText, NumArgs: 2, Deterministic: true, Fn: decimalText},
		{Name: functions.UDFDoubleText, NumArgs: 2, Deterministic: true, Fn: doubleText},
		{Name: functions.UDFMod, NumArgs: 3, Deterministic: true, Fn: mod},
		{Name: functions.UDFDivide, NumArgs: 3, Deterministic: true, Fn: divide},
		{Name: functions.UDFIntDiv, NumArgs: 3, Deterministic: true, Fn: intDiv},
		{Name: functions.UDFXor, NumArgs: 2, Deterministic: true, Fn: xor},
		{Name: functions.UDFLike, NumArgs: 4, Deterministic: true, Fn: like},
		{Name: functions.UDFCut, NumArgs: 2, Deterministic: true, Fn: cut},
		{Name: functions.UDFRaise, NumArgs: 1, Fn: raise},
		// Not deterministic: it writes to the sink on every call.
		{Name: functions.UDFLastInsertID, NumArgs: 2, Fn: lastInsertID},
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

// The division functions take a third argument, raise: 1 makes a zero divisor MySQL's
// error 1365 (an INSERT or UPDATE in strict mode with ERROR_FOR_DIVISION_BY_ZERO), 0 makes
// it NULL.

// operands converts the two operands of a division; done is set when the result is
// NULL or an error.
func operands(args []any) (a, b any, done bool, err error) {
	a, b = number(args[0]), number(args[1])
	if a == nil || b == nil {
		return nil, nil, true, nil
	}
	if toFloat(b) == 0 {
		if raise, _ := args[2].(int64); raise != 0 {
			return nil, nil, true, errs.FunctionError(errs.DivisionByZero())
		}
		return nil, nil, true, nil
	}
	return a, b, false, nil
}

// mod is MySQL's MOD and %: NULL for a NULL operand or a zero divisor, the sign of the
// dividend, exact for two integers. Strings convert to their leading number, as in MySQL.
func mod(args []any) (any, error) {
	a, b, done, err := operands(args)
	if done {
		return nil, err
	}
	if x, ok := a.(int64); ok {
		if y, ok := b.(int64); ok {
			if y == -1 {
				return int64(0), nil // MinInt64 % -1 would overflow
			}
			return x % y, nil
		}
	}
	return math.Mod(toFloat(a), toFloat(b)), nil
}

// divide is MySQL's /: never integer division. The result is a REAL; the dialect types
// it DECIMAL or DOUBLE and the text protocol prints it at that scale.
func divide(args []any) (any, error) {
	a, b, done, err := operands(args)
	if done {
		return nil, err
	}
	return toFloat(a) / toFloat(b), nil
}

// intDiv is MySQL's DIV: the quotient truncated toward zero, exact for two integers. A
// quotient outside BIGINT stays a REAL, which the text protocol reports as 1690.
func intDiv(args []any) (any, error) {
	a, b, done, err := operands(args)
	if done {
		return nil, err
	}
	if x, ok := a.(int64); ok {
		if y, ok := b.(int64); ok {
			if x == math.MinInt64 && y == -1 {
				return -float64(math.MinInt64), nil
			}
			return x / y, nil
		}
	}
	q := math.Trunc(toFloat(a) / toFloat(b))
	if q >= -(1<<63) && q < 1<<63 {
		return int64(q), nil
	}
	return q, nil
}

// xor is MySQL's logical XOR: NULL if either operand is NULL, else 1 when exactly one
// operand is true (a non-zero number; strings convert to their leading number).
func xor(args []any) (any, error) {
	a, b := number(args[0]), number(args[1])
	if a == nil || b == nil {
		return nil, nil
	}
	if (toFloat(a) != 0) != (toFloat(b) != 0) {
		return int64(1), nil
	}
	return int64(0), nil
}

// like(value, pattern, escape, binary) is MySQL's LIKE: escape is the escape character's
// code point (negative: none), binary 1 compares code points, 0 the mysql_ci weights.
// NULL if value or pattern is NULL.
func like(args []any) (any, error) {
	value, ok1 := text(args[0])
	pattern, ok2 := text(args[1])
	if !ok1 || !ok2 {
		return nil, nil
	}
	escape, _ := args[2].(int64)
	match := collate.LikeCI
	if bin, _ := args[3].(int64); bin != 0 {
		match = collate.LikeBin
	}
	if match(value, pattern, rune(escape)) {
		return int64(1), nil
	}
	return int64(0), nil
}

// text is an argument's string form; false for NULL.
func text(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case []byte:
		return string(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case float64:
		return meta.FormatDouble(x, 64), true
	}
	return "", false
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

// cut(s, n) is s cut to at most n bytes without splitting a UTF-8 character, as MySQL
// cuts GROUP_CONCAT at group_concat_max_len. A non-integer n (beyond BIGINT) cuts
// nothing.
func cut(args []any) (any, error) {
	n, ok := args[1].(int64)
	var s string
	switch x := args[0].(type) {
	case nil:
		return nil, nil
	case string:
		s = x
	case []byte:
		s = string(x)
	default:
		return args[0], nil
	}
	if !ok || int64(len(s)) <= n {
		return s, nil
	}
	end := int(n)
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end], nil
}

// raise(code) fails with the MySQL error code; errs.Map recovers it.
func raise(args []any) (any, error) {
	code, _ := args[0].(int64)
	switch code {
	case 1242:
		return nil, errs.FunctionError(errs.SubqueryRows())
	case 1365:
		return nil, errs.FunctionError(errs.DivisionByZero())
	}
	return nil, fmt.Errorf("udf: %s(%v)", functions.UDFRaise, args[0])
}

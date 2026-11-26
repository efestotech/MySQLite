// Package udf implements the SQL functions mysqlite registers on every SQLite
// connection, for the MySQL functions SQLite has no equivalent of
// (docs/05-dialect.md section 5). The function table (package functions) names them.
package udf

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
		{Name: functions.UDFRound, NumArgs: 3, Deterministic: true, Fn: round},
		{Name: functions.UDFTzFromUTC, NumArgs: 2, Deterministic: true, Fn: tzFromUTC},
		{Name: functions.UDFTzToUTC, NumArgs: 2, Deterministic: true, Fn: tzToUTC},
		{Name: functions.UDFDatePart, NumArgs: 2, Deterministic: true, Fn: datePart},
		{Name: functions.UDFDateName, NumArgs: 2, Deterministic: true, Fn: dateName},
		{Name: functions.UDFDateOnly, NumArgs: 2, Deterministic: true, Fn: dateOnly},
		{Name: functions.UDFDateDiff, NumArgs: 2, Deterministic: true, Fn: dateDiff},
		{Name: functions.UDFDateFormat, NumArgs: 2, Deterministic: true, Fn: dateFormat},
		{Name: functions.UDFUnixTimestamp, NumArgs: 2, Deterministic: true, Fn: unixTimestamp},
		{Name: functions.UDFFromUnixtime, NumArgs: 3, Deterministic: true, Fn: fromUnixtime},
		{Name: functions.UDFDateAdd, NumArgs: 4, Deterministic: true, Fn: dateAdd},
		{Name: functions.UDFTimestampAdd, NumArgs: 3, Deterministic: true, Fn: timestampAdd},
		{Name: functions.UDFTimestampDiff, NumArgs: 3, Deterministic: true, Fn: timestampDiff},
		{Name: functions.UDFExtract, NumArgs: 2, Deterministic: true, Fn: extract},
		{Name: functions.UDFWeek, NumArgs: 2, Deterministic: true, Fn: weekOf},
		{Name: functions.UDFTime, NumArgs: 1, Deterministic: true, Fn: timeOf},
		{Name: functions.UDFMakeDate, NumArgs: 2, Deterministic: true, Fn: makeDate},
		{Name: functions.UDFPeriodDiff, NumArgs: 2, Deterministic: true, Fn: periodDiff},
		{Name: functions.UDFStrToDate, NumArgs: 2, Deterministic: true, Fn: strToDate},
		{Name: functions.UDFTimeFormat, NumArgs: 2, Deterministic: true, Fn: timeFormat},
		{Name: functions.UDFTruncate, NumArgs: 3, Deterministic: true, Fn: truncate},
		{Name: functions.UDFDivide, NumArgs: 3, Deterministic: true, Fn: divide},
		{Name: functions.UDFIntDiv, NumArgs: 3, Deterministic: true, Fn: intDiv},
		{Name: functions.UDFXor, NumArgs: 2, Deterministic: true, Fn: xor},
		{Name: functions.UDFLike, NumArgs: 4, Deterministic: true, Fn: like},
		{Name: functions.UDFRegexp, NumArgs: 3, Deterministic: true, Fn: regexpMatch},
		{Name: functions.UDFSubstring, NumArgs: -1, Deterministic: true, Fn: substring},
		{Name: functions.UDFLeft, NumArgs: 2, Deterministic: true, Fn: left},
		{Name: functions.UDFRight, NumArgs: 2, Deterministic: true, Fn: right},
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

// like(value, pattern, escape, mode) is MySQL's LIKE: escape is the escape character's
// code point (negative: none); mode 1 compares code points, 2 the utf8mb4_0900_ai_ci
// keys, 0 the mysql_ci weights.
// NULL if value or pattern is NULL.
func like(args []any) (any, error) {
	value, ok1 := text(args[0])
	pattern, ok2 := text(args[1])
	if !ok1 || !ok2 {
		return nil, nil
	}
	escape, _ := args[2].(int64)
	match := collate.LikeCI
	switch mode, _ := args[3].(int64); mode {
	case 1:
		match = collate.LikeBin
	case 2:
		match = collate.Like0900AI
	}
	if match(value, pattern, rune(escape)) {
		return int64(1), nil
	}
	return int64(0), nil
}

// regexpMatch is mysqlite_regexp(value, pattern, binary): 1 when pattern matches
// anywhere in value, NULL when either is NULL. Compiled patterns are cached.
func regexpMatch(args []any) (any, error) {
	value, ok1 := text(args[0])
	pattern, ok2 := text(args[1])
	if !ok1 || !ok2 {
		return nil, nil
	}
	binary, _ := args[2].(int64)
	re, err := compileRegexp(pattern, binary == 1)
	if err != nil {
		return nil, err
	}
	if re.MatchString(value) {
		return int64(1), nil
	}
	return int64(0), nil
}

var regexps struct {
	sync.Mutex
	m map[regexpKey]*regexp.Regexp
}

type regexpKey struct {
	pattern string
	binary  bool
}

// compileRegexp compiles a REGEXP pattern, case-insensitive unless binary. The cache is
// emptied when it holds 256 patterns: queries use few distinct patterns.
func compileRegexp(pattern string, binary bool) (*regexp.Regexp, error) {
	k := regexpKey{pattern, binary}
	regexps.Lock()
	re, ok := regexps.m[k]
	regexps.Unlock()
	if ok {
		return re, nil
	}
	src := pattern
	if !binary {
		src = "(?i)" + pattern
	}
	re, err := regexp.Compile(src)
	if err != nil {
		return nil, errs.FunctionError(errs.RegexpIllegal())
	}
	regexps.Lock()
	if regexps.m == nil || len(regexps.m) >= 256 {
		regexps.m = map[regexpKey]*regexp.Regexp{}
	}
	regexps.m[k] = re
	regexps.Unlock()
	return re, nil
}

// pieces splits a string argument into characters, or bytes for a binary string (a
// []byte); ok is false for NULL. join puts a slice of them back in the same form.
func pieces(v any) (parts []string, join func([]string) any, ok bool) {
	switch x := v.(type) {
	case []byte:
		parts = make([]string, len(x))
		for i := range x {
			parts[i] = string(x[i : i+1])
		}
		return parts, func(p []string) any { return []byte(strings.Join(p, "")) }, true
	case nil:
		return nil, nil, false
	}
	s, ok := text(v)
	if !ok {
		return nil, nil, false
	}
	return strings.Split(s, ""), func(p []string) any { return strings.Join(p, "") }, true
}

// intArg is an integer argument as MySQL rounds it; false for NULL.
func intArg(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case nil:
		return 0, false
	}
	s, ok := text(v)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, true
	}
	return int64(math.Round(f)), true
}

// substring is SUBSTRING(s, pos[, len]): pos counts from 1, or from the end when
// negative; pos 0, a position past either end, or len < 1 give ”.
func substring(args []any) (any, error) {
	if len(args) < 2 || len(args) > 3 {
		return nil, fmt.Errorf("udf: %s takes 2 or 3 arguments", functions.UDFSubstring)
	}
	parts, join, ok := pieces(args[0])
	pos, ok2 := intArg(args[1])
	if !ok || !ok2 {
		return nil, nil
	}
	n := int64(len(parts))
	count := n
	if len(args) == 3 {
		c, ok := intArg(args[2])
		if !ok {
			return nil, nil
		}
		count = c
	}
	var start int64
	switch {
	case pos > 0 && pos <= n:
		start = pos - 1
	case pos < 0 && -pos <= n:
		start = n + pos
	default:
		return join(nil), nil
	}
	if count < 1 {
		return join(nil), nil
	}
	end := min(n, start+min(count, n))
	return join(parts[start:end]), nil
}

// left is LEFT(s, n): the first n characters, ” for n < 1.
func left(args []any) (any, error) {
	parts, join, ok := pieces(args[0])
	n, ok2 := intArg(args[1])
	if !ok || !ok2 {
		return nil, nil
	}
	return join(parts[:max(0, min(n, int64(len(parts))))]), nil
}

// right is RIGHT(s, n): the last n characters, ” for n < 1.
func right(args []any) (any, error) {
	parts, join, ok := pieces(args[0])
	n, ok2 := intArg(args[1])
	if !ok || !ok2 {
		return nil, nil
	}
	k := max(0, min(n, int64(len(parts))))
	return join(parts[int64(len(parts))-k:]), nil
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

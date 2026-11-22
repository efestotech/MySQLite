package udf

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// round(x, d, exact) is ROUND(x, d) and truncate(x, d, exact) is TRUNCATE(x, d). An
// exact number (exact = 1: an integer or a DECIMAL, stored as REAL under ADR 0006)
// rounds half away from zero on the digits of its shortest decimal form, so 2.675 is
// 2.68 as in MySQL; a DOUBLE rounds half to even on its binary value, as MySQL's
// my_double_round does.
func round(args []any) (any, error) {
	return roundArgs(args, false)
}

func truncate(args []any) (any, error) {
	return roundArgs(args, true)
}

func roundArgs(args []any, trunc bool) (any, error) {
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	places, err := placesArg(args[1])
	if err != nil {
		return nil, err
	}
	exact, _ := args[2].(int64)
	switch x := args[0].(type) {
	case int64:
		if exact == 1 {
			return roundInt(x, places, trunc), nil
		}
		return roundDouble(float64(x), places, trunc), nil
	case float64:
		if exact == 1 {
			return roundDecimal(x, places, trunc), nil
		}
		return roundDouble(x, places, trunc), nil
	case string:
		return roundDouble(toFloat(parseNumber(x)), places, trunc), nil
	case []byte:
		return roundDouble(toFloat(parseNumber(string(x))), places, trunc), nil
	}
	return nil, fmt.Errorf("udf: round of %T", args[0])
}

func placesArg(v any) (int, error) {
	switch d := v.(type) {
	case int64:
		return int(max(min(d, 1000), -1000)), nil
	case float64:
		return int(max(min(math.Round(d), 1000), -1000)), nil
	case string:
		return int(max(min(math.Round(toFloat(parseNumber(d))), 1000), -1000)), nil
	}
	return 0, fmt.Errorf("udf: round places of %T", v)
}

// roundInt rounds an integer to 10^-places; places >= 0 leaves it unchanged.
func roundInt(x int64, places int, trunc bool) any {
	if places >= 0 {
		return x
	}
	if places < -18 {
		return int64(0)
	}
	p := int64(1)
	for range -places {
		p *= 10
	}
	q, r := x/p, x%p
	if !trunc && (r >= p-r || -r >= p+r) {
		if x < 0 {
			q--
		} else {
			q++
		}
	}
	if q > math.MaxInt64/p || q < math.MinInt64/p {
		return roundDecimal(float64(x), places, trunc)
	}
	return q * p
}

// roundDecimal rounds the shortest decimal form of x half away from zero (or truncates).
func roundDecimal(x float64, places int, trunc bool) float64 {
	if math.IsInf(x, 0) || math.IsNaN(x) {
		return x
	}
	s := strconv.FormatFloat(math.Abs(x), 'f', -1, 64)
	intPart, frac, _ := strings.Cut(s, ".")
	digits := intPart + frac
	keep := len(intPart) + places
	if keep >= len(digits) {
		return x
	}
	var kept string
	up := false
	if keep >= 0 {
		kept = digits[:keep]
		up = !trunc && digits[keep] >= '5'
	}
	if up {
		kept = incrementDigits(kept)
	}
	if kept == "" {
		kept = "0"
	}
	v, err := strconv.ParseFloat(kept+"e"+strconv.Itoa(len(intPart)-keep), 64)
	if err != nil {
		return x
	}
	if x < 0 {
		v = -v
	}
	return v
}

// incrementDigits adds one to a decimal digit string.
func incrementDigits(s string) string {
	b := []byte(s)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < '9' {
			b[i]++
			return string(b)
		}
		b[i] = '0'
	}
	return "1" + string(b)
}

// roundDouble is MySQL's my_double_round.
func roundDouble(x float64, places int, trunc bool) float64 {
	neg := places < 0
	p := math.Pow(10, math.Abs(float64(places)))
	if neg && math.IsInf(p, 0) {
		return 0
	}
	scaled := x * p
	if neg {
		scaled = x / p
	}
	if !neg && math.IsInf(scaled, 0) {
		return x
	}
	var r float64
	switch {
	case !trunc:
		r = math.RoundToEven(scaled)
	case x >= 0:
		r = math.Floor(scaled)
	default:
		r = math.Ceil(scaled)
	}
	if neg {
		return r * p
	}
	return r / p
}

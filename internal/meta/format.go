package meta

import (
	"strconv"
	"strings"
)

// Number formatting shared by the text protocol and the SQL functions that turn numbers
// into strings (CONCAT, LENGTH, ...), so a value prints the same in both places.

// FormatDouble prints the shortest representation that reads back as the same value,
// in plain notation for decimal exponents -6..14 and as "1.5e-7" / "1e20" otherwise,
// as MySQL prints a DOUBLE (bits 64) or FLOAT (bits 32) without a declared scale.
func FormatDouble(f float64, bits int) string {
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

// FormatDecimal prints an integer or a REAL with scale decimals. DECIMAL is stored as
// REAL (docs/05-dialect.md section 9, option a), so rounding follows the binary value.
// ok is false for other value types.
func FormatDecimal(v any, scale int) (s string, ok bool) {
	switch x := v.(type) {
	case int64:
		s = strconv.FormatInt(x, 10)
		if scale > 0 {
			s += "." + strings.Repeat("0", scale)
		}
	case float64:
		s = strconv.FormatFloat(x, 'f', scale, 64)
		if strings.Trim(s, "-0.") == "" {
			s = strings.TrimPrefix(s, "-") // no negative zero
		}
	default:
		return "", false
	}
	return s, true
}

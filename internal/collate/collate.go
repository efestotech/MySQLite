// Package collate implements the SQLite collations that give text columns MySQL's
// comparison rules (docs/05-dialect.md section 8, ADR 0004):
//
//   - mysql_ci approximates utf8mb4_general_ci: each character compares by the upper case
//     of its base letter (accents removed by canonical decomposition), so 'città' =
//     'CITTA'; every character outside the Basic Multilingual Plane weighs the same, as in
//     MySQL's general_ci.
//   - mysql_bin is utf8mb4_bin: code point order.
//
// These two are PAD SPACE, like the MySQL collations they stand for: trailing spaces do not
// take part in the comparison, so 'a' = 'a  '. The NO PAD 0900 collations are in
// collate0900.go.
//
// The weights are derived from Unicode data with golang.org/x/text, not from MySQL's
// tables. Full parity with MySQL's general_ci table is a non-goal; the differences are
// listed in ADR 0004.
package collate

import (
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"mysqlite/internal/engine"
)

// Names of the collations registered on every SQLite connection.
const (
	CI  = "mysql_ci"
	Bin = "mysql_bin"
)

// Collations returns the collations to register on every connection.
func Collations() []engine.Collation {
	return []engine.Collation{
		{Name: CI, Compare: CompareCI},
		{Name: Bin, Compare: CompareBin},
		{Name: AI0900, Compare: Compare0900AI},
		{Name: Bin0900, Compare: Compare0900Bin},
	}
}

// supplementary is the weight of every character above U+FFFF (MySQL: 0xFFFD).
const supplementary = 0xFFFD

var (
	weightsOnce sync.Once
	weights     []uint16 // general_ci-like weight of each BMP code point
)

func buildWeights() {
	weights = make([]uint16, 0x10000)
	var buf [utf8.UTFMax]byte
	for r := rune(0); r < 0x10000; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			weights[r] = supplementary
			continue
		}
		n := utf8.EncodeRune(buf[:], r)
		base, _ := utf8.DecodeRune(norm.NFD.Bytes(buf[:n]))
		switch base {
		case 'ß':
			base = 'S'
		default:
			base = unicode.ToUpper(base)
		}
		if base > 0xFFFF {
			base = supplementary
		}
		weights[r] = uint16(base)
	}
}

func weight(r rune) uint16 {
	if r > 0xFFFF {
		return supplementary
	}
	return weights[r]
}

// CompareCI is mysql_ci.
func CompareCI(a, b string) int {
	weightsOnce.Do(buildWeights)
	a, b = trimPad(a), trimPad(b)
	for a != "" && b != "" {
		ra, na := utf8.DecodeRuneInString(a)
		rb, nb := utf8.DecodeRuneInString(b)
		if wa, wb := weight(ra), weight(rb); wa != wb {
			if wa < wb {
				return -1
			}
			return 1
		}
		a, b = a[na:], b[nb:]
	}
	return compareLen(a, b)
}

// CompareBin is mysql_bin. UTF-8 byte order is code point order.
func CompareBin(a, b string) int {
	return strings.Compare(trimPad(a), trimPad(b))
}

// trimPad drops the trailing spaces PAD SPACE ignores. Only U+0020 is padding.
func trimPad(s string) string {
	return strings.TrimRight(s, " ")
}

func compareLen(a, b string) int {
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return -1
	}
	return 1
}

// LikeCI is MySQL's LIKE under mysql_ci: % matches any run of characters, _ one
// character, and escape makes the next pattern character literal (a negative escape is
// none). Characters compare by their mysql_ci weight; unlike =, LIKE does not ignore
// trailing spaces.
func LikeCI(value, pattern string, escape rune) bool {
	weightsOnce.Do(buildWeights)
	return like(value, pattern, escape, func(r rune) rune { return rune(weight(r)) })
}

// LikeBin is MySQL's LIKE under a binary collation: characters compare by code point.
func LikeBin(value, pattern string, escape rune) bool {
	return like(value, pattern, escape, func(r rune) rune { return r })
}

func like(value, pattern string, escape rune, weigh func(rune) rune) bool {
	type token struct {
		w    rune
		kind byte // 0 literal, '%' or '_'
	}
	var p []token
	pr := []rune(pattern)
	for i := 0; i < len(pr); i++ {
		r := pr[i]
		switch {
		case escape >= 0 && r == escape && i+1 < len(pr):
			i++
			p = append(p, token{w: weigh(pr[i])})
		case r == '%' || r == '_':
			p = append(p, token{kind: byte(r)})
		default:
			p = append(p, token{w: weigh(r)})
		}
	}
	var v []rune
	for _, r := range value {
		v = append(v, weigh(r))
	}
	// Greedy matching with one backtrack point, the last %.
	vi, pi, star, mark := 0, 0, -1, 0
	for vi < len(v) {
		switch {
		case pi < len(p) && p[pi].kind == '%':
			star, mark = pi, vi
			pi++
		case pi < len(p) && (p[pi].kind == '_' || p[pi].kind == 0 && p[pi].w == v[vi]):
			vi++
			pi++
		case star >= 0:
			mark++
			vi, pi = mark, star+1
		default:
			return false
		}
	}
	for pi < len(p) && p[pi].kind == '%' {
		pi++
	}
	return pi == len(p)
}

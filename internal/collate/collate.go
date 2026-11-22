// Package collate implements the SQLite collations that give text columns MySQL's
// comparison rules (docs/05-dialect.md section 8, ADR 0004):
//
//   - mysql_ci approximates utf8mb4_general_ci: each character compares by the upper case
//     of its base letter (accents removed by canonical decomposition), so 'città' =
//     'CITTA'; every character outside the Basic Multilingual Plane weighs the same, as in
//     MySQL's general_ci.
//   - mysql_bin is utf8mb4_bin: code point order.
//
// Both are PAD SPACE, like the MySQL collations they stand for: trailing spaces do not
// take part in the comparison, so 'a' = 'a  '.
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
	return []engine.Collation{{Name: CI, Compare: CompareCI}, {Name: Bin, Compare: CompareBin}}
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

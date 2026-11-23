package collate

import (
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Names of the SQLite collations that stand for MySQL 8's utf8mb4_0900_ai_ci and
// utf8mb4_0900_bin.
//
// Both are NO PAD: trailing spaces take part in the comparison, so 'a' < 'a '.
// mysql_0900_ai_ci approximates the Unicode Collation Algorithm at primary strength: a
// character compares by the lower case of its base letter (accents removed by canonical
// decomposition), ß and ẞ expand to "ss", and characters outside the Basic Multilingual
// Plane keep distinct weights (their code points). It is not UCA: punctuation, digits
// and letters keep code point order among themselves, and contractions and other
// expansions are not modelled. ADR 0004 lists the differences.
const (
	AI0900  = "mysql_0900_ai_ci"
	Bin0900 = "mysql_0900_bin"
)

var (
	keysOnce sync.Once
	keys0900 []rune // 0900_ai_ci key of each BMP code point; ß and ẞ expand in Compare0900AI
)

func buildKeys0900() {
	keys0900 = make([]rune, 0x10000)
	var buf [utf8.UTFMax]byte
	for r := rune(0); r < 0x10000; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			keys0900[r] = r
			continue
		}
		n := utf8.EncodeRune(buf[:], r)
		base, _ := utf8.DecodeRune(norm.NFD.Bytes(buf[:n]))
		keys0900[r] = unicode.ToLower(base)
	}
}

func key0900(r rune) rune {
	if r > 0xFFFF {
		return r
	}
	return keys0900[r]
}

// expanded appends the 0900_ai_ci keys of s, with ß and ẞ as two s.
func expanded(dst []rune, s string) []rune {
	for _, r := range s {
		if r == 'ß' || r == 'ẞ' {
			dst = append(dst, 's', 's')
			continue
		}
		dst = append(dst, key0900(r))
	}
	return dst
}

// Compare0900AI is mysql_0900_ai_ci.
func Compare0900AI(a, b string) int {
	keysOnce.Do(buildKeys0900)
	var ba, bb [64]rune
	ka, kb := expanded(ba[:0], a), expanded(bb[:0], b)
	for i := 0; i < len(ka) && i < len(kb); i++ {
		if ka[i] != kb[i] {
			if ka[i] < kb[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(ka) < len(kb):
		return -1
	case len(ka) > len(kb):
		return 1
	}
	return 0
}

// Compare0900Bin is mysql_0900_bin: code point order, NO PAD.
func Compare0900Bin(a, b string) int {
	return strings.Compare(a, b)
}

// Like0900AI is MySQL's LIKE under mysql_0900_ai_ci. Each character compares by its
// key, one character at a time: ß matches one character, not "ss".
func Like0900AI(value, pattern string, escape rune) bool {
	keysOnce.Do(buildKeys0900)
	return like(value, pattern, escape, key0900)
}

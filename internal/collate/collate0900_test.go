package collate

import "testing"

// utf8mb4_0900_ai_ci approximated by docs/05-dialect.md section 8: NFD, combining
// marks stripped, case folded, NO PAD. `oracle: pending`.
func TestCompare0900AI(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"città", "CITTA", 0},
		{"città", "citta", 0},
		{"perché", "PERCHE", 0},
		{"Perché", "perche ", -1}, // NO PAD: the trailing space counts
		{"a", "a ", -1},
		{"a", "B", -1},
		{"straße", "STRASSE", 0}, // full case folding: ß is ss
		{"😀", "😁", -1},           // supplementary characters keep their own weights
		{"_", "a", -1},           // punctuation before letters, as in UCA
		{"Ünïcödé", "unicode", 0},
		{"", "", 0},
		{"", " ", -1},
	}
	for _, c := range cases {
		if got := sign(Compare0900AI(c.a, c.b)); got != c.want {
			t.Errorf("Compare0900AI(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := sign(Compare0900AI(c.b, c.a)); got != -c.want {
			t.Errorf("Compare0900AI(%q, %q) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

// utf8mb4_0900_bin: code point order, NO PAD.
func TestCompare0900Bin(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"a", "a", 0},
		{"a", "a ", -1},
		{"A", "a", -1},
		{"città", "citta", 1},
	}
	for _, c := range cases {
		if got := sign(Compare0900Bin(c.a, c.b)); got != c.want {
			t.Errorf("Compare0900Bin(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// LIKE under utf8mb4_0900_ai_ci folds accents and case one character at a time.
func TestLike0900AI(t *testing.T) {
	cases := []struct {
		value, pattern string
		want           bool
	}{
		{"città", "CITTA", true},
		{"città", "cit_a", true},
		{"perché", "PERCH%", true},
		{"perché", "perche_", false},
		{"Perché", "%É", true},
		{"😀", "😁", false},
		{"a b", "a_b", true},
	}
	for _, c := range cases {
		if got := Like0900AI(c.value, c.pattern, '\\'); got != c.want {
			t.Errorf("Like0900AI(%q, %q) = %v, want %v", c.value, c.pattern, got, c.want)
		}
	}
}

package collate

import "testing"

// Expected results are MySQL 8.0's for utf8mb4_general_ci and utf8mb4_bin
// (`oracle: pending`, ADR 0002 addendum).
func TestCompareCI(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"città", "CITTA", 0},
		{"perché", "PERCHE", 0},
		{"Perché", "perche ", 0}, // PAD SPACE
		{"a", "a  ", 0},
		{"a", "B", -1},
		{"b", "A", 1},
		{"a", "ab", -1},
		{"a ", "a\t", -1}, // only U+0020 is padding
		{"straße", "STRASE", 0},
		{"😀", "😁", 0}, // general_ci gives every supplementary character one weight
		{"", "", 0},
		{"", " ", 0},
		{"Ünïcödé", "unicode", 0},
	}
	for _, c := range cases {
		if got := sign(CompareCI(c.a, c.b)); got != c.want {
			t.Errorf("CompareCI(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := sign(CompareCI(c.b, c.a)); got != -c.want {
			t.Errorf("CompareCI(%q, %q) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

func TestCompareBin(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"a", "a  ", 0},
		{"A", "a", -1},
		{"città", "CITTA", 1},
		{"é", "z", 1}, // code point order
		{"😀", "😁", -1},
	}
	for _, c := range cases {
		if got := sign(CompareBin(c.a, c.b)); got != c.want {
			t.Errorf("CompareBin(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

func TestLikeCI(t *testing.T) {
	cases := []struct {
		value, pattern string
		want           bool
	}{
		{"città", "CITTA", true},
		{"città", "%a", true},
		{"città", "c_tt_", true},
		{"città", "c_t", false},
		{"items", "it%", true},
		{"items", "%", true},
		{"", "%", true},
		{"", "_", false},
		{"g_x", `g\_%`, true},
		{"gadgets", `g\_%`, false},
		{"abc", "a%c%", true},
		{"abcbc", "%bc", true},
		{"a ", "a", false},
		{"Straße", "STRASE", true},
		{"100%", `100\%`, true},
		{"1000", `100\%`, false},
	}
	for _, c := range cases {
		if got := LikeCI(c.value, c.pattern, '\\'); got != c.want {
			t.Errorf("%q LIKE %q: want %v, got %v", c.value, c.pattern, c.want, got)
		}
	}
}

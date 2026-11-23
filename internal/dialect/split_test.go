package dialect

import (
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	tr := New(Config{ServerVersion: 80036})
	for _, c := range []struct {
		query string
		want  []string
	}{
		{"SELECT 1", []string{"SELECT 1"}},
		{"SELECT 1;", []string{"SELECT 1;"}},
		{"SELECT 1; -- done", []string{"SELECT 1; -- done"}},
		{"SELECT 1; SELECT 2", []string{"SELECT 1", " SELECT 2"}},
		{"SELECT 1;SELECT 2;", []string{"SELECT 1", "SELECT 2"}},
		{"SELECT 1;; SELECT 2", []string{"SELECT 1", " SELECT 2"}},
		{"SELECT 1; /* c */ ; # c\nSELECT 2", []string{"SELECT 1", " # c\nSELECT 2"}},
		{"SELECT 'a;b'; SELECT \"c;d\"", []string{"SELECT 'a;b'", " SELECT \"c;d\""}},
		{"SELECT `x;y` FROM t; SELECT 2", []string{"SELECT `x;y` FROM t", " SELECT 2"}},
		{"SELECT 'it\\'s;'; SELECT 2", []string{"SELECT 'it\\'s;'", " SELECT 2"}},
		{"SELECT 1 /* ; */; SELECT 2 -- ;\n", []string{"SELECT 1 /* ; */", " SELECT 2 -- ;\n"}},
		{"SELECT 1 /*!80000 ; */; SELECT 2", []string{"SELECT 1 /*!80000 ; */", " SELECT 2"}},
		{";", []string{";"}},
	} {
		if got := tr.Split(c.query); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Split(%q) = %q, want %q", c.query, got, c.want)
		}
	}
}

// With NO_BACKSLASH_ESCAPES a backslash does not escape the quote.
func TestSplitNoBackslashEscapes(t *testing.T) {
	tr := New(Config{ServerVersion: 80036})
	tr.SetSQLMode("NO_BACKSLASH_ESCAPES")
	want := []string{`SELECT 'a\'`, " SELECT 2"}
	if got := tr.Split(`SELECT 'a\'; SELECT 2`); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

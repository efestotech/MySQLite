package dialect

import (
	"reflect"
	"testing"
)

func TestMarkers(t *testing.T) {
	tr := newTranslator(t)
	cases := []struct {
		query string
		want  []Marker
	}{
		{"SELECT * FROM t WHERE a = ? AND b = '?' AND `c?` = ? /* ? */", []Marker{{Offset: 26}, {Offset: 51}}},
		{"SELECT a FROM t ORDER BY a LIMIT ?, ?", []Marker{{Offset: 33, Limit: true}, {Offset: 36, Limit: true}}},
		{"SELECT a FROM t LIMIT ? OFFSET ?", []Marker{{Offset: 22, Limit: true}, {Offset: 31, Limit: true}}},
		{"INSERT INTO t (a, b) VALUES (?, ?), (?, -?)", []Marker{{Offset: 29}, {Offset: 32}, {Offset: 37}, {Offset: 41}}},
		{"SELECT 1", nil},
	}
	for _, c := range cases {
		got, e := tr.Markers(c.query)
		if e != nil {
			t.Errorf("%s: %v", c.query, e)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.query, got, c.want)
		}
		for _, m := range got {
			if c.query[m.Offset] != '?' {
				t.Errorf("%s: offset %d is %q", c.query, m.Offset, c.query[m.Offset])
			}
		}
	}
	if _, e := tr.Markers("SELECT ? FROM"); e == nil || e.Code != 1064 {
		t.Errorf("syntax error: %v", e)
	}
	if _, e := tr.Markers("SELECT 1; SELECT ?"); e == nil || e.Code != 1064 {
		t.Errorf("two statements: %v", e)
	}
}

func TestSubstitute(t *testing.T) {
	q := "SELECT a FROM t WHERE b = ? LIMIT ?"
	tr := newTranslator(t)
	m, e := tr.Markers(q)
	if e != nil {
		t.Fatal(e)
	}
	if got := Substitute(q, m, Placeholders(m)); got != "SELECT a FROM t WHERE b = NULL LIMIT 0" {
		t.Errorf("placeholders: %q", got)
	}
	if got := Substitute(q, m, []string{"'x?'", "5"}); got != "SELECT a FROM t WHERE b = 'x?' LIMIT 5" {
		t.Errorf("values: %q", got)
	}
}

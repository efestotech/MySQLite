package dialect

import "testing"

// MySQL names an unaliased select item by its text as written, from its first token to
// the end of its last one (MySQL 8.0.46, harness run of 2026-11-24, corpus
// trap.lexical.008, .012-.014).
func TestItemText(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"1 /* inline */ + 1 # to the end of the line", "1 /* inline */ + 1"},
		{"1--1", "1--1"}, // "--" without a space is two minus signs
		{"1 -- 1", "1"},  // "-- " starts a comment
		{"1 /*!40000 + 1 */ /*!90000 + 100 */", "1  + 1"}, // executable comment kept, newer one dropped
		{"2.50 /* c */", "2.50"},                          // trailing comment
		{"'a#b' -- c", "'a#b'"},                           // a marker inside a string is text
		{"CONCAT('x', \"/*\")", "CONCAT('x', \"/*\")"},    // quotes protect comment markers
		{"@@session.sql_mode", "@@session.sql_mode"},
		{"a  +\tb", "a  +\tb"},
	} {
		if got := itemText(c.in); got != c.want {
			t.Errorf("itemText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A string written as adjacent literals is named by the first one.
func TestFirstStringPart(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
		ok   bool
	}{
		{`'a' 'b' "c"`, "a", true},
		{`'it''s' 'x'`, "it's", true},
		{`'a\nb' 'c'`, "a\nb", true},
		{`'abc'`, "", false},
		{`"one"`, "", false},
	} {
		got, ok := firstStringPart(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("firstStringPart(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

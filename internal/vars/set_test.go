package vars

import (
	"testing"
)

func TestNormalizeSQLMode(t *testing.T) {
	cases := []struct {
		in, want string
		warnings int
	}{
		{"strict_trans_tables , ansi_quotes", "ANSI_QUOTES,STRICT_TRANS_TABLES", 1}, // dropped NO_ZERO_* etc.
		{DefaultSQLMode, DefaultSQLMode, 0},
		{"TRADITIONAL", "STRICT_TRANS_TABLES,STRICT_ALL_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,TRADITIONAL,NO_ENGINE_SUBSTITUTION", 0},
		{"", "", 2}, // not strict, and the NO_ZERO_* modes dropped
	}
	for _, c := range cases {
		got, warnings, e := NormalizeSQLMode(c.in)
		if e != nil || got != c.want || len(warnings) != c.warnings {
			t.Errorf("%q: got %q, %d warnings, %v; want %q, %d warnings", c.in, got, len(warnings), e, c.want, c.warnings)
		}
	}
	if _, _, e := NormalizeSQLMode("ANSI,NOPE"); e == nil || e.Code != 1231 {
		t.Errorf("unknown mode: got %v", e)
	}
	if got, _, e := sqlModeFromBits("6"); e != nil || got != "PIPES_AS_CONCAT,ANSI_QUOTES" {
		t.Errorf("sql_mode = 6: got %q %v", got, e)
	}
	if _, _, e := sqlModeFromBits("16"); e == nil {
		t.Error("sql_mode with an unused bit was accepted")
	}
}

func TestCheckOrderAndValues(t *testing.T) {
	s := newSession(t, nil)
	num := func(n string) Input { return Input{Number: true, Integer: true, Text: n} }
	word := func(w string) Input { return Input{Text: w} }
	cases := []struct {
		name   string
		global bool
		in     Input
		code   uint16
	}{
		{"nope", false, num("1"), 1193},
		{"version", true, word("x"), 1238},  // read-only first, whatever the scope
		{"timestamp", true, num("1"), 1228}, // session-only with GLOBAL
		{"max_allowed_packet", false, num("1"), 1621},
		{"autocommit", false, num("2"), 1231},
		{"autocommit", false, Input{Number: true, Text: "1.5"}, 1232},
		{"wait_timeout", false, word("x"), 1232},
		{"sql_mode", false, num("1"), 0},
		{"time_zone", false, Input{Null: true}, 1231},
		{"time_zone", false, word("+14:01"), 1298},
		{"time_zone", false, word("-05:30"), 0},
		{"character_set_client", false, word("latin1"), 1235},
		{"character_set_client", false, word("klingon"), 1115},
		{"sql_select_limit", false, num("18446744073709551615"), 0}, // the default is allowed
		{"sql_select_limit", false, num("5"), 1235},
		{"binlog_format", false, word("row"), 0},
	}
	for _, c := range cases {
		_, _, e := s.Check(c.name, c.global, c.in)
		var code uint16
		if e != nil {
			code = e.Code
		}
		if code != c.code {
			t.Errorf("SET %s = %q (global %v): want %d, got %v", c.name, c.in.Text, c.global, c.code, e)
		}
	}
}

func TestApplyLinksCharsetAndCollation(t *testing.T) {
	s := newSession(t, nil)
	c, _, e := s.Check("collation_connection", false, Input{Text: "utf8_bin"})
	if e != nil {
		t.Fatal(e)
	}
	s.Apply(c)
	if v, _, _ := s.Get("character_set_connection", ReadDefault); v.Text != "utf8mb3" {
		t.Errorf("character_set_connection: got %q", v.Text)
	}
	c, _, _ = s.Check("character_set_connection", false, Input{Text: "utf8mb4"})
	s.Apply(c)
	if v, _, _ := s.Get("collation_connection", ReadDefault); v.Text != "utf8mb4_general_ci" {
		t.Errorf("collation_connection: got %q", v.Text)
	}

	// SESSION DEFAULT takes the global value; GLOBAL DEFAULT the compiled default.
	g, _, _ := s.Check("wait_timeout", true, Input{Number: true, Integer: true, Text: "60"})
	s.Apply(g)
	d, _, _ := s.Check("wait_timeout", false, Input{Default: true})
	s.Apply(d)
	if v, _, _ := s.Get("wait_timeout", ReadSession); v.Text != "60" {
		t.Errorf("SET SESSION wait_timeout = DEFAULT: got %q", v.Text)
	}
	gd, _, _ := s.Check("wait_timeout", true, Input{Default: true})
	s.Apply(gd)
	if v, _, _ := s.Get("wait_timeout", ReadGlobal); v.Text != "28800" {
		t.Errorf("SET GLOBAL wait_timeout = DEFAULT: got %q", v.Text)
	}

	// SET timestamp fixes the clock; DEFAULT returns to it.
	ts, _, _ := s.Check("timestamp", false, Input{Number: true, Text: "1000.5"})
	s.Apply(ts)
	if v, _, _ := s.Get("timestamp", ReadDefault); v.Text != "1000.500000" {
		t.Errorf("SET timestamp: got %q", v.Text)
	}
	ts, _, _ = s.Check("timestamp", false, Input{Default: true})
	s.Apply(ts)
	if v, _, _ := s.Get("timestamp", ReadDefault); v.Text == "1000.500000" {
		t.Error("SET timestamp = DEFAULT kept the fixed value")
	}
}

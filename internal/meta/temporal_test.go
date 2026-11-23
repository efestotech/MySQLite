package meta

import "testing"

func TestCheckTemporal(t *testing.T) {
	strict := InsertMode{Strict: true, NoZeroDate: true, NoZeroInDate: true}
	cases := []struct {
		ty    uint8
		value string
		mode  InsertMode
		want  string // "" accepted
	}{
		{TypeDate, "0000-00-00", strict, "Incorrect date value: '0000-00-00' for column 'c' at row 2"},
		{TypeDate, "0000-00-00", InsertMode{Strict: true, NoZeroInDate: true}, ""},
		{TypeDate, "0000-00-00", InsertMode{NoZeroDate: true}, ""},
		{TypeDate, "2024-00-10", strict, "Incorrect date value: '2024-00-10' for column 'c' at row 2"},
		{TypeDate, "2024-00-10", InsertMode{Strict: true}, ""},
		{TypeDate, "2023-02-29", strict, "Incorrect date value: '2023-02-29' for column 'c' at row 2"},
		{TypeDate, "2024-02-29", strict, ""},
		{TypeDate, "2023-02-31", InsertMode{Strict: true, AllowInvalidDates: true}, ""},
		{TypeDate, "2023-13-01", InsertMode{Strict: true, AllowInvalidDates: true}, "Incorrect date value: '2023-13-01' for column 'c' at row 2"},
		{TypeDatetime, "2024-01-01 24:00:00", strict, "Incorrect datetime value: '2024-01-01 24:00:00' for column 'c' at row 2"},
		{TypeTimestamp, "2024-04-31 10:00:00", strict, "Incorrect datetime value: '2024-04-31 10:00:00' for column 'c' at row 2"},
		{TypeDatetime, "2024-01-01", strict, ""},
		{TypeDatetime, "2024-1-1", strict, ""}, // not canonical: stored as given
		{TypeDate, "2024-01-01 10:00:00", strict, ""},
		{TypeVarchar, "0000-00-00", strict, ""},
	}
	for _, c := range cases {
		e := checkTemporal(c.ty, "c", &Literal{Kind: LiteralString, Text: c.value}, 2, c.mode)
		switch {
		case c.want == "" && e != nil:
			t.Errorf("%q (%+v): unexpected %v", c.value, c.mode, e)
		case c.want != "" && e == nil:
			t.Errorf("%q (%+v): accepted, want %q", c.value, c.mode, c.want)
		case e != nil && (e.Code != 1292 || e.State != "22007" || e.Message != c.want):
			t.Errorf("%q: got %d %s %q", c.value, e.Code, e.State, e.Message)
		}
	}
}

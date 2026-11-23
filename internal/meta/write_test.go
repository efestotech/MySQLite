package meta

import (
	"reflect"
	"testing"
)

func lit(kind LiteralKind, text string) *Literal { return &Literal{Kind: kind, Text: text} }

func TestCheckInsertAutoIncrement(t *testing.T) {
	null := lit(LiteralNull, "NULL")
	cases := []struct {
		name      string
		columns   []string
		rows      [][]*Literal
		generated int
		explicit  []int64
	}{
		{"omitted", []string{"name"}, [][]*Literal{{lit(LiteralString, "a")}, {lit(LiteralString, "b")}}, 2, nil},
		{"null", []string{"ID", "name"}, [][]*Literal{{null, lit(LiteralString, "a")}}, 1, nil},
		{"explicit", []string{"id", "name"}, [][]*Literal{{lit(LiteralInt, "10"), lit(LiteralString, "a")}, {lit(LiteralInt, "-3"), lit(LiteralString, "b")}}, 0, []int64{10, -3}},
		{"mixed", []string{"id", "name"}, [][]*Literal{{null, lit(LiteralString, "a")}, {lit(LiteralInt, "7"), nil}}, 1, []int64{7}},
		{"no column list", nil, [][]*Literal{{null, lit(LiteralString, "a"), nil, nil, nil, nil, nil}}, 1, nil},
		// MySQL converts a string of digits; PDO's execute(array) binds keys that way.
		{"digit string", []string{"id", "name"}, [][]*Literal{{lit(LiteralString, "12"), lit(LiteralString, "a")}, {lit(LiteralString, "-4"), nil}}, 0, []int64{12, -4}},
	}
	for _, tc := range cases {
		info, err := CheckInsert(shop(), tc.columns, tc.rows)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if info.Auto != "id" || info.Generated != tc.generated || !reflect.DeepEqual(info.Explicit, tc.explicit) {
			t.Errorf("%s: got %+v", tc.name, info)
		}
	}

	// VALUES () inserts one row of defaults; id is generated, name has no default.
	_, err := CheckInsert(shop(), nil, [][]*Literal{{}})
	if err == nil || err.Code != 1364 || err.Message != "Field 'name' doesn't have a default value" {
		t.Errorf("VALUES (): want 1364 for name, got %v", err)
	}

	noAuto := shop()
	noAuto.AutoIncrement = ""
	info, err := CheckInsert(noAuto, []string{"id", "name"}, [][]*Literal{{lit(LiteralInt, "1"), lit(LiteralString, "a")}})
	if err != nil || info.Auto != "" {
		t.Errorf("a table without AUTO_INCREMENT: got %+v, %v", info, err)
	}
}

func TestCheckInsertErrors(t *testing.T) {
	str := lit(LiteralString, "a")
	cases := []struct {
		name    string
		table   func(*Table)
		columns []string
		rows    [][]*Literal
		code    uint16
		msg     string
	}{
		{"unknown column", nil, []string{"name", "nope"}, [][]*Literal{{str, str}}, 1054, "Unknown column 'nope' in 'field list'"},
		{"column twice", nil, []string{"name", "NAME"}, [][]*Literal{{str, str}}, 1110, "Column 'NAME' specified twice"},
		{"row width without column list", nil, nil, [][]*Literal{{str}}, 1136, "Column count doesn't match value count at row 1"},
		{"missing value without default", nil, []string{"price"}, [][]*Literal{{lit(LiteralInt, "1")}}, 1364, "Field 'name' doesn't have a default value"},
		// MySQL generates a value for 0 unless NO_AUTO_VALUE_ON_ZERO; SQLite stores 0.
		{"zero for auto increment", nil, []string{"id", "name"}, [][]*Literal{{lit(LiteralInt, "0"), str}}, 1235,
			"mysqlite: unsupported construct value 0 for an AUTO_INCREMENT column: MySQL generates a value, SQLite would store 0; use NULL, or SET sql_mode with NO_AUTO_VALUE_ON_ZERO to store 0"},
		{"text for auto increment", nil, []string{"id", "name"}, [][]*Literal{{str, str}}, 1235,
			"mysqlite: unsupported construct non-integer value for an AUTO_INCREMENT column: only integers and strings of decimal digits are converted"},
		{"digits and text for auto increment", nil, []string{"id", "name"}, [][]*Literal{{lit(LiteralString, "1a"), str}}, 1235,
			"mysqlite: unsupported construct non-integer value for an AUTO_INCREMENT column: only integers and strings of decimal digits are converted"},
		{"zero string for auto increment", nil, []string{"id", "name"}, [][]*Literal{{lit(LiteralString, "0"), str}}, 1235,
			"mysqlite: unsupported construct value 0 for an AUTO_INCREMENT column: MySQL generates a value, SQLite would store 0; use NULL, or SET sql_mode with NO_AUTO_VALUE_ON_ZERO to store 0"},
		{"expression for auto increment", nil, []string{"id", "name"}, [][]*Literal{{nil, str}}, 1235,
			"mysqlite: unsupported construct non-integer value for an AUTO_INCREMENT column: only integers and strings of decimal digits are converted"},
		// An INTEGER PRIMARY KEY without AUTOINCREMENT generates a rowid for NULL; for
		// MySQL the column is a plain NOT NULL key.
		{"null in a key that is not auto increment", func(t *Table) { t.AutoIncrement = "" }, []string{"id", "name"},
			[][]*Literal{{lit(LiteralNull, "NULL"), str}}, 1048, "Column 'id' cannot be null"},
		{"omitted key that is not auto increment", func(t *Table) { t.AutoIncrement = "" }, []string{"name"},
			[][]*Literal{{str}}, 1364, "Field 'id' doesn't have a default value"},
	}
	for _, tc := range cases {
		tbl := shop()
		if tc.table != nil {
			tc.table(tbl)
		}
		_, err := CheckInsert(tbl, tc.columns, tc.rows)
		if err == nil || err.Code != tc.code || err.Message != tc.msg {
			t.Errorf("%s: want %d %q, got %v", tc.name, tc.code, tc.msg, err)
		}
	}
}

func TestCheckInsertValues(t *testing.T) {
	info, err := CheckInsert(shop(), []string{"Name", "code"}, [][]*Literal{{lit(LiteralString, "a"), nil}})
	if err != nil {
		t.Fatal(err)
	}
	row := info.Values[0]
	if row["name"] == nil || row["name"].Text != "a" {
		t.Errorf("values are keyed by stored column name, got %+v", row)
	}
	if v, ok := row["code"]; !ok || v != nil {
		t.Errorf("an expression is present with a nil literal, got %v %v", v, ok)
	}
	if _, ok := row["price"]; ok {
		t.Error("an omitted column is absent")
	}
}

func TestKeyName(t *testing.T) {
	tbl := shop()
	tbl.Indexes[0].Name = "code"
	tbl.Indexes[1].Name = "ab_uq"
	cases := map[string][]string{
		"PRIMARY": {"ID"},
		"code":    {"code"},
		"ab_uq":   {"a", "b"},
		// Unknown to the catalog: MySQL would name it, the columns are the best guess.
		"b_a": {"b", "a"},
	}
	for want, cols := range cases {
		if got := tbl.KeyName(cols); got != want {
			t.Errorf("%v: want %q, got %q", cols, want, got)
		}
	}
}

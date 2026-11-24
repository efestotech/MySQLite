package catalog

import (
	"strings"
	"testing"

	"mysqlite/internal/meta"
)

func TestDefaultValue(t *testing.T) {
	cases := []struct {
		in   string
		want string // "NULL" for nil
		ok   bool
	}{
		{"NULL", "NULL", true},
		{"'it''s'", "it's", true},
		{"''", "", true},
		{"-1.50", "-1.50", true},
		{"42", "42", true},
		{"CURRENT_TIMESTAMP", "", false},
		{"(1 + 1)", "", false},
	}
	for _, c := range cases {
		v, ok := DefaultValue(c.in)
		if ok != c.ok {
			t.Errorf("%s: ok %v", c.in, ok)
			continue
		}
		if !ok {
			continue
		}
		got := "NULL"
		if v != nil {
			got = *v
		}
		if got != c.want {
			t.Errorf("%s: want %q, got %q", c.in, c.want, got)
		}
	}
}

func TestColumnKey(t *testing.T) {
	tbl := &meta.Table{
		PrimaryKey: []string{"a", "b"},
		Indexes: []meta.Index{
			{Name: "u", Unique: true, Columns: []string{"c"}},
			{Name: "uc", Unique: true, Columns: []string{"d", "c"}},
			{Name: "k", Columns: []string{"e"}},
		},
	}
	for col, want := range map[string]string{"A": "PRI", "b": "PRI", "c": "UNI", "d": "MUL", "e": "MUL", "f": ""} {
		if got := ColumnKey(tbl, col); got != want {
			t.Errorf("%s: want %q, got %q", col, want, got)
		}
	}
}

func TestCreateTableSQLQuoting(t *testing.T) {
	def := "'a\\b''c'"
	tbl := &meta.Table{
		Name: "we`ird",
		Columns: []meta.Column{
			{Name: "x`y", DeclType: "varchar(5)", Default: &def, Options: map[string]string{meta.OptionCollation: "utf8mb4_general_ci"}},
		},
		Options: map[string]string{SidecarKeyComment: "it's"},
	}
	got := CreateTableSQL(tbl, 0)
	for _, want := range []string{"CREATE TABLE `we``ird`", "`x``y` varchar(5) COLLATE utf8mb4_general_ci DEFAULT 'a\\\\b''c'", "ENGINE=SQLite", "COMMENT='it''s'"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %s in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "AUTO_INCREMENT") {
		t.Errorf("no AUTO_INCREMENT expected:\n%s", got)
	}
}

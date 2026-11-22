package meta

import (
	"testing"
)

func TestParseType(t *testing.T) {
	cases := []struct {
		decl     string
		typ      uint8
		length   uint32
		decimals uint8
		charset  uint16
		flags    uint16
	}{
		{"TINYINT", TypeTiny, 4, 0, CharsetBinary, FlagNum},
		{"tinyint(1)", TypeTiny, 1, 0, CharsetBinary, FlagNum},
		{"BOOLEAN", TypeTiny, 1, 0, CharsetBinary, FlagNum},
		{"TINYINT UNSIGNED", TypeTiny, 3, 0, CharsetBinary, FlagNum | FlagUnsigned},
		{"SMALLINT", TypeShort, 6, 0, CharsetBinary, FlagNum},
		{"MEDIUMINT UNSIGNED", TypeInt24, 8, 0, CharsetBinary, FlagNum | FlagUnsigned},
		{"INT", TypeLong, 11, 0, CharsetBinary, FlagNum},
		{"INTEGER", TypeLong, 11, 0, CharsetBinary, FlagNum},
		{"INT ZEROFILL", TypeLong, 10, 0, CharsetBinary, FlagNum | FlagUnsigned | FlagZerofill},
		{"INT(5)", TypeLong, 5, 0, CharsetBinary, FlagNum},
		{"BIGINT", TypeLongLong, 20, 0, CharsetBinary, FlagNum},
		{"BIGINT UNSIGNED", TypeLongLong, 20, 0, CharsetBinary, FlagNum | FlagUnsigned},
		{"FLOAT", TypeFloat, 12, 31, CharsetBinary, FlagNum},
		{"FLOAT(30)", TypeDouble, 22, 31, CharsetBinary, FlagNum},
		{"DOUBLE", TypeDouble, 22, 31, CharsetBinary, FlagNum},
		{"REAL", TypeDouble, 22, 31, CharsetBinary, FlagNum},
		{"DOUBLE(8,3)", TypeDouble, 8, 3, CharsetBinary, FlagNum},
		{"DECIMAL", TypeNewDecimal, 11, 0, CharsetBinary, FlagNum},
		{"DECIMAL(10,2)", TypeNewDecimal, 12, 2, CharsetBinary, FlagNum},
		{"NUMERIC(5)", TypeNewDecimal, 6, 0, CharsetBinary, FlagNum},
		{"DECIMAL UNSIGNED", TypeNewDecimal, 10, 0, CharsetBinary, FlagNum | FlagUnsigned},
		{"BIT", TypeBit, 1, 0, CharsetBinary, FlagUnsigned},
		{"BIT(8)", TypeBit, 8, 0, CharsetBinary, FlagUnsigned},
		{"CHAR", TypeString, 4, 0, CollationDefault, 0},
		{"CHAR(10)", TypeString, 40, 0, CollationDefault, 0},
		{"VARCHAR(20)", TypeVarString, 80, 0, CollationDefault, 0},
		{"NVARCHAR(20)", TypeVarString, 60, 0, CollationUTF8MB3, 0},
		{"BINARY(4)", TypeString, 4, 0, CharsetBinary, FlagBinary},
		{"VARBINARY(16)", TypeVarString, 16, 0, CharsetBinary, FlagBinary},
		{"TINYTEXT", TypeBlob, 1020, 0, CollationDefault, FlagBlob},
		{"TEXT", TypeBlob, 262140, 0, CollationDefault, FlagBlob},
		{"TEXT(100)", TypeBlob, 1020, 0, CollationDefault, FlagBlob},
		{"MEDIUMTEXT", TypeBlob, 67108860, 0, CollationDefault, FlagBlob},
		{"LONGTEXT", TypeBlob, 4294967295, 0, CollationDefault, FlagBlob},
		{"TINYBLOB", TypeBlob, 255, 0, CharsetBinary, FlagBlob | FlagBinary},
		{"BLOB", TypeBlob, 65535, 0, CharsetBinary, FlagBlob | FlagBinary},
		{"MEDIUMBLOB", TypeBlob, 16777215, 0, CharsetBinary, FlagBlob | FlagBinary},
		{"LONGBLOB", TypeBlob, 4294967295, 0, CharsetBinary, FlagBlob | FlagBinary},
		{"DATE", TypeDate, 10, 0, CharsetBinary, FlagBinary},
		{"TIME", TypeTime, 10, 0, CharsetBinary, FlagBinary},
		{"TIME(3)", TypeTime, 14, 3, CharsetBinary, FlagBinary},
		{"DATETIME", TypeDatetime, 19, 0, CharsetBinary, FlagBinary},
		{"DATETIME(6)", TypeDatetime, 26, 6, CharsetBinary, FlagBinary},
		{"TIMESTAMP", TypeTimestamp, 19, 0, CharsetBinary, FlagBinary | FlagTimestamp},
		{"YEAR", TypeYear, 4, 0, CharsetBinary, FlagUnsigned | FlagZerofill | FlagNum},
		{"JSON", TypeJSON, 4294967295, 0, CharsetBinary, FlagBlob | FlagBinary},
	}
	for _, tc := range cases {
		got, ok := ParseType(tc.decl)
		if !ok {
			t.Errorf("%s: not recognized", tc.decl)
			continue
		}
		if got.Type != tc.typ || got.Length != tc.length || got.Decimals != tc.decimals || got.Charset != tc.charset || got.Flags != tc.flags {
			t.Errorf("%s: want type=%d len=%d dec=%d cs=%d flags=%d, got type=%d len=%d dec=%d cs=%d flags=%d",
				tc.decl, tc.typ, tc.length, tc.decimals, tc.charset, tc.flags,
				got.Type, got.Length, got.Decimals, got.Charset, got.Flags)
		}
	}
	for _, bad := range []string{"", "FOO", "VARCHAR", "INT(x)", "DECIMAL(10,2,1)", "ENUM", "INT SIGNEDISH", "DECIMAL(70,2)", "DECIMAL(10,40)", "DATETIME(7)"} {
		if _, ok := ParseType(bad); ok {
			t.Errorf("%q: want not recognized", bad)
		}
	}
}

func shop() *Table {
	return &Table{
		Database: "shop",
		Name:     "items",
		Columns: []Column{
			{Name: "id", DeclType: "INTEGER", NotNull: true},
			{Name: "name", DeclType: "VARCHAR(20)", NotNull: true},
			{Name: "price", DeclType: "DECIMAL(10,2)", NotNull: true, HasDefault: true},
			{Name: "code", DeclType: "VARCHAR(10)"},
			{Name: "a", DeclType: "INT"},
			{Name: "b", DeclType: "INT"},
			{Name: "odd", DeclType: "WHATEVER"},
		},
		PrimaryKey:    []string{"id"},
		AutoIncrement: "id",
		Indexes: []Index{
			{Columns: []string{"code"}, Unique: true},
			{Columns: []string{"a", "b"}, Unique: true},
			{Columns: []string{"b"}},
		},
	}
}

func TestStarExpandsTableColumns(t *testing.T) {
	defs, err := Columns([]Item{{Star: true}}, shop(), "")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name  string
		typ   uint8
		flags uint16
	}{
		{"id", TypeLong, FlagNotNull | FlagPriKey | FlagAutoIncrement | FlagPartKey | FlagNum},
		{"name", TypeVarString, FlagNotNull | FlagNoDefault},
		{"price", TypeNewDecimal, FlagNotNull | FlagNum},
		{"code", TypeVarString, FlagUniqueKey | FlagPartKey},
		{"a", TypeLong, FlagMultipleKey | FlagPartKey | FlagNum},
		{"b", TypeLong, FlagMultipleKey | FlagPartKey | FlagNum},
		{"odd", TypeVarString, 0},
	}
	if len(defs) != len(want) {
		t.Fatalf("want %d columns, got %d", len(want), len(defs))
	}
	for i, w := range want {
		d := defs[i]
		if d.Name != w.name || d.OrgName != w.name || d.Type != w.typ || d.Flags != w.flags {
			t.Errorf("column %d: want %s type=%d flags=%d, got %s/%s type=%d flags=%d", i, w.name, w.typ, w.flags, d.Name, d.OrgName, d.Type, d.Flags)
		}
		if d.Schema != "shop" || d.Table != "items" || d.OrgTable != "items" {
			t.Errorf("column %d: want shop.items, got %s.%s (%s)", i, d.Schema, d.Table, d.OrgTable)
		}
	}
	if !defs[6].Degraded || defs[0].Degraded {
		t.Error("only the column with an unknown declared type is degraded")
	}
	if defs[6].Charset != CollationDefault || defs[6].Length != 262140 {
		t.Errorf("a degraded column is reported as text, got cs=%d len=%d", defs[6].Charset, defs[6].Length)
	}
}

func TestNamedColumnsAliasesAndQualifiers(t *testing.T) {
	items := []Item{
		{Column: "NAME", Name: "NAME"},
		{Qualifier: "i", Column: "price", Name: "cost"},
		{Star: true, Qualifier: "i"},
	}
	defs, err := Columns(items, shop(), "i")
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 9 {
		t.Fatalf("want 2 + 7 columns, got %d", len(defs))
	}
	if d := defs[0]; d.Name != "NAME" || d.OrgName != "name" || d.Table != "i" || d.OrgTable != "items" {
		t.Errorf("name as written, org_name as stored, table alias: got %+v", d)
	}
	if d := defs[1]; d.Name != "cost" || d.OrgName != "price" || d.Decimals != 2 || d.Length != 12 {
		t.Errorf("alias keeps the column type: got %+v", d)
	}
}

func TestColumnErrors(t *testing.T) {
	cases := []struct {
		items []Item
		alias string
		code  uint16
		msg   string
	}{
		{[]Item{{Column: "nope", Name: "nope"}}, "", 1054, "Unknown column 'nope' in 'field list'"},
		{[]Item{{Qualifier: "x", Column: "id", Name: "id"}}, "", 1054, "Unknown column 'x.id' in 'field list'"},
		// With an alias, the table name no longer qualifies columns (MySQL rule).
		{[]Item{{Qualifier: "items", Column: "id", Name: "id"}}, "i", 1054, "Unknown column 'items.id' in 'field list'"},
		{[]Item{{Star: true, Qualifier: "x"}}, "", 1051, "Unknown table 'x'"},
	}
	for _, tc := range cases {
		_, err := Columns(tc.items, shop(), tc.alias)
		if err == nil || err.Code != tc.code || err.Message != tc.msg {
			t.Errorf("want %d %q, got %v", tc.code, tc.msg, err)
		}
	}
	if _, err := Columns([]Item{{Column: "id", Name: "id"}}, nil, ""); err == nil || err.Code != 1054 {
		t.Errorf("a column without FROM is unknown, got %v", err)
	}
}

func TestLiteralColumns(t *testing.T) {
	cases := []struct {
		lit      Literal
		name     string
		typ      uint8
		length   uint32
		decimals uint8
		charset  uint16
		flags    uint16
	}{
		{Literal{Kind: LiteralInt, Text: "1"}, "1", TypeLongLong, 1, 0, CharsetBinary, FlagNotNull | FlagBinary | FlagNum},
		{Literal{Kind: LiteralInt, Text: "-12"}, "-12", TypeLongLong, 3, 0, CharsetBinary, FlagNotNull | FlagBinary | FlagNum},
		{Literal{Kind: LiteralDecimal, Text: "2.50"}, "2.50", TypeNewDecimal, 5, 2, CharsetBinary, FlagNotNull | FlagBinary | FlagNum},
		{Literal{Kind: LiteralDecimal, Text: "-0.5"}, "-0.5", TypeNewDecimal, 3, 1, CharsetBinary, FlagNotNull | FlagBinary | FlagNum},
		{Literal{Kind: LiteralFloat, Text: "1e3"}, "1e3", TypeDouble, 3, 31, CharsetBinary, FlagNotNull | FlagBinary | FlagNum},
		{Literal{Kind: LiteralString, Text: "città"}, "città", TypeVarString, 20, 0, CollationDefault, FlagNotNull},
		{Literal{Kind: LiteralNull, Text: "NULL"}, "NULL", TypeNull, 0, 0, CharsetBinary, FlagBinary},
	}
	for _, tc := range cases {
		lit := tc.lit
		defs, err := Columns([]Item{{Literal: &lit, Name: tc.name}}, nil, "")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		d := defs[0]
		if d.Name != tc.name || d.Type != tc.typ || d.Length != tc.length || d.Decimals != tc.decimals || d.Charset != tc.charset || d.Flags != tc.flags {
			t.Errorf("%s: want type=%d len=%d dec=%d cs=%d flags=%d, got %+v", tc.name, tc.typ, tc.length, tc.decimals, tc.charset, tc.flags, d)
		}
		if d.Schema != "" || d.Table != "" || d.OrgTable != "" || d.OrgName != "" {
			t.Errorf("%s: a literal has no table or original name, got %+v", tc.name, d)
		}
	}
}

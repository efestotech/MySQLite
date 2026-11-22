package catalog

import (
	"context"
	"testing"

	"mysqlite/internal/dialect"
	"mysqlite/internal/meta"
)

// The dialect describes the select list, the catalog the table, meta the columns:
// the three compose into column definitions for a real table (T5, criterion 1).
func TestSelectColumnDefinitions(t *testing.T) {
	w := newConn(t, "CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(20) NOT NULL, price DECIMAL(10,2))")
	st, e := dialect.New(dialect.Config{ServerVersion: 80036}).Translate("SELECT i.*, name AS n, 1 FROM shop.items AS i", "")
	if e != nil {
		t.Fatal(e)
	}
	table, err := ReadTable(context.Background(), w, st.Database, st.Shape.Table)
	if err != nil {
		t.Fatal(err)
	}
	defs, me := meta.Columns(st.Shape.Items, meta.Single(table, st.Shape.Alias))
	if me != nil {
		t.Fatal(me)
	}
	want := []struct {
		name, orgName, table string
		typ                  uint8
	}{
		{"id", "id", "i", meta.TypeLong},
		{"name", "name", "i", meta.TypeVarString},
		{"price", "price", "i", meta.TypeNewDecimal},
		{"n", "name", "i", meta.TypeVarString},
		{"1", "", "", meta.TypeLongLong},
	}
	if len(defs) != len(want) {
		t.Fatalf("want %d columns, got %d: %+v", len(want), len(defs), defs)
	}
	for i, w := range want {
		d := defs[i]
		if d.Name != w.name || d.OrgName != w.orgName || d.Table != w.table || d.Type != w.typ {
			t.Errorf("column %d: want %+v, got %+v", i, w, d)
		}
	}
	if defs[0].Schema != "shop" || defs[0].OrgTable != "items" {
		t.Errorf("schema and org_table come from the catalog: %+v", defs[0])
	}
}

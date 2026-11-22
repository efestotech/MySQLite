package dialect

import (
	"testing"

	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// fakeCatalog records the lookups the emitter makes (docs/01-architecture.md section 3:
// the translator's input includes the catalog).
type fakeCatalog struct {
	tables map[string]*meta.Table
	asked  []string
}

func (c *fakeCatalog) Table(database, table string) (*meta.Table, *errs.Error) {
	c.asked = append(c.asked, database+"."+table)
	t, ok := c.tables[table]
	if !ok {
		return nil, errs.NoSuchTable(database, table)
	}
	return t, nil
}

func TestTranslateReadsTheCatalog(t *testing.T) {
	items := &meta.Table{Database: "shop", Name: "items", Columns: []meta.Column{{Name: "id", DeclType: "int"}}}
	cat := &fakeCatalog{tables: map[string]*meta.Table{"items": items}}
	tr := newTranslator(t)

	cases := []struct {
		query string
		meta  func(*Statement) *meta.Table
	}{
		{"SELECT id FROM items", func(s *Statement) *meta.Table { return s.Shape.Meta }},
		{"SELECT id FROM other.items", func(s *Statement) *meta.Table { return s.Shape.Meta }},
		{"UPDATE items SET id = 2", func(s *Statement) *meta.Table { return s.Write.Meta }},
		{"DELETE FROM items WHERE id = 1", func(s *Statement) *meta.Table { return s.Write.Meta }},
		{"INSERT INTO items (id) VALUES (1)", func(s *Statement) *meta.Table { return s.Write.Meta }},
	}
	for _, c := range cases {
		cat.asked = nil
		st, e := tr.TranslateIn(c.query, "shop", cat)
		if e != nil {
			t.Fatalf("%s: %v", c.query, e)
		}
		if c.meta(st) != items {
			t.Errorf("%s: table metadata not attached", c.query)
		}
		want := "shop.items"
		if st.Database == "other" {
			want = "other.items"
		}
		if len(cat.asked) != 1 || cat.asked[0] != want {
			t.Errorf("%s: lookups %v, want [%s]", c.query, cat.asked, want)
		}
	}

	// No table, no lookup.
	cat.asked = nil
	if _, e := tr.TranslateIn("SELECT 1", "shop", cat); e != nil || len(cat.asked) != 0 {
		t.Errorf("SELECT 1: %v, lookups %v", e, cat.asked)
	}
	// The catalog's error is the statement's error.
	_, e := tr.TranslateIn("SELECT * FROM nosuch", "shop", cat)
	if e == nil || e.Code != 1146 {
		t.Fatalf("unknown table: %v", e)
	}
	// Without a catalog, translation stays catalog-free.
	st, e := tr.Translate("SELECT id FROM nosuch", "shop")
	if e != nil || st.Shape.Meta != nil {
		t.Errorf("catalog-free: %v %v", e, st)
	}
}

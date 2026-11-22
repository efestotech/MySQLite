package catalog

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"mysqlite/internal/dialect/udf"
	"mysqlite/internal/engine"
	"mysqlite/internal/meta"
)

func newConn(t *testing.T, ddl ...string) *sql.Conn {
	t.Helper()
	e, err := engine.Open(engine.Options{
		DataDir:      filepath.Join(t.TempDir(), "data"),
		MaxReadConns: 1,
		BusyTimeout:  50 * time.Millisecond,
		Synchronous:  "NORMAL",
	}, udf.Extensions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.CreateDatabase("shop"); err != nil {
		t.Fatal(err)
	}
	db, err := e.Database("shop")
	if err != nil {
		t.Fatal(err)
	}
	w, err := db.Writer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	for _, q := range ddl {
		if _, err := w.ExecContext(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return w
}

func TestReadTable(t *testing.T) {
	w := newConn(t,
		"CREATE TABLE Items (id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(20) NOT NULL, price DECIMAL(10,2) NOT NULL DEFAULT 0, code VARCHAR(10) UNIQUE, a INT, b INT, note TEXT, total INT GENERATED ALWAYS AS (a + b) VIRTUAL)",
		"CREATE UNIQUE INDEX items_ab ON Items (a, b)",
		"CREATE INDEX items_b ON Items (b)",
	)
	// The lookup is case-insensitive, as SQLite resolves names; the stored name is kept.
	got, err := ReadTable(context.Background(), w, "shop", "items")
	if err != nil {
		t.Fatal(err)
	}
	want := &meta.Table{
		Database: "shop",
		Name:     "Items",
		Columns: []meta.Column{
			{Name: "id", DeclType: "INTEGER"},
			{Name: "name", DeclType: "VARCHAR(20)", NotNull: true},
			{Name: "price", DeclType: "DECIMAL(10,2)", NotNull: true, HasDefault: true, Default: strPtr("0")},
			{Name: "code", DeclType: "VARCHAR(10)"},
			{Name: "a", DeclType: "INT"},
			{Name: "b", DeclType: "INT"},
			{Name: "note", DeclType: "TEXT"},
			{Name: "total", DeclType: "INT"},
		},
		PrimaryKey:    []string{"id"},
		AutoIncrement: "id",
		Indexes: []meta.Index{
			{Name: "code", Columns: []string{"code"}, Unique: true, SQLiteName: "sqlite_autoindex_Items_1"},
			{Name: "items_ab", Columns: []string{"a", "b"}, Unique: true, SQLiteName: "items_ab"},
			{Name: "items_b", Columns: []string{"b"}, SQLiteName: "items_b"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want\n%+v\ngot\n%+v", want, got)
	}
}

func TestReadTableCompositeKeyWithoutAutoIncrement(t *testing.T) {
	w := newConn(t, "CREATE TABLE m (x INT NOT NULL, y INT NOT NULL, note TEXT DEFAULT 'autoincrement', PRIMARY KEY (y, x))")
	got, err := ReadTable(context.Background(), w, "shop", "m")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.PrimaryKey, []string{"y", "x"}) {
		t.Errorf("primary key in key order: got %v", got.PrimaryKey)
	}
	if got.AutoIncrement != "" {
		t.Errorf("the word in a default is not AUTOINCREMENT: got %q", got.AutoIncrement)
	}
	if len(got.Indexes) != 0 {
		t.Errorf("the primary-key index is not reported as another index: got %v", got.Indexes)
	}
}

// An unnamed UNIQUE constraint is named after its first column, with a numeric suffix
// when that name is taken, as MySQL names it.
func TestReadTableUnnamedUniqueKeys(t *testing.T) {
	w := newConn(t,
		"CREATE TABLE u (a INT UNIQUE, b INT, UNIQUE (a, b), UNIQUE (b))",
		"CREATE UNIQUE INDEX b_2 ON u (b, a)",
	)
	got, err := ReadTable(context.Background(), w, "shop", "u")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ix := range got.Indexes {
		names = append(names, ix.Name)
	}
	if want := []string{"a", "a_2", "b", "b_2"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("want %v, got %v", want, names)
	}
}

func TestReadMissingTable(t *testing.T) {
	w := newConn(t)
	_, err := ReadTable(context.Background(), w, "shop", "missing")
	if err == nil || err.Error() != "ERROR 1146 (42S02): Table 'shop.missing' doesn't exist" {
		t.Fatalf("want 1146, got %v", err)
	}
}

func strPtr(s string) *string { return &s }

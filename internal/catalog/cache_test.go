package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"mysqlite/internal/dialect/udf"
	"mysqlite/internal/engine"
	"mysqlite/internal/errs"
)

func openShop(t *testing.T) *engine.Database {
	t.Helper()
	e, err := engine.Open(engine.Options{
		DataDir: filepath.Join(t.TempDir(), "data"), MaxReadConns: 2,
		BusyTimeout: time.Second, Synchronous: "NORMAL",
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
	return db
}

func exec(t *testing.T, db *engine.Database, stmts ...string) {
	t.Helper()
	ctx := context.Background()
	w, err := db.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, s := range stmts {
		if _, err := w.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func TestCacheInvalidatedByDDLFromAnotherConnection(t *testing.T) {
	ctx := context.Background()
	db := openShop(t)
	exec(t, db, "CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)")
	cache := NewCache()

	r, err := db.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	first, err := cache.Table(ctx, r, "shop", "items")
	if err != nil {
		t.Fatal(err)
	}
	again, err := cache.Table(ctx, r, "shop", "ITEMS")
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Error("second read without DDL did not come from the cache")
	}

	// DDL on the writer, a different connection.
	exec(t, db, "ALTER TABLE items ADD COLUMN price REAL")
	after, err := cache.Table(ctx, r, "shop", "items")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Columns) != 3 {
		t.Errorf("after ALTER TABLE: want 3 columns, got %d", len(after.Columns))
	}

	exec(t, db, "DROP TABLE items")
	_, err = cache.Table(ctx, r, "shop", "items")
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != 1146 {
		t.Errorf("after DROP TABLE: want 1146, got %v", err)
	}
}

func TestSidecar(t *testing.T) {
	ctx := context.Background()
	db := openShop(t)
	w, err := db.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, s := range []string{
		"BEGIN",
		"CREATE TABLE items (id INTEGER PRIMARY KEY, kind TEXT COLLATE mysql_ci, code TEXT)",
		"CREATE INDEX items__by_kind ON items (kind)",
		"CREATE UNIQUE INDEX items__code ON items (code)",
	} {
		if _, err := w.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	err = WriteSidecar(ctx, w, "items", []SidecarFact{
		{Key: SidecarKeyEngine, Value: "InnoDB"},
		{Key: SidecarKeyComment, Value: "stock"},
		{Column: "kind", Key: SidecarKeyType, Value: "enum('a','b')"},
		{Column: SidecarIndex("items__by_kind"), Key: SidecarKeyName, Value: "by_kind"},
		{Column: SidecarIndex("items__code"), Key: SidecarKeyName, Value: "code"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}

	tb, err := ReadTable(ctx, w, "shop", "items")
	if err != nil {
		t.Fatal(err)
	}
	if tb.Options[SidecarKeyEngine] != "InnoDB" || tb.Options[SidecarKeyComment] != "stock" {
		t.Errorf("table options: %v", tb.Options)
	}
	if got := tb.Columns[1].Options[SidecarKeyType]; got != "enum('a','b')" {
		t.Errorf("column type: %q", got)
	}
	if tb.Columns[2].Options != nil {
		t.Errorf("column without facts: %v", tb.Columns[2].Options)
	}
	if len(tb.Indexes) != 2 || tb.Indexes[0].Name != "by_kind" || tb.Indexes[1].Name != "code" || tb.Indexes[0].SQLiteName != "items__by_kind" {
		t.Errorf("index names: %+v", tb.Indexes)
	}

	// mysqlite's own tables are not tables to clients.
	_, err = ReadTable(ctx, w, "shop", "__MYSQLITE_meta")
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != 1146 {
		t.Errorf("sidecar: want 1146, got %v", err)
	}

	// Dropping a table's facts leaves the others.
	if err := DropSidecar(ctx, w, "other"); err != nil {
		t.Fatal(err)
	}
	if tb, _ := ReadTable(ctx, w, "shop", "items"); tb.Options == nil {
		t.Error("DropSidecar removed another table's facts")
	}
}

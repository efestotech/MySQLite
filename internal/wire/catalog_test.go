package wire

import (
	"testing"
)

// The catalog cache follows DDL from other connections, and the sidecar is hidden
// (docs/04-session-catalog.md section 3).
func TestCatalogCacheAndSidecar(t *testing.T) {
	e, addr := shopSetup(t)
	conn := dial(t, addr)
	if n := len(execOK(t, conn, "SELECT * FROM items").Fields); n != 5 {
		t.Fatalf("want 5 columns, got %d", n)
	}
	seed(t, e, "shop",
		"ALTER TABLE items ADD COLUMN stock INTEGER",
		"CREATE TABLE IF NOT EXISTS __mysqlite_meta (tbl TEXT, col TEXT, key TEXT, value TEXT)",
	)
	if n := len(execOK(t, conn, "SELECT * FROM items").Fields); n != 6 {
		t.Errorf("after ALTER TABLE on another connection: want 6 columns, got %d", n)
	}
	for _, q := range []string{"SELECT * FROM __mysqlite_meta", "DELETE FROM __mysqlite_meta"} {
		_, err := conn.Execute(q)
		requireMyError(t, err, 1146, "42S02")
	}
}

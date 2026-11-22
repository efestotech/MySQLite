package collate

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mysqlite/internal/engine"
)

// Every pooled connection has the collations (docs/06-transactions.md section 2), and a
// column declared with mysql_ci compares and indexes with it.
func TestCollationsOnEveryConnection(t *testing.T) {
	ctx := context.Background()
	const readers = 3
	e, err := engine.Open(engine.Options{
		DataDir: filepath.Join(t.TempDir(), "data"), MaxReadConns: readers,
		BusyTimeout: time.Second, Synchronous: "NORMAL",
	}, engine.Extensions{Collations: Collations()})
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
	w, err := db.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"CREATE TABLE city (name TEXT COLLATE mysql_ci NOT NULL, code TEXT COLLATE mysql_bin)",
		"CREATE UNIQUE INDEX city_name ON city (name)",
		"INSERT INTO city VALUES ('città', 'a')",
	} {
		if _, err := w.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	// PAD SPACE and case/accent folding reach the unique index.
	if _, err := w.ExecContext(ctx, "INSERT INTO city VALUES ('CITTA ', 'b')"); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("duplicate under mysql_ci: got %v", err)
	}
	var plan string
	row := w.QueryRowContext(ctx, "EXPLAIN QUERY PLAN SELECT name FROM city WHERE name = 'CITTA'")
	var a, b, c any
	if err := row.Scan(&a, &b, &c, &plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "city_name") {
		t.Errorf("equality on a mysql_ci column does not use its index: %s", plan)
	}
	_ = w.Close()

	// Hold readers+2 connections at once: each must know both collations.
	var wg sync.WaitGroup
	errc := make(chan error, readers+2)
	start := make(chan struct{})
	for i := 0; i < readers+2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, err := db.Read(ctx)
			if err != nil {
				errc <- err
				return
			}
			defer c.Close()
			var n int
			err = c.QueryRowContext(ctx, "SELECT count(*) FROM city WHERE name = 'Citta' AND code = 'a  ' COLLATE mysql_bin").Scan(&n)
			if err == nil && n != 1 {
				err = fmt.Errorf("want 1 row, got %d", n)
			}
			if err != nil {
				errc <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
}

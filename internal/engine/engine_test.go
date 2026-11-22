package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testReadConns = 4

func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		DataDir:      filepath.Join(t.TempDir(), "data"),
		MaxReadConns: testReadConns,
		BusyTimeout:  1234 * time.Millisecond,
		Synchronous:  "NORMAL",
	}
}

var testExtensions = Extensions{
	Functions: []ScalarFunction{{
		Name:          "t1_udf",
		NumArgs:       1,
		Deterministic: true,
		Fn: func(args []any) (any, error) {
			return fmt.Sprintf("udf:%v", args[0]), nil
		},
	}},
	Collations: []Collation{{
		Name:    "t1_ci",
		Compare: func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) },
	}},
}

func openEngine(t *testing.T, opts Options) *Engine {
	t.Helper()
	e, err := Open(opts, testExtensions)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return e
}

func openTestDB(t *testing.T) (*Engine, *Database) {
	t.Helper()
	e := openEngine(t, testOptions(t))
	if err := e.CreateDatabase("shop"); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	db, err := e.Database("shop")
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	return e, db
}

type connState struct {
	udf, journal string
	collEq, fk   int
	busy, sync   int
	queryOnly    int
}

func readState(t *testing.T, ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) connState {
	t.Helper()
	var s connState
	checks := []struct {
		sql  string
		dest any
	}{
		{"SELECT t1_udf('x')", &s.udf},
		{"SELECT 'ABC' = 'abc' COLLATE t1_ci", &s.collEq},
		{"PRAGMA journal_mode", &s.journal},
		{"PRAGMA foreign_keys", &s.fk},
		{"PRAGMA busy_timeout", &s.busy},
		{"PRAGMA synchronous", &s.sync},
		{"PRAGMA query_only", &s.queryOnly},
	}
	for _, c := range checks {
		if err := q.QueryRowContext(ctx, c.sql).Scan(c.dest); err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
	}
	return s
}

// Criterion 5 of M1 (docs/08-milestones.md) and docs/06-transactions.md section 2:
// every physical connection, in both pools, carries the PRAGMAs and the extensions.
func TestConnectionInitOnEveryPooledConnection(t *testing.T) {
	_, db := openTestDB(t)
	ctx := context.Background()
	// Force a new physical connection on every acquisition, so the test proves the hook
	// runs per connection and not once per pool.
	db.read.SetMaxIdleConns(0)
	before := db.opened.Load()

	held := make([]*sql.Conn, 0, testReadConns)
	for i := 0; i < testReadConns; i++ {
		c, err := db.Read(ctx)
		if err != nil {
			t.Fatalf("read conn %d: %v", i, err)
		}
		held = append(held, c)
	}
	for i := 0; i < 2; i++ { // max_read_conns + 2 physical read connections in total
		held[i].Close()
		c, err := db.Read(ctx)
		if err != nil {
			t.Fatalf("extra read conn %d: %v", i, err)
		}
		held[i] = c
	}
	for i, c := range held {
		s := readState(t, ctx, c)
		if s.udf != "udf:x" || s.collEq != 1 || s.journal != "wal" || s.fk != 1 ||
			s.busy != 1234 || s.sync != 1 || s.queryOnly != 1 {
			t.Fatalf("read conn %d not initialized: %+v", i, s)
		}
		c.Close()
	}

	w, err := db.Writer(ctx)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	s := readState(t, ctx, w)
	w.Close()
	if s.udf != "udf:x" || s.collEq != 1 || s.journal != "wal" || s.fk != 1 ||
		s.busy != 1234 || s.sync != 1 || s.queryOnly != 0 {
		t.Fatalf("writer not initialized: %+v", s)
	}

	// The writer was opened (and kept idle) by CreateDatabase, so only readers are new here.
	if got := db.opened.Load() - before; got < testReadConns+2 {
		t.Fatalf("want at least %d new physical read connections, got %d", testReadConns+2, got)
	}
}

func TestReadConnectionsCannotWrite(t *testing.T) {
	_, db := openTestDB(t)
	ctx := context.Background()
	c, err := db.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "CREATE TABLE x (id INTEGER)"); err == nil {
		t.Fatal("a read connection must reject writes")
	}
}

func TestSingleWriterIsSerialized(t *testing.T) {
	_, db := openTestDB(t)
	ctx := context.Background()
	w1, err := db.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := db.Writer(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second writer must wait while the first is held, got %v", err)
	}

	w1.Close()
	w2, err := db.Writer(ctx)
	if err != nil {
		t.Fatalf("writer after release: %v", err)
	}
	w2.Close()
}

func TestWriterPinsConnectionScopedState(t *testing.T) {
	_, db := openTestDB(t)
	ctx := context.Background()
	w, err := db.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ExecContext(ctx, "INSERT INTO t (v) VALUES ('a'), ('b')"); err != nil {
		t.Fatal(err)
	}
	var lastID, changes int64
	if err := w.QueryRowContext(ctx, "SELECT last_insert_rowid(), changes()").Scan(&lastID, &changes); err != nil {
		t.Fatal(err)
	}
	if lastID != 2 || changes != 2 {
		t.Fatalf("want last_insert_rowid 2 and changes 2 on the same connection, got %d, %d", lastID, changes)
	}
}

func TestCommittedWritesAreVisibleToReaders(t *testing.T) {
	_, db := openTestDB(t)
	ctx := context.Background()
	w, err := db.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"CREATE TABLE t (id INTEGER PRIMARY KEY)", "INSERT INTO t VALUES (7)"} {
		if _, err := w.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()

	r, err := db.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var id int
	if err := r.QueryRowContext(ctx, "SELECT id FROM t").Scan(&id); err != nil || id != 7 {
		t.Fatalf("reader must see committed row 7, got %d, %v", id, err)
	}
}

func TestDatabaseLifecycle(t *testing.T) {
	opts := testOptions(t)
	e := openEngine(t, opts)

	if _, err := e.Database("missing"); !errors.Is(err, ErrUnknownDatabase) {
		t.Fatalf("want ErrUnknownDatabase, got %v", err)
	}
	// Opening an unknown database must not create its file: the driver creates missing
	// files on open (docs/agent-notes/api-sqlite-driver.md).
	if _, err := os.Stat(filepath.Join(opts.DataDir, "missing"+fileExt)); !os.IsNotExist(err) {
		t.Fatalf("unknown database must not create a file, stat err = %v", err)
	}

	for _, name := range []string{"shop", "a_b$1"} {
		if err := e.CreateDatabase(name); err != nil {
			t.Fatalf("CreateDatabase(%q): %v", name, err)
		}
	}
	if err := e.CreateDatabase("shop"); !errors.Is(err, ErrDatabaseExists) {
		t.Fatalf("want ErrDatabaseExists, got %v", err)
	}

	names, err := e.Databases()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "a_b$1,shop" {
		t.Fatalf("want sorted [a_b$1 shop], got %v", names)
	}

	db1, err := e.Database("shop")
	if err != nil {
		t.Fatal(err)
	}
	db2, err := e.Database("shop")
	if err != nil {
		t.Fatal(err)
	}
	if db1 != db2 {
		t.Fatal("the same database must be shared, not reopened")
	}
}

func TestInvalidDatabaseNames(t *testing.T) {
	e := openEngine(t, testOptions(t))
	for _, name := range []string{"", "../x", "a/b", `a\b`, "a.b", "a b", strings.Repeat("x", 65)} {
		if err := e.CreateDatabase(name); !errors.Is(err, ErrInvalidDatabaseName) {
			t.Errorf("CreateDatabase(%q): want ErrInvalidDatabaseName, got %v", name, err)
		}
		if _, err := e.Database(name); !errors.Is(err, ErrInvalidDatabaseName) {
			t.Errorf("Database(%q): want ErrInvalidDatabaseName, got %v", name, err)
		}
	}
}

func TestOpenValidatesOptions(t *testing.T) {
	for name, mutate := range map[string]func(*Options){
		"empty data dir": func(o *Options) { o.DataDir = "" },
		"zero readers":   func(o *Options) { o.MaxReadConns = 0 },
		"negative busy":  func(o *Options) { o.BusyTimeout = -time.Second },
		"bad sync":       func(o *Options) { o.Synchronous = "OFF" },
	} {
		t.Run(name, func(t *testing.T) {
			opts := testOptions(t)
			mutate(&opts)
			if e, err := Open(opts, Extensions{}); err == nil {
				_ = e.Close()
				t.Fatal("want error")
			}
		})
	}
}

func TestSQLiteVersion(t *testing.T) {
	e := openEngine(t, testOptions(t))
	v, err := e.SQLiteVersion(context.Background())
	if err != nil || v == "" {
		t.Fatalf("want a version, got %q, %v", v, err)
	}
}

func TestScratchHasExtensionsAndCannotWrite(t *testing.T) {
	e := openEngine(t, testOptions(t))
	ctx := context.Background()
	c, err := e.Scratch(ctx)
	if err != nil {
		t.Fatalf("Scratch: %v", err)
	}
	defer c.Close()

	var got string
	if err := c.QueryRowContext(ctx, "SELECT t1_udf(1)").Scan(&got); err != nil || got != "udf:1" {
		t.Fatalf("want the function on scratch connections, got %q, %v", got, err)
	}
	if _, err := c.ExecContext(ctx, "CREATE TABLE x (a)"); err == nil {
		t.Fatal("a scratch connection must be query-only")
	}
	entries, err := os.ReadDir(e.opts.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("scratch must not create files, found %d", len(entries))
	}
}

func TestScratchAfterCloseFails(t *testing.T) {
	e, err := Open(testOptions(t), Extensions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Scratch(context.Background()); err == nil {
		t.Fatal("want an error after Close")
	}
}

// An ephemeral connection is a new, writable, empty database each time, with the
// extensions registered.
func TestEphemeralIsDiscardedOnClose(t *testing.T) {
	e := openEngine(t, testOptions(t))
	ctx := context.Background()
	for i := range 3 {
		c, err := e.Ephemeral(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.ExecContext(ctx, "CREATE TABLE x (a TEXT COLLATE t1_ci)"); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		var v string
		if err := c.QueryRowContext(ctx, "SELECT t1_udf(1)").Scan(&v); err != nil || v != "udf:1" {
			t.Fatalf("round %d: %q %v", i, v, err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

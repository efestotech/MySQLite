package engine

// Spike for docs/02-research.md section 2: what modernc.org/sqlite provides.
// Findings are recorded in docs/agent-notes/api-sqlite-driver.md. These tests pin
// the observed behaviour so a driver upgrade that changes it fails loudly.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"
)

var (
	registerOnce sync.Once
	hookCalls    atomic.Int64
)

// registerSpikeExtensions registers process-wide extensions once. In modernc.org/sqlite
// functions and collations are global and apply to connections opened afterwards.
func registerSpikeExtensions(t *testing.T) {
	t.Helper()
	registerOnce.Do(func() {
		if err := sqlite.RegisterScalarFunction("spike_udf", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			return fmt.Sprintf("udf:%v", args[0]), nil
		}); err != nil {
			t.Fatalf("register function: %v", err)
		}
		if err := sqlite.RegisterCollationUtf8("spike_ci", func(a, b string) int {
			return strings.Compare(strings.ToLower(a), strings.ToLower(b))
		}); err != nil {
			t.Fatalf("register collation: %v", err)
		}
		sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
			hookCalls.Add(1)
			for _, p := range []string{
				"PRAGMA journal_mode=WAL",
				"PRAGMA synchronous=NORMAL",
				"PRAGMA busy_timeout=5000",
				"PRAGMA foreign_keys=ON",
			} {
				if _, err := conn.ExecContext(context.Background(), p, nil); err != nil {
					return fmt.Errorf("%s: %w", p, err)
				}
			}
			return nil
		})
	})
}

func openSpikeDB(t *testing.T) *sql.DB {
	t.Helper()
	registerSpikeExtensions(t)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "spike.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSpikeSQLiteVersion(t *testing.T) {
	db := openSpikeDB(t)
	var v string
	if err := db.QueryRow("SELECT sqlite_version()").Scan(&v); err != nil {
		t.Fatal(err)
	}
	t.Logf("sqlite_version() = %s", v)
	if v != "3.54.1" {
		t.Fatalf("embedded SQLite changed: %s; update api-sqlite-driver.md", v)
	}
}

func TestSpikeHookRunsOnEveryPooledConnection(t *testing.T) {
	db := openSpikeDB(t)
	const n = 6 // max_read_conns (4) + 2, docs/06-transactions.md section 2
	ctx := context.Background()
	before := hookCalls.Load()

	conns := make([]*sql.Conn, n)
	for i := range conns {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
		conns[i] = c
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	if got := hookCalls.Load() - before; got != n {
		t.Fatalf("want hook called %d times, got %d", n, got)
	}

	for i, c := range conns {
		var udf, mode string
		var fk, eq int
		if err := c.QueryRowContext(ctx, "SELECT spike_udf(?)", i).Scan(&udf); err != nil {
			t.Fatalf("conn %d udf: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, "SELECT 'ABC' = 'abc' COLLATE spike_ci").Scan(&eq); err != nil {
			t.Fatalf("conn %d collation: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatalf("conn %d pragma: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatalf("conn %d journal_mode: %v", i, err)
		}
		if udf != fmt.Sprintf("udf:%d", i) || eq != 1 || fk != 1 || mode != "wal" {
			t.Fatalf("conn %d: udf=%q collation_eq=%d foreign_keys=%d journal_mode=%q", i, udf, eq, fk, mode)
		}
	}
}

func TestSpikeFeatureAvailability(t *testing.T) {
	db := openSpikeDB(t)
	mustExec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec("CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT)")
	mustExec("CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER)")
	mustExec("INSERT INTO a (v) VALUES ('x'), ('y'), ('z')")
	mustExec("INSERT INTO b (a_id) VALUES (1), (9)")

	cases := []struct {
		name  string
		query string
		want  bool
	}{
		{"DELETE ... LIMIT (SQLITE_ENABLE_UPDATE_DELETE_LIMIT)", "DELETE FROM b WHERE a_id = 9 LIMIT 1", false},
		{"UPDATE ... ORDER BY ... LIMIT", "UPDATE a SET v = v ORDER BY id LIMIT 1", false},
		{"RETURNING", "INSERT INTO a (v) VALUES ('r') RETURNING id", true},
		{"window functions", "SELECT row_number() OVER (ORDER BY id) FROM a", true},
		{"RIGHT JOIN", "SELECT a.id FROM a RIGHT JOIN b ON b.a_id = a.id", true},
		{"FULL JOIN", "SELECT a.id FROM a FULL JOIN b ON b.a_id = a.id", true},
		{"concat()", "SELECT concat('a', 1, NULL)", true},
		{"iif()", "SELECT iif(1, 'y', 'n')", true},
	}
	for _, tc := range cases {
		rows, err := db.Query(tc.query)
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
			_ = rows.Close()
		}
		got := err == nil
		t.Logf("%s: available=%v err=%v", tc.name, got, err)
		if got != tc.want {
			t.Errorf("%s: want available=%v, got %v (err=%v)", tc.name, tc.want, got, err)
		}
	}
}

func TestSpikeValueTypesThroughDatabaseSQL(t *testing.T) {
	db := openSpikeDB(t)
	row := db.QueryRow("SELECT NULL, x'00ff', 42, 1.5, 'text', 9223372036854775807")
	vals := make([]any, 6)
	ptrs := make([]any, 6)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := row.Scan(ptrs...); err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(vals))
	for i, v := range vals {
		got[i] = fmt.Sprintf("%T", v)
	}
	t.Logf("NULL, BLOB, INTEGER, REAL, TEXT, max int64 -> %v", got)
	want := []string{"<nil>", "[]uint8", "int64", "float64", "string", "int64"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("column %d: want %s, got %s", i, want[i], got[i])
		}
	}
}

// The driver looks at the declared column type: TEXT stored in a DATE/DATETIME/TIMESTAMP
// column may come back as time.Time instead of string. The engine must control this,
// because the wire layer formats dates itself (docs/03-protocol.md section 5).
func TestSpikeDeclaredTimeColumnsChangeScanType(t *testing.T) {
	db := openSpikeDB(t)
	if _, err := db.Exec("CREATE TABLE t (d DATE, dt DATETIME, ts TIMESTAMP, txt TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO t VALUES ('2024-01-02', '2024-01-02 03:04:05', '2024-01-02 03:04:05', '2024-01-02 03:04:05')"); err != nil {
		t.Fatal(err)
	}
	vals := make([]any, 4)
	if err := db.QueryRow("SELECT d, dt, ts, txt FROM t").Scan(&vals[0], &vals[1], &vals[2], &vals[3]); err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(vals))
	for i, v := range vals {
		got[i] = fmt.Sprintf("%T", v)
	}
	t.Logf("DATE, DATETIME, TIMESTAMP, TEXT -> %v (values %v)", got, vals)
	want := []string{"time.Time", "time.Time", "time.Time", "string"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("column %d: want %s, got %s", i, want[i], got[i])
		}
	}
}

// Error codes drive the MySQL error mapping (docs/09-conventions.md section 4).
func TestSpikeErrorCodes(t *testing.T) {
	db := openSpikeDB(t)
	for _, q := range []string{
		"CREATE TABLE p (id INTEGER PRIMARY KEY, u TEXT UNIQUE, n TEXT NOT NULL, c INTEGER CHECK (c > 0))",
		"CREATE TABLE ch (id INTEGER PRIMARY KEY, p_id INTEGER REFERENCES p(id))",
		"INSERT INTO p VALUES (1, 'a', 'x', 1)",
		"INSERT INTO ch VALUES (1, 1)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	cases := []struct {
		name  string
		query string
		code  int
	}{
		{"unique", "INSERT INTO p VALUES (2, 'a', 'x', 1)", 2067},      // SQLITE_CONSTRAINT_UNIQUE
		{"primary key", "INSERT INTO p VALUES (1, 'b', 'x', 1)", 1555}, // SQLITE_CONSTRAINT_PRIMARYKEY
		{"not null", "INSERT INTO p VALUES (3, 'c', NULL, 1)", 1299},   // SQLITE_CONSTRAINT_NOTNULL
		{"check", "INSERT INTO p VALUES (4, 'd', 'x', 0)", 275},        // SQLITE_CONSTRAINT_CHECK
		{"fk parent missing", "INSERT INTO ch VALUES (2, 99)", 787},    // SQLITE_CONSTRAINT_FOREIGNKEY
		{"fk child exists", "DELETE FROM p WHERE id = 1", 787},         // SQLITE_CONSTRAINT_FOREIGNKEY
		{"no such table", "SELECT * FROM missing", 1},                  // SQLITE_ERROR
		{"no such column", "SELECT nope FROM p", 1},                    // SQLITE_ERROR
	}
	for _, tc := range cases {
		_, err := db.Exec(tc.query)
		var se *sqlite.Error
		if !errors.As(err, &se) {
			t.Errorf("%s: want *sqlite.Error, got %T %v", tc.name, err, err)
			continue
		}
		t.Logf("%s: code=%d msg=%q", tc.name, se.Code(), se.Error())
		if se.Code() != tc.code {
			t.Errorf("%s: want code %d, got %d", tc.name, tc.code, se.Code())
		}
	}
}

// T4: this build has DQS enabled, so an unknown double-quoted identifier silently
// becomes a string literal. The dialect quotes identifiers with backticks, which fail
// as they should (docs/agent-notes/rule-order.md, emitter invariants).
func TestSpikeDoubleQuotedIdentifierFallsBackToString(t *testing.T) {
	db := openSpikeDB(t)
	if _, err := db.Exec("CREATE TABLE q (a INTEGER); INSERT INTO q VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	var v any
	if err := db.QueryRow(`SELECT "nope" FROM q`).Scan(&v); err != nil {
		t.Fatalf("DQS appears disabled (good news): update the dialect notes. Error: %v", err)
	}
	if v != "nope" {
		t.Fatalf("want the string fallback, got %#v", v)
	}
	err := db.QueryRow("SELECT `nope` FROM q").Scan(&v)
	if code, _, ok := ErrorCode(err); !ok || code != 1 {
		t.Fatalf("a backtick-quoted unknown column must fail with SQLITE_ERROR, got %v", err)
	}
}

// UPDATE counts MySQL's changed rows with NOT (col IS (new) COLLATE BINARY ...)
// (docs/agent-notes/rule-order.md). The comparison must apply the column's affinity to
// the new value, as storing it would, and must ignore the column's own collation:
// MySQL compares records byte for byte.
func TestSpikeChangePredicate(t *testing.T) {
	db := openSpikeDB(t)
	if _, err := db.Exec("CREATE TABLE c (i INTEGER, r REAL, s TEXT COLLATE NOCASE, d DECIMAL(10,2), n TEXT); INSERT INTO c VALUES (1, 1.5, 'abc', 2, NULL)"); err != nil {
		t.Fatal(err)
	}
	cases := map[string]int64{
		"i IS ('1') COLLATE BINARY":    1, // '1' stored in INTEGER is 1
		"i IS (1.0) COLLATE BINARY":    1,
		"r IS (1.5) COLLATE BINARY":    1,
		"s IS ('ABC') COLLATE BINARY":  0, // NOCASE column, binary comparison
		"s IS ('abc') COLLATE BINARY":  1,
		"s IS ('abc ') COLLATE BINARY": 0,
		"d IS ('2.00') COLLATE BINARY": 1, // NUMERIC affinity turns '2.00' into 2
		"n IS (NULL) COLLATE BINARY":   1,
		"n IS ('x') COLLATE BINARY":    0,
	}
	for expr, want := range cases {
		var got int64
		if err := db.QueryRow("SELECT " + expr + " FROM c").Scan(&got); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if got != want {
			t.Errorf("%s: want %d, got %d", expr, want, got)
		}
	}
}

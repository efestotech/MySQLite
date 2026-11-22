package errs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"mysqlite/internal/engine"
)

// newWriter returns a writer connection on a fresh database with a parent/child schema.
// The errors under test come from real SQLite, so a SQLite upgrade that rewords a
// message fails here instead of silently producing 1105.
func newWriter(t *testing.T) *sql.Conn {
	t.Helper()
	e, err := engine.Open(engine.Options{
		DataDir:      filepath.Join(t.TempDir(), "data"),
		MaxReadConns: 1,
		BusyTimeout:  50 * time.Millisecond,
		Synchronous:  "NORMAL",
	}, engine.Extensions{})
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
	for _, q := range []string{
		"CREATE TABLE p (id INTEGER PRIMARY KEY, u TEXT UNIQUE, n TEXT NOT NULL, c INTEGER CONSTRAINT p_chk_1 CHECK (c > 0), a INTEGER, b INTEGER, UNIQUE (a, b))",
		"CREATE TABLE ch (id INTEGER PRIMARY KEY, p_id INTEGER REFERENCES p(id))",
		"INSERT INTO p VALUES (1, 'x', 'n', 1, 1, 1)",
		"INSERT INTO ch VALUES (1, 1)",
	} {
		if _, err := w.ExecContext(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return w
}

func TestMapSQLiteErrors(t *testing.T) {
	w := newWriter(t)
	cases := []struct {
		name  string
		query string
		hint  Hint
		code  uint16
		state string
		msg   string
	}{
		{"unique column", "INSERT INTO p VALUES (2, 'x', 'n', 1, 2, 2)",
			Hint{Database: "shop", Op: OpInsert, DuplicateEntry: "x",
				KeyName: func(table string, cols []string) string { return "uq_" + cols[0] }},
			1062, "23000", "Duplicate entry 'x' for key 'p.uq_u'"},
		{"unique without key resolver", "INSERT INTO p VALUES (2, 'x', 'n', 1, 2, 2)",
			Hint{Database: "shop", Op: OpInsert, DuplicateEntry: "x"},
			1062, "23000", "Duplicate entry 'x' for key 'p.u'"},
		{"composite unique", "INSERT INTO p VALUES (2, 'y', 'n', 1, 1, 1)",
			Hint{Database: "shop", Op: OpInsert, DuplicateEntry: "1-1"},
			1062, "23000", "Duplicate entry '1-1' for key 'p.a_b'"},
		{"primary key", "INSERT INTO p VALUES (1, 'z', 'n', 1, 3, 3)",
			Hint{Database: "shop", Op: OpInsert, DuplicateEntry: "1"},
			1062, "23000", "Duplicate entry '1' for key 'p.PRIMARY'"},
		{"not null", "INSERT INTO p VALUES (3, 'w', NULL, 1, 4, 4)",
			Hint{Database: "shop", Op: OpInsert},
			1048, "23000", "Column 'n' cannot be null"},
		{"check", "INSERT INTO p VALUES (4, 'v', 'n', 0, 5, 5)",
			Hint{Database: "shop", Op: OpInsert},
			3819, "HY000", "Check constraint 'p_chk_1' is violated."},
		{"fk insert child", "INSERT INTO ch VALUES (2, 99)",
			Hint{Database: "shop", Op: OpInsert},
			1452, "23000", "Cannot add or update a child row: a foreign key constraint fails"},
		{"fk delete parent", "DELETE FROM p WHERE id = 1",
			Hint{Database: "shop", Op: OpDelete},
			1451, "23000", "Cannot delete or update a parent row: a foreign key constraint fails"},
		{"fk update parent with side", "UPDATE p SET id = 5 WHERE id = 1",
			Hint{Database: "shop", Op: OpUpdate, FK: FKParent, FKDetail: "`shop`.`ch`, CONSTRAINT `fk` FOREIGN KEY (`p_id`) REFERENCES `p` (`id`)"},
			1451, "23000", "Cannot delete or update a parent row: a foreign key constraint fails (`shop`.`ch`, CONSTRAINT `fk` FOREIGN KEY (`p_id`) REFERENCES `p` (`id`))"},
		{"no such table", "SELECT * FROM missing",
			Hint{Database: "shop"},
			1146, "42S02", "Table 'shop.missing' doesn't exist"},
		{"no such column default clause", "SELECT nope FROM p",
			Hint{Database: "shop"},
			1054, "42S22", "Unknown column 'nope' in 'field list'"},
		{"no such column where", "SELECT id FROM p WHERE nope = 1",
			Hint{Database: "shop", Clause: "where clause"},
			1054, "42S22", "Unknown column 'nope' in 'where clause'"},
		{"value count without column list", "INSERT INTO p VALUES (9)",
			Hint{Database: "shop", Op: OpInsert},
			1136, "21S01", "Column count doesn't match value count at row 1"},
		{"value count with column list", "INSERT INTO p (id, u) VALUES (9)",
			Hint{Database: "shop", Op: OpInsert},
			1136, "21S01", "Column count doesn't match value count at row 1"},
		{"other sqlite error", "SELECT * FROM",
			Hint{Database: "shop"},
			1105, "HY000", "Unknown error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := w.ExecContext(context.Background(), tc.query)
			if err == nil {
				t.Fatal("query must fail")
			}
			got := Map(err, tc.hint)
			if got.Code != tc.code || got.State != tc.state || got.Message != tc.msg {
				t.Fatalf("want %d/%s %q, got %d/%s %q (sqlite: %v)", tc.code, tc.state, tc.msg, got.Code, got.State, got.Message, err)
			}
		})
	}
}

func TestMapNonSQLiteErrors(t *testing.T) {
	own := UnknownDatabase("x")
	cases := []struct {
		name  string
		err   error
		code  uint16
		state string
	}{
		{"own error passes through", fmt.Errorf("wrapped: %w", own), 1049, "42000"},
		{"deadline waiting for the writer", fmt.Errorf("engine: %w", context.DeadlineExceeded), 1205, "HY000"},
		{"cancelled", context.Canceled, 1317, "70100"},
		{"anything else", errors.New("boom"), 1105, "HY000"},
	}
	for _, tc := range cases {
		got := Map(tc.err, Hint{})
		if got.Code != tc.code || got.State != tc.state {
			t.Errorf("%s: want %d/%s, got %d/%s", tc.name, tc.code, tc.state, got.Code, got.State)
		}
	}
	if Map(nil, Hint{}) != nil {
		t.Error("Map(nil) must be nil")
	}
	if Map(own, Hint{}) != own {
		t.Error("an *Error must be returned unchanged")
	}
}

func TestMapEngineErrors(t *testing.T) {
	cases := []struct {
		err  error
		arg  string
		code uint16
		msg  string
	}{
		{fmt.Errorf("%w: %q", engine.ErrUnknownDatabase, "nope"), "nope", 1049, "Unknown database 'nope'"},
		{fmt.Errorf("%w: %q", engine.ErrDatabaseExists, "shop"), "shop", 1007, "Can't create database 'shop'; database exists"},
		{fmt.Errorf("%w: %q", engine.ErrInvalidDatabaseName, "a/b"), "a/b", 1102, "Incorrect database name 'a/b'"},
	}
	for _, tc := range cases {
		// DatabaseArg is the name the caller passed to the engine.
		got := Map(tc.err, Hint{DatabaseArg: tc.arg})
		if got.Code != tc.code || got.Message != tc.msg {
			t.Errorf("want %d %q, got %d %q", tc.code, tc.msg, got.Code, got.Message)
		}
	}
}

func TestConstructors(t *testing.T) {
	cases := []struct {
		err   *Error
		code  uint16
		state string
		msg   string
	}{
		{UnknownDatabase("x"), 1049, "42000", "Unknown database 'x'"},
		{NoDatabaseSelected(), 1046, "3D000", "No database selected"},
		{Parse("FROM", 1), 1064, "42000", "You have an error in your SQL syntax; check the manual that corresponds to your MySQL server version for the right syntax to use near 'FROM' at line 1"},
		{Unsupported("LOCK TABLES", "single writer"), 1235, "42000", "mysqlite: unsupported construct LOCK TABLES: single writer"},
		{UnknownCommand(), 1047, "08S01", "Unknown command"},
		{AccessDenied("test", "127.0.0.1", true), 1045, "28000", "Access denied for user 'test'@'127.0.0.1' (using password: YES)"},
		{AccessDenied("test", "127.0.0.1", false), 1045, "28000", "Access denied for user 'test'@'127.0.0.1' (using password: NO)"},
		{CommandDenied("SUPER"), 1227, "42000", "Access denied; you need (at least one of) the SUPER privilege(s) for this operation"},
		{Internal(), 1105, "HY000", "Unknown error"},
		{NoSuchTable("shop", "t"), 1146, "42S02", "Table 'shop.t' doesn't exist"},
		{UnknownTable("x"), 1051, "42S02", "Unknown table 'x'"},
		{UnknownColumn("x.a", "field list"), 1054, "42S22", "Unknown column 'x.a' in 'field list'"},
		{EmptyQuery(), 1065, "42000", "Query was empty"},
		{ColumnCount(3), 1136, "21S01", "Column count doesn't match value count at row 3"},
	}
	for _, tc := range cases {
		if tc.err.Code != tc.code || tc.err.State != tc.state || tc.err.Message != tc.msg {
			t.Errorf("want %d/%s %q, got %d/%s %q", tc.code, tc.state, tc.msg, tc.err.Code, tc.err.State, tc.err.Message)
		}
	}
	if got := UnknownDatabase("x").Error(); got != "ERROR 1049 (42000): Unknown database 'x'" {
		t.Errorf("unexpected Error(): %q", got)
	}
}

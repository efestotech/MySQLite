package session

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"mysqlite/internal/engine"
)

func useShop(t *testing.T, dbs ...string) (*Session, *engine.Database) {
	t.Helper()
	e := newEngine(t, append([]string{"shop"}, dbs...)...)
	s := New(e)
	if err := s.Use("shop"); err != nil {
		t.Fatal(err)
	}
	_, db := s.Database()
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s, db
}

func exec(t *testing.T, c *sql.Conn, q string) {
	t.Helper()
	if _, err := c.ExecContext(context.Background(), q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// write runs statements on the writer of db outside the session.
func write(t *testing.T, db *engine.Database, stmts ...string) {
	t.Helper()
	w, err := db.Writer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, q := range stmts {
		exec(t, w, q)
	}
}

func count(t *testing.T, c *sql.Conn) int {
	t.Helper()
	var n int
	if err := c.QueryRowContext(context.Background(), "SELECT count(*) FROM items").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func readerCount(t *testing.T, db *engine.Database) int {
	t.Helper()
	r, err := db.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	return count(t, r)
}

// writerFree reports whether another caller can take the writer within a short wait.
func writerFree(t *testing.T, db *engine.Database) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	w, err := db.Writer(ctx)
	if err != nil {
		return false
	}
	_ = w.Close()
	return true
}

func conn(t *testing.T, s *Session, database string, write bool) *sql.Conn {
	t.Helper()
	c, release, err := s.Conn(context.Background(), database, write)
	if err != nil {
		t.Fatalf("Conn(%q, %v): %v", database, write, err)
	}
	t.Cleanup(release)
	return c
}

func TestAutocommitConnectionsAreReleased(t *testing.T) {
	s, db := useShop(t)
	ctx := context.Background()
	if s.InTransaction() || !s.Autocommit() {
		t.Fatal("a new session is in autocommit mode without a transaction")
	}
	c, release, err := s.Conn(ctx, "shop", true)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, c, "CREATE TABLE items (a)")
	release()
	if !writerFree(t, db) {
		t.Fatal("release must return the writer")
	}
	r, release, err := s.Conn(ctx, "shop", false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := r.ExecContext(ctx, "INSERT INTO items VALUES (1)"); err == nil {
		t.Fatal("a read connection outside a transaction is query-only")
	}
}

func TestCommitAndRollback(t *testing.T) {
	s, db := useShop(t)
	ctx := context.Background()
	write(t, db, "CREATE TABLE items (a)")

	if err := s.Begin(ctx, TxOptions{}); err != nil {
		t.Fatal(err)
	}
	if !s.InTransaction() || s.ReadOnlyTransaction() {
		t.Fatal("want a read-write transaction")
	}
	// The writer is taken by the first statement, not by BEGIN.
	if !writerFree(t, db) {
		t.Fatal("BEGIN alone must not take the writer")
	}
	// Reads inside a read-write transaction see its own writes: they use the writer.
	exec(t, conn(t, s, "shop", false), "INSERT INTO items VALUES (1)")
	if writerFree(t, db) {
		t.Fatal("the transaction holds the writer until it ends")
	}
	if got := count(t, conn(t, s, "shop", false)); got != 1 {
		t.Fatalf("the transaction sees its own row, got %d", got)
	}
	if got := readerCount(t, db); got != 0 {
		t.Fatalf("other sessions must not see uncommitted rows, got %d", got)
	}
	if err := s.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if s.InTransaction() || !writerFree(t, db) || readerCount(t, db) != 1 {
		t.Fatal("COMMIT ends the transaction, releases the writer and publishes the row")
	}

	if err := s.Begin(ctx, TxOptions{}); err != nil {
		t.Fatal(err)
	}
	exec(t, conn(t, s, "shop", true), "INSERT INTO items VALUES (2)")
	if err := s.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if s.InTransaction() || !writerFree(t, db) || readerCount(t, db) != 1 {
		t.Fatal("ROLLBACK ends the transaction, releases the writer and discards the row")
	}
	// Without a transaction COMMIT and ROLLBACK do nothing, as in MySQL.
	if err := s.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestBeginCommitsTheOpenTransaction(t *testing.T) {
	s, db := useShop(t)
	ctx := context.Background()
	write(t, db, "CREATE TABLE items (a)")

	if err := s.Begin(ctx, TxOptions{}); err != nil {
		t.Fatal(err)
	}
	exec(t, conn(t, s, "shop", true), "INSERT INTO items VALUES (1)")
	if err := s.Begin(ctx, TxOptions{}); err != nil {
		t.Fatal(err)
	}
	if !s.InTransaction() || readerCount(t, db) != 1 {
		t.Fatal("BEGIN inside a transaction commits it and starts a new one")
	}
}

func TestReadOnlyTransaction(t *testing.T) {
	s, db := useShop(t)
	ctx := context.Background()
	write(t, db, "CREATE TABLE items (a)", "INSERT INTO items VALUES (1)")

	if err := s.Begin(ctx, TxOptions{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if !s.ReadOnlyTransaction() {
		t.Fatal("want a read-only transaction")
	}
	if got := count(t, conn(t, s, "shop", false)); got != 1 {
		t.Fatalf("want 1, got %d", got)
	}
	// A read-only transaction never takes the writer (docs/06-transactions.md section 6).
	if !writerFree(t, db) {
		t.Fatal("a read-only transaction must not hold the writer")
	}
	write(t, db, "INSERT INTO items VALUES (2)")
	if got := count(t, conn(t, s, "shop", false)); got != 1 {
		t.Fatalf("the snapshot starts at the first read, got %d", got)
	}
	_, _, err := s.Conn(ctx, "shop", true)
	requireErr(t, err, 1792, "Cannot execute statement in a READ ONLY transaction.")
	if err := s.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readerCount(t, db); got != 2 {
		t.Fatalf("want 2 after the transaction, got %d", got)
	}
}

func TestConsistentSnapshotStartsAtBegin(t *testing.T) {
	s, db := useShop(t)
	ctx := context.Background()
	write(t, db, "CREATE TABLE items (a)", "INSERT INTO items VALUES (1)")

	if err := s.Begin(ctx, TxOptions{ReadOnly: true, Snapshot: true}); err != nil {
		t.Fatal(err)
	}
	write(t, db, "INSERT INTO items VALUES (2)")
	if got := count(t, conn(t, s, "shop", false)); got != 1 {
		t.Fatalf("WITH CONSISTENT SNAPSHOT reads as of START TRANSACTION, got %d", got)
	}
	if err := s.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// Without the clause the snapshot starts with the first read.
	if err := s.Begin(ctx, TxOptions{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	write(t, db, "INSERT INTO items VALUES (3)")
	if got := count(t, conn(t, s, "shop", false)); got != 3 {
		t.Fatalf("want 3, got %d", got)
	}
}

func TestConsistentSnapshotNeedsADatabase(t *testing.T) {
	e := newEngine(t, "shop")
	s := New(e)
	err := s.Begin(context.Background(), TxOptions{Snapshot: true})
	requireErr(t, err, 1235, "mysqlite: unsupported construct WITH CONSISTENT SNAPSHOT: a transaction spans one database in M1; select it first with USE")
	if s.InTransaction() {
		t.Fatal("a refused BEGIN starts nothing")
	}
}

func TestTransactionSpansOneDatabase(t *testing.T) {
	s, _ := useShop(t, "other")
	ctx := context.Background()
	if err := s.Begin(ctx, TxOptions{}); err != nil {
		t.Fatal(err)
	}
	conn(t, s, "shop", false)
	// A statement without tables does not bind the transaction.
	conn(t, s, "", false)
	_, _, err := s.Conn(ctx, "other", false)
	requireErr(t, err, 1235, "mysqlite: unsupported construct statement on another database inside a transaction: a transaction spans one database in M1")
	if !s.InTransaction() {
		t.Fatal("the refusal keeps the transaction open")
	}
}

func TestCloseRollsBack(t *testing.T) {
	s, db := useShop(t)
	ctx := context.Background()
	write(t, db, "CREATE TABLE items (a)")
	if err := s.Begin(ctx, TxOptions{}); err != nil {
		t.Fatal(err)
	}
	exec(t, conn(t, s, "shop", true), "INSERT INTO items VALUES (1)")
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if !writerFree(t, db) || readerCount(t, db) != 0 {
		t.Fatal("closing the session rolls back and releases the writer")
	}
}

// SQLite rolls a transaction back by itself after some errors (SQLITE_FULL, IOERR,
// BUSY, NOMEM). The session must notice, so IN_TRANS never claims a transaction that
// no longer exists.
func TestStatementFailedDetectsAutomaticRollback(t *testing.T) {
	s, db := useShop(t)
	ctx := context.Background()
	write(t, db, "CREATE TABLE items (a)")

	if err := s.Begin(ctx, TxOptions{}); err != nil {
		t.Fatal(err)
	}
	c := conn(t, s, "shop", true)
	exec(t, c, "INSERT INTO items VALUES (1)")
	s.StatementFailed(ctx)
	if !s.InTransaction() {
		t.Fatal("a transaction SQLite kept stays open")
	}
	exec(t, c, "ROLLBACK") // what SQLite does on SQLITE_FULL
	s.StatementFailed(ctx)
	if s.InTransaction() || !writerFree(t, db) || readerCount(t, db) != 0 {
		t.Fatal("a transaction SQLite rolled back is over")
	}
}

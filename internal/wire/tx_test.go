package wire

import (
	"context"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/engine"
)

func execOK(t *testing.T, conn *client.Conn, q string) *mysql.Result {
	t.Helper()
	res, err := conn.Execute(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return res
}

// writerFree reports whether the writer of shop can be taken within wait.
func writerFree(t *testing.T, e *engine.Engine, wait time.Duration) bool {
	t.Helper()
	db, err := e.Database("shop")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	w, err := db.Writer(ctx)
	if err != nil {
		return false
	}
	_ = w.Close()
	return true
}

// Clients read IN_TRANS from every OK and EOF packet to decide whether to send COMMIT
// (docs/03-protocol.md section 7).
func TestTransactionStatusFlags(t *testing.T) {
	conn := shopServer(t)
	if conn.IsInTransaction() || !conn.IsAutoCommit() {
		t.Fatalf("after connect: want AUTOCOMMIT without IN_TRANS, got %s", conn.StatusString())
	}

	for _, begin := range []string{"BEGIN", "START TRANSACTION", "START TRANSACTION WITH CONSISTENT SNAPSHOT"} {
		for _, end := range []string{"COMMIT", "ROLLBACK"} {
			execOK(t, conn, begin)
			if !conn.IsInTransaction() || !conn.IsAutoCommit() {
				t.Fatalf("after %s: want IN_TRANS and AUTOCOMMIT, got %s", begin, conn.StatusString())
			}
			// A result set carries the status in its EOF packet.
			execOK(t, conn, "SELECT id FROM items")
			if !conn.IsInTransaction() {
				t.Fatalf("after a SELECT in a transaction: want IN_TRANS, got %s", conn.StatusString())
			}
			execOK(t, conn, end)
			if conn.IsInTransaction() || !conn.IsAutoCommit() {
				t.Fatalf("after %s: want AUTOCOMMIT without IN_TRANS, got %s", end, conn.StatusString())
			}
		}
	}

	// COMMIT and ROLLBACK without a transaction are accepted and change nothing.
	execOK(t, conn, "COMMIT")
	execOK(t, conn, "ROLLBACK")
	if conn.IsInTransaction() {
		t.Fatal("no transaction was started")
	}
}

func TestReadOnlyTransaction(t *testing.T) {
	e, addr := shopSetup(t)
	conn := dial(t, addr)

	res := execOK(t, conn, "START TRANSACTION READ ONLY")
	if res.Status&mysql.SERVER_STATUS_IN_TRANS_READONLY == 0 || !conn.IsInTransaction() {
		t.Fatalf("want IN_TRANS and IN_TRANS_READONLY, got %s", conn.StatusString())
	}
	if got, _ := execOK(t, conn, "SELECT id FROM items").GetString(0, 0); got != "1" {
		t.Fatalf("want 1, got %q", got)
	}
	if !writerFree(t, e, 200*time.Millisecond) {
		t.Fatal("a read-only transaction must not hold the writer")
	}
	_, err := conn.Execute("INSERT INTO items (name) VALUES ('x')")
	requireMyError(t, err, mysql.ER_CANT_EXECUTE_IN_READ_ONLY_TRANSACTION, "25006")
	if !conn.IsInTransaction() {
		t.Fatal("the refused write keeps the transaction open")
	}
	res = execOK(t, conn, "COMMIT")
	if res.Status&mysql.SERVER_STATUS_IN_TRANS_READONLY != 0 || conn.IsInTransaction() {
		t.Fatalf("after COMMIT: got %s", conn.StatusString())
	}
}

func TestTransactionHoldsTheWriter(t *testing.T) {
	e, addr := shopSetup(t)
	conn := dial(t, addr)

	execOK(t, conn, "BEGIN")
	if !writerFree(t, e, 200*time.Millisecond) {
		t.Fatal("BEGIN alone must not take the writer")
	}
	execOK(t, conn, "SELECT id FROM items")
	if writerFree(t, e, 200*time.Millisecond) {
		t.Fatal("the first statement of a read-write transaction takes the writer")
	}
	execOK(t, conn, "COMMIT")
	if !writerFree(t, e, time.Second) {
		t.Fatal("COMMIT releases the writer")
	}

	// WITH CONSISTENT SNAPSHOT takes the snapshot, and so the writer, at once.
	execOK(t, conn, "START TRANSACTION /*!40100 WITH CONSISTENT SNAPSHOT */")
	if writerFree(t, e, 200*time.Millisecond) {
		t.Fatal("WITH CONSISTENT SNAPSHOT starts the transaction at once")
	}
	execOK(t, conn, "ROLLBACK")
}

func TestDisconnectRollsBackAndReleasesTheWriter(t *testing.T) {
	e, addr := shopSetup(t)
	conn, err := client.Connect(addr, testUser, testPassword, "shop")
	if err != nil {
		t.Fatal(err)
	}
	execOK(t, conn, "BEGIN")
	execOK(t, conn, "SELECT id FROM items")
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if !writerFree(t, e, 2*time.Second) {
		t.Fatal("a disconnect must roll back and release the writer")
	}
}

func TestTransactionRefusals(t *testing.T) {
	conn := shopServer(t)

	_, err := conn.Execute("SAVEPOINT a")
	requireMyError(t, err, mysql.ER_NOT_SUPPORTED_YET, "42000")
	_, err = conn.Execute("BEGIN PESSIMISTIC")
	requireMyError(t, err, mysql.ER_PARSE_ERROR, "42000")

	// Without a current database, the snapshot has no file to start on.
	addr := startServer(t, map[string]string{testUser: testPassword})
	bare := connect(t, addr, testUser, testPassword)
	_, err = bare.Execute("START TRANSACTION WITH CONSISTENT SNAPSHOT")
	requireMyError(t, err, mysql.ER_NOT_SUPPORTED_YET, "42000")
	if bare.IsInTransaction() {
		t.Fatal("a refused START TRANSACTION starts nothing")
	}
}

package wire

import (
	"path/filepath"
	"testing"
	"time"

	"mysqlite/internal/dialect/udf"
	"mysqlite/internal/engine"
)

// queueServer serves "shop" (table t) from an engine whose writer queue holds depth
// sessions.
func queueServer(t *testing.T, depth int) string {
	t.Helper()
	e, err := engine.Open(engine.Options{
		DataDir:         filepath.Join(t.TempDir(), "data"),
		MaxReadConns:    2,
		BusyTimeout:     50 * time.Millisecond,
		Synchronous:     "NORMAL",
		WriteQueueDepth: depth,
	}, udf.Extensions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.CreateDatabase("shop"); err != nil {
		t.Fatal(err)
	}
	seed(t, e, "shop", "CREATE TABLE t (id INTEGER PRIMARY KEY, v INT)")
	return startServerWith(t, Config{
		ServerVersion:    "8.0.36-mysqlite",
		MaxAllowedPacket: 64 << 20,
		Users:            map[string]string{testUser: testPassword},
		Engine:           e,
	})
}

// A session waiting for the writer gives up after innodb_lock_wait_timeout with 1205.
func TestLockWaitTimeout(t *testing.T) {
	addr := queueServer(t, 8)
	holder := dial(t, addr)
	execOK(t, holder, "BEGIN")
	execOK(t, holder, "INSERT INTO t VALUES (1, 1)")

	waiter := dial(t, addr)
	if v := decodedText(execOK(t, waiter, "SELECT @@innodb_lock_wait_timeout")); v != "50" {
		t.Fatalf("default innodb_lock_wait_timeout %s", v)
	}
	execOK(t, waiter, "SET innodb_lock_wait_timeout = 1")
	start := time.Now()
	_, err := waiter.Execute("INSERT INTO t VALUES (2, 2)")
	requireMyError(t, err, 1205, "HY000")
	if d := time.Since(start); d < 900*time.Millisecond || d > 5*time.Second {
		t.Fatalf("waited %v, want about 1s", d)
	}
	execOK(t, holder, "COMMIT")
	execOK(t, waiter, "INSERT INTO t VALUES (2, 2)")
}

// docs/06-transactions.md section 6: a full write queue is 1205 at once, not a hang.
func TestWriteQueueFull(t *testing.T) {
	addr := queueServer(t, 1)
	holder := dial(t, addr)
	execOK(t, holder, "BEGIN")
	execOK(t, holder, "INSERT INTO t VALUES (1, 1)")

	queued := dial(t, addr)
	done := make(chan error, 1)
	go func() {
		_, err := queued.Execute("INSERT INTO t VALUES (2, 2)")
		done <- err
	}()
	// Wait until the second session is in the queue: a third one is then refused.
	third := dial(t, addr)
	// Before the queue fills, the third session would wait its turn: 1 s, then retry.
	execOK(t, third, "SET innodb_lock_wait_timeout = 1")
	deadline := time.Now().Add(5 * time.Second)
	for {
		start := time.Now()
		_, err := third.Execute("INSERT INTO t VALUES (3, 3)")
		if err == nil {
			t.Fatal("third writer was not refused")
		}
		requireMyError(t, err, 1205, "HY000")
		if time.Since(start) < 500*time.Millisecond {
			break // refused at once: the queue is full
		}
		if time.Now().After(deadline) {
			t.Fatal("the queue never filled")
		}
	}
	execOK(t, holder, "COMMIT")
	if err := <-done; err != nil {
		t.Fatalf("queued writer: %v", err)
	}
	if n := decodedText(execOK(t, third, "SELECT COUNT(*) FROM t")); n != "2" {
		t.Fatalf("rows %s", n)
	}
}

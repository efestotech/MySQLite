package wire

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mysqlite/internal/config"
	"mysqlite/internal/dialect/udf"
	"mysqlite/internal/engine"
	"mysqlite/internal/obs"
)

// busyServer serves "shop" (table t) with a 50 ms busy_timeout and a journal. It returns
// the address, the journal file and the path of shop's database file.
func busyServer(t *testing.T) (addr, journal, file string) {
	t.Helper()
	data := filepath.Join(t.TempDir(), "data")
	e, err := engine.Open(engine.Options{
		DataDir:      data,
		MaxReadConns: 2,
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
	seed(t, e, "shop", "CREATE TABLE t (id INTEGER PRIMARY KEY, v INT)")
	logs := filepath.Join(t.TempDir(), "logs")
	j, err := obs.NewJournal(config.Log{Dir: logs, MaxSizeMB: 1, MaxFiles: 1, Journal: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	addr = startServerWith(t, Config{
		ServerVersion:    "8.0.36-mysqlite",
		MaxAllowedPacket: 64 << 20,
		Users:            map[string]string{testUser: testPassword},
		Engine:           e,
		Journal:          j,
	})
	return addr, filepath.Join(logs, "journal.jsonl"), filepath.Join(data, "shop.db")
}

// holdWriteLock takes file's write lock from another SQLite connection, as another
// process would, and releases it after d.
func holdWriteLock(t *testing.T, file string, d time.Duration) <-chan struct{} {
	t.Helper()
	other, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	c, err := other.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(d)
		_, _ = c.ExecContext(context.Background(), "COMMIT")
		_ = c.Close()
		close(released)
	}()
	return released
}

// journalRetries returns the "retries" of the journal entries whose sql is query.
func journalRetries(t *testing.T, journal, query string) []float64 {
	t.Helper()
	f, err := os.Open(journal)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []float64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		if rec["sql"] == query {
			n, _ := rec["retries"].(float64)
			out = append(out, n)
		}
	}
	return out
}

// docs/06-transactions.md section 5: an autocommit write that finds the database busy
// is retried with backoff, and the journal records the retries.
func TestBusyWriteIsRetried(t *testing.T) {
	addr, journal, file := busyServer(t)
	conn := dial(t, addr)
	released := holdWriteLock(t, file, 150*time.Millisecond)
	const q = "INSERT INTO t VALUES (1, 1)"
	execOK(t, conn, q)
	<-released
	if n := decodedText(execOK(t, conn, "SELECT COUNT(*) FROM t")); n != "1" {
		t.Fatalf("rows %s", n)
	}
	if r := journalRetries(t, journal, q); len(r) != 1 || r[0] < 1 {
		t.Fatalf("journal retries %v, want one entry with at least 1", r)
	}
}

// After the bounded retries the write fails with 1205 and has no effect.
func TestBusyWriteGivesUp(t *testing.T) {
	addr, journal, file := busyServer(t)
	conn := dial(t, addr)
	released := holdWriteLock(t, file, 3*time.Second)
	const q = "INSERT INTO t VALUES (1, 1)"
	start := time.Now()
	_, err := conn.Execute(q)
	requireMyError(t, err, 1205, "HY000")
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("gave up after %v; the retries must be bounded", d)
	}
	<-released
	if n := decodedText(execOK(t, conn, "SELECT COUNT(*) FROM t")); n != "0" {
		t.Fatalf("rows %s", n)
	}
	if r := journalRetries(t, journal, q); len(r) != 1 || r[0] < 1 {
		t.Fatalf("journal retries %v", r)
	}
}

// A statement inside a transaction is never retried: the client restarts it.
func TestBusyInTransactionNotRetried(t *testing.T) {
	addr, journal, file := busyServer(t)
	conn := dial(t, addr)
	execOK(t, conn, "BEGIN")
	released := holdWriteLock(t, file, 400*time.Millisecond)
	const q = "INSERT INTO t VALUES (1, 1)"
	_, err := conn.Execute(q)
	requireMyError(t, err, 1205, "HY000")
	<-released
	execOK(t, conn, "ROLLBACK")
	if r := journalRetries(t, journal, q); len(r) != 1 || r[0] != 0 {
		t.Fatalf("journal retries %v, want [0]", r)
	}
}

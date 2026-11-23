package wire

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mysqlite/internal/config"
	"mysqlite/internal/dialect/udf"
	"mysqlite/internal/engine"
	"mysqlite/internal/obs"
)

// idleServer serves "shop" (table t) with an idle transaction timeout and a journal.
func idleServer(t *testing.T, timeout time.Duration) (addr, journal string) {
	t.Helper()
	e, err := engine.Open(engine.Options{
		DataDir:      filepath.Join(t.TempDir(), "data"),
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
		IdleTxTimeout:    timeout,
	})
	return addr, filepath.Join(logs, "journal.jsonl")
}

// M5 criterion 3 (docs/06-transactions.md section 6, decision Q6): a transaction idle
// for idle_tx_timeout_s is rolled back, which releases the writer; the client gets
// ER_CLIENT_INTERACTION_TIMEOUT (4031) and the connection is closed; the journal records
// it. `oracle: pending`.
func TestIdleTransactionTimeout(t *testing.T) {
	addr, journal := idleServer(t, 500*time.Millisecond)
	idle := dial(t, addr)
	execOK(t, idle, "BEGIN")
	execOK(t, idle, "INSERT INTO t VALUES (1, 1)")

	// Outside a transaction a session may idle as long as it likes.
	calm := dial(t, addr)
	execOK(t, calm, "SELECT 1")

	other := dial(t, addr)
	execOK(t, other, "SET innodb_lock_wait_timeout = 10")
	start := time.Now()
	execOK(t, other, "INSERT INTO t VALUES (2, 2)")
	if d := time.Since(start); d < 300*time.Millisecond || d > 5*time.Second {
		t.Fatalf("the writer was released after %v, want about 500ms", d)
	}

	// The unsolicited error, sequence 0, then the end of the connection.
	_ = idle.SetReadDeadline(time.Now().Add(2 * time.Second))
	idle.ResetSequence()
	data, err := idle.ReadPacket()
	if err != nil {
		t.Fatalf("read the timeout error: %v", err)
	}
	e := errPacket(t, idle, data)
	if e.Code != 4031 || e.State != "HY000" || !strings.HasPrefix(e.Message, "The client was disconnected by the server because of inactivity.") {
		t.Fatalf("error %v", e)
	}
	if _, err := idle.ReadPacket(); err == nil {
		t.Fatal("the connection is still open")
	}

	time.Sleep(700 * time.Millisecond) // longer than the timeout, without a transaction
	execOK(t, calm, "SELECT 1")
	if n := decodedText(execOK(t, calm, "SELECT COUNT(*) FROM t")); n != "1" {
		t.Fatalf("rows %s: the idle transaction's insert must be rolled back", n)
	}

	f, err := os.Open(journal)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	found := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		if rec["error_code"] == float64(4031) {
			found = true
		}
	}
	if !found {
		t.Error("the journal has no 4031 entry")
	}
}

// With the timeout off (0) an idle transaction keeps the writer.
func TestIdleTransactionTimeoutOff(t *testing.T) {
	addr, _ := idleServer(t, 0)
	idle := dial(t, addr)
	execOK(t, idle, "BEGIN")
	execOK(t, idle, "INSERT INTO t VALUES (1, 1)")
	time.Sleep(300 * time.Millisecond)
	other := dial(t, addr)
	execOK(t, other, "SET innodb_lock_wait_timeout = 1")
	_, err := other.Execute("INSERT INTO t VALUES (2, 2)")
	requireMyError(t, err, 1205, "HY000")
	execOK(t, idle, "COMMIT")
}

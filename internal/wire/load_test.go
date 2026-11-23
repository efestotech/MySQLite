package wire

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/client"

	"mysqlite/internal/dialect/udf"
	"mysqlite/internal/engine"
)

// loadDuration is how long TestConcurrentLoad runs. M5 criterion 1 asks for 60s:
//
//	go test ./internal/wire -run TestConcurrentLoad -load 60s
var loadDuration = flag.Duration("load", 3*time.Second, "duration of TestConcurrentLoad")

// M5 criterion 1 (docs/08-milestones.md): "8 readers + 4 writers for 60s: zero lost
// updates, zero surfaced lock errors." Writers increment counters, half in autocommit,
// half as read-modify-write transactions; readers sum the counters, which must never
// decrease. Every increment a client saw succeed must be in the final sum.
func TestConcurrentLoad(t *testing.T) {
	addr := queueServer(t, 256)
	setup := dial(t, addr)
	execOK(t, setup, "CREATE TABLE counters (id INT PRIMARY KEY, n BIGINT NOT NULL)")
	execOK(t, setup, "INSERT INTO counters VALUES (1, 0), (2, 0), (3, 0), (4, 0)")

	const readers, writers = 8, 4
	deadline := time.Now().Add(*loadDuration)
	var increments, reads atomic.Int64
	errc := make(chan error, readers+writers)
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := dial(t, addr)
			id := w%4 + 1
			for i := 0; time.Now().Before(deadline); i++ {
				if w%2 == 0 {
					if _, err := conn.Execute(fmt.Sprintf("UPDATE counters SET n = n + 1 WHERE id = %d", id)); err != nil {
						errc <- fmt.Errorf("writer %d: %w", w, err)
						return
					}
				} else if err := readModifyWrite(conn, id); err != nil {
					errc <- fmt.Errorf("writer %d: %w", w, err)
					return
				}
				increments.Add(1)
			}
		}()
	}
	for r := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := dial(t, addr)
			last := int64(-1)
			for time.Now().Before(deadline) {
				res, err := conn.Execute("SELECT SUM(n) FROM counters")
				if err != nil {
					errc <- fmt.Errorf("reader %d: %w", r, err)
					return
				}
				sum, err := strconv.ParseInt(decodedText(res), 10, 64)
				if err != nil || sum < last {
					errc <- fmt.Errorf("reader %d: sum %q after %d", r, decodedText(res), last)
					return
				}
				last = sum
				reads.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	total := decodedText(execOK(t, setup, "SELECT SUM(n) FROM counters"))
	if total != strconv.FormatInt(increments.Load(), 10) {
		t.Fatalf("lost updates: sum %s, increments %d", total, increments.Load())
	}
	t.Logf("%v: %d increments, %d reads, sum %s", *loadDuration, increments.Load(), reads.Load(), total)
}

// readModifyWrite increments counter id in a transaction that reads it first: the
// transaction holds the writer from its first statement, so no other increment can
// slip in between the read and the write.
func readModifyWrite(conn *client.Conn, id int) error {
	if _, err := conn.Execute("BEGIN"); err != nil {
		return err
	}
	res, err := conn.Execute(fmt.Sprintf("SELECT n FROM counters WHERE id = %d", id))
	if err != nil {
		_, _ = conn.Execute("ROLLBACK")
		return err
	}
	n, err := strconv.ParseInt(decodedText(res), 10, 64)
	if err != nil {
		_, _ = conn.Execute("ROLLBACK")
		return err
	}
	if _, err := conn.Execute(fmt.Sprintf("UPDATE counters SET n = %d WHERE id = %d", n+1, id)); err != nil {
		_, _ = conn.Execute("ROLLBACK")
		return err
	}
	_, err = conn.Execute("COMMIT")
	return err
}

const (
	killDirEnv   = "MYSQLITE_KILL_DIR"
	killRoundEnv = "MYSQLITE_KILL_ROUND" // keeps each round's ids apart
)

// TestKillHelper is the process TestKillDuringWrites kills: four sessions insert rows,
// alone and in transactions of five, and print "ack <id>" for each committed row.
func TestKillHelper(t *testing.T) {
	dir := os.Getenv(killDirEnv)
	if dir == "" {
		t.Skip("run by TestKillDuringWrites")
	}
	e, err := engine.Open(engine.Options{DataDir: dir, MaxReadConns: 2, BusyTimeout: time.Second, Synchronous: "NORMAL"}, udf.Extensions())
	if err != nil {
		t.Fatal(err)
	}
	addr := startServerWith(t, Config{
		ServerVersion:    "8.0.36-mysqlite",
		MaxAllowedPacket: 64 << 20,
		Users:            map[string]string{testUser: testPassword},
		Engine:           e,
	})
	round, _ := strconv.Atoi(os.Getenv(killRoundEnv))
	var mu sync.Mutex
	ack := func(ids ...int) {
		mu.Lock()
		defer mu.Unlock()
		for _, id := range ids {
			fmt.Printf("ack %d\n", id)
		}
	}
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := dial(t, addr)
			pad := strings.Repeat("x", 200)
			for i := 0; ; i++ {
				id := round*100_000_000 + w*10_000_000 + i*5
				if w%2 == 0 {
					if _, err := conn.Execute(fmt.Sprintf("INSERT INTO rows_ VALUES (%d, '%s')", id, pad)); err != nil {
						t.Error(err)
						return
					}
					ack(id)
					continue
				}
				ids := []int{id, id + 1, id + 2, id + 3, id + 4}
				_, _ = conn.Execute("BEGIN")
				for _, x := range ids {
					if _, err := conn.Execute(fmt.Sprintf("INSERT INTO rows_ VALUES (%d, '%s')", x, pad)); err != nil {
						t.Error(err)
						return
					}
				}
				if _, err := conn.Execute("COMMIT"); err != nil {
					t.Error(err)
					return
				}
				ack(ids...)
			}
		}()
	}
	wg.Wait()
}

// M5 criterion 2: "Kill -9 during writes: `integrity_check` OK on restart." The helper
// process is killed without warning (SIGKILL on Unix, TerminateProcess on Windows).
// After it: the instance lock is free, integrity_check is ok, and every row whose
// commit a client saw acknowledged is there.
func TestKillDuringWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a child process")
	}
	dir := filepath.Join(t.TempDir(), "data")
	e, err := engine.Open(engine.Options{DataDir: dir, MaxReadConns: 1, BusyTimeout: time.Second, Synchronous: "NORMAL"}, udf.Extensions())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.CreateDatabase("shop"); err != nil {
		t.Fatal(err)
	}
	seed(t, e, "shop", "CREATE TABLE rows_ (id INT PRIMARY KEY, pad VARCHAR(255))")
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	for round := range 3 {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestKillHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), killDirEnv+"="+dir, fmt.Sprintf("%s=%d", killRoundEnv, round))
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		acked := map[int]bool{}
		sc := bufio.NewScanner(out)
		start := time.Now()
		for sc.Scan() {
			if id, ok := strings.CutPrefix(sc.Text(), "ack "); ok {
				n, _ := strconv.Atoi(id)
				acked[n] = true
			}
			if len(acked) >= 200*(round+1) || time.Since(start) > 10*time.Second {
				break
			}
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		for sc.Scan() { // acks written before the kill
			if id, ok := strings.CutPrefix(sc.Text(), "ack "); ok {
				n, _ := strconv.Atoi(id)
				acked[n] = true
			}
		}
		_ = cmd.Wait()
		if len(acked) == 0 {
			t.Fatalf("round %d: the helper acknowledged no write; stderr:\n%s", round, stderr.String())
		}

		e, err := engine.Open(engine.Options{DataDir: dir, MaxReadConns: 1, BusyTimeout: time.Second, Synchronous: "NORMAL"}, udf.Extensions())
		if err != nil {
			t.Fatalf("round %d: restart: %v", round, err)
		}
		if integrity, _, _ := rawCheck(t, e, "shop"); integrity != "ok" {
			t.Errorf("round %d: integrity_check %q", round, integrity)
		}
		d, err := e.Database("shop")
		if err != nil {
			t.Fatal(err)
		}
		r, err := d.Read(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		present := map[int]bool{}
		rows, err := r.QueryContext(context.Background(), "SELECT id FROM rows_")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id int
			_ = rows.Scan(&id)
			present[id] = true
		}
		rows.Close()
		_ = r.Close()
		lost := 0
		for id := range acked {
			if !present[id] {
				lost++
			}
		}
		if lost > 0 {
			t.Errorf("round %d: %d of %d acknowledged rows lost", round, lost, len(acked))
		}
		t.Logf("round %d: %d acknowledged, %d rows present", round, len(acked), len(present))
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

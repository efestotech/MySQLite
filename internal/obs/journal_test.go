package obs

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mysqlite/internal/config"
)

func readJournal(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	defer f.Close()
	var lines []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("line is not JSON: %q", sc.Text())
		}
		lines = append(lines, rec)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan journal: %v", err)
	}
	return lines
}

func TestJournalWritesEntryFields(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	j, err := NewJournal(config.Log{Dir: dir, MaxSizeMB: 1, MaxFiles: 2, Journal: true})
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	at := time.Date(2026, 11, 22, 10, 0, 0, 123000000, time.UTC)
	err = j.Record(Entry{
		Time: at, Session: 7, User: "root", Database: "shop",
		SQL: "SELECT `a` FROM t LIMIT 1, 2", Kind: "read", Rules: []string{"R001", "R005"},
		Emitted: "SELECT `a` FROM `t` LIMIT 2 OFFSET 1", Params: []any{int64(1)},
		Duration: 1500 * time.Microsecond, Outcome: OutcomeOK, Rows: 2,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	err = j.Record(Entry{
		Time: at, Session: 7, User: "root", SQL: "SELECT 1 UNION SELECT 2",
		Outcome: OutcomeError, ErrorCode: 1235, SQLState: "42000",
		Error: "mysqlite: unsupported construct UNION: arrives with M5", Unsupported: "UNION",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := readJournal(t, filepath.Join(dir, journalName))
	if len(lines) != 2 {
		t.Fatalf("want 2 entries, got %d", len(lines))
	}
	ok := lines[0]
	want := map[string]any{
		"ts": "2026-11-22T10:00:00.123Z", "session": float64(7), "user": "root", "database": "shop",
		"sql": "SELECT `a` FROM t LIMIT 1, 2", "kind": "read",
		"emitted": "SELECT `a` FROM `t` LIMIT 2 OFFSET 1", "duration_us": float64(1500),
		"outcome": "ok", "rows": float64(2), "affected": float64(0),
	}
	for k, v := range want {
		if ok[k] != v {
			t.Errorf("%s: want %v, got %v", k, v, ok[k])
		}
	}
	if rules, _ := ok["rules"].([]any); len(rules) != 2 || rules[0] != "R001" || rules[1] != "R005" {
		t.Errorf("rules: got %v", ok["rules"])
	}
	if params, _ := ok["params"].([]any); len(params) != 1 || params[0] != float64(1) {
		t.Errorf("params: got %v", ok["params"])
	}
	if _, present := ok["error_code"]; present {
		t.Errorf("a successful entry has no error_code: %v", ok)
	}

	fail := lines[1]
	for k, v := range map[string]any{
		"outcome": "error", "error_code": float64(1235), "sqlstate": "42000",
		"error": "mysqlite: unsupported construct UNION: arrives with M5", "unsupported": "UNION",
	} {
		if fail[k] != v {
			t.Errorf("%s: want %v, got %v", k, v, fail[k])
		}
	}
	if _, present := fail["database"]; present {
		t.Errorf("no current database: the key is omitted, got %v", fail["database"])
	}
}

func TestJournalRotatesBySizeAndKeepsMaxFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	const maxFiles = 2
	j, err := NewJournal(config.Log{Dir: dir, MaxSizeMB: 1, MaxFiles: maxFiles, Journal: true})
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	// About 4.5 MiB of entries with a 1 MiB limit: at least 4 rotations.
	sql := "SELECT '" + strings.Repeat("x", 1000) + "'"
	for i := 0; i < 4500; i++ {
		if err := j.Record(Entry{Time: time.Now(), Session: uint32(i), SQL: sql, Outcome: OutcomeOK}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// lumberjack deletes old backups in a background goroutine, so wait for it.
	deadline := time.Now().Add(5 * time.Second)
	var backups []string
	for {
		backups = nil
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir: %v", err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "journal-") && strings.HasSuffix(e.Name(), ".jsonl") {
				backups = append(backups, e.Name())
			}
		}
		if len(backups) == maxFiles {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("want %d journal backups, got %v", maxFiles, backups)
		}
		time.Sleep(50 * time.Millisecond)
	}

	current := filepath.Join(dir, journalName)
	info, err := os.Stat(current)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() > 1<<20 {
		t.Fatalf("current journal exceeds max size: %d bytes", info.Size())
	}
	// Rotation never splits an entry: every file holds whole JSON lines, and the last
	// entry written is in the current file.
	for _, name := range backups {
		readJournal(t, filepath.Join(dir, name))
	}
	lines := readJournal(t, current)
	if last := lines[len(lines)-1]["session"]; last != float64(4499) {
		t.Fatalf("last entry: want session 4499, got %v", last)
	}
}

func TestJournalDisabledWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	j, err := NewJournal(config.Log{Dir: dir, MaxSizeMB: 1, MaxFiles: 1, Journal: false})
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	if err := j.Record(Entry{Time: time.Now(), SQL: "SELECT 1", Outcome: OutcomeOK}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, journalName)); !os.IsNotExist(err) {
		t.Fatalf("a disabled journal creates no file, stat: %v", err)
	}
}

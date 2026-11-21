package obs

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mysqlite/internal/config"
)

func logFiles(t *testing.T, dir string) (current bool, backups int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read log dir: %v", err)
	}
	for _, e := range entries {
		switch {
		case e.Name() == serverLogName:
			current = true
		case strings.HasPrefix(e.Name(), "server-") && strings.HasSuffix(e.Name(), ".log"):
			backups++
		}
	}
	return current, backups
}

func TestServerLogWritesJSONLinesAtConfiguredLevel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	log, closer, err := NewServerLogger(config.Log{Level: "warn", Dir: dir, MaxSizeMB: 1, MaxFiles: 2}, io.Discard)
	if err != nil {
		t.Fatalf("NewServerLogger: %v", err)
	}
	log.Info("dropped")
	log.Warn("kept", "conn_id", 7)
	if err := closer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	f, err := os.Open(filepath.Join(dir, serverLogName))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	var lines []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("line is not JSON: %q", sc.Text())
		}
		lines = append(lines, rec)
	}
	if len(lines) != 1 {
		t.Fatalf("want 1 line at level warn, got %d: %v", len(lines), lines)
	}
	if lines[0]["msg"] != "kept" || lines[0]["level"] != "WARN" || lines[0]["conn_id"] != float64(7) {
		t.Fatalf("unexpected record: %v", lines[0])
	}
}

func TestServerLogRotatesBySizeAndKeepsMaxFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	const maxFiles = 2
	log, closer, err := NewServerLogger(config.Log{Level: "info", Dir: dir, MaxSizeMB: 1, MaxFiles: maxFiles}, io.Discard)
	if err != nil {
		t.Fatalf("NewServerLogger: %v", err)
	}
	// About 4.5 MiB of records with a 1 MiB limit: at least 4 rotations.
	payload := strings.Repeat("x", 1000)
	for i := 0; i < 4500; i++ {
		log.Info("filler", "i", i, "payload", payload)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// lumberjack deletes old backups in a background goroutine, so wait for it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		current, backups := logFiles(t, dir)
		if current && backups == maxFiles {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("want current log and %d backups, got current=%v backups=%d", maxFiles, current, backups)
		}
		time.Sleep(50 * time.Millisecond)
	}

	info, err := os.Stat(filepath.Join(dir, serverLogName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() > 1<<20 {
		t.Fatalf("current log exceeds max size: %d bytes", info.Size())
	}
}

func TestServerLogRejectsUnknownLevel(t *testing.T) {
	_, _, err := NewServerLogger(config.Log{Level: "loud", Dir: t.TempDir(), MaxSizeMB: 1, MaxFiles: 1}, io.Discard)
	if err == nil {
		t.Fatal("want error for unknown level")
	}
}

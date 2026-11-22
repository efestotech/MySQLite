package wire

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mysqlite/internal/config"
	"mysqlite/internal/obs"
)

// Every COM_QUERY is journalled with what was received, what was run and how it ended
// (docs/08-milestones.md M1, criterion 4).
func TestQueriesAreJournalled(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	j, err := obs.NewJournal(config.Log{Dir: dir, MaxSizeMB: 1, MaxFiles: 1, Journal: true})
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	e := newEngine(t)
	seed(t, e, "shop", "CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(20) NOT NULL)",
		"INSERT INTO items (name) VALUES ('pen'), ('ink')")
	addr := startServerWith(t, Config{
		ServerVersion:    "8.0.36-mysqlite",
		MaxAllowedPacket: 64 << 20,
		Users:            map[string]string{testUser: testPassword},
		Engine:           e,
		Journal:          j,
	})
	conn := dial(t, addr)

	queries := []string{
		"SELECT name FROM items WHERE name = 'pen' LIMIT 0, 1",
		"INSERT INTO items (name) VALUES ('cap'), ('nib')",
		"SELECT nope FROM missing",
		"SELECT 1 UNION SELECT 2",
		"BEGIN",
	}
	for _, q := range queries {
		_, _ = conn.Execute(q)
	}

	f, err := os.Open(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	defer f.Close()
	var got []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("journal line is not JSON: %q", sc.Text())
		}
		got = append(got, rec)
	}
	if len(got) != len(queries) {
		t.Fatalf("want %d entries, got %d: %v", len(queries), len(got), got)
	}
	for i, rec := range got {
		if rec["sql"] != queries[i] {
			t.Errorf("entry %d: sql %v, want %q", i, rec["sql"], queries[i])
		}
		if rec["user"] != testUser || rec["database"] != "shop" || rec["session"] == float64(0) {
			t.Errorf("entry %d: user/database/session %v/%v/%v", i, rec["user"], rec["database"], rec["session"])
		}
		if _, ok := rec["duration_us"].(float64); !ok {
			t.Errorf("entry %d: no duration_us", i)
		}
	}

	sel := got[0]
	if sel["kind"] != "read" || sel["outcome"] != "ok" || sel["rows"] != float64(1) {
		t.Errorf("select: %v", sel)
	}
	if rules, _ := sel["rules"].([]any); len(rules) != 2 || rules[0] != "R002" || rules[1] != "R005" {
		t.Errorf("select rules: %v", sel["rules"])
	}
	if em, _ := sel["emitted"].(string); !strings.Contains(em, "LIMIT 1 OFFSET 0") {
		t.Errorf("select emitted: %v", sel["emitted"])
	}
	if params, _ := sel["params"].([]any); len(params) != 1 || params[0] != "pen" {
		t.Errorf("select params: %v", sel["params"])
	}

	ins := got[1]
	if ins["kind"] != "write" || ins["outcome"] != "ok" || ins["affected"] != float64(2) {
		t.Errorf("insert: %v", ins)
	}

	missing := got[2]
	if missing["outcome"] != "error" || missing["error_code"] != float64(1146) || missing["sqlstate"] != "42S02" {
		t.Errorf("missing table: %v", missing)
	}

	union := got[3]
	if union["error_code"] != float64(1235) || union["unsupported"] == nil || union["unsupported"] == "" {
		t.Errorf("unsupported construct: %v", union)
	}
	if _, ok := union["kind"]; ok {
		t.Errorf("a statement that was not translated has no kind: %v", union)
	}

	if begin := got[4]; begin["kind"] != "transaction" || begin["outcome"] != "ok" {
		t.Errorf("begin: %v", begin)
	}
}

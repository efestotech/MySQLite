package obs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadReport(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := write("journal-2026-11-22T10-00-00.000.jsonl",
		`{"ts":"2026-11-22T10:00:00Z","session":1,"user":"u","sql":"SELECT 1 INTERSECT ALL SELECT 2","duration_us":1,"outcome":"error","rows":0,"affected":0,"error_code":1235,"unsupported":"INTERSECT ALL"}
{"ts":"2026-11-22T10:00:01Z","session":1,"user":"u","sql":"SELECT 1","duration_us":1,"outcome":"ok","rows":1,"affected":0}
`)
	cur := write("journal.jsonl",
		`{"ts":"2026-11-23T09:00:00Z","session":2,"user":"u","sql":"SELECT 1 INTERSECT ALL SELECT 3","duration_us":1,"outcome":"error","rows":0,"affected":0,"error_code":1235,"unsupported":"INTERSECT ALL"}
{"ts":"2026-11-23T08:00:00Z","session":2,"user":"u","sql":"SELECT 1 INTERSECT ALL SELECT 3","duration_us":1,"outcome":"error","rows":0,"affected":0,"error_code":1235,"unsupported":"INTERSECT ALL"}
{"ts":"2026-11-23T09:00:01Z","session":2,"user":"u","sql":"SELECT a FROM t WINDOW w AS ()","duration_us":1,"outcome":"error","rows":0,"affected":0,"error_code":1235,"unsupported":"WINDOW"}
{"ts":"2026-11-23T09:00:02Z","session":3,"user":"u","sql":"SELECT 1 & 2","duration_us":1,"outcome":"error","rows":0,"affected":0,"error_code":1235,"unsupported":"operator &"}

{"ts":"2026-11-23T09:00:03Z","session":3,"user":"u","sql":"SEL
`)
	files, err := JournalFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0] != old || files[1] != cur {
		t.Fatalf("JournalFiles = %v", files)
	}
	rep, err := ReadReport(files)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Statements != 6 || rep.Unsupported != 5 || rep.Malformed != 1 {
		t.Errorf("totals: %+v", rep)
	}
	at := func(s string) time.Time {
		ts, _ := time.Parse(time.RFC3339, s)
		return ts
	}
	want := []Construct{
		{Name: "INTERSECT ALL", Count: 3, Sessions: 2, Statements: 2, First: at("2026-11-22T10:00:00Z"), Last: at("2026-11-23T09:00:00Z"), Example: "SELECT 1 INTERSECT ALL SELECT 2"},
		{Name: "WINDOW", Count: 1, Sessions: 1, Statements: 1, First: at("2026-11-23T09:00:01Z"), Last: at("2026-11-23T09:00:01Z"), Example: "SELECT a FROM t WINDOW w AS ()"},
		{Name: "operator &", Count: 1, Sessions: 1, Statements: 1, First: at("2026-11-23T09:00:02Z"), Last: at("2026-11-23T09:00:02Z"), Example: "SELECT 1 & 2"},
	}
	if len(rep.Constructs) != len(want) {
		t.Fatalf("constructs: %+v", rep.Constructs)
	}
	for i, w := range want {
		g := rep.Constructs[i]
		if g.Name != w.Name || g.Count != w.Count || g.Sessions != w.Sessions || g.Statements != w.Statements ||
			!g.First.Equal(w.First) || !g.Last.Equal(w.Last) || g.Example != w.Example {
			t.Errorf("rank %d: got %+v, want %+v", i+1, g, w)
		}
	}
}

func TestReadReportMissingFile(t *testing.T) {
	if _, err := ReadReport([]string{filepath.Join(t.TempDir(), "nope.jsonl")}); err == nil {
		t.Error("missing file accepted")
	}
}

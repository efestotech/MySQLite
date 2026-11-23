package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/config"
	"mysqlite/internal/dialect/udf"
	"mysqlite/internal/engine"
	"mysqlite/internal/obs"
	"mysqlite/internal/wire"
)

// journalOf runs statements through a mysqlite server with the journal on and returns
// the log directory. Each statement runs on its own connection when sessions is set.
func journalOf(t *testing.T, statements []string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "logs")
	j, err := obs.NewJournal(config.Log{Dir: dir, MaxSizeMB: 1, MaxFiles: 1, Journal: true})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.Open(engine.Options{
		DataDir: filepath.Join(t.TempDir(), "data"), MaxReadConns: 2, BusyTimeout: time.Second, Synchronous: "NORMAL",
	}, udf.Extensions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	if err := eng.CreateDatabase("shop"); err != nil {
		t.Fatal(err)
	}
	srv, err := wire.NewServer(wire.Config{
		ServerVersion: "8.0.36-mysqlite", MaxAllowedPacket: 64 << 20,
		Users: map[string]string{"u": "p"}, Engine: eng, Journal: j,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	for i, q := range statements {
		conn, err := client.Connect(ln.Addr().String(), "u", "p", "shop")
		if err != nil {
			t.Fatal(err)
		}
		_, err = conn.Execute(q)
		var me *mysql.MyError
		if i < 4 && (!errors.As(err, &me) || me.Code != 1235) {
			t.Fatalf("%s: want 1235, got %v", q, err)
		}
		conn.Close()
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// M3 criteria 5 and 6: statements refused with 1235 reach the journal, and the report
// ranks their constructs by frequency.
func TestReportRanksUnsupportedConstructs(t *testing.T) {
	dir := journalOf(t, []string{
		"SELECT 1 INTERSECT ALL SELECT 2",
		"SELECT 1 & 2",
		"SELECT 3 INTERSECT ALL SELECT 4",
		"SELECT 1 INTERSECT ALL SELECT 2",
		"SELECT 1",
	})

	var out, errOut bytes.Buffer
	if code := run([]string{"report", dir}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if lines[0] != "5 statements, 4 unsupported (80.0%)" {
		t.Errorf("summary %q", lines[0])
	}
	if len(lines) != 5 {
		t.Fatalf("report:\n%s", out.String())
	}
	header := strings.Fields(lines[2])
	if strings.Join(header, " ") != "RANK COUNT SESSIONS STATEMENTS CONSTRUCT EXAMPLE" {
		t.Errorf("header %q", lines[2])
	}
	for i, want := range []string{
		"1 3 3 2 INTERSECT ALL SELECT 1 INTERSECT ALL SELECT 2",
		"2 1 1 1 operator & SELECT 1 & 2",
	} {
		if got := strings.Join(strings.Fields(lines[3+i]), " "); got != want {
			t.Errorf("rank %d: %q, want %q", i+1, got, want)
		}
	}

	out.Reset()
	if code := run([]string{"report", "-json", "-top", "1", filepath.Join(dir, "journal.jsonl")}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var rep obs.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("JSON: %v\n%s", err, out.String())
	}
	if rep.Statements != 5 || rep.Unsupported != 4 || len(rep.Constructs) != 1 ||
		rep.Constructs[0].Name != "INTERSECT ALL" || rep.Constructs[0].Count != 3 {
		t.Errorf("JSON report %+v", rep)
	}
}

func TestReportUsageAndErrors(t *testing.T) {
	empty := t.TempDir()
	cases := []struct {
		args []string
		code int
		msg  string
	}{
		{nil, 2, "usage: mysqlite-corpus report"},
		{[]string{"import"}, 2, "usage: mysqlite-corpus report"},
		{[]string{"report"}, 2, "usage: mysqlite-corpus report"},
		{[]string{"report", "-top", "-1", empty}, 2, "usage: mysqlite-corpus report"},
		{[]string{"report", "-nope", empty}, 2, "flag provided but not defined"},
		{[]string{"report", filepath.Join(empty, "missing.jsonl")}, 1, "mysqlite-corpus:"},
		{[]string{"report", empty}, 1, "no journal.jsonl or journal-*.jsonl"},
	}
	for _, c := range cases {
		var out, errOut bytes.Buffer
		if code := run(c.args, &out, &errOut); code != c.code || !strings.Contains(errOut.String(), c.msg) {
			t.Errorf("%v: exit %d, stderr %q; want %d with %q", c.args, code, errOut.String(), c.code, c.msg)
		}
		if out.Len() != 0 {
			t.Errorf("%v: stdout %q", c.args, out.String())
		}
	}
}

// An empty journal reports zero statements and no table; malformed lines are counted
// on stderr.
func TestReportEmptyAndMalformed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "journal.jsonl"), []byte("{\"ts\":\"x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"report", dir}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out.String() != "0 statements, 0 unsupported (0.0%)\n" {
		t.Errorf("stdout %q", out.String())
	}
	if errOut.String() != "mysqlite-corpus: skipped 1 malformed journal lines\n" {
		t.Errorf("stderr %q", errOut.String())
	}
}

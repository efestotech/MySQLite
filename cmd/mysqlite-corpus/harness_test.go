package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mysqlite/internal/dialect/udf"
	"mysqlite/internal/engine"
	"mysqlite/internal/wire"
)

// mysqliteAt starts a mysqlite server with user u/p and returns its address.
func mysqliteAt(t *testing.T) string {
	t.Helper()
	eng, err := engine.Open(engine.Options{
		DataDir: filepath.Join(t.TempDir(), "data"), MaxReadConns: 2, BusyTimeout: time.Second, Synchronous: "NORMAL",
	}, udf.Extensions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	srv, err := wire.NewServer(wire.Config{
		ServerVersion: "8.0.36-mysqlite", MaxAllowedPacket: 64 << 20,
		Users: map[string]string{"u": "p"}, Engine: eng,
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
	return ln.Addr().String()
}

const harnessCorpus = `cases:
  - id: h.select.001
    milestone: m6
    setup:
      - CREATE TABLE t (id INT PRIMARY KEY, v VARCHAR(10))
      - INSERT INTO t VALUES (1, 'a'), (2, NULL)
    sql: SELECT id, v FROM t
    expect:
      types: [LONG, VAR_STRING]
      rows: [["1", a], ["2", null]]
`

// harness runs a corpus on two servers, prints a summary, writes the JSON report and
// the golden outcomes, and exits 0 when nothing fails; a wrong expectation exits 1.
func TestHarnessCommand(t *testing.T) {
	ref, sut := mysqliteAt(t), mysqliteAt(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "h.yaml"), []byte(harnessCorpus), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	report, golden := filepath.Join(out, "report.json"), filepath.Join(out, "golden")
	var stdout, stderr bytes.Buffer
	code := run([]string{"harness", "-ref", "u:p@" + ref, "-sut", "u:p@" + sut, "-json", report, "-golden", golden, dir}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "1 cases: 1 passed, 0 failed, 0 deviated, 0 unsupported; 0 expectations disagree") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	var rep struct {
		Passed  int `json:"passed"`
		Results []struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			Reference struct {
				Types []string `json:"types"`
				Rows  []string `json:"rows"`
			} `json:"reference"`
		} `json:"results"`
	}
	b, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Passed != 1 || len(rep.Results) != 1 || rep.Results[0].Status != "passed" ||
		strings.Join(rep.Results[0].Reference.Rows, " ") != `("1", "a") ("2", NULL)` {
		t.Errorf("report: %s", b)
	}
	if _, err := os.Stat(filepath.Join(golden, "h.select.001.json")); err != nil {
		t.Error(err)
	}

	// An expectation the reference contradicts fails the run.
	wrong := strings.Replace(harnessCorpus, `["1", a]`, `["1", b]`, 1)
	if err := os.WriteFile(filepath.Join(dir, "h.yaml"), []byte(wrong), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := run([]string{"harness", "-ref", "u:p@" + ref, "-sut", "u:p@" + sut, dir}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stdout.String(), "EXPECTATION h.select.001") {
		t.Errorf("wrong expectation: exit %d, stdout %s", code, stdout.String())
	}
}

func TestHarnessUsage(t *testing.T) {
	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"harness"}, 2, "usage:"},
		{[]string{"harness", "-ref", "u:p@h:1", "dir"}, 2, "usage:"},
		{[]string{"harness", "-ref", "nohost", "-sut", "u:p@h:1", "dir"}, 2, "want user:password@host:port"},
		{[]string{"harness", "-ref", "u:p@h:1", "-sut", "u:p@h", "dir"}, 2, "want user:password@host:port"},
		{[]string{"harness", "-ref", "u:p@h:1", "-sut", "u:p@h:1", t.TempDir()}, 1, "no corpus case selected"},
	} {
		var out, errOut bytes.Buffer
		if code := run(c.args, &out, &errOut); code != c.code || !strings.Contains(errOut.String(), c.msg) {
			t.Errorf("%v: exit %d, stderr %q; want %d with %q", c.args, code, errOut.String(), c.code, c.msg)
		}
	}
}

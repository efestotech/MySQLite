package corpus

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/engine"
	"mysqlite/internal/wire"
)

const (
	corpusUser     = "corpus"
	corpusPassword = "corpus"
)

// inProcess is a Target running mysqlite in the test process: fixtures are loaded
// straight into SQLite (SQLiteFixture), statements go through the MySQL protocol.
type inProcess struct {
	eng  *engine.Engine
	addr string
	n    int
}

func startMysqlite(t *testing.T) *inProcess {
	t.Helper()
	eng, err := engine.Open(engine.Options{
		DataDir:      filepath.Join(t.TempDir(), "data"),
		MaxReadConns: 2,
		BusyTimeout:  time.Second,
		Synchronous:  "NORMAL",
	}, engine.Extensions{})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	srv, err := wire.NewServer(wire.Config{
		ServerVersion:    "8.0.36-mysqlite",
		MaxAllowedPacket: 64 << 20,
		Users:            map[string]string{corpusUser: corpusPassword},
		Engine:           eng,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &inProcess{eng: eng, addr: ln.Addr().String()}
}

func (p *inProcess) Prepare(ctx context.Context, c *Case) (string, error) {
	p.n++
	name := fmt.Sprintf("case%04d", p.n)
	if err := p.eng.CreateDatabase(name); err != nil {
		return "", err
	}
	db, err := p.eng.Database(name)
	if err != nil {
		return "", err
	}
	w, err := db.Writer(ctx)
	if err != nil {
		return "", err
	}
	defer w.Close()
	for _, s := range c.Setup {
		if _, err := w.ExecContext(ctx, SQLiteFixture(s)); err != nil {
			return "", fmt.Errorf("%q: %w", s, err)
		}
	}
	return name, nil
}

// Dial sends CLIENT_SESSION_TRACK like libmysqlclient, without which go-mysql leaves
// out the OK info string (ADR 0003, G1).
func (p *inProcess) Dial(db string, foundRows bool) (*client.Conn, error) {
	return client.Connect(p.addr, corpusUser, corpusPassword, db, func(c *client.Conn) error {
		flags := []uint32{mysql.CLIENT_SESSION_TRACK}
		if foundRows {
			flags = append(flags, mysql.CLIENT_FOUND_ROWS)
		}
		for _, f := range flags {
			if err := c.SetCapability(f); err != nil {
				return err
			}
		}
		return nil
	})
}

// TestCorpusM1 runs corpus subset m1 against mysqlite (M1 criterion 1). Its
// expectations are `oracle: pending` until mysql-ref runs them (ADR 0002).
func TestCorpusM1(t *testing.T) {
	cases, err := Load(filepath.Join("..", "..", "test", "corpus"), "m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("corpus subset m1 is empty")
	}
	rep := Run(context.Background(), startMysqlite(t), cases)
	for _, r := range rep.Results {
		switch r.Status {
		case Failed:
			t.Errorf("%s (%s): %s", r.Case.ID, r.Case.File, r.Detail)
		case Deviated:
			t.Logf("%s deviated: %s (%s)", r.Case.ID, r.Case.KnownDeviation.Reason, r.Case.KnownDeviation.Ref)
		}
	}
	t.Log(rep.Summary())
}

// The runner must catch a wrong expectation in every compared key, or a green corpus
// means nothing.
func TestRunnerReportsEveryMismatch(t *testing.T) {
	u := func(n uint64) *uint64 { return &n }
	s := func(v string) *string { return &v }
	setup := []string{
		"CREATE TABLE t (id INTEGER PRIMARY KEY AUTO_INCREMENT, name VARCHAR(10))",
		"INSERT INTO t (name) VALUES ('a'), (NULL)",
	}
	sel := "SELECT id, name FROM t ORDER BY id"
	cases := []Case{
		{ID: "runner.ok.001", Setup: setup, SQL: sel, Expect: Expect{
			Columns: []string{"id", "name"}, Types: []string{"LONG", "VAR_STRING"},
			Rows: &[][]*string{{s("1"), s("a")}, {s("2"), nil}}, Ordered: true}},
		{ID: "runner.ok.002", Setup: setup, SQL: sel, Expect: Expect{
			Rows: &[][]*string{{s("2"), nil}, {s("1"), s("a")}}}},
		{ID: "runner.columns.001", Setup: setup, SQL: sel, Expect: Expect{Columns: []string{"id", "nom"}}},
		{ID: "runner.types.001", Setup: setup, SQL: sel, Expect: Expect{Types: []string{"LONGLONG", "VAR_STRING"}}},
		{ID: "runner.null.001", Setup: setup, SQL: sel, Expect: Expect{
			Rows: &[][]*string{{s("1"), s("a")}, {s("2"), s("NULL")}}}},
		{ID: "runner.order.001", Setup: setup, SQL: sel, Expect: Expect{
			Rows: &[][]*string{{s("2"), nil}, {s("1"), s("a")}}, Ordered: true}},
		{ID: "runner.affected.001", Setup: setup, SQL: "DELETE FROM t", Expect: Expect{AffectedRows: u(1)}},
		{ID: "runner.insertid.001", Setup: setup, SQL: "INSERT INTO t (name) VALUES ('b')", Expect: Expect{InsertID: u(4)}},
		{ID: "runner.info.001", Setup: setup, SQL: "UPDATE t SET name = 'z'", Expect: Expect{Info: s("Rows matched: 2  Changed: 1  Warnings: 0")}},
		{ID: "runner.error.001", Setup: setup, SQL: "SELECT * FROM missing", Expect: Expect{Error: &Error{Code: 1146, SQLState: "42000"}}},
		{ID: "runner.error.002", Setup: setup, SQL: sel, Expect: Expect{Error: &Error{Code: 1146, SQLState: "42S02"}}},
		{ID: "runner.setup.001", Setup: []string{"CREATE TABLE"}, SQL: sel, Expect: Expect{AffectedRows: u(0)}},
		{ID: "runner.session.001", Setup: setup, Session: []string{"SELECT nope FROM t"}, SQL: sel, Expect: Expect{AffectedRows: u(0)}},
		{ID: "runner.deviation.001", Setup: setup, SQL: sel, Expect: Expect{Columns: []string{"id", "name"}},
			KnownDeviation: &Deviation{Reason: "r", Ref: "x"}},
	}
	rep := Run(context.Background(), startMysqlite(t), cases)
	for _, r := range rep.Results {
		want := Failed
		switch r.Case.ID {
		case "runner.ok.001", "runner.ok.002":
			want = Passed
		case "runner.deviation.001":
			want = Deviated
		}
		if r.Status != want {
			t.Errorf("%s: want %s, got %s (%s)", r.Case.ID, want, r.Status, r.Detail)
		}
	}
	if rep.Passed != 2 || rep.Deviated != 1 || rep.Failed != len(cases)-3 {
		t.Errorf("counts: %s", rep.Summary())
	}
}

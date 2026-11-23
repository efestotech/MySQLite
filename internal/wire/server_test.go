package wire

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/dialect/udf"
	"mysqlite/internal/engine"
)

const (
	testUser     = "test"
	testPassword = "test"
)

// startServer runs a wire server on a random loopback port and returns its address.
func startServer(t *testing.T, users map[string]string) string {
	t.Helper()
	return startServerWith(t, Config{
		ServerVersion:    "8.0.36-mysqlite",
		MaxAllowedPacket: 64 << 20,
		Users:            users,
	})
}

// newEngine opens an engine on a temporary data directory with one database, "shop".
func newEngine(t *testing.T) *engine.Engine {
	t.Helper()
	e, err := engine.Open(engine.Options{
		DataDir:      filepath.Join(t.TempDir(), "data"),
		MaxReadConns: 1,
		BusyTimeout:  50 * time.Millisecond,
		Synchronous:  "NORMAL",
	}, udf.Extensions())
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.CreateDatabase("shop"); err != nil {
		t.Fatalf("create shop: %v", err)
	}
	return e
}

func startServerWith(t *testing.T, cfg Config) string {
	t.Helper()
	if cfg.Engine == nil {
		cfg.Engine = newEngine(t)
	}
	srv, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return ln.Addr().String()
}

func connect(t *testing.T, addr, user, password string) *client.Conn {
	t.Helper()
	conn, err := client.Connect(addr, user, password, "")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func requireMyError(t *testing.T, err error, code uint16, state string) *mysql.MyError {
	t.Helper()
	var myErr *mysql.MyError
	if !errors.As(err, &myErr) {
		t.Fatalf("want MySQL error %d, got %v", code, err)
	}
	if myErr.Code != code || myErr.State != state {
		t.Fatalf("want %d/%s, got %d/%s (%s)", code, state, myErr.Code, myErr.State, myErr.Message)
	}
	return myErr
}

func TestSelectOneReturnsOne(t *testing.T) {
	addr := startServer(t, map[string]string{testUser: testPassword})
	conn := connect(t, addr, testUser, testPassword)

	res, err := conn.Execute("SELECT 1")
	if err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	got, err := res.GetInt(0, 0)
	if err != nil {
		t.Fatalf("GetInt: %v", err)
	}
	if got != 1 {
		t.Fatalf("want 1, got %d", got)
	}
	if name := string(res.Fields[0].Name); name != "1" {
		t.Fatalf("want column name %q, got %q", "1", name)
	}
	if !conn.IsAutoCommit() {
		t.Fatal("want SERVER_STATUS_AUTOCOMMIT set")
	}
}

func TestServerVersionIsAdvertised(t *testing.T) {
	addr := startServer(t, map[string]string{testUser: testPassword})
	conn := connect(t, addr, testUser, testPassword)

	if v := conn.GetServerVersion(); v != "8.0.36-mysqlite" {
		t.Fatalf("want 8.0.36-mysqlite, got %q", v)
	}
}

func TestPing(t *testing.T) {
	addr := startServer(t, map[string]string{testUser: testPassword})
	conn := connect(t, addr, testUser, testPassword)

	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestWrongPasswordIsAccessDenied(t *testing.T) {
	addr := startServer(t, map[string]string{testUser: testPassword})

	_, err := client.Connect(addr, testUser, "wrong", "")
	requireMyError(t, err, mysql.ER_ACCESS_DENIED_ERROR, "28000")
}

func TestUnknownUserIsAccessDenied(t *testing.T) {
	addr := startServer(t, map[string]string{testUser: testPassword})

	_, err := client.Connect(addr, "nobody", testPassword, "")
	requireMyError(t, err, mysql.ER_ACCESS_DENIED_ERROR, "28000")
}

func TestEmptyPasswordUser(t *testing.T) {
	addr := startServer(t, map[string]string{"anon": ""})

	conn := connect(t, addr, "anon", "")
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	_, err := client.Connect(addr, "anon", "secret", "")
	requireMyError(t, err, mysql.ER_ACCESS_DENIED_ERROR, "28000")
}

func TestUnsupportedQueryIsError(t *testing.T) {
	addr := startServer(t, map[string]string{testUser: testPassword})
	conn := connect(t, addr, testUser, testPassword)

	_, err := conn.Execute("SELECT 1 INTERSECT ALL SELECT 2")
	requireMyError(t, err, mysql.ER_NOT_SUPPORTED_YET, "42000")

	// The connection must stay usable after an error.
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping after error: %v", err)
	}
}

func TestOversizedQueryIsPacketTooLarge(t *testing.T) {
	addr := startServerWith(t, Config{
		ServerVersion:    "8.0.36-mysqlite",
		MaxAllowedPacket: 1024,
		Users:            map[string]string{testUser: testPassword},
	})
	conn := connect(t, addr, testUser, testPassword)

	_, err := conn.Execute("SELECT '" + strings.Repeat("x", 2048) + "'")
	requireMyError(t, err, mysql.ER_NET_PACKET_TOO_LARGE, "08S01")
}

func TestUnknownDatabaseAtConnect(t *testing.T) {
	addr := startServer(t, map[string]string{testUser: testPassword})

	_, err := client.Connect(addr, testUser, testPassword, "missing")
	myErr := requireMyError(t, err, mysql.ER_BAD_DB_ERROR, "42000")
	if myErr.Message != "Unknown database 'missing'" {
		t.Fatalf("unexpected message %q", myErr.Message)
	}
}

func TestServerVersionMustBeNumeric(t *testing.T) {
	_, err := NewServer(Config{
		ServerVersion:    "eight-mysqlite",
		MaxAllowedPacket: 64 << 20,
		Users:            map[string]string{testUser: testPassword},
		Engine:           newEngine(t),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("want an error for a server version that is not major.minor.patch")
	}
}

func TestCapabilities(t *testing.T) {
	srv, err := NewServer(Config{
		ServerVersion:    "8.0.36-mysqlite",
		MaxAllowedPacket: 64 << 20,
		Users:            map[string]string{testUser: testPassword},
		Engine:           newEngine(t),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	caps := srv.Capability()
	forbidden := map[string]uint32{
		"CLIENT_SSL":              mysql.CLIENT_SSL,
		"CLIENT_COMPRESS":         mysql.CLIENT_COMPRESS,
		"CLIENT_LOCAL_FILES":      mysql.CLIENT_LOCAL_FILES,
		"CLIENT_DEPRECATE_EOF":    mysql.CLIENT_DEPRECATE_EOF,
	}
	for name, flag := range forbidden {
		if caps&flag != 0 {
			t.Errorf("%s must not be advertised", name)
		}
	}
	required := mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_MULTI_STATEMENTS |
		mysql.CLIENT_MULTI_RESULTS | mysql.CLIENT_PS_MULTI_RESULTS
	if caps&required != required {
		t.Errorf("missing flags %#x", required&^caps)
	}
}

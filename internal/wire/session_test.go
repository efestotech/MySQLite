package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

func TestConnectWithDatabase(t *testing.T) {
	addr := startServer(t, map[string]string{testUser: testPassword})

	conn, err := client.Connect(addr, testUser, testPassword, "shop")
	if err != nil {
		t.Fatalf("connect with shop: %v", err)
	}
	defer conn.Close()
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestInitDBSwitchesDatabase(t *testing.T) {
	addr := startServer(t, map[string]string{testUser: testPassword})
	conn := connect(t, addr, testUser, testPassword)

	if err := conn.UseDB("shop"); err != nil {
		t.Fatalf("COM_INIT_DB shop: %v", err)
	}
	err := conn.UseDB("missing")
	myErr := requireMyError(t, err, mysql.ER_BAD_DB_ERROR, "42000")
	if myErr.Message != "Unknown database 'missing'" {
		t.Fatalf("unexpected message %q", myErr.Message)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("connection must stay usable after 1049: %v", err)
	}
}

func TestInitDBEmptyNameIsNoDatabaseSelected(t *testing.T) {
	addr := startServer(t, map[string]string{testUser: testPassword})
	conn := connect(t, addr, testUser, testPassword)

	requireMyError(t, conn.UseDB(""), mysql.ER_NO_DB_ERROR, "3D000")
}

// The handshake database is read before the password is checked (W2). A client with a
// wrong password must get 1045 whether or not the database exists, so it cannot probe
// database names without credentials.
func TestHandshakeDatabaseIsNotProbedBeforeAuth(t *testing.T) {
	addr := startServer(t, map[string]string{testUser: testPassword})

	for _, db := range []string{"missing", "shop"} {
		_, err := client.Connect(addr, testUser, "wrong", db)
		requireMyError(t, err, mysql.ER_ACCESS_DENIED_ERROR, "28000")
	}
}

// MySQL prints the client host without the port (docs/agent-notes/progress.md, M0 note).
func TestAccessDeniedMessageShowsHostOnly(t *testing.T) {
	addr := startServer(t, map[string]string{testUser: testPassword})

	cases := []struct {
		user, password, msg string
	}{
		{testUser, "wrong", "Access denied for user 'test'@'127.0.0.1' (using password: YES)"},
		{testUser, "", "Access denied for user 'test'@'127.0.0.1' (using password: NO)"},
		{"nobody", "x", "Access denied for user 'nobody'@'127.0.0.1' (using password: YES)"},
	}
	for _, tc := range cases {
		_, err := client.Connect(addr, tc.user, tc.password, "")
		myErr := requireMyError(t, err, mysql.ER_ACCESS_DENIED_ERROR, "28000")
		if myErr.Message != tc.msg {
			t.Errorf("want %q, got %q", tc.msg, myErr.Message)
		}
	}
}

func TestUseStatement(t *testing.T) {
	addr := startServer(t, map[string]string{testUser: testPassword})
	conn := connect(t, addr, testUser, testPassword)

	if _, err := conn.Execute("USE shop"); err != nil {
		t.Fatalf("USE shop: %v", err)
	}
	_, err := conn.Execute("USE `missing`")
	myErr := requireMyError(t, err, mysql.ER_BAD_DB_ERROR, "42000")
	if myErr.Message != "Unknown database 'missing'" {
		t.Fatalf("unexpected message %q", myErr.Message)
	}
}

func TestQueriesGoThroughTheParser(t *testing.T) {
	addr := startServer(t, map[string]string{testUser: testPassword})
	conn := connect(t, addr, testUser, testPassword)

	_, err := conn.Execute("SELEC 1")
	myErr := requireMyError(t, err, mysql.ER_PARSE_ERROR, "42000")
	const want = "You have an error in your SQL syntax; check the manual that corresponds to your MySQL server version for the right syntax to use near 'SELEC 1' at line 1"
	if myErr.Message != want {
		t.Fatalf("unexpected message %q", myErr.Message)
	}
	_, err = conn.Execute("SELECT * FROM t")
	requireMyError(t, err, mysql.ER_NO_DB_ERROR, "3D000")

	// Whitespace and the trailing ';' are the parser's business.
	res, err := conn.Execute("select  1 ;")
	if err != nil {
		t.Fatalf("select 1: %v", err)
	}
	if got, _ := res.GetInt(0, 0); got != 1 {
		t.Fatalf("want 1, got %d", got)
	}
}

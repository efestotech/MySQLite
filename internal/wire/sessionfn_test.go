package wire

import (
	"strconv"
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// Session functions (docs/05-dialect.md sections 5 and 6). `oracle: pending`.
func TestSessionFunctions(t *testing.T) {
	_, addr := shopSetup(t, "CREATE TABLE seq (id INTEGER PRIMARY KEY AUTOINCREMENT, v INT)")
	conn := dial(t, addr)
	one := func(q string) string {
		t.Helper()
		v, _ := execOK(t, conn, q).GetString(0, 0)
		return v
	}

	res := execOK(t, conn, "SELECT LAST_INSERT_ID(), ROW_COUNT(), CONNECTION_ID()")
	for i, want := range []struct {
		typ      uint8
		unsigned bool
	}{{mysql.MYSQL_TYPE_LONGLONG, true}, {mysql.MYSQL_TYPE_LONGLONG, false}, {mysql.MYSQL_TYPE_LONGLONG, true}} {
		f := res.Fields[i]
		if f.Type != want.typ || (f.Flag&mysql.UNSIGNED_FLAG != 0) != want.unsigned {
			t.Errorf("column %d: type %d flags %#x", i, f.Type, f.Flag)
		}
	}
	if v, _ := res.GetString(0, 0); v != "0" {
		t.Errorf("LAST_INSERT_ID() before any insert: %q", v)
	}
	if v, _ := res.GetString(0, 2); v != strconv.Itoa(int(conn.GetConnectionID())) {
		t.Errorf("CONNECTION_ID(): %q, want %d", v, conn.GetConnectionID())
	}

	// A multi-row INSERT reports its first generated id, in the OK packet and after.
	r := execOK(t, conn, "INSERT INTO seq (v) VALUES (1), (2), (3)")
	if r.InsertId != 1 {
		t.Errorf("OK insert id %d", r.InsertId)
	}
	if v := one("SELECT ROW_COUNT()"); v != "3" {
		t.Errorf("ROW_COUNT() after INSERT: %q", v)
	}
	// The SELECT itself set ROW_COUNT() to -1.
	if v := one("SELECT ROW_COUNT()"); v != "-1" {
		t.Errorf("ROW_COUNT() after SELECT: %q", v)
	}
	if v := one("SELECT LAST_INSERT_ID()"); v != "1" {
		t.Errorf("LAST_INSERT_ID() after multi-row INSERT: %q", v)
	}
	// An explicit id does not change it; the next generated one does.
	execOK(t, conn, "INSERT INTO seq (id, v) VALUES (10, 1)")
	if v := one("SELECT LAST_INSERT_ID()"); v != "1" {
		t.Errorf("LAST_INSERT_ID() after explicit id: %q", v)
	}
	execOK(t, conn, "INSERT INTO seq (v) VALUES (5)")
	if v := one("SELECT LAST_INSERT_ID()"); v != "11" {
		t.Errorf("LAST_INSERT_ID() after generated id: %q", v)
	}

	// ROW_COUNT() is rows changed for UPDATE, affected for DELETE, -1 after an error.
	execOK(t, conn, "UPDATE seq SET v = 9 WHERE id = 1")
	if v := one("SELECT ROW_COUNT()"); v != "1" {
		t.Errorf("ROW_COUNT() after UPDATE: %q", v)
	}
	execOK(t, conn, "UPDATE seq SET v = 9 WHERE id = 1")
	if v := one("SELECT ROW_COUNT()"); v != "0" {
		t.Errorf("ROW_COUNT() after a no-op UPDATE: %q", v)
	}
	execOK(t, conn, "DELETE FROM seq WHERE id > 100")
	if v := one("SELECT ROW_COUNT()"); v != "0" {
		t.Errorf("ROW_COUNT() after DELETE: %q", v)
	}
	if _, err := conn.Execute("INSERT INTO seq (id, v) VALUES (1, 1)"); err == nil {
		t.Fatal("duplicate INSERT succeeded")
	}
	if v := one("SELECT ROW_COUNT()"); v != "-1" {
		t.Errorf("ROW_COUNT() after an error: %q", v)
	}

	// LAST_INSERT_ID(expr) returns expr and sets the value, also from an UPDATE, whose
	// OK packet carries it.
	if v := one("SELECT LAST_INSERT_ID(42)"); v != "42" {
		t.Errorf("LAST_INSERT_ID(42): %q", v)
	}
	if v := one("SELECT LAST_INSERT_ID()"); v != "42" {
		t.Errorf("LAST_INSERT_ID() after LAST_INSERT_ID(42): %q", v)
	}
	r = execOK(t, conn, "UPDATE seq SET v = LAST_INSERT_ID(v + 1) WHERE id = 1")
	if r.InsertId != 10 {
		t.Errorf("UPDATE ... LAST_INSERT_ID(v + 1): OK insert id %d", r.InsertId)
	}
	if v := one("SELECT LAST_INSERT_ID()"); v != "10" {
		t.Errorf("LAST_INSERT_ID() after the UPDATE: %q", v)
	}
	if v := one("SELECT v FROM seq WHERE id = 1"); v != "10" {
		t.Errorf("v after the UPDATE: %q", v)
	}

	// Accounts and version.
	res = execOK(t, conn, "SELECT USER(), CURRENT_USER(), SESSION_USER(), SYSTEM_USER(), VERSION()")
	for i, want := range []string{testUser + "@localhost", testUser + "@%", testUser + "@localhost", testUser + "@localhost", "8.0.36-mysqlite"} {
		if v, _ := res.GetString(0, i); v != want {
			t.Errorf("column %d: %q, want %q", i, v, want)
		}
		if res.Fields[i].Type != mysql.MYSQL_TYPE_VAR_STRING {
			t.Errorf("column %d: type %d", i, res.Fields[i].Type)
		}
	}
	if v := one("SELECT CURRENT_USER"); v != testUser+"@%" {
		t.Errorf("CURRENT_USER without parentheses: %q", v)
	}

	// Each connection has its own values.
	other := dial(t, addr)
	if v, _ := execOK(t, other, "SELECT LAST_INSERT_ID()").GetString(0, 0); v != "0" {
		t.Errorf("LAST_INSERT_ID() on another connection: %q", v)
	}
}

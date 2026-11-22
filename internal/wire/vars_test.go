package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// System variable reads (docs/04-session-catalog.md section 1). Values, names and
// column types are `oracle: pending`.
func TestSystemVariableReads(t *testing.T) {
	conn := shopServer(t)

	res := execOK(t, conn, "SELECT @@version, @@session.sql_mode, @@global.max_allowed_packet, @@autocommit, @@character_set_results")
	want := []struct {
		name  string
		typ   byte
		value string
	}{
		{"@@version", mysql.MYSQL_TYPE_VAR_STRING, "8.0.36-mysqlite"},
		{"@@session.sql_mode", mysql.MYSQL_TYPE_VAR_STRING, "ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION"},
		{"@@global.max_allowed_packet", mysql.MYSQL_TYPE_LONGLONG, "67108864"},
		{"@@autocommit", mysql.MYSQL_TYPE_LONGLONG, "1"},
		{"@@character_set_results", mysql.MYSQL_TYPE_VAR_STRING, "utf8mb4"},
	}
	if len(res.Fields) != len(want) || res.RowNumber() != 1 {
		t.Fatalf("want 1 row of %d columns, got %d rows of %d", len(want), res.RowNumber(), len(res.Fields))
	}
	for i, w := range want {
		f := res.Fields[i]
		if string(f.Name) != w.name || f.Type != w.typ {
			t.Errorf("column %d: want %s type %d, got %s type %d", i, w.name, w.typ, f.Name, f.Type)
		}
		if got, _ := res.GetString(0, i); got != w.value {
			t.Errorf("%s: want %q, got %q", w.name, w.value, got)
		}
	}

	// Connector/J reads many variables in one statement, with aliases.
	res = execOK(t, conn, "SELECT @@session.auto_increment_increment AS auto_increment_increment, @@character_set_client AS character_set_client, @@sql_select_limit AS sql_select_limit")
	if got := string(res.Fields[1].Name); got != "character_set_client" {
		t.Errorf("alias: got %q", got)
	}
	if res.Fields[2].Flag&mysql.UNSIGNED_FLAG == 0 {
		t.Error("sql_select_limit is unsigned")
	}
	if got, _ := res.GetString(0, 2); got != "18446744073709551615" {
		t.Errorf("sql_select_limit: got %q", got)
	}

	// A variable is a value like any other: it works in WHERE.
	res = execOK(t, conn, "SELECT name FROM items WHERE @@autocommit = 1 AND id = 1")
	if res.RowNumber() != 1 {
		t.Errorf("WHERE @@autocommit = 1: want 1 row, got %d", res.RowNumber())
	}

	// In writes too: the UPDATE change predicate reads the same value, so a second run
	// changes nothing.
	requireOK(t, conn, "UPDATE items SET note = @@version_comment WHERE id = 1", 1, 0, "Rows matched: 1  Changed: 1  Warnings: 0")
	requireOK(t, conn, "UPDATE items SET note = @@version_comment WHERE id = 1", 0, 0, "Rows matched: 1  Changed: 0  Warnings: 0")
	requireOK(t, conn, "INSERT INTO items (name) VALUES (@@version_comment)", 1, 3, "")
	if got, _ := execOK(t, conn, "SELECT name FROM items WHERE id = 3").GetString(0, 0); got != "mysqlite" {
		t.Errorf("INSERT ... VALUES (@@version_comment): got %q", got)
	}

	// BEGIN does not change @@autocommit.
	execOK(t, conn, "BEGIN")
	if got, _ := execOK(t, conn, "SELECT @@autocommit").GetInt(0, 0); got != 1 {
		t.Errorf("@@autocommit inside BEGIN: got %d", got)
	}
	execOK(t, conn, "ROLLBACK")
}

func TestSystemVariableErrors(t *testing.T) {
	conn := shopServer(t)
	cases := []struct {
		query, state, msg string
		code              uint16
	}{
		{"SELECT @@nope", "HY000", "Unknown system variable 'nope'", 1193},
		{"SELECT @@tx_isolation", "HY000", "Unknown system variable 'tx_isolation'", 1193},
		{"SELECT name FROM items WHERE @@nope = 1", "HY000", "Unknown system variable 'nope'", 1193},
		{"SELECT @@session.version", "HY000", "Variable 'version' is a GLOBAL variable", 1238},
		{"SELECT @@global.timestamp", "HY000", "Variable 'timestamp' is a SESSION variable", 1238},
	}
	for _, c := range cases {
		_, err := conn.Execute(c.query)
		myErr := requireMyError(t, err, c.code, c.state)
		if myErr.Message != c.msg {
			t.Errorf("%s: want %q, got %q", c.query, c.msg, myErr.Message)
		}
	}
	// User variables arrive later; they are refused, not guessed.
	_, err := conn.Execute("SELECT @x")
	requireMyError(t, err, 1235, "42000")
}

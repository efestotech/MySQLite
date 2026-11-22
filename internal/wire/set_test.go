package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// SET and diagnostics (docs/04-session-catalog.md section 1). Messages, values and
// column types are `oracle: pending`.

func varText(t *testing.T, conn *client.Conn, name string) string {
	t.Helper()
	s, err := execOK(t, conn, "SELECT @@"+name).GetString(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSetVariables(t *testing.T) {
	conn := shopServer(t)

	execOK(t, conn, "SET sql_mode = 'ansi_quotes,strict_all_tables'")
	if got := varText(t, conn, "sql_mode"); got != "ANSI_QUOTES,STRICT_ALL_TABLES" {
		t.Errorf("sql_mode: got %q", got)
	}
	// The parser follows the mode: "name" is an identifier now.
	if got, _ := execOK(t, conn, `SELECT "name" FROM items WHERE id = 1`).GetString(0, 0); got != "pen" {
		t.Errorf(`ANSI_QUOTES: SELECT "name": got %q`, got)
	}
	execOK(t, conn, "SET sql_mode = 'ANSI'")
	if got := varText(t, conn, "sql_mode"); got != "REAL_AS_FLOAT,PIPES_AS_CONCAT,ANSI_QUOTES,IGNORE_SPACE,ONLY_FULL_GROUP_BY,ANSI" {
		t.Errorf("sql_mode ANSI: got %q", got)
	}
	execOK(t, conn, "SET @@session.sql_mode = DEFAULT")
	if got := varText(t, conn, "sql_mode"); got != "ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION" {
		t.Errorf("sql_mode DEFAULT: got %q", got)
	}

	execOK(t, conn, "SET SESSION group_concat_max_len = 2048, unique_checks = OFF")
	if got := varText(t, conn, "group_concat_max_len"); got != "2048" {
		t.Errorf("group_concat_max_len: got %q", got)
	}
	if got := varText(t, conn, "unique_checks"); got != "0" {
		t.Errorf("unique_checks: got %q", got)
	}
	// A variable can take another's value.
	execOK(t, conn, "SET character_set_results = @@character_set_client")
	// NULL asks for results without conversion (JDBC drivers send it).
	execOK(t, conn, "SET character_set_results = NULL")
	if v, _ := execOK(t, conn, "SELECT @@character_set_results").GetValue(0, 0); v != nil {
		t.Errorf("character_set_results = NULL: got %v", v)
	}

	// A failing SET applies none of its assignments.
	_, err := conn.Execute("SET autocommit = 0, @@nope = 1")
	requireMyError(t, err, 1193, "HY000")
	if got := varText(t, conn, "autocommit"); got != "1" {
		t.Errorf("failed SET changed autocommit to %q", got)
	}

	// SET GLOBAL is seen by new connections, not by the open ones.
	execOK(t, conn, "SET GLOBAL group_concat_max_len = 4096")
	if got := varText(t, conn, "group_concat_max_len"); got != "2048" {
		t.Errorf("session after SET GLOBAL: got %q", got)
	}
	if got := varText(t, conn, "global.group_concat_max_len"); got != "4096" {
		t.Errorf("@@global after SET GLOBAL: got %q", got)
	}
}

func TestSetErrors(t *testing.T) {
	conn := shopServer(t)
	cases := []struct {
		query, state, msg string
		code              uint16
	}{
		{"SET nope = 1", "HY000", "Unknown system variable 'nope'", 1193},
		{"SET version = 'x'", "HY000", "Variable 'version' is a read only variable", 1238},
		{"SET GLOBAL timestamp = 1", "HY000", "Variable 'timestamp' is a SESSION variable and can't be used with SET GLOBAL", 1228},
		{"SET SESSION net_buffer_length = 2048", "HY000", "SESSION variable 'net_buffer_length' is read-only. Use SET GLOBAL to assign the value", 1621},
		{"SET autocommit = 2", "42000", "Variable 'autocommit' can't be set to the value of '2'", 1231},
		{"SET autocommit = 'maybe'", "42000", "Variable 'autocommit' can't be set to the value of 'maybe'", 1231},
		{"SET wait_timeout = 'x'", "42000", "Incorrect argument type to variable 'wait_timeout'", 1232},
		{"SET sql_mode = 'NOPE'", "42000", "Variable 'sql_mode' can't be set to the value of 'NOPE'", 1231},
		{"SET sql_mode = NULL", "42000", "Variable 'sql_mode' can't be set to the value of 'NULL'", 1231},
		{"SET NAMES nope", "42000", "Unknown character set: 'nope'", 1115},
		{"SET NAMES utf8mb4 COLLATE utf8mb3_bin", "42000", "COLLATION 'utf8mb3_bin' is not valid for CHARACTER SET 'utf8mb4'", 1253},
		{"SET collation_connection = 'nope'", "HY000", "Unknown collation: 'nope'", 1273},
		{"SET time_zone = 'Europe/Rome'", "HY000", "Unknown or incorrect time zone: 'Europe/Rome'", 1298},
	}
	for _, c := range cases {
		_, err := conn.Execute(c.query)
		myErr := requireMyError(t, err, c.code, c.state)
		if myErr.Message != c.msg {
			t.Errorf("%s: want %q, got %q", c.query, c.msg, myErr.Message)
		}
	}
	// Values mysqlite would not apply are refused, not ignored.
	for _, q := range []string{
		"SET NAMES latin1",
		"SET sql_select_limit = 10",
		"SET transaction_isolation = 'READ-UNCOMMITTED'",
		"SET @u = 1",
		"SET autocommit = 1 + 0",
	} {
		_, err := conn.Execute(q)
		requireMyError(t, err, 1235, "42000")
	}
}

func TestSetNames(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "SET NAMES utf8mb4 COLLATE utf8mb4_bin")
	for name, want := range map[string]string{
		"character_set_client":     "utf8mb4",
		"character_set_connection": "utf8mb4",
		"character_set_results":    "utf8mb4",
		"collation_connection":     "utf8mb4_bin",
	} {
		if got := varText(t, conn, name); got != want {
			t.Errorf("SET NAMES ... COLLATE: %s = %q, want %q", name, got, want)
		}
	}
	// utf8 is utf8mb3 in MySQL 8.0.
	execOK(t, conn, "SET NAMES utf8")
	if got := varText(t, conn, "character_set_connection"); got != "utf8mb3" {
		t.Errorf("SET NAMES utf8: got %q", got)
	}
	execOK(t, conn, "SET NAMES DEFAULT")
	if got := varText(t, conn, "collation_connection"); got != "utf8mb4_general_ci" {
		t.Errorf("SET NAMES DEFAULT: got %q", got)
	}
	execOK(t, conn, "SET CHARACTER SET utf8mb3")
	if got := varText(t, conn, "character_set_client"); got != "utf8mb3" {
		t.Errorf("SET CHARACTER SET: client %q", got)
	}
	if got := varText(t, conn, "character_set_connection"); got != "utf8mb4" {
		t.Errorf("SET CHARACTER SET: connection follows the database, got %q", got)
	}
}

func TestDiagnostics(t *testing.T) {
	conn := shopServer(t)

	// 0 is below the minimum: clamped with 1292, and a timeout mysqlite does not apply.
	res := execOK(t, conn, "SET wait_timeout = 0")
	if res.Warnings != 2 {
		t.Errorf("OK warnings: want 2, got %d", res.Warnings)
	}
	if got := varText(t, conn, "wait_timeout"); got != "1" {
		t.Errorf("clamped wait_timeout: got %q", got)
	}
	res = execOK(t, conn, "SHOW WARNINGS")
	if len(res.Fields) != 3 || string(res.Fields[0].Name) != "Level" || string(res.Fields[1].Name) != "Code" || string(res.Fields[2].Name) != "Message" {
		t.Fatalf("SHOW WARNINGS columns: %v", res.Fields)
	}
	if res.Fields[1].Type != mysql.MYSQL_TYPE_LONG || res.Fields[1].Flag&mysql.UNSIGNED_FLAG == 0 {
		t.Errorf("Code column: type %d flags %d", res.Fields[1].Type, res.Fields[1].Flag)
	}
	if res.RowNumber() != 2 {
		t.Fatalf("SHOW WARNINGS: want 2 rows, got %d", res.RowNumber())
	}
	level, _ := res.GetString(0, 0)
	code, _ := res.GetInt(0, 1)
	msg, _ := res.GetString(0, 2)
	if level != "Warning" || code != 1292 || msg != "Truncated incorrect wait_timeout value: '0'" {
		t.Errorf("first warning: %s %d %q", level, code, msg)
	}
	if code, _ := res.GetInt(1, 1); code != 1235 {
		t.Errorf("second warning: code %d", code)
	}

	// SHOW WARNINGS and table-less statements keep the list; COUNT(*) reads it.
	if n, _ := execOK(t, conn, "SHOW COUNT(*) WARNINGS").GetInt(0, 0); n != 2 {
		t.Errorf("SHOW COUNT(*) WARNINGS: got %d", n)
	}
	if n, _ := execOK(t, conn, "SELECT @@warning_count").GetInt(0, 0); n != 2 {
		t.Errorf("@@warning_count: got %d", n)
	}
	// A statement that uses a table resets it.
	execOK(t, conn, "SELECT name FROM items")
	if n, _ := execOK(t, conn, "SELECT @@warning_count").GetInt(0, 0); n != 0 {
		t.Errorf("@@warning_count after a table read: got %d", n)
	}

	// An error is an Error row.
	_, err := conn.Execute("SELECT nope FROM items")
	requireMyError(t, err, 1054, "42S22")
	res = execOK(t, conn, "SHOW ERRORS")
	if res.RowNumber() != 1 {
		t.Fatalf("SHOW ERRORS: want 1 row, got %d", res.RowNumber())
	}
	if level, _ := res.GetString(0, 0); level != "Error" {
		t.Errorf("SHOW ERRORS level: %q", level)
	}
	if n, _ := execOK(t, conn, "SHOW COUNT(*) ERRORS").GetInt(0, 0); n != 1 {
		t.Errorf("SHOW COUNT(*) ERRORS: got %d", n)
	}
	if n, _ := execOK(t, conn, "SELECT @@error_count").GetInt(0, 0); n != 1 {
		t.Errorf("@@error_count: got %d", n)
	}
	if string(execOK(t, conn, "SHOW COUNT(*) ERRORS").Fields[0].Name) != "@@session.error_count" {
		t.Error("SHOW COUNT(*) ERRORS column name")
	}
}

func TestSetAutocommit(t *testing.T) {
	_, addr := shopSetup(t)
	a, b := dial(t, addr), dial(t, addr)

	res := execOK(t, a, "SET autocommit = 0")
	if res.Status&mysql.SERVER_STATUS_AUTOCOMMIT != 0 {
		t.Error("status still has AUTOCOMMIT")
	}
	if got := varText(t, a, "autocommit"); got != "0" {
		t.Errorf("@@autocommit: got %q", got)
	}
	// The first statement that uses a table starts a transaction.
	res = execOK(t, a, "INSERT INTO items (name) VALUES ('ink')")
	if res.Status&mysql.SERVER_STATUS_IN_TRANS == 0 {
		t.Error("INSERT with autocommit=0 did not start a transaction")
	}
	if n := execOK(t, b, "SELECT id FROM items WHERE name = 'ink'").RowNumber(); n != 0 {
		t.Error("uncommitted row visible to another connection")
	}
	execOK(t, a, "ROLLBACK")
	if n := execOK(t, a, "SELECT id FROM items WHERE name = 'ink'").RowNumber(); n != 0 {
		t.Error("ROLLBACK kept the row")
	}
	execOK(t, a, "ROLLBACK")

	// Turning autocommit on commits the open transaction.
	execOK(t, a, "INSERT INTO items (name) VALUES ('ink')")
	res = execOK(t, a, "SET autocommit = ON")
	if res.Status&mysql.SERVER_STATUS_IN_TRANS != 0 || res.Status&mysql.SERVER_STATUS_AUTOCOMMIT == 0 {
		t.Errorf("status after SET autocommit = ON: %b", res.Status)
	}
	if n := execOK(t, b, "SELECT id FROM items WHERE name = 'ink'").RowNumber(); n != 1 {
		t.Error("SET autocommit = 1 did not commit")
	}
}

func TestSetTransactionReadOnly(t *testing.T) {
	conn := shopServer(t)

	execOK(t, conn, "SET SESSION TRANSACTION READ ONLY")
	if got := varText(t, conn, "transaction_read_only"); got != "1" {
		t.Errorf("@@transaction_read_only: got %q", got)
	}
	_, err := conn.Execute("INSERT INTO items (name) VALUES ('ink')")
	requireMyError(t, err, 1792, "25006")

	// Without a scope, SET TRANSACTION applies to the next transaction only.
	execOK(t, conn, "SET TRANSACTION READ WRITE")
	execOK(t, conn, "BEGIN")
	execOK(t, conn, "INSERT INTO items (name) VALUES ('ink')")
	_, err = conn.Execute("SET TRANSACTION READ ONLY")
	requireMyError(t, err, 1568, "25001")
	execOK(t, conn, "COMMIT")
	execOK(t, conn, "BEGIN")
	_, err = conn.Execute("INSERT INTO items (name) VALUES ('ink')")
	requireMyError(t, err, 1792, "25006")
	execOK(t, conn, "ROLLBACK")

	execOK(t, conn, "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE")
	execOK(t, conn, "SET SESSION transaction_read_only = 0")
	execOK(t, conn, "INSERT INTO items (name) VALUES ('ink')")
}

func TestNoAutoValueOnZero(t *testing.T) {
	conn := shopServer(t)
	_, err := conn.Execute("INSERT INTO items (id, name) VALUES (0, 'zero')")
	requireMyError(t, err, 1235, "42000")
	execOK(t, conn, "SET sql_mode = 'NO_AUTO_VALUE_ON_ZERO,STRICT_TRANS_TABLES'")
	requireOK(t, conn, "INSERT INTO items (id, name) VALUES (0, 'zero')", 1, 0, "")
	if got, _ := execOK(t, conn, "SELECT name FROM items WHERE id = 0").GetString(0, 0); got != "zero" {
		t.Errorf("id 0: got %q", got)
	}
}

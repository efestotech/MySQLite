package wire

import (
	"strconv"
	"strings"
	"testing"
)

// Server SHOW statements (docs/04-session-catalog.md section 4). `oracle: pending`.

func TestShowVariables(t *testing.T) {
	conn := shopServer(t)
	res := execOK(t, conn, "SHOW VARIABLES LIKE 'autocommit'")
	if res.RowNumber() != 1 || column(t, res, 0, "Value") != "ON" {
		t.Fatalf("autocommit: %d rows", res.RowNumber())
	}
	execOK(t, conn, "SET autocommit = 0")
	res = execOK(t, conn, "SHOW SESSION VARIABLES WHERE Variable_name = 'AUTOCOMMIT'")
	if column(t, res, 0, "Value") != "OFF" {
		t.Errorf("session autocommit: %s", column(t, res, 0, "Value"))
	}
	res = execOK(t, conn, "SHOW GLOBAL VARIABLES LIKE 'autocommit'")
	if column(t, res, 0, "Value") != "ON" {
		t.Errorf("global autocommit: %s", column(t, res, 0, "Value"))
	}
	// Session-only variables are not global.
	if n := execOK(t, conn, "SHOW GLOBAL VARIABLES LIKE 'warning_count'").RowNumber(); n != 0 {
		t.Errorf("GLOBAL warning_count: %d rows", n)
	}
	res = execOK(t, conn, "SHOW VARIABLES LIKE 'character\\_set\\_%'")
	if res.RowNumber() < 5 {
		t.Errorf("character_set_%%: %d rows", res.RowNumber())
	}
	res = execOK(t, conn, "SHOW VARIABLES")
	names := firstColumn(res)
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("not sorted at %s", names[i])
		}
	}
}

func TestShowStatusAndProcessList(t *testing.T) {
	_, addr := shopSetup(t)
	a, b := dial(t, addr), dial(t, addr)
	execOK(t, b, "USE shop")

	res := execOK(t, a, "SHOW GLOBAL STATUS LIKE 'Threads_connected'")
	if column(t, res, 0, "Value") != "2" {
		t.Errorf("Threads_connected: %s", column(t, res, 0, "Value"))
	}
	res = execOK(t, a, "SHOW STATUS WHERE Variable_name IN ('Questions', 'Uptime')")
	if res.RowNumber() != 2 {
		t.Fatalf("SHOW STATUS WHERE: %d rows", res.RowNumber())
	}
	if q, _ := strconv.Atoi(column(t, res, 0, "Value")); q < 2 {
		t.Errorf("session Questions: %d", q)
	}

	res = execOK(t, a, "SHOW PROCESSLIST")
	if res.RowNumber() != 2 {
		t.Fatalf("SHOW PROCESSLIST: %d rows", res.RowNumber())
	}
	var self, other int = -1, -1
	for i := range res.RowNumber() {
		if column(t, res, i, "Command") == "Query" {
			self = i
		} else {
			other = i
		}
	}
	if self < 0 || other < 0 {
		t.Fatal("want one running and one sleeping connection")
	}
	if got := column(t, res, self, "Info"); got != "SHOW PROCESSLIST" {
		t.Errorf("own Info: %q", got)
	}
	if column(t, res, other, "Command") != "Sleep" || column(t, res, other, "Info") != "NULL" ||
		column(t, res, other, "db") != "shop" || column(t, res, other, "User") != testUser {
		t.Errorf("other connection: %s %s %s", column(t, res, other, "Command"), column(t, res, other, "Info"), column(t, res, other, "db"))
	}
	if id := column(t, res, self, "Id"); id != strconv.Itoa(int(a.GetConnectionID())) {
		t.Errorf("Id %s, connection id %d", id, a.GetConnectionID())
	}

	res = execOK(t, a, "SHOW FULL PROCESSLIST")
	if res.RowNumber() != 2 {
		t.Errorf("SHOW FULL PROCESSLIST: %d rows", res.RowNumber())
	}
	_ = b.Close()
}

func TestShowEnginesCharsetsCollations(t *testing.T) {
	conn := shopServer(t)
	res := execOK(t, conn, "SHOW ENGINES")
	if res.RowNumber() != 1 || column(t, res, 0, "Engine") != "SQLite" || column(t, res, 0, "Support") != "DEFAULT" {
		t.Errorf("SHOW ENGINES: %d rows", res.RowNumber())
	}
	res = execOK(t, conn, "SHOW CHARACTER SET LIKE 'utf8%'")
	if got := strings.Join(firstColumn(res), ","); got != "utf8mb3,utf8mb4" {
		t.Errorf("SHOW CHARACTER SET LIKE: %s", got)
	}
	if column(t, res, 1, "Default collation") != "utf8mb4_general_ci" || column(t, res, 1, "Maxlen") != "4" {
		t.Errorf("utf8mb4 row: %s %s", column(t, res, 1, "Default collation"), column(t, res, 1, "Maxlen"))
	}
	res = execOK(t, conn, "SHOW CHARSET WHERE Charset = 'binary'")
	if res.RowNumber() != 1 {
		t.Errorf("SHOW CHARSET WHERE: %d rows", res.RowNumber())
	}
	res = execOK(t, conn, "SHOW COLLATION WHERE Charset = 'utf8mb4' AND `Default` = 'Yes'")
	if got := strings.Join(firstColumn(res), ","); got != "utf8mb4_general_ci" {
		t.Errorf("default utf8mb4 collation: %s", got)
	}
	res = execOK(t, conn, "SHOW COLLATION LIKE 'utf8mb4_bin'")
	if column(t, res, 0, "Id") != "46" || column(t, res, 0, "Pad_attribute") != "PAD SPACE" || column(t, res, 0, "Sortlen") != "1" {
		t.Errorf("utf8mb4_bin: %s %s", column(t, res, 0, "Id"), column(t, res, 0, "Pad_attribute"))
	}
}

func TestShowGrants(t *testing.T) {
	conn := shopServer(t)
	want := "GRANT ALL PRIVILEGES ON *.* TO `" + testUser + "`@`%` WITH GRANT OPTION"
	for _, q := range []string{"SHOW GRANTS", "SHOW GRANTS FOR CURRENT_USER()", "SHOW GRANTS FOR '" + testUser + "'"} {
		res := execOK(t, conn, q)
		if got, _ := res.GetString(0, 0); got != want {
			t.Errorf("%s: %q", q, got)
		}
		if name := string(res.Fields[0].Name); name != "Grants for "+testUser+"@%" {
			t.Errorf("%s column: %q", q, name)
		}
	}
	_, err := conn.Execute("SHOW GRANTS FOR 'nobody'@'localhost'")
	if e := requireMyError(t, err, 1141, "42000"); e.Message != "There is no such grant defined for user 'nobody' on host 'localhost'" {
		t.Errorf("1141 message: %q", e.Message)
	}
}

package wire

import (
	"testing"
)

// CREATE DATABASE creates a new database file (docs/05-dialect.md section 4). It
// commits the open transaction first, as MySQL does for DDL. Only the server's
// character set and collation are accepted: a database of its own collation would
// change how its tables compare strings. DROP DATABASE stays 1235. `oracle: pending`.
func TestCreateDatabase(t *testing.T) {
	addr := queueServer(t, 8)
	conn := dial(t, addr)

	res := execOK(t, conn, "CREATE DATABASE app")
	if res.AffectedRows != 1 {
		t.Errorf("affected rows %d, want 1", res.AffectedRows)
	}
	execOK(t, conn, "CREATE SCHEMA IF NOT EXISTS app2 DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	execOK(t, conn, "CREATE DATABASE /*!32312 IF NOT EXISTS*/ `app3` /*!40100 DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci */ /*!80016 DEFAULT ENCRYPTION='N' */")

	rows := execOK(t, conn, "SHOW DATABASES")
	found := map[string]bool{}
	for i := range rows.Values {
		s, _ := rows.GetString(i, 0)
		found[s] = true
	}
	for _, db := range []string{"app", "app2", "app3", "shop"} {
		if !found[db] {
			t.Errorf("SHOW DATABASES lacks %s: %v", db, found)
		}
	}

	execOK(t, conn, "USE app")
	execOK(t, conn, "CREATE TABLE t (id INT PRIMARY KEY, v VARCHAR(10))")
	execOK(t, conn, "INSERT INTO t VALUES (1, 'a')")
	if v := decodedText(execOK(t, conn, "SELECT v FROM app.t")); v != "a" {
		t.Errorf("app.t: %q", v)
	}

	_, err := conn.Execute("CREATE DATABASE app")
	requireMyError(t, err, 1007, "HY000")
	execOK(t, conn, "CREATE DATABASE IF NOT EXISTS app")
	w := execOK(t, conn, "SHOW WARNINGS")
	if len(w.Values) != 1 {
		t.Fatalf("warnings: %v", w.Values)
	}
	if level, _ := w.GetString(0, 0); level != "Note" {
		t.Errorf("level %q", level)
	}
	if code, _ := w.GetInt(0, 1); code != 1007 {
		t.Errorf("code %d", code)
	}

	// DDL commits the open transaction.
	execOK(t, conn, "BEGIN")
	execOK(t, conn, "INSERT INTO t VALUES (2, 'b')")
	execOK(t, conn, "CREATE DATABASE app4")
	execOK(t, conn, "ROLLBACK")
	if n := decodedText(execOK(t, conn, "SELECT COUNT(*) FROM t")); n != "2" {
		t.Errorf("rows after implicit commit: %s", n)
	}

	for _, tc := range []struct {
		query string
		code  uint16
		state string
	}{
		{"CREATE DATABASE `a-b`", 1102, "42000"},
		{"CREATE DATABASE information_schema", 1044, "42000"},
		{"CREATE DATABASE l1 CHARACTER SET latin1", 1235, "42000"},
		{"CREATE DATABASE l2 COLLATE utf8mb4_bin", 1235, "42000"},
		{"CREATE DATABASE l3 DEFAULT ENCRYPTION='Y'", 1235, "42000"},
		{"DROP DATABASE app4", 1235, "42000"},
	} {
		_, err := conn.Execute(tc.query)
		requireMyError(t, err, tc.code, tc.state)
	}
	if found := execOK(t, conn, "SHOW DATABASES LIKE 'l%'"); len(found.Values) != 0 {
		t.Errorf("a refused CREATE DATABASE created %v", found.Values)
	}
}

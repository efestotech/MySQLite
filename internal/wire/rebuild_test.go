package wire

import (
	"strings"
	"testing"
)

// ALTER TABLE forms SQLite runs through the 12-step rebuild: MODIFY, CHANGE, ALTER
// COLUMN, RENAME COLUMN, positioned or NOT NULL ADD COLUMN. Rows, keys, sidecar facts
// and the AUTO_INCREMENT counter carry over. `oracle: pending`.
func TestAlterTableRebuild(t *testing.T) {
	conn := gadgetServer(t)
	execOK(t, conn, "CREATE INDEX by_name ON gadgets (name)")
	execOK(t, conn, "INSERT INTO gadgets (name) VALUES ('tmp')")
	execOK(t, conn, "DELETE FROM gadgets WHERE id = 3")

	execOK(t, conn, "ALTER TABLE gadgets MODIFY name VARCHAR(30) NOT NULL DEFAULT 'x' COMMENT 'label', "+
		"ADD qty INT NOT NULL AFTER id, ADD flag VARCHAR(3) NOT NULL FIRST")
	want := "CREATE TABLE `gadgets` (\n" +
		"  `flag` varchar(3) NOT NULL,\n" +
		"  `id` int NOT NULL AUTO_INCREMENT,\n" +
		"  `qty` int NOT NULL,\n" +
		"  `name` varchar(30) NOT NULL DEFAULT 'x' COMMENT 'label',\n" +
		"  `note` text,\n" +
		"  PRIMARY KEY (`id`),\n" +
		"  KEY `by_name` (`name`)\n" +
		") ENGINE=InnoDB AUTO_INCREMENT=4 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci"
	if got := showCreate(t, conn, "gadgets"); got != want {
		t.Errorf("after MODIFY and ADD:\n%s", got)
	}
	// Existing rows get NOT NULL's implicit defaults; new rows must give a value.
	if got := rowsOf(t, execOK(t, conn, "SELECT flag, id, qty, name FROM gadgets ORDER BY id")); got != ",1,0,pen;,2,0,ink" {
		t.Errorf("rows %q", got)
	}
	_, err := conn.Execute("INSERT INTO gadgets (flag, name) VALUES ('b', 'x')")
	requireMyError(t, err, 1364, "HY000")
	// The counter carries over: id 3 stays lost.
	if res := execOK(t, conn, "INSERT INTO gadgets (flag, qty, name) VALUES ('a', 1, 'cup')"); res.InsertId != 4 {
		t.Errorf("id after rebuild %d, want 4", res.InsertId)
	}
	_, err = conn.Execute("INSERT INTO gadgets (flag, qty, name) VALUES ('a', 1, '" + strings.Repeat("x", 31) + "')")
	requireMyError(t, err, 1406, "22001")

	execOK(t, conn, "ALTER TABLE gadgets ALTER COLUMN name DROP DEFAULT")
	if got := showCreate(t, conn, "gadgets"); !strings.Contains(got, "`name` varchar(30) NOT NULL COMMENT 'label',") {
		t.Errorf("after DROP DEFAULT:\n%s", got)
	}
	execOK(t, conn, "ALTER TABLE gadgets ALTER name SET DEFAULT 'y'")
	if got := showCreate(t, conn, "gadgets"); !strings.Contains(got, "`name` varchar(30) NOT NULL DEFAULT 'y' COMMENT 'label',") {
		t.Errorf("after SET DEFAULT:\n%s", got)
	}

	// The range and length CHECKs follow a renamed column: errors name the new one.
	execOK(t, conn, "ALTER TABLE gadgets RENAME COLUMN name TO title")
	_, err = conn.Execute("INSERT INTO gadgets (flag, qty, title) VALUES ('c', 1, '" + strings.Repeat("x", 31) + "')")
	if e := requireMyError(t, err, 1406, "22001"); e.Message != "Data too long for column 'title' at row 1" {
		t.Errorf("1406 message %q", e.Message)
	}
	execOK(t, conn, "ALTER TABLE gadgets CHANGE title label VARCHAR(30) NOT NULL AFTER flag")
	want = "CREATE TABLE `gadgets` (\n" +
		"  `flag` varchar(3) NOT NULL,\n" +
		"  `label` varchar(30) NOT NULL,\n" +
		"  `id` int NOT NULL AUTO_INCREMENT,\n" +
		"  `qty` int NOT NULL,\n" +
		"  `note` text,\n" +
		"  PRIMARY KEY (`id`),\n" +
		"  KEY `by_name` (`label`)\n" +
		") ENGINE=InnoDB AUTO_INCREMENT=5 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci"
	if got := showCreate(t, conn, "gadgets"); got != want {
		t.Errorf("after CHANGE:\n%s", got)
	}
	if got := rowsOf(t, execOK(t, conn, "SELECT id, label FROM gadgets ORDER BY id")); got != "1,pen;2,ink;4,cup" {
		t.Errorf("rows after CHANGE %q", got)
	}

	refused := []struct {
		query string
		code  uint16
		state string
		msg   string
	}{
		{"ALTER TABLE gadgets MODIFY label INT", 1235, "42000", ""},
		{"ALTER TABLE gadgets MODIFY label VARCHAR(2) NOT NULL", 1406, "22001", "Data too long for column 'label' at row 1"},
		{"ALTER TABLE gadgets MODIFY note TEXT NOT NULL", 1138, "22004", "Invalid use of NULL value"},
		{"ALTER TABLE gadgets ADD d DATE NOT NULL", 1235, "42000", ""},
		{"ALTER TABLE gadgets MODIFY nope INT", 1054, "42S22", "Unknown column 'nope' in 'gadgets'"},
		{"ALTER TABLE gadgets CHANGE label flag INT", 1060, "42S21", "Duplicate column name 'flag'"},
		{"ALTER TABLE gadgets ADD x INT AFTER nope", 1054, "42S22", "Unknown column 'nope' in 'gadgets'"},
		{"ALTER TABLE gadgets RENAME COLUMN nope TO x", 1054, "42S22", "Unknown column 'nope' in 'gadgets'"},
		{"ALTER TABLE gadgets DROP PRIMARY KEY", 1075, "42000", ""},
		// A failed spec undoes the rebuild before it.
		{"ALTER TABLE gadgets MODIFY qty BIGINT NOT NULL, DROP COLUMN nope", 1091, "42000", ""},
	}
	for _, c := range refused {
		_, err := conn.Execute(c.query)
		e := requireMyError(t, err, c.code, c.state)
		if c.msg != "" && e.Message != c.msg {
			t.Errorf("%s: message %q, want %q", c.query, e.Message, c.msg)
		}
	}
	if got := showCreate(t, conn, "gadgets"); got != want {
		t.Errorf("after refused ALTERs:\n%s", got)
	}
}

// Primary keys: ADD/DROP PRIMARY KEY, DROP COLUMN of a key column, and an
// AUTO_INCREMENT key added to a table that has rows. `oracle: pending`.
func TestAlterTablePrimaryKey(t *testing.T) {
	conn := shopServer(t)

	execOK(t, conn, "CREATE TABLE logs (msg VARCHAR(10))")
	execOK(t, conn, "INSERT INTO logs VALUES ('b'), ('a')")
	execOK(t, conn, "ALTER TABLE logs ADD id INT AUTO_INCREMENT PRIMARY KEY FIRST")
	if got := rowsOf(t, execOK(t, conn, "SELECT id, msg FROM logs ORDER BY id")); got != "1,b;2,a" {
		t.Errorf("logs %q", got)
	}
	if res := execOK(t, conn, "INSERT INTO logs (msg) VALUES ('c')"); res.InsertId != 3 {
		t.Errorf("next id %d, want 3", res.InsertId)
	}

	execOK(t, conn, "CREATE TABLE kv (k INT PRIMARY KEY, v INT)")
	execOK(t, conn, "INSERT INTO kv VALUES (1, 1), (2, NULL)")
	execOK(t, conn, "ALTER TABLE kv DROP PRIMARY KEY")
	execOK(t, conn, "INSERT INTO kv VALUES (1, 3)")
	_, err := conn.Execute("ALTER TABLE kv ADD PRIMARY KEY (k)")
	if e := requireMyError(t, err, 1062, "23000"); e.Message != "Duplicate entry '1' for key 'kv.PRIMARY'" {
		t.Errorf("1062 message %q", e.Message)
	}
	_, err = conn.Execute("ALTER TABLE kv ADD PRIMARY KEY (v)")
	requireMyError(t, err, 1138, "22004")
	_, err = conn.Execute("ALTER TABLE kv DROP PRIMARY KEY")
	requireMyError(t, err, 1091, "42000")
	execOK(t, conn, "DELETE FROM kv WHERE v = 3")
	execOK(t, conn, "ALTER TABLE kv ADD PRIMARY KEY (k)")
	_, err = conn.Execute("ALTER TABLE kv ADD PRIMARY KEY (v)")
	requireMyError(t, err, 1068, "42000")
	if got := showCreate(t, conn, "kv"); !strings.Contains(got, "`k` int NOT NULL,") || !strings.Contains(got, "PRIMARY KEY (`k`)") {
		t.Errorf("kv:\n%s", got)
	}

	execOK(t, conn, "CREATE TABLE pair (a INT, b INT, c INT, PRIMARY KEY (a, b))")
	execOK(t, conn, "INSERT INTO pair VALUES (1, 1, 1), (1, 2, 2)")
	_, err = conn.Execute("ALTER TABLE pair DROP COLUMN b")
	if e := requireMyError(t, err, 1062, "23000"); e.Message != "Duplicate entry '1' for key 'pair.PRIMARY'" {
		t.Errorf("1062 message %q", e.Message)
	}
	execOK(t, conn, "DELETE FROM pair WHERE b = 2")
	execOK(t, conn, "ALTER TABLE pair DROP COLUMN b")
	if got := showCreate(t, conn, "pair"); !strings.Contains(got, "PRIMARY KEY (`a`)") || strings.Contains(got, "`b`") {
		t.Errorf("pair:\n%s", got)
	}

	// A collation change can make a UNIQUE key's values collide.
	execOK(t, conn, "CREATE TABLE tags (t VARCHAR(5) COLLATE utf8mb4_bin, UNIQUE KEY (t))")
	execOK(t, conn, "INSERT INTO tags VALUES ('a'), ('A')")
	_, err = conn.Execute("ALTER TABLE tags MODIFY t VARCHAR(5)")
	requireMyError(t, err, 1062, "23000")
	execOK(t, conn, "ALTER TABLE tags MODIFY t VARCHAR(8) COLLATE utf8mb4_bin")
	if got := rowsOf(t, execOK(t, conn, "SELECT t FROM tags ORDER BY t")); got != "A;a" {
		t.Errorf("tags %q", got)
	}
}

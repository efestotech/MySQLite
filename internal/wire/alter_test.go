package wire

import (
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/client"
)

// ALTER TABLE forms SQLite runs in place: ADD COLUMN, DROP COLUMN, RENAME, index
// changes and table options, several in one statement. `oracle: pending`.
func TestAlterTableNative(t *testing.T) {
	conn := gadgetServer(t)

	execOK(t, conn, "ALTER TABLE gadgets ADD COLUMN qty TINYINT DEFAULT 1 COMMENT 'count', "+
		"ADD code VARCHAR(8) COLLATE utf8mb4_bin UNIQUE, ADD INDEX (name), ADD UNIQUE KEY uq (name, code), "+
		"ENGINE = MyISAM, COMMENT = 'stock', ALGORITHM = INPLACE, LOCK = NONE")
	want := "CREATE TABLE `gadgets` (\n" +
		"  `id` int NOT NULL AUTO_INCREMENT,\n" +
		"  `name` varchar(20) COLLATE utf8mb4_general_ci NOT NULL,\n" +
		"  `note` text COLLATE utf8mb4_general_ci,\n" +
		"  `qty` tinyint DEFAULT '1' COMMENT 'count',\n" +
		"  `code` varchar(8) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL,\n" +
		"  PRIMARY KEY (`id`),\n" +
		"  UNIQUE KEY `code` (`code`),\n" +
		"  UNIQUE KEY `uq` (`name`,`code`),\n" +
		"  KEY `name` (`name`)\n" +
		") ENGINE=MyISAM AUTO_INCREMENT=3 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='stock'"
	if got := showCreate(t, conn, "gadgets"); got != want {
		t.Errorf("after ADD:\n%s", got)
	}
	if got := rowsOf(t, execOK(t, conn, "SELECT id, name, qty, code FROM gadgets ORDER BY id")); got != "1,pen,1,NULL;2,ink,1,NULL" {
		t.Errorf("rows %q", got)
	}
	// The new column keeps its type's range and its collation.
	_, err := conn.Execute("INSERT INTO gadgets (name, qty) VALUES ('x', 300)")
	requireMyError(t, err, 1264, "22003")
	execOK(t, conn, "INSERT INTO gadgets (name, code) VALUES ('cup', 'a')")
	_, err = conn.Execute("INSERT INTO gadgets (name, code) VALUES ('mug', 'a')")
	if e := requireMyError(t, err, 1062, "23000"); e.Message != "Duplicate entry 'a' for key 'gadgets.code'" {
		t.Errorf("1062 message %q", e.Message)
	}
	execOK(t, conn, "INSERT INTO gadgets (name, code) VALUES ('mug', 'A')")

	refused := []struct {
		query string
		code  uint16
		state string
		msg   string
	}{
		{"ALTER TABLE gadgets ADD COLUMN NAME INT", 1060, "42S21", "Duplicate column name 'NAME'"},
		{"ALTER TABLE gadgets ADD COLUMN x TEXT UNIQUE", 1170, "42000", "BLOB/TEXT column 'x' used in key specification without a key length"},
		{"ALTER TABLE gadgets DROP COLUMN nope", 1091, "42000", "Can't DROP 'nope'; check that column/key exists"},
		{"ALTER TABLE gadgets RENAME COLUMN nope TO x", 1054, "42S22", "Unknown column 'nope' in 'gadgets'"},
		{"ALTER TABLE gadgets RENAME COLUMN note TO Name", 1060, "42S21", "Duplicate column name 'Name'"},
		{"ALTER TABLE gadgets RENAME TO shop.items", 1050, "42S01", "Table 'items' already exists"},
		{"ALTER TABLE gadgets RENAME TO elsewhere.gadgets", 1235, "42000", ""},
		{"ALTER TABLE gadgets RENAME TO __mysqlite_x", 1235, "42000", ""},
		{"ALTER TABLE gadgets ADD INDEX Name (qty)", 1061, "42000", "Duplicate key name 'Name'"},
		{"ALTER TABLE gadgets ADD INDEX x (nope)", 1072, "42000", "Key column 'nope' doesn't exist in table"},
		{"ALTER TABLE gadgets ADD FOREIGN KEY (qty) REFERENCES items (id)", 1235, "42000", ""},
		{"ALTER TABLE gadgets DROP INDEX nope", 1091, "42000", "Can't DROP 'nope'; check that column/key exists"},
		{"ALTER TABLE gadgets RENAME INDEX nope TO x", 1176, "42000", "Key 'nope' doesn't exist in table 'gadgets'"},
		{"ALTER TABLE gadgets RENAME INDEX uq TO NAME", 1061, "42000", "Duplicate key name 'NAME'"},
		{"ALTER TABLE gadgets RENAME INDEX uq TO `PRIMARY`", 1280, "42000", "Incorrect index name 'PRIMARY'"},
		{"ALTER TABLE gadgets AUTO_INCREMENT = 10", 1235, "42000", ""},
		{"ALTER TABLE missing ADD x INT", 1146, "42S02", "Table 'shop.missing' doesn't exist"},
		// One statement is atomic: the ADD does not survive the failed DROP.
		{"ALTER TABLE gadgets ADD COLUMN extra INT, DROP COLUMN nope", 1091, "42000", ""},
	}
	for _, c := range refused {
		_, err := conn.Execute(c.query)
		e := requireMyError(t, err, c.code, c.state)
		if c.msg != "" && e.Message != c.msg {
			t.Errorf("%s: message %q, want %q", c.query, e.Message, c.msg)
		}
	}
	_, err = conn.Execute("SELECT extra FROM gadgets")
	requireMyError(t, err, 1054, "42S22")

	// DROP COLUMN drops the keys it empties and shrinks the others; a UNIQUE key that
	// would then hold duplicates stops it.
	execOK(t, conn, "INSERT INTO gadgets (name, code) VALUES ('pen', 'z')")
	_, err = conn.Execute("ALTER TABLE gadgets DROP COLUMN code")
	if e := requireMyError(t, err, 1062, "23000"); e.Message != "Duplicate entry 'pen' for key 'gadgets.uq'" {
		t.Errorf("1062 message %q", e.Message)
	}
	execOK(t, conn, "DELETE FROM gadgets WHERE code = 'z'")
	execOK(t, conn, "ALTER TABLE gadgets DROP COLUMN code, DROP qty, RENAME INDEX uq TO uq_name, DROP INDEX name")
	want = "CREATE TABLE `gadgets` (\n" +
		"  `id` int NOT NULL AUTO_INCREMENT,\n" +
		"  `name` varchar(20) COLLATE utf8mb4_general_ci NOT NULL,\n" +
		"  `note` text COLLATE utf8mb4_general_ci,\n" +
		"  PRIMARY KEY (`id`),\n" +
		"  UNIQUE KEY `uq_name` (`name`)\n" +
		") ENGINE=MyISAM AUTO_INCREMENT=6 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='stock'"
	if got := showCreate(t, conn, "gadgets"); got != want {
		t.Errorf("after DROP:\n%s", got)
	}
	_, err = conn.Execute("INSERT INTO gadgets (name) VALUES ('cup')")
	if e := requireMyError(t, err, 1062, "23000"); e.Message != "Duplicate entry 'cup' for key 'gadgets.uq_name'" {
		t.Errorf("1062 message %q", e.Message)
	}

	// RENAME TO carries the rows, keys, AUTO_INCREMENT counter and metadata; the old
	// name is free for a new table with keys of the same names.
	execOK(t, conn, "ALTER TABLE gadgets RENAME TO shop.tools")
	if res := execOK(t, conn, "INSERT INTO tools (name) VALUES ('saw')"); res.InsertId != 6 {
		t.Errorf("id after RENAME %d, want 6", res.InsertId)
	}
	if got := showCreate(t, conn, "tools"); !strings.Contains(got, "UNIQUE KEY `uq_name` (`name`)") || !strings.Contains(got, "COMMENT='stock'") {
		t.Errorf("after RENAME:\n%s", got)
	}
	_, err = conn.Execute("SELECT * FROM gadgets")
	requireMyError(t, err, 1146, "42S02")
	execOK(t, conn, "CREATE TABLE gadgets (name VARCHAR(5), UNIQUE KEY uq_name (name))")
	execOK(t, conn, "INSERT INTO gadgets VALUES ('saw')")

	// Several specs see each other's changes, in order.
	execOK(t, conn, "ALTER TABLE tools ADD COLUMN made DATE, RENAME COLUMN made TO built, ADD INDEX (built), RENAME TO kit")
	if got := showCreate(t, conn, "kit"); !strings.Contains(got, "`built` date DEFAULT NULL") || !strings.Contains(got, "KEY `built` (`built`)") {
		t.Errorf("after several specs:\n%s", got)
	}
	execOK(t, conn, "ALTER TABLE kit ALGORITHM = COPY")
	execOK(t, conn, "ALTER TABLE kit RENAME TO Kit")
	if got := rowsOf(t, execOK(t, conn, "SHOW TABLES")); got != "Kit;gadgets;items" {
		t.Errorf("tables after a case-only RENAME %q", got)
	}
	execOK(t, conn, "INSERT INTO Kit (name) VALUES ('nail')")
	_, err = conn.Execute("ALTER TABLE kit DROP COLUMN id, DROP COLUMN name, DROP COLUMN note, DROP COLUMN built")
	requireMyError(t, err, 1090, "42000")
	execOK(t, conn, "CREATE TABLE one (a INT)")
	_, err = conn.Execute("ALTER TABLE one DROP COLUMN a")
	if e := requireMyError(t, err, 1090, "42000"); e.Message != "You can't delete all columns with ALTER TABLE; use DROP TABLE instead" {
		t.Errorf("1090 message %q", e.Message)
	}
}

func showCreate(t *testing.T, conn *client.Conn, table string) string {
	t.Helper()
	return column(t, execOK(t, conn, "SHOW CREATE TABLE "+table), 0, "Create Table")
}

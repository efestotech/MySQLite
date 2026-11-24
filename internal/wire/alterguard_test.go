package wire

import (
	"strings"
	"testing"
)

// Regression tests from the independent verification of M3 T10.

// ADD PRIMARY KEY on a nullable column drops its implicit DEFAULT NULL: the column
// becomes NOT NULL without a default, as in MySQL.
func TestAlterAddPrimaryKeyNullableDefault(t *testing.T) {
	_, addr := shopSetup(t)
	conn := dial(t, addr)
	execOK(t, conn, "CREATE TABLE np (a INT, b VARCHAR(5))")
	execOK(t, conn, "INSERT INTO np VALUES (1, 'x')")
	execOK(t, conn, "ALTER TABLE np ADD PRIMARY KEY (a)")
	want := "CREATE TABLE `np` (\n  `a` int NOT NULL,\n  `b` varchar(5) COLLATE utf8mb4_general_ci DEFAULT NULL,\n  PRIMARY KEY (`a`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci"
	if got := showCreate(t, conn, "np"); got != want {
		t.Errorf("got\n%s", got)
	}
	execOK(t, conn, "ALTER TABLE np MODIFY b VARCHAR(6)")
	_, err := conn.Execute("INSERT INTO np (b) VALUES ('z')")
	requireMyError(t, err, 1364, "HY000")
}

// DROP COLUMN of a column a FOREIGN KEY uses, on either side, is refused.
func TestAlterDropColumnForeignKey(t *testing.T) {
	eng, addr := shopSetup(t)
	conn := dial(t, addr)
	execOK(t, conn, "CREATE TABLE own (id INT PRIMARY KEY, code VARCHAR(5) NOT NULL, UNIQUE KEY uc (code))")
	execOK(t, conn, "INSERT INTO own VALUES (1, 'a'), (2, 'b')")
	seed(t, eng, "shop",
		"CREATE TABLE pet (id INTEGER PRIMARY KEY, code TEXT REFERENCES own (code))",
		"INSERT INTO pet VALUES (1, 'a')",
		"CREATE TABLE kid (id INTEGER PRIMARY KEY, pid INT REFERENCES own (id), x INT)")
	_, err := conn.Execute("ALTER TABLE own DROP COLUMN code")
	requireMyError(t, err, 1235, "42000")
	_, err = conn.Execute("ALTER TABLE kid DROP COLUMN pid")
	requireMyError(t, err, 1235, "42000")
	execOK(t, conn, "INSERT INTO pet VALUES (2, 'b')")
	if got := rowsOf(t, execOK(t, conn, "SELECT code FROM own ORDER BY id")); got != "a;b" {
		t.Errorf("own %q", got)
	}
	// A column no foreign key uses still drops.
	execOK(t, conn, "ALTER TABLE kid DROP COLUMN x")
}

// A rebuild refuses a table with indexes the catalog cannot re-create (partial,
// expression, descending) instead of dropping or changing them.
func TestAlterRebuildRefusesForeignIndexes(t *testing.T) {
	eng, addr := shopSetup(t)
	conn := dial(t, addr)
	execOK(t, conn, "CREATE TABLE k (id INT PRIMARY KEY, x INT, y VARCHAR(5))")
	for _, ix := range []string{"CREATE INDEX px ON k (x) WHERE x > 0", "CREATE INDEX ex ON k (lower(y))", "CREATE INDEX dx ON k (y DESC)"} {
		seed(t, eng, "shop", ix)
		_, err := conn.Execute("ALTER TABLE k MODIFY x BIGINT")
		requireMyError(t, err, 1235, "42000")
		seed(t, eng, "shop", "DROP INDEX "+strings.Fields(ix)[2])
	}
	execOK(t, conn, "ALTER TABLE k MODIFY x BIGINT")
}

// A collation change that makes primary-key values collide reports the table's key
// and a value, not the rebuild's temporary table.
func TestAlterPrimaryKeyCollationDuplicate(t *testing.T) {
	_, addr := shopSetup(t)
	conn := dial(t, addr)
	execOK(t, conn, "CREATE TABLE codes (id VARCHAR(5) COLLATE utf8mb4_bin PRIMARY KEY)")
	execOK(t, conn, "INSERT INTO codes VALUES ('a'), ('A')")
	_, err := conn.Execute("ALTER TABLE codes MODIFY id VARCHAR(5) COLLATE utf8mb4_general_ci")
	e := requireMyError(t, err, 1062, "23000")
	if !strings.HasSuffix(e.Message, "for key 'codes.PRIMARY'") || strings.Contains(e.Message, "''") {
		t.Errorf("1062 message %q", e.Message)
	}
}

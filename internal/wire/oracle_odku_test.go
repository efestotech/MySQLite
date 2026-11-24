package wire

import "testing"

// A row that conflicts with two different rows updates the one found through the
// PRIMARY KEY, then through the unique keys in creation order. Observed on MySQL 8.0.46
// (oracle probes o.81.1 to o.81.3).
func TestOnDuplicateKeyUpdateKeyOrder(t *testing.T) {
	conn := shopServer(t)
	for _, c := range []struct {
		name, create, insert, upsert, query, want string
	}{
		{
			"primary before unique",
			"CREATE TABLE k1 (id INT PRIMARY KEY, u INT UNIQUE, n INT)",
			"INSERT INTO k1 VALUES (1, 10, 0), (2, 20, 0)",
			"INSERT INTO k1 VALUES (1, 20, 5) ON DUPLICATE KEY UPDATE n = 9",
			"SELECT id, u, n FROM k1 ORDER BY id", "1,10,9;2,20,0",
		},
		{
			"primary declared after unique",
			"CREATE TABLE k2 (u INT UNIQUE, id INT PRIMARY KEY, n INT)",
			"INSERT INTO k2 VALUES (10, 1, 0), (20, 2, 0)",
			"INSERT INTO k2 VALUES (20, 1, 5) ON DUPLICATE KEY UPDATE n = 9",
			"SELECT id, u, n FROM k2 ORDER BY id", "1,10,9;2,20,0",
		},
		{
			"unique keys in creation order",
			"CREATE TABLE k3 (id INT PRIMARY KEY, a INT UNIQUE, b INT UNIQUE, n INT)",
			"INSERT INTO k3 VALUES (1, 10, 100, 0), (2, 20, 200, 0)",
			"INSERT INTO k3 VALUES (3, 20, 100, 5) ON DUPLICATE KEY UPDATE n = 9",
			"SELECT id, a, b, n FROM k3 ORDER BY id", "1,10,100,0;2,20,200,9",
		},
		{
			"composite primary key",
			"CREATE TABLE k4 (a INT, b INT, u INT UNIQUE, n INT, PRIMARY KEY (a, b))",
			"INSERT INTO k4 VALUES (1, 1, 10, 0), (2, 2, 20, 0)",
			"INSERT INTO k4 VALUES (1, 1, 20, 5) ON DUPLICATE KEY UPDATE n = 9",
			"SELECT a, b, u, n FROM k4 ORDER BY a", "1,1,10,9;2,2,20,0",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			execOK(t, conn, c.create)
			execOK(t, conn, c.insert)
			requireOK(t, conn, c.upsert, 2, 0, "")
			if got := rowsOf(t, execOK(t, conn, c.query)); got != c.want {
				t.Errorf("rows %q, want %q", got, c.want)
			}
		})
	}
}

// Within one statement the id a conflicting row was given is used by the next row that
// inserts (MySQL 8.0.46, probe o.80.2: insert id 3, not 4).
func TestOnDuplicateKeyUpdateIDInStatement(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "CREATE TABLE t80 (id INT AUTO_INCREMENT PRIMARY KEY, u INT UNIQUE, n INT)")
	execOK(t, conn, "INSERT INTO t80 (u, n) VALUES (1, 0), (2, 0)")
	requireOK(t, conn, "INSERT INTO t80 (u, n) VALUES (2, 1), (3, 0) ON DUPLICATE KEY UPDATE n = n + 1", 3, 3, "Records: 2  Duplicates: 1  Warnings: 0")
	if got := rowsOf(t, execOK(t, conn, "SELECT id, u, n FROM t80 ORDER BY id")); got != "1,1,0;2,2,1;3,3,0" {
		t.Errorf("rows %q", got)
	}
}

// A multi-row INSERT IGNORE gives the rows it inserts consecutive ids; the values of
// the skipped rows are lost at the end of the statement, so the counter ends at the
// first id plus the number of rows (MySQL 8.0.46, oracle CLI probe cli2: ids 2 and 3,
// LAST_INSERT_ID() 2, next id 6).
func TestInsertIgnoreIDs(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "CREATE TABLE ti (id INT AUTO_INCREMENT PRIMARY KEY, u INT UNIQUE)")
	execOK(t, conn, "INSERT INTO ti (u) VALUES (1)")
	requireOK(t, conn, "INSERT IGNORE INTO ti (u) VALUES (1), (2), (1), (3)", 2, 2, "Records: 4  Duplicates: 2  Warnings: 2")
	if got := rowsOf(t, execOK(t, conn, "SELECT LAST_INSERT_ID()")); got != "2" {
		t.Errorf("LAST_INSERT_ID %s", got)
	}
	execOK(t, conn, "INSERT INTO ti (u) VALUES (4)")
	if got := rowsOf(t, execOK(t, conn, "SELECT id, u FROM ti ORDER BY id")); got != "1,1;2,2;3,3;6,4" {
		t.Errorf("rows %q", got)
	}
}

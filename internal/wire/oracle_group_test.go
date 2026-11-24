package wire

import "testing"

// Under ONLY_FULL_GROUP_BY, MySQL 8.0.46 also takes a column as functionally dependent
// on GROUP BY through a unique NOT NULL key, an equality with a constant in WHERE, and
// an inner join's equality on a primary key; an equality with an expression does not
// count (oracle probes o.58.1-o.58.4).
func TestFunctionalDependence(t *testing.T) {
	conn := shopServer(t,
		"CREATE TABLE t58 (id INT PRIMARY KEY, u INT NOT NULL UNIQUE, v INT)",
		"INSERT INTO t58 VALUES (1, 10, 100), (2, 20, 200)",
		"CREATE TABLE t58b (x INT, y INT)",
		"INSERT INTO t58b VALUES (1, 2), (1, 3)",
	)
	for _, c := range []struct{ sql, want string }{
		{"SELECT u, v FROM t58 GROUP BY u ORDER BY u", "10,100;20,200"},
		{"SELECT x, y FROM t58b WHERE y = 2 GROUP BY x", "1,2"},
		{"SELECT x, y FROM t58b WHERE 2 = y AND x > 0 GROUP BY x", "1,2"},
		{"SELECT t58b.x, t58.v FROM t58b JOIN t58 ON t58.id = t58b.x GROUP BY t58b.x", "1,100"},
	} {
		if got := rowsOf(t, execOK(t, conn, c.sql)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.sql, got, c.want)
		}
	}
	_, err := conn.Execute("SELECT x, y FROM t58b WHERE y = x + 1 GROUP BY x")
	requireMyError(t, err, 1055, "42000")
	// A unique key with a nullable column does not determine the row.
	execOK(t, conn, "CREATE TABLE t58c (id INT PRIMARY KEY, n INT UNIQUE, v INT)")
	_, err = conn.Execute("SELECT n, v FROM t58c GROUP BY n")
	requireMyError(t, err, 1055, "42000")
}

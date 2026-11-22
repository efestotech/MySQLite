package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// gadgetServer has shop.gadgets, created through mysqlite so its sidecar facts exist.
func gadgetServer(t *testing.T) *client.Conn {
	t.Helper()
	conn := shopServer(t)
	execOK(t, conn, "CREATE TABLE gadgets (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(20) NOT NULL, note TEXT)")
	execOK(t, conn, "INSERT INTO gadgets (name) VALUES ('pen'), ('ink')")
	return conn
}

// CREATE INDEX and DROP INDEX. `oracle: pending`.
func TestCreateDropIndex(t *testing.T) {
	conn := gadgetServer(t)
	execOK(t, conn, "CREATE INDEX by_name ON gadgets (name)")
	execOK(t, conn, "CREATE UNIQUE INDEX uq_name ON gadgets (name)")
	_, err := conn.Execute("INSERT INTO gadgets (name) VALUES ('pen')")
	if e := requireMyError(t, err, 1062, "23000"); e.Message != "Duplicate entry 'pen' for key 'gadgets.uq_name'" {
		t.Errorf("1062 message %q", e.Message)
	}

	refused := []struct {
		query string
		code  uint16
		state string
		msg   string
	}{
		{"CREATE INDEX BY_NAME ON gadgets (name)", 1061, "42000", "Duplicate key name 'BY_NAME'"},
		{"CREATE INDEX x ON gadgets (nope)", 1072, "42000", "Key column 'nope' doesn't exist in table"},
		{"CREATE INDEX x ON gadgets (note)", 1170, "42000", "BLOB/TEXT column 'note' used in key specification without a key length"},
		{"CREATE INDEX x ON missing (a)", 1146, "42S02", "Table 'shop.missing' doesn't exist"},
		{"CREATE INDEX `PRIMARY` ON gadgets (name)", 1280, "42000", "Incorrect index name 'PRIMARY'"},
		{"CREATE INDEX x ON gadgets (name(5))", 1235, "42000", ""},
		{"CREATE FULLTEXT INDEX x ON gadgets (name)", 1235, "42000", ""},
		{"DROP INDEX nope ON gadgets", 1091, "42000", "Can't DROP 'nope'; check that column/key exists"},
		{"DROP INDEX `PRIMARY` ON gadgets", 1235, "42000", ""},
		{"DROP INDEX x ON missing", 1146, "42S02", "Table 'shop.missing' doesn't exist"},
	}
	for _, c := range refused {
		_, err := conn.Execute(c.query)
		e := requireMyError(t, err, c.code, c.state)
		if c.msg != "" && e.Message != c.msg {
			t.Errorf("%s: message %q, want %q", c.query, e.Message, c.msg)
		}
	}

	execOK(t, conn, "DROP INDEX uq_name ON gadgets")
	execOK(t, conn, "INSERT INTO gadgets (name) VALUES ('pen')")
	execOK(t, conn, "DROP INDEX By_Name ON gadgets")
	_, err = conn.Execute("DROP INDEX by_name ON gadgets")
	requireMyError(t, err, 1091, "42000")

	// A UNIQUE index over duplicate values is not created.
	_, err = conn.Execute("CREATE UNIQUE INDEX uq ON gadgets (name)")
	if e := requireMyError(t, err, 1062, "23000"); e.Message != "Duplicate entry 'pen' for key 'gadgets.uq'" {
		t.Errorf("1062 message %q", e.Message)
	}
	execOK(t, conn, "CREATE INDEX uq ON gadgets (name)")
}

// TRUNCATE empties the table and restarts AUTO_INCREMENT. `oracle: pending`.
func TestTruncate(t *testing.T) {
	conn := gadgetServer(t)
	res := execOK(t, conn, "TRUNCATE TABLE gadgets")
	if res.AffectedRows != 0 {
		t.Errorf("affected rows %d", res.AffectedRows)
	}
	if got := rowsOf(t, execOK(t, conn, "SELECT COUNT(*) FROM gadgets")); got != "0" {
		t.Errorf("rows after TRUNCATE: %s", got)
	}
	if res := execOK(t, conn, "INSERT INTO gadgets (name) VALUES ('a')"); res.InsertId != 1 {
		t.Errorf("insert id after TRUNCATE %d, want 1", res.InsertId)
	}
	execOK(t, conn, "TRUNCATE gadgets")
	_, err := conn.Execute("TRUNCATE TABLE missing")
	if e := requireMyError(t, err, 1146, "42S02"); e.Message != "Table 'shop.missing' doesn't exist" {
		t.Errorf("1146 message %q", e.Message)
	}
}

// DROP TABLE is atomic: a missing table without IF EXISTS drops nothing. `oracle: pending`.
func TestDropTable(t *testing.T) {
	conn := gadgetServer(t)
	_, err := conn.Execute("DROP TABLE gadgets, missing, missing2")
	if e := requireMyError(t, err, 1051, "42S02"); e.Message != "Unknown table 'shop.missing,shop.missing2'" {
		t.Errorf("1051 message %q", e.Message)
	}
	execOK(t, conn, "SELECT * FROM gadgets")

	res := execOK(t, conn, "DROP TABLE IF EXISTS gadgets, missing")
	if res.Warnings != 1 {
		t.Errorf("IF EXISTS: want 1 warning, got %d", res.Warnings)
	}
	w := execOK(t, conn, "SHOW WARNINGS")
	if got := rowsOf(t, w); got != "Note,1051,Unknown table 'shop.missing'" {
		t.Errorf("warnings %q", got)
	}
	_, err = conn.Execute("SELECT * FROM gadgets")
	requireMyError(t, err, 1146, "42S02")

	// The dropped table's metadata is gone: a new gadgets has its own types.
	execOK(t, conn, "CREATE TABLE gadgets (name INT)")
	res = execOK(t, conn, "SELECT name FROM gadgets")
	if res.Fields[0].Type != mysql.MYSQL_TYPE_LONG {
		t.Errorf("new column type %d", res.Fields[0].Type)
	}

	for _, q := range []string{"DROP TEMPORARY TABLE gadgets", "DROP TABLE gadgets, other.t"} {
		_, err := conn.Execute(q)
		requireMyError(t, err, 1235, "42000")
	}
	execOK(t, conn, "DROP TABLE shop.gadgets, items")
}

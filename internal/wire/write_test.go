package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// Affected rows, insert id and info strings are `oracle: pending` (ADR 0002 addendum).

func requireOK(t *testing.T, conn *client.Conn, q string, affected, insertID uint64, info string) {
	t.Helper()
	res, err := conn.Execute(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if res.AffectedRows != affected || res.InsertId != insertID || res.StatusMessage != info {
		t.Fatalf("%s: want affected=%d insert_id=%d info=%q, got affected=%d insert_id=%d info=%q",
			q, affected, insertID, info, res.AffectedRows, res.InsertId, res.StatusMessage)
	}
}

func TestInsertAffectedRowsAndInsertID(t *testing.T) {
	conn := shopServer(t, "CREATE TABLE notes (k INT, v TEXT)")

	// MySQL reports the first generated id of a multi-row INSERT; SQLite's
	// last_insert_rowid() is the last one.
	requireOK(t, conn, "INSERT INTO items (name) VALUES ('a'), ('b'), ('c')", 3, 3, "Records: 3  Duplicates: 0  Warnings: 0")
	requireOK(t, conn, "INSERT INTO items (name) VALUES ('d')", 1, 6, "")
	// An explicit value is reported when nothing is generated.
	requireOK(t, conn, "INSERT INTO items (id, name) VALUES (100, 'e')", 1, 100, "")
	// Mixed: the first generated value, not the explicit one.
	requireOK(t, conn, "INSERT INTO items (id, name) VALUES (NULL, 'f'), (200, 'g'), (NULL, 'h')", 3, 101, "Records: 3  Duplicates: 0  Warnings: 0")
	requireOK(t, conn, "INSERT INTO items SET name = 'i'", 1, 202, "")
	res := execOK(t, conn, "SELECT id FROM items ORDER BY id")
	if res.RowNumber() != 11 {
		t.Fatalf("want 11 rows, got %d", res.RowNumber())
	}
	// No AUTO_INCREMENT column: insert id 0.
	requireOK(t, conn, "INSERT INTO notes VALUES (1, 'x'), (2, 'y')", 2, 0, "Records: 2  Duplicates: 0  Warnings: 0")
}

func TestUpdateAffectedRows(t *testing.T) {
	conn := shopServer(t)

	// Row 1 has note NULL, row 2 has 'x': two rows match, one changes.
	requireOK(t, conn, "UPDATE items SET note = 'x'", 1, 0, "Rows matched: 2  Changed: 1  Warnings: 0")
	requireOK(t, conn, "UPDATE items SET note = 'x'", 0, 0, "Rows matched: 2  Changed: 0  Warnings: 0")
	requireOK(t, conn, "UPDATE items SET note = 'y' WHERE id = 42", 0, 0, "Rows matched: 0  Changed: 0  Warnings: 0")
	// A value equal after conversion to the column type is not a change.
	requireOK(t, conn, "UPDATE items SET price = '1.50' WHERE id = 1", 0, 0, "Rows matched: 1  Changed: 0  Warnings: 0")
	// The comparison is binary, as MySQL compares records.
	requireOK(t, conn, "UPDATE items SET name = 'PEN' WHERE id = 1", 1, 0, "Rows matched: 1  Changed: 1  Warnings: 0")
	requireOK(t, conn, "UPDATE items SET name = 'z' ORDER BY id DESC LIMIT 1", 1, 0, "Rows matched: 1  Changed: 1  Warnings: 0")
	if got, _ := execOK(t, conn, "SELECT name FROM items WHERE id = 2").GetString(0, 0); got != "z" {
		t.Fatalf("ORDER BY id DESC LIMIT 1 updates the last row, got %q", got)
	}
	requireOK(t, conn, "UPDATE items SET note = NULL WHERE note IS NOT NULL", 2, 0, "Rows matched: 2  Changed: 2  Warnings: 0")
}

// With CLIENT_FOUND_ROWS the affected rows are the matched rows (ADR 0003, G2).
func TestUpdateFoundRows(t *testing.T) {
	_, addr := shopSetup(t)
	conn := dial(t, addr, mysql.CLIENT_FOUND_ROWS)
	requireOK(t, conn, "UPDATE items SET note = 'x'", 2, 0, "Rows matched: 2  Changed: 1  Warnings: 0")
}

func TestDeleteAffectedRows(t *testing.T) {
	conn := shopServer(t)
	requireOK(t, conn, "DELETE FROM items WHERE id = 1", 1, 0, "")
	requireOK(t, conn, "DELETE FROM items WHERE id = 1", 0, 0, "")
	requireOK(t, conn, "DELETE FROM items", 1, 0, "")
}

func TestWriteErrors(t *testing.T) {
	conn := shopServer(t, "CREATE TABLE tags (id INTEGER PRIMARY KEY AUTOINCREMENT, code VARCHAR(10) NOT NULL UNIQUE, label TEXT)")
	execOK(t, conn, "INSERT INTO tags (code) VALUES ('a')")

	cases := []struct {
		query string
		code  uint16
		state string
		msg   string
	}{
		{"INSERT INTO tags (code) VALUES ('b'), ('a')", 1062, "23000", "Duplicate entry 'a' for key 'tags.code'"},
		{"INSERT INTO tags (code) VALUES ('c'), ('c')", 1062, "23000", "Duplicate entry 'c' for key 'tags.code'"},
		{"INSERT INTO items (id, name) VALUES (1, 'x')", 1062, "23000", "Duplicate entry '1' for key 'items.PRIMARY'"},
		{"INSERT INTO missing VALUES (1)", 1146, "42S02", "Table 'shop.missing' doesn't exist"},
		{"UPDATE missing SET a = 1", 1146, "42S02", "Table 'shop.missing' doesn't exist"},
		{"INSERT INTO items (nope) VALUES (1)", 1054, "42S22", "Unknown column 'nope' in 'field list'"},
		{"UPDATE items SET nope = 1", 1054, "42S22", "Unknown column 'nope' in 'field list'"},
		{"UPDATE items SET name = 'x' WHERE nope = 1", 1054, "42S22", "Unknown column 'nope' in 'where clause'"},
		{"DELETE FROM items ORDER BY nope LIMIT 1", 1054, "42S22", "Unknown column 'nope' in 'order clause'"},
		{"INSERT INTO items (note) VALUES ('x')", 1364, "HY000", "Field 'name' doesn't have a default value"},
		{"INSERT INTO items (name) VALUES (NULL)", 1048, "23000", "Column 'name' cannot be null"},
		{"UPDATE items SET name = NULL", 1048, "23000", "Column 'name' cannot be null"},
		{"INSERT INTO items (id, name) VALUES (0, 'x')", 1235, "42000",
			"mysqlite: unsupported construct value 0 for an AUTO_INCREMENT column: MySQL generates a value, SQLite would store 0; use NULL, or SET sql_mode with NO_AUTO_VALUE_ON_ZERO to store 0"},
	}
	for _, tc := range cases {
		_, err := conn.Execute(tc.query)
		myErr := requireMyError(t, err, tc.code, tc.state)
		if myErr.Message != tc.msg {
			t.Errorf("%s: want %q, got %q", tc.query, tc.msg, myErr.Message)
		}
	}
	// A failed statement leaves no rows behind, as InnoDB rolls back the statement.
	if res := execOK(t, conn, "SELECT code FROM tags"); res.RowNumber() != 1 {
		t.Fatalf("want only the first tag, got %d rows", res.RowNumber())
	}

	execOK(t, conn, "INSERT INTO tags (code) VALUES ('b')")
	_, err := conn.Execute("UPDATE tags SET code = 'a' WHERE code = 'b'")
	myErr := requireMyError(t, err, 1062, "23000")
	if myErr.Message != "Duplicate entry 'a' for key 'tags.code'" {
		t.Errorf("update: unexpected message %q", myErr.Message)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("connection must stay usable: %v", err)
	}
}

func TestWritesInTransaction(t *testing.T) {
	_, addr := shopSetup(t)
	conn := dial(t, addr)
	other := dial(t, addr)

	execOK(t, conn, "BEGIN")
	requireOK(t, conn, "INSERT INTO items (name) VALUES ('t')", 1, 3, "")
	if n := execOK(t, conn, "SELECT id FROM items").RowNumber(); n != 3 {
		t.Fatalf("the transaction sees its own row, got %d rows", n)
	}
	if n := execOK(t, other, "SELECT id FROM items").RowNumber(); n != 2 {
		t.Fatalf("another session must not see the uncommitted row, got %d rows", n)
	}
	// A failed statement inside a transaction keeps the transaction.
	_, err := conn.Execute("INSERT INTO items (id, name) VALUES (1, 'dup')")
	requireMyError(t, err, 1062, "23000")
	if !conn.IsInTransaction() {
		t.Fatal("a duplicate key error does not end the transaction")
	}
	execOK(t, conn, "ROLLBACK")
	if n := execOK(t, other, "SELECT id FROM items").RowNumber(); n != 2 {
		t.Fatalf("ROLLBACK discards the row, got %d rows", n)
	}

	execOK(t, conn, "START TRANSACTION")
	requireOK(t, conn, "DELETE FROM items WHERE id = 1", 1, 0, "")
	execOK(t, conn, "COMMIT")
	if n := execOK(t, other, "SELECT id FROM items").RowNumber(); n != 1 {
		t.Fatalf("COMMIT publishes the delete, got %d rows", n)
	}
}

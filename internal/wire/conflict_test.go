package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// REPLACE deletes the rows a new row conflicts with, then inserts it; each deleted row
// counts as an affected row. `oracle: pending`.
func TestReplace(t *testing.T) {
	conn := gadgetServer(t)
	execOK(t, conn, "CREATE UNIQUE INDEX uq ON gadgets (name)")

	requireOK(t, conn, "REPLACE INTO gadgets (id, name) VALUES (1, 'pencil')", 2, 1, "")
	requireOK(t, conn, "REPLACE INTO gadgets (name) VALUES ('new')", 1, 3, "")
	// Conflicts with id 1 on the primary key and with id 2 on uq: both go.
	requireOK(t, conn, "REPLACE INTO gadgets (id, name) VALUES (1, 'ink')", 3, 1, "")
	if got := rowsOf(t, execOK(t, conn, "SELECT id, name FROM gadgets ORDER BY id")); got != "1,ink;3,new" {
		t.Errorf("rows %q", got)
	}
	// A later row of the statement replaces an earlier one.
	requireOK(t, conn, "REPLACE INTO gadgets (id, name) VALUES (5, 'a'), (5, 'b')", 3, 5, "Records: 2  Duplicates: 1  Warnings: 0")
	requireOK(t, conn, "REPLACE INTO gadgets (name) SELECT 'new'", 2, 6, "Records: 1  Duplicates: 1  Warnings: 0")
	requireOK(t, conn, "REPLACE gadgets SET name = 'cup'", 1, 7, "")
	if got := rowsOf(t, execOK(t, conn, "SELECT id, name FROM gadgets ORDER BY id")); got != "1,ink;5,b;6,new;7,cup" {
		t.Errorf("rows %q", got)
	}
}

// INSERT IGNORE skips the rows that hit a unique key, with warning 1062 each.
// `oracle: pending`.
func TestInsertIgnore(t *testing.T) {
	conn := gadgetServer(t)
	execOK(t, conn, "CREATE UNIQUE INDEX uq ON gadgets (name)")

	// The skipped row's id (3) is lost, as InnoDB loses it.
	requireOK(t, conn, "INSERT IGNORE INTO gadgets (name) VALUES ('ink'), ('cup')", 1, 4, "Records: 2  Duplicates: 1  Warnings: 1")
	if got := rowsOf(t, execOK(t, conn, "SHOW WARNINGS")); got != "Warning,1062,Duplicate entry 'ink' for key 'gadgets.uq'" {
		t.Errorf("warnings %q", got)
	}
	requireOK(t, conn, "INSERT IGNORE INTO gadgets (id, name) VALUES (1, 'x')", 0, 0, "")
	if got := rowsOf(t, execOK(t, conn, "SHOW WARNINGS")); got != "Warning,1062,Duplicate entry '1' for key 'gadgets.PRIMARY'" {
		t.Errorf("warnings %q", got)
	}
	requireOK(t, conn, "INSERT IGNORE INTO gadgets (name) VALUES ('d'), ('d')", 1, 5, "Records: 2  Duplicates: 1  Warnings: 1")
	requireOK(t, conn, "INSERT IGNORE gadgets SET name = 'e'", 1, 7, "")
	if got := rowsOf(t, execOK(t, conn, "SELECT id, name FROM gadgets ORDER BY id")); got != "1,pen;2,ink;4,cup;5,d;7,e" {
		t.Errorf("rows %q", got)
	}
	if got := rowsOf(t, execOK(t, conn, "SELECT LAST_INSERT_ID()")); got != "7" {
		t.Errorf("LAST_INSERT_ID %s", got)
	}

	// Rows run one at a time still bind the session's time zone.
	execOK(t, conn, "CREATE TABLE stamps (k INT PRIMARY KEY, at TIMESTAMP NULL)")
	requireOK(t, conn, "INSERT IGNORE INTO stamps VALUES (1, '2024-01-01 12:00:00'), (1, '2024-01-02 12:00:00')", 1, 0, "Records: 2  Duplicates: 1  Warnings: 1")
	if got := rowsOf(t, execOK(t, conn, "SELECT at FROM stamps")); got != "2024-01-01 12:00:00" {
		t.Errorf("TIMESTAMP %q", got)
	}

	// MySQL adjusts other bad values with a warning; mysqlite refuses rather than guess.
	for _, q := range []string{
		"INSERT IGNORE INTO gadgets (name) VALUES (NULL)",
		"INSERT IGNORE INTO gadgets (note) VALUES ('x')",
		"INSERT IGNORE INTO gadgets (name) SELECT 'z'",
		"UPDATE IGNORE gadgets SET name = 'pen'",
	} {
		_, err := conn.Execute(q)
		requireMyError(t, err, 1235, "42000")
	}
	// Errors MySQL raises even with IGNORE stay errors.
	_, err := conn.Execute("INSERT IGNORE INTO gadgets (nope) VALUES (1)")
	requireMyError(t, err, 1054, "42S22")
	_, err = conn.Execute("INSERT IGNORE INTO gadgets (name) VALUES ('a', 'b')")
	requireMyError(t, err, 1136, "21S01")
}

// ON DUPLICATE KEY UPDATE: 1 per inserted row, 2 per updated row, 0 for a row the
// update leaves unchanged (1 with CLIENT_FOUND_ROWS). `oracle: pending`.
func TestOnDuplicateKeyUpdate(t *testing.T) {
	conn := gadgetServer(t)
	execOK(t, conn, "CREATE UNIQUE INDEX uq ON gadgets (name)")

	// Each row that generates an id uses one, also when it updates instead (InnoDB).
	requireOK(t, conn, "INSERT INTO gadgets (name, note) VALUES ('pen', 'a') ON DUPLICATE KEY UPDATE note = 'b'", 2, 1, "")
	requireOK(t, conn, "INSERT INTO gadgets (name, note) VALUES ('pen', 'a') ON DUPLICATE KEY UPDATE note = 'b'", 0, 0, "")
	requireOK(t, conn, "INSERT INTO gadgets (name, note) VALUES ('cup', 'c') ON DUPLICATE KEY UPDATE note = VALUES(note)", 1, 5, "")
	requireOK(t, conn, "INSERT INTO gadgets (name, note) VALUES ('pen', 'x'), ('new', 'y') ON DUPLICATE KEY UPDATE note = VALUES(note)", 3, 7, "Records: 2  Duplicates: 1  Warnings: 0")
	requireOK(t, conn, "INSERT INTO gadgets (name) VALUES ('pen') ON DUPLICATE KEY UPDATE note = CONCAT(note, '+')", 2, 1, "")
	if got := rowsOf(t, execOK(t, conn, "SELECT id, name, note FROM gadgets ORDER BY id")); got != "1,pen,x+;2,ink,NULL;5,cup,c;7,new,y" {
		t.Errorf("rows %q", got)
	}
	if res := execOK(t, conn, "INSERT INTO gadgets (name) VALUES ('next')"); res.InsertId != 9 {
		t.Errorf("next id %d, want 9", res.InsertId)
	}
	if got := rowsOf(t, execOK(t, conn, "SELECT LAST_INSERT_ID()")); got != "9" {
		t.Errorf("LAST_INSERT_ID %s", got)
	}

	refused := []struct {
		query string
		code  uint16
		state string
		msg   string
	}{
		{"INSERT INTO gadgets (name) VALUES ('pen') ON DUPLICATE KEY UPDATE nope = 1", 1054, "42S22", "Unknown column 'nope' in 'field list'"},
		{"INSERT INTO gadgets (name) VALUES ('pen') ON DUPLICATE KEY UPDATE note = nope", 1054, "42S22", "Unknown column 'nope' in 'field list'"},
		{"INSERT INTO gadgets (name) VALUES ('pen') ON DUPLICATE KEY UPDATE name = 'ink'", 1062, "23000", "Duplicate entry 'ink' for key 'gadgets.uq'"},
		{"INSERT INTO gadgets (name) SELECT 'pen' ON DUPLICATE KEY UPDATE note = 'z'", 1235, "42000", ""},
		{"INSERT IGNORE INTO gadgets (name) VALUES ('pen') ON DUPLICATE KEY UPDATE note = 'z'", 1235, "42000", ""},
		{"INSERT INTO gadgets (name) VALUES ('pen') ON DUPLICATE KEY UPDATE note = 'a', name = note", 1235, "42000", ""},
		{"SELECT VALUES(name) FROM gadgets", 1235, "42000", ""},
	}
	for _, c := range refused {
		_, err := conn.Execute(c.query)
		e := requireMyError(t, err, c.code, c.state)
		if c.msg != "" && e.Message != c.msg {
			t.Errorf("%s: message %q, want %q", c.query, e.Message, c.msg)
		}
	}
	// The failed statement changed nothing.
	if got := rowsOf(t, execOK(t, conn, "SELECT note FROM gadgets WHERE name = 'pen'")); got != "x+" {
		t.Errorf("pen after failures %q", got)
	}

	// VALUES(ts) is the inserted value, converted like any TIMESTAMP.
	execOK(t, conn, "CREATE TABLE events (k INT PRIMARY KEY, at TIMESTAMP NULL)")
	execOK(t, conn, "SET time_zone = '+02:00'")
	execOK(t, conn, "INSERT INTO events VALUES (1, '2024-01-01 08:00:00')")
	requireOK(t, conn, "INSERT INTO events VALUES (1, '2024-01-01 12:00:00') ON DUPLICATE KEY UPDATE at = VALUES(at)", 2, 0, "")
	if got := rowsOf(t, execOK(t, conn, "SELECT at FROM events")); got != "2024-01-01 12:00:00" {
		t.Errorf("TIMESTAMP %q", got)
	}
	execOK(t, conn, "SET time_zone = '+00:00'")
	if got := rowsOf(t, execOK(t, conn, "SELECT at FROM events")); got != "2024-01-01 10:00:00" {
		t.Errorf("TIMESTAMP stored %q", got)
	}
}

func TestOnDuplicateKeyUpdateFoundRows(t *testing.T) {
	_, addr := shopSetup(t)
	conn := dial(t, addr, mysql.CLIENT_FOUND_ROWS)
	execOK(t, conn, "CREATE TABLE kv (k INT PRIMARY KEY, v INT)")
	execOK(t, conn, "INSERT INTO kv VALUES (1, 1)")
	requireOK(t, conn, "INSERT INTO kv VALUES (1, 1) ON DUPLICATE KEY UPDATE v = 1", 1, 0, "")
	requireOK(t, conn, "INSERT INTO kv VALUES (1, 1) ON DUPLICATE KEY UPDATE v = 2", 2, 0, "")
}

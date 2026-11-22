package wire

import "testing"

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

	requireOK(t, conn, "INSERT IGNORE INTO gadgets (name) VALUES ('ink'), ('cup')", 1, 3, "Records: 2  Duplicates: 1  Warnings: 1")
	if got := rowsOf(t, execOK(t, conn, "SHOW WARNINGS")); got != "Warning,1062,Duplicate entry 'ink' for key 'gadgets.uq'" {
		t.Errorf("warnings %q", got)
	}
	requireOK(t, conn, "INSERT IGNORE INTO gadgets (id, name) VALUES (1, 'x')", 0, 0, "")
	if got := rowsOf(t, execOK(t, conn, "SHOW WARNINGS")); got != "Warning,1062,Duplicate entry '1' for key 'gadgets.PRIMARY'" {
		t.Errorf("warnings %q", got)
	}
	requireOK(t, conn, "INSERT IGNORE INTO gadgets (name) VALUES ('d'), ('d')", 1, 4, "Records: 2  Duplicates: 1  Warnings: 1")
	requireOK(t, conn, "INSERT IGNORE gadgets SET name = 'e'", 1, 5, "")
	if got := rowsOf(t, execOK(t, conn, "SELECT id, name FROM gadgets ORDER BY id")); got != "1,pen;2,ink;3,cup;4,d;5,e" {
		t.Errorf("rows %q", got)
	}
	if got := rowsOf(t, execOK(t, conn, "SELECT LAST_INSERT_ID()")); got != "5" {
		t.Errorf("LAST_INSERT_ID %s", got)
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

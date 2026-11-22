package wire

import "testing"

// INSERT ... SELECT. `oracle: pending`.
func TestInsertSelect(t *testing.T) {
	conn := gadgetServer(t)
	execOK(t, conn, "CREATE TABLE copies (id INT AUTO_INCREMENT PRIMARY KEY, label VARCHAR(20) NOT NULL, price DECIMAL(10,2), at TIMESTAMP NULL)")

	res := execOK(t, conn, "INSERT INTO copies (label, price) SELECT name, price FROM items ORDER BY id")
	if res.AffectedRows != 2 || res.InsertId != 1 {
		t.Errorf("affected %d insert id %d, want 2 1", res.AffectedRows, res.InsertId)
	}
	if got := rowsOf(t, execOK(t, conn, "SELECT LAST_INSERT_ID()")); got != "1" {
		t.Errorf("LAST_INSERT_ID %s", got)
	}
	if got := rowsOf(t, execOK(t, conn, "SELECT id, label, price FROM copies ORDER BY id")); got != "1,pen,1.50;2,città,10.00" {
		t.Errorf("rows %q", got)
	}

	// One row still reports the info string; zero rows insert nothing.
	execOK(t, conn, "INSERT INTO copies (label) SELECT 'x' UNION SELECT 'y'")
	res = execOK(t, conn, "INSERT INTO copies (label) SELECT name FROM gadgets WHERE name = 'nope'")
	if res.AffectedRows != 0 {
		t.Errorf("zero rows: affected %d", res.AffectedRows)
	}
	// The source may be the target table.
	execOK(t, conn, "INSERT INTO copies (label) SELECT label FROM copies WHERE label = 'x'")
	if got := rowsOf(t, execOK(t, conn, "SELECT COUNT(*) FROM copies WHERE label = 'x'")); got != "2" {
		t.Errorf("self insert: %s", got)
	}

	// TIMESTAMP values are converted from the session zone to UTC on the way in.
	execOK(t, conn, "SET time_zone = '+02:00'")
	execOK(t, conn, "INSERT INTO copies (label, at) SELECT 'tz', '2024-01-01 12:00:00'")
	if got := rowsOf(t, execOK(t, conn, "SELECT at FROM copies WHERE label = 'tz'")); got != "2024-01-01 12:00:00" {
		t.Errorf("TIMESTAMP round trip %q", got)
	}
	execOK(t, conn, "SET time_zone = '+00:00'")
	if got := rowsOf(t, execOK(t, conn, "SELECT at FROM copies WHERE label = 'tz'")); got != "2024-01-01 10:00:00" {
		t.Errorf("TIMESTAMP stored %q", got)
	}

	refused := []struct {
		query string
		code  uint16
		state string
		msg   string
	}{
		{"INSERT INTO copies (label) SELECT name, price FROM items", 1136, "21S01", "Column count doesn't match value count at row 1"},
		{"INSERT INTO copies (nope) SELECT name FROM items", 1054, "42S22", "Unknown column 'nope' in 'field list'"},
		{"INSERT INTO copies (label) SELECT nope FROM items", 1054, "42S22", "Unknown column 'nope' in 'field list'"},
		{"INSERT INTO copies (price) SELECT price FROM items", 1364, "HY000", "Field 'label' doesn't have a default value"},
		{"INSERT INTO copies (label) SELECT note FROM items", 1048, "23000", "Column 'label' cannot be null"},
		{"INSERT INTO copies SELECT * FROM copies", 1235, "42000", ""},
		{"INSERT INTO copies (id, label) SELECT id, name FROM items", 1235, "42000", ""},
	}
	for _, c := range refused {
		_, err := conn.Execute(c.query)
		e := requireMyError(t, err, c.code, c.state)
		if c.msg != "" && e.Message != c.msg {
			t.Errorf("%s: message %q, want %q", c.query, e.Message, c.msg)
		}
	}
}

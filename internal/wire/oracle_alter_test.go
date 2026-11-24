package wire

import "testing"

// ALTER TABLE that copies the rows reports the first row, in copy order, that the new
// definition refuses: out of range is 1264, a string too long 1265 (MySQL 8.0.46,
// oracle probes o.84.1 and o.84.2).
func TestAlterCopyRowErrors(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "CREATE TABLE nums (a INT)")
	execOK(t, conn, "INSERT INTO nums VALUES (1), (300), (2)")
	execOK(t, conn, "CREATE TABLE strs (s VARCHAR(10))")
	execOK(t, conn, "INSERT INTO strs VALUES ('a'), ('abcdef')")
	for _, c := range []struct {
		query string
		code  uint16
		state string
		msg   string
	}{
		{"ALTER TABLE nums MODIFY a TINYINT", 1264, "22003", "Out of range value for column 'a' at row 2"},
		{"ALTER TABLE strs MODIFY s VARCHAR(3)", 1265, "01000", "Data truncated for column 's' at row 2"},
	} {
		_, err := conn.Execute(c.query)
		if e := requireMyError(t, err, c.code, c.state); e.Message != c.msg {
			t.Errorf("%s: message %q, want %q", c.query, e.Message, c.msg)
		}
	}
}

// ADD COLUMN DATE / DATETIME NOT NULL without a default gives existing rows the zero
// date, which strict mode with NO_ZERO_DATE refuses for row 1 (MySQL 8.0.46, probes
// o.85.2 and o.85.4). A table without rows takes the column.
func TestAlterAddZeroDate(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "CREATE TABLE full (id INT)")
	execOK(t, conn, "INSERT INTO full VALUES (1)")
	for _, c := range []struct{ query, msg string }{
		{"ALTER TABLE full ADD COLUMN d DATE NOT NULL", "Incorrect date value: '0000-00-00' for column 'd' at row 1"},
		{"ALTER TABLE full ADD COLUMN dt DATETIME NOT NULL", "Incorrect datetime value: '0000-00-00 00:00:00' for column 'dt' at row 1"},
	} {
		_, err := conn.Execute(c.query)
		if e := requireMyError(t, err, 1292, "22007"); e.Message != c.msg {
			t.Errorf("%s: message %q", c.query, e.Message)
		}
	}
	execOK(t, conn, "CREATE TABLE empty (id INT)")
	execOK(t, conn, "ALTER TABLE empty ADD COLUMN d DATE NOT NULL")
	execOK(t, conn, "INSERT INTO empty VALUES (1, '2024-01-02')")
	if got := rowsOf(t, execOK(t, conn, "SELECT id, d FROM empty")); got != "1,2024-01-02" {
		t.Errorf("rows %q", got)
	}
}

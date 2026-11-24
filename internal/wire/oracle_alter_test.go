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

// ALTER TABLE and CREATE INDEX answer with MySQL's info string. A column type change
// copies the rows (affected rows and Records count them); the other forms run in place
// (MySQL 8.0.46, oracle probes o.87.1 to o.87.6).
func TestAlterInfo(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "CREATE TABLE t87 (a INT, s VARCHAR(10))")
	execOK(t, conn, "INSERT INTO t87 VALUES (1, 'x'), (2, 'y')")
	const zero = "Records: 0  Duplicates: 0  Warnings: 0"
	for _, c := range []struct {
		query    string
		affected uint64
		info     string
	}{
		{"ALTER TABLE t87 ADD b INT", 0, zero},
		{"CREATE INDEX i ON t87 (a)", 0, zero},
		{"ALTER TABLE t87 ADD INDEX j (a)", 0, zero},
		{"ALTER TABLE t87 RENAME COLUMN b TO z", 0, zero},
		{"ALTER TABLE t87 ADD UNIQUE KEY k (a)", 0, zero},
		{"ALTER TABLE t87 MODIFY a BIGINT", 2, "Records: 2  Duplicates: 0  Warnings: 0"},
		{"ALTER TABLE t87 MODIFY s VARCHAR(20)", 0, zero},
		{"ALTER TABLE t87 MODIFY s VARCHAR(5)", 2, "Records: 2  Duplicates: 0  Warnings: 0"},
		{"ALTER TABLE t87 MODIFY s VARCHAR(5) NOT NULL", 0, zero},
		{"DROP INDEX j ON t87", 0, zero},
	} {
		requireOK(t, conn, c.query, c.affected, 0, c.info)
	}
}

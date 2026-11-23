package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// CREATE TABLE, the M2 subset (docs/05-dialect.md section 7, Q4). Values, messages and
// metadata are `oracle: pending`.

const gadgets = "CREATE TABLE gadgets (" +
	"id INT UNSIGNED NOT NULL AUTO_INCREMENT, " +
	"name VARCHAR(10) NOT NULL, " +
	"code VARCHAR(8) COLLATE utf8mb4_bin, " +
	"qty TINYINT NOT NULL DEFAULT 1, " +
	"price DECIMAL(8,2) UNSIGNED, " +
	"note TEXT, " +
	"added DATE DEFAULT '2024-01-01', " +
	"PRIMARY KEY (id), UNIQUE KEY (name), KEY by_code (code)" +
	") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='stock' AUTO_INCREMENT=100"

func TestCreateTable(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, gadgets)

	requireOK(t, conn, "INSERT INTO gadgets (name, code) VALUES ('città', 'A1')", 1, 100, "")
	res := execOK(t, conn, "SELECT id, name, code, qty, price, note, added FROM gadgets")
	want := []struct {
		typ     byte
		charset uint16
	}{
		{mysql.MYSQL_TYPE_LONG, 63},
		{mysql.MYSQL_TYPE_VAR_STRING, 45},
		{mysql.MYSQL_TYPE_VAR_STRING, 46}, // utf8mb4_bin
		{mysql.MYSQL_TYPE_TINY, 63},
		{mysql.MYSQL_TYPE_NEWDECIMAL, 63},
		{mysql.MYSQL_TYPE_BLOB, 45},
		{mysql.MYSQL_TYPE_DATE, 63},
	}
	for i, w := range want {
		f := res.Fields[i]
		if f.Type != w.typ || f.Charset != w.charset {
			t.Errorf("%s: want type %d charset %d, got %d %d", f.Name, w.typ, w.charset, f.Type, f.Charset)
		}
	}
	if res.Fields[0].Flag&(mysql.AUTO_INCREMENT_FLAG|mysql.UNSIGNED_FLAG|mysql.PRI_KEY_FLAG) != mysql.AUTO_INCREMENT_FLAG|mysql.UNSIGNED_FLAG|mysql.PRI_KEY_FLAG {
		t.Errorf("id flags: %d", res.Fields[0].Flag)
	}
	if got, _ := res.GetString(0, 6); got != "2024-01-01" {
		t.Errorf("DEFAULT date: got %q", got)
	}
	if got, _ := res.GetInt(0, 3); got != 1 {
		t.Errorf("DEFAULT 1: got %d", got)
	}

	// Text columns compare with mysql_ci; the _bin column does not.
	if n := execOK(t, conn, "SELECT id FROM gadgets WHERE name = 'CITTA '").RowNumber(); n != 1 {
		t.Errorf("mysql_ci equality: want 1 row, got %d", n)
	}
	if n := execOK(t, conn, "SELECT id FROM gadgets WHERE code = 'a1'").RowNumber(); n != 0 {
		t.Errorf("utf8mb4_bin equality: want 0 rows, got %d", n)
	}
	// The unnamed UNIQUE KEY is named after its column.
	_, err := conn.Execute("INSERT INTO gadgets (name) VALUES ('CITTA')")
	if e := requireMyError(t, err, 1062, "23000"); e.Message != "Duplicate entry 'CITTA' for key 'gadgets.name'" {
		t.Errorf("1062 message: %q", e.Message)
	}

	// MySQL's strict range and length checks, with the row number.
	_, err = conn.Execute("INSERT INTO gadgets (name, qty) VALUES ('a', 1), ('b', 128)")
	if e := requireMyError(t, err, 1264, "22003"); e.Message != "Out of range value for column 'qty' at row 2" {
		t.Errorf("1264 message: %q", e.Message)
	}
	_, err = conn.Execute("INSERT INTO gadgets (name) VALUES ('elevenchars')")
	if e := requireMyError(t, err, 1406, "22001"); e.Message != "Data too long for column 'name' at row 1" {
		t.Errorf("1406 message: %q", e.Message)
	}
	// Bytes that are not UTF-8 cannot be stored in a utf8mb4 column (strict mode).
	_, err = conn.Execute("INSERT INTO gadgets (name) VALUES ('ok'), ('citt\xe0')")
	if e := requireMyError(t, err, 1366, "HY000"); e.Message != `Incorrect string value: '\xE0' for column 'name' at row 2` {
		t.Errorf("1366 message: %q", e.Message)
	}
	_, err = conn.Execute("INSERT INTO gadgets (name, price) VALUES ('a', -1)")
	requireMyError(t, err, 1264, "22003")
	// UPDATE reaches the CHECK constraint.
	_, err = conn.Execute("UPDATE gadgets SET qty = 200")
	if e := requireMyError(t, err, 1264, "22003"); e.Message != "Out of range value for column 'qty' at row 1" {
		t.Errorf("UPDATE 1264 message: %q", e.Message)
	}

	// IF NOT EXISTS on an existing table is a Note.
	res = execOK(t, conn, "CREATE TABLE IF NOT EXISTS gadgets (a INT)")
	if res.Warnings != 1 {
		t.Errorf("IF NOT EXISTS: want 1 warning, got %d", res.Warnings)
	}
	res = execOK(t, conn, "SHOW WARNINGS")
	if level, _ := res.GetString(0, 0); level != "Note" {
		t.Errorf("IF NOT EXISTS level: %q", level)
	}
	if code, _ := res.GetInt(0, 1); code != 1050 {
		t.Errorf("IF NOT EXISTS code: %d", code)
	}
}

func TestCreateTableImplicitCommit(t *testing.T) {
	_, addr := shopSetup(t)
	a, b := dial(t, addr), dial(t, addr)
	execOK(t, a, "BEGIN")
	execOK(t, a, "INSERT INTO items (name) VALUES ('ink')")
	res := execOK(t, a, "CREATE TABLE t2 (a INT)")
	if res.Status&mysql.SERVER_STATUS_IN_TRANS != 0 {
		t.Error("CREATE TABLE left the transaction open")
	}
	if n := execOK(t, b, "SELECT id FROM items WHERE name = 'ink'").RowNumber(); n != 1 {
		t.Error("CREATE TABLE did not commit the open transaction")
	}
	execOK(t, b, "INSERT INTO t2 VALUES (1)")
}

func TestCreateTableErrors(t *testing.T) {
	conn := shopServer(t)
	cases := []struct {
		query, state, msg string
		code              uint16
	}{
		{"CREATE TABLE items (a INT)", "42S01", "Table 'items' already exists", 1050},
		{"CREATE TABLE x (a INT, A INT)", "42S21", "Duplicate column name 'A'", 1060},
		{"CREATE TABLE x (a INT, b INT, KEY k (a), KEY k (b))", "42000", "Duplicate key name 'k'", 1061},
		{"CREATE TABLE x (a INT PRIMARY KEY, b INT, PRIMARY KEY (b))", "42000", "Multiple primary key defined", 1068},
		{"CREATE TABLE x (a INT, KEY (b))", "42000", "Key column 'b' doesn't exist in table", 1072},
		{"CREATE TABLE x (a INT AUTO_INCREMENT)", "42000", "Incorrect table definition; there can be only one auto column and it must be defined as a key", 1075},
		{"CREATE TABLE x (a VARCHAR(5) AUTO_INCREMENT PRIMARY KEY)", "42000", "Incorrect column specifier for column 'a'", 1063},
		{"CREATE TABLE x (a TEXT, KEY (a))", "42000", "BLOB/TEXT column 'a' used in key specification without a key length", 1170},
		{"CREATE TABLE x (a VARCHAR(20000))", "42000", "Column length too big for column 'a' (max = 16383); use BLOB or TEXT instead", 1074},
		{"CREATE TABLE x (a INT) ENGINE=Nope", "42000", "Unknown storage engine 'Nope'", 1286},
		{"CREATE TABLE x (a INT NOT NULL DEFAULT NULL)", "42000", "Invalid default value for 'a'", 1067},
		{"CREATE TABLE x (a TINYINT DEFAULT 300)", "42000", "Invalid default value for 'a'", 1067},
		{"CREATE TABLE x (a TEXT DEFAULT 'x')", "42000", "BLOB, TEXT, GEOMETRY or JSON column 'a' can't have a default value", 1101},
		{"CREATE TABLE x (a INT NULL, PRIMARY KEY (a))", "42000", "All parts of a PRIMARY KEY must be NOT NULL; if you need NULL in a key, use UNIQUE instead", 1171},
		{"CREATE TABLE x (a INT, KEY `PRIMARY` (a))", "42000", "Incorrect index name 'PRIMARY'", 1280},
		{"CREATE TABLE x (a VARCHAR(5) COLLATE nope)", "HY000", "Unknown collation: 'nope'", 1273},
		{"CREATE TABLE x (a VARCHAR(5) CHARACTER SET utf8mb4 COLLATE utf8mb3_bin)", "42000", "COLLATION 'utf8mb3_bin' is not valid for CHARACTER SET 'utf8mb4'", 1253},
	}
	for _, c := range cases {
		_, err := conn.Execute(c.query)
		e := requireMyError(t, err, c.code, c.state)
		if e.Message != c.msg {
			t.Errorf("%s: want %q, got %q", c.query, c.msg, e.Message)
		}
	}
	for _, q := range []string{
		"CREATE TABLE x (a ENUM('a','b'))",
		"CREATE TABLE x (a INT, FOREIGN KEY (a) REFERENCES items (id))",
		"CREATE TABLE x (a VARCHAR(5) CHARACTER SET latin1)",
		"CREATE TABLE x (a DATETIME DEFAULT CURRENT_TIMESTAMP)",
		"CREATE TABLE x (a VARCHAR(20), KEY (a(5)))",
		"CREATE TABLE x (a INT) ENGINE=MEMORY",
		"CREATE TABLE x (a INT, b INT, PRIMARY KEY (a, b), KEY (b), c INT AUTO_INCREMENT UNIQUE)",
		"CREATE TABLE x (a INT DEFAULT '5x')",
		"CREATE TABLE x (a DECIMAL(5,1) DEFAULT 1.25)",
		"CREATE TABLE __mysqlite_x (a INT)",
		"CREATE TABLE x SELECT 1",
	} {
		_, err := conn.Execute(q)
		requireMyError(t, err, 1235, "42000")
	}
	// A refused statement leaves no table behind.
	_, err := conn.Execute("SELECT * FROM x")
	requireMyError(t, err, 1146, "42S02")
}

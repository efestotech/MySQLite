package wire

import (
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// Catalog SHOW statements (docs/04-session-catalog.md section 4). Formats, lengths and
// flags are `oracle: pending`.

// column returns the named column of row as text, "NULL" for NULL.
func column(t *testing.T, res *mysql.Result, row int, name string) string {
	t.Helper()
	i, ok := res.FieldNames[name]
	if !ok {
		t.Fatalf("no column %q in %v", name, res.FieldNames)
	}
	v, err := res.GetValue(row, i)
	if err != nil {
		t.Fatal(err)
	}
	if v == nil {
		return "NULL"
	}
	s, _ := res.GetString(row, i)
	return s
}

func firstColumn(res *mysql.Result) []string {
	var out []string
	for i := range res.RowNumber() {
		s, _ := res.GetString(i, 0)
		out = append(out, s)
	}
	return out
}

func TestShowDatabasesAndTables(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, gadgets)

	res := execOK(t, conn, "SHOW DATABASES")
	if got := strings.Join(firstColumn(res), ","); got != "information_schema,shop" {
		t.Errorf("SHOW DATABASES: %s", got)
	}
	if res.Fields[0].Charset != 33 || res.Fields[0].Type != mysql.MYSQL_TYPE_VAR_STRING {
		t.Errorf("SHOW DATABASES metadata: %+v", res.Fields[0])
	}
	res = execOK(t, conn, "SHOW DATABASES LIKE 'SH%'")
	if got := strings.Join(firstColumn(res), ","); got != "shop" || string(res.Fields[0].Name) != "Database (SH%)" {
		t.Errorf("SHOW DATABASES LIKE: %s, column %s", got, res.Fields[0].Name)
	}

	// The sidecar table is hidden.
	res = execOK(t, conn, "SHOW TABLES")
	if got := strings.Join(firstColumn(res), ","); got != "gadgets,items" || string(res.Fields[0].Name) != "Tables_in_shop" {
		t.Errorf("SHOW TABLES: %s, column %s", got, res.Fields[0].Name)
	}
	res = execOK(t, conn, "SHOW FULL TABLES FROM shop WHERE Tables_in_shop = 'ITEMS'")
	if res.RowNumber() != 1 || column(t, res, 0, "Table_type") != "BASE TABLE" {
		t.Errorf("SHOW FULL TABLES WHERE: %d rows", res.RowNumber())
	}
	res = execOK(t, conn, "SHOW TABLES LIKE 'g\\_%'")
	if got := strings.Join(firstColumn(res), ","); got != "" {
		t.Errorf("escaped LIKE: %q", got)
	}
	res = execOK(t, conn, "SHOW TABLES WHERE Tables_in_shop <> 'zz' OR 1 = 0")
	if got := strings.Join(firstColumn(res), ","); got != "gadgets,items" {
		t.Errorf("WHERE predicate: %q", got)
	}

	_, err := conn.Execute("SHOW TABLES WHERE nope = 1")
	if e := requireMyError(t, err, 1054, "42S22"); e.Message != "Unknown column 'nope' in 'where clause'" {
		t.Errorf("1054 message: %q", e.Message)
	}
	_, err = conn.Execute("SHOW TABLES FROM nope")
	requireMyError(t, err, 1049, "42000")
}

func TestShowColumns(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, gadgets)

	res := execOK(t, conn, "SHOW COLUMNS FROM gadgets")
	want := [][]string{
		{"id", "int unsigned", "NO", "PRI", "NULL", "auto_increment"},
		{"name", "varchar(10)", "NO", "UNI", "NULL", ""},
		{"code", "varchar(8)", "YES", "MUL", "NULL", ""},
		{"qty", "tinyint", "NO", "", "1", ""},
		{"price", "decimal(8,2) unsigned", "YES", "", "NULL", ""},
		{"note", "text", "YES", "", "NULL", ""},
		{"added", "date", "YES", "", "2024-01-01", ""},
	}
	if res.RowNumber() != len(want) {
		t.Fatalf("want %d rows, got %d", len(want), res.RowNumber())
	}
	for i, w := range want {
		got := []string{
			column(t, res, i, "Field"), column(t, res, i, "Type"), column(t, res, i, "Null"),
			column(t, res, i, "Key"), column(t, res, i, "Default"), column(t, res, i, "Extra"),
		}
		if strings.Join(got, "|") != strings.Join(w, "|") {
			t.Errorf("row %d: want %v, got %v", i, w, got)
		}
	}

	res = execOK(t, conn, "SHOW FULL COLUMNS FROM shop.gadgets LIKE 'co%'")
	if res.RowNumber() != 1 || column(t, res, 0, "Collation") != "utf8mb4_bin" ||
		column(t, res, 0, "Privileges") != "select,insert,update,references" {
		t.Errorf("SHOW FULL COLUMNS LIKE: %d rows", res.RowNumber())
	}
	// LIKE follows the collation: accents and case are ignored.
	res = execOK(t, conn, "SHOW COLUMNS FROM gadgets LIKE 'NÀM_'")
	if res.RowNumber() != 1 || column(t, res, 0, "Field") != "name" {
		t.Errorf("SHOW COLUMNS LIKE with an accent: %d rows", res.RowNumber())
	}
	res = execOK(t, conn, "DESCRIBE gadgets qty")
	if res.RowNumber() != 1 || column(t, res, 0, "Field") != "qty" {
		t.Errorf("DESCRIBE t col: %d rows", res.RowNumber())
	}
	res = execOK(t, conn, "DESC gadgets")
	if res.RowNumber() != 7 {
		t.Errorf("DESC: %d rows", res.RowNumber())
	}
	res = execOK(t, conn, "SHOW COLUMNS FROM gadgets WHERE `Null` = 'no' AND `Key` <> ''")
	if got := strings.Join(firstColumn(res), ","); got != "id,name" {
		t.Errorf("SHOW COLUMNS WHERE: %q", got)
	}

	// A table mysqlite did not create is described from SQLite's schema.
	res = execOK(t, conn, "SHOW COLUMNS FROM items")
	if column(t, res, 0, "Key") != "PRI" || column(t, res, 0, "Extra") != "auto_increment" {
		t.Errorf("items.id: %s %s", column(t, res, 0, "Key"), column(t, res, 0, "Extra"))
	}

	_, err := conn.Execute("SHOW COLUMNS FROM nope")
	if e := requireMyError(t, err, 1146, "42S02"); e.Message != "Table 'shop.nope' doesn't exist" {
		t.Errorf("1146 message: %q", e.Message)
	}
}

func TestShowCreateTable(t *testing.T) {
	eng, addr := shopSetup(t)
	if err := eng.CreateDatabase("copy"); err != nil {
		t.Fatal(err)
	}
	conn := dial(t, addr)
	execOK(t, conn, gadgets)

	res := execOK(t, conn, "SHOW CREATE TABLE gadgets")
	got := column(t, res, 0, "Create Table")
	want := "CREATE TABLE `gadgets` (\n" +
		"  `id` int unsigned NOT NULL AUTO_INCREMENT,\n" +
		"  `name` varchar(10) COLLATE utf8mb4_general_ci NOT NULL,\n" +
		"  `code` varchar(8) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL,\n" +
		"  `qty` tinyint NOT NULL DEFAULT '1',\n" +
		"  `price` decimal(8,2) unsigned DEFAULT NULL,\n" +
		"  `note` text COLLATE utf8mb4_general_ci,\n" +
		"  `added` date DEFAULT '2024-01-01',\n" +
		"  PRIMARY KEY (`id`),\n" +
		"  UNIQUE KEY `name` (`name`),\n" +
		"  KEY `by_code` (`code`)\n" +
		") ENGINE=InnoDB AUTO_INCREMENT=100 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='stock'"
	if got != want {
		t.Errorf("SHOW CREATE TABLE:\n%s", got)
	}
	if column(t, res, 0, "Table") != "gadgets" {
		t.Errorf("Table: %s", column(t, res, 0, "Table"))
	}

	// The output is re-executable and describes the same table.
	execOK(t, conn, "USE copy")
	execOK(t, conn, got)
	again := column(t, execOK(t, conn, "SHOW CREATE TABLE gadgets"), 0, "Create Table")
	if again != got {
		t.Errorf("round trip:\n%s\nvs\n%s", again, got)
	}
	execOK(t, conn, "INSERT INTO gadgets (name) VALUES ('a')")
	if id := column(t, execOK(t, conn, "SELECT id FROM gadgets"), 0, "id"); id != "100" {
		t.Errorf("AUTO_INCREMENT after round trip: %s", id)
	}
}

func TestShowIndexAndTableStatus(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, gadgets)
	execOK(t, conn, "CREATE TABLE pairs (a INT NOT NULL, b INT, c INT, PRIMARY KEY (a, b), KEY ab (c, a))")

	if n := execOK(t, conn, "SHOW KEYS FROM pairs").RowNumber(); n != 4 {
		t.Errorf("SHOW KEYS: %d rows", n)
	}
	res := execOK(t, conn, "SHOW INDEX FROM pairs")
	want := []string{"PRIMARY|0|1|a|", "PRIMARY|0|2|b|", "ab|1|1|c|YES", "ab|1|2|a|"}
	if res.RowNumber() != len(want) {
		t.Fatalf("SHOW INDEX: %d rows", res.RowNumber())
	}
	for i, w := range want {
		got := strings.Join([]string{
			column(t, res, i, "Key_name"), column(t, res, i, "Non_unique"), column(t, res, i, "Seq_in_index"),
			column(t, res, i, "Column_name"), column(t, res, i, "Null"),
		}, "|")
		if got != w {
			t.Errorf("row %d: want %s, got %s", i, w, got)
		}
	}
	if res.Fields[res.FieldNames["Non_unique"]].Type != mysql.MYSQL_TYPE_LONGLONG {
		t.Error("Non_unique is not numeric")
	}

	execOK(t, conn, "INSERT INTO gadgets (name) VALUES ('a'), ('b')")
	res = execOK(t, conn, "SHOW TABLE STATUS WHERE Name = 'gadgets'")
	if res.RowNumber() != 1 {
		t.Fatalf("SHOW TABLE STATUS: %d rows", res.RowNumber())
	}
	checks := map[string]string{
		"Engine": "InnoDB", "Rows": "2", "Auto_increment": "102", "Comment": "stock",
		"Row_format": "Dynamic", "Collation": "utf8mb4_general_ci", "Create_time": "NULL",
	}
	for name, w := range checks {
		if got := column(t, res, 0, name); got != w {
			t.Errorf("%s: want %s, got %s", name, w, got)
		}
	}
	if column(t, res, 0, "Data_length") == "0" {
		t.Error("Data_length is 0")
	}
	res = execOK(t, conn, "SHOW TABLE STATUS LIKE 'items'")
	if column(t, res, 0, "Engine") != "SQLite" || column(t, res, 0, "Auto_increment") != "3" {
		t.Errorf("items: %s %s", column(t, res, 0, "Engine"), column(t, res, 0, "Auto_increment"))
	}
}

// SHOW reads through the open transaction, so it sees tables created in it... DDL
// commits, so this checks it does not open one.
func TestShowKeepsTransactionState(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "SET autocommit = 0")
	res := execOK(t, conn, "SHOW TABLES")
	if res.Status&mysql.SERVER_STATUS_IN_TRANS != 0 {
		t.Error("SHOW TABLES opened a transaction")
	}
	execOK(t, conn, "BEGIN")
	execOK(t, conn, "INSERT INTO items (name) VALUES ('ink')")
	res = execOK(t, conn, "SHOW TABLE STATUS LIKE 'items'")
	if column(t, res, 0, "Rows") != "3" {
		t.Errorf("Rows inside the transaction: %s", column(t, res, 0, "Rows"))
	}
}

// SHOW CREATE TABLE prints numeric defaults as strings; they are accepted back, and a
// DECIMAL default takes the column's scale.
func TestNumericStringDefaults(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "CREATE TABLE d (n INT DEFAULT '-3', p DECIMAL(6,2) DEFAULT '1.5', q DECIMAL(4,0) DEFAULT 7)")
	res := execOK(t, conn, "SHOW COLUMNS FROM d")
	for i, want := range []string{"-3", "1.50", "7"} {
		if got := column(t, res, i, "Default"); got != want {
			t.Errorf("row %d: want default %s, got %s", i, want, got)
		}
	}
	execOK(t, conn, "INSERT INTO d () VALUES ()")
	if got := column(t, execOK(t, conn, "SELECT p FROM d"), 0, "p"); got != "1.50" {
		t.Errorf("stored default: %s", got)
	}
}

// SHOW CREATE TABLE is re-executable for every type and option CREATE TABLE accepts,
// and prints the same DDL again (docs/04-session-catalog.md section 4).
func TestShowCreateTableRoundTrip(t *testing.T) {
	eng, addr := shopSetup(t)
	conn := dial(t, addr)
	ddl := []string{
		"CREATE TABLE ints (a TINYINT, b SMALLINT UNSIGNED, c MEDIUMINT NOT NULL DEFAULT -5, d INT(5) UNSIGNED ZEROFILL, e BIGINT UNSIGNED, f BOOL DEFAULT 1)",
		"CREATE TABLE nums (a DECIMAL, b NUMERIC(8,2) UNSIGNED DEFAULT 0.5, c REAL, d FLOAT(7,4), e DOUBLE NOT NULL DEFAULT -1.25)",
		"CREATE TABLE strs (a CHAR, b VARCHAR(10) CHARACTER SET utf8mb3, c VARCHAR(5) COLLATE utf8mb4_bin DEFAULT 'it''s', d BINARY(3), e VARBINARY(4), f TINYTEXT, g TEXT(70), h MEDIUMTEXT, i LONGBLOB, j BLOB COMMENT 'raw\\data')",
		"CREATE TABLE times (a DATE DEFAULT '2024-02-29', b DATETIME(3), c TIMESTAMP NULL, d TIME DEFAULT '-01:02:03', e YEAR, f JSON)",
		"CREATE TABLE keyed (a INT NOT NULL, b VARCHAR(8) NOT NULL, c INT, PRIMARY KEY (a, b), UNIQUE KEY uc (c, a), KEY (b), KEY kc (c)) ENGINE=MyISAM ROW_FORMAT=COMPACT COMMENT='k'",
		"CREATE TABLE autos (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, v INT) AUTO_INCREMENT=42 DEFAULT CHARSET=utf8mb3",
	}
	for i, q := range ddl {
		execOK(t, conn, q)
		name := strings.Fields(q)[2]
		first := column(t, execOK(t, conn, "SHOW CREATE TABLE "+name), 0, "Create Table")
		before := catalogSnapshot(t, conn, name)

		copyDB := "rt" + string(rune('a'+i))
		if err := eng.CreateDatabase(copyDB); err != nil {
			t.Fatal(err)
		}
		execOK(t, conn, "USE "+copyDB)
		if _, err := conn.Execute(first); err != nil {
			t.Errorf("%s: re-executing\n%s\n%v", name, first, err)
			execOK(t, conn, "USE shop")
			continue
		}
		again := column(t, execOK(t, conn, "SHOW CREATE TABLE "+name), 0, "Create Table")
		if name == "times" && !strings.Contains(first, "`c` timestamp NULL DEFAULT NULL,") {
			t.Errorf("nullable TIMESTAMP:\n%s", first)
		}
		if again != first {
			t.Errorf("%s: round trip differs\n%s\nvs\n%s", name, first, again)
		}
		if after := catalogSnapshot(t, conn, name); after != before {
			t.Errorf("%s: catalog snapshot differs\n%s\nvs\n%s", name, before, after)
		}
		execOK(t, conn, "USE shop")
	}
}

// catalogSnapshot is everything the catalog reports about a table: its columns, keys
// and options, as text.
func catalogSnapshot(t *testing.T, conn *client.Conn, table string) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{"SHOW FULL COLUMNS FROM " + table, "SHOW INDEX FROM " + table, "SHOW TABLE STATUS LIKE '" + table + "'"} {
		res := execOK(t, conn, q)
		for i := range res.RowNumber() {
			for _, f := range res.Fields {
				switch string(f.Name) {
				case "Data_length", "Index_length", "Avg_row_length":
					continue // page counts, not catalog
				}
				b.WriteString(column(t, res, i, string(f.Name)))
				b.WriteString("|")
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

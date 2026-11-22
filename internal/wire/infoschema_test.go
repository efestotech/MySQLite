package wire

import (
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// information_schema (docs/04-session-catalog.md section 5, ADR 0005). Column lists,
// types and values are `oracle: pending`.

func TestInfoSchemaTables(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, gadgets)

	res := execOK(t, conn, "SELECT TABLE_NAME, ENGINE, TABLE_ROWS, AUTO_INCREMENT, TABLE_COMMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA = 'SHOP' ORDER BY TABLE_NAME")
	if got := strings.Join(firstColumn(res), ","); got != "gadgets,items" {
		t.Fatalf("TABLES: %s", got)
	}
	if column(t, res, 0, "ENGINE") != "InnoDB" || column(t, res, 0, "AUTO_INCREMENT") != "100" || column(t, res, 0, "TABLE_COMMENT") != "stock" {
		t.Errorf("gadgets row: %s %s %s", column(t, res, 0, "ENGINE"), column(t, res, 0, "AUTO_INCREMENT"), column(t, res, 0, "TABLE_COMMENT"))
	}
	if column(t, res, 1, "TABLE_ROWS") != "2" {
		t.Errorf("items TABLE_ROWS: %s", column(t, res, 1, "TABLE_ROWS"))
	}
	f := res.Fields[res.FieldNames["TABLE_ROWS"]]
	if f.Type != mysql.MYSQL_TYPE_LONGLONG || f.Flag&mysql.UNSIGNED_FLAG == 0 {
		t.Errorf("TABLE_ROWS metadata: type %d flags %d", f.Type, f.Flag)
	}
	if res.Fields[0].Charset != 33 {
		t.Errorf("TABLE_NAME charset: %d", res.Fields[0].Charset)
	}

	// Its own tables are listed too, as SYSTEM VIEW.
	res = execOK(t, conn, "SELECT TABLE_TYPE FROM information_schema.tables WHERE table_schema = 'information_schema' AND table_name = 'columns'")
	if res.RowNumber() != 1 || column(t, res, 0, "TABLE_TYPE") != "SYSTEM VIEW" {
		t.Errorf("information_schema.TABLES self row: %d", res.RowNumber())
	}
	res = execOK(t, conn, "SELECT SCHEMA_NAME, DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA ORDER BY 1")
	if got := strings.Join(firstColumn(res), ","); got != "information_schema,shop" {
		t.Errorf("SCHEMATA: %s", got)
	}
}

func TestInfoSchemaColumnsAndKeys(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, gadgets)

	res := execOK(t, conn, "SELECT COLUMN_NAME, ORDINAL_POSITION, COLUMN_DEFAULT, IS_NULLABLE, DATA_TYPE, "+
		"CHARACTER_MAXIMUM_LENGTH, CHARACTER_OCTET_LENGTH, NUMERIC_PRECISION, NUMERIC_SCALE, "+
		"COLLATION_NAME, COLUMN_TYPE, COLUMN_KEY, EXTRA "+
		"FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = 'shop' AND TABLE_NAME = 'gadgets' ORDER BY ORDINAL_POSITION")
	want := []string{
		"id|1|NULL|NO|int|NULL|NULL|10|0|NULL|int unsigned|PRI|auto_increment",
		"name|2|NULL|NO|varchar|10|40|NULL|NULL|utf8mb4_general_ci|varchar(10)|UNI|",
		"code|3|NULL|YES|varchar|8|32|NULL|NULL|utf8mb4_bin|varchar(8)|MUL|",
		"qty|4|1|NO|tinyint|NULL|NULL|3|0|NULL|tinyint||",
		"price|5|NULL|YES|decimal|NULL|NULL|8|2|NULL|decimal(8,2) unsigned||",
		"note|6|NULL|YES|text|65535|65535|NULL|NULL|utf8mb4_general_ci|text||",
		"added|7|2024-01-01|YES|date|NULL|NULL|NULL|NULL|NULL|date||",
	}
	if res.RowNumber() != len(want) {
		t.Fatalf("COLUMNS: %d rows", res.RowNumber())
	}
	for i, w := range want {
		var got []string
		for j := range res.Fields {
			got = append(got, column(t, res, i, string(res.Fields[j].Name)))
		}
		if strings.Join(got, "|") != w {
			t.Errorf("row %d:\nwant %s\ngot  %s", i, w, strings.Join(got, "|"))
		}
	}

	res = execOK(t, conn, "SELECT INDEX_NAME, SEQ_IN_INDEX, COLUMN_NAME, NON_UNIQUE FROM information_schema.STATISTICS WHERE TABLE_NAME = 'gadgets' ORDER BY INDEX_NAME, SEQ_IN_INDEX")
	if got := strings.Join(firstColumn(res), ","); got != "by_code,name,PRIMARY" {
		t.Errorf("STATISTICS: %s", got)
	}
	res = execOK(t, conn, "SELECT CONSTRAINT_NAME, CONSTRAINT_TYPE FROM information_schema.TABLE_CONSTRAINTS WHERE TABLE_NAME = 'gadgets' ORDER BY 1")
	if got := strings.Join(firstColumn(res), ","); got != "name,PRIMARY" {
		t.Errorf("TABLE_CONSTRAINTS: %s", got)
	}
	res = execOK(t, conn, "SELECT COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_NAME = 'gadgets' AND CONSTRAINT_NAME = 'PRIMARY'")
	if got := strings.Join(firstColumn(res), ","); got != "id" {
		t.Errorf("KEY_COLUMN_USAGE: %s", got)
	}
}

func TestInfoSchemaEmptyAndStatic(t *testing.T) {
	conn := shopServer(t)
	for _, tbl := range []string{"VIEWS", "TRIGGERS", "ROUTINES", "PARAMETERS", "EVENTS", "PARTITIONS",
		"REFERENTIAL_CONSTRAINTS", "CHECK_CONSTRAINTS", "SCHEMA_PRIVILEGES", "TABLE_PRIVILEGES"} {
		res := execOK(t, conn, "SELECT * FROM information_schema."+tbl)
		if res.RowNumber() != 0 || len(res.Fields) < 2 {
			t.Errorf("%s: %d rows, %d columns", tbl, res.RowNumber(), len(res.Fields))
		}
	}
	res := execOK(t, conn, "SELECT COLLATION_NAME FROM information_schema.COLLATIONS WHERE IS_DEFAULT = 'Yes' AND CHARACTER_SET_NAME = 'utf8mb4'")
	if got := strings.Join(firstColumn(res), ","); got != "utf8mb4_general_ci" {
		t.Errorf("COLLATIONS: %s", got)
	}
	res = execOK(t, conn, "SELECT MAXLEN FROM information_schema.CHARACTER_SETS WHERE CHARACTER_SET_NAME = 'UTF8MB4'")
	if column(t, res, 0, "MAXLEN") != "4" {
		t.Errorf("CHARACTER_SETS: %s", column(t, res, 0, "MAXLEN"))
	}
	res = execOK(t, conn, "SELECT ENGINE, SUPPORT FROM information_schema.ENGINES")
	if column(t, res, 0, "SUPPORT") != "DEFAULT" {
		t.Errorf("ENGINES: %s", column(t, res, 0, "SUPPORT"))
	}
	res = execOK(t, conn, "SELECT INFO, COMMAND FROM information_schema.PROCESSLIST")
	if res.RowNumber() != 1 || column(t, res, 0, "COMMAND") != "Query" {
		t.Errorf("PROCESSLIST: %d rows", res.RowNumber())
	}
	res = execOK(t, conn, "SELECT PRIVILEGE_TYPE FROM information_schema.USER_PRIVILEGES WHERE PRIVILEGE_TYPE = 'SELECT'")
	if res.RowNumber() != 1 {
		t.Errorf("USER_PRIVILEGES: %d rows", res.RowNumber())
	}
}

func TestInfoSchemaUseShowAndErrors(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "USE INFORMATION_SCHEMA")
	res := execOK(t, conn, "SHOW TABLES LIKE 'tables'")
	if name := string(res.Fields[0].Name); name != "Tables_in_information_schema (tables)" {
		t.Errorf("current database: %q", name)
	}
	res = execOK(t, conn, "SELECT TABLE_NAME FROM TABLES WHERE TABLE_SCHEMA = 'shop'")
	if got := strings.Join(firstColumn(res), ","); got != "items" {
		t.Errorf("unqualified TABLES: %s", got)
	}
	res = execOK(t, conn, "SHOW FULL TABLES LIKE 'SCHEMATA'")
	if res.RowNumber() != 1 || column(t, res, 0, "Table_type") != "SYSTEM VIEW" {
		t.Errorf("SHOW FULL TABLES: %d rows", res.RowNumber())
	}
	res = execOK(t, conn, "SHOW COLUMNS FROM SCHEMATA")
	if got := strings.Join(firstColumn(res), ","); !strings.HasPrefix(got, "CATALOG_NAME,SCHEMA_NAME") {
		t.Errorf("SHOW COLUMNS: %s", got)
	}

	_, err := conn.Execute("SELECT * FROM information_schema.NOPE")
	if e := requireMyError(t, err, 1109, "42S02"); e.Message != "Unknown table 'NOPE' in information_schema" {
		t.Errorf("1109 message: %q", e.Message)
	}
	_, err = conn.Execute("INSERT INTO TABLES (TABLE_NAME) VALUES ('x')")
	if e := requireMyError(t, err, 1044, "42000"); e.Message != "Access denied for user '"+testUser+"'@'%' to database 'information_schema'" {
		t.Errorf("1044 message: %q", e.Message)
	}
	_, err = conn.Execute("CREATE TABLE x (a INT)")
	requireMyError(t, err, 1044, "42000")
	_, err = conn.Execute("SHOW CREATE TABLE TABLES")
	requireMyError(t, err, 1235, "42000")

	// Back to a real database, everything works as before.
	execOK(t, conn, "USE shop")
	if n := execOK(t, conn, "SELECT id FROM items").RowNumber(); n != 2 {
		t.Errorf("after USE shop: %d rows", n)
	}
}

// information_schema sees the tables of an open transaction, through the transaction's
// own connection.
func TestInfoSchemaInsideTransaction(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "BEGIN")
	execOK(t, conn, "INSERT INTO items (name) VALUES ('ink')")
	res := execOK(t, conn, "SELECT TABLE_ROWS FROM information_schema.TABLES WHERE TABLE_NAME = 'items'")
	if column(t, res, 0, "TABLE_ROWS") != "3" {
		t.Errorf("TABLE_ROWS inside the transaction: %s", column(t, res, 0, "TABLE_ROWS"))
	}
	if res.Status&mysql.SERVER_STATUS_IN_TRANS == 0 {
		t.Error("the transaction ended")
	}
	execOK(t, conn, "ROLLBACK")
}

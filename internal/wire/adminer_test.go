package wire

import (
	"strings"
	"testing"
)

// Statements Adminer sends while browsing a database (M2 smoke run). Values are
// `oracle: pending`.

func TestCurrentDatabaseFunction(t *testing.T) {
	_, addr := shopSetup(t)
	conn := dial(t, addr)
	res := execOK(t, conn, "SELECT DATABASE(), SCHEMA()")
	if got := string(res.Fields[0].Name); got != "DATABASE()" {
		t.Errorf("column name: %q", got)
	}
	if column(t, res, 0, "DATABASE()") != "shop" || column(t, res, 0, "SCHEMA()") != "shop" {
		t.Errorf("DATABASE(): %v", res.Values)
	}
	res = execOK(t, conn, "SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE()")
	if got := strings.Join(firstColumn(res), ","); got != "items" {
		t.Errorf("TABLES of DATABASE(): %s", got)
	}
	res = execOK(t, conn, "SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'items' ORDER BY ORDINAL_POSITION")
	if got := strings.Join(firstColumn(res), ","); got != "id,name,price,added,note" {
		t.Errorf("COLUMNS of DATABASE(): %s", got)
	}
	// With no current database, DATABASE() is NULL.
	execOK(t, conn, "USE information_schema")
	res = execOK(t, conn, "SELECT DATABASE()")
	if v, _ := res.GetString(0, 0); v != "information_schema" {
		t.Errorf("after USE: %q", v)
	}
	noDB := connect(t, addr, testUser, testPassword)
	res = execOK(t, noDB, "SELECT DATABASE()")
	if isNull, _ := res.IsNull(0, 0); !isNull {
		t.Errorf("no database: want NULL, got %v", res.Values[0][0].Value())
	}
}

func TestShowTriggersAndEvents(t *testing.T) {
	conn := shopServer(t)
	res := execOK(t, conn, "SHOW TRIGGERS LIKE 'items'")
	if res.RowNumber() != 0 || len(res.Fields) != 11 || string(res.Fields[0].Name) != "Trigger" {
		t.Errorf("SHOW TRIGGERS: %d rows, %d columns", res.RowNumber(), len(res.Fields))
	}
	res = execOK(t, conn, "SHOW EVENTS")
	if res.RowNumber() != 0 || len(res.Fields) != 15 || string(res.Fields[0].Name) != "Db" {
		t.Errorf("SHOW EVENTS: %d rows, %d columns", res.RowNumber(), len(res.Fields))
	}
	execOK(t, conn, "SHOW TRIGGERS FROM information_schema")
	_, err := conn.Execute("SHOW EVENTS FROM nosuch")
	requireMyError(t, err, 1049, "42000")
}

func TestShowCreateDatabase(t *testing.T) {
	conn := shopServer(t)
	res := execOK(t, conn, "SHOW CREATE DATABASE `shop`")
	want := "CREATE DATABASE `shop` /*!40100 DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci */ /*!80016 DEFAULT ENCRYPTION='N' */"
	if got := column(t, res, 0, "Create Database"); got != want || column(t, res, 0, "Database") != "shop" {
		t.Errorf("SHOW CREATE DATABASE: %q", got)
	}
	res = execOK(t, conn, "SHOW CREATE SCHEMA IF NOT EXISTS shop")
	if got := column(t, res, 0, "Create Database"); !strings.HasPrefix(got, "CREATE DATABASE /*!32312 IF NOT EXISTS*/ `shop` ") {
		t.Errorf("IF NOT EXISTS: %q", got)
	}
	_, err := conn.Execute("SHOW CREATE DATABASE nosuch")
	requireMyError(t, err, 1049, "42000")
}

func TestSqlQuoteShowCreate(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "SET sql_quote_show_create = 1, autocommit = 1")
	if got := varText(t, conn, "sql_quote_show_create"); got != "1" {
		t.Errorf("sql_quote_show_create: %q", got)
	}
}

func TestAdminerFieldListQuery(t *testing.T) {
	conn := shopServer(t)
	// Adminer's SQL-editor field list (M2 smoke run, 1235 before M3).
	res := execOK(t, conn, "SELECT TABLE_NAME AS tab, COLUMN_NAME AS field, IS_NULLABLE AS nullable, DATA_TYPE AS type, CHARACTER_MAXIMUM_LENGTH AS length, COLUMN_KEY = 'PRI' AS `primary`\nFROM INFORMATION_SCHEMA.COLUMNS\nWHERE TABLE_SCHEMA = 'shop'\nORDER BY TABLE_NAME, ORDINAL_POSITION")
	if res.RowNumber() != 5 || column(t, res, 0, "field") != "id" || column(t, res, 0, "primary") != "1" || column(t, res, 1, "primary") != "0" {
		t.Errorf("field list: %d rows", res.RowNumber())
	}
}

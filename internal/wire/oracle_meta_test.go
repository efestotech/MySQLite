package wire

import (
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// SHOW PROCESSLIST describes its columns as MySQL 8.0.46 does, and the session running
// it is in State "init" (oracle CLI run with --column-type-info).
func TestShowProcesslistMetadata(t *testing.T) {
	conn := shopServer(t)
	res := execOK(t, conn, "SHOW PROCESSLIST")
	want := []struct {
		name   string
		typ    byte
		length uint32
		flags  uint16
	}{
		{"Id", mysql.MYSQL_TYPE_LONGLONG, 22, mysql.NOT_NULL_FLAG | mysql.BINARY_FLAG | mysql.NUM_FLAG},
		{"User", mysql.MYSQL_TYPE_VAR_STRING, 32 * 3, mysql.NOT_NULL_FLAG},
		{"Host", mysql.MYSQL_TYPE_VAR_STRING, 255 * 3, mysql.NOT_NULL_FLAG},
		{"db", mysql.MYSQL_TYPE_VAR_STRING, 64 * 3, 0},
		{"Command", mysql.MYSQL_TYPE_VAR_STRING, 16 * 3, mysql.NOT_NULL_FLAG},
		{"Time", mysql.MYSQL_TYPE_LONG, 8, mysql.NOT_NULL_FLAG | mysql.BINARY_FLAG | mysql.NUM_FLAG},
		{"State", mysql.MYSQL_TYPE_VAR_STRING, 30 * 3, 0},
		{"Info", mysql.MYSQL_TYPE_VAR_STRING, 100 * 3, 0},
	}
	if len(res.Fields) != len(want) {
		t.Fatalf("%d columns", len(res.Fields))
	}
	for i, w := range want {
		f := res.Fields[i]
		if string(f.Name) != w.name || f.Type != w.typ || f.ColumnLength != w.length || f.Flag != w.flags {
			t.Errorf("column %d: %s type %d length %d flags %#x, want %+v", i, f.Name, f.Type, f.ColumnLength, f.Flag, w)
		}
	}
	if got := rowsOf(t, res); !strings.Contains(got, "Query,0,init,SHOW PROCESSLIST") {
		t.Errorf("rows %q", got)
	}
}

// A column whose collation differs from the table's names its character set and its
// collation, the primary one included (MySQL 8.0.46, oracle probe o.110.1).
func TestShowCreateTablePrimaryCollation(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "CREATE TABLE t110 (a VARCHAR(5) COLLATE utf8mb4_0900_ai_ci, b VARCHAR(5)) DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_general_ci")
	want := "CREATE TABLE `t110` (\n" +
		"  `a` varchar(5) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci DEFAULT NULL,\n" +
		"  `b` varchar(5) COLLATE utf8mb4_general_ci DEFAULT NULL\n" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci"
	if got := showCreate(t, conn, "t110"); got != want {
		t.Errorf("got\n%s", got)
	}
}

// EXPLAIN's partitions column is MEDIUM_BLOB in MySQL 8.0.46 (oracle probe o.64.1).
func TestExplainPartitionsType(t *testing.T) {
	conn := shopServer(t)
	res := execOK(t, conn, "EXPLAIN SELECT * FROM items WHERE id = 1")
	if f := res.Fields[3]; string(f.Name) != "partitions" || f.Type != mysql.MYSQL_TYPE_MEDIUM_BLOB {
		t.Errorf("column 3: %s type %d", f.Name, f.Type)
	}
}

// @@have_query_cache still exists in MySQL 8.0.46, as NO (oracle probe o.23.4).
func TestHaveQueryCache(t *testing.T) {
	conn := shopServer(t)
	if got := rowsOf(t, execOK(t, conn, "SELECT @@have_query_cache")); got != "NO" {
		t.Errorf("got %q", got)
	}
}

// An information_schema column is named as the view defines it, whatever case the
// query wrote; an alias still wins (MySQL 8.0.46, oracle probe o.42.1).
func TestInfoSchemaColumnNames(t *testing.T) {
	conn := shopServer(t)
	res := execOK(t, conn, "SELECT table_name, t.Table_Schema, table_rows AS n, 1 + 1 FROM information_schema.tables t WHERE table_schema = DATABASE() AND table_name = 'items'")
	var names []string
	for _, f := range res.Fields {
		names = append(names, string(f.Name))
	}
	if got := strings.Join(names, ","); got != "TABLE_NAME,TABLE_SCHEMA,n,1 + 1" {
		t.Errorf("names %s", got)
	}
}

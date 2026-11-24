package wire

import (
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// The type codes MySQL 8.0.46 sends for SHOW and information_schema columns (harness
// run of 2026-11-24 against mysql-ref): TEXT columns of the data dictionary are BLOB,
// ENUM columns are STRING, Maxlen is LONG, the table status times are TIMESTAMP and
// DATETIME. Clients that look at the type (a GUI deciding how to show a cell) see what
// MySQL sends.
func TestShowColumnTypes(t *testing.T) {
	conn := dial(t, queueServer(t, 8))
	execOK(t, conn, "CREATE DATABASE app")
	execOK(t, conn, "USE app")
	execOK(t, conn, "CREATE TABLE items (id INT PRIMARY KEY, name VARCHAR(20), note TEXT)")

	names := map[byte]string{
		mysql.MYSQL_TYPE_VAR_STRING: "VAR_STRING", mysql.MYSQL_TYPE_STRING: "STRING",
		mysql.MYSQL_TYPE_BLOB: "BLOB", mysql.MYSQL_TYPE_LONG: "LONG", mysql.MYSQL_TYPE_LONGLONG: "LONGLONG",
		mysql.MYSQL_TYPE_TIMESTAMP: "TIMESTAMP", mysql.MYSQL_TYPE_DATETIME: "DATETIME",
	}
	for _, c := range []struct{ sql, want string }{
		{"DESCRIBE items", "VAR_STRING BLOB VAR_STRING STRING BLOB VAR_STRING"},
		{"SHOW FULL COLUMNS FROM items", "VAR_STRING BLOB VAR_STRING VAR_STRING STRING BLOB VAR_STRING VAR_STRING BLOB"},
		{"SHOW FULL TABLES", "VAR_STRING STRING"},
		{"SHOW CHARACTER SET LIKE 'utf8mb3'", "VAR_STRING VAR_STRING VAR_STRING LONG"},
		{"SHOW TABLE STATUS", "VAR_STRING VAR_STRING LONGLONG STRING LONGLONG LONGLONG LONGLONG LONGLONG LONGLONG LONGLONG LONGLONG TIMESTAMP DATETIME DATETIME VAR_STRING LONGLONG VAR_STRING VAR_STRING"},
		{"SELECT TABLE_NAME, TABLE_TYPE FROM information_schema.TABLES WHERE TABLE_SCHEMA = 'app'", "VAR_STRING STRING"},
		{"SELECT COLUMN_NAME, COLUMN_KEY FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = 'app'", "VAR_STRING STRING"},
		{"SELECT IFNULL(note, 'none'), IFNULL(name, 'none') FROM items", "BLOB VAR_STRING"},
	} {
		res := execOK(t, conn, c.sql)
		var got []string
		for _, f := range res.Fields {
			got = append(got, names[f.Type])
		}
		if g := strings.Join(got, " "); g != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.sql, g, c.want)
		}
	}
}

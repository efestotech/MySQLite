package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// Result metadata of select-list expressions (docs/04-session-catalog.md section 6).
// Types, decimals and values are `oracle: pending`.

func exprServer(t *testing.T) *client.Conn {
	t.Helper()
	conn := shopServer(t)
	execOK(t, conn, "CREATE TABLE nums (i INT NOT NULL, u INT UNSIGNED NOT NULL, b BIGINT, d DECIMAL(10,2), f DOUBLE, s VARCHAR(20), dt DATE)")
	execOK(t, conn, "INSERT INTO nums VALUES (7, 3, 100, 2.50, 1.5, '12', '2024-01-02')")
	return conn
}

func TestExpressionTypes(t *testing.T) {
	conn := exprServer(t)
	const any = -1
	cases := []struct {
		expr     string
		typ      uint8
		decimals int // any: not checked
		unsigned int // 0 signed, 1 unsigned, any: not checked
		value    string
	}{
		{"i + 1", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "8"},
		{"i - 10", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "-3"},
		{"i * 2", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "14"},
		{"u + 1", mysql.MYSQL_TYPE_LONGLONG, 0, 1, "4"},
		{"u * 2", mysql.MYSQL_TYPE_LONGLONG, 0, 1, "6"},
		{"b * b", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "10000"},
		{"-i", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "-7"},
		{"i + u", mysql.MYSQL_TYPE_LONGLONG, 0, 1, "10"},
		{"(i + 1) * 2", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "16"},
		{"1 + 1", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "2"},
		{"d + 1", mysql.MYSQL_TYPE_NEWDECIMAL, 2, 0, "3.50"},
		{"d * 2", mysql.MYSQL_TYPE_NEWDECIMAL, 2, 0, "5.00"},
		{"d * d", mysql.MYSQL_TYPE_NEWDECIMAL, 4, 0, "6.2500"},
		{"d - 0.5", mysql.MYSQL_TYPE_NEWDECIMAL, 2, 0, "2.00"},
		{"d + 1.125", mysql.MYSQL_TYPE_NEWDECIMAL, 3, 0, "3.625"},
		{"i * d", mysql.MYSQL_TYPE_NEWDECIMAL, 2, 0, "17.50"},
		{"-d", mysql.MYSQL_TYPE_NEWDECIMAL, 2, 0, "-2.50"},
		{"2 * 3.5", mysql.MYSQL_TYPE_NEWDECIMAL, 1, 0, "7.0"},
		{"f + 1", mysql.MYSQL_TYPE_DOUBLE, any, 0, "2.5"},
		{"f * d", mysql.MYSQL_TYPE_DOUBLE, any, 0, "3.75"},
		{"s + 1", mysql.MYSQL_TYPE_DOUBLE, any, 0, "13"},
		{"1.5e0 * 2", mysql.MYSQL_TYPE_DOUBLE, any, 0, "3"},
		{"-(1.5e0)", mysql.MYSQL_TYPE_DOUBLE, any, 0, "-1.5"},
		{"i = 7", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "1"},
		{"i <> 7", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "0"},
		{"i < u", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "0"},
		{"d > 2", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "1"},
		{"s = '12'", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "1"},
		{"i IS NULL", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "0"},
		{"b IS NOT NULL", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "1"},
		{"i IN (1, 7)", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "1"},
		{"i NOT IN (1, 2)", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "1"},
		{"i BETWEEN 1 AND 5", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "0"},
		{"NOT (i = 7)", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "0"},
		{"i = 7 AND u = 3", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "1"},
		{"i = 1 OR u = 1", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "0"},
		{"b <=> NULL", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "0"},
		{"(i = 7) + 1", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "2"},
		{"i + 1 = 8", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "1"},
		{"DATABASE() = 'x'", mysql.MYSQL_TYPE_LONGLONG, 0, 0, "0"},
		{"@@auto_increment_increment + 1", mysql.MYSQL_TYPE_LONGLONG, 0, any, "2"},
		{"NULL + 1", mysql.MYSQL_TYPE_DOUBLE, any, 0, ""},
	}
	for _, c := range cases {
		res, err := conn.Execute("SELECT " + c.expr + " FROM nums")
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		f := res.Fields[0]
		if string(f.Name) != c.expr {
			t.Errorf("%s: column name %q", c.expr, f.Name)
		}
		if f.Type != c.typ {
			t.Errorf("%s: type %d, want %d", c.expr, f.Type, c.typ)
		}
		if c.decimals != any && int(f.Decimal) != c.decimals {
			t.Errorf("%s: decimals %d, want %d", c.expr, f.Decimal, c.decimals)
		}
		if u := f.Flag&mysql.UNSIGNED_FLAG != 0; c.unsigned != any && u != (c.unsigned == 1) {
			t.Errorf("%s: unsigned %v", c.expr, u)
		}
		if c.value == "" {
			if isNull, _ := res.IsNull(0, 0); !isNull {
				t.Errorf("%s: want NULL", c.expr)
			}
			continue
		}
		if got, _ := res.GetString(0, 0); got != c.value {
			t.Errorf("%s: value %q, want %q", c.expr, got, c.value)
		}
	}
}

func TestExpressionAliasAndWhere(t *testing.T) {
	conn := exprServer(t)
	res := execOK(t, conn, "SELECT i * 2 AS twice, s FROM nums WHERE i * 2 > 10 ORDER BY twice")
	if string(res.Fields[0].Name) != "twice" || column(t, res, 0, "twice") != "14" {
		t.Errorf("alias: %q %v", res.Fields[0].Name, res.Values)
	}
}

func TestExpressionErrors(t *testing.T) {
	conn := exprServer(t)
	for _, q := range []string{
		// MySQL turns a DATE into the number 20240102; SQLite would compute 2025.
		"SELECT dt + 1 FROM nums",
		"SELECT -dt FROM nums",
		"SELECT i FROM nums WHERE dt + 1 > 0",
		"UPDATE nums SET dt = dt + 1",
		// MySQL raises 1690 when an UNSIGNED subtraction goes negative.
		"SELECT u - 1 FROM nums",
		"SELECT i FROM nums ORDER BY u - i",
		"DELETE FROM nums WHERE u - 5 < 0",
	} {
		_, err := conn.Execute(q)
		requireMyError(t, err, 1235, "42000")
	}
	_, err := conn.Execute("SELECT nosuch + 1 FROM nums")
	if e := requireMyError(t, err, 1054, "42S22"); e.Message != "Unknown column 'nosuch' in 'field list'" {
		t.Errorf("1054 message: %q", e.Message)
	}
	// SQLite turns an overflowing integer product into a REAL; MySQL raises 1690.
	_, err = conn.Execute("SELECT b * 9223372036854775807 FROM nums")
	requireMyError(t, err, 1690, "22003")
}

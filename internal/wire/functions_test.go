package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// Function batch 1 (docs/05-dialect.md section 5): result types and values over the
// nums row (i 7, u 3, b 100, d 2.50, f 1.5, s '12'). `oracle: pending`.
func TestFunctions(t *testing.T) {
	conn := exprServer(t)
	const any = -1
	null := "\x00NULL"
	cases := []struct {
		expr     string
		typ      uint8
		decimals int
		value    string
	}{
		// Control flow: the result type merges the arguments' types.
		{"IFNULL(b, 0)", mysql.MYSQL_TYPE_LONGLONG, 0, "100"},
		{"IFNULL(NULL, 'x')", mysql.MYSQL_TYPE_VAR_STRING, any, "x"},
		{"COALESCE(NULL, d, 1)", mysql.MYSQL_TYPE_NEWDECIMAL, 2, "2.50"},
		// NULLIF has the type of its first argument, here INT.
		{"NULLIF(i, 7)", mysql.MYSQL_TYPE_LONG, 0, null},
		{"NULLIF(i, 8)", mysql.MYSQL_TYPE_LONG, 0, "7"},
		{"IF(i > 5, 'big', 'small')", mysql.MYSQL_TYPE_VAR_STRING, any, "big"},
		{"IF(i > 5, i, d)", mysql.MYSQL_TYPE_NEWDECIMAL, 2, "7.00"},
		// Math.
		{"ABS(-i)", mysql.MYSQL_TYPE_LONGLONG, 0, "7"},
		{"ABS(d - 3)", mysql.MYSQL_TYPE_NEWDECIMAL, 2, "0.50"},
		{"SIGN(-d)", mysql.MYSQL_TYPE_LONGLONG, 0, "-1"},
		{"CEIL(d)", mysql.MYSQL_TYPE_LONGLONG, 0, "3"},
		{"CEILING(-2.5)", mysql.MYSQL_TYPE_LONGLONG, 0, "-2"},
		{"FLOOR(f)", mysql.MYSQL_TYPE_DOUBLE, any, "1"},
		{"SQRT(16)", mysql.MYSQL_TYPE_DOUBLE, any, "4"},
		{"SQRT(-1)", mysql.MYSQL_TYPE_DOUBLE, any, null},
		{"POW(2, 10)", mysql.MYSQL_TYPE_DOUBLE, any, "1024"},
		{"POWER(2, -1)", mysql.MYSQL_TYPE_DOUBLE, any, "0.5"},
		{"EXP(0)", mysql.MYSQL_TYPE_DOUBLE, any, "1"},
		{"LN(1)", mysql.MYSQL_TYPE_DOUBLE, any, "0"},
		{"LOG(2, 8)", mysql.MYSQL_TYPE_DOUBLE, any, "3"},
		{"LOG(0)", mysql.MYSQL_TYPE_DOUBLE, any, null},
		{"LOG2(8)", mysql.MYSQL_TYPE_DOUBLE, any, "3"},
		{"LOG10(1000)", mysql.MYSQL_TYPE_DOUBLE, any, "3"},
		{"PI()", mysql.MYSQL_TYPE_DOUBLE, 6, "3.141593"},
		{"DEGREES(PI())", mysql.MYSQL_TYPE_DOUBLE, any, "180"},
		{"COS(0)", mysql.MYSQL_TYPE_DOUBLE, any, "1"},
		{"ATAN(1, 1)", mysql.MYSQL_TYPE_DOUBLE, any, "0.7853981633974483"},
		{"MOD(i, 3)", mysql.MYSQL_TYPE_LONGLONG, 0, "1"},
		{"MOD(d, 1)", mysql.MYSQL_TYPE_NEWDECIMAL, 2, "0.50"},
		{"MOD(i, 0)", mysql.MYSQL_TYPE_LONGLONG, 0, null},
		{"i % 3", mysql.MYSQL_TYPE_LONGLONG, 0, "1"},
		{"-7 % 3", mysql.MYSQL_TYPE_LONGLONG, 0, "-1"},
		{"7.5 % 2", mysql.MYSQL_TYPE_NEWDECIMAL, 1, "1.5"},
		{"9007199254740993 % 2", mysql.MYSQL_TYPE_LONGLONG, 0, "1"},
		// Strings: Unicode case mapping, bytes vs characters, MySQL number formatting.
		{"UPPER('città')", mysql.MYSQL_TYPE_VAR_STRING, any, "CITTÀ"},
		{"LCASE('CITTÀ')", mysql.MYSQL_TYPE_VAR_STRING, any, "città"},
		{"UCASE(s)", mysql.MYSQL_TYPE_VAR_STRING, any, "12"},
		{"LENGTH('città')", mysql.MYSQL_TYPE_LONGLONG, 0, "6"},
		{"CHAR_LENGTH('città')", mysql.MYSQL_TYPE_LONGLONG, 0, "5"},
		{"LENGTH(d)", mysql.MYSQL_TYPE_LONGLONG, 0, "4"},
		{"CONCAT('a', i, d, f)", mysql.MYSQL_TYPE_VAR_STRING, any, "a72.501.5"},
		{"CONCAT('a', NULL)", mysql.MYSQL_TYPE_VAR_STRING, any, null},
		{"CONCAT(1e20)", mysql.MYSQL_TYPE_VAR_STRING, any, "1e20"},
		{"CONCAT_WS(',', 'a', NULL, d)", mysql.MYSQL_TYPE_VAR_STRING, any, "a,2.50"},
		{"CONCAT_WS(NULL, 'a')", mysql.MYSQL_TYPE_VAR_STRING, any, null},
	}
	for _, c := range cases {
		res, err := conn.Execute("SELECT " + c.expr + " FROM nums")
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		f := res.Fields[0]
		if f.Type != c.typ {
			t.Errorf("%s: type %d, want %d", c.expr, f.Type, c.typ)
		}
		if c.decimals != any && int(f.Decimal) != c.decimals {
			t.Errorf("%s: decimals %d, want %d", c.expr, f.Decimal, c.decimals)
		}
		if c.value == null {
			if isNull, _ := res.IsNull(0, 0); !isNull {
				got, _ := res.GetString(0, 0)
				t.Errorf("%s: want NULL, got %q", c.expr, got)
			}
			continue
		}
		if got, _ := res.GetString(0, 0); got != c.value {
			t.Errorf("%s: value %q, want %q", c.expr, got, c.value)
		}
	}
}

func TestFunctionErrors(t *testing.T) {
	conn := exprServer(t)
	// Wrong argument count is MySQL's 1582.
	_, err := conn.Execute("SELECT ABS(1, 2) FROM nums")
	if e := requireMyError(t, err, 1582, "42000"); e.Message != "Incorrect parameter count in the call to native function 'ABS'" {
		t.Errorf("1582 message: %q", e.Message)
	}
	// Functions outside the table stay 1235, by name.
	_, err = conn.Execute("SELECT FORMAT(d, 1) FROM nums")
	if e := requireMyError(t, err, 1235, "42000"); e.Message != "mysqlite: unsupported construct function FORMAT: not implemented yet" {
		t.Errorf("1235 message: %q", e.Message)
	}
	// Functions work in WHERE too.
	res := execOK(t, conn, "SELECT i FROM nums WHERE UPPER(s) = '12' AND ABS(i) = 7")
	if res.RowNumber() != 1 {
		t.Errorf("WHERE: %d rows", res.RowNumber())
	}
}

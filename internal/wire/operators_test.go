package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// Operators of docs/05-dialect.md section 4 over the nums row (i 7, u 3, b 100, d 2.50,
// f 1.5, s '12'). `oracle: pending`.
func TestOperators(t *testing.T) {
	conn := exprServer(t)
	execOK(t, conn, "CREATE TABLE bins (v VARCHAR(10) COLLATE utf8mb4_bin, c VARCHAR(10))")
	execOK(t, conn, "INSERT INTO bins VALUES ('abc', 'abc')")
	const any = -1
	null := "\x00NULL"
	cases := []struct {
		from, expr string
		typ        uint8
		decimals   int
		value      string
	}{
		// / is never integer division: DECIMAL with 4 more decimals, or DOUBLE.
		{"nums", "i / 2", mysql.MYSQL_TYPE_NEWDECIMAL, 4, "3.5000"},
		{"nums", "5 / 2", mysql.MYSQL_TYPE_NEWDECIMAL, 4, "2.5000"},
		{"nums", "1 / 3", mysql.MYSQL_TYPE_NEWDECIMAL, 4, "0.3333"},
		{"nums", "d / 2", mysql.MYSQL_TYPE_NEWDECIMAL, 6, "1.250000"},
		{"nums", "f / 2", mysql.MYSQL_TYPE_DOUBLE, any, "0.75"},
		{"nums", "i / 0", mysql.MYSQL_TYPE_NEWDECIMAL, 4, null},
		{"nums", "i DIV 2", mysql.MYSQL_TYPE_LONGLONG, 0, "3"},
		{"nums", "-7 DIV 2", mysql.MYSQL_TYPE_LONGLONG, 0, "-3"},
		{"nums", "7.5 DIV 2", mysql.MYSQL_TYPE_LONGLONG, 0, "3"},
		{"nums", "i DIV 0", mysql.MYSQL_TYPE_LONGLONG, 0, null},
		// XOR with NULL propagation.
		{"nums", "1 XOR 0", mysql.MYSQL_TYPE_LONGLONG, 0, "1"},
		{"nums", "i XOR 1", mysql.MYSQL_TYPE_LONGLONG, 0, "0"},
		{"nums", "1 XOR NULL", mysql.MYSQL_TYPE_LONGLONG, 0, null},
		// IS [NOT] TRUE / FALSE are never NULL.
		{"nums", "i IS TRUE", mysql.MYSQL_TYPE_LONGLONG, 0, "1"},
		{"nums", "0 IS TRUE", mysql.MYSQL_TYPE_LONGLONG, 0, "0"},
		{"nums", "NULL IS NOT TRUE", mysql.MYSQL_TYPE_LONGLONG, 0, "1"},
		{"nums", "0 IS FALSE", mysql.MYSQL_TYPE_LONGLONG, 0, "1"},
		{"nums", "NULL IS FALSE", mysql.MYSQL_TYPE_LONGLONG, 0, "0"},
		// CASE: the result type merges the THEN and ELSE branches.
		{"nums", "CASE WHEN i > 5 THEN 'big' ELSE 'small' END", mysql.MYSQL_TYPE_VAR_STRING, any, "big"},
		{"nums", "CASE i WHEN 7 THEN d ELSE 0 END", mysql.MYSQL_TYPE_NEWDECIMAL, 2, "2.50"},
		{"nums", "CASE WHEN i < 0 THEN 1 END", mysql.MYSQL_TYPE_LONGLONG, 0, null},
		// LIKE follows the collation: general_ci folds case and accents, _bin does not.
		{"nums", "'città' LIKE 'CITT%'", mysql.MYSQL_TYPE_LONGLONG, 0, "1"},
		{"nums", "'città' LIKE 'citta'", mysql.MYSQL_TYPE_LONGLONG, 0, "1"},
		{"nums", "s LIKE '1_'", mysql.MYSQL_TYPE_LONGLONG, 0, "1"},
		{"nums", "'abc' NOT LIKE 'a%'", mysql.MYSQL_TYPE_LONGLONG, 0, "0"},
		{"nums", "'a%b' LIKE 'a|%b' ESCAPE '|'", mysql.MYSQL_TYPE_LONGLONG, 0, "1"},
		{"nums", "'axb' LIKE 'a\\_b'", mysql.MYSQL_TYPE_LONGLONG, 0, "0"},
		{"nums", "'a_b' LIKE 'a\\_b'", mysql.MYSQL_TYPE_LONGLONG, 0, "1"},
		{"nums", "NULL LIKE 'a'", mysql.MYSQL_TYPE_LONGLONG, 0, null},
		{"nums", "d LIKE '2.50'", mysql.MYSQL_TYPE_LONGLONG, 0, "1"},
		{"bins", "v LIKE 'ABC'", mysql.MYSQL_TYPE_LONGLONG, 0, "0"},
		{"bins", "c LIKE 'ABC'", mysql.MYSQL_TYPE_LONGLONG, 0, "1"},
		// || is OR by default.
		{"nums", "1 || 0", mysql.MYSQL_TYPE_LONGLONG, 0, "1"},
	}
	for _, c := range cases {
		res, err := conn.Execute("SELECT " + c.expr + " FROM " + c.from)
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
	// LIKE in WHERE.
	if n := execOK(t, conn, "SELECT i FROM nums WHERE s LIKE '1%' AND s NOT LIKE '2%'").RowNumber(); n != 1 {
		t.Errorf("LIKE in WHERE: %d rows", n)
	}
}

func TestDivisionByZeroInWrites(t *testing.T) {
	conn := exprServer(t)
	// Strict mode with ERROR_FOR_DIVISION_BY_ZERO (the default): writes fail.
	for _, q := range []string{
		"UPDATE nums SET b = i / 0",
		"UPDATE nums SET b = i DIV 0",
		"UPDATE nums SET b = MOD(i, 0)",
	} {
		_, err := conn.Execute(q)
		if e := requireMyError(t, err, 1365, "22012"); e.Message != "Division by 0" {
			t.Errorf("%s: message %q", q, e.Message)
		}
	}
	if v := column(t, execOK(t, conn, "SELECT b FROM nums"), 0, "b"); v != "100" {
		t.Errorf("failed UPDATE changed b: %q", v)
	}
	// SELECT and DELETE get NULL: the error is for INSERT and UPDATE.
	execOK(t, conn, "SELECT i / 0 FROM nums")
	if n := execOK(t, conn, "DELETE FROM nums WHERE i / 0 = 1").AffectedRows; n != 0 {
		t.Errorf("DELETE with division by zero: %d rows", n)
	}
	_, err := conn.Execute("INSERT INTO nums (i, u) VALUES (1 DIV 0, 1)")
	requireMyError(t, err, 1365, "22012")
	// Without the mode, the write stores NULL.
	execOK(t, conn, "SET sql_mode = 'STRICT_TRANS_TABLES'")
	execOK(t, conn, "UPDATE nums SET b = i / 0")
	if isNull, _ := execOK(t, conn, "SELECT b FROM nums").IsNull(0, 0); !isNull {
		t.Errorf("non-strict division by zero: want NULL")
	}
}

func TestPipesAsConcat(t *testing.T) {
	conn := exprServer(t)
	execOK(t, conn, "SET sql_mode = 'PIPES_AS_CONCAT'")
	if v, _ := execOK(t, conn, "SELECT 'a' || s FROM nums").GetString(0, 0); v != "a12" {
		t.Errorf("PIPES_AS_CONCAT: %q", v)
	}
}

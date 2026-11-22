package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// STR_TO_DATE and TIME_FORMAT. Expected values follow the MySQL 8.0 manual's examples
// under the default sql_mode (NO_ZERO_DATE, NO_ZERO_IN_DATE). `oracle: pending`.
func TestStrToDateAndTimeFormat(t *testing.T) {
	conn := joinServer(t)
	cases := []struct {
		query, rows string
	}{
		{"SELECT STR_TO_DATE('01,5,2013', '%d,%m,%Y'), STR_TO_DATE('May 1, 2013', '%M %d,%Y'), STR_TO_DATE('a09:30:17', 'a%h:%i:%s'), STR_TO_DATE('09:30:17a', '%h:%i:%s')",
			"2013-05-01,2013-05-01,09:30:17,09:30:17"},
		{"SELECT STR_TO_DATE('2024-01-02 03:04:05 PM', '%Y-%m-%d %r'), STR_TO_DATE('2024-01-02 10:20:30.25', '%Y-%m-%d %H:%i:%s.%f'), STR_TO_DATE('24-1-2', '%y-%c-%e'), STR_TO_DATE('99-12-31', '%y-%m-%d')",
			"2024-01-02 15:04:05,2024-01-02 10:20:30.250000,2024-01-02,1999-12-31"},
		{"SELECT STR_TO_DATE('Feb 29th 2024', '%b %D %Y'), STR_TO_DATE('2024 060', '%Y %j'), STR_TO_DATE('Thursday 2024-02-29', '%W %Y-%m-%d'), STR_TO_DATE('12:00:00 AM', '%r')",
			"2024-02-29,2024-02-29,2024-02-29,00:00:00"},
		{"SELECT STR_TO_DATE('2024-02-30', '%Y-%m-%d'), STR_TO_DATE('2013', '%Y'), STR_TO_DATE(NULL, '%Y'), STR_TO_DATE('x', '%Y'), STR_TO_DATE('2024-13-01', '%Y-%m-%d'), STR_TO_DATE('2024-01-01 25:00', '%Y-%m-%d %H:%i')",
			"NULL,NULL,NULL,NULL,NULL,NULL"},
		{"SELECT STR_TO_DATE('2024-01-02 ', '%Y-%m-%d %%'), STR_TO_DATE('2024-01-02 %', '%Y-%m-%d %%')", "2024-01-02,2024-01-02"},
		{"SELECT TIME_FORMAT('100:00:00', '%H %k %h %I %l'), TIME_FORMAT('10:05:03.25', '%h:%i:%s %p %f'), TIME_FORMAT('15:00:00', '%r %T'), TIME_FORMAT(NULL, '%H'), TIME_FORMAT('2024-01-02 07:08:09', '%H%i%s %%')",
			"100 100 04 04 4,10:05:03 AM 250000,03:00:00 PM 15:00:00,NULL,070809 %"},
		{"SELECT name FROM items WHERE added = STR_TO_DATE('02/01/2024', '%d/%m/%Y')", "pen"},
	}
	for _, c := range cases {
		res, err := conn.Execute(c.query)
		if err != nil {
			t.Errorf("%s: %v", c.query, err)
			continue
		}
		if got := rowsOf(t, res); got != c.rows {
			t.Errorf("%s:\n got %q\nwant %q", c.query, got, c.rows)
		}
	}

	types := []struct {
		query    string
		typ      uint8
		decimals uint8
	}{
		{"SELECT STR_TO_DATE('2024-01-02', '%Y-%m-%d')", mysql.MYSQL_TYPE_DATE, 0},
		{"SELECT STR_TO_DATE('10:20', '%H:%i')", mysql.MYSQL_TYPE_TIME, 0},
		{"SELECT STR_TO_DATE('2024-01-02 10', '%Y-%m-%d %H')", mysql.MYSQL_TYPE_DATETIME, 0},
		{"SELECT STR_TO_DATE('2024-01-02 10:00:00.5', '%Y-%m-%d %T.%f')", mysql.MYSQL_TYPE_DATETIME, 6},
		{"SELECT TIME_FORMAT('10:00:00', '%H')", mysql.MYSQL_TYPE_VAR_STRING, 0},
	}
	for _, c := range types {
		res, err := conn.Execute(c.query)
		if err != nil {
			t.Errorf("%s: %v", c.query, err)
			continue
		}
		if f := res.Fields[0]; f.Type != c.typ || f.Type != mysql.MYSQL_TYPE_VAR_STRING && uint8(f.Decimal) != c.decimals {
			t.Errorf("%s: type %d decimals %d, want %d %d", c.query, f.Type, f.Decimal, c.typ, c.decimals)
		}
	}

	// Formats mysqlite cannot type or does not implement are refused, never guessed.
	refused := []string{
		"SELECT STR_TO_DATE('2024', name) FROM items",
		"SELECT STR_TO_DATE('2024 01', '%X %V')",
		"SELECT STR_TO_DATE('abc', 'abc')",
		"SELECT TIME_FORMAT('10:00:00', '%Y')",
		"SELECT TIME_FORMAT('10:00:00', name) FROM items",
		"SELECT TIME_FORMAT('-01:00:00', '%H')",
	}
	for _, q := range refused {
		_, err := conn.Execute(q)
		requireMyError(t, err, 1235, "42000")
	}
	if _, err := conn.Execute("SET sql_mode = ''"); err != nil {
		t.Fatal(err)
	}
	_, err := conn.Execute("SELECT STR_TO_DATE('9', '%m')")
	requireMyError(t, err, 1235, "42000")
}

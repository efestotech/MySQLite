package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// Date functions, batch A: parts, names, DATE, LAST_DAY, DATEDIFF, DATE_FORMAT,
// UNIX_TIMESTAMP(x) and FROM_UNIXTIME through the session time zone. Invalid dates are
// NULL. `oracle: pending`.
func TestDateFunctions(t *testing.T) {
	conn := joinServer(t)
	const dt = "'2024-02-29 13:45:10'"
	cases := []struct {
		query, rows string
	}{
		{"SELECT YEAR(" + dt + "), MONTH(" + dt + "), DAY(" + dt + "), DAYOFMONTH(" + dt + "), HOUR(" + dt + "), MINUTE(" + dt + "), SECOND(" + dt + ")",
			"2024,2,29,29,13,45,10"},
		{"SELECT DAYOFWEEK(" + dt + "), WEEKDAY(" + dt + "), DAYOFYEAR(" + dt + "), QUARTER(" + dt + "), MICROSECOND('2024-01-01 00:00:00.25')",
			"5,3,60,1,250000"},
		{"SELECT DAYNAME(" + dt + "), MONTHNAME(" + dt + "), DATE(" + dt + "), LAST_DAY('2023-02-10'), LAST_DAY('2024-02-10 08:00:00')",
			"Thursday,February,2024-02-29,2023-02-28,2024-02-29"},
		{"SELECT DATEDIFF('2024-03-01', '2024-02-28'), DATEDIFF('2024-03-01 23:59:59', '2024-03-02')", "2,-1"},
		{"SELECT HOUR('25:30:00'), MINUTE('10:05'), YEAR('2024-01-05')", "25,5,2024"},
		{"SELECT MONTH('2024-13-01'), YEAR(NULL), DAYNAME('nope'), DATE('2024-02-30')", "NULL,NULL,NULL,NULL"},
		{"SELECT DATE_FORMAT('2024-02-29 13:05:09.123456', '%Y-%m-%d %H:%i:%s|%W %M %D %j|%a %b %e %c|%f %p %r %T|%y %% %h %l %k %q')",
			"2024-02-29 13:05:09|Thursday February 29th 060|Thu Feb 29 2|123456 PM 01:05:09 PM 13:05:09|24 % 01 1 13 q"},
		{"SELECT DATE_FORMAT('2024-01-01', '%U %u %V %v %X %x'), DATE_FORMAT('2024-12-30', '%U %u %V %v %X %x')",
			"00 01 53 01 2023 2024,52 53 52 01 2024 2025"},
		{"SELECT DATE_FORMAT('2024-03-01', '%D %D'), DATE_FORMAT('2024-03-02', '%D'), DATE_FORMAT('2024-03-03', '%D'), DATE_FORMAT('2024-03-11', '%D'), DATE_FORMAT('2024-03-22', '%D')",
			"1st 1st,2nd,3rd,11th,22nd"},
		{"SELECT DATE_FORMAT(NULL, '%Y'), DATE_FORMAT('2024-01-01', NULL)", "NULL,NULL"},
		{"SELECT name FROM items WHERE YEAR(added) = 2024 AND MONTH(added) = 2", "città"},
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

	zoned := []struct {
		zone, query, rows string
	}{
		{"+01:00", "SELECT UNIX_TIMESTAMP('2024-01-01 00:00:00'), FROM_UNIXTIME(1704063600)", "1704063600,2024-01-01 00:00:00"},
		{"+00:00", "SELECT UNIX_TIMESTAMP('2024-01-01'), FROM_UNIXTIME(0, '%Y %H'), UNIX_TIMESTAMP(NULL)", "1704067200,1970 00,NULL"},
	}
	for _, c := range zoned {
		execOK(t, conn, "SET time_zone = '"+c.zone+"'")
		res, err := conn.Execute(c.query)
		if err != nil {
			t.Errorf("%s: %v", c.query, err)
			continue
		}
		if got := rowsOf(t, res); got != c.rows {
			t.Errorf("%s %s: rows %q, want %q", c.zone, c.query, got, c.rows)
		}
	}

	types := []struct {
		query string
		typ   uint8
	}{
		{"SELECT DATE('2024-01-01 10:00:00')", mysql.MYSQL_TYPE_DATE},
		{"SELECT LAST_DAY('2024-01-01')", mysql.MYSQL_TYPE_DATE},
		{"SELECT YEAR('2024-01-01')", mysql.MYSQL_TYPE_LONGLONG},
		{"SELECT DAYNAME('2024-01-01')", mysql.MYSQL_TYPE_VAR_STRING},
		{"SELECT DATE_FORMAT('2024-01-01', '%Y')", mysql.MYSQL_TYPE_VAR_STRING},
		{"SELECT FROM_UNIXTIME(0)", mysql.MYSQL_TYPE_DATETIME},
	}
	for _, c := range types {
		res, err := conn.Execute(c.query)
		if err != nil {
			t.Errorf("%s: %v", c.query, err)
			continue
		}
		if f := res.Fields[0]; f.Type != c.typ {
			t.Errorf("%s: type %d, want %d", c.query, f.Type, c.typ)
		}
	}
}

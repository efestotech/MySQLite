package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// Date arithmetic and the remaining parts: DATE_ADD family with INTERVAL units,
// TIMESTAMPDIFF/TIMESTAMPADD, EXTRACT, WEEK modes, TIME, MAKEDATE, PERIOD_DIFF.
// `oracle: pending`.
func TestDateArithmetic(t *testing.T) {
	conn := joinServer(t)
	cases := []struct {
		query, rows string
	}{
		{"SELECT DATE_ADD('2024-01-31', INTERVAL 1 MONTH), DATE_ADD('2024-02-29', INTERVAL 1 YEAR), DATE_SUB('2024-03-01', INTERVAL 1 DAY)",
			"2024-02-29,2025-02-28,2024-02-29"},
		{"SELECT '2024-01-31' + INTERVAL 1 DAY, ADDDATE('2024-01-31', 3), SUBDATE('2024-01-01', INTERVAL 1 WEEK), DATE_ADD('2024-01-15', INTERVAL 1 QUARTER)",
			"2024-02-01,2024-02-03,2023-12-25,2024-04-15"},
		{"SELECT DATE_ADD('2024-01-01', INTERVAL 90 MINUTE), DATE_ADD('2024-01-01 23:00:00', INTERVAL '1:30' HOUR_MINUTE), DATE_SUB('2024-01-01 00:00:00', INTERVAL 1 SECOND)",
			"2024-01-01 01:30:00,2024-01-02 00:30:00,2023-12-31 23:59:59"},
		{"SELECT DATE_ADD('2024-01-01', INTERVAL '1 2:3:4' DAY_SECOND), DATE_ADD('2024-01-01', INTERVAL '1-2' YEAR_MONTH), DATE_ADD('2024-01-01 00:00:00', INTERVAL 1.5 SECOND)",
			"2024-01-02 02:03:04,2025-03-01,2024-01-01 00:00:01.500000"},
		{"SELECT DATE_ADD('2024-01-01', INTERVAL -1 DAY), DATE_ADD(NULL, INTERVAL 1 DAY), DATE_ADD('nope', INTERVAL 1 DAY), DATE_ADD('9999-12-31', INTERVAL 1 DAY)",
			"2023-12-31,NULL,NULL,NULL"},
		{"SELECT TIMESTAMPDIFF(MONTH, '2024-01-31', '2024-02-29'), TIMESTAMPDIFF(MONTH, '2024-01-31', '2024-03-30'), TIMESTAMPDIFF(YEAR, '2000-02-29', '2024-02-28'), TIMESTAMPDIFF(DAY, '2024-03-02', '2024-03-01 12:00:00')",
			"0,1,23,0"},
		{"SELECT TIMESTAMPDIFF(HOUR, '2024-01-01 00:00:00', '2024-01-02 01:59:59'), TIMESTAMPDIFF(SECOND, '2024-01-01', '2024-01-01 00:01:00'), TIMESTAMPDIFF(WEEK, '2024-01-01', '2024-01-15')",
			"25,60,2"},
		{"SELECT TIMESTAMPADD(DAY, 1, '2024-02-28'), TIMESTAMPADD(MINUTE, 30, '2024-02-28 23:45:00')", "2024-02-29,2024-02-29 00:15:00"},
		{"SELECT EXTRACT(YEAR FROM '2024-02-29 13:45:10'), EXTRACT(YEAR_MONTH FROM '2024-02-29'), EXTRACT(DAY_MINUTE FROM '2024-02-29 13:45:10'), EXTRACT(HOUR_SECOND FROM '2024-02-29 03:04:05'), EXTRACT(MICROSECOND FROM '2024-01-01 00:00:00.5')",
			"2024,202402,291345,30405,500000"},
		{"SELECT WEEK('2024-01-01'), WEEK('2024-01-01', 1), WEEK('2024-01-01', 2), WEEK('2024-12-30', 3), WEEKOFYEAR('2024-12-30'), WEEK('2024-12-30', 1)",
			"0,1,53,1,1,53"},
		{"SELECT WEEK('2021-01-01', 4), WEEK('2021-01-01', 5), WEEK('2021-01-01', 6), WEEK('2021-01-01', 7), WEEK('2021-01-03', 6)",
			"0,0,53,52,1"},
		{"SELECT TIME('2024-01-01 10:20:30'), TIME('10:20:30.25'), MAKEDATE(2024, 60), MAKEDATE(24, 1), MAKEDATE(2024, 0), PERIOD_DIFF(202402, 202311), PERIOD_DIFF(2402, 2311)",
			"10:20:30,10:20:30.25,2024-02-29,2024-01-01,NULL,3,3"},
		{"SELECT name FROM items WHERE added < DATE_ADD('2024-01-01', INTERVAL 1 MONTH)", "pen"},
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
		query string
		typ   uint8
	}{
		{"SELECT DATE_ADD(added, INTERVAL 1 DAY) FROM items", mysql.MYSQL_TYPE_DATE},
		{"SELECT DATE_ADD(added, INTERVAL 1 HOUR) FROM items", mysql.MYSQL_TYPE_DATETIME},
		{"SELECT DATE_ADD('2024-01-01', INTERVAL 1 DAY)", mysql.MYSQL_TYPE_VAR_STRING},
		{"SELECT TIMESTAMPDIFF(DAY, added, added) FROM items", mysql.MYSQL_TYPE_LONGLONG},
		{"SELECT MAKEDATE(2024, 1)", mysql.MYSQL_TYPE_DATE},
		{"SELECT TIME('10:00:00')", mysql.MYSQL_TYPE_TIME},
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

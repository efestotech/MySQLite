package wire

import (
	"strconv"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// Current-time functions follow the session time_zone (ADR 0007), one value per
// statement, frozen by SET timestamp as in MySQL. `oracle: pending`.
func TestCurrentTime(t *testing.T) {
	conn := joinServer(t)
	execOK(t, conn, "SET timestamp = 1700000000.123456") // 2023-11-14 22:13:20.123456 UTC
	cases := []struct {
		setup, query, rows string
	}{
		{"SET time_zone = '+00:00'", "SELECT NOW(), CURDATE(), CURTIME(), UTC_TIMESTAMP(), UNIX_TIMESTAMP()",
			"2023-11-14 22:13:20,2023-11-14,22:13:20,2023-11-14 22:13:20,1700000000"},
		{"SET time_zone = '+05:30'", "SELECT NOW(), CURRENT_TIMESTAMP, LOCALTIME, LOCALTIMESTAMP(), SYSDATE()",
			"2023-11-15 03:43:20,2023-11-15 03:43:20,2023-11-15 03:43:20,2023-11-15 03:43:20,2023-11-15 03:43:20"},
		{"SET time_zone = '+05:30'", "SELECT CURRENT_DATE, CURRENT_TIME, UTC_DATE(), UTC_TIME()",
			"2023-11-15,03:43:20,2023-11-14,22:13:20"},
		{"SET time_zone = '-08:00'", "SELECT NOW(3), CURTIME(6), UTC_TIMESTAMP(1)",
			"2023-11-14 14:13:20.123,14:13:20.123456,2023-11-14 22:13:20.1"},
		{"SET time_zone = 'UTC'", "SELECT id FROM items WHERE CURDATE() > '2023-01-01' ORDER BY id", "1;2"},
		{"SET time_zone = 'SYSTEM'", "SELECT NOW()",
			time.Unix(1700000000, 0).In(time.Local).Format("2006-01-02 15:04:05")},
	}
	for _, c := range cases {
		execOK(t, conn, c.setup)
		res, err := conn.Execute(c.query)
		if err != nil {
			t.Errorf("%s: %v", c.query, err)
			continue
		}
		if got := rowsOf(t, res); got != c.rows {
			t.Errorf("%s / %s: rows %q, want %q", c.setup, c.query, got, c.rows)
		}
	}

	types := []struct {
		query string
		typ   uint8
		dec   uint8
	}{
		{"SELECT NOW()", mysql.MYSQL_TYPE_DATETIME, 0},
		{"SELECT NOW(3)", mysql.MYSQL_TYPE_DATETIME, 3},
		{"SELECT CURDATE()", mysql.MYSQL_TYPE_DATE, 0},
		{"SELECT CURTIME(2)", mysql.MYSQL_TYPE_TIME, 2},
		{"SELECT UNIX_TIMESTAMP()", mysql.MYSQL_TYPE_LONGLONG, 0},
	}
	for _, c := range types {
		res, err := conn.Execute(c.query)
		if err != nil {
			t.Errorf("%s: %v", c.query, err)
			continue
		}
		f := res.Fields[0]
		if f.Type != c.typ || f.Decimal != c.dec || f.Flag&mysql.NOT_NULL_FLAG == 0 {
			t.Errorf("%s: type %d decimals %d flags %#x", c.query, f.Type, f.Decimal, f.Flag)
		}
	}

	// time_zone is applied, so setting it raises no warning.
	execOK(t, conn, "SET time_zone = '+01:00'")
	if res := execOK(t, conn, "SHOW WARNINGS"); len(res.Values) != 0 {
		t.Errorf("SET time_zone warned: %s", rowsOf(t, res))
	}

	// Back on the clock: UNIX_TIMESTAMP() is now.
	execOK(t, conn, "SET timestamp = DEFAULT")
	res := execOK(t, conn, "SELECT UNIX_TIMESTAMP()")
	s, _ := res.GetString(0, 0)
	n, _ := strconv.ParseInt(s, 10, 64)
	if d := time.Now().Unix() - n; d < -5 || d > 5 {
		t.Errorf("UNIX_TIMESTAMP() %d, now %d", n, time.Now().Unix())
	}

	_, err := conn.Execute("SELECT NOW(7)")
	if e := requireMyError(t, err, 1426, "42000"); e.Message != "Too-big precision 7 specified for 'now'. Maximum is 6." {
		t.Errorf("1426 message %q", e.Message)
	}
}

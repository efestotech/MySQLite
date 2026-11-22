package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// groupServer has sales: north (pen 2 1.50, ink 3 2.00), south (pen 5 1.50, NULL 1 NULL).
func groupServer(t *testing.T) *client.Conn {
	t.Helper()
	return shopServer(t,
		"CREATE TABLE sales (id INTEGER PRIMARY KEY AUTOINCREMENT, region VARCHAR(10) NOT NULL, item VARCHAR(10), qty INT NOT NULL, price DECIMAL(10,2))",
		"INSERT INTO sales (region, item, qty, price) VALUES ('north', 'pen', 2, 1.50), ('north', 'ink', 3, 2.00), ('south', 'pen', 5, 1.50), ('south', NULL, 1, NULL)",
	)
}

// Aggregates, GROUP BY and HAVING (docs/05-dialect.md section 4). `oracle: pending`.
func TestAggregates(t *testing.T) {
	conn := groupServer(t)
	const any = -1
	types := []struct {
		expr     string
		typ      uint8
		decimals int
		notNull  bool
		value    string
	}{
		{"COUNT(*)", mysql.MYSQL_TYPE_LONGLONG, 0, true, "4"},
		{"COUNT(item)", mysql.MYSQL_TYPE_LONGLONG, 0, true, "3"},
		{"COUNT(DISTINCT item)", mysql.MYSQL_TYPE_LONGLONG, 0, true, "2"},
		{"SUM(qty)", mysql.MYSQL_TYPE_NEWDECIMAL, 0, false, "11"},
		{"SUM(price)", mysql.MYSQL_TYPE_NEWDECIMAL, 2, false, "5.00"},
		{"SUM(DISTINCT qty)", mysql.MYSQL_TYPE_NEWDECIMAL, 0, false, "11"},
		{"AVG(qty)", mysql.MYSQL_TYPE_NEWDECIMAL, 4, false, "2.7500"},
		{"AVG(price)", mysql.MYSQL_TYPE_NEWDECIMAL, 6, false, "1.666667"},
		{"SUM(qty * 0.5e0)", mysql.MYSQL_TYPE_DOUBLE, any, false, "5.5"},
		{"MIN(item)", mysql.MYSQL_TYPE_VAR_STRING, any, false, "ink"},
		{"MAX(qty)", mysql.MYSQL_TYPE_LONG, 0, false, "5"},
		{"MAX(price)", mysql.MYSQL_TYPE_NEWDECIMAL, 2, false, "2.00"},
		{"GROUP_CONCAT(item ORDER BY item)", mysql.MYSQL_TYPE_BLOB, any, false, "ink,pen,pen"},
		{"GROUP_CONCAT(DISTINCT item ORDER BY item DESC)", mysql.MYSQL_TYPE_BLOB, any, false, "pen,ink"},
		{"GROUP_CONCAT(price ORDER BY id)", mysql.MYSQL_TYPE_BLOB, any, false, "1.50,2.00,1.50"},
		{"GROUP_CONCAT(item, '-', qty ORDER BY qty SEPARATOR ';')", mysql.MYSQL_TYPE_BLOB, any, false, "pen-2;ink-3;pen-5"},
		{"COUNT(*) + 1", mysql.MYSQL_TYPE_LONGLONG, 0, true, "5"},
	}
	for _, c := range types {
		res, err := conn.Execute("SELECT " + c.expr + " FROM sales")
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
		if (f.Flag&mysql.NOT_NULL_FLAG != 0) != c.notNull {
			t.Errorf("%s: flags %#x, want NOT NULL %v", c.expr, f.Flag, c.notNull)
		}
		if got := rowsOf(t, res); got != c.value {
			t.Errorf("%s: value %q, want %q", c.expr, got, c.value)
		}
	}

	queries := []struct {
		query, rows string
	}{
		{"SELECT region, SUM(qty) FROM sales GROUP BY region ORDER BY region", "north,5;south,6"},
		{"SELECT region, COUNT(*) AS n FROM sales GROUP BY region HAVING n > 1 ORDER BY region", "north,2;south,2"},
		{"SELECT region, SUM(qty) AS n FROM sales GROUP BY region HAVING n > 5", "south,6"},
		{"SELECT region FROM sales GROUP BY region HAVING SUM(qty) > 5", "south"},
		{"SELECT region FROM sales GROUP BY region HAVING region = 'north'", "north"},
		{"SELECT UPPER(region) AS r, COUNT(*) FROM sales GROUP BY r ORDER BY r", "NORTH,2;SOUTH,2"},
		{"SELECT region, MAX(qty) FROM sales GROUP BY 1 ORDER BY 2", "north,3;south,5"},
		{"SELECT region, item, COUNT(*) FROM sales WHERE item IS NOT NULL GROUP BY region, item ORDER BY region, item", "north,ink,1;north,pen,1;south,pen,1"},
		{"SELECT COUNT(*), SUM(qty), GROUP_CONCAT(item) FROM sales WHERE qty > 100", "0,NULL,NULL"},
		// Columns that depend on a grouped primary key are allowed.
		{"SELECT id, region, COUNT(*) FROM sales GROUP BY id ORDER BY id LIMIT 1", "1,north,1"},
		{"SELECT s.region, COUNT(*) FROM sales s JOIN items i ON i.name = s.item GROUP BY s.region ORDER BY s.region", "north,1;south,1"},
		{"SELECT region, COUNT(*) FROM sales GROUP BY region ORDER BY COUNT(*) DESC, region", "north,2;south,2"},
	}
	for _, c := range queries {
		res, err := conn.Execute(c.query)
		if err != nil {
			t.Errorf("%s: %v", c.query, err)
			continue
		}
		if got := rowsOf(t, res); got != c.rows {
			t.Errorf("%s: rows %q, want %q", c.query, got, c.rows)
		}
	}

	// GROUP_CONCAT stops at group_concat_max_len bytes.
	execOK(t, conn, "SET SESSION group_concat_max_len = 6")
	if got := rowsOf(t, execOK(t, conn, "SELECT GROUP_CONCAT(item ORDER BY id) FROM sales")); got != "pen,in" {
		t.Errorf("group_concat_max_len 6: %q", got)
	}

	// Without ONLY_FULL_GROUP_BY a non-grouped column is accepted.
	execOK(t, conn, "SET sql_mode = ''")
	execOK(t, conn, "SELECT region, item FROM sales GROUP BY region")
}

func TestAggregateErrors(t *testing.T) {
	conn := groupServer(t)
	cases := []struct {
		query   string
		code    uint16
		state   string
		message string
	}{
		{"SELECT region, item FROM sales GROUP BY region", 1055, "42000", "Expression #2 of SELECT list is not in GROUP BY clause and contains nonaggregated column 'shop.sales.item' which is not functionally dependent on columns in GROUP BY clause; this is incompatible with sql_mode=only_full_group_by"},
		{"SELECT s.region, UPPER(s.item) FROM sales s GROUP BY s.region", 1055, "42000", "Expression #2 of SELECT list is not in GROUP BY clause and contains nonaggregated column 'shop.sales.item' which is not functionally dependent on columns in GROUP BY clause; this is incompatible with sql_mode=only_full_group_by"},
		{"SELECT item, COUNT(*) FROM sales", 1140, "42000", "In aggregated query without GROUP BY, expression #1 of SELECT list contains nonaggregated column 'shop.sales.item'; this is incompatible with sql_mode=only_full_group_by"},
		{"SELECT region FROM sales GROUP BY region ORDER BY qty", 1055, "42000", "Expression #1 of ORDER BY clause is not in GROUP BY clause and contains nonaggregated column 'shop.sales.qty' which is not functionally dependent on columns in GROUP BY clause; this is incompatible with sql_mode=only_full_group_by"},
		{"SELECT region FROM sales GROUP BY region HAVING qty > 1", 1054, "42S22", "Unknown column 'qty' in 'having clause'"},
		{"SELECT region FROM sales GROUP BY nope", 1054, "42S22", "Unknown column 'nope' in 'group statement'"},
		{"SELECT region FROM sales WHERE COUNT(*) > 1", 1111, "HY000", "Invalid use of group function"},
		{"SELECT SUM(COUNT(*)) FROM sales", 1111, "HY000", "Invalid use of group function"},
		{"SELECT COUNT(*) FROM sales GROUP BY COUNT(*)", 1111, "HY000", "Invalid use of group function"},
		{"UPDATE sales SET qty = COUNT(*)", 1111, "HY000", "Invalid use of group function"},
		{"SELECT region, COUNT(*) FROM sales GROUP BY region WITH ROLLUP", 1235, "42000", "mysqlite: unsupported construct WITH ROLLUP: not implemented yet"},
		{"SELECT SUM(qty) OVER () FROM sales", 1235, "42000", "mysqlite: unsupported construct window function: not implemented yet"},
		{"SELECT STD(qty) FROM sales", 1235, "42000", "mysqlite: unsupported construct aggregate STDDEV_POP: not implemented yet"},
		{"SELECT GROUP_CONCAT(DISTINCT item SEPARATOR ';') FROM sales", 1235, "42000", "mysqlite: unsupported construct GROUP_CONCAT(DISTINCT) with a separator: SQLite's DISTINCT aggregates take one argument"},
	}
	for _, c := range cases {
		_, err := conn.Execute(c.query)
		if e := requireMyError(t, err, c.code, c.state); e.Message != c.message {
			t.Errorf("%s:\n got %q\nwant %q", c.query, e.Message, c.message)
		}
	}
}

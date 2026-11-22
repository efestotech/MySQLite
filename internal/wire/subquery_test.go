package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// Subqueries over joinServer: items (1 pen 1.50, 2 città 10), orders of item 1 (qty 2,
// 3), stock of item 1. `oracle: pending`.
func TestSubqueries(t *testing.T) {
	conn := joinServer(t)
	cases := []struct {
		query, rows string
	}{
		{"SELECT name, (SELECT SUM(qty) FROM orders o WHERE o.item_id = i.id) FROM items i ORDER BY id", "pen,5;città,NULL"},
		{"SELECT (SELECT name FROM items WHERE id = 2)", "città"},
		{"SELECT (SELECT qty FROM orders WHERE qty > 100)", "NULL"},
		{"SELECT (SELECT MAX(qty) FROM orders) AS m", "3"},
		{"SELECT (SELECT qty FROM orders ORDER BY qty DESC LIMIT 1)", "3"},
		{"SELECT name FROM items WHERE id IN (SELECT item_id FROM orders)", "pen"},
		{"SELECT name FROM items WHERE id NOT IN (SELECT item_id FROM orders)", "città"},
		{"SELECT name FROM items i WHERE EXISTS (SELECT 1 FROM orders o WHERE o.item_id = i.id)", "pen"},
		{"SELECT name FROM items i WHERE NOT EXISTS (SELECT 1 FROM orders o WHERE o.item_id = i.id)", "città"},
		{"SELECT name FROM items WHERE id = ANY (SELECT item_id FROM orders)", "pen"},
		{"SELECT name FROM items WHERE id = SOME (SELECT item_id FROM orders)", "pen"},
		{"SELECT name FROM items WHERE id <> ALL (SELECT item_id FROM orders)", "città"},
		{"SELECT name FROM items WHERE price > (SELECT AVG(price) FROM items)", "città"},
		{"SELECT name FROM items i WHERE EXISTS (SELECT 1 FROM orders o WHERE o.item_id = i.id AND EXISTS (SELECT 1 FROM stock s WHERE s.id = i.id))", "pen"},
		{"SELECT i.id, (SELECT COUNT(*) FROM orders o WHERE o.item_id = i.id) FROM items i GROUP BY i.id ORDER BY i.id", "1,2;2,0"},
		{"SELECT EXISTS (SELECT 1 FROM orders), EXISTS (SELECT 1 FROM orders WHERE qty > 100)", "1,0"},
	}
	for _, c := range cases {
		res, err := conn.Execute(c.query)
		if err != nil {
			t.Errorf("%s: %v", c.query, err)
			continue
		}
		if got := rowsOf(t, res); got != c.rows {
			t.Errorf("%s: rows %q, want %q", c.query, got, c.rows)
		}
	}

	// A scalar subquery has its column's type, nullable; EXISTS is a boolean.
	res := execOK(t, conn, "SELECT (SELECT SUM(qty) FROM orders), (SELECT name FROM items WHERE id = 1), EXISTS (SELECT 1 FROM orders)")
	for i, want := range []struct {
		typ     uint8
		notNull bool
	}{{mysql.MYSQL_TYPE_NEWDECIMAL, false}, {mysql.MYSQL_TYPE_VAR_STRING, false}, {mysql.MYSQL_TYPE_LONGLONG, true}} {
		f := res.Fields[i]
		if f.Type != want.typ || (f.Flag&mysql.NOT_NULL_FLAG != 0) != want.notNull {
			t.Errorf("column %d: type %d flags %#x", i, f.Type, f.Flag)
		}
	}
}

func TestSubqueryErrors(t *testing.T) {
	conn := joinServer(t)
	cases := []struct {
		query   string
		code    uint16
		state   string
		message string
	}{
		{"SELECT (SELECT qty FROM orders)", 1242, "21000", "Subquery returns more than 1 row"},
		{"SELECT name FROM items WHERE price = (SELECT qty FROM orders)", 1242, "21000", "Subquery returns more than 1 row"},
		{"SELECT (SELECT id, qty FROM orders LIMIT 1)", 1241, "21000", "Operand should contain 1 column(s)"},
		{"SELECT name FROM items WHERE id IN (SELECT id, qty FROM orders)", 1241, "21000", "Operand should contain 1 column(s)"},
		{"SELECT name FROM items WHERE id IN (SELECT item_id FROM orders LIMIT 1)", 1235, "42000", "This version of MySQL doesn't yet support 'LIMIT & IN/ALL/ANY/SOME subquery'"},
		{"SELECT name FROM items i WHERE EXISTS (SELECT 1 FROM orders o WHERE o.nope = i.id)", 1054, "42S22", "Unknown column 'o.nope' in 'where clause'"},
		{"SELECT (SELECT nope FROM orders LIMIT 1)", 1054, "42S22", "Unknown column 'nope' in 'field list'"},
		{"SELECT name FROM items WHERE id > ANY (SELECT item_id FROM orders)", 1235, "42000", "mysqlite: unsupported construct > ANY subquery: not implemented yet"},
		{"SELECT (SELECT * FROM stock)", 1235, "42000", "mysqlite: unsupported construct * in a scalar subquery: not implemented yet"},
		{"UPDATE items SET price = 1 WHERE id IN (SELECT item_id FROM orders)", 1235, "42000", "mysqlite: unsupported construct subquery in a write statement: not implemented yet"},
		{"SELECT * FROM (SELECT 1) AS d", 1235, "42000", "mysqlite: unsupported construct derived table: not implemented yet"},
	}
	for _, c := range cases {
		_, err := conn.Execute(c.query)
		if e := requireMyError(t, err, c.code, c.state); e.Message != c.message {
			t.Errorf("%s:\n got %q\nwant %q", c.query, e.Message, c.message)
		}
	}
}

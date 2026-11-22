package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// UNION, INTERSECT and EXCEPT over joinServer: items (1 pen 1.50, 2 città 10), orders
// of item 1 (qty 2, 3), stock (1, 5). `oracle: pending`.
func TestUnion(t *testing.T) {
	conn := joinServer(t)
	cases := []struct {
		query, rows string
	}{
		{"SELECT id, name FROM items UNION SELECT id, qty FROM orders ORDER BY 1, 2", "1,2;1,pen;2,3;2,città"},
		{"SELECT item_id FROM orders UNION ALL SELECT item_id FROM orders", "1;1;1;1"},
		{"SELECT item_id FROM orders UNION SELECT item_id FROM orders", "1"},
		{"SELECT item_id FROM orders UNION DISTINCT SELECT id FROM stock", "1"},
		{"(SELECT id FROM items ORDER BY id DESC LIMIT 1) UNION ALL (SELECT id FROM orders ORDER BY id LIMIT 1)", "2;1"},
		{"SELECT name FROM items UNION SELECT 'zz' ORDER BY name DESC LIMIT 2", "zz;pen"},
		{"SELECT id FROM items UNION SELECT 9 ORDER BY id DESC LIMIT 1 OFFSET 1", "2"},
		{"SELECT id FROM items INTERSECT SELECT item_id FROM orders", "1"},
		{"SELECT id FROM items EXCEPT SELECT item_id FROM orders", "2"},
		{"SELECT id FROM items WHERE id IN (SELECT 1) UNION SELECT 3 ORDER BY 1", "1;3"},
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

	// Column types merge over the SELECTs; names come from the first.
	types := []struct {
		query   string
		name    string
		typ     uint8
		dec     int
		notNull bool
	}{
		{"SELECT qty FROM orders UNION SELECT n FROM stock", "qty", mysql.MYSQL_TYPE_LONG, 0, true},
		{"SELECT qty AS q FROM orders UNION SELECT NULL", "q", mysql.MYSQL_TYPE_LONG, 0, false},
		{"SELECT 1 AS v UNION SELECT 'a'", "v", mysql.MYSQL_TYPE_VAR_STRING, -1, true},
		{"SELECT price FROM items UNION SELECT qty FROM orders", "price", mysql.MYSQL_TYPE_NEWDECIMAL, 2, false},
		{"SELECT qty FROM orders UNION SELECT 1.5e0", "qty", mysql.MYSQL_TYPE_DOUBLE, -1, true},
	}
	for _, c := range types {
		res, err := conn.Execute(c.query)
		if err != nil {
			t.Errorf("%s: %v", c.query, err)
			continue
		}
		f := res.Fields[0]
		if string(f.Name) != c.name || f.Type != c.typ || (c.dec >= 0 && int(f.Decimal) != c.dec) || (f.Flag&mysql.NOT_NULL_FLAG != 0) != c.notNull {
			t.Errorf("%s: name %q type %d decimals %d flags %#x", c.query, f.Name, f.Type, f.Decimal, f.Flag)
		}
		if len(f.Table) != 0 || len(f.OrgTable) != 0 {
			t.Errorf("%s: a UNION column belongs to no table, got %q/%q", c.query, f.Table, f.OrgTable)
		}
	}
}

func TestUnionErrors(t *testing.T) {
	conn := joinServer(t)
	cases := []struct {
		query   string
		code    uint16
		state   string
		message string
	}{
		{"SELECT id FROM items UNION SELECT id, qty FROM orders", 1222, "21000", "The used SELECT statements have a different number of columns"},
		{"SELECT id FROM items UNION SELECT item_id FROM orders ORDER BY items.id", 1250, "42000", "Table 'items' from one of the SELECTs cannot be used in global ORDER clause"},
		{"SELECT id FROM items UNION SELECT item_id FROM orders ORDER BY nope", 1054, "42S22", "Unknown column 'nope' in 'order clause'"},
		{"SELECT id FROM items UNION SELECT nope FROM orders", 1054, "42S22", "Unknown column 'nope' in 'field list'"},
		{"SELECT id FROM items INTERSECT ALL SELECT item_id FROM orders", 1235, "42000", "mysqlite: unsupported construct INTERSECT ALL: not implemented yet"},
		{"SELECT name FROM items WHERE id IN (SELECT 1 UNION SELECT 2)", 1235, "42000", "mysqlite: unsupported construct UNION in a subquery: not implemented yet"},
	}
	for _, c := range cases {
		_, err := conn.Execute(c.query)
		if e := requireMyError(t, err, c.code, c.state); e.Message != c.message {
			t.Errorf("%s:\n got %q\nwant %q", c.query, e.Message, c.message)
		}
	}
}

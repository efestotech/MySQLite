package wire

import (
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// joinServer has items (1 pen, 2 città), orders of item 1 only, and stock of item 1.
func joinServer(t *testing.T) *client.Conn {
	t.Helper()
	return shopServer(t,
		"CREATE TABLE orders (id INTEGER PRIMARY KEY AUTOINCREMENT, item_id INT NOT NULL, qty INT NOT NULL)",
		"INSERT INTO orders (item_id, qty) VALUES (1, 2), (1, 3)",
		"CREATE TABLE stock (id INT NOT NULL, n INT NOT NULL)",
		"INSERT INTO stock VALUES (1, 5)",
	)
}

// rowsOf renders a result as "a,b;c,d", NULL as NULL.
func rowsOf(t *testing.T, res *mysql.Result) string {
	t.Helper()
	var rows []string
	for r := range res.Values {
		var cells []string
		for c := range res.Fields {
			if isNull, _ := res.IsNull(r, c); isNull {
				cells = append(cells, "NULL")
				continue
			}
			v, _ := res.GetString(r, c)
			cells = append(cells, v)
		}
		rows = append(rows, strings.Join(cells, ","))
	}
	return strings.Join(rows, ";")
}

// JOINs (docs/05-dialect.md section 4). `oracle: pending`.
func TestJoins(t *testing.T) {
	conn := joinServer(t)
	cases := []struct {
		query, rows string
	}{
		{"SELECT i.name, o.qty FROM items i JOIN orders o ON o.item_id = i.id ORDER BY o.id", "pen,2;pen,3"},
		{"SELECT i.name, o.qty FROM items i INNER JOIN orders o ON o.item_id = i.id WHERE o.qty > 2", "pen,3"},
		{"SELECT i.name, o.qty FROM items i LEFT JOIN orders o ON o.item_id = i.id ORDER BY i.id, o.id", "pen,2;pen,3;città,NULL"},
		{"SELECT i.name, o.qty FROM orders o RIGHT JOIN items i ON o.item_id = i.id ORDER BY i.id, o.id", "pen,2;pen,3;città,NULL"},
		{"SELECT i.id, o.id FROM items i, orders o ORDER BY i.id, o.id", "1,1;1,2;2,1;2,2"},
		{"SELECT i.id, o.id FROM items i CROSS JOIN orders o WHERE i.id = 2 ORDER BY o.id", "2,1;2,2"},
		{"SELECT id, n FROM items JOIN stock USING (id)", "1,5"},
		{"SELECT items.name, s.n FROM items LEFT JOIN stock s USING (id) ORDER BY items.id", "pen,5;città,NULL"},
		{"SELECT o.* FROM items i JOIN orders o ON o.item_id = i.id ORDER BY o.id", "1,1,2;2,1,3"},
		{"SELECT i.price * o.qty FROM items i JOIN orders o ON o.item_id = i.id ORDER BY o.id", "3.00;4.50"},
		{"SELECT name FROM items JOIN orders ON orders.item_id = items.id AND orders.qty = 3", "pen"},
		// ORDER BY finds a name in the select list before the tables.
		{"SELECT o.id FROM items i JOIN orders o ON o.item_id = i.id ORDER BY id DESC", "2;1"},
		{"SELECT o.qty AS id FROM items i JOIN orders o ON o.item_id = i.id ORDER BY id DESC", "3;2"},
		{"SELECT i.name FROM items i JOIN orders o ON o.item_id = i.id JOIN stock s ON s.id = i.id WHERE o.qty = 2", "pen"},
		{"SELECT i.name, s.n FROM items i LEFT JOIN (orders o JOIN stock s ON s.id = o.item_id) ON o.item_id = i.id AND o.qty = 2 ORDER BY i.id", "pen,5;città,NULL"},
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

	// Column metadata: the visible table name, and NOT NULL dropped on the inner side
	// of an outer join.
	res := execOK(t, conn, "SELECT i.name, o.qty FROM items i LEFT JOIN orders o ON o.item_id = i.id")
	if f := res.Fields[0]; string(f.Table) != "i" || string(f.OrgTable) != "items" || f.Flag&mysql.NOT_NULL_FLAG == 0 {
		t.Errorf("i.name: table %q org %q flags %#x", f.Table, f.OrgTable, f.Flag)
	}
	if f := res.Fields[1]; string(f.Table) != "o" || f.Flag&mysql.NOT_NULL_FLAG != 0 {
		t.Errorf("o.qty on the inner side: table %q flags %#x", f.Table, f.Flag)
	}
	res = execOK(t, conn, "SELECT o.qty FROM items i JOIN orders o ON o.item_id = i.id")
	if f := res.Fields[0]; f.Flag&mysql.NOT_NULL_FLAG == 0 {
		t.Errorf("o.qty in an inner join: flags %#x", f.Flag)
	}
	res = execOK(t, conn, "SELECT * FROM items i JOIN orders o ON o.item_id = i.id")
	if len(res.Fields) != 8 {
		t.Errorf("* over two tables: %d columns", len(res.Fields))
	}
}

func TestJoinErrors(t *testing.T) {
	conn := joinServer(t)
	cases := []struct {
		query   string
		code    uint16
		state   string
		message string
	}{
		{"SELECT id FROM items JOIN orders ON orders.item_id = items.id", 1052, "23000", "Column 'id' in field list is ambiguous"},
		{"SELECT name FROM items JOIN orders ON orders.item_id = items.id WHERE id = 1", 1052, "23000", "Column 'id' in where clause is ambiguous"},
		{"SELECT name FROM items JOIN orders ON orders.item_id = items.id ORDER BY id", 1052, "23000", "Column 'id' in order clause is ambiguous"},
		{"SELECT name FROM items JOIN orders ON item_id = id", 1052, "23000", "Column 'id' in on clause is ambiguous"},
		{"SELECT i.name FROM items i JOIN orders o ON o.nope = i.id", 1054, "42S22", "Unknown column 'o.nope' in 'on clause'"},
		{"SELECT x.name FROM items i JOIN orders o ON o.item_id = i.id", 1054, "42S22", "Unknown column 'x.name' in 'field list'"},
		{"SELECT * FROM items JOIN items", 1066, "42000", "Not unique table/alias: 'items'"},
		{"SELECT * FROM items i JOIN orders i ON 1", 1066, "42000", "Not unique table/alias: 'i'"},
		{"SELECT * FROM items JOIN nope ON 1", 1146, "42S02", "Table 'shop.nope' doesn't exist"},
	}
	for _, c := range cases {
		_, err := conn.Execute(c.query)
		if e := requireMyError(t, err, c.code, c.state); e.Message != c.message {
			t.Errorf("%s: message %q, want %q", c.query, e.Message, c.message)
		}
	}
}

package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// Derived tables over joinServer: items (1 pen 1.50, 2 città 10), orders of item 1
// (qty 2, 3), stock (1, 5). `oracle: pending`.
func TestDerivedTable(t *testing.T) {
	conn := joinServer(t)
	cases := []struct {
		query, rows string
	}{
		{"SELECT d.n FROM (SELECT id AS n FROM items) AS d ORDER BY n", "1;2"},
		{"SELECT * FROM (SELECT item_id, SUM(qty) AS total FROM orders GROUP BY item_id) t", "1,5"},
		{"SELECT i.name, t.total FROM items i JOIN (SELECT item_id, SUM(qty) AS total FROM orders GROUP BY item_id) t ON t.item_id = i.id", "pen,5"},
		{"SELECT COUNT(*) FROM (SELECT id FROM items UNION ALL SELECT item_id FROM orders) u", "4"},
		{"SELECT x FROM (SELECT x FROM (SELECT id AS x FROM items ORDER BY id DESC LIMIT 1) a) b", "2"},
		{"SELECT name FROM items i WHERE EXISTS (SELECT 1 FROM (SELECT item_id FROM orders WHERE item_id = i.id) o)", "pen"},
		{"SELECT d.id, s.n FROM (SELECT id FROM items) d LEFT JOIN stock s ON s.id = d.id ORDER BY d.id", "1,5;2,NULL"},
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

	// A derived column keeps its type and nullability; its table is the alias.
	res, err := conn.Execute("SELECT n, m FROM (SELECT qty AS n, NULL AS m FROM orders) d")
	if err != nil {
		t.Fatal(err)
	}
	n, m := res.Fields[0], res.Fields[1]
	if n.Type != mysql.MYSQL_TYPE_LONG || n.Flag&mysql.NOT_NULL_FLAG == 0 || string(n.Table) != "d" || string(n.Name) != "n" {
		t.Errorf("n: type %d flags %#x table %q", n.Type, n.Flag, n.Table)
	}
	if m.Flag&mysql.NOT_NULL_FLAG != 0 {
		t.Errorf("m: NULL column flagged NOT NULL: %#x", m.Flag)
	}
}

func TestDerivedTableErrors(t *testing.T) {
	conn := joinServer(t)
	cases := []struct {
		query   string
		code    uint16
		state   string
		message string
	}{
		{"SELECT * FROM (SELECT 1)", 1248, "42000", "Every derived table must have its own alias"},
		{"SELECT * FROM (SELECT 1 AS a, 2 AS a) d", 1060, "42S21", "Duplicate column name 'a'"},
		{"SELECT nope FROM (SELECT id FROM items) d", 1054, "42S22", "Unknown column 'nope' in 'field list'"},
		{"SELECT items.id FROM (SELECT id FROM items) d", 1054, "42S22", "Unknown column 'items.id' in 'field list'"},
		{"SELECT 1 FROM (SELECT id FROM items) d JOIN items d", 1066, "42000", "Not unique table/alias: 'd'"},
		{"SELECT * FROM (SELECT nope FROM items) d", 1054, "42S22", "Unknown column 'nope' in 'field list'"},
		{"SELECT * FROM (SELECT table_name FROM information_schema.tables) t", 1235, "42000", "mysqlite: unsupported construct JOIN, UNION or derived table in information_schema: information_schema tables are built one at a time"},
	}
	for _, c := range cases {
		_, err := conn.Execute(c.query)
		if e := requireMyError(t, err, c.code, c.state); e.Message != c.message {
			t.Errorf("%s:\n got %q\nwant %q", c.query, e.Message, c.message)
		}
	}
}

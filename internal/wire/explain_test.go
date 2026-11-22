package wire

import (
	"strings"
	"testing"
)

// EXPLAIN maps SQLite's query plan to MySQL's traditional EXPLAIN columns: one row per
// table access, rows/filtered/key_len/ref unknown (NULL). `oracle: pending`: MySQL
// plans differ, only the shape is MySQL's.
func TestExplain(t *testing.T) {
	conn := joinServer(t)
	if _, err := conn.Execute("CREATE TABLE codes (code VARCHAR(10) NOT NULL UNIQUE, n INT)"); err != nil {
		t.Fatal(err)
	}
	const none = "NULL,NULL,NULL,NULL,NULL"
	cases := []struct {
		query, rows string
	}{
		{"EXPLAIN SELECT * FROM items", "1,SIMPLE,items,NULL,ALL,NULL,NULL," + none},
		{"EXPLAIN SELECT * FROM items WHERE id = 1", "1,SIMPLE,items,NULL,eq_ref,PRIMARY,PRIMARY," + none},
		{"EXPLAIN SELECT i.name FROM items i JOIN orders o ON o.item_id = i.id ORDER BY i.name",
			"1,SIMPLE,o,NULL,ALL,NULL,NULL,NULL,NULL,NULL,NULL,Using filesort;" +
				"1,SIMPLE,i,NULL,eq_ref,PRIMARY,PRIMARY," + none},
		{"EXPLAIN SELECT 1", "1,SIMPLE,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,No tables used"},
		{"EXPLAIN SELECT name FROM items WHERE id IN (SELECT item_id FROM orders)",
			"1,PRIMARY,items,NULL,eq_ref,PRIMARY,PRIMARY," + none + ";" +
				"2,SUBQUERY,orders,NULL,ALL,NULL,NULL," + none},
		{"EXPLAIN SELECT id FROM items UNION SELECT item_id FROM orders",
			"1,PRIMARY,items,NULL,ALL,NULL,NULL," + none + ";" +
				// SQLite merges the two sides, sorting the second.
				"2,UNION,orders,NULL,ALL,NULL,NULL,NULL,NULL,NULL,NULL,Using filesort"},
		{"EXPLAIN SELECT n FROM codes WHERE code = 'a'", "1,SIMPLE,codes,NULL,eq_ref,code,code," + none},
		{"EXPLAIN SELECT code FROM codes WHERE code > 'a'", "1,SIMPLE,codes,NULL,range,code,code,NULL,NULL,NULL,NULL,Using index"},
		{"DESCRIBE SELECT * FROM items", "1,SIMPLE,items,NULL,ALL,NULL,NULL," + none},
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

	res, err := conn.Execute("EXPLAIN SELECT * FROM items")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range res.Fields {
		names = append(names, string(f.Name))
	}
	if got := strings.Join(names, ","); got != "id,select_type,table,partitions,type,possible_keys,key,key_len,ref,rows,filtered,Extra" {
		t.Errorf("columns %s", got)
	}
}

func TestExplainErrors(t *testing.T) {
	conn := joinServer(t)
	cases := []struct {
		query   string
		code    uint16
		state   string
		message string
	}{
		{"EXPLAIN SELECT nope FROM items", 1054, "42S22", "Unknown column 'nope' in 'field list'"},
		{"EXPLAIN FORMAT=JSON SELECT 1", 1235, "42000", "mysqlite: unsupported construct EXPLAIN FORMAT=JSON: not implemented yet"},
		{"EXPLAIN ANALYZE SELECT 1", 1235, "42000", "mysqlite: unsupported construct EXPLAIN ANALYZE: not implemented yet"},
		{"EXPLAIN UPDATE items SET name = 'x'", 1235, "42000", "mysqlite: unsupported construct EXPLAIN of a write statement: not implemented yet"},
	}
	for _, c := range cases {
		_, err := conn.Execute(c.query)
		if e := requireMyError(t, err, c.code, c.state); e.Message != c.message {
			t.Errorf("%s:\n got %q\nwant %q", c.query, e.Message, c.message)
		}
	}
}

package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/config"
	"mysqlite/internal/obs"
)

// DECIMAL is stored and computed as REAL (ADR 0006): the server says so at startup.
func TestDecimalStartupWarning(t *testing.T) {
	var buf bytes.Buffer
	_, err := NewServer(Config{
		ServerVersion:    "8.0.36-mysqlite",
		MaxAllowedPacket: 64 << 20,
		Users:            map[string]string{testUser: testPassword},
		Engine:           newEngine(t),
	}, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "DECIMAL") || !strings.Contains(out, "README.md") {
		t.Errorf("no DECIMAL warning at startup: %q", out)
	}
}

// ROUND and TRUNCATE: exact numbers (DECIMAL, integers) round half away from zero on
// their decimal digits, DOUBLE rounds half to even, as MySQL. `oracle: pending`.
func TestRoundTruncate(t *testing.T) {
	conn := joinServer(t)
	cases := []struct {
		query, rows string
	}{
		{"SELECT ROUND(1.005, 2), ROUND(2.675, 2), ROUND(2.5), ROUND(-2.5)", "1.01,2.68,3,-3"},
		{"SELECT ROUND(2.5e0), ROUND(3.5e0), ROUND(-2.5e0)", "2,4,-2"},
		{"SELECT ROUND(1234, -2), ROUND(1250, -2), ROUND(-1250, -2), ROUND(15, 1)", "1200,1300,-1300,15"},
		{"SELECT ROUND(price, 1), ROUND(price) FROM items ORDER BY id", "1.5,2;10.0,10"},
		{"SELECT ROUND(price * 1.1, 2) FROM items ORDER BY id", "1.65;11.00"},
		{"SELECT ROUND(NULL), ROUND(1.5, NULL)", "NULL,NULL"},
		{"SELECT TRUNCATE(1.999, 2), TRUNCATE(-1.999, 1), TRUNCATE(1234, -2), TRUNCATE(1.999e0, 2)", "1.99,-1.9,1200,1.99"},
		{"SELECT name FROM items WHERE ROUND(price) = 2", "pen"},
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

	types := []struct {
		query string
		typ   uint8
		dec   int
	}{
		{"SELECT ROUND(price, 1) FROM items", mysql.MYSQL_TYPE_NEWDECIMAL, 1},
		{"SELECT ROUND(price) FROM items", mysql.MYSQL_TYPE_NEWDECIMAL, 0},
		{"SELECT ROUND(qty) FROM orders", mysql.MYSQL_TYPE_LONGLONG, 0},
		{"SELECT ROUND(1.5e0, 1)", mysql.MYSQL_TYPE_DOUBLE, -1},
		{"SELECT TRUNCATE(price, 3) FROM items", mysql.MYSQL_TYPE_NEWDECIMAL, 3},
	}
	for _, c := range types {
		res, err := conn.Execute(c.query)
		if err != nil {
			t.Errorf("%s: %v", c.query, err)
			continue
		}
		f := res.Fields[0]
		if f.Type != c.typ || (c.dec >= 0 && int(f.Decimal) != c.dec) {
			t.Errorf("%s: type %d decimals %d", c.query, f.Type, f.Decimal)
		}
	}
}

// A statement whose arithmetic involves a DECIMAL is flagged in the journal: its result
// was computed in binary floating point.
func TestDecimalJournalFlag(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	j, err := obs.NewJournal(config.Log{Dir: dir, MaxSizeMB: 1, MaxFiles: 1, Journal: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	e := newEngine(t)
	seed(t, e, "shop", "CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, qty INT, price DECIMAL(10,2))",
		"INSERT INTO items (qty, price) VALUES (1, 1.5)")
	addr := startServerWith(t, Config{
		ServerVersion:    "8.0.36-mysqlite",
		MaxAllowedPacket: 64 << 20,
		Users:            map[string]string{testUser: testPassword},
		Engine:           e,
		Journal:          j,
	})
	conn := dial(t, addr)
	want := map[string]bool{
		"SELECT price * 2 FROM items":                     true,
		"SELECT SUM(price) FROM items":                    true,
		"SELECT id FROM items WHERE price - 1 > 0":        true,
		"SELECT qty * 2 FROM items":                       false,
		"SELECT price FROM items":                         false,
		"UPDATE items SET price = price + 1 WHERE id = 1": true,
	}
	for q := range want {
		if _, err := conn.Execute(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	f, err := os.Open(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	seen := 0
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		q, _ := rec["sql"].(string)
		w, ok := want[q]
		if !ok {
			continue
		}
		seen++
		if got := rec["decimal_as_real"] == true; got != w {
			t.Errorf("%s: decimal_as_real %v, want %v", q, rec["decimal_as_real"], w)
		}
	}
	if seen != len(want) {
		t.Errorf("journal has %d of %d statements", seen, len(want))
	}
}

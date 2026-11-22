package wire

import (
	"context"
	"testing"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/engine"
	"mysqlite/internal/meta"
)

// seed runs statements on the writer of database name, bypassing the wire layer:
// writes through the protocol arrive with T8.
func seed(t *testing.T, e *engine.Engine, name string, stmts ...string) {
	t.Helper()
	ctx := context.Background()
	db, err := e.Database(name)
	if err != nil {
		t.Fatal(err)
	}
	w, err := db.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, s := range stmts {
		if _, err := w.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
}

func shopServer(t *testing.T, extra ...string) *client.Conn {
	t.Helper()
	e := newEngine(t)
	seed(t, e, "shop", append([]string{
		"CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, name VARCHAR(20) NOT NULL, price DECIMAL(10,2), added DATE, note TEXT)",
		"INSERT INTO items (name, price, added, note) VALUES ('pen', 1.5, '2024-01-02', NULL), ('città', 10, '2024-02-03', 'x')",
	}, extra...)...)
	addr := startServerWith(t, Config{
		ServerVersion:    "8.0.36-mysqlite",
		MaxAllowedPacket: 64 << 20,
		Users:            map[string]string{testUser: testPassword},
		Engine:           e,
	})
	conn, err := client.Connect(addr, testUser, testPassword, "shop")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestSelectFromTable(t *testing.T) {
	conn := shopServer(t)

	res, err := conn.Execute("SELECT id, name, price, added, note FROM items WHERE price > 1 ORDER BY id")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	want := [][]string{{"1", "pen", "1.50", "2024-01-02", ""}, {"2", "città", "10.00", "2024-02-03", "x"}}
	if res.RowNumber() != len(want) {
		t.Fatalf("want %d rows, got %d", len(want), res.RowNumber())
	}
	for r, row := range want {
		for c, v := range row {
			got, err := res.GetString(r, c)
			if err != nil {
				t.Fatal(err)
			}
			if got != v {
				t.Errorf("row %d column %d: want %q, got %q", r, c, v, got)
			}
		}
	}
	if null, _ := res.IsNull(0, 4); !null {
		t.Error("note of row 1 must be NULL")
	}

	types := []uint8{meta.TypeLong, meta.TypeVarString, meta.TypeNewDecimal, meta.TypeDate, meta.TypeBlob}
	for i, f := range res.Fields {
		if f.Type != types[i] {
			t.Errorf("column %s: want type %d, got %d", f.Name, types[i], f.Type)
		}
		if string(f.Table) != "items" || string(f.Schema) != "shop" {
			t.Errorf("column %s: want shop.items, got %s.%s", f.Name, f.Schema, f.Table)
		}
	}
	if res.Fields[0].Flag&meta.FlagPriKey == 0 || res.Fields[0].Flag&meta.FlagAutoIncrement == 0 {
		t.Errorf("id flags %#x must include PRI_KEY and AUTO_INCREMENT", res.Fields[0].Flag)
	}
}

func TestSelectStarAndAlias(t *testing.T) {
	conn := shopServer(t)

	res, err := conn.Execute("SELECT * FROM items LIMIT 1")
	if err != nil {
		t.Fatalf("select *: %v", err)
	}
	if len(res.Fields) != 5 || res.RowNumber() != 1 {
		t.Fatalf("want 5 columns and 1 row, got %d and %d", len(res.Fields), res.RowNumber())
	}
	res, err = conn.Execute("SELECT i.name AS n FROM items AS i WHERE i.id = 2")
	if err != nil {
		t.Fatalf("select alias: %v", err)
	}
	f := res.Fields[0]
	if string(f.Name) != "n" || string(f.OrgName) != "name" || string(f.Table) != "i" || string(f.OrgTable) != "items" {
		t.Fatalf("unexpected field names %s/%s %s/%s", f.Name, f.OrgName, f.Table, f.OrgTable)
	}
	if got, _ := res.GetString(0, 0); got != "città" {
		t.Fatalf("want città, got %q", got)
	}
}

func TestSelectWithoutFrom(t *testing.T) {
	conn := shopServer(t)

	res, err := conn.Execute("SELECT 2, 'a', 1.5, NULL, -3")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	for i, want := range []string{"2", "a", "1.5", "", "-3"} {
		got, _ := res.GetString(0, i)
		if got != want {
			t.Errorf("column %d: want %q, got %q", i, want, got)
		}
	}
	if null, _ := res.IsNull(0, 3); !null {
		t.Error("NULL must be NULL")
	}
	// The client reformats DOUBLE values, so check the bytes sent.
	doubles, err := conn.Execute("SELECT 1e20, 0.5e0")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if got, want := string(doubles.RowDatas[0]), "1e200.5"; got != want {
		t.Errorf("want row % x, got % x", want, got)
	}
	names := []string{"2", "a", "1.5", "NULL", "-3"}
	for i, f := range res.Fields {
		if string(f.Name) != names[i] {
			t.Errorf("column %d: want name %q, got %q", i, names[i], f.Name)
		}
	}
}

func TestSelectErrors(t *testing.T) {
	conn := shopServer(t)

	_, err := conn.Execute("SELECT * FROM missing")
	myErr := requireMyError(t, err, mysql.ER_NO_SUCH_TABLE, "42S02")
	if myErr.Message != "Table 'shop.missing' doesn't exist" {
		t.Errorf("unexpected message %q", myErr.Message)
	}
	_, err = conn.Execute("SELECT nope FROM items")
	myErr = requireMyError(t, err, mysql.ER_BAD_FIELD_ERROR, "42S22")
	if myErr.Message != "Unknown column 'nope' in 'field list'" {
		t.Errorf("unexpected message %q", myErr.Message)
	}
	// SQLite reports any unknown column without the clause, and accepts a select-list
	// alias in WHERE, which MySQL rejects.
	for q, want := range map[string]string{
		"SELECT id FROM items WHERE nope = 1":         "Unknown column 'nope' in 'where clause'",
		"SELECT id FROM items ORDER BY nope":          "Unknown column 'nope' in 'order clause'",
		"SELECT name AS n FROM items WHERE n = 'pen'": "Unknown column 'n' in 'where clause'",
	} {
		_, err = conn.Execute(q)
		myErr = requireMyError(t, err, mysql.ER_BAD_FIELD_ERROR, "42S22")
		if myErr.Message != want {
			t.Errorf("%s: want %q, got %q", q, want, myErr.Message)
		}
	}
	res, err := conn.Execute("SELECT name AS n FROM items ORDER BY n DESC")
	if err != nil {
		t.Fatalf("ORDER BY a select-list alias: %v", err)
	}
	if got, _ := res.GetString(0, 0); got != "pen" {
		t.Errorf("want pen first, got %q", got)
	}

	_, err = conn.Execute("SELECT * FROM nodb.items")
	requireMyError(t, err, mysql.ER_BAD_DB_ERROR, "42000")

	if err := conn.Ping(); err != nil {
		t.Fatalf("connection must stay usable: %v", err)
	}
}

// A value the declared type cannot hold is an error, never a reformatted value.
func TestSelectRejectsValuesOutsideTheDeclaredType(t *testing.T) {
	// SQLite keeps 'abc' as TEXT in a DECIMAL column; MySQL would have refused it.
	conn := shopServer(t, "INSERT INTO items (name, price) VALUES ('bad', 'abc')")

	_, err := conn.Execute("SELECT price FROM items WHERE name = 'bad'")
	requireMyError(t, err, mysql.ER_UNKNOWN_ERROR, "HY000")
	if err := conn.Ping(); err != nil {
		t.Fatalf("connection must stay usable: %v", err)
	}
}

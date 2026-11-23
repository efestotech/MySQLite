package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// Prepared statements through the binary protocol (ADR 0009, M4 T2). go-mysql's client
// sends COM_STMT_PREPARE/EXECUTE/CLOSE and decodes binary rows independently of the
// server's encoder.

func prepare(t *testing.T, conn *client.Conn, q string) *client.Stmt {
	t.Helper()
	st, err := conn.Prepare(q)
	if err != nil {
		t.Fatalf("prepare %s: %v", q, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func execute(t *testing.T, st *client.Stmt, args ...any) *mysql.Result {
	t.Helper()
	res, err := st.Execute(args...)
	if err != nil {
		t.Fatalf("execute %v: %v", args, err)
	}
	return res
}

// rowStrings is a binary result set's rows as text.
func rowStrings(res *mysql.Result) [][]string {
	var out [][]string
	for _, row := range res.Values {
		var r []string
		for _, v := range row {
			r = append(r, decoded(v))
		}
		out = append(out, r)
	}
	return out
}

func sameRows(t *testing.T, what string, got [][]string, want ...[]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %q, want %q", what, got, want)
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("%s: %q, want %q", what, got, want)
		}
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Fatalf("%s: %q, want %q", what, got, want)
			}
		}
	}
}

func TestPreparedCRUD(t *testing.T) {
	conn := shopServer(t)
	ins := prepare(t, conn, "INSERT INTO items (name, price, added, note) VALUES (?, ?, ?, ?)")
	if ins.ParamNum() != 4 || ins.ColumnNum() != 0 {
		t.Fatalf("insert: %d params, %d columns", ins.ParamNum(), ins.ColumnNum())
	}
	res := execute(t, ins, `o'b\x`, mysql.TypedBytes{Type: mysql.MYSQL_TYPE_NEWDECIMAL, Bytes: []byte("-3.25")},
		mysql.TypedBytes{Type: mysql.MYSQL_TYPE_DATE, Bytes: []byte{0xe8, 0x07, 1, 2}}, nil)
	if res.AffectedRows != 1 || res.InsertId != 3 {
		t.Fatalf("insert: %d rows, id %d", res.AffectedRows, res.InsertId)
	}
	execute(t, ins, "pad", 7, mysql.TypedBytes{Type: mysql.MYSQL_TYPE_DATETIME, Bytes: []byte{0xe8, 0x07, 3, 4, 10, 20, 30}}, "n")

	sel := prepare(t, conn, "SELECT id, name, price, added, note FROM items WHERE name = ?")
	if sel.ParamNum() != 1 || sel.ColumnNum() != 5 {
		t.Fatalf("select: %d params, %d columns", sel.ParamNum(), sel.ColumnNum())
	}
	res = execute(t, sel, `o'b\x`)
	sameRows(t, "select", rowStrings(res), []string{"3", `o'b\x`, "-3.25", "2024-01-02", "NULL"})
	if f := res.Fields; f[0].Type != mysql.MYSQL_TYPE_LONG || f[2].Type != mysql.MYSQL_TYPE_NEWDECIMAL || f[3].Type != mysql.MYSQL_TYPE_DATE {
		t.Errorf("types %d %d %d", f[0].Type, f[2].Type, f[3].Type)
	}
	// The same statement runs again with another value.
	sameRows(t, "select again", rowStrings(execute(t, sel, "pad")), []string{"4", "pad", "7.00", "2024-03-04", "n"})

	upd := prepare(t, conn, "UPDATE items SET price = price + ? WHERE id = ?")
	if res := execute(t, upd, 1.5, int64(4)); res.AffectedRows != 1 {
		t.Fatalf("update: %d rows", res.AffectedRows)
	}
	del := prepare(t, conn, "DELETE FROM items WHERE id = ? OR name = ?")
	if res := execute(t, del, 3, nil); res.AffectedRows != 1 {
		t.Fatalf("delete: %d rows", res.AffectedRows)
	}
	sameRows(t, "after", rowStrings(execute(t, prepare(t, conn, "SELECT id, price FROM items WHERE id > ? ORDER BY id"), 2)),
		[]string{"4", "8.50"})
}

// A value is data, never SQL: quotes and backslashes are escaped by the session's
// NO_BACKSLASH_ESCAPES.
func TestPreparedStringEscaping(t *testing.T) {
	conn := shopServer(t)
	sel := prepare(t, conn, "SELECT COUNT(*) FROM items WHERE name = ?")
	sameRows(t, "injection", rowStrings(execute(t, sel, "x' OR '1'='1")), []string{"0"})

	echo := prepare(t, conn, "SELECT ?")
	for _, v := range []string{`a\b'c`, `\'`, "nul\x00byte", `ends\`} {
		sameRows(t, v, rowStrings(execute(t, echo, v)), []string{v})
	}
	execOK(t, conn, "SET sql_mode = 'NO_BACKSLASH_ESCAPES'")
	echo2 := prepare(t, conn, "SELECT ?")
	for _, v := range []string{`a\b'c`, `\'`, `ends\`} {
		sameRows(t, "no backslash escapes "+v, rowStrings(execute(t, echo2, v)), []string{v})
	}
}

func TestPreparedParameterTypes(t *testing.T) {
	conn := shopServer(t)
	st := prepare(t, conn, "SELECT ? + 1")
	res := execute(t, st, 41)
	sameRows(t, "int", rowStrings(res), []string{"42"})
	if res.Fields[0].Type != mysql.MYSQL_TYPE_LONGLONG {
		t.Errorf("int + 1: type %d", res.Fields[0].Type)
	}
	sameRows(t, "negative", rowStrings(execute(t, prepare(t, conn, "SELECT -?"), -5)), []string{"5"})
	sameRows(t, "uint64", rowStrings(execute(t, prepare(t, conn, "SELECT ?"), uint64(1<<62))), []string{"4611686018427387904"})
	// Above 2^63 the literal is refused, as in COM_QUERY (an M3 limit).
	_, err := prepare(t, conn, "SELECT ?").Execute(uint64(1 << 63))
	requireMyError(t, err, 1235, "42000")
	sameRows(t, "double", rowStrings(execute(t, prepare(t, conn, "SELECT ? * 2"), 0.25)), []string{"0.5"})
	sameRows(t, "null", rowStrings(execute(t, prepare(t, conn, "SELECT ? IS NULL"), nil)), []string{"1"})
	tm := mysql.TypedBytes{Type: mysql.MYSQL_TYPE_TIME, Bytes: []byte{1, 1, 0, 0, 0, 2, 0, 1}}
	sameRows(t, "time", rowStrings(execute(t, prepare(t, conn, "SELECT ?"), tm)), []string{"-26:00:01"})
	ts := mysql.TypedBytes{Type: mysql.MYSQL_TYPE_DATETIME, Bytes: []byte{0xe8, 0x07, 1, 2, 10, 20, 30, 0xf4, 0x01, 0, 0}}
	sameRows(t, "datetime", rowStrings(execute(t, prepare(t, conn, "SELECT ?"), ts)), []string{"2024-01-02 10:20:30.000500"})

	bad := prepare(t, conn, "SELECT ?")
	_, err = bad.Execute(mysql.TypedBytes{Type: mysql.MYSQL_TYPE_NEWDECIMAL, Bytes: []byte("1; DROP TABLE items")})
	requireMyError(t, err, 1210, "HY000")
	_, err = bad.Execute(mysql.TypedBytes{Type: mysql.MYSQL_TYPE_DATE, Bytes: []byte{1, 2, 3}})
	requireMyError(t, err, 1210, "HY000")
}

// 05 section 10: LIMIT with placeholders.
func TestPreparedLimit(t *testing.T) {
	conn := shopServer(t)
	st := prepare(t, conn, "SELECT name FROM items ORDER BY id LIMIT ? OFFSET ?")
	sameRows(t, "limit 1 offset 1", rowStrings(execute(t, st, 1, 1)), []string{"città"})
	sameRows(t, "limit as text", rowStrings(execute(t, st, "1", "0")), []string{"pen"})
	sameRows(t, "limit 0", rowStrings(execute(t, st, 0, 0)))
	_, err := st.Execute(-1, 0)
	requireMyError(t, err, 1210, "HY000")
	_, err = st.Execute(nil, 0)
	requireMyError(t, err, 1210, "HY000")
	sameRows(t, "limit a, b", rowStrings(execute(t, prepare(t, conn, "SELECT id FROM items ORDER BY id LIMIT ?, ?"), 1, 5)), []string{"2"})
}

// PREPARE checks the statement and describes its result columns without running it.
func TestPreparedPrepareErrors(t *testing.T) {
	conn := shopServer(t)
	for _, c := range []struct {
		q     string
		code  uint16
		state string
	}{
		{"SELECT nope FROM items WHERE id = ?", 1054, "42S22"},
		{"SELECT * FROM missing WHERE id = ?", 1146, "42S02"},
		{"SELECT ? FROM", 1064, "42000"},
		{"SELECT 1; SELECT ?", 1064, "42000"},
	} {
		_, err := conn.Prepare(c.q)
		requireMyError(t, err, c.code, c.state)
	}
	// Preparing a write does not run it.
	prepare(t, conn, "DELETE FROM items WHERE id > ?")
	res := execOK(t, conn, "SELECT COUNT(*) FROM items")
	if n, _ := res.GetInt(0, 0); n != 2 {
		t.Fatalf("prepare ran the DELETE: %d rows left", n)
	}
	// SHOW is described and run through the binary protocol too.
	show := prepare(t, conn, "SHOW TABLES")
	if show.ColumnNum() != 1 {
		t.Fatalf("SHOW TABLES: %d columns", show.ColumnNum())
	}
	sameRows(t, "show", rowStrings(execute(t, show)), []string{"items"})
}

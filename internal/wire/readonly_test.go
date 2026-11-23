package wire

import "testing"

// docs/06-transactions.md section 7: --readonly refuses every write with 1290.
func TestReadOnlyServer(t *testing.T) {
	e := newEngine(t)
	seed(t, e, "shop", "CREATE TABLE items (id INTEGER PRIMARY KEY, name VARCHAR(20))", "INSERT INTO items VALUES (1, 'pen')")
	addr := startServerWith(t, Config{
		ServerVersion:    "8.0.36-mysqlite",
		MaxAllowedPacket: 64 << 20,
		Users:            map[string]string{testUser: testPassword},
		Engine:           e,
		ReadOnly:         true,
	})
	conn := dial(t, addr)
	for _, q := range []string{
		"INSERT INTO items VALUES (2, 'x')",
		"UPDATE items SET name = 'y'",
		"DELETE FROM items",
		"REPLACE INTO items VALUES (1, 'z')",
		"CREATE TABLE t2 (a INT)",
		"ALTER TABLE items ADD COLUMN b INT",
		"DROP TABLE items",
		"CREATE INDEX i ON items (name)",
	} {
		_, err := conn.Execute(q)
		requireMyError(t, err, 1290, "HY000")
	}
	if _, err := conn.Prepare("INSERT INTO items VALUES (?, ?)"); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	st := prepare(t, conn, "INSERT INTO items VALUES (?, ?)")
	_, err := st.Execute(3, "p")
	requireMyError(t, err, 1290, "HY000")

	// Reads, transactions and session settings still work.
	if res := execOK(t, conn, "SELECT name FROM items"); len(res.Values) != 1 {
		t.Fatalf("rows %d", len(res.Values))
	}
	execOK(t, conn, "BEGIN")
	execOK(t, conn, "SELECT COUNT(*) FROM items")
	execOK(t, conn, "COMMIT")
	execOK(t, conn, "SET autocommit = 0")
	if v := decodedText(execOK(t, conn, "SELECT @@read_only")); v != "1" {
		t.Fatalf("@@read_only = %s", v)
	}
	_, err = conn.Execute("SET GLOBAL read_only = 0")
	requireMyError(t, err, 1238, "HY000")
}

// Without --readonly, @@read_only is 0.
func TestReadOnlyVariableOff(t *testing.T) {
	conn := shopServer(t)
	if v := decodedText(execOK(t, conn, "SELECT @@global.read_only")); v != "0" {
		t.Fatalf("@@read_only = %s", v)
	}
}

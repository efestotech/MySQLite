package wire

import (
	"testing"
)

// M6 (client matrix, PDO with emulated prepares): PDO quotes a LOB parameter as
// _binary'...'. A binary string literal (_binary'...', X'..', 0x..) written to a binary
// column (BINARY, VARBINARY, BLOB) by INSERT or UPDATE stores its bytes, as a prepared
// BLOB parameter does. Anywhere else its type depends on the context: still 1235
// (docs/agent-notes/discrepancies.md). `oracle: pending`.
func TestBinaryLiteralIntoBinaryColumn(t *testing.T) {
	addr := queueServer(t, 8)
	conn := dial(t, addr)
	execOK(t, conn, "CREATE TABLE b (id INT PRIMARY KEY, photo BLOB, code VARBINARY(4), name VARCHAR(20))")

	execOK(t, conn, "INSERT INTO b (id, photo, code) VALUES (1, _binary'\\0a\\'b\xff', X'00ff41'), (2, 0x4243, _binary'z')")
	execOK(t, conn, "INSERT INTO b VALUES (3, _binary'q', 0x7a, 'x')")
	execOK(t, conn, "UPDATE b SET photo = X'0001' WHERE id = 2")

	for _, tc := range []struct{ query, want string }{
		{"SELECT photo FROM b WHERE id = 1", "\x00a'b\xff"},
		{"SELECT code FROM b WHERE id = 1", "\x00\xffA"},
		{"SELECT photo FROM b WHERE id = 2", "\x00\x01"},
		{"SELECT code FROM b WHERE id = 2", "z"},
		{"SELECT photo FROM b WHERE id = 3", "q"},
		{"SELECT code FROM b WHERE id = 3", "z"},
	} {
		if got := decodedText(execOK(t, conn, tc.query)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.query, got, tc.want)
		}
	}

	// The column's byte limit still applies (strict mode).
	_, err := conn.Execute("INSERT INTO b (id, code) VALUES (4, X'0102030405')")
	requireMyError(t, err, 1406, "22001")

	// Not a binary column, or no column at all: the type depends on the context.
	for _, q := range []string{
		"INSERT INTO b (id, name) VALUES (5, _binary'x')",
		"INSERT INTO b (id, name) VALUES (5, X'41')",
		"SELECT _binary'x'",
		"SELECT id FROM b WHERE photo = _binary'q'",
	} {
		_, err := conn.Execute(q)
		requireMyError(t, err, 1235, "42000")
	}
}

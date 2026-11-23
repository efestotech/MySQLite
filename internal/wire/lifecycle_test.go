package wire

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// command sends one command packet and returns the first response packet.
func command(t *testing.T, conn *client.Conn, payload ...byte) []byte {
	t.Helper()
	conn.ResetSequence()
	if err := conn.WritePacket(append([]byte{0, 0, 0, 0}, payload...)); err != nil {
		t.Fatal(err)
	}
	data, err := conn.ReadPacket()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// okStatus returns the status flags of an OK packet.
func okStatus(t *testing.T, data []byte) uint16 {
	t.Helper()
	if len(data) == 0 || data[0] != mysql.OK_HEADER {
		t.Fatalf("not an OK packet: %v", data)
	}
	pos := 1
	for range 2 { // affected rows, last insert id: length-encoded, small here
		if data[pos] >= 0xfb {
			t.Fatalf("unexpected length-encoded integer in %v", data)
		}
		pos++
	}
	return binary.LittleEndian.Uint16(data[pos:])
}

// errPacket decodes an ERR packet.
func errPacket(t *testing.T, conn *client.Conn, data []byte) *mysql.MyError {
	t.Helper()
	if len(data) == 0 || data[0] != mysql.ERR_HEADER {
		t.Fatalf("not an ERR packet: %v", data)
	}
	return requireMyError(t, conn.HandleErrorPacket(data), binary.LittleEndian.Uint16(data[1:]), string(data[4:9]))
}

// COM_RESET_CONNECTION (docs/03-protocol.md section 4): the session is reset as after
// the handshake, the authentication and the current database are kept.
// `oracle: pending`.
func TestResetConnection(t *testing.T) {
	addr := queueServer(t, 8)
	conn := dial(t, addr)
	execOK(t, conn, "SET SESSION innodb_lock_wait_timeout = 7")
	execOK(t, conn, "SET autocommit = 0")
	execOK(t, conn, "INSERT INTO t VALUES (1, 1)")
	if id := decodedText(execOK(t, conn, "SELECT LAST_INSERT_ID(5)")); id != "5" {
		t.Fatalf("LAST_INSERT_ID %s", id)
	}
	stmt, err := conn.Prepare("SELECT ?")
	if err != nil {
		t.Fatal(err)
	}

	status := okStatus(t, command(t, conn, mysql.COM_RESET_CONNECTION))
	if status&mysql.SERVER_STATUS_IN_TRANS != 0 || status&mysql.SERVER_STATUS_AUTOCOMMIT == 0 {
		t.Fatalf("status after reset %#x", status)
	}
	// The writer is free at once: the transaction was rolled back, not left open.
	other := dial(t, addr)
	execOK(t, other, "SET innodb_lock_wait_timeout = 1")
	execOK(t, other, "INSERT INTO t VALUES (10, 10)")

	for q, want := range map[string]string{
		"SELECT COUNT(*) FROM t":            "1",
		"SELECT @@autocommit":               "1",
		"SELECT @@innodb_lock_wait_timeout": "50",
		"SELECT LAST_INSERT_ID()":           "0",
		"SELECT DATABASE()":                 "shop",
	} {
		if got := decodedText(execOK(t, conn, q)); got != want {
			t.Errorf("%s: %s, want %s", q, got, want)
		}
	}
	_, err = stmt.Execute(1)
	requireMyError(t, err, 1243, "HY000")
}

// changeUserServer serves shop and misc to three users.
func changeUserServer(t *testing.T) string {
	t.Helper()
	e := newEngine(t)
	seed(t, e, "shop", "CREATE TABLE t (id INTEGER PRIMARY KEY, v INT)")
	if err := e.CreateDatabase("misc"); err != nil {
		t.Fatal(err)
	}
	return startServerWith(t, Config{
		ServerVersion:    "8.0.36-mysqlite",
		MaxAllowedPacket: 64 << 20,
		Users:            map[string]string{testUser: testPassword, "other": "secret", "nopw": ""},
		Engine:           e,
	})
}

// changeUser sends COM_CHANGE_USER, answers the server's auth switch with password,
// and returns the final response packet.
func changeUser(t *testing.T, conn *client.Conn, user, password, db string) []byte {
	t.Helper()
	p := []byte{mysql.COM_CHANGE_USER}
	p = append(p, user...)
	p = append(p, 0)
	// The scramble of the handshake salt; the server asks again with a new salt.
	stale := mysql.CalcNativePassword([]byte("01234567890123456789"), []byte(password))
	p = append(p, byte(len(stale)))
	p = append(p, stale...)
	p = append(p, db...)
	p = append(p, 0)
	p = append(p, 255, 0) // utf8mb4_0900_ai_ci
	p = append(p, mysql.AUTH_NATIVE_PASSWORD...)
	p = append(p, 0)
	data := command(t, conn, p...)
	if data[0] != mysql.EOF_HEADER {
		return data
	}
	plugin, salt, ok := cutNUL(data[1:])
	if !ok || string(plugin) != mysql.AUTH_NATIVE_PASSWORD || len(salt) != 21 || salt[20] != 0 {
		t.Fatalf("auth switch request %q", data)
	}
	if err := conn.WritePacket(append([]byte{0, 0, 0, 0}, mysql.CalcNativePassword(salt[:20], []byte(password))...)); err != nil {
		t.Fatal(err)
	}
	data, err := conn.ReadPacket()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// COM_CHANGE_USER (docs/03-protocol.md section 4): re-authentication, then a reset
// session on the new user and database. `oracle: pending`.
func TestChangeUser(t *testing.T) {
	addr := changeUserServer(t)
	conn := dial(t, addr)
	execOK(t, conn, "BEGIN")
	execOK(t, conn, "INSERT INTO t VALUES (1, 1)")

	status := okStatus(t, changeUser(t, conn, "other", "secret", "shop"))
	if status&mysql.SERVER_STATUS_IN_TRANS != 0 {
		t.Fatalf("status after COM_CHANGE_USER %#x", status)
	}
	for q, want := range map[string]string{
		"SELECT CURRENT_USER()":  "other@%",
		"SELECT COUNT(*) FROM t": "0",
	} {
		if got := decodedText(execOK(t, conn, q)); got != want {
			t.Errorf("%s: %s, want %s", q, got, want)
		}
	}

	okStatus(t, changeUser(t, conn, "nopw", "", "misc"))
	for q, want := range map[string]string{
		"SELECT CURRENT_USER()": "nopw@%",
		"SELECT DATABASE()":     "misc",
	} {
		if got := decodedText(execOK(t, conn, q)); got != want {
			t.Errorf("%s: %s, want %s", q, got, want)
		}
	}
}

// A failed COM_CHANGE_USER is 1045 and ends the connection. `oracle: pending`.
func TestChangeUserWrongPassword(t *testing.T) {
	addr := changeUserServer(t)
	conn := dial(t, addr)
	e := errPacket(t, conn, changeUser(t, conn, "other", "wrong", "shop"))
	if e.Code != 1045 || e.Message != "Access denied for user 'other'@'127.0.0.1' (using password: YES)" {
		t.Fatalf("error %v", e)
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := conn.Ping(); err == nil {
		t.Fatal("the connection is still open")
	}
}

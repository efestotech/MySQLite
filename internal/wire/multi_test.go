package wire

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// result is one response of a multi-statement query.
type result struct {
	res *mysql.Result
	err error
}

func executeMultiple(t *testing.T, conn *client.Conn, query string) []result {
	t.Helper()
	var out []result
	if _, err := conn.ExecuteMultiple(query, func(r *mysql.Result, err error) {
		out = append(out, result{r, err})
	}); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return out
}

// The initial handshake advertises CLIENT_MULTI_STATEMENTS and CLIENT_MULTI_RESULTS
// from M5 (docs/03-protocol.md section 2), read from the bytes on the wire.
func TestHandshakeAdvertisesMultiStatements(t *testing.T) {
	addr := queueServer(t, 8)
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	var header [4]byte
	if _, err := readFull(c, header[:]); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, int(header[0])|int(header[1])<<8|int(header[2])<<16)
	if _, err := readFull(c, body); err != nil {
		t.Fatal(err)
	}
	_, rest, ok := cutNUL(body[1:]) // protocol version, server version
	if !ok || len(rest) < 4+8+1+2+1+2+2 {
		t.Fatalf("handshake %q", body)
	}
	low := binary.LittleEndian.Uint16(rest[13:])
	high := binary.LittleEndian.Uint16(rest[18:])
	caps := uint32(low) | uint32(high)<<16
	for name, flag := range map[string]uint32{
		"CLIENT_MULTI_STATEMENTS": mysql.CLIENT_MULTI_STATEMENTS,
		"CLIENT_MULTI_RESULTS":    mysql.CLIENT_MULTI_RESULTS,
		"CLIENT_PROTOCOL_41":      mysql.CLIENT_PROTOCOL_41,
		"CLIENT_PLUGIN_AUTH":      mysql.CLIENT_PLUGIN_AUTH,
	} {
		if caps&flag == 0 {
			t.Errorf("%s not advertised (%#x)", name, caps)
		}
	}
	if caps&mysql.CLIENT_SSL != 0 || caps&mysql.CLIENT_DEPRECATE_EOF != 0 {
		t.Errorf("unexpected flags %#x", caps)
	}
}

func readFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := c.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// Criterion 4: every result but the last carries SERVER_MORE_RESULTS_EXISTS; result
// sets and OK packets mix. `oracle: pending`.
func TestMultiStatementFraming(t *testing.T) {
	addr := queueServer(t, 8)
	conn := dial(t, addr, mysql.CLIENT_MULTI_STATEMENTS)
	got := executeMultiple(t, conn, "INSERT INTO t VALUES (1, 10); SELECT v FROM t; UPDATE t SET v = 11 WHERE id = 1;SELECT v FROM t")
	if len(got) != 4 {
		t.Fatalf("%d results", len(got))
	}
	for i, r := range got {
		if r.err != nil {
			t.Fatalf("result %d: %v", i, r.err)
		}
		more := r.res.Status&mysql.SERVER_MORE_RESULTS_EXISTS != 0
		if more != (i < len(got)-1) {
			t.Errorf("result %d: more results %v", i, more)
		}
	}
	if got[0].res.AffectedRows != 1 || got[2].res.AffectedRows != 1 {
		t.Errorf("affected rows %d, %d", got[0].res.AffectedRows, got[2].res.AffectedRows)
	}
	if v := decodedText(got[1].res); v != "10" {
		t.Errorf("first SELECT %s", v)
	}
	if v := decodedText(got[3].res); v != "11" {
		t.Errorf("second SELECT %s", v)
	}
	// The connection is in step: the next query sees its own response.
	if v := decodedText(execOK(t, conn, "SELECT COUNT(*) FROM t")); v != "1" {
		t.Errorf("rows %s", v)
	}
}

// An error ends the batch: the statements before it took effect, the ones after it do
// not run. `oracle: pending`.
func TestMultiStatementError(t *testing.T) {
	addr := queueServer(t, 8)
	conn := dial(t, addr, mysql.CLIENT_MULTI_STATEMENTS)
	got := executeMultiple(t, conn, "INSERT INTO t VALUES (1, 1); SELECT nope FROM missing; INSERT INTO t VALUES (2, 2)")
	if len(got) != 2 || got[0].err != nil {
		t.Fatalf("results %v", got)
	}
	requireMyError(t, got[1].err, 1146, "42S02")
	if v := decodedText(execOK(t, conn, "SELECT COUNT(*) FROM t")); v != "1" {
		t.Errorf("rows %s", v)
	}
	// A syntax error in a later statement is found when that statement runs.
	got = executeMultiple(t, conn, "INSERT INTO t VALUES (3, 3); SELEC 1")
	if len(got) != 2 || got[0].err != nil {
		t.Fatalf("results %v", got)
	}
	requireMyError(t, got[1].err, 1064, "42000")
	if v := decodedText(execOK(t, conn, "SELECT COUNT(*) FROM t")); v != "2" {
		t.Errorf("rows %s", v)
	}
}

// Without CLIENT_MULTI_STATEMENTS a second statement is a syntax error; COM_SET_OPTION
// turns multi-statements on and off for the session. `oracle: pending`.
func TestSetOption(t *testing.T) {
	addr := queueServer(t, 8)
	conn := dial(t, addr)
	_, err := conn.Execute("SELECT 1; SELECT 2")
	requireMyError(t, err, 1064, "42000")

	if data := command(t, conn, mysql.COM_SET_OPTION, 0, 0); data[0] != mysql.EOF_HEADER {
		t.Fatalf("COM_SET_OPTION ON: %v", data)
	}
	if got := executeMultiple(t, conn, "SELECT 1; SELECT 2"); len(got) != 2 || got[1].err != nil {
		t.Fatalf("results %v", got)
	}

	if data := command(t, conn, mysql.COM_SET_OPTION, 1, 0); data[0] != mysql.EOF_HEADER {
		t.Fatalf("COM_SET_OPTION OFF: %v", data)
	}
	_, err = conn.Execute("SELECT 1; SELECT 2")
	requireMyError(t, err, 1064, "42000")

	e := errPacket(t, conn, command(t, conn, mysql.COM_SET_OPTION, 7, 0))
	if e.Code != 1047 {
		t.Fatalf("unknown option: %v", e)
	}
	e = errPacket(t, conn, command(t, conn, mysql.COM_SET_OPTION, 0))
	if e.Code != 1835 {
		t.Fatalf("short packet: %v", e)
	}
	execOK(t, conn, "SELECT 1")
}

// Verifier A1: each statement of a batch is split with the sql_mode the statements
// before it set, as MySQL parses one statement at a time. `oracle: pending`.
func TestMultiStatementSQLModeChange(t *testing.T) {
	addr := queueServer(t, 8)
	conn := dial(t, addr, mysql.CLIENT_MULTI_STATEMENTS)
	got := executeMultiple(t, conn, "SET sql_mode='NO_BACKSLASH_ESCAPES'; SELECT 'a\\'; SELECT 2")
	if len(got) != 3 {
		t.Fatalf("%d results: %v", len(got), got)
	}
	for i, r := range got {
		if r.err != nil {
			t.Fatalf("result %d: %v", i, r.err)
		}
	}
	if v := decodedText(got[1].res); v != "a\\" {
		t.Errorf("second result %q", v)
	}
}

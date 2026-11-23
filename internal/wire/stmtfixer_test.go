package wire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// G6 and G7 (ADR 0003): long data and EXECUTE without new types, sent as raw packets
// since go-mysql's client never sends either. M4 criterion 5: BLOB round trip.

func allBytes() []byte {
	b := make([]byte, 0, 512)
	for i := 0; i < 512; i++ {
		b = append(b, byte(i))
	}
	return b
}

func blobServer(t *testing.T) *client.Conn {
	t.Helper()
	conn := shopServer(t)
	execOK(t, conn, "CREATE TABLE files (id INT PRIMARY KEY, data LONGBLOB, label VARCHAR(20))")
	return conn
}

func selectBlob(t *testing.T, conn *client.Conn, id int) (data []byte, label string) {
	t.Helper()
	res := execute(t, prepare(t, conn, "SELECT data, label FROM files WHERE id = ?"), id)
	if len(res.Values) != 1 {
		t.Fatalf("id %d: %d rows", id, len(res.Values))
	}
	if v, ok := res.Values[0][0].Value().([]byte); ok {
		data = v
	}
	return data, decoded(res.Values[0][1])
}

// M4 criterion 5: every byte value survives INSERT and SELECT through prepared
// statements, in both escaping modes.
func TestPreparedBlobRoundTrip(t *testing.T) {
	conn := blobServer(t)
	want := allBytes()
	ins := prepare(t, conn, "INSERT INTO files (id, data) VALUES (?, ?)")
	execute(t, ins, 1, want)
	execOK(t, conn, "SET sql_mode = 'NO_BACKSLASH_ESCAPES'")
	execute(t, prepare(t, conn, "INSERT INTO files (id, data) VALUES (?, ?)"), 2, want)
	for id := 1; id <= 2; id++ {
		got, _ := selectBlob(t, conn, id)
		if !bytes.Equal(got, want) {
			t.Fatalf("id %d: %d bytes back, first difference at %d", id, len(got), firstDiff(got, want))
		}
	}
	if res := execOK(t, conn, "SELECT LENGTH(data) FROM files WHERE id = 1"); decodedText(res) != "512" {
		t.Fatalf("LENGTH: %s", decodedText(res))
	}
}

func firstDiff(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

func decodedText(res *mysql.Result) string {
	s, _ := res.GetString(0, 0)
	return s
}

// rawStmt prepares q with go-mysql's client and returns the statement id.
func rawStmt(t *testing.T, conn *client.Conn, q string) uint32 {
	t.Helper()
	return prepare(t, conn, q).ID
}

// send writes one command packet and returns the first response packet.
func send(t *testing.T, conn *client.Conn, payload []byte, response bool) []byte {
	t.Helper()
	conn.ResetSequence()
	if err := conn.WritePacket(append(make([]byte, 4), payload...)); err != nil {
		t.Fatal(err)
	}
	if !response {
		return nil
	}
	p, err := conn.ReadPacket()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func longData(id uint32, param uint16, data []byte) []byte {
	p := []byte{mysql.COM_STMT_SEND_LONG_DATA}
	p = binary.LittleEndian.AppendUint32(p, id)
	p = binary.LittleEndian.AppendUint16(p, param)
	return append(p, data...)
}

// executePacket builds COM_STMT_EXECUTE; types nil sends new-params-bound 0.
func executePacket(id uint32, params int, nulls []int, types []byte, values []byte) []byte {
	p := []byte{mysql.COM_STMT_EXECUTE}
	p = binary.LittleEndian.AppendUint32(p, id)
	p = append(p, 0, 1, 0, 0, 0)
	bitmap := make([]byte, (params+7)/8)
	for _, i := range nulls {
		bitmap[i/8] |= 1 << (i % 8)
	}
	p = append(p, bitmap...)
	if types == nil {
		p = append(p, 0)
	} else {
		p = append(p, 1)
		p = append(p, types...)
	}
	return append(p, values...)
}

func requireOKPacket(t *testing.T, p []byte) {
	t.Helper()
	if len(p) == 0 || p[0] != mysql.OK_HEADER {
		t.Fatalf("not OK: % x", p)
	}
}

// G6: long data in several chunks, with an inline value after it.
func TestPreparedLongData(t *testing.T) {
	conn := blobServer(t)
	id := rawStmt(t, conn, "INSERT INTO files (id, data, label) VALUES (?, ?, ?)")
	want := allBytes()
	send(t, conn, longData(id, 1, want[:100]), false)
	send(t, conn, longData(id, 1, want[100:]), false)
	types := []byte{mysql.MYSQL_TYPE_LONG, 0, mysql.MYSQL_TYPE_BLOB, 0, mysql.MYSQL_TYPE_STRING, 0}
	values := binary.LittleEndian.AppendUint32(nil, 10)
	values = append(values, mysql.PutLengthEncodedString([]byte("after"))...)
	requireOKPacket(t, send(t, conn, executePacket(id, 3, nil, types, values), true))
	got, label := selectBlob(t, conn, 10)
	if !bytes.Equal(got, want) || label != "after" {
		t.Fatalf("long data: %d bytes (diff at %d), label %q", len(got), firstDiff(got, want), label)
	}

	// Long data is consumed by the EXECUTE: the next one sends the value inline.
	values = binary.LittleEndian.AppendUint32(nil, 11)
	values = append(values, mysql.PutLengthEncodedString([]byte("inline"))...)
	values = append(values, mysql.PutLengthEncodedString([]byte("b"))...)
	requireOKPacket(t, send(t, conn, executePacket(id, 3, nil, types, values), true))
	if got, _ := selectBlob(t, conn, 11); string(got) != "inline" {
		t.Fatalf("after long data: %q", got)
	}

	// COM_STMT_RESET drops pending long data (it answers OK).
	send(t, conn, longData(id, 1, []byte("dropped")), false)
	reset := binary.LittleEndian.AppendUint32([]byte{mysql.COM_STMT_RESET}, id)
	requireOKPacket(t, send(t, conn, reset, true))
	values = binary.LittleEndian.AppendUint32(nil, 12)
	values = append(values, mysql.PutLengthEncodedString([]byte("kept"))...)
	values = append(values, mysql.PutLengthEncodedString([]byte("c"))...)
	requireOKPacket(t, send(t, conn, executePacket(id, 3, nil, types, values), true))
	if got, _ := selectBlob(t, conn, 12); string(got) != "kept" {
		t.Fatalf("after reset: %q", got)
	}
}

// G7: a re-EXECUTE with new-params-bound 0 uses the types of the previous one.
func TestPreparedReexecuteWithoutTypes(t *testing.T) {
	conn := blobServer(t)
	id := rawStmt(t, conn, "INSERT INTO files (id, data, label) VALUES (?, ?, ?)")
	types := []byte{mysql.MYSQL_TYPE_LONGLONG, 0, mysql.MYSQL_TYPE_STRING, 0, mysql.MYSQL_TYPE_STRING, 0}
	row := func(n uint64, data, label string) []byte {
		v := binary.LittleEndian.AppendUint64(nil, n)
		v = append(v, mysql.PutLengthEncodedString([]byte(data))...)
		return append(v, mysql.PutLengthEncodedString([]byte(label))...)
	}
	requireOKPacket(t, send(t, conn, executePacket(id, 3, nil, types, row(20, "one", "x")), true))
	requireOKPacket(t, send(t, conn, executePacket(id, 3, nil, nil, row(21, "two", "y")), true))
	// A NULL parameter takes no value bytes.
	v := binary.LittleEndian.AppendUint64(nil, 22)
	v = append(v, mysql.PutLengthEncodedString([]byte("three"))...)
	requireOKPacket(t, send(t, conn, executePacket(id, 3, []int{2}, nil, v), true))
	for _, c := range []struct {
		id          int
		data, label string
	}{{20, "one", "x"}, {21, "two", "y"}, {22, "three", "NULL"}} {
		data, label := selectBlob(t, conn, c.id)
		if string(data) != c.data || label != c.label {
			t.Errorf("id %d: %q %q, want %q %q", c.id, data, label, c.data, c.label)
		}
	}
}

// Long data above max_allowed_packet is refused at EXECUTE (oracle: pending).
func TestPreparedLongDataLimit(t *testing.T) {
	addr := startServerWith(t, Config{ServerVersion: "8.0.36-mysqlite", MaxAllowedPacket: 1024,
		Users: map[string]string{testUser: testPassword}})
	conn := dial(t, addr)
	execOK(t, conn, "CREATE TABLE f (data LONGBLOB)")
	id := rawStmt(t, conn, "INSERT INTO f VALUES (?)")
	send(t, conn, longData(id, 0, make([]byte, 800)), false)
	send(t, conn, longData(id, 0, make([]byte, 800)), false)
	p := send(t, conn, executePacket(id, 1, nil, []byte{mysql.MYSQL_TYPE_BLOB, 0}, nil), true)
	if len(p) < 3 || p[0] != mysql.ERR_HEADER || binary.LittleEndian.Uint16(p[1:3]) != mysql.ER_NET_PACKET_TOO_LARGE {
		t.Fatalf("over the limit: % x", p)
	}
	// The connection stays usable.
	if res := execOK(t, conn, "SELECT COUNT(*) FROM f"); decodedText(res) != "0" {
		t.Fatalf("rows: %s", decodedText(res))
	}
}

// Verifier probe: long data that makes the rewritten EXECUTE larger than one 16 MiB
// chunk. go-mysql numbers its response after the chunks it read, so the response's
// sequence ids must be brought back to the client's.
func TestPreparedLongDataOver16MiB(t *testing.T) {
	conn := blobServer(t) // max_allowed_packet 64 MiB
	id := rawStmt(t, conn, "INSERT INTO files (id, data) VALUES (?, ?)")
	big := bytes.Repeat([]byte{'a'}, 17<<20)
	send(t, conn, longData(id, 1, big[:9<<20]), false)
	send(t, conn, longData(id, 1, big[9<<20:]), false)
	types := []byte{mysql.MYSQL_TYPE_LONG, 0, mysql.MYSQL_TYPE_BLOB, 0}
	requireOKPacket(t, send(t, conn, executePacket(id, 2, nil, types, binary.LittleEndian.AppendUint32(nil, 1)), true))
	if res := execOK(t, conn, "SELECT LENGTH(data) FROM files WHERE id = 1"); decodedText(res) != "17825792" {
		t.Fatalf("length %s", decodedText(res))
	}
}

// Verifier probe: a flag-0 EXECUTE just under one chunk, which the remembered types push
// over it.
func TestPreparedReexecuteAcrossChunk(t *testing.T) {
	conn := blobServer(t)
	id := rawStmt(t, conn, "INSERT INTO files (id, data) VALUES (?, ?)")
	types := []byte{mysql.MYSQL_TYPE_LONG, 0, mysql.MYSQL_TYPE_BLOB, 0}
	small := binary.LittleEndian.AppendUint32(nil, 1)
	small = append(small, mysql.PutLengthEncodedString([]byte("x"))...)
	requireOKPacket(t, send(t, conn, executePacket(id, 2, nil, types, small), true))
	// Payload: 10 header bytes, 1 bitmap, 1 flag, 4 id, then a 4-byte lenenc prefix.
	n := maxChunkPayload - 2 - 10 - 1 - 1 - 4 - 4
	values := binary.LittleEndian.AppendUint32(nil, 2)
	values = append(values, mysql.PutLengthEncodedString(bytes.Repeat([]byte{'b'}, n))...)
	p := executePacket(id, 2, nil, nil, values)
	if len(p) != maxChunkPayload-2 {
		t.Fatalf("payload %d bytes", len(p))
	}
	requireOKPacket(t, send(t, conn, p, true))
	if res := execOK(t, conn, "SELECT LENGTH(data) FROM files WHERE id = 2"); decodedText(res) != fmt.Sprint(n) {
		t.Fatalf("length %s", decodedText(res))
	}
}

// A1: a flag-0 EXECUTE before any EXECUTE sent types has no types to bind with; it is
// refused rather than storing NULLs (oracle: pending).
func TestPreparedReexecuteWithoutAnyTypes(t *testing.T) {
	conn := blobServer(t)
	id := rawStmt(t, conn, "INSERT INTO files (id, label) VALUES (?, ?)")
	v := binary.LittleEndian.AppendUint64(nil, 1)
	v = append(v, mysql.PutLengthEncodedString([]byte("x"))...)
	p := send(t, conn, executePacket(id, 2, nil, nil, v), true)
	if len(p) < 3 || p[0] != mysql.ERR_HEADER || binary.LittleEndian.Uint16(p[1:3]) != mysql.ER_WRONG_ARGUMENTS {
		t.Fatalf("flag 0 without types: % x", p)
	}
	if res := execOK(t, conn, "SELECT COUNT(*) FROM files"); decodedText(res) != "0" {
		t.Fatalf("rows: %s", decodedText(res))
	}
}

// A2: long data wins over the parameter's NULL bit, as in MySQL.
func TestPreparedLongDataOverNullBit(t *testing.T) {
	conn := blobServer(t)
	id := rawStmt(t, conn, "INSERT INTO files (id, data) VALUES (?, ?)")
	send(t, conn, longData(id, 1, []byte("long")), false)
	types := []byte{mysql.MYSQL_TYPE_LONG, 0, mysql.MYSQL_TYPE_BLOB, 0}
	requireOKPacket(t, send(t, conn, executePacket(id, 2, []int{1}, types, binary.LittleEndian.AppendUint32(nil, 30)), true))
	if got, _ := selectBlob(t, conn, 30); string(got) != "long" {
		t.Fatalf("data %q", got)
	}
}

// A5: M4 criterion 4 through the server's own path (text rows re-encoded as binary).
func TestPreparedWideNullBitmap(t *testing.T) {
	conn := blobServer(t)
	var cols, want []string
	for i := 0; i < 70; i++ {
		v := fmt.Sprint(i)
		if i%3 == 0 || i == 63 || i == 69 {
			v = "NULL"
		}
		cols = append(cols, v)
		want = append(want, v)
	}
	got := rowStrings(execute(t, prepare(t, conn, "SELECT "+strings.Join(cols, ", ")+", ?"), int64(7)))[0]
	if strings.Join(got[:70], ",") != strings.Join(want, ",") || got[70] != "7" {
		t.Fatalf("got %v", got)
	}
}

// Every parameter NULL: go-mysql's client sends flag 0 and no types, and the NULLs
// are bound (not refused as A1).
func TestPreparedAllNullWithoutTypes(t *testing.T) {
	conn := blobServer(t)
	sameRows(t, "all NULL", rowStrings(execute(t, prepare(t, conn, "SELECT ? IS NULL, ? IS NULL"), nil, nil)), []string{"1", "1"})
}

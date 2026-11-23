package wire

import (
	"encoding/binary"
	"io"
	"net"
	"sync/atomic"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// stmtFixer rewrites the client's prepared-statement packets before go-mysql reads
// them (ADR 0003, G6 and G7):
//
//   - COM_STMT_SEND_LONG_DATA is swallowed (it has no response) and its data is kept
//     per statement and parameter;
//   - COM_STMT_EXECUTE is rewritten with the new-params-bound flag set, the types of
//     the last EXECUTE that sent them, and the long data inline as length-encoded
//     strings. go-mysql then binds every value: on its own it overwrites long data and,
//     for flag 0, binds every parameter to NULL;
//   - COM_STMT_RESET clears the long data, COM_STMT_CLOSE the statement.
//
// The parameter count of each statement is read from the server's PREPARE response
// as it is written. The fixer is active only once authenticated: handshake packets are
// not commands. Read and Write are called from go-mysql's one goroutine per connection.
type stmtFixer struct {
	net.Conn
	limit  int
	active atomic.Bool

	out            []byte // rewritten bytes not yet returned by Read
	stmts          map[uint32]*fixerStmt
	pendingPrepare bool // the last command was COM_STMT_PREPARE; its response is next
}

type fixerStmt struct {
	params   int
	types    []byte // 2 bytes per parameter, from the last EXECUTE that sent them
	longData map[int][]byte
	longSize int
}

func newStmtFixer(c net.Conn, limit int) *stmtFixer {
	return &stmtFixer{Conn: c, limit: limit, stmts: map[uint32]*fixerStmt{}}
}

func (f *stmtFixer) Read(p []byte) (int, error) {
	if !f.active.Load() && len(f.out) == 0 {
		return f.Conn.Read(p)
	}
	for len(f.out) == 0 {
		if err := f.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, f.out)
	f.out = f.out[n:]
	return n, nil
}

// next reads one logical packet from the client and queues what go-mysql must read
// instead: the packet itself, a rewritten one, or nothing.
func (f *stmtFixer) next() error {
	payload, seq, err := f.readPacket()
	if err != nil {
		return err
	}
	f.pendingPrepare = false
	if len(payload) > 0 {
		switch payload[0] {
		case mysql.COM_STMT_PREPARE:
			f.pendingPrepare = true
		case mysql.COM_STMT_SEND_LONG_DATA:
			f.longData(payload)
			return nil
		case mysql.COM_STMT_EXECUTE:
			if payload, err = f.execute(payload, seq); err != nil || payload == nil {
				return err
			}
		case mysql.COM_STMT_RESET:
			if s := f.stmt(payload); s != nil {
				s.longData, s.longSize = nil, 0
			}
		case mysql.COM_STMT_CLOSE:
			if len(payload) >= 5 {
				delete(f.stmts, binary.LittleEndian.Uint32(payload[1:5]))
			}
		}
	}
	f.queue(payload, seq)
	return nil
}

// readPacket reads one logical packet, joining 16 MiB chunks.
func (f *stmtFixer) readPacket() ([]byte, byte, error) {
	var payload []byte
	var hdr [4]byte
	var seq byte
	first := true
	for {
		if _, err := io.ReadFull(f.Conn, hdr[:]); err != nil {
			return nil, 0, err
		}
		if first {
			seq, first = hdr[3], false
		}
		n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
		start := len(payload)
		payload = append(payload, make([]byte, n)...)
		if _, err := io.ReadFull(f.Conn, payload[start:]); err != nil {
			return nil, 0, err
		}
		if n < maxChunkPayload {
			return payload, seq, nil
		}
	}
}

// queue appends payload to the bytes Read returns, split into chunks from sequence id
// seq.
func (f *stmtFixer) queue(payload []byte, seq byte) {
	for {
		n := min(len(payload), maxChunkPayload)
		f.out = append(f.out, byte(n), byte(n>>8), byte(n>>16), seq)
		f.out = append(f.out, payload[:n]...)
		payload = payload[n:]
		seq++
		if n < maxChunkPayload {
			return
		}
	}
}

func (f *stmtFixer) stmt(payload []byte) *fixerStmt {
	if len(payload) < 5 {
		return nil
	}
	return f.stmts[binary.LittleEndian.Uint32(payload[1:5])]
}

// longData keeps COM_STMT_SEND_LONG_DATA's chunk: statement id, parameter id, data.
func (f *stmtFixer) longData(payload []byte) {
	s := f.stmt(payload)
	if s == nil || len(payload) < 7 {
		return // as MySQL, an unknown statement or parameter is ignored here
	}
	param := int(binary.LittleEndian.Uint16(payload[5:7]))
	if param >= s.params {
		return
	}
	if s.longData == nil {
		s.longData = map[int][]byte{}
	}
	data := payload[7:]
	s.longSize += len(data)
	if s.longSize > f.limit {
		// Reported at EXECUTE; the data is not kept.
		s.longData[param] = nil
		return
	}
	s.longData[param] = append(s.longData[param], data...)
}

// execute rewrites COM_STMT_EXECUTE: payload is 0x17, statement id (4), flags (1),
// iteration count (4), then for a statement with parameters the NULL bitmap, the
// new-params-bound flag, the types when that flag is 1, and the values. A nil result
// means the fixer answered the client itself.
func (f *stmtFixer) execute(payload []byte, seq byte) ([]byte, error) {
	s := f.stmt(payload)
	if s == nil || s.params == 0 {
		return payload, nil
	}
	defer func() { s.longData, s.longSize = nil, 0 }()
	if s.longSize > f.limit {
		return nil, f.reply(seq+1, mysql.NewDefaultError(mysql.ER_NET_PACKET_TOO_LARGE))
	}
	bitmapLen := (s.params + 7) / 8
	pos := 10
	if len(payload) < pos+bitmapLen+1 {
		return payload, nil // go-mysql reports the malformed packet
	}
	bitmap := payload[pos : pos+bitmapLen]
	pos += bitmapLen
	bound := payload[pos] == 1
	pos++
	types := s.types
	if bound {
		if len(payload) < pos+2*s.params {
			return payload, nil
		}
		types = payload[pos : pos+2*s.params]
		s.types = append([]byte(nil), types...)
		pos += 2 * s.params
	}
	if types == nil || (bound && s.longData == nil) {
		return payload, nil // nothing to fix
	}
	values, ok := f.values(s, bitmap, types, payload[pos:])
	if !ok {
		return payload, nil
	}
	out := make([]byte, 0, 10+bitmapLen+1+len(types)+len(values))
	out = append(out, payload[:10]...)
	out = append(out, bitmap...)
	out = append(out, 1)
	out = append(out, types...)
	return append(out, values...), nil
}

// values rebuilds the values part: the client's values in order, with each long-data
// parameter's data inserted where the client sent none.
func (f *stmtFixer) values(s *fixerStmt, bitmap, types, in []byte) ([]byte, bool) {
	var out []byte
	pos := 0
	for i := 0; i < s.params; i++ {
		if bitmap[i/8]&(1<<(i%8)) != 0 {
			continue
		}
		tp := types[2*i]
		if data, ok := s.longData[i]; ok {
			if !lengthEncodedType(tp) {
				return nil, false
			}
			out = append(out, mysql.PutLengthEncodedString(data)...)
			continue
		}
		n, ok := valueSize(tp, in[pos:])
		if !ok {
			return nil, false
		}
		out = append(out, in[pos:pos+n]...)
		pos += n
	}
	return out, true
}

// valueSize is the size of one binary-protocol parameter value of type tp at the start
// of b.
func valueSize(tp byte, b []byte) (int, bool) {
	size := 0
	switch tp {
	case mysql.MYSQL_TYPE_NULL:
		size = 0
	case mysql.MYSQL_TYPE_TINY:
		size = 1
	case mysql.MYSQL_TYPE_SHORT, mysql.MYSQL_TYPE_YEAR:
		size = 2
	case mysql.MYSQL_TYPE_INT24, mysql.MYSQL_TYPE_LONG, mysql.MYSQL_TYPE_FLOAT:
		size = 4
	case mysql.MYSQL_TYPE_LONGLONG, mysql.MYSQL_TYPE_DOUBLE:
		size = 8
	default:
		if !lengthEncodedType(tp) || len(b) == 0 {
			return 0, false
		}
		_, _, n, err := mysql.LengthEncodedString(b)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return size, len(b) >= size
}

// lengthEncodedType reports a parameter type go-mysql reads as a length-encoded string.
func lengthEncodedType(tp byte) bool {
	switch tp {
	case mysql.MYSQL_TYPE_DECIMAL, mysql.MYSQL_TYPE_NEWDECIMAL, mysql.MYSQL_TYPE_VARCHAR, mysql.MYSQL_TYPE_BIT,
		mysql.MYSQL_TYPE_ENUM, mysql.MYSQL_TYPE_SET, mysql.MYSQL_TYPE_TINY_BLOB, mysql.MYSQL_TYPE_MEDIUM_BLOB,
		mysql.MYSQL_TYPE_LONG_BLOB, mysql.MYSQL_TYPE_BLOB, mysql.MYSQL_TYPE_VAR_STRING, mysql.MYSQL_TYPE_STRING,
		mysql.MYSQL_TYPE_GEOMETRY, mysql.MYSQL_TYPE_DATE, mysql.MYSQL_TYPE_NEWDATE,
		mysql.MYSQL_TYPE_TIMESTAMP, mysql.MYSQL_TYPE_DATETIME, mysql.MYSQL_TYPE_TIME:
		return true
	}
	return false
}

// reply writes an ERR packet to the client in place of go-mysql's response.
func (f *stmtFixer) reply(seq byte, e *mysql.MyError) error {
	payload := make([]byte, 0, 9+len(e.Message))
	payload = append(payload, mysql.ERR_HEADER, byte(e.Code), byte(e.Code>>8), '#')
	payload = append(payload, e.State...)
	payload = append(payload, e.Message...)
	packet := append([]byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), seq}, payload...)
	_, err := f.Conn.Write(packet)
	return err
}

// Write passes the server's packets through, reading the statement id and parameter
// count of a PREPARE response: 0x00, id (4), columns (2), parameters (2).
func (f *stmtFixer) Write(p []byte) (int, error) {
	if f.pendingPrepare {
		f.pendingPrepare = false
		if len(p) >= 4+9 && p[4] == 0x00 {
			id := binary.LittleEndian.Uint32(p[5:9])
			f.stmts[id] = &fixerStmt{params: int(binary.LittleEndian.Uint16(p[11:13]))}
		}
	}
	return f.Conn.Write(p)
}

package wire

import (
	"errors"
	"net"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// maxChunkPayload is the largest payload of one physical packet. A packet of exactly this
// size means the logical packet continues in the next one.
const maxChunkPayload = 0xffffff

var errPacketTooLarge = errors.New("wire: packet larger than max_allowed_packet")

// packetGuard enforces max_allowed_packet on incoming data before go-mysql reads the
// payload into memory. go-mysql reassembles multi-packet payloads without any limit
// (docs/adr/0003-go-mysql-capability-gaps.md, G3).
//
// It parses the packet framing of the byte stream as it passes through Read. When a
// header announces a logical packet over the limit, it sends ERR 1153 to the client and
// fails every later Read, so go-mysql closes the connection without allocating.
//
// Read is not safe for concurrent use; go-mysql reads each connection from one goroutine.
type packetGuard struct {
	net.Conn
	limit int

	hdr        [4]byte
	hdrLen     int
	remaining  int  // payload bytes still to pass for the current physical packet
	total      int  // payload bytes announced so far for the current logical packet
	continuing bool // the previous physical packet had the maximum chunk size
	err        error
}

func newPacketGuard(c net.Conn, limit int) *packetGuard {
	return &packetGuard{Conn: c, limit: limit}
}

func (g *packetGuard) Read(p []byte) (int, error) {
	if g.err != nil {
		return 0, g.err
	}
	n, err := g.Conn.Read(p)
	for i := 0; i < n; {
		if g.remaining > 0 {
			step := min(g.remaining, n-i)
			g.remaining -= step
			i += step
			continue
		}
		g.hdr[g.hdrLen] = p[i]
		g.hdrLen++
		i++
		if g.hdrLen < len(g.hdr) {
			continue
		}
		g.hdrLen = 0
		length := int(g.hdr[0]) | int(g.hdr[1])<<8 | int(g.hdr[2])<<16
		if !g.continuing {
			g.total = 0
		}
		g.total += length
		g.continuing = length == maxChunkPayload
		g.remaining = length
		if g.total > g.limit {
			g.reject(g.hdr[3] + 1)
			return 0, g.err
		}
	}
	return n, err
}

// reject sends ERR 1153 / 08S01 with the given sequence id and makes the guard fail.
func (g *packetGuard) reject(seq byte) {
	g.err = errPacketTooLarge
	myErr := mysql.NewDefaultError(mysql.ER_NET_PACKET_TOO_LARGE)
	payload := make([]byte, 0, 9+len(myErr.Message))
	payload = append(payload, mysql.ERR_HEADER, byte(myErr.Code), byte(myErr.Code>>8), '#')
	payload = append(payload, myErr.State...)
	payload = append(payload, myErr.Message...)
	packet := append([]byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), seq}, payload...)
	// Best effort: the connection is closed right after, whether the client reads it or not.
	_, _ = g.Write(packet)
}

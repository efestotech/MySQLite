package wire

import (
	"encoding/binary"
	"net"
	"sync"
	"time"

	"mysqlite/internal/errs"
	"mysqlite/internal/obs"
)

// idleGuard ends a connection whose transaction stays idle for timeout
// (docs/06-transactions.md section 6, decision Q6): it sends
// ER_CLIENT_INTERACTION_TIMEOUT, as MySQL does when wait_timeout expires, and closes
// the socket. The connection's goroutine then fails its read and closes the session,
// which rolls the transaction back and releases the writer.
//
// The guard is armed when the connection starts waiting for its next command, after
// go-mysql wrote the previous response, and disarmed when a command arrives. Both and
// the timer take mu, so the error packet is never written while the connection's
// goroutine handles a command.
type idleGuard struct {
	timeout time.Duration
	raw     net.Conn

	mu    sync.Mutex
	timer *time.Timer
	gen   uint64 // the current wait; a timer of an older wait does nothing
	fired bool
}

// waiting is called before the connection reads its next command; inTx reports an
// open transaction.
func (g *idleGuard) waiting(inTx bool) {
	if g == nil || g.timeout <= 0 || !inTx {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gen++
	gen := g.gen
	g.timer = time.AfterFunc(g.timeout, func() { g.fire(gen) })
}

// arrived is called once a command arrived. It reports false when the guard already
// ended the connection.
func (g *idleGuard) arrived() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
	g.gen++
	return !g.fired
}

// ended reports whether the guard ended the connection.
func (g *idleGuard) ended() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fired
}

func (g *idleGuard) fire(gen uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if gen != g.gen || g.fired {
		return
	}
	g.fired = true
	// Unsolicited, so sequence id 0. A client that does not read must not keep the
	// lock: the write is bounded.
	_ = g.raw.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = g.raw.Write(errorPacket(errs.ClientInteractionTimeout(), 0))
	_ = g.raw.Close()
}

// errorPacket encodes e as an ERR packet (protocol 4.1) with sequence id seq.
func errorPacket(e *errs.Error, seq byte) []byte {
	body := []byte{0xff, 0, 0, '#'}
	binary.LittleEndian.PutUint16(body[1:], e.Code)
	body = append(body, e.State...)
	body = append(body, e.Message...)
	n := len(body)
	return append([]byte{byte(n), byte(n >> 8), byte(n >> 16), seq}, body...)
}

// idleEnded logs and journals a connection the idle guard ended, once its transaction
// was rolled back.
func (h *handler) idleEnded(g *idleGuard) {
	if !g.ended() {
		return
	}
	e := errs.ClientInteractionTimeout()
	h.log.Warn("idle transaction rolled back, connection closed", "timeout", g.timeout.String())
	if h.journal == nil {
		return
	}
	entry := obs.Entry{
		Time:      time.Now(),
		User:      h.user(),
		Kind:      "transaction",
		SQL:       "-- idle transaction timeout: rollback",
		Outcome:   obs.OutcomeError,
		ErrorCode: e.Code,
		SQLState:  e.State,
		Error:     e.Message,
	}
	if h.conn != nil {
		entry.Session = h.conn.ConnectionID()
	}
	if werr := h.journal.Record(entry); werr != nil {
		h.log.Warn("query journal", "error", werr.Error())
	}
}

package wire

import (
	"net"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/dialect"
	"mysqlite/internal/dialect/functions"
	"mysqlite/internal/dialect/udf"
	"mysqlite/internal/vars"
)

// Session functions (docs/05-dialect.md section 5): the dialect binds them as
// dialect.SessionValue arguments, filled here before the statement runs, so a statement
// sees the values the previous statement left.

// statementSink is the LAST_INSERT_ID(expr) sink of the running statement.
type statementSink struct {
	id   int64
	take func() (int64, bool)
}

func (h *handler) sessionValue(sv *dialect.SessionValue) any {
	switch name := sv.Name; name {
	case functions.SessionNow, functions.SessionCurDate, functions.SessionCurTime,
		functions.SessionUTCNow, functions.SessionUTCDate, functions.SessionUTCTime,
		functions.SessionUnixTimestamp:
		return h.clockValue(name, sv.Fsp)
	case functions.SessionTimeZone:
		if v, _, e := h.vars.Get("time_zone", vars.ReadSession); e == nil {
			return v.Text
		}
		return "SYSTEM"
	case functions.SessionLastInsertID:
		return h.lastInsertID
	case functions.SessionRowCount:
		return h.rowCount
	case functions.SessionConnectionID:
		if h.conn == nil {
			return int64(0)
		}
		return int64(h.conn.ConnectionID())
	case functions.SessionUser:
		return h.user() + "@" + h.clientHost()
	case functions.SessionCurrentUser:
		// The account that authenticated: every configured user is user@'%'.
		return h.user() + "@%"
	case functions.SessionVersion:
		return h.version
	case functions.SessionSink:
		if h.sink == nil {
			id, take := udf.NewSink()
			h.sink = &statementSink{id: id, take: take}
		}
		return h.sink.id
	}
	return nil
}

func (h *handler) user() string {
	if h.conn == nil {
		return ""
	}
	return h.conn.GetUser()
}

// clientHost is the host USER() reports: the client's address, localhost for a
// loopback one (MySQL resolves it). `oracle: pending`.
func (h *handler) clientHost() string {
	if h.proc == nil {
		return "localhost"
	}
	host, _, err := net.SplitHostPort(h.proc.host)
	if err != nil {
		host = h.proc.host
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return "localhost"
	}
	return host
}

// endStatement records what the next statement's LAST_INSERT_ID() and ROW_COUNT() see.
// ROW_COUNT() is the affected rows of a write (changed rows of an UPDATE, matched with
// CLIENT_FOUND_ROWS), -1 after a result set or an error, 0 otherwise.
func (h *handler) endStatement(st *dialect.Statement, res *mysql.Result, err error) {
	if h.sink != nil {
		v, set := h.sink.take()
		h.sink = nil
		if set && err == nil {
			h.lastInsertID = v
			if res != nil && st.Kind == dialect.KindWrite {
				// The OK packet carries the value, as mysql_insert_id() reports it.
				res.InsertId = uint64(v)
			}
		}
	}
	switch {
	case err != nil || st == nil:
		h.rowCount = -1
	case st.Kind == dialect.KindRead || st.Kind == dialect.KindShow:
		h.rowCount = -1
	case st.Kind == dialect.KindWrite && res != nil:
		h.rowCount = int64(res.AffectedRows)
	default:
		h.rowCount = 0
	}
}

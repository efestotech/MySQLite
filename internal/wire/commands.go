package wire

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/collate"
	"mysqlite/internal/errs"
	"mysqlite/internal/infoschema"
	"mysqlite/internal/meta"
)

// COM_FIELD_LIST and COM_STATISTICS (docs/03-protocol.md section 4).

// HandleFieldList answers COM_FIELD_LIST: the column definitions of a table of the
// current database, filtered by a LIKE wildcard. Default values are not sent: go-mysql
// encodes their length as 8 fixed bytes instead of a length-encoded integer (ADR 0003,
// G4), and MySQL 8.0 clients do not need them.
func (h *handler) HandleFieldList(table, wildcard string) ([]*mysql.Field, error) {
	ctx := context.Background()
	db, _ := h.sess.Database()
	if db == "" {
		return nil, toMySQL(errs.NoDatabaseSelected())
	}
	var t *meta.Table
	if infoschema.Is(db) {
		var ok bool
		if t, ok = infoschema.Lookup(table); !ok {
			return nil, toMySQL(errs.UnknownTableIn(table, infoschema.Name))
		}
	} else {
		conn, release, err := h.sess.CatalogConn(ctx, db)
		if err != nil {
			return nil, h.fail(err, errs.Hint{DatabaseArg: db})
		}
		defer release()
		if t, err = h.catalog.Table(ctx, conn, db, table); err != nil {
			return nil, h.fail(err, errs.Hint{Database: db})
		}
	}
	defs, e := meta.Columns([]meta.Item{{Star: true}}, t, "")
	if e != nil {
		return nil, toMySQL(e)
	}
	var out []meta.ColumnDef
	for _, d := range defs {
		if wildcard == "" || collate.LikeCI(d.OrgName, wildcard, '\\') {
			out = append(out, d)
		}
	}
	return fields(out), nil
}

func (h *handler) HandleOtherCommand(cmd byte, _ []byte) error {
	if cmd == mysql.COM_STATISTICS && h.filter != nil && h.conn != nil {
		return h.statistics()
	}
	return unknownCommand()
}

// statistics answers COM_STATISTICS with MySQL's one-line status string, a bare
// string packet. go-mysql writes an OK packet after HandleOtherCommand returns nil,
// so the packet is written here and the OK is dropped by the response filter
// (ADR 0003, G5).
func (h *handler) statistics() error {
	c := h.procs.status(nil)
	qps := 0.0
	if c.uptime > 0 {
		qps = float64(c.questions) / float64(c.uptime)
	}
	text := fmt.Sprintf("Uptime: %d  Threads: %d  Questions: %d  Slow queries: 0  Opens: 0  Flush tables: 1  Open tables: 0  Queries per second avg: %.3f",
		c.uptime, c.connected, c.questions, qps)
	data := append(make([]byte, 4, 4+len(text)), text...)
	if err := h.conn.WritePacket(data); err != nil {
		return err
	}
	h.filter.dropNext()
	return nil
}

// responseFilter sits between go-mysql and the client connection. dropNext discards
// the next write, which is one whole packet: go-mysql writes each uncompressed packet
// with one Write call, and mysqlite never negotiates compression.
type responseFilter struct {
	net.Conn
	drop atomic.Bool
}

func (f *responseFilter) dropNext() { f.drop.Store(true) }

func (f *responseFilter) Write(p []byte) (int, error) {
	if f.drop.Swap(false) {
		return len(p), nil
	}
	return f.Conn.Write(p)
}

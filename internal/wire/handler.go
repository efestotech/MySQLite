package wire

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/catalog"
	"mysqlite/internal/dialect"
	"mysqlite/internal/engine"
	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
	"mysqlite/internal/session"
)

// handler implements server.Handler for one connection.
type handler struct {
	eng  *engine.Engine
	sess *session.Session
	tr   *dialect.Translator
	log  *slog.Logger

	// go-mysql calls UseDB for the handshake database before checking the password
	// (docs/agent-notes/api-go-mysql.md, W2). Until authenticated, the name is only
	// recorded here and validated by connAuth.OnAuthSuccess, so a client without
	// credentials cannot learn which databases exist.
	authenticated bool
	pending       string
}

func newHandler(eng *engine.Engine, sess *session.Session, tr *dialect.Translator, log *slog.Logger) *handler {
	return &handler{eng: eng, sess: sess, tr: tr, log: log}
}

func (h *handler) UseDB(dbName string) error {
	if !h.authenticated {
		h.pending = dbName
		return nil
	}
	return h.use(dbName)
}

// afterAuth runs once the password is verified and applies the handshake database.
func (h *handler) afterAuth() error {
	h.authenticated = true
	if h.pending == "" {
		return nil
	}
	name := h.pending
	h.pending = ""
	return h.use(name)
}

func (h *handler) use(name string) error {
	return h.fail(h.sess.Use(name), errs.Hint{DatabaseArg: name})
}

// fail maps err to the MySQL error sent to the client. The original of an unmapped
// error is logged, because the client only sees 1105.
func (h *handler) fail(err error, hint errs.Hint) error {
	e := errs.Map(err, hint)
	if e == nil {
		return nil
	}
	if e.Code == errs.Internal().Code {
		h.log.Error("internal error", "error", err.Error())
	}
	return toMySQL(e)
}

func (h *handler) HandleQuery(query string) (*mysql.Result, error) {
	current, _ := h.sess.Database()
	st, e := h.tr.Translate(query, current)
	if e != nil {
		return nil, toMySQL(e)
	}
	if st.Kind == dialect.KindUse {
		return nil, h.use(st.UseDB)
	}
	if st.Kind == dialect.KindRead {
		return h.read(st)
	}
	// Writes arrive with T7-T8 (transactions, affected rows).
	return nil, toMySQL(errs.Unsupported("statement execution", "writes arrive with M1 T7-T8"))
}

// read runs a SELECT on a read connection and encodes the rows with the text protocol.
// The column definitions come from the catalog, not from the values (docs/04 section 3).
func (h *handler) read(st *dialect.Statement) (*mysql.Result, error) {
	ctx := context.Background()
	conn, err := h.readConn(ctx, st.Database)
	if err != nil {
		return nil, h.fail(err, errs.Hint{DatabaseArg: st.Database})
	}
	defer conn.Close()

	var table *meta.Table
	if st.Shape.Table != "" {
		table, err = catalog.ReadTable(ctx, conn, st.Database, st.Shape.Table)
		if err != nil {
			return nil, h.fail(err, st.Hint())
		}
	}
	defs, e := meta.Columns(st.Shape.Items, table, st.Shape.Alias)
	if e != nil {
		return nil, toMySQL(e)
	}
	if e := meta.CheckRefs(st.Shape.Refs, st.Shape.Items, table, st.Shape.Alias); e != nil {
		return nil, toMySQL(e)
	}
	for _, d := range defs {
		if d.Degraded {
			h.log.Warn("column metadata degraded to text", "table", d.OrgTable, "column", d.OrgName)
		}
	}

	rows, err := conn.QueryContext(ctx, st.SQL, st.Args...)
	if err != nil {
		return nil, h.fail(err, st.Hint())
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		return nil, h.fail(err, st.Hint())
	}
	if len(names) != len(defs) {
		return nil, h.fail(fmt.Errorf("wire: %d result columns, %d column definitions for %q", len(names), len(defs), st.SQL), errs.Hint{})
	}

	rs := &mysql.Resultset{Fields: fields(defs)}
	vals := make([]any, len(defs))
	ptrs := make([]any, len(defs))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, h.fail(err, st.Hint())
		}
		row, err := textRow(defs, vals)
		if err != nil {
			return nil, h.fail(fmt.Errorf("wire: encode row of %q: %w", st.SQL, err), errs.Hint{})
		}
		rs.RowDatas = append(rs.RowDatas, row)
	}
	if err := rows.Err(); err != nil {
		return nil, h.fail(err, st.Hint())
	}
	return mysql.NewResult(rs), nil
}

// readConn returns a read connection of the named database, or the engine's scratch
// connection for a statement that reads no table.
func (h *handler) readConn(ctx context.Context, database string) (*sql.Conn, error) {
	if database == "" {
		return h.eng.Scratch(ctx)
	}
	db, err := h.eng.Database(database)
	if err != nil {
		return nil, err
	}
	return db.Read(ctx)
}

func fields(defs []meta.ColumnDef) []*mysql.Field {
	out := make([]*mysql.Field, len(defs))
	for i, d := range defs {
		out[i] = &mysql.Field{
			Schema:       []byte(d.Schema),
			Table:        []byte(d.Table),
			OrgTable:     []byte(d.OrgTable),
			Name:         []byte(d.Name),
			OrgName:      []byte(d.OrgName),
			Charset:      d.Charset,
			ColumnLength: d.Length,
			Type:         d.Type,
			Flag:         d.Flags,
			Decimal:      d.Decimals,
		}
	}
	return out
}

func (h *handler) HandleFieldList(string, string) ([]*mysql.Field, error) {
	return nil, unknownCommand()
}

func (h *handler) HandleStmtPrepare(string) (int, int, any, error) {
	return 0, 0, nil, unknownCommand()
}

func (h *handler) HandleStmtExecute(any, string, []any) (*mysql.Result, error) {
	return nil, unknownCommand()
}

func (h *handler) HandleStmtClose(any) error {
	return nil
}

func (h *handler) HandleOtherCommand(byte, []byte) error {
	return unknownCommand()
}

// unknownCommand is ER_UNKNOWN_COM_ERROR (1047 / 08S01), docs/03-protocol.md section 4.
func unknownCommand() error {
	return toMySQL(errs.UnknownCommand())
}

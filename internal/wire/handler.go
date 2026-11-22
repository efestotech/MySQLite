package wire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"

	"mysqlite/internal/catalog"
	"mysqlite/internal/dialect"
	"mysqlite/internal/engine"
	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
	"mysqlite/internal/obs"
	"mysqlite/internal/session"
	"mysqlite/internal/vars"
)

// handler implements server.Handler for one connection.
type handler struct {
	eng  *engine.Engine
	sess *session.Session
	tr   *dialect.Translator
	log  *slog.Logger
	// journal records every COM_QUERY; nil records nothing.
	journal *obs.Journal
	// vars is this connection's view of the system variables.
	vars *vars.Session
	// catalog describes tables; shared by all connections.
	catalog *catalog.Cache
	// diag is the diagnostics area SHOW WARNINGS reads; pendingDiag collects the
	// messages of the running statement (diag.go).
	diag, pendingDiag []diagnostic

	// go-mysql calls UseDB for the handshake database before checking the password
	// (docs/agent-notes/api-go-mysql.md, W2). Until authenticated, the name is only
	// recorded here and validated by connAuth.OnAuthSuccess, so a client without
	// credentials cannot learn which databases exist.
	authenticated bool
	pending       string

	// procs is the server's process list and proc this connection's entry, set once
	// authenticated; users are the configured accounts, for SHOW GRANTS.
	procs *processList
	proc  *process
	users map[string]string

	// conn carries the status flags of every OK and EOF packet; set after
	// authentication.
	conn *server.Conn
}

func newHandler(eng *engine.Engine, sess *session.Session, tr *dialect.Translator, log *slog.Logger) *handler {
	return &handler{eng: eng, sess: sess, tr: tr, log: log}
}

func (h *handler) UseDB(dbName string) error {
	if !h.authenticated {
		h.pending = dbName
		return nil
	}
	err := h.use(dbName)
	db, _ := h.sess.Database()
	h.procs.end(h.proc, db)
	return err
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

// syncStatus copies the session's transaction state to the status flags go-mysql puts
// in the response (docs/03-protocol.md section 7), after every command.
func (h *handler) syncStatus() {
	if h.conn == nil {
		return
	}
	flag := func(bit uint16, on bool) {
		if on {
			h.conn.SetStatus(bit)
		} else {
			h.conn.UnsetStatus(bit)
		}
	}
	flag(mysql.SERVER_STATUS_AUTOCOMMIT, h.sess.Autocommit())
	flag(mysql.SERVER_STATUS_IN_TRANS, h.sess.InTransaction())
	flag(mysql.SERVER_STATUS_IN_TRANS_READONLY, h.sess.ReadOnlyTransaction())
}

// close ends the connection's session: an open transaction is rolled back.
func (h *handler) close() {
	if err := h.sess.Close(context.Background()); err != nil {
		h.log.Error("rollback on disconnect", "error", err.Error())
	}
}

func (h *handler) HandleQuery(query string) (*mysql.Result, error) {
	defer h.syncStatus()
	start := time.Now()
	current, _ := h.sess.Database()
	h.beginDiagnostics()
	h.procs.begin(h.proc, current, query)
	st, res, err := h.query(query, current)
	db, _ := h.sess.Database()
	h.procs.end(h.proc, db)
	h.endDiagnostics(st, res, err)
	h.record(query, current, st, res, err, start)
	return res, err
}

func (h *handler) query(query, current string) (*dialect.Statement, *mysql.Result, error) {
	st, e := h.tr.Translate(query, current)
	if e != nil {
		return nil, nil, toMySQL(e)
	}
	if e := h.bindVariables(st); e != nil {
		return st, nil, toMySQL(e)
	}
	var res *mysql.Result
	var err error
	switch st.Kind {
	case dialect.KindUse:
		err = h.use(st.UseDB)
	case dialect.KindTransaction:
		err = h.transaction(st.Tx)
	case dialect.KindRead:
		res, err = h.read(st)
	case dialect.KindShow:
		res, err = h.show(st.Show)
	case dialect.KindSet:
		res, err = h.set(st.Set)
	case dialect.KindDDL:
		res, err = h.createTable(st)
	default:
		res, err = h.write(st)
	}
	return st, res, err
}

// bindVariables reads every system variable the statement uses: each
// dialect.Variable argument becomes the variable's value, and each @@name in the select
// list gets the definition and value its column metadata needs.
func (h *handler) bindVariables(st *dialect.Statement) *errs.Error {
	bind := func(args []any) *errs.Error {
		for i, a := range args {
			v, ok := a.(*dialect.Variable)
			if !ok {
				continue
			}
			val, def, e := h.vars.Get(v.Name, v.Scope)
			if e != nil {
				return e
			}
			args[i] = vars.Arg(def, val)
		}
		return nil
	}
	if e := bind(st.Args); e != nil {
		return e
	}
	if st.Write != nil {
		if e := bind(st.Write.MatchArgs); e != nil {
			return e
		}
	}
	if st.Shape != nil {
		for i := range st.Shape.Items {
			it := st.Shape.Items[i].Var
			if it == nil {
				continue
			}
			val, def, e := h.vars.Get(it.Name, it.Scope)
			if e != nil {
				return e
			}
			it.Def, it.Value = def, val
		}
	}
	return nil
}

// record writes the journal entry of one COM_QUERY (docs/09-conventions.md section 3).
// st is nil when the statement was not translated.
func (h *handler) record(query, current string, st *dialect.Statement, res *mysql.Result, err error, start time.Time) {
	if h.journal == nil {
		return
	}
	e := obs.Entry{
		Time:     start,
		Database: current,
		SQL:      query,
		Duration: time.Since(start),
		Outcome:  obs.OutcomeOK,
	}
	if h.conn != nil {
		e.Session = h.conn.ConnectionID()
		e.User = h.conn.GetUser()
	}
	if st != nil {
		e.Kind = st.Kind.String()
		e.Rules = st.Rules
		e.Emitted = st.SQL
		e.Params = st.Args
	}
	if res != nil {
		e.Affected = res.AffectedRows
		if res.Resultset != nil {
			e.Rows = uint64(len(res.Resultset.RowDatas))
		}
	}
	if err != nil {
		e.Outcome = obs.OutcomeError
		var my *mysql.MyError
		if errors.As(err, &my) {
			e.ErrorCode, e.SQLState, e.Error = my.Code, my.State, my.Message
			if my.Code == errs.Unsupported("", "").Code {
				e.Unsupported = unsupportedConstruct(my.Message)
			}
		} else {
			e.Error = err.Error()
		}
	}
	if werr := h.journal.Record(e); werr != nil {
		h.log.Warn("query journal", "error", werr.Error())
	}
}

// unsupportedConstruct extracts the construct name of a strict-mode 1235 message
// ("mysqlite: unsupported construct <name>: <reason>").
func unsupportedConstruct(msg string) string {
	rest, ok := strings.CutPrefix(msg, "mysqlite: unsupported construct ")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, ": ")
	return name
}

func (h *handler) transaction(tx *dialect.Transaction) error {
	ctx := context.Background()
	var err error
	switch tx.Op {
	case dialect.TxBegin:
		readOnly := tx.ReadOnly || !tx.ReadWrite && h.sess.DefaultReadOnly()
		err = h.sess.Begin(ctx, session.TxOptions{ReadOnly: readOnly, Snapshot: tx.Snapshot})
	case dialect.TxCommit:
		err = h.sess.Commit(ctx)
	case dialect.TxRollback:
		err = h.sess.Rollback(ctx)
	}
	return h.fail(err, errs.Hint{})
}

// read runs a SELECT on a read connection and encodes the rows with the text protocol.
// The column definitions come from the catalog, not from the values (docs/04 section 3).
func (h *handler) read(st *dialect.Statement) (res *mysql.Result, err error) {
	ctx := context.Background()
	conn, release, err := h.sess.Conn(ctx, st.Database, false)
	if err != nil {
		return nil, h.fail(err, errs.Hint{DatabaseArg: st.Database})
	}
	defer release()
	defer func() {
		if err != nil {
			h.sess.StatementFailed(ctx)
		}
	}()

	var table *meta.Table
	if st.Shape.Table != "" {
		table, err = h.catalog.Table(ctx, conn, st.Database, st.Shape.Table)
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

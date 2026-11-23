package wire

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
	"github.com/go-mysql-org/go-mysql/stmt"

	"mysqlite/internal/catalog"
	"mysqlite/internal/dialect"
	"mysqlite/internal/engine"
	"mysqlite/internal/errs"
	"mysqlite/internal/infoschema"
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
	// skipBackup turns off the copy before an ALTER TABLE rebuild (ADR 0008).
	skipBackup bool
	// readOnly refuses writes and DDL with 1290 (--readonly).
	readOnly bool
	// retries counts the statement's attempts to take a busy write lock, for the journal.
	retries int
	// account is the user after a COM_CHANGE_USER; "" means the handshake's user.
	account string
	// fresh replaces sess, tr and vars with those of a new connection.
	fresh func()
	// fixer rewrites prepared-statement packets (stmtfixer.go).
	fixer *stmtFixer
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
	// filter can drop go-mysql's next response packet (COM_STATISTICS).
	filter *responseFilter

	// conn carries the status flags of every OK and EOF packet; set after
	// authentication.
	conn *server.Conn

	// Session function state (sessionfn.go): version is VERSION(); lastInsertID and
	// rowCount are what LAST_INSERT_ID() and ROW_COUNT() return; sink is the open
	// LAST_INSERT_ID(expr) sink of the running statement.
	version      string
	lastInsertID int64
	rowCount     int64
	sink         *statementSink
	// clock is the running statement's time (ADR 0007).
	clock statementClock

	// stmts are the connection's prepared statements (prepared.go), keyed by the
	// context go-mysql hands back at EXECUTE and CLOSE; describe makes read return its
	// column definitions without running the query, for PREPARE.
	stmts map[*stmt.PreparedStmt]*prepared
	// stmtSeq mirrors go-mysql's statement ids, for 1243's message.
	stmtSeq  uint32
	describe bool
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
	e := h.mapErr(err, hint)
	if e == nil {
		return nil
	}
	return toMySQL(e)
}

// mapErr maps err to a MySQL error and logs the internal ones.
func (h *handler) mapErr(err error, hint errs.Hint) *errs.Error {
	e := errs.Map(err, hint)
	if e != nil && e.Code == errs.Internal().Code {
		h.log.Error("internal error", "error", err.Error())
	}
	return e
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
	return h.run(query)
}

// run is COM_QUERY's pipeline, also used by COM_STMT_EXECUTE once the parameter values
// are written into the statement (ADR 0009).
func (h *handler) run(query string) (*mysql.Result, error) {
	defer h.syncStatus()
	start := time.Now()
	h.clock = statementClock{}
	current, _ := h.sess.Database()
	h.beginDiagnostics()
	h.procs.begin(h.proc, current, query)
	h.applyLockWait()
	h.retries = 0
	st, res, err := h.query(query, current)
	h.endStatement(st, res, err)
	db, _ := h.sess.Database()
	h.procs.end(h.proc, db)
	h.endDiagnostics(st, res, err)
	h.record(query, current, st, res, err, start)
	return res, err
}

func (h *handler) query(query, current string) (*dialect.Statement, *mysql.Result, error) {
	st, e := h.tr.TranslateIn(query, current, sessionCatalog{h})
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
	case dialect.KindWrite, dialect.KindDDL:
		if h.readOnly {
			err = toMySQL(errs.ReadOnlyServer())
			break
		}
		if infoschema.Is(st.Database) {
			err = h.infoSchemaDenied()
			break
		}
		if st.Kind == dialect.KindDDL {
			res, err = h.ddlStatement(st)
		} else {
			res, err = h.write(st)
		}
	case dialect.KindShow:
		res, err = h.show(st.Show)
	case dialect.KindSet:
		res, err = h.set(st.Set)
	default:
		err = h.fail(fmt.Errorf("wire: statement kind %v not dispatched", st.Kind), errs.Hint{})
	}
	return st, res, err
}

// bindVariables reads every system variable the statement uses: each
// dialect.Variable argument becomes the variable's value, and each @@name in the select
// list gets the definition and value its column metadata needs.
func (h *handler) bindVariables(st *dialect.Statement) *errs.Error {
	bind := func(args []any) *errs.Error {
		for i, a := range args {
			if sv, ok := a.(*dialect.SessionValue); ok {
				args[i] = h.sessionValue(sv)
				continue
			}
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
		// INSERT IGNORE and ON DUPLICATE KEY UPDATE run one statement per row.
		for _, list := range [][][]any{st.Write.RowArgs, st.Write.UpdateArgs} {
			for _, args := range list {
				if e := bind(args); e != nil {
					return e
				}
			}
		}
	}
	if st.Shape != nil {
		for i := range st.Shape.Items {
			vs := st.Shape.Items[i].Expr.Vars()
			if v := st.Shape.Items[i].Var; v != nil {
				vs = append(vs, v)
			}
			for _, it := range vs {
				val, def, e := h.vars.Get(it.Name, it.Scope)
				if e != nil {
					return e
				}
				it.Def, it.Value = def, val
			}
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
		Retries:  h.retries,
	}
	if h.conn != nil {
		e.Session = h.conn.ConnectionID()
		e.User = h.user()
	}
	if st != nil {
		e.Kind = st.Kind.String()
		e.Rules = st.Rules
		e.Emitted = st.SQL
		e.Params = st.Args
		e.DecimalAsReal = st.DecimalAsReal
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
	var conn *sql.Conn
	var from meta.From
	if infoschema.Is(st.Database) {
		if st.Explain {
			return nil, toMySQL(errs.Unsupported("EXPLAIN in information_schema", "information_schema tables are built when read"))
		}
		if len(st.Shape.From.Sources) > 1 || len(st.Shape.Union) > 0 || len(st.Shape.From.Sources) == 1 && st.Shape.From.Sources[0].Derived != nil {
			return nil, toMySQL(errs.Unsupported("JOIN, UNION or derived table in information_schema", "information_schema tables are built one at a time"))
		}
		var table *meta.Table
		conn, table, err = h.infoSchemaConn(ctx, st.Shape.Table)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		from = meta.Single(table, st.Shape.Alias)
	} else {
		var release func()
		conn, release, err = h.sess.Conn(ctx, st.Database, false)
		if err != nil {
			return nil, h.fail(err, errs.Hint{DatabaseArg: st.Database})
		}
		defer release()
		defer func() {
			if err != nil {
				h.sess.StatementFailed(ctx)
			}
		}()
		// The translator attached the FROM tables, read through the same session.
		from = st.Shape.From
	}
	sel := st.Shape.Select
	sel.From = from
	var defs []meta.ColumnDef
	var e *errs.Error
	if len(st.Shape.Union) > 0 {
		defs, e = meta.CheckUnion(&sel, st.Shape.Union)
	} else {
		defs, e = meta.CheckSelect(&sel)
	}
	if e != nil {
		return nil, toMySQL(e)
	}
	if st.Explain {
		return h.explain(ctx, conn, st, &sel)
	}
	for _, d := range defs {
		if d.Degraded {
			h.log.Warn("column metadata degraded to text", "table", d.OrgTable, "column", d.OrgName)
		}
	}

	if h.describe {
		return mysql.NewResult(&mysql.Resultset{Fields: fields(defs)}), nil
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
		var me *errs.Error
		if errors.As(err, &me) {
			return nil, toMySQL(me)
		}
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

// unknownCommand is ER_UNKNOWN_COM_ERROR (1047 / 08S01), docs/03-protocol.md section 4.
func unknownCommand() error {
	return toMySQL(errs.UnknownCommand())
}

// sessionCatalog is the translator's view of the catalog: tables are read through the
// session, inside its open transaction when it uses that database, and cached by
// schema version (catalog.Cache).
type sessionCatalog struct{ h *handler }

func (c sessionCatalog) Table(database, table string) (*meta.Table, *errs.Error) {
	if infoschema.Is(database) {
		t, ok := infoschema.Lookup(table)
		if !ok {
			return nil, errs.UnknownTableIn(table, infoschema.Name)
		}
		return t, nil
	}
	ctx := context.Background()
	conn, release, err := c.h.sess.CatalogConn(ctx, database)
	if err != nil {
		return nil, c.h.mapErr(err, errs.Hint{DatabaseArg: database})
	}
	defer release()
	t, err := c.h.catalog.Table(ctx, conn, database, table)
	if err != nil {
		return nil, c.h.mapErr(err, errs.Hint{Database: database})
	}
	return t, nil
}

// applyLockWait gives the session the statement's innodb_lock_wait_timeout, the longest
// wait for a database's writer before 1205.
func (h *handler) applyLockWait() {
	if h.vars == nil {
		return
	}
	v, _, e := h.vars.Get("innodb_lock_wait_timeout", vars.ReadSession)
	if e != nil {
		return
	}
	if n, err := strconv.Atoi(v.Text); err == nil && n > 0 {
		h.sess.SetLockWait(time.Duration(n) * time.Second)
	}
}

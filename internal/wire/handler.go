package wire

import (
	"log/slog"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/dialect"
	"mysqlite/internal/errs"
	"mysqlite/internal/session"
)

// handler implements server.Handler for one connection.
type handler struct {
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

func newHandler(sess *session.Session, tr *dialect.Translator, log *slog.Logger) *handler {
	return &handler{sess: sess, tr: tr, log: log}
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
	// Executing translated statements arrives with T5-T8 (result metadata, encoding,
	// transactions, affected rows). Until then only the M0 constant is answered, now
	// recognized from the AST instead of the query text.
	if st.Kind == dialect.KindRead && st.Database == "" && st.SQL == "SELECT 1" {
		rs, err := mysql.BuildSimpleTextResultset([]string{"1"}, [][]any{{int64(1)}})
		if err != nil {
			return nil, h.fail(err, errs.Hint{})
		}
		return mysql.NewResult(rs), nil
	}
	return nil, toMySQL(errs.Unsupported("statement execution", "arrives with M1 T5-T8"))
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

package wire

import (
	"strings"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/errs"
)

// handler implements server.Handler for M0: a hardcoded SELECT 1 and nothing else.
// The real router and dialect layers replace HandleQuery from M1 on.
type handler struct{}

func (h *handler) UseDB(dbName string) error {
	// M0 has no databases. From M1, the name requested during the handshake must be
	// validated only after authentication (docs/agent-notes/api-go-mysql.md, W2).
	return toMySQL(errs.UnknownDatabase(dbName))
}

func (h *handler) HandleQuery(query string) (*mysql.Result, error) {
	// M0 scope is a hardcoded result (docs/08-milestones.md). This string comparison is
	// temporary and is removed when the dialect layer parses every query.
	if strings.EqualFold(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(query), ";")), "SELECT 1") {
		rs, err := mysql.BuildSimpleTextResultset([]string{"1"}, [][]any{{int64(1)}})
		if err != nil {
			return nil, toMySQL(errs.Internal())
		}
		return mysql.NewResult(rs), nil
	}
	return nil, toMySQL(errs.Unsupported("statement", "only SELECT 1 is implemented"))
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

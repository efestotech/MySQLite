package wire

import (
	"errors"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/dialect"
	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// Diagnostics (docs/04-session-catalog.md section 1). MySQL keeps the messages of the
// last statement that produced any, for SHOW WARNINGS / SHOW ERRORS:
//
//   - a statement that raises warnings or fails replaces the list;
//   - a statement that uses a table replaces it, with an empty list when it raised none;
//   - SHOW WARNINGS, SHOW ERRORS and statements without tables and without messages
//     keep it.
//
// The rule is oracle pending (ADR 0002 addendum).

const (
	levelWarning = "Warning"
	levelError   = "Error"
)

type diagnostic struct {
	Level   string
	Code    uint16
	Message string
}

// warn records a warning of the running statement.
func (h *handler) warn(e *errs.Error) {
	h.pendingDiag = append(h.pendingDiag, diagnostic{Level: levelWarning, Code: e.Code, Message: e.Message})
}

// diagCounts is @@warning_count and @@error_count.
func (h *handler) diagCounts() (warnings, errors int) {
	for _, d := range h.diag {
		if d.Level == levelError {
			errors++
		}
	}
	return len(h.diag), errors
}

// beginDiagnostics starts a statement: its own warnings start empty.
func (h *handler) beginDiagnostics() {
	h.pendingDiag = nil
}

// endDiagnostics applies the rule above once the statement has run, and sets the
// warning count of the response.
func (h *handler) endDiagnostics(st *dialect.Statement, res *mysql.Result, err error) {
	pending := h.pendingDiag
	h.pendingDiag = nil
	if err != nil {
		var my *mysql.MyError
		if errors.As(err, &my) {
			pending = append(pending, diagnostic{Level: levelError, Code: my.Code, Message: my.Message})
		}
		h.diag = pending
		return
	}
	switch {
	case st != nil && st.Kind == dialect.KindShow && st.Show != nil &&
		(st.Show.Type == dialect.ShowWarnings || st.Show.Type == dialect.ShowErrors):
	case len(pending) > 0 || usesTable(st):
		h.diag = pending
	}
	n := uint16(min(len(pending), 65535))
	if res != nil {
		res.Warnings = n
	}
	if h.conn != nil {
		h.conn.SetWarnings(n)
	}
}

func usesTable(st *dialect.Statement) bool {
	if st == nil {
		return false
	}
	switch st.Kind {
	case dialect.KindRead:
		return st.Shape != nil && st.Shape.Table != ""
	case dialect.KindTransaction, dialect.KindUse, dialect.KindSet, dialect.KindShow:
		return false
	}
	return true
}

// showDiagnostics is SHOW WARNINGS, SHOW ERRORS and their COUNT(*) forms.
func (h *handler) showDiagnostics(s *dialect.Show) (*mysql.Result, error) {
	warnings, errorCount := h.diagCounts()
	if s.Count {
		name, n := "@@session.warning_count", warnings
		if s.Type == dialect.ShowErrors {
			name, n = "@@session.error_count", errorCount
		}
		defs := []meta.ColumnDef{{
			Name: name, Charset: meta.CharsetBinary, Type: meta.TypeLongLong,
			Flags: meta.FlagBinary | meta.FlagNum | meta.FlagUnsigned, Length: 20,
		}}
		return staticResult(defs, [][]any{{int64(n)}})
	}
	defs := []meta.ColumnDef{
		{Name: "Level", Charset: meta.CollationUTF8MB3, Type: meta.TypeVarString, Flags: meta.FlagNotNull, Length: 7 * 3},
		{Name: "Code", Charset: meta.CharsetBinary, Type: meta.TypeLong, Flags: meta.FlagNotNull | meta.FlagUnsigned | meta.FlagBinary | meta.FlagNum, Length: 4},
		{Name: "Message", Charset: meta.CollationUTF8MB3, Type: meta.TypeVarString, Flags: meta.FlagNotNull, Length: 512 * 3},
	}
	var rows [][]any
	for _, d := range h.diag {
		if s.Type == dialect.ShowErrors && d.Level != levelError {
			continue
		}
		rows = append(rows, []any{d.Level, int64(d.Code), d.Message})
	}
	return staticResult(defs, rows)
}

// staticResult builds a result set from values computed by mysqlite, encoded like rows
// read from SQLite.
func staticResult(defs []meta.ColumnDef, rows [][]any) (*mysql.Result, error) {
	rs := &mysql.Resultset{Fields: fields(defs)}
	for _, vals := range rows {
		row, err := textRow(defs, vals)
		if err != nil {
			return nil, toMySQL(errs.Internal())
		}
		rs.RowDatas = append(rs.RowDatas, row)
	}
	return mysql.NewResult(rs), nil
}

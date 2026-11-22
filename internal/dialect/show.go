package dialect

import (
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"

	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// ShowType is the SHOW statement kind.
type ShowType int

const (
	ShowWarnings ShowType = iota + 1
	ShowErrors
	ShowDatabases
	ShowTables
	ShowColumns
	ShowCreateTable
	ShowIndex
	ShowTableStatus
	ShowVariables
	ShowStatus
	ShowEngines
	ShowCharset
	ShowCollation
	ShowProcessList
	ShowGrants
	ShowTriggers
	ShowEvents
	ShowCreateDatabase
)

// Show describes a SHOW statement; the executor builds its rows from session or catalog
// state (docs/04-session-catalog.md section 4) and filters them.
type Show struct {
	Type ShowType
	// Count is SHOW COUNT(*) WARNINGS / ERRORS.
	Count bool
	// Full is SHOW FULL TABLES / COLUMNS / PROCESSLIST.
	Full bool
	// IfNotExists is SHOW CREATE DATABASE IF NOT EXISTS.
	IfNotExists bool
	// Global is SHOW GLOBAL VARIABLES / STATUS.
	Global bool
	// User is the account of SHOW GRANTS FOR; nil for the current user.
	User *ShowUser
	// Database is FROM db or the current database; Table the table of SHOW COLUMNS,
	// CREATE TABLE and INDEX.
	Database, Table string
	// Like is the LIKE pattern, applied to the first result column; nil without LIKE.
	Like *string
	// Where is " WHERE <predicate>" over the result columns, as SQLite SQL with Args.
	// The executor runs it over the rows, so it is a real predicate, not a string match
	// (docs/01-architecture.md rule 3). Refs are its column references.
	Where string
	Args  []any
	Refs  []meta.Ref
}

// ShowUser is a user@host account.
type ShowUser struct {
	Name, Host string
}

var showTypes = map[ast.ShowStmtType]ShowType{
	ast.ShowVariables:      ShowVariables,
	ast.ShowStatus:         ShowStatus,
	ast.ShowEngines:        ShowEngines,
	ast.ShowCharset:        ShowCharset,
	ast.ShowCollation:      ShowCollation,
	ast.ShowProcessList:    ShowProcessList,
	ast.ShowGrants:         ShowGrants,
	ast.ShowDatabases:      ShowDatabases,
	ast.ShowTables:         ShowTables,
	ast.ShowColumns:        ShowColumns,
	ast.ShowCreateTable:    ShowCreateTable,
	ast.ShowIndex:          ShowIndex,
	ast.ShowTableStatus:    ShowTableStatus,
	ast.ShowTriggers:       ShowTriggers,
	ast.ShowEvents:         ShowEvents,
	ast.ShowCreateDatabase: ShowCreateDatabase,
}

func (e *emitter) show(n *ast.ShowStmt) *Statement {
	if n.Limit != nil {
		e.unsupported("SHOW ... LIMIT", reasonLater)
		return nil
	}
	switch n.Tp {
	case ast.ShowWarnings, ast.ShowErrors:
		t := ShowWarnings
		if n.Tp == ast.ShowErrors {
			t = ShowErrors
		}
		return &Statement{Kind: KindShow, Show: &Show{Type: t, Count: n.CountWarningsOrErrors}}
	}
	t, ok := showTypes[n.Tp]
	if !ok {
		e.unsupported(statementName(n), reasonLater)
		return nil
	}
	s := &Show{Type: t, Full: n.Full, Global: n.GlobalScope}
	switch t {
	case ShowGrants:
		if len(n.Roles) > 0 {
			e.unsupported("SHOW GRANTS ... USING", reasonLater)
			return nil
		}
		if n.User != nil && !n.User.CurrentUser {
			s.User = &ShowUser{Name: n.User.Username, Host: n.User.Hostname}
		}
	case ShowCreateDatabase:
		s.Database, s.IfNotExists = n.DBName, n.IfNotExists
	case ShowTables, ShowTableStatus, ShowTriggers, ShowEvents:
		s.Database = n.DBName
		if s.Database == "" {
			s.Database = e.current
		}
		if s.Database == "" {
			e.fail(errs.NoDatabaseSelected())
			return nil
		}
	case ShowColumns, ShowCreateTable, ShowIndex:
		s.Table = n.Table.Name.O
		s.Database = n.Table.Schema.O
		if n.DBName != "" {
			// SHOW COLUMNS FROM t FROM db: the FROM db wins, as in MySQL.
			s.Database = n.DBName
		}
		if s.Database == "" {
			s.Database = e.current
		}
		if s.Database == "" {
			e.fail(errs.NoDatabaseSelected())
			return nil
		}
		if n.Column != nil {
			// DESCRIBE t col: col is a LIKE pattern on Field.
			p := n.Column.Name.O
			s.Like = &p
		}
	}
	if n.Pattern != nil {
		lit := valueLiteral(n.Pattern.Pattern)
		if lit == nil || lit.Kind != meta.LiteralString || n.Pattern.Not {
			e.unsupported("SHOW ... LIKE with an expression", reasonLater)
			return nil
		}
		p := lit.Text
		s.Like = &p
	}
	if n.Where != nil {
		e.write(" WHERE ")
		e.clause = meta.ClauseWhere
		e.refs = &s.Refs
		e.expr(n.Where)
		e.clause = ""
		if e.err != nil {
			return nil
		}
		s.Where, s.Args = e.b.String(), e.args
	}
	return &Statement{Kind: KindShow, Database: s.Database, Show: s}
}

// LikeColumn names the first column of SHOW TABLES / DATABASES with a LIKE pattern, as
// MySQL does: "Tables_in_shop (it%)".
func LikeColumn(base string, like *string) string {
	if like == nil {
		return base
	}
	return base + " (" + strings.TrimSpace(*like) + ")"
}

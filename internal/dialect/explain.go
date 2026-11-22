package dialect

import (
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"

	"mysqlite/internal/errs"
)

// explain translates EXPLAIN [FORMAT=TRADITIONAL] of a SELECT or a set operation: the
// query is translated and checked as a read, and the executor shows SQLite's plan for
// it in MySQL's EXPLAIN columns (Statement.Explain).
func (e *emitter) explain(n *ast.ExplainStmt) *Statement {
	switch format := strings.ToLower(n.Format); {
	case n.Analyze:
		e.unsupported("EXPLAIN ANALYZE", reasonLater)
		return nil
	case n.Stmt == nil || n.Explore:
		e.unsupported("EXPLAIN without a statement", reasonLater)
		return nil
	case format != "" && format != "row" && format != "traditional":
		e.unsupported("EXPLAIN FORMAT="+strings.ToUpper(n.Format), reasonLater)
		return nil
	}
	switch q := n.Stmt.(type) {
	case *ast.SelectStmt:
		e.selectStmt(q)
	case *ast.SetOprStmt:
		e.setOpr(q)
	default:
		e.unsupported("EXPLAIN of a write statement", reasonLater)
		return nil
	}
	st := e.result(KindRead, errs.OpOther)
	st.Explain = true
	return st
}

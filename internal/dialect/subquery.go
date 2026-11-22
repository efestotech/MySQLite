package dialect

import (
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/opcode"

	"mysqlite/internal/dialect/functions"
	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// scalarColumn names the column of a scalar subquery whose item has no alias, so the
// row check can read it.
const scalarColumn = "mysqlite_scalar"

// subSelect emits a subquery's SELECT into its own text and arguments, with its own
// shape, and returns what meta checks of it. Correlated references resolve against
// the enclosing queries when the statement is checked. scalar aliases the first item
// (scalarColumn) for the row check.
func (e *emitter) subSelect(q *ast.SubqueryExpr, scalar bool) (string, []any, *meta.Select) {
	if e.shape == nil || e.wr != nil {
		// MySQL also refuses most of these (1093, a write reading its own table).
		e.unsupported("subquery in a write statement", reasonLater)
		return "", nil, nil
	}
	sel, ok := q.Query.(*ast.SelectStmt)
	if !ok {
		e.unsupported("UNION in a subquery", reasonLater)
		return "", nil, nil
	}
	e.apply(ruleSubquery)
	shape, refs, checks, clause, inAggregate := e.shape, e.refs, e.checks, e.clause, e.inAggregate
	text, args := e.sub(func() {
		e.clause, e.inAggregate, e.aliasFirst = "", false, scalar
		e.selectStmt(sel)
	})
	inner := e.shape
	e.shape, e.refs, e.checks, e.clause, e.inAggregate, e.aliasFirst = shape, refs, checks, clause, inAggregate, false
	if e.err != nil || inner == nil {
		return "", nil, nil
	}
	return text, args, &inner.Select
}

// scalarSubquery emits a subquery used as a value. SQLite returns the first row of a
// subquery with several; MySQL fails with 1242, so the value is read through a check
// of at most two rows.
func (e *emitter) scalarSubquery(q *ast.SubqueryExpr) *meta.Expr {
	text, args, sel := e.subSelect(q, true)
	if sel == nil {
		return nil
	}
	e.write("(SELECT CASE WHEN count(*) > 1 THEN ", functions.UDFRaise, "(1242) ELSE max(",
		quoteIdent(e.scalarName), ") END FROM (SELECT * FROM (", text, ") LIMIT 2))")
	e.args = append(e.args, args...)
	return e.check(&meta.Expr{Kind: meta.ExprSubquery, Sub: sel})
}

// inSubquery emits x [NOT] IN (subquery).
func (e *emitter) inSubquery(x ast.ExprNode, q *ast.SubqueryExpr, not bool) *meta.Expr {
	if s, ok := q.Query.(*ast.SelectStmt); ok && s.Limit != nil {
		e.fail(errs.LimitInSubquery())
		return nil
	}
	e.write("(")
	l := e.expr(x)
	if not {
		e.write(" NOT")
	}
	e.write(" IN (")
	text, args, sel := e.subSelect(q, false)
	if sel == nil || l == nil {
		return nil
	}
	e.write(text, "))")
	e.args = append(e.args, args...)
	return e.check(&meta.Expr{Kind: meta.ExprBool, Op: meta.OpInSubquery, Args: []*meta.Expr{l}, Sub: sel})
}

// exists emits [NOT] EXISTS (subquery), never NULL.
func (e *emitter) exists(n *ast.ExistsSubqueryExpr) *meta.Expr {
	q, ok := n.Sel.(*ast.SubqueryExpr)
	if !ok {
		e.fail(errs.Internal())
		return nil
	}
	text, args, sel := e.subSelect(q, false)
	if sel == nil {
		return nil
	}
	if n.Not {
		e.write("NOT ")
	}
	e.write("EXISTS (", text, ")")
	e.args = append(e.args, args...)
	return e.check(&meta.Expr{Kind: meta.ExprBool, Op: meta.OpExists, Sub: sel, NotNull: true})
}

// compareSubquery emits x op ANY|SOME|ALL (subquery). = ANY is IN and <> ALL is NOT IN,
// also for NULLs; the other forms need MIN/MAX rewrites whose NULL handling differs, and
// are refused.
func (e *emitter) compareSubquery(n *ast.CompareSubqueryExpr) *meta.Expr {
	q, ok := n.R.(*ast.SubqueryExpr)
	if !ok {
		e.unsupported("ANY or ALL over a row", reasonLater)
		return nil
	}
	switch {
	case n.Op == opcode.EQ && !n.All:
		return e.inSubquery(n.L, q, false)
	case n.Op == opcode.NE && n.All:
		return e.inSubquery(n.L, q, true)
	}
	quantifier := "ANY"
	if n.All {
		quantifier = "ALL"
	}
	op := binaryOps[n.Op]
	if op == "" {
		op = n.Op.String()
	}
	e.unsupported(op+" "+quantifier+" subquery", reasonLater)
	return nil
}

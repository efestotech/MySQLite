package dialect

import (
	"strconv"

	"github.com/pingcap/tidb/pkg/parser/ast"

	"mysqlite/internal/charset"
	"mysqlite/internal/dialect/functions"
	"mysqlite/internal/meta"
)

// Operators SQLite computes differently (docs/05-dialect.md section 4) go through the
// UDFs of package udf.

// division emits udf(L, R, raise) for /, DIV and MOD. raise makes a zero divisor
// MySQL's 1365: an INSERT or UPDATE in strict mode with ERROR_FOR_DIVISION_BY_ZERO
// fails, everything else gets NULL.
func (e *emitter) division(udf string, n *ast.BinaryOperationExpr) (l, r *meta.Expr) {
	e.apply(ruleFunction)
	e.write(udf, "(")
	l = e.expr(n.L)
	e.write(", ")
	r = e.expr(n.R)
	raise := "0"
	if e.changes && e.mode.HasStrictMode() && e.mode.HasErrorForDivisionByZeroMode() {
		raise = "1"
	}
	e.write(", ", raise, ")")
	return l, r
}

// like emits [NOT] mysqlite_like(value, pattern, escape, mode). The comparison is binary
// (mode 1) when a string operand has a binary collation (binary, *_bin), follows
// utf8mb4_0900_ai_ci (mode 2) when an operand has that collation, else mysql_ci (mode 0).
// Numbers compare as their MySQL string form.
func (e *emitter) like(n *ast.PatternLikeOrIlikeExpr) *meta.Expr {
	if !n.IsLike {
		e.unsupported("ILIKE", reasonLater)
		return nil
	}
	e.apply(ruleFunction)
	value, vx := e.subExpr(n.Expr)
	pattern, px := e.subExpr(n.Pattern)
	x := node(meta.ExprBool, "LIKE", vx, px)
	if x == nil {
		return nil
	}
	// No ESCAPE clause: backslash, or none under NO_BACKSLASH_ESCAPES (MySQL's grammar).
	escape := -1
	switch {
	case n.EscapeExplicit && n.Escape != 0:
		escape = int(n.Escape)
	case !e.mode.HasNoBackslashEscapesMode():
		escape = '\\'
	}
	mode := "0"
	switch {
	case e.binaryString(vx) || e.binaryString(px):
		mode = "1"
	case e.accentInsensitive0900(vx) || e.accentInsensitive0900(px):
		mode = "2"
	}
	if n.Not {
		e.write("(NOT ")
	}
	e.write(functions.UDFLike, "(", e.textForm(value, vx), ", ", e.textForm(pattern, px), ", ",
		strconv.Itoa(escape), ", ", mode, ")")
	if n.Not {
		e.write(")")
	}
	return x
}

// binaryString reports a string operand with a binary collation. An operand whose type
// is unknown (no catalog) is not binary.
func (e *emitter) binaryString(x *meta.Expr) bool {
	d, err := meta.Infer(x, e.scope())
	return err == nil && meta.IsString(d) && charset.Binary(d.Charset)
}

// accentInsensitive0900 reports a string operand collated utf8mb4_0900_ai_ci.
func (e *emitter) accentInsensitive0900(x *meta.Expr) bool {
	d, err := meta.Infer(x, e.scope())
	return err == nil && meta.IsString(d) && d.Charset == charset.ID0900AI
}

// caseExpr emits CASE natively: SQLite evaluates it as MySQL does, and the simple form
// compares with =, like the = operator. The result type merges the THEN and ELSE
// branches: the node is a CASE function whose first argument groups the operand and the
// conditions, so they are still typed and bound.
func (e *emitter) caseExpr(n *ast.CaseExpr) *meta.Expr {
	var conds, results []*meta.Expr
	e.write("CASE")
	if n.Value != nil {
		e.write(" ")
		conds = append(conds, e.expr(n.Value))
	}
	for _, w := range n.WhenClauses {
		e.write(" WHEN ")
		conds = append(conds, e.expr(w.Expr))
		e.write(" THEN ")
		results = append(results, e.expr(w.Result))
	}
	if n.ElseClause != nil {
		e.write(" ELSE ")
		results = append(results, e.expr(n.ElseClause))
	} else {
		results = append(results, &meta.Expr{Kind: meta.ExprLiteral, Literal: &meta.Literal{Kind: meta.LiteralNull, Text: "NULL"}})
	}
	e.write(" END")
	group := node(meta.ExprBool, "WHEN", conds...)
	x := node(meta.ExprFunc, "CASE", append([]*meta.Expr{group}, results...)...)
	if x != nil {
		x.Ret = meta.RetMergeTail
	}
	return x
}

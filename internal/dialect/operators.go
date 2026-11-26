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
	valueNode, binV := unwrapBinary(n.Expr)
	patternNode, binP := unwrapBinary(n.Pattern)
	value, vx := e.subExpr(valueNode)
	pattern, px := e.subExpr(patternNode)
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
	case binV || binP || e.binaryString(vx) || e.binaryString(px):
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

// unwrapBinary strips the BINARY operator from an operand of LIKE or REGEXP, where it
// only makes the comparison binary. Elsewhere BINARY is a cast mysqlite does not
// translate: a SQLite BLOB never equals a TEXT.
func unwrapBinary(x ast.ExprNode) (ast.ExprNode, bool) {
	if c, ok := x.(*ast.FuncCastExpr); ok && c.FunctionType == ast.CastBinaryOperator {
		return c.Expr, true
	}
	return x, false
}

// regexp emits [NOT] mysqlite_regexp(value, pattern, binary). As in MySQL the pattern
// matches anywhere in the value, case-insensitively unless an operand is a binary
// string. Numbers match as their MySQL string form.
func (e *emitter) regexp(n *ast.PatternRegexpExpr) *meta.Expr {
	e.apply(ruleFunction)
	valueNode, binV := unwrapBinary(n.Expr)
	patternNode, binP := unwrapBinary(n.Pattern)
	value, vx := e.subExpr(valueNode)
	pattern, px := e.subExpr(patternNode)
	x := node(meta.ExprBool, "REGEXP", vx, px)
	if x == nil {
		return nil
	}
	binary := "0"
	if binV || binP || e.binaryString(vx) || e.binaryString(px) {
		binary = "1"
	}
	if n.Not {
		e.write("(NOT ")
	}
	e.write(functions.UDFRegexp, "(", e.textForm(value, vx), ", ", e.textForm(pattern, px), ", ", binary, ")")
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

// comparison emits (l op r). MySQL compares a string with a number as two doubles
// ('10' = 10, 'abc' = 0, '1abc' = 1); SQLite finds a TEXT value unequal to any number
// when no column affinity converts it. The string operand of such a pair becomes
// CAST(... AS REAL), which takes the numeric prefix as MySQL does. Operands of unknown
// type are left alone.
func (e *emitter) comparison(n *ast.BinaryOperationExpr, op string) (l, r *meta.Expr) {
	lt, l := e.subExpr(n.L)
	rt, r := e.subExpr(n.R)
	if l != nil && r != nil {
		ld, lok := meta.InferKnown(l, e.scope())
		rd, rok := meta.InferKnown(r, e.scope())
		if lok && rok {
			switch {
			case comparesAsText(ld) && meta.IsNumber(rd):
				lt = "CAST(" + lt + " AS REAL)"
			case meta.IsNumber(ld) && comparesAsText(rd):
				rt = "CAST(" + rt + " AS REAL)"
			}
		}
	}
	e.write("(", lt, " ", op, " ", rt, ")")
	return l, r
}

// comparesAsText is a character or binary string that MySQL converts to a double when
// compared with a number (not ENUM, SET or JSON, which have their own rules).
func comparesAsText(d meta.ColumnDef) bool {
	switch d.Type {
	case meta.TypeEnum, meta.TypeSet, meta.TypeJSON:
		return false
	}
	return meta.IsString(d)
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

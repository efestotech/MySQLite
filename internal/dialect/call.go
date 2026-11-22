package dialect

import (
	"math"
	"strconv"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/test_driver"

	"mysqlite/internal/dialect/functions"
	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// call emits a function call from the function table (docs/05-dialect.md section 5). A
// function outside the table is 1235; a wrong argument count is MySQL's 1582.
func (e *emitter) call(n *ast.FuncCallExpr) *meta.Expr {
	name := strings.ToUpper(n.FnName.L)
	f, ok := functions.Lookup(name)
	if !ok {
		e.unsupported("function "+name, reasonLater)
		return nil
	}
	if len(n.Args) < f.MinArgs || (f.MaxArgs >= 0 && len(n.Args) > f.MaxArgs) {
		e.fail(errs.WrongParamCount(n.FnName.O))
		return nil
	}
	e.apply(ruleFunction)
	switch {
	case f.Emit == functions.Session && len(n.Args) > 0 && f.ArgUDF != "":
		f = functions.Func{Emit: functions.Rename, SQLite: f.ArgUDF, Zoned: true, Ret: f.Ret}
	case f.Emit == functions.Session || f.Emit == functions.LastInsertID && len(n.Args) == 0:
		sv := &SessionValue{Name: f.Session}
		x := &meta.Expr{Kind: meta.ExprFunc, Op: name, Ret: f.Ret, NotNull: true}
		if len(n.Args) > 0 {
			fsp, ok := e.precision(n, f)
			if !ok {
				return nil
			}
			sv.Fsp = fsp
			x.Args = []*meta.Expr{{Kind: meta.ExprLiteral, Literal: &meta.Literal{Kind: meta.LiteralInt, Text: strconv.Itoa(fsp)}}}
		}
		e.write("?")
		e.args = append(e.args, sv)
		return x
	case f.Emit == functions.LastInsertID:
		// The sink is bound before the argument's own arguments, in text order.
		e.write(functions.UDFLastInsertID, "(?, ")
		e.args = append(e.args, &SessionValue{Name: functions.SessionSink})
		x := e.expr(n.Args[0])
		e.write(")")
		if x == nil {
			return nil
		}
		return &meta.Expr{Kind: meta.ExprFunc, Op: name, Ret: f.Ret, Args: []*meta.Expr{x}}
	}
	texts := make([]string, len(n.Args))
	nodes := make([]*meta.Expr, len(n.Args))
	for i, a := range n.Args {
		texts[i], nodes[i] = e.subExpr(a)
		if nodes[i] == nil {
			if e.err == nil {
				e.unsupported("function "+name+" argument", reasonLater)
			}
			return nil
		}
		if f.TextArgs {
			texts[i] = e.textForm(texts[i], nodes[i])
		}
	}
	switch f.Emit {
	case functions.Rename:
		name := f.SQLiteName(len(n.Args))
		for len(texts) < f.Pad {
			texts = append(texts, "NULL")
		}
		if f.Extra != "" {
			texts = append(texts, f.Extra)
		}
		if f.Zoned {
			texts = append(texts, "?")
		}
		e.write(name, "(", strings.Join(texts, ", "), ")")
		if f.Zoned {
			e.args = append(e.args, &SessionValue{Name: functions.SessionTimeZone})
		}
	case functions.Bytes:
		e.write("length(CAST(", texts[0], " AS BLOB))")
	case functions.Exact:
		if len(texts) == 1 {
			texts = append(texts, "0")
		}
		exact := "1"
		if d, err := meta.Infer(nodes[0], e.scope()); err == nil && !meta.IsExact(d) {
			exact = "0"
		}
		e.write(f.SQLite, "(", strings.Join(texts, ", "), ", ", exact, ")")
	case functions.Concat:
		if len(texts) == 1 {
			texts = append(texts, "''")
		}
		e.write("(", strings.Join(texts, " || "), ")")
	}
	return &meta.Expr{Kind: meta.ExprFunc, Op: name, Ret: f.Ret, Args: nodes}
}

// subExpr emits x into its own text, leaving the statement text unchanged; its bound
// arguments are appended in order as usual.
func (e *emitter) subExpr(x ast.ExprNode) (string, *meta.Expr) {
	prefix := e.b.String()
	e.b.Reset()
	node := e.expr(x)
	text := e.b.String()
	e.b.Reset()
	e.b.WriteString(prefix)
	return text, node
}

// textForm converts an argument to the string MySQL makes of it: a DECIMAL keeps its
// scale (2.50, where SQLite's REAL prints 2.5), a DOUBLE prints as MySQL prints it
// (1e20, not 1.0e+20). Integers, strings and dates are already in MySQL's form.
func (e *emitter) textForm(text string, node *meta.Expr) string {
	d, err := meta.Infer(node, e.scope())
	if err != nil {
		return text
	}
	switch d.Type {
	case meta.TypeNewDecimal, meta.TypeDecimal:
		return functions.UDFDecimalText + "(" + text + ", " + strconv.Itoa(int(d.Decimals)) + ")"
	case meta.TypeDouble, meta.TypeFloat:
		if d.Decimals < meta.NotFixedDecimals {
			return functions.UDFDecimalText + "(" + text + ", " + strconv.Itoa(int(d.Decimals)) + ")"
		}
		bits := "64"
		if d.Type == meta.TypeFloat {
			bits = "32"
		}
		return functions.UDFDoubleText + "(" + text + ", " + bits + ")"
	}
	return text
}

// scope is the FROM clause the statement's column references resolve against, as far
// as it has been read: a JOIN's ON sees the tables before it.
func (e *emitter) scope() meta.From {
	switch {
	case e.shape != nil:
		return e.shape.From
	case e.wr != nil:
		return meta.Single(e.wr.Meta, e.wr.Alias)
	}
	return meta.From{}
}

// precision reads the fractional seconds precision of NOW(fsp) and the like: an integer
// literal up to 6 (1426 above, as MySQL).
func (e *emitter) precision(n *ast.FuncCallExpr, f functions.Func) (int, bool) {
	if !f.Fsp {
		e.fail(errs.WrongParamCount(n.FnName.O))
		return 0, false
	}
	v, ok := n.Args[0].(*test_driver.ValueExpr)
	if !ok {
		e.unsupported(strings.ToUpper(n.FnName.L)+" with a computed precision", reasonLater)
		return 0, false
	}
	var fsp int64
	switch v.Kind() {
	case test_driver.KindInt64:
		fsp = v.GetInt64()
	case test_driver.KindUint64:
		fsp = int64(min(v.GetUint64(), math.MaxInt64))
	default:
		e.unsupported(strings.ToUpper(n.FnName.L)+" with a computed precision", reasonLater)
		return 0, false
	}
	if fsp < 0 || fsp > 6 {
		e.fail(errs.TooBigPrecision(fsp, n.FnName.L))
		return 0, false
	}
	return int(fsp), true
}

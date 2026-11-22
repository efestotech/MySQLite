package dialect

import (
	"strconv"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"

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
		e.write(f.SQLiteName(len(texts)), "(", strings.Join(texts, ", "), ")")
	case functions.Bytes:
		e.write("length(CAST(", texts[0], " AS BLOB))")
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
	t, alias := e.scope()
	d, err := meta.Infer(node, t, alias)
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

// scope is the table the statement's column references resolve against.
func (e *emitter) scope() (*meta.Table, string) {
	switch {
	case e.shape != nil:
		return e.shape.Meta, e.shape.Alias
	case e.wr != nil:
		return e.wr.Meta, e.wr.Alias
	}
	return nil, ""
}

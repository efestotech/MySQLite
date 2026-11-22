package dialect

import (
	"strconv"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/test_driver"

	"mysqlite/internal/dialect/functions"
	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
	"mysqlite/internal/vars"
)

// aggregates are the aggregate functions SQLite computes as MySQL does, with their
// result types (docs/05-dialect.md section 5). GROUP_CONCAT is emitted on its own.
var aggregates = map[string]meta.FuncRet{
	"COUNT":        meta.RetCount,
	"SUM":          meta.RetSum,
	"AVG":          meta.RetAvg,
	"MIN":          meta.RetMinMax,
	"MAX":          meta.RetMinMax,
	"GROUP_CONCAT": meta.RetGroupConcat,
}

// aggregate emits an aggregate function. Aggregates belong to the select list, HAVING
// and ORDER BY of a SELECT; anywhere else, or inside another aggregate, they are
// MySQL's 1111.
func (e *emitter) aggregate(n *ast.AggregateFuncExpr) *meta.Expr {
	name := strings.ToUpper(n.F)
	ret, ok := aggregates[name]
	if !ok {
		e.unsupported("aggregate "+name, reasonLater)
		return nil
	}
	if e.shape == nil || e.inAggregate {
		e.fail(errs.InvalidGroupFunc())
		return nil
	}
	switch e.clause {
	case meta.ClauseWhere, meta.ClauseOn, meta.ClauseGroup:
		e.fail(errs.InvalidGroupFunc())
		return nil
	}
	e.apply(ruleFunction)
	e.shape.Group.Aggregated = true
	e.inAggregate = true
	defer func() { e.inAggregate = false }()

	args := n.Args
	if name == "GROUP_CONCAT" {
		return e.groupConcat(n, ret)
	}
	if len(args) != 1 {
		if name == "COUNT" && n.Distinct {
			e.unsupported("COUNT(DISTINCT) with several expressions", "SQLite's DISTINCT aggregates take one argument")
		} else {
			e.fail(errs.WrongParamCount(n.F))
		}
		return nil
	}
	e.write(strings.ToLower(name), "(")
	if n.Distinct {
		e.write("DISTINCT ")
	}
	x := e.expr(args[0])
	e.write(")")
	if x == nil {
		return nil
	}
	return &meta.Expr{Kind: meta.ExprFunc, Op: name, Ret: ret, Args: []*meta.Expr{x}, Agg: true}
}

// groupConcat emits GROUP_CONCAT([DISTINCT] x, ... [ORDER BY ...] [SEPARATOR s]) as
// SQLite's group_concat over the arguments' MySQL string forms, cut at
// group_concat_max_len bytes. The parser appends the separator as the last argument.
func (e *emitter) groupConcat(n *ast.AggregateFuncExpr, ret meta.FuncRet) *meta.Expr {
	if len(n.Args) < 2 {
		e.fail(errs.WrongParamCount(n.F))
		return nil
	}
	sepValue, ok := n.Args[len(n.Args)-1].(*test_driver.ValueExpr)
	if !ok {
		e.fail(errs.Internal())
		return nil
	}
	sep := sepValue.GetString()
	if n.Distinct && sep != "," {
		e.unsupported("GROUP_CONCAT(DISTINCT) with a separator", "SQLite's DISTINCT aggregates take one argument")
		return nil
	}
	exprs := n.Args[:len(n.Args)-1]
	texts := make([]string, len(exprs))
	nodes := make([]*meta.Expr, len(exprs))
	for i, a := range exprs {
		texts[i], nodes[i] = e.subExpr(a)
		if nodes[i] == nil {
			if e.err == nil {
				e.unsupported("GROUP_CONCAT argument", reasonLater)
			}
			return nil
		}
		texts[i] = e.textForm(texts[i], nodes[i])
	}
	e.write(functions.UDFCut, "(group_concat(")
	if n.Distinct {
		e.write("DISTINCT ")
	}
	value := texts[0]
	if len(texts) > 1 {
		value = "(" + strings.Join(texts, " || ") + ")"
	}
	e.write(value)
	if !n.Distinct {
		// SQLite's default separator is MySQL's, ",".
		e.write(", ?")
		e.args = append(e.args, sep)
	}
	if n.Order != nil {
		e.write(" ORDER BY ")
		for i, item := range n.Order.Items {
			if i > 0 {
				e.write(", ")
			}
			if p, ok := item.Expr.(*ast.PositionExpr); ok && p.P == nil {
				// A position counts the GROUP_CONCAT arguments.
				if p.N < 1 || p.N > len(texts) {
					e.fail(errs.UnknownColumn(strconv.Itoa(p.N), meta.ClauseOrder))
					return nil
				}
				e.write(texts[p.N-1])
			} else if e.expr(item.Expr) == nil && e.err != nil {
				return nil
			}
			if item.Desc {
				e.write(" DESC")
			}
		}
	}
	e.write("), ?)")
	e.args = append(e.args, &Variable{Name: "group_concat_max_len", Scope: vars.ReadSession})
	return &meta.Expr{Kind: meta.ExprFunc, Op: "GROUP_CONCAT", Ret: ret, Args: nodes, Agg: true}
}

// groupBy emits GROUP BY. A name that is no table column but a select-list name is
// written as the item's position, which SQLite reads as MySQL means it.
func (e *emitter) groupBy(g *ast.GroupByClause) {
	if g.Rollup {
		e.unsupported("WITH ROLLUP", reasonLater)
		return
	}
	e.shape.Group.Grouped = true
	e.write(" GROUP BY ")
	e.clause = meta.ClauseGroup
	for i, item := range g.Items {
		if i > 0 {
			e.write(", ")
		}
		if item.Desc {
			// GROUP BY ... DESC was removed in MySQL 8.0.
			e.fail(parseErrorAt(e.query, 0))
			return
		}
		pos := 0
		if p, ok := item.Expr.(*ast.PositionExpr); ok && p.P == nil {
			pos = p.N
			if pos < 1 || pos > len(e.shape.Items) {
				e.fail(errs.UnknownColumn(strconv.Itoa(pos), meta.ClauseGroup))
				return
			}
		} else if c, ok := item.Expr.(*ast.ColumnNameExpr); ok && c.Name.Table.O == "" && !e.tableColumn(c.Name.Name.O) {
			pos = e.itemPosition(c.Name.Name.O)
		}
		if pos > 0 {
			e.write(strconv.Itoa(pos))
			e.shape.Group.By = append(e.shape.Group.By, itemExpr(e.shape.Items[pos-1]))
			continue
		}
		e.shape.Group.By = append(e.shape.Group.By, e.expr(item.Expr))
	}
	e.clause = ""
}

// having emits HAVING.
func (e *emitter) having(h *ast.HavingClause) {
	e.write(" HAVING ")
	e.clause = meta.ClauseHaving
	e.shape.Group.Having = e.expr(h.Expr)
	e.clause = ""
}

// tableColumn reports a column of some FROM table. Without a catalog every name is
// taken as a column.
func (e *emitter) tableColumn(name string) bool {
	for _, src := range e.shape.From.Sources {
		if src.Table == nil {
			return true
		}
		for _, c := range src.Table.Columns {
			if strings.EqualFold(c.Name, name) {
				return true
			}
		}
	}
	return false
}

// itemPosition is the 1-based position of the select-list item named name, 0 if none
// or after a *.
func (e *emitter) itemPosition(name string) int {
	for i, it := range e.shape.Items {
		if it.Star {
			return 0
		}
		if strings.EqualFold(it.Name, name) {
			return i + 1
		}
	}
	return 0
}

// itemExpr is a select-list item as an expression, for the grouping checks.
func itemExpr(it meta.Item) *meta.Expr {
	switch {
	case it.Expr != nil:
		return it.Expr
	case it.Literal != nil:
		return &meta.Expr{Kind: meta.ExprLiteral, Literal: it.Literal}
	case it.Var != nil:
		return &meta.Expr{Kind: meta.ExprVar, Var: it.Var}
	case it.Star:
		return nil
	}
	return &meta.Expr{Kind: meta.ExprColumn, Qualifier: it.Qualifier, Column: it.Column}
}

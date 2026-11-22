package dialect

import (
	"strconv"

	"github.com/pingcap/tidb/pkg/parser/ast"

	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

var setOperators = map[ast.SetOprType]string{
	ast.Union:     " UNION ",
	ast.UnionAll:  " UNION ALL ",
	ast.Except:    " EXCEPT ",
	ast.Intersect: " INTERSECT ",
}

// setOpr emits UNION [ALL], INTERSECT and EXCEPT. The statement's shape is the first
// SELECT's, with the others in Shape.Union. A SELECT with its own ORDER BY or LIMIT is
// read through a subquery, as SQLite allows neither in a compound member.
func (e *emitter) setOpr(n *ast.SetOprStmt) {
	if n.With != nil {
		e.unsupported("WITH", reasonLater)
		return
	}
	list := n.SelectList
	if list == nil || list.With != nil || list.OrderBy != nil || list.Limit != nil {
		e.unsupported("nested set operation", reasonLater)
		return
	}
	var first *Shape
	var others []*meta.Select
	intersect, other := false, false
	for i, node := range list.Selects {
		sel, after := setMember(node)
		if sel == nil {
			e.unsupported("nested set operation", reasonLater)
			return
		}
		if i > 0 {
			if after == nil {
				e.fail(errs.Internal())
				return
			}
			op, ok := setOperators[*after]
			if !ok {
				e.unsupported(after.String(), reasonLater)
				return
			}
			if *after == ast.Intersect {
				intersect = true
			} else {
				other = true
			}
			e.write(op)
		}
		wrap := sel.OrderBy != nil || sel.Limit != nil
		if wrap {
			e.write("SELECT * FROM (")
		}
		e.clause = ""
		e.selectStmt(sel)
		if e.err != nil {
			return
		}
		if wrap {
			e.write(")")
		}
		if i == 0 {
			first = e.shape
		} else {
			others = append(others, &e.shape.Select)
		}
	}
	if intersect && other {
		// MySQL's INTERSECT binds tighter than UNION and EXCEPT; SQLite's do not.
		e.unsupported("INTERSECT mixed with UNION or EXCEPT", reasonLater)
		return
	}
	e.shape = first
	first.Union = others
	if n.OrderBy != nil {
		e.unionOrder(n.OrderBy)
	}
	e.limit(n.Limit)
}

// setMember returns a set operation's SELECT and the operator before it. The parser
// gives a parenthesized SELECT as a one-element list carrying the operator; nil means a
// nested set operation.
func setMember(node ast.Node) (*ast.SelectStmt, *ast.SetOprType) {
	switch x := node.(type) {
	case *ast.SelectStmt:
		return x, x.AfterSetOperator
	case *ast.SetOprSelectList:
		if len(x.Selects) != 1 || x.With != nil || x.OrderBy != nil || x.Limit != nil {
			return nil, nil
		}
		if sel, ok := x.Selects[0].(*ast.SelectStmt); ok {
			return sel, x.AfterSetOperator
		}
	}
	return nil, nil
}

// unionOrder emits the ORDER BY of a set operation: positions and the first SELECT's
// column names, written as positions.
func (e *emitter) unionOrder(o *ast.OrderByClause) {
	e.write(" ORDER BY ")
	for i, item := range o.Items {
		if i > 0 {
			e.write(", ")
		}
		switch x := item.Expr.(type) {
		case *ast.PositionExpr:
			if x.P != nil {
				e.fail(parseErrorAt(e.query, 0))
				return
			}
			e.write(strconv.Itoa(x.N))
		case *ast.ColumnNameExpr:
			if x.Name.Table.O != "" {
				e.fail(errs.GlobalOrderTable(x.Name.Table.O))
				return
			}
			pos := e.itemPosition(x.Name.Name.O)
			if pos == 0 {
				if e.hasStar() {
					// The name may come from a *: SQLite finds it by name.
					e.write(quoteIdent(x.Name.Name.O))
					break
				}
				e.fail(errs.UnknownColumn(x.Name.Name.O, meta.ClauseOrder))
				return
			}
			e.write(strconv.Itoa(pos))
		default:
			e.unsupported("expression in a UNION ORDER BY", reasonLater)
			return
		}
		if item.Desc {
			e.write(" DESC")
		}
	}
}

func (e *emitter) hasStar() bool {
	for _, it := range e.shape.Items {
		if it.Star {
			return true
		}
	}
	return false
}

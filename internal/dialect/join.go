package dialect

import (
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"

	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// fromClause emits a SELECT's FROM clause (without the keyword) into its own text and
// arguments, and records its tables in the shape: the select list, emitted before it in
// the text, resolves against them.
func (e *emitter) fromClause(refs *ast.TableRefsClause) (string, []any) {
	return e.sub(func() {
		if refs == nil || refs.TableRefs == nil {
			e.fail(errs.Internal())
			return
		}
		e.joinNode(refs.TableRefs)
	})
}

// joinNode emits a table or a join (docs/05-dialect.md section 4). SQLite has the same
// INNER, LEFT, RIGHT and CROSS joins with ON and USING; NATURAL is refused.
func (e *emitter) joinNode(n ast.ResultSetNode) {
	switch n := n.(type) {
	case *ast.TableSource:
		e.tableSource(n)
	case *ast.Join:
		if n.Right == nil {
			e.joinNode(n.Left)
			return
		}
		if n.NaturalJoin {
			e.unsupported("NATURAL JOIN", reasonLater)
			return
		}
		if n.StraightJoin {
			e.apply(ruleHintStrip)
		}
		from := &e.shape.From
		leftStart := len(from.Sources)
		e.joinNode(n.Left)
		rightStart := len(from.Sources)
		switch n.Tp {
		case ast.LeftJoin:
			e.write(" LEFT JOIN ")
		case ast.RightJoin:
			e.write(" RIGHT JOIN ")
		default:
			e.write(" JOIN ")
		}
		if right, ok := n.Right.(*ast.Join); ok && right.Right != nil {
			e.write("(")
			e.joinNode(right)
			e.write(")")
		} else {
			e.joinNode(n.Right)
		}
		// The inner side of an outer join can be NULL.
		inner := from.Sources[rightStart:]
		if n.Tp == ast.RightJoin {
			inner = from.Sources[leftStart:rightStart]
		}
		if n.Tp == ast.LeftJoin || n.Tp == ast.RightJoin {
			for i := range inner {
				inner[i].Nullable = true
			}
		}
		switch {
		case n.On != nil:
			e.write(" ON ")
			e.clause = meta.ClauseOn
			on := e.expr(n.On.Expr)
			if n.Tp != ast.LeftJoin && n.Tp != ast.RightJoin && !nullable(from.Sources[leftStart:]) {
				// An outer join's ON does not make its columns dependent.
				e.shape.Group.Conds = append(e.shape.Group.Conds, on)
			}
			e.clause = ""
		case len(n.Using) > 0:
			if n.Tp == ast.RightJoin {
				// MySQL coalesces the columns to the right table's; check SQLite before
				// accepting it.
				e.unsupported("RIGHT JOIN with USING", reasonLater)
				return
			}
			names := make([]string, len(n.Using))
			for i, c := range n.Using {
				names[i] = quoteIdent(c.Name.O)
				from.Using = append(from.Using, c.Name.O)
			}
			e.write(" USING (", strings.Join(names, ", "), ")")
		}
	default:
		e.unsupported("derived table", reasonLater)
	}
}

// tableSource emits one table of a FROM clause. Every table is in the statement's
// database: one statement runs in one database file.
func (e *emitter) tableSource(ts *ast.TableSource) {
	switch ts.Source.(type) {
	case *ast.SelectStmt, *ast.SetOprStmt:
		e.derivedTable(ts)
		return
	}
	name, alias, db, ok := e.tableName(ts)
	if !ok {
		return
	}
	if e.target == "" {
		e.target = db
	} else if db != e.target {
		e.unsupported("cross-database reference", reasonM1Scope)
		return
	}
	src := meta.Source{Table: e.lookup(name), Name: name, Alias: alias}
	visible := alias
	if visible == "" {
		visible = name
	}
	if !e.uniqueTable(visible) {
		return
	}
	if src.Table == nil && alias == "" {
		// Without a catalog the name still identifies the table.
		src.Alias = name
	}
	if len(e.shape.From.Sources) == 0 {
		e.shape.Table, e.shape.Alias, e.shape.Meta = name, alias, src.Table
	}
	e.shape.From.Sources = append(e.shape.From.Sources, src)
	e.writeTable(name, alias)
}

// uniqueTable fails with 1066 when the FROM clause already has a table named visible.
func (e *emitter) uniqueTable(visible string) bool {
	for _, s := range e.shape.From.Sources {
		if s.Visible() == visible || s.Table == nil && s.Alias == visible {
			e.fail(errs.NonUniqueTable(visible))
			return false
		}
	}
	return true
}

// derivedTable emits FROM (SELECT ...) AS alias. meta builds the table from the query's
// result columns when the statement is checked.
func (e *emitter) derivedTable(ts *ast.TableSource) {
	alias := ts.AsName.O
	switch {
	case ts.Lateral:
		e.unsupported("LATERAL", reasonLater)
		return
	case len(ts.ColumnNames) > 0:
		e.unsupported("derived table column list", reasonLater)
		return
	case alias == "":
		e.fail(errs.DerivedAlias())
		return
	}
	if !e.uniqueTable(alias) {
		return
	}
	text, args, inner := e.subQuery(ts.Source, false)
	if inner == nil {
		return
	}
	src := meta.Source{Alias: alias, Derived: &meta.Derived{Select: inner.Select, Union: inner.Union}}
	e.shape.From.Sources = append(e.shape.From.Sources, src)
	e.write("(", text, ") AS ", quoteIdent(alias))
	e.args = append(e.args, args...)
}

// nullable reports a source on the inner side of an outer join.
func nullable(sources []meta.Source) bool {
	for _, s := range sources {
		if s.Nullable {
			return true
		}
	}
	return false
}

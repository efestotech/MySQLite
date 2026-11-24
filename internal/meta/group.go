package meta

import (
	"strings"

	"mysqlite/internal/errs"
)

// Group describes the grouping of a SELECT for CheckGroup.
type Group struct {
	// Grouped: the statement has GROUP BY. Aggregated: an aggregate appears in the
	// select list, HAVING or ORDER BY.
	Grouped, Aggregated bool
	// By are the GROUP BY expressions; a select-list position or alias is the item's
	// expression.
	By []*Expr
	// Having is the HAVING condition, or nil.
	Having *Expr
	// Order are the ORDER BY expressions; nil for a position or a select-list name.
	Order []*Expr
	// Full is ONLY_FULL_GROUP_BY.
	Full bool
	// Conds are WHERE and the ON conditions of inner joins: the equalities among their
	// top-level AND terms make columns functionally dependent.
	Conds []*Expr
}

// CheckGroup checks the columns of a grouped or aggregated SELECT, after CheckRefs.
// HAVING may name a select-list item, a GROUP BY column or anything inside an aggregate
// (1054 otherwise). Under ONLY_FULL_GROUP_BY, every other column of the select list and
// ORDER BY must be grouped or depend on grouped columns (1055; 1140 without GROUP BY):
// the GROUP BY columns, a column equal to a constant or to a dependent column in WHERE
// or an inner join's ON, and every column of a table whose primary key or unique NOT
// NULL key is dependent (MySQL 8.0.46, oracle probes o.58.1-o.58.4). Equalities in an
// outer join's ON are not used: such queries get 1055 here.
func CheckGroup(items []Item, g Group, from From) *errs.Error {
	return newScope(from).checkGroup(items, g)
}

func (s scope) checkGroup(items []Item, g Group) *errs.Error {
	if !g.Grouped && !g.Aggregated {
		return nil
	}
	from := s.from
	c := groupCheck{s: s, g: g}
	if g.Full {
		c.det = c.determined()
	}
	if g.Having != nil {
		if col := c.firstColumn(g.Having, func(x *Expr) bool {
			return x.Qualifier == "" && selectAlias(items, x.Column) || c.grouped(x)
		}); col != nil {
			name := col.Column
			if col.Qualifier != "" {
				name = col.Qualifier + "." + col.Column
			}
			return errs.UnknownColumn(name, ClauseHaving)
		}
	}
	if !g.Full {
		return nil
	}
	n := 0
	for _, it := range items {
		var exprs []*Expr
		switch {
		case it.Expr != nil:
			exprs = []*Expr{it.Expr}
		case it.Star:
			for _, src := range from.Sources {
				if src.Table == nil || it.Qualifier != "" && it.Qualifier != src.Visible() {
					continue
				}
				for _, col := range src.Table.Columns {
					exprs = append(exprs, &Expr{Kind: ExprColumn, Qualifier: src.Visible(), Column: col.Name})
				}
			}
		case it.Literal == nil && it.Var == nil:
			exprs = []*Expr{{Kind: ExprColumn, Qualifier: it.Qualifier, Column: it.Column}}
		default:
			n++
			continue
		}
		for _, x := range exprs {
			n++
			if col := c.firstColumn(x, c.dependent); col != nil {
				if !g.Grouped {
					return errs.MixedAggregate(n, c.fullName(col))
				}
				return errs.NotInGroupBy(n, "SELECT list", c.fullName(col))
			}
		}
	}
	if g.Grouped {
		for i, x := range g.Order {
			if x == nil {
				continue
			}
			if col := c.firstColumn(x, c.dependent); col != nil {
				return errs.NotInGroupBy(i+1, "ORDER BY clause", c.fullName(col))
			}
		}
	}
	return nil
}

type groupCheck struct {
	s scope
	g Group
	// det are the functionally dependent columns, by colKey.
	det map[string]bool
}

// colKey names a column of a FROM source; ok is false for a column that does not
// resolve here. An enclosing query's column is a constant (constant true).
func (c groupCheck) colKey(x *Expr) (key string, constant, ok bool) {
	src, col, depth, e := c.s.resolveDepth(x.Qualifier, x.Column, "")
	if e != nil {
		return "", false, false
	}
	if depth > 0 {
		return "", true, true
	}
	return depKey(src, col.Name), false, true
}

// depKey is a column's key in the dependent set.
func depKey(src Source, column string) string {
	return src.Visible() + "\x00" + strings.ToLower(column)
}

// determined computes the dependent columns: GROUP BY columns, then, to a fixed point,
// equalities with constants or dependent columns, and the columns of a table whose
// primary key or unique NOT NULL key is dependent.
func (c groupCheck) determined() map[string]bool {
	det := map[string]bool{}
	for _, b := range c.g.By {
		if b != nil && b.Kind == ExprColumn {
			if k, _, ok := c.colKey(b); ok && k != "" {
				det[k] = true
			}
		}
	}
	var eqs [][2]*Expr
	var terms func(x *Expr)
	terms = func(x *Expr) {
		if x == nil || x.Kind != ExprBool {
			return
		}
		switch x.Op {
		case "AND":
			for _, a := range x.Args {
				terms(a)
			}
		case "=":
			if len(x.Args) == 2 && x.Args[0] != nil && x.Args[1] != nil {
				eqs = append(eqs, [2]*Expr{x.Args[0], x.Args[1]})
			}
		}
	}
	for _, cond := range c.g.Conds {
		terms(cond)
	}
	// side is a column's key, or "" with constant set for a literal, a variable or an
	// enclosing query's column.
	side := func(x *Expr) (string, bool) {
		switch x.Kind {
		case ExprLiteral, ExprVar:
			return "", true
		case ExprColumn:
			if k, constant, ok := c.colKey(x); ok {
				return k, constant
			}
		}
		return "", false
	}
	for changed := true; changed; {
		changed = false
		mark := func(k string) {
			if k != "" && !det[k] {
				det[k], changed = true, true
			}
		}
		for _, eq := range eqs {
			lk, lc := side(eq[0])
			rk, rc := side(eq[1])
			if lc || det[lk] && lk != "" {
				mark(rk)
			}
			if rc || det[rk] && rk != "" {
				mark(lk)
			}
		}
		for _, src := range c.s.from.Sources {
			if src.Table == nil || !c.keyDetermined(src, det) {
				continue
			}
			for _, col := range src.Table.Columns {
				mark(depKey(src, col.Name))
			}
		}
	}
	return det
}

// keyDetermined reports a source whose primary key or a unique key on NOT NULL columns
// has only dependent columns.
func (c groupCheck) keyDetermined(src Source, det map[string]bool) bool {
	all := func(cols []string) bool {
		for _, k := range cols {
			if !det[depKey(src, k)] {
				return false
			}
		}
		return true
	}
	t := src.Table
	if len(t.PrimaryKey) > 0 && all(t.PrimaryKey) {
		return true
	}
	for _, ix := range t.Indexes {
		if !ix.Unique || len(ix.Columns) == 0 || !all(ix.Columns) {
			continue
		}
		notNull := true
		for _, k := range ix.Columns {
			col, ok := t.column(k)
			notNull = notNull && ok && col.NotNull
		}
		if notNull {
			return true
		}
	}
	return false
}

// firstColumn returns the first column of x outside aggregates and outside the GROUP BY
// expressions that ok rejects, or nil.
func (c groupCheck) firstColumn(x *Expr, ok func(*Expr) bool) *Expr {
	if x == nil || x.Agg || c.groupExpr(x) {
		return nil
	}
	if x.Kind == ExprColumn {
		if ok(x) {
			return nil
		}
		return x
	}
	for _, a := range x.Args {
		if col := c.firstColumn(a, ok); col != nil {
			return col
		}
	}
	return nil
}

// groupExpr reports x equal to a GROUP BY expression.
func (c groupCheck) groupExpr(x *Expr) bool {
	for _, b := range c.g.By {
		if c.equal(x, b) {
			return true
		}
	}
	return false
}

// grouped reports a column named in GROUP BY.
func (c groupCheck) grouped(col *Expr) bool {
	for _, b := range c.g.By {
		if b != nil && b.Kind == ExprColumn && c.equal(col, b) {
			return true
		}
	}
	return false
}

// dependent reports a column grouped or functionally dependent on GROUP BY.
func (c groupCheck) dependent(col *Expr) bool {
	if c.grouped(col) {
		return true
	}
	k, constant, ok := c.colKey(col)
	if !ok || constant {
		// Unresolved columns are CheckRefs' business; an enclosing query's column is a
		// constant here.
		return true
	}
	return c.det[k]
}

// equal compares two expressions structurally; columns compare by the table and column
// they resolve to.
func (c groupCheck) equal(a, b *Expr) bool {
	if a == nil || b == nil || a.Kind != b.Kind || a.Op != b.Op || a.Agg != b.Agg || a.Sub != b.Sub || len(a.Args) != len(b.Args) {
		return false
	}
	switch a.Kind {
	case ExprColumn:
		sa, ca, ea := c.s.resolve(a.Qualifier, a.Column, "")
		sb, cb, eb := c.s.resolve(b.Qualifier, b.Column, "")
		return ea == nil && eb == nil && sa.Visible() == sb.Visible() && strings.EqualFold(ca.Name, cb.Name)
	case ExprLiteral:
		return a.Literal.Kind == b.Literal.Kind && a.Literal.Text == b.Literal.Text
	case ExprVar:
		return strings.EqualFold(a.Var.Name, b.Var.Name) && a.Var.Scope == b.Var.Scope
	}
	for i := range a.Args {
		if !c.equal(a.Args[i], b.Args[i]) {
			return false
		}
	}
	return true
}

// fullName is database.table.column, as MySQL names a column in 1055 and 1140.
func (c groupCheck) fullName(col *Expr) string {
	src, cc, e := c.s.resolve(col.Qualifier, col.Column, "")
	if e != nil {
		return col.Column
	}
	return src.Table.Database + "." + src.Table.Name + "." + cc.Name
}

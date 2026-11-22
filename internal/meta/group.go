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
}

// CheckGroup checks the columns of a grouped or aggregated SELECT, after CheckRefs.
// HAVING may name a select-list item, a GROUP BY column or anything inside an aggregate
// (1054 otherwise). Under ONLY_FULL_GROUP_BY, every other column of the select list and
// ORDER BY must be grouped or depend on grouped columns: the GROUP BY columns
// themselves, and every column of a table whose whole primary key is grouped (1055;
// 1140 without GROUP BY). MySQL also derives dependencies from equalities in WHERE and
// ON, which this does not: such queries get 1055 here. `oracle: pending`.
func CheckGroup(items []Item, g Group, from From) *errs.Error {
	return newScope(from).checkGroup(items, g)
}

func (s scope) checkGroup(items []Item, g Group) *errs.Error {
	if !g.Grouped && !g.Aggregated {
		return nil
	}
	from := s.from
	c := groupCheck{s: s, g: g}
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

// dependent reports a column grouped, or of a table whose primary key is grouped.
func (c groupCheck) dependent(col *Expr) bool {
	if c.grouped(col) {
		return true
	}
	src, _, depth, e := c.s.resolveDepth(col.Qualifier, col.Column, "")
	if e != nil || depth > 0 {
		// Unresolved columns are CheckRefs' business; an enclosing query's column is a
		// constant here.
		return true
	}
	if len(src.Table.PrimaryKey) == 0 {
		return false
	}
	for _, k := range src.Table.PrimaryKey {
		if !c.grouped(&Expr{Kind: ExprColumn, Qualifier: src.Visible(), Column: k}) {
			return false
		}
	}
	return true
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

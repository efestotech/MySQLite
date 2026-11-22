package meta

import "mysqlite/internal/errs"

// Select is one SELECT as the dialect describes it, checked once the catalog is read.
type Select struct {
	Items []Item
	// Refs are the column references outside the select list, in clause order.
	Refs []Ref
	// Exprs are the nodes CheckExprs types: arithmetic and subqueries, every clause.
	Exprs []*Expr
	// From is the FROM clause; a source's Table is nil without a catalog.
	From From
	// Group is the grouping of the statement.
	Group Group
}

// CheckSelect describes the result columns of a SELECT and runs every check on it, in
// MySQL's order: the select list, the other clauses' references, expression types,
// grouping.
func CheckSelect(sel *Select) ([]ColumnDef, *errs.Error) {
	return newScope(sel.From).checkSelect(sel)
}

func (s scope) checkSelect(sel *Select) ([]ColumnDef, *errs.Error) {
	s, e := s.derive()
	if e != nil {
		return nil, e
	}
	defs, e := s.columns(sel.Items)
	if e != nil {
		return nil, e
	}
	if e := s.checkRefs(sel.Refs, sel.Items); e != nil {
		return nil, e
	}
	if e := s.checkExprs(sel.Exprs); e != nil {
		return nil, e
	}
	if e := s.checkGroup(sel.Items, sel.Group); e != nil {
		return nil, e
	}
	return defs, nil
}

// subquery checks a subquery whose enclosing query is s, and returns its columns.
func (s scope) subquery(sel *Select) ([]ColumnDef, *errs.Error) {
	outer := s
	return scope{from: sel.From, outer: &outer}.checkSelect(sel)
}

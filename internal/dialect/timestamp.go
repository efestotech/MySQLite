package dialect

import (
	"github.com/pingcap/tidb/pkg/parser/ast"

	"mysqlite/internal/dialect/functions"
	"mysqlite/internal/meta"
)

// TIMESTAMP columns hold UTC (ADR 0007): a read converts them to the session zone, a
// write converts the value to UTC. The zone is bound from the session.

// timestampRef reports whether a column reference names a TIMESTAMP column of a catalog
// table, in this query or an enclosing one.
func (e *emitter) timestampRef(qualifier, column string) bool {
	try := func(from meta.From) (bool, bool) {
		d, err := meta.Infer(&meta.Expr{Kind: meta.ExprColumn, Qualifier: qualifier, Column: column}, from)
		if err != nil || d.OrgTable == "" {
			return false, false
		}
		return d.Type == meta.TypeTimestamp, true
	}
	if ts, found := try(e.scope()); found {
		return ts
	}
	// The nearest enclosing query first.
	for i := len(e.outer) - 1; i >= 0; i-- {
		if ts, found := try(e.outer[i]); found {
			return ts
		}
	}
	return false
}

// fromUTC wraps the text emit writes in the conversion of a stored TIMESTAMP.
func (e *emitter) fromUTC(emit func()) {
	e.write(functions.UDFTzFromUTC, "(")
	emit()
	e.write(", ?)")
	e.args = append(e.args, &SessionValue{Name: functions.SessionTimeZone})
}

// toUTC emits a value written to a TIMESTAMP column, converted for storage.
func (e *emitter) toUTC(x ast.ExprNode) *meta.Expr {
	e.write(functions.UDFTzToUTC, "(")
	node := e.expr(x)
	e.write(", ?)")
	e.args = append(e.args, &SessionValue{Name: functions.SessionTimeZone})
	return node
}

// writeValue emits a value assigned to column of the written table.
func (e *emitter) writeValue(column string, x ast.ExprNode) *meta.Expr {
	if _, isDefault := x.(*ast.DefaultExpr); !isDefault && e.wr != nil && e.wr.Meta != nil && e.wr.Meta.IsTimestamp(column) {
		return e.toUTC(x)
	}
	return e.expr(x)
}

// starNeedsList reports whether a * must be written as a column list: one of its
// tables has a TIMESTAMP column to convert.
func (e *emitter) starNeedsList(qualifier string) bool {
	for _, src := range e.shape.From.Sources {
		if src.Table == nil || qualifier != "" && src.Visible() != qualifier {
			continue
		}
		for _, c := range src.Table.Columns {
			if src.Table.IsTimestamp(c.Name) {
				return true
			}
		}
	}
	return false
}

// starList writes * (or q.*) as its columns, TIMESTAMPs converted; a table without
// catalog columns keeps its own *.
func (e *emitter) starList(qualifier string) {
	first := true
	for _, src := range e.shape.From.Sources {
		vis := src.Visible()
		if qualifier != "" && vis != qualifier {
			continue
		}
		if !first {
			e.write(", ")
		}
		first = false
		if src.Table == nil {
			e.write(quoteIdent(vis), ".*")
			continue
		}
		for i, c := range src.Table.Columns {
			if i > 0 {
				e.write(", ")
			}
			ref := quoteIdent(vis) + "." + quoteIdent(c.Name)
			if src.Table.IsTimestamp(c.Name) {
				e.fromUTC(func() { e.write(ref) })
				e.write(" AS ", quoteIdent(c.Name))
				continue
			}
			e.write(ref)
		}
	}
}

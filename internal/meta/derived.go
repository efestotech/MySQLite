package meta

import (
	"strings"

	"mysqlite/internal/errs"
)

// Derived is the query of a derived table, FROM (SELECT ...) AS alias: a SELECT and,
// for a set operation, its other SELECTs.
type Derived struct {
	Select Select
	Union  []*Select
}

// derive gives every derived table of the FROM clause a table built from its result
// columns. A derived table sees the enclosing queries (MySQL 8.0.14 and later) but not
// the other tables of its own FROM clause.
func (s scope) derive() (scope, *errs.Error) {
	var srcs []Source
	for i, src := range s.from.Sources {
		if src.Derived == nil || src.Table != nil {
			continue
		}
		if srcs == nil {
			srcs = append([]Source(nil), s.from.Sources...)
		}
		defs, e := checkUnion(s.outer, &src.Derived.Select, src.Derived.Union)
		if e != nil {
			return s, e
		}
		t := &Table{Name: src.Alias}
		for j, d := range defs {
			for _, prev := range defs[:j] {
				if strings.EqualFold(prev.Name, d.Name) {
					return s, errs.DuplicateColumn(d.Name)
				}
			}
			def := d
			t.Columns = append(t.Columns, Column{Name: d.Name, NotNull: d.Flags&FlagNotNull != 0, Def: &def})
		}
		srcs[i].Table = t
	}
	if srcs != nil {
		s.from.Sources = srcs
	}
	return s, nil
}

// derivedColumn describes a derived table's column: its result column, belonging to
// the derived table. `oracle: pending` for the schema and original names.
func derivedColumn(t *Table, c Column, visible, name string) ColumnDef {
	d := *c.Def
	d.Schema, d.Table, d.OrgTable, d.Name, d.OrgName = "", visible, t.Name, name, c.Name
	d.Flags &^= FlagPriKey | FlagUniqueKey | FlagMultipleKey | FlagPartKey | FlagAutoIncrement
	return d
}

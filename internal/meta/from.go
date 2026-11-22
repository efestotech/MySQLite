package meta

import (
	"strings"

	"mysqlite/internal/errs"
)

// Source is one table of a FROM clause.
type Source struct {
	// Table is nil when the statement was translated without a catalog.
	Table *Table
	Alias string
	// Nullable marks the inner side of an outer join: its columns can be NULL.
	Nullable bool
}

// Visible is the name the statement refers to the table by: its alias, else its name.
func (s Source) Visible() string {
	if s.Alias != "" || s.Table == nil {
		return s.Alias
	}
	return s.Table.Name
}

// From is a SELECT's FROM clause: its tables in order, and the column names joined with
// USING, which an unqualified reference may name without ambiguity.
type From struct {
	Sources []Source
	Using   []string
}

// Single is the FROM clause of a single-table statement; nil t is no FROM.
func Single(t *Table, alias string) From {
	if t == nil {
		return From{}
	}
	return From{Sources: []Source{{Table: t, Alias: alias}}}
}

// scope resolves the column references of an expression against a FROM clause.
type scope struct {
	from From
}

func newScope(from From) scope {
	return scope{from: from}
}

// resolve finds the table of a column reference. A name in several tables is ambiguous
// (1052) unless it was joined with USING, which takes the first table's column.
func (s scope) resolve(qualifier, column, clause string) (Source, Column, *errs.Error) {
	name := column
	if qualifier != "" {
		name = qualifier + "." + column
	}
	var found Source
	var col Column
	n := 0
	for _, src := range s.from.Sources {
		if src.Table == nil || qualifier != "" && qualifier != src.Visible() {
			continue
		}
		c, ok := src.Table.column(column)
		if !ok {
			continue
		}
		if n == 0 {
			found, col = src, c
		}
		n++
	}
	switch {
	case n == 0:
		return Source{}, Column{}, errs.UnknownColumn(name, clause)
	case n > 1 && (qualifier != "" || !s.using(column)):
		return Source{}, Column{}, errs.AmbiguousColumn(name, clause)
	}
	return found, col, nil
}

func (s scope) using(column string) bool {
	for _, u := range s.from.Using {
		if strings.EqualFold(u, column) {
			return true
		}
	}
	return false
}

// column describes a table column as a result column named name.
func (src Source) column(c Column, name string) ColumnDef {
	d := tableColumn(src.Table, c, src.Visible(), name)
	if src.Nullable {
		d.Flags &^= FlagNotNull
	}
	return d
}

// source finds the table a qualified star names; 1051 if none.
func (s scope) source(qualifier string) (Source, *errs.Error) {
	for _, src := range s.from.Sources {
		if src.Table != nil && src.Visible() == qualifier {
			return src, nil
		}
	}
	return Source{}, errs.UnknownTable(qualifier)
}

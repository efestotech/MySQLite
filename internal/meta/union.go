package meta

import "mysqlite/internal/errs"

// CheckUnion checks the SELECTs of a UNION, INTERSECT or EXCEPT and describes its result
// columns: named as the first SELECT's, typed by merging each position over every
// SELECT, nullable when any SELECT's column is, and belonging to no table.
func CheckUnion(first *Select, others []*Select) ([]ColumnDef, *errs.Error) {
	return checkUnion(nil, first, others)
}

// checkUnion is CheckUnion for SELECTs whose enclosing query is outer (nil for none);
// without others it is the first SELECT's columns.
func checkUnion(outer *scope, first *Select, others []*Select) ([]ColumnDef, *errs.Error) {
	check := func(sel *Select) ([]ColumnDef, *errs.Error) {
		return scope{from: sel.From, outer: outer}.checkSelect(sel)
	}
	defs, e := check(first)
	if e != nil || len(others) == 0 {
		return defs, e
	}
	if e != nil {
		return nil, e
	}
	all := [][]ColumnDef{defs}
	for _, sel := range others {
		d, e := check(sel)
		if e != nil {
			return nil, e
		}
		if len(d) != len(defs) {
			return nil, errs.UnionColumns()
		}
		all = append(all, d)
	}
	out := make([]ColumnDef, len(defs))
	for i := range defs {
		col := make([]ColumnDef, len(all))
		for j := range all {
			col[j] = all[j][i]
		}
		out[i] = unionColumn(col)
		out[i].Name = defs[i].Name
	}
	return out, nil
}

// unionColumn merges one result position. Columns of one type keep it (INT stays INT,
// VARCHAR the longest); others follow merge. `oracle: pending`.
func unionColumn(col []ColumnDef) ColumnDef {
	notNull, unsigned := true, true
	var typed []ColumnDef
	for _, d := range col {
		notNull = notNull && d.Flags&FlagNotNull != 0
		unsigned = unsigned && d.Flags&FlagUnsigned != 0
		if classOf(d) != classNull {
			typed = append(typed, d)
		}
	}
	same := len(typed) > 0
	for _, d := range typed {
		same = same && d.Type == typed[0].Type && d.Charset == typed[0].Charset && classOf(d) != classDecimal
	}
	var d ColumnDef
	if same {
		d = typed[0]
		for _, t := range typed {
			d.Length = max(d.Length, t.Length)
			d.Decimals = max(d.Decimals, t.Decimals)
		}
	} else {
		d = merge(col)
	}
	d.Schema, d.Table, d.OrgTable, d.OrgName = "", "", "", ""
	d.Flags &^= FlagNotNull | FlagUnsigned | FlagPriKey | FlagUniqueKey | FlagMultipleKey | FlagPartKey | FlagAutoIncrement | FlagNoDefault
	if notNull {
		d.Flags |= FlagNotNull
	}
	if unsigned && same {
		d.Flags |= FlagUnsigned
	}
	return d
}

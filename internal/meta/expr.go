package meta

import (
	"mysqlite/internal/errs"
)

// ExprKind is the kind of a typed expression node.
type ExprKind int

const (
	// ExprColumn is a column reference (Qualifier, Column).
	ExprColumn ExprKind = iota + 1
	// ExprLiteral is a constant (Literal).
	ExprLiteral
	// ExprVar is a system variable (Var).
	ExprVar
	// ExprBool is a comparison, a logical operator, IS [NOT] NULL, IN or BETWEEN: an
	// integer 0, 1 or NULL.
	ExprBool
	// ExprArith is binary + - * (Op), on Args[0] and Args[1].
	ExprArith
	// ExprNeg is unary minus on Args[0].
	ExprNeg
)

// Expr is the typed shape of an expression, built by the dialect next to the SQLite
// text. Its column types are known only once the catalog is read, so meta resolves it:
// result metadata (docs/04-session-catalog.md section 6) and the checks that refuse
// arithmetic SQLite would compute differently.
type Expr struct {
	Kind              ExprKind
	Op                string
	Qualifier, Column string
	Literal           *Literal
	Var               *Variable
	Args              []*Expr
	// NotNull is set by the dialect for a node that is never NULL (IS NULL).
	NotNull bool
}

// Vars returns the system variables of x, for the executor to read before Columns.
func (x *Expr) Vars() []*Variable {
	if x == nil {
		return nil
	}
	var out []*Variable
	if x.Var != nil {
		out = append(out, x.Var)
	}
	for _, a := range x.Args {
		out = append(out, a.Vars()...)
	}
	return out
}

// scope resolves the column references of an expression against the one table of a
// single-table statement.
type scope struct {
	t       *Table
	visible string
}

func newScope(t *Table, alias string) scope {
	s := scope{t: t}
	if t != nil {
		s.visible = t.Name
		if alias != "" {
			s.visible = alias
		}
	}
	return s
}

func (s scope) lookup(qualifier, column string) (Column, bool) {
	if s.t == nil || (qualifier != "" && qualifier != s.visible) {
		return Column{}, false
	}
	return s.t.column(column)
}

// CheckExprs runs the arithmetic checks on expressions outside the select list (WHERE,
// ORDER BY, SET). Unknown columns are left to CheckRefs, which names their clause.
func CheckExprs(exprs []*Expr, t *Table, alias string) *errs.Error {
	s := newScope(t, alias)
	for _, x := range exprs {
		if _, e := s.infer(x, ""); e != nil {
			return e
		}
	}
	return nil
}

// exprColumn describes a computed select-list item named name.
func (s scope) exprColumn(x *Expr, name string) (ColumnDef, *errs.Error) {
	d, e := s.infer(x, ClauseField)
	if e != nil {
		return ColumnDef{}, e
	}
	// A computed column belongs to no table.
	d.Schema, d.Table, d.OrgTable, d.OrgName = "", "", "", ""
	d.Name = name
	d.Flags &^= FlagPriKey | FlagUniqueKey | FlagMultipleKey | FlagPartKey | FlagAutoIncrement | FlagNoDefault
	return d, nil
}

// ClauseField is the clause error 1054 names for the select list.
const ClauseField = "field list"

// infer types x. clause names the clause of an unknown column (1054); "" skips the
// check (CheckRefs reports it).
func (s scope) infer(x *Expr, clause string) (ColumnDef, *errs.Error) {
	if x == nil {
		return ColumnDef{}, errs.Internal()
	}
	switch x.Kind {
	case ExprColumn:
		c, ok := s.lookup(x.Qualifier, x.Column)
		if !ok {
			if clause == "" {
				return ColumnDef{Type: TypeVarString, Charset: CollationDefault}, nil
			}
			name := x.Column
			if x.Qualifier != "" {
				name = x.Qualifier + "." + x.Column
			}
			return ColumnDef{}, errs.UnknownColumn(name, clause)
		}
		return tableColumn(s.t, c, s.visible, c.Name), nil
	case ExprLiteral:
		return literalColumn(*x.Literal, ""), nil
	case ExprVar:
		if x.Var.Def == nil {
			if clause == "" {
				// Outside the select list the value is bound; only its type is missing.
				return ColumnDef{Type: TypeVarString, Charset: CollationDefault}, nil
			}
			return ColumnDef{}, errs.UnknownSystemVariable(x.Var.Name)
		}
		return variableColumn(*x.Var, ""), nil
	case ExprBool:
		notNull := true
		for _, a := range x.Args {
			d, e := s.infer(a, clause)
			if e != nil {
				return ColumnDef{}, e
			}
			notNull = notNull && d.Flags&FlagNotNull != 0
		}
		d := ColumnDef{Type: TypeLongLong, Charset: CharsetBinary, Length: 1, Flags: FlagBinary | FlagNum}
		if notNull || x.NotNull {
			d.Flags |= FlagNotNull
		}
		return d, nil
	case ExprNeg:
		a, e := s.infer(x.Args[0], clause)
		if e != nil {
			return ColumnDef{}, e
		}
		return negate(a)
	case ExprArith:
		l, e := s.infer(x.Args[0], clause)
		if e != nil {
			return ColumnDef{}, e
		}
		r, e := s.infer(x.Args[1], clause)
		if e != nil {
			return ColumnDef{}, e
		}
		return arith(x.Op, l, r)
	}
	return ColumnDef{}, errs.Internal()
}

// class is how MySQL treats a value in arithmetic.
type class int

const (
	classInt class = iota + 1
	classDecimal
	classReal // FLOAT, DOUBLE, and strings, which MySQL converts to DOUBLE
	classTemporal
	classNull
)

func classOf(d ColumnDef) class {
	switch d.Type {
	case TypeTiny, TypeShort, TypeInt24, TypeLong, TypeLongLong, TypeYear, TypeBit:
		return classInt
	case TypeNewDecimal, TypeDecimal:
		return classDecimal
	case TypeDate, TypeDatetime, TypeTimestamp, TypeTime:
		return classTemporal
	case TypeNull:
		return classNull
	}
	return classReal
}

// Arithmetic result shapes. Lengths follow MySQL's Item_num_op loosely and are
// `oracle: pending`.
const (
	maxIntLength     = 21
	doubleLength     = 22
	maxDecimalDigits = 65
	maxDecimalScale  = 30
)

func numeric(t uint8) ColumnDef {
	return ColumnDef{Type: t, Charset: CharsetBinary, Flags: FlagBinary | FlagNum}
}

func refuseTemporal() *errs.Error {
	return errs.Unsupported("arithmetic on a temporal value", "MySQL converts dates and times to numbers such as 20240102, SQLite to their year")
}

func negate(a ColumnDef) (ColumnDef, *errs.Error) {
	var d ColumnDef
	switch classOf(a) {
	case classTemporal:
		return ColumnDef{}, refuseTemporal()
	case classInt:
		d = numeric(TypeLongLong)
		d.Length = min(a.Length+1, maxIntLength)
	case classDecimal:
		p, sc := decimalOf(a)
		d = numeric(TypeNewDecimal)
		d.Decimals = uint8(sc)
		d.Length = uint32(decimalLength(p, sc, false))
	default:
		d = numeric(TypeDouble)
		d.Length, d.Decimals = doubleLength, notFixedDecimals
	}
	d.Flags |= a.Flags & FlagNotNull
	return d, nil
}

func arith(op string, l, r ColumnDef) (ColumnDef, *errs.Error) {
	lc, rc := classOf(l), classOf(r)
	if lc == classTemporal || rc == classTemporal {
		return ColumnDef{}, refuseTemporal()
	}
	unsigned := l.Flags&FlagUnsigned != 0 || r.Flags&FlagUnsigned != 0
	if op == "-" && unsigned {
		return ColumnDef{}, errs.Unsupported("subtraction with an UNSIGNED operand", "MySQL raises 1690 when the result is negative, SQLite returns it")
	}
	var d ColumnDef
	switch {
	case lc == classReal || rc == classReal || lc == classNull || rc == classNull:
		d = numeric(TypeDouble)
		d.Length, d.Decimals = doubleLength, notFixedDecimals
		unsigned = false
	case lc == classDecimal || rc == classDecimal:
		lp, ls := decimalOf(l)
		rp, rs := decimalOf(r)
		var p, sc int
		if op == "*" {
			p, sc = lp+rp, ls+rs
		} else {
			sc = max(ls, rs)
			p = max(lp-ls, rp-rs) + 1 + sc
		}
		sc = min(sc, maxDecimalScale)
		p = min(p, maxDecimalDigits)
		d = numeric(TypeNewDecimal)
		d.Decimals = uint8(sc)
		d.Length = uint32(decimalLength(p, sc, unsigned))
	default:
		d = numeric(TypeLongLong)
		if op == "*" {
			d.Length = l.Length + r.Length
		} else {
			d.Length = max(l.Length, r.Length) + 1
		}
		d.Length = min(d.Length, maxIntLength)
	}
	if unsigned {
		d.Flags |= FlagUnsigned
	}
	if l.Flags&FlagNotNull != 0 && r.Flags&FlagNotNull != 0 {
		d.Flags |= FlagNotNull
	}
	return d, nil
}

// decimalOf returns the precision and scale of a numeric operand; an integer is a
// decimal with no scale.
func decimalOf(d ColumnDef) (precision, scale int) {
	if classOf(d) != classDecimal {
		n := int(d.Length)
		if d.Flags&FlagUnsigned == 0 && n > 1 {
			n-- // the sign
		}
		return max(n, 1), 0
	}
	scale = int(d.Decimals)
	precision = int(d.Length)
	if scale > 0 {
		precision--
	}
	if d.Flags&FlagUnsigned == 0 {
		precision--
	}
	return max(precision, 1), scale
}

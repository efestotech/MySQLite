package meta

import (
	"strconv"

	"mysqlite/internal/errs"
)

// IsExact reports whether MySQL treats d as an exact number: an integer or a DECIMAL.
// ROUND and TRUNCATE round those on their decimal digits, others as a DOUBLE.
func IsExact(d ColumnDef) bool {
	c := classOf(d)
	return c == classInt || c == classDecimal
}

// IsDecimal reports whether d is a DECIMAL, which mysqlite computes as REAL (ADR 0006).
func IsDecimal(d ColumnDef) bool {
	return classOf(d) == classDecimal
}

// roundResult is the type of ROUND(x[, d]) and TRUNCATE(x, d): an integer stays an
// integer, a DECIMAL gets d decimals (its own when d is not a constant), anything else
// is DOUBLE. `oracle: pending` for lengths.
func roundResult(x *Expr, args []ColumnDef) (ColumnDef, *errs.Error) {
	places, known := 0, true
	if len(x.Args) > 1 {
		places, known = constInt(x.Args[1])
	}
	a := args[0]
	switch classOf(a) {
	case classTemporal:
		return ColumnDef{}, refuseTemporal()
	case classInt:
		d := numeric(TypeLongLong)
		d.Length = max(a.Length, 1)
		d.Flags |= a.Flags & FlagUnsigned
		return d, nil
	case classDecimal:
		p, sc := decimalOf(a)
		scale := sc
		if known {
			scale = min(max(places, 0), maxDecimalScale)
		}
		precision := max(p-sc+1+scale, 1)
		d := numeric(TypeNewDecimal)
		d.Decimals = uint8(scale)
		d.Length = uint32(decimalLength(precision, scale, a.Flags&FlagUnsigned != 0))
		d.Flags |= a.Flags & FlagUnsigned
		return d, nil
	}
	return double(), nil
}

// constInt is the value of an integer literal, possibly negated.
func constInt(x *Expr) (int, bool) {
	neg := false
	if x.Kind == ExprNeg && len(x.Args) == 1 {
		neg, x = true, x.Args[0]
	}
	if x.Kind != ExprLiteral || x.Literal == nil || x.Literal.Kind != LiteralInt {
		return 0, false
	}
	n, err := strconv.Atoi(x.Literal.Text)
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

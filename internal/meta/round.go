package meta

import (
	"strconv"
	"strings"

	"mysqlite/internal/errs"
)

// IsExact reports whether MySQL treats d as an exact number: an integer or a DECIMAL.
// ROUND and TRUNCATE round those on their decimal digits, others as a DOUBLE.
func IsExact(d ColumnDef) bool {
	c := classOf(d)
	return c == classInt || c == classDecimal
}

// IsNumber reports an integer, DECIMAL, FLOAT or DOUBLE.
func IsNumber(d ColumnDef) bool {
	switch classOf(d) {
	case classInt, classDecimal:
		return true
	}
	return d.Type == TypeDouble || d.Type == TypeFloat
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

// temporalResult is the type of a function returning DATETIME, DATE or TIME, with the
// precision its first argument gives.
func temporalResult(x *Expr, ret FuncRet) (ColumnDef, *errs.Error) {
	fsp := 0
	if len(x.Args) > 0 && x.Kind == ExprFunc && x.Op != "" && x.Ret == ret && isClock(x.Op) {
		fsp, _ = constInt(x.Args[0])
	}
	d, e := temporalDef(ret, fsp)
	if e == nil && x.NotNull {
		d.Flags |= FlagNotNull
	}
	return d, e
}

// temporalDef is a nullable DATETIME, DATE or TIME with fsp fractional digits.
func temporalDef(ret FuncRet, fsp int) (ColumnDef, *errs.Error) {
	decl := map[FuncRet]string{RetDatetime: "DATETIME", RetDate: "DATE", RetTime: "TIME"}[ret]
	if ret != RetDate && fsp > 0 {
		decl += "(" + strconv.Itoa(fsp) + ")"
	}
	ti, ok := ParseType(decl)
	if !ok {
		return ColumnDef{}, errs.Internal()
	}
	return ColumnDef{Type: ti.Type, Charset: ti.Charset, Length: ti.Length, Flags: ti.Flags, Decimals: ti.Decimals}, nil
}

// dateAddResult types DATE_ADD(d, n, unit) and TIMESTAMPADD(unit, n, d).
func dateAddResult(x *Expr, args []ColumnDef) (ColumnDef, *errs.Error) {
	arg, unitNode := args[0], x.Args[2]
	if x.Op == "TIMESTAMPADD" {
		arg, unitNode = args[2], x.Args[0]
	}
	unit := ""
	if unitNode.Kind == ExprLiteral && unitNode.Literal != nil {
		unit = unitNode.Literal.Text
	}
	fsp := 0
	if strings.Contains(unit, "MICROSECOND") {
		fsp = 6
	}
	switch arg.Type {
	case TypeDate:
		switch unit {
		case "YEAR", "QUARTER", "MONTH", "WEEK", "DAY", "YEAR_MONTH":
			return temporalDef(RetDate, 0)
		}
		return temporalDef(RetDatetime, fsp)
	case TypeDatetime, TypeTimestamp:
		return temporalDef(RetDatetime, max(fsp, int(arg.Decimals)))
	}
	return funcResult(RetString, args[:1])
}

// timeOfResult types TIME(x): the fraction digits of a temporal argument, of a string
// literal as written, else 6.
func timeOfResult(x *Expr, args []ColumnDef) (ColumnDef, *errs.Error) {
	fsp := 6
	switch a := args[0]; {
	case a.Type == TypeDate:
		fsp = 0
	case a.Type == TypeDatetime || a.Type == TypeTimestamp || a.Type == TypeTime:
		fsp = int(a.Decimals)
	case x.Args[0].Kind == ExprLiteral && x.Args[0].Literal != nil:
		_, frac, _ := strings.Cut(x.Args[0].Literal.Text, ".")
		fsp = min(len(frac), 6)
	}
	return temporalDef(RetTime, fsp)
}

// IsTimestamp reports whether column is a TIMESTAMP column of t (ADR 0007).
func (t *Table) IsTimestamp(column string) bool {
	c, ok := t.column(column)
	if !ok {
		return false
	}
	ti, ok := ParseType(c.MySQLType())
	return ok && ti.Type == TypeTimestamp
}

// IsBinary reports whether column holds binary strings: BINARY, VARBINARY or a BLOB.
func (t *Table) IsBinary(column string) bool {
	c, ok := t.column(column)
	if !ok {
		return false
	}
	ti, ok := ParseType(c.MySQLType())
	if !ok || ti.Charset != CharsetBinary {
		return false
	}
	return ti.Type == TypeString || ti.Type == TypeVarString || ti.Type == TypeBlob
}

// isClock names the current-time functions, whose argument is a precision.
func isClock(op string) bool {
	switch op {
	case "NOW", "CURRENT_TIMESTAMP", "LOCALTIME", "LOCALTIMESTAMP", "SYSDATE", "CURTIME", "CURRENT_TIME", "UTC_TIMESTAMP", "UTC_TIME":
		return true
	}
	return false
}

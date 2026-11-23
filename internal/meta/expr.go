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
	// ExprArith is binary + - * / DIV (Op), on Args[0] and Args[1].
	ExprArith
	// ExprSubquery is a scalar subquery (Sub).
	ExprSubquery
	// ExprNeg is unary minus on Args[0].
	ExprNeg
	// ExprFunc is a function call (Op is its upper-case name) typed by Ret.
	ExprFunc
)

// FuncRet is how a function's result type follows from its arguments
// (docs/04-session-catalog.md section 6 rule 3; the table is internal/dialect/functions).
type FuncRet int

const (
	// RetMerge is the merged type of all arguments (IFNULL, COALESCE).
	RetMerge FuncRet = iota + 1
	// RetMergeTail is the merged type of the arguments after the first (IF).
	RetMergeTail
	// RetFirst is the first argument's type (NULLIF).
	RetFirst
	// RetNumeric keeps an integer or DECIMAL argument's type, else DOUBLE (ABS).
	RetNumeric
	// RetExactInt is BIGINT for an integer or DECIMAL argument, else DOUBLE (CEIL, FLOOR).
	RetExactInt
	// RetInt is BIGINT (SIGN, LENGTH, CHAR_LENGTH).
	RetInt
	// RetDouble is DOUBLE (SQRT, LN, POW, trigonometry).
	RetDouble
	// RetPi is DOUBLE printed with 6 decimals (PI).
	RetPi
	// RetMod is the arithmetic type of MOD and %.
	RetMod
	// RetString is VAR_STRING in the connection collation (UPPER, CONCAT).
	RetString
	// RetUint is BIGINT UNSIGNED (LAST_INSERT_ID, CONNECTION_ID).
	RetUint
	// RetAccount is a user@host string in the system character set (USER).
	RetAccount
	// RetVersion is the server version string (VERSION).
	RetVersion
	// Aggregates (Expr.Agg is set).
	// RetCount is COUNT: BIGINT, never NULL.
	RetCount
	// RetSum is SUM: DECIMAL with the argument's scale for exact numbers, else DOUBLE.
	RetSum
	// RetAvg is AVG: DECIMAL with div_precision_increment more decimals, else DOUBLE.
	RetAvg
	// RetMinMax is MIN and MAX: the argument's type, nullable.
	RetMinMax
	// RetGroupConcat is GROUP_CONCAT: a BLOB-typed string in the connection collation.
	RetGroupConcat
	// RetRound is ROUND and TRUNCATE: integer, DECIMAL with the places given, or DOUBLE.
	RetRound
	// RetDatetime, RetDate and RetTime are DATETIME, DATE and TIME; an argument is the
	// fractional seconds precision (NOW(3)).
	RetDatetime
	RetDate
	RetTime
	// RetFromUnixtime is FROM_UNIXTIME: DATETIME, or a string with a format argument.
	RetFromUnixtime
	// RetDateAdd is DATE_ADD and the like: DATE for a DATE and a date unit, DATETIME for
	// a temporal value, else a string.
	RetDateAdd
	// RetTimeOf is TIME(x): TIME with x's fractional digits.
	RetTimeOf
	// RetStrToDate is STR_TO_DATE: DATETIME, DATE or TIME by its literal format.
	RetStrToDate
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
	Ret               FuncRet
	Args              []*Expr
	// Agg marks an aggregate function (COUNT, SUM, ...).
	Agg bool
	// Sub is the subquery of an ExprSubquery (scalar), or of an ExprBool with Op
	// OpExists or OpInSubquery (Args[0] is the left operand).
	Sub *Select
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

// CheckExprs runs the arithmetic checks on expressions outside the select list (WHERE,
// ORDER BY, SET). Unknown columns are left to CheckRefs, which names their clause.
func CheckExprs(exprs []*Expr, from From) *errs.Error {
	return newScope(from).checkExprs(exprs)
}

func (s scope) checkExprs(exprs []*Expr) *errs.Error {
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

// Infer types x for the dialect, which needs argument types to translate some functions
// (CONCAT prints a DECIMAL with its scale). Unknown columns are not reported here.
func Infer(x *Expr, from From) (ColumnDef, *errs.Error) {
	return newScope(from).infer(x, "")
}

// InferKnown types x only when every column and system variable in it is known: false
// where Infer would fall back to a string for a name it cannot resolve.
func InferKnown(x *Expr, from From) (ColumnDef, bool) {
	d, err := newScope(from).infer(x, "where clause")
	return d, err == nil
}

// ClauseField is the clause error 1054 names for the select list.
const ClauseField = "field list"

// infer types x. clause names the clause of an unknown column (1054); "" skips the
// check (CheckRefs reports it).
func (s scope) infer(x *Expr, clause string) (ColumnDef, *errs.Error) {
	if x == nil {
		return ColumnDef{}, errs.Internal()
	}
	if x.Sub != nil {
		defs, e := s.subquery(x.Sub)
		if e != nil {
			return ColumnDef{}, e
		}
		if x.Op != OpExists && len(defs) != 1 {
			return ColumnDef{}, errs.OperandColumns(1)
		}
		if x.Kind == ExprSubquery {
			// A scalar subquery is NULL when it returns no row.
			d := defs[0]
			d.Flags &^= FlagNotNull | FlagPriKey | FlagUniqueKey | FlagMultipleKey | FlagPartKey | FlagAutoIncrement | FlagNoDefault
			return d, nil
		}
	}
	switch x.Kind {
	case ExprColumn:
		src, c, e := s.resolve(x.Qualifier, x.Column, clause)
		if e != nil {
			if clause == "" {
				return ColumnDef{Type: TypeVarString, Charset: CollationDefault}, nil
			}
			return ColumnDef{}, e
		}
		return src.column(c, c.Name), nil
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
		if x.Sub != nil && x.Op != OpExists {
			notNull = false // the subquery's values can be NULL
		}
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
	case ExprFunc:
		args := make([]ColumnDef, len(x.Args))
		for i, a := range x.Args {
			d, e := s.infer(a, clause)
			if e != nil {
				return ColumnDef{}, e
			}
			args[i] = d
		}
		switch x.Ret {
		case RetRound:
			return roundResult(x, args)
		case RetDatetime, RetDate, RetTime:
			return temporalResult(x, x.Ret)
		case RetDateAdd:
			return dateAddResult(x, args)
		case RetTimeOf:
			return timeOfResult(x, args)
		case RetStrToDate:
			return strToDateResult(x)
		case RetFromUnixtime:
			if len(args) > 1 {
				return funcResult(RetString, args)
			}
			return temporalDef(RetDatetime, 0)
		}
		d, e := funcResult(x.Ret, args)
		if e == nil && x.NotNull {
			d.Flags |= FlagNotNull
		}
		return d, e
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
	// divPrecisionIncrement is the default of MySQL's div_precision_increment; mysqlite
	// does not support changing it.
	divPrecisionIncrement = 4
	// decimalLongLongDigits is the precision SUM adds to an exact argument.
	decimalLongLongDigits = 22
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
		d.Length, d.Decimals = doubleLength, NotFixedDecimals
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
	case op == "DIV":
		// The quotient truncated to an integer, whatever the operands.
		d = numeric(TypeLongLong)
		p, sc := decimalOf(l)
		d.Length = uint32(min(p-sc+1, maxIntLength))
		unsigned = false
	case lc == classReal || rc == classReal || lc == classNull || rc == classNull:
		d = numeric(TypeDouble)
		d.Length, d.Decimals = doubleLength, NotFixedDecimals
		unsigned = false
	case lc == classDecimal || rc == classDecimal:
		lp, ls := decimalOf(l)
		rp, rs := decimalOf(r)
		var p, sc int
		switch op {
		case "*":
			p, sc = lp+rp, ls+rs
		case "/":
			// div_precision_increment (4) more decimals than the dividend.
			p, sc = lp+rs+divPrecisionIncrement, ls+divPrecisionIncrement
			unsigned = false
		default:
			sc = max(ls, rs)
			p = max(lp-ls, rp-rs) + 1 + sc
		}
		sc = min(sc, maxDecimalScale)
		p = min(p, maxDecimalDigits)
		d = numeric(TypeNewDecimal)
		d.Decimals = uint8(sc)
		d.Length = uint32(decimalLength(p, sc, unsigned))
	case op == "/":
		// Integers divide into a DECIMAL too.
		lp, _ := decimalOf(l)
		d = numeric(TypeNewDecimal)
		d.Decimals = divPrecisionIncrement
		d.Length = uint32(decimalLength(min(lp+divPrecisionIncrement, maxDecimalDigits), divPrecisionIncrement, false))
		unsigned = false
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
	// A division is NULL for a zero divisor.
	divides := op == "/" || op == "DIV" || op == "%"
	if l.Flags&FlagNotNull != 0 && r.Flags&FlagNotNull != 0 && !divides {
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

func funcResult(ret FuncRet, args []ColumnDef) (ColumnDef, *errs.Error) {
	switch ret {
	case RetMerge:
		return merge(args), nil
	case RetMergeTail:
		return merge(args[1:]), nil
	case RetFirst:
		d := args[0]
		d.Flags &^= FlagNotNull | FlagPriKey | FlagUniqueKey | FlagMultipleKey | FlagPartKey | FlagAutoIncrement | FlagNoDefault
		return d, nil
	case RetNumeric:
		switch classOf(args[0]) {
		case classTemporal:
			return ColumnDef{}, refuseTemporal()
		case classInt:
			d := numeric(TypeLongLong)
			d.Length = args[0].Length
			d.Flags |= args[0].Flags & FlagUnsigned
			return d, nil
		case classDecimal:
			p, sc := decimalOf(args[0])
			d := numeric(TypeNewDecimal)
			d.Decimals = uint8(sc)
			d.Length = uint32(decimalLength(p, sc, false))
			return d, nil
		}
		return double(), nil
	case RetExactInt:
		switch classOf(args[0]) {
		case classTemporal:
			return ColumnDef{}, refuseTemporal()
		case classInt, classDecimal:
			p, sc := decimalOf(args[0])
			d := numeric(TypeLongLong)
			d.Length = uint32(min(p-sc+2, maxIntLength))
			return d, nil
		}
		return double(), nil
	case RetInt:
		d := numeric(TypeLongLong)
		d.Length = maxIntLength
		return d, nil
	case RetDouble:
		return double(), nil
	case RetPi:
		d := numeric(TypeDouble)
		d.Length, d.Decimals = 8, 6
		return d, nil
	case RetMod:
		return arith("%", args[0], args[1])
	case RetUint:
		d := numeric(TypeLongLong)
		d.Length = maxIntLength
		d.Flags |= FlagUnsigned | FlagNotNull
		return d, nil
	case RetAccount:
		// (USERNAME_CHAR_LENGTH + HOSTNAME_LENGTH + 1) characters of utf8mb3.
		return ColumnDef{Type: TypeVarString, Charset: CollationUTF8MB3, Length: (32 + 255 + 1) * 3, Flags: FlagNotNull}, nil
	case RetVersion:
		// SERVER_VERSION_LENGTH characters of utf8mb3.
		return ColumnDef{Type: TypeVarString, Charset: CollationUTF8MB3, Length: 60 * 3, Flags: FlagNotNull}, nil
	case RetCount:
		d := numeric(TypeLongLong)
		d.Length = maxIntLength
		d.Flags |= FlagNotNull
		return d, nil
	case RetSum, RetAvg:
		switch classOf(args[0]) {
		case classTemporal:
			return ColumnDef{}, refuseTemporal()
		case classInt, classDecimal:
			p, sc := decimalOf(args[0])
			if ret == RetSum {
				p += decimalLongLongDigits
			} else {
				p += divPrecisionIncrement
				sc += divPrecisionIncrement
			}
			p, sc = min(p, maxDecimalDigits), min(sc, maxDecimalScale)
			d := numeric(TypeNewDecimal)
			d.Decimals = uint8(sc)
			d.Length = uint32(decimalLength(p, sc, false))
			return d, nil
		}
		return double(), nil
	case RetMinMax:
		d := args[0]
		d.Flags &^= FlagNotNull | FlagPriKey | FlagUniqueKey | FlagMultipleKey | FlagPartKey | FlagAutoIncrement | FlagNoDefault
		return d, nil
	case RetGroupConcat:
		// group_concat_max_len's default, 1024 characters. `oracle: pending`.
		return ColumnDef{Type: TypeBlob, Charset: CollationDefault, Length: 1024 * 4, Flags: FlagBlob}, nil
	case RetString:
		n := 0
		for _, a := range args {
			n += charLength(a)
		}
		return ColumnDef{Type: TypeVarString, Charset: CollationDefault, Length: uint32(min(n, maxStringChars) * 4)}, nil
	}
	return ColumnDef{}, errs.Internal()
}

const maxStringChars = 65535

func double() ColumnDef {
	d := numeric(TypeDouble)
	d.Length, d.Decimals = doubleLength, NotFixedDecimals
	return d
}

// IsString is a character or binary string type.
func IsString(d ColumnDef) bool {
	switch d.Type {
	case TypeVarString, TypeString, TypeVarchar, TypeBlob, TypeJSON, TypeEnum, TypeSet:
		return true
	}
	return false
}

// charLength is the length of d in characters: strings carry bytes of their charset,
// numbers and dates their display width.
func charLength(d ColumnDef) int {
	if !IsString(d) || d.Charset == CharsetBinary {
		return int(d.Length)
	}
	return int(d.Length) / maxLen(d.Charset)
}

// merge is MySQL's type aggregation for IFNULL, COALESCE and IF, simplified: NULL
// arguments do not count; integers stay BIGINT, integers and DECIMAL give DECIMAL, any
// other number gives DOUBLE, equal temporal types stay, anything else is a string.
// `oracle: pending`.
func merge(args []ColumnDef) ColumnDef {
	var defs []ColumnDef
	for _, a := range args {
		if classOf(a) != classNull {
			defs = append(defs, a)
		}
	}
	if len(defs) == 0 {
		return ColumnDef{Type: TypeNull, Charset: CharsetBinary, Flags: FlagBinary}
	}
	allInt, exact, number, sameTemporal := true, true, true, true
	unsigned := true
	for _, d := range defs {
		c := classOf(d)
		str := IsString(d)
		allInt = allInt && c == classInt && !str
		exact = exact && (c == classInt || c == classDecimal) && !str
		number = number && c != classTemporal && !str
		sameTemporal = sameTemporal && c == classTemporal && d.Type == defs[0].Type
		unsigned = unsigned && d.Flags&FlagUnsigned != 0
	}
	switch {
	case allInt:
		d := numeric(TypeLongLong)
		for _, a := range defs {
			d.Length = max(d.Length, a.Length)
		}
		if unsigned {
			d.Flags |= FlagUnsigned
		}
		return d
	case exact:
		intDigits, scale := 0, 0
		for _, a := range defs {
			p, s := decimalOf(a)
			intDigits, scale = max(intDigits, p-s), max(scale, s)
		}
		d := numeric(TypeNewDecimal)
		d.Decimals = uint8(scale)
		d.Length = uint32(decimalLength(intDigits+scale, scale, false))
		return d
	case number:
		return double()
	case sameTemporal:
		d := defs[0]
		d.Schema, d.Table, d.OrgTable, d.Name, d.OrgName = "", "", "", "", ""
		d.Flags &= FlagBinary | FlagTimestamp
		return d
	}
	n := 0
	for _, a := range defs {
		n = max(n, charLength(a))
	}
	return ColumnDef{Type: TypeVarString, Charset: CollationDefault, Length: uint32(n * 4)}
}

// Subquery operators of ExprBool nodes.
const (
	OpExists     = "EXISTS"
	OpInSubquery = "IN SUBQUERY"
)

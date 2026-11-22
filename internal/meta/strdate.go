package meta

import "mysqlite/internal/errs"

// StrToDateKind classifies a STR_TO_DATE format as MySQL types the result: DATETIME
// when it reads date and time parts, DATE or TIME when it reads only one kind, with
// fsp 6 when it reads %f. ok is false for the formats mysqlite does not implement: week
// specifiers (%U %u %V %v %X %x %w), unknown specifiers, and formats without a date or
// time part.
func StrToDateKind(format string) (ret FuncRet, fsp int, ok bool) {
	date, clock := false, false
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 == len(format) {
			continue
		}
		i++
		switch format[i] {
		case 'Y', 'y', 'm', 'c', 'd', 'e', 'M', 'b', 'j', 'D':
			date = true
		case 'a', 'W', '%':
			// Weekday names are read and ignored; %% is a literal.
		case 'H', 'k', 'h', 'I', 'l', 'i', 's', 'S', 'p', 'T', 'r':
			clock = true
		case 'f':
			clock, fsp = true, 6
		default:
			return 0, 0, false
		}
	}
	switch {
	case date && clock:
		return RetDatetime, fsp, true
	case date:
		return RetDate, 0, true
	case clock:
		return RetTime, fsp, true
	}
	return 0, 0, false
}

// TimeFormatOK reports whether a TIME_FORMAT format uses only the hour, minute,
// second, microsecond and AM/PM specifiers. MySQL documents the others as producing
// "a NULL value or 0", so mysqlite refuses them.
func TimeFormatOK(format string) bool {
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 == len(format) {
			continue
		}
		i++
		switch format[i] {
		case 'H', 'k', 'h', 'I', 'l', 'i', 's', 'S', 'f', 'p', 'r', 'T', '%':
		default:
			return false
		}
	}
	return true
}

// strToDateResult types STR_TO_DATE(s, format) from its literal format.
func strToDateResult(x *Expr) (ColumnDef, *errs.Error) {
	f := x.Args[1]
	if f.Kind == ExprLiteral && f.Literal != nil && f.Literal.Kind == LiteralString {
		if ret, fsp, ok := StrToDateKind(f.Literal.Text); ok {
			return temporalDef(ret, fsp)
		}
	}
	return ColumnDef{}, errs.Unsupported("STR_TO_DATE with this format", "the result type depends on the format")
}

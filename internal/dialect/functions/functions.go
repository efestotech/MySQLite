// Package functions is the MySQL function table (docs/05-dialect.md section 5): for
// each supported function, how it is translated and what it returns. The dialect emits
// from it, meta infers result types from it, and the UDFs it names are registered by
// package udf. A function that is not in the table is refused with 1235.
package functions

import "mysqlite/internal/meta"

// Emit is how a call is translated.
type Emit int

const (
	// Rename calls the SQLite function SQLite with the same arguments.
	Rename Emit = iota + 1
	// Bytes is LENGTH: the byte length of the argument's MySQL string form.
	Bytes
	// Concat is CONCAT: || over the arguments' string forms, NULL if any is NULL.
	Concat
	// Session is a value the executor binds from the session (Func.Session names it).
	Session
	// LastInsertID is LAST_INSERT_ID(): a Session value without an argument; with one,
	// UDFLastInsertID, which also hands the value to the executor.
	LastInsertID
	// Exact calls the SQLite function SQLite with the arguments (a missing second one
	// as 0) and a last argument 1 when the first is an exact number, else 0 (ROUND).
	Exact
)

// Session values the executor binds (dialect.SessionValue).
const (
	SessionLastInsertID = "LAST_INSERT_ID"
	SessionRowCount     = "ROW_COUNT"
	SessionConnectionID = "CONNECTION_ID"
	SessionUser         = "USER"
	SessionCurrentUser  = "CURRENT_USER"
	SessionVersion      = "VERSION"
	// SessionSink is the sink number LAST_INSERT_ID(expr) writes to (udf.NewSink).
	SessionSink = "LAST_INSERT_ID sink"
	// The statement's time (ADR 0007): in the session time zone, or in UTC.
	SessionNow           = "NOW"
	SessionCurDate       = "CURDATE"
	SessionCurTime       = "CURTIME"
	SessionUTCNow        = "UTC_TIMESTAMP"
	SessionUTCDate       = "UTC_DATE"
	SessionUTCTime       = "UTC_TIME"
	SessionUnixTimestamp = "UNIX_TIMESTAMP"
	// SessionTimeZone is @@time_zone, the zone TIMESTAMP values convert through.
	SessionTimeZone = "TIME_ZONE"
)

// Func is one table entry.
type Func struct {
	// MinArgs and MaxArgs bound the argument count (1582 outside); MaxArgs -1 is
	// variadic.
	MinArgs, MaxArgs int
	Emit             Emit
	// SQLite is the function a Rename call becomes. ByArgs overrides it per argument
	// count (LOG(x) is ln, LOG(b, x) is log).
	SQLite string
	ByArgs map[int]string
	Ret    meta.FuncRet
	// TextArgs converts numeric arguments to MySQL's string form first, so a DECIMAL
	// keeps its scale and a DOUBLE prints as MySQL prints it.
	TextArgs bool
	// Session is the session value of a Session call.
	Session string
	// Fsp makes the argument of a Session call its fractional seconds precision.
	Fsp bool
	// ArgUDF is the UDF a Session function becomes with an argument
	// (UNIX_TIMESTAMP(x)); it is called as a Zoned Rename.
	ArgUDF string
	// Extra is an SQL literal a Rename call passes after the arguments (the part of
	// UDFDatePart).
	Extra string
	// Pad passes NULL for missing arguments up to Pad; Zoned then passes @@time_zone.
	Pad   int
	Zoned bool
}

// UDF names registered by package udf.
const (
	UDFUpper       = "mysqlite_upper"
	UDFLower       = "mysqlite_lower"
	UDFDecimalText = "mysqlite_decimal_text"
	UDFDoubleText  = "mysqlite_double_text"
	// UDFMod is MOD and %: exact on integers, fmod otherwise. SQLite's % truncates
	// REALs to integers and its mod() loses integer precision above 2^53.
	UDFMod = "mysqlite_mod"
	// UDFDivide is /, UDFIntDiv is DIV: SQLite's / divides integers as integers.
	UDFDivide = "mysqlite_divide"
	UDFIntDiv = "mysqlite_intdiv"
	// UDFXor is XOR: MySQL converts string operands to numbers, SQLite does not.
	UDFXor = "mysqlite_xor"
	// UDFLike is LIKE under MySQL's collations; SQLite's LIKE folds ASCII only.
	UDFLike = "mysqlite_like"
	// UDFLastInsertID(sink, x) is LAST_INSERT_ID(x): x as an integer, also written to
	// the sink.
	UDFLastInsertID = "mysqlite_last_insert_id"
	// UDFCut(s, n) is s cut to at most n bytes on a character boundary
	// (group_concat_max_len).
	UDFCut = "mysqlite_cut"
	// UDFRaise(code) raises the MySQL error code (errs.FunctionError); not deterministic,
	// so SQLite never evaluates it ahead of the branch that calls it.
	UDFRaise = "mysqlite_raise"
	// UDFRound and UDFTruncate are ROUND and TRUNCATE (Emit Exact): exact numbers round
	// on their decimal digits, DOUBLEs on their binary value.
	UDFRound    = "mysqlite_round"
	UDFTruncate = "mysqlite_truncate"
	// UDFTzFromUTC(v, zone) and UDFTzToUTC(v, zone) convert TIMESTAMP values between
	// storage (UTC) and the session zone (ADR 0007).
	UDFTzFromUTC = "mysqlite_tz_from_utc"
	UDFTzToUTC   = "mysqlite_tz_to_utc"
	// Date functions (ADR 0007): parts, names, DATE/LAST_DAY, DATEDIFF, DATE_FORMAT,
	// UNIX_TIMESTAMP(x), FROM_UNIXTIME.
	UDFDatePart      = "mysqlite_date_part"
	UDFDateName      = "mysqlite_date_name"
	UDFDateOnly      = "mysqlite_date_only"
	UDFDateDiff      = "mysqlite_datediff"
	UDFDateFormat    = "mysqlite_date_format"
	UDFUnixTimestamp = "mysqlite_unix_timestamp"
	UDFFromUnixtime  = "mysqlite_from_unixtime"
)

func rename(name string, minArgs, maxArgs int, ret meta.FuncRet) Func {
	return Func{MinArgs: minArgs, MaxArgs: maxArgs, Emit: Rename, SQLite: name, Ret: ret}
}

func session(name string, ret meta.FuncRet) Func {
	return Func{Emit: Session, Session: name, Ret: ret}
}

// clock is a current-time function; fsp allows the precision argument.
func clock(name string, ret meta.FuncRet, fsp bool) Func {
	f := Func{Emit: Session, Session: name, Ret: ret, Fsp: fsp}
	if fsp {
		f.MaxArgs = 1
	}
	return f
}

// part is a date function reading one part of its argument through UDF with the part
// name.
func part(udf, name string, ret meta.FuncRet) Func {
	return Func{MinArgs: 1, MaxArgs: 1, Emit: Rename, SQLite: udf, Extra: "'" + name + "'", Ret: ret}
}

func text(f Func) Func {
	f.TextArgs = true
	return f
}

var table = map[string]Func{
	// Control flow.
	"IFNULL":   rename("ifnull", 2, 2, meta.RetMerge),
	"COALESCE": rename("coalesce", 1, -1, meta.RetMerge),
	"NULLIF":   rename("nullif", 2, 2, meta.RetFirst),
	"IF":       rename("iif", 3, 3, meta.RetMergeTail),

	// Math. SQLite is built with SQLITE_ENABLE_MATH_FUNCTIONS (verified 2026-11-22);
	// domain errors give NULL in both engines.
	"ABS":     rename("abs", 1, 1, meta.RetNumeric),
	"SIGN":    rename("sign", 1, 1, meta.RetInt),
	"CEIL":    rename("ceil", 1, 1, meta.RetExactInt),
	"CEILING": rename("ceil", 1, 1, meta.RetExactInt),
	"FLOOR":   rename("floor", 1, 1, meta.RetExactInt),
	"SQRT":    rename("sqrt", 1, 1, meta.RetDouble),
	"EXP":     rename("exp", 1, 1, meta.RetDouble),
	"LN":      rename("ln", 1, 1, meta.RetDouble),
	// MySQL LOG(x) is the natural logarithm; SQLite log(x) is base 10.
	"LOG":     {MinArgs: 1, MaxArgs: 2, Emit: Rename, ByArgs: map[int]string{1: "ln", 2: "log"}, Ret: meta.RetDouble},
	"LOG2":    rename("log2", 1, 1, meta.RetDouble),
	"LOG10":   rename("log10", 1, 1, meta.RetDouble),
	"POW":     rename("pow", 2, 2, meta.RetDouble),
	"POWER":   rename("pow", 2, 2, meta.RetDouble),
	"PI":      rename("pi", 0, 0, meta.RetPi),
	"SIN":     rename("sin", 1, 1, meta.RetDouble),
	"COS":     rename("cos", 1, 1, meta.RetDouble),
	"TAN":     rename("tan", 1, 1, meta.RetDouble),
	"ASIN":    rename("asin", 1, 1, meta.RetDouble),
	"ACOS":    rename("acos", 1, 1, meta.RetDouble),
	"ATAN":    {MinArgs: 1, MaxArgs: 2, Emit: Rename, ByArgs: map[int]string{1: "atan", 2: "atan2"}, Ret: meta.RetDouble},
	"ATAN2":   rename("atan2", 2, 2, meta.RetDouble),
	"DEGREES": rename("degrees", 1, 1, meta.RetDouble),
	// SQLite's round() rounds the binary value: round(2.675, 2) is 2.67, MySQL's 2.68.
	"ROUND":    {MinArgs: 1, MaxArgs: 2, Emit: Exact, SQLite: UDFRound, Ret: meta.RetRound},
	"TRUNCATE": {MinArgs: 2, MaxArgs: 2, Emit: Exact, SQLite: UDFTruncate, Ret: meta.RetRound},
	"RADIANS":  rename("radians", 1, 1, meta.RetDouble),

	// Strings. SQLite's upper/lower fold ASCII only; MySQL folds every letter.
	"UPPER":            text(rename(UDFUpper, 1, 1, meta.RetString)),
	"UCASE":            text(rename(UDFUpper, 1, 1, meta.RetString)),
	"LOWER":            text(rename(UDFLower, 1, 1, meta.RetString)),
	"LCASE":            text(rename(UDFLower, 1, 1, meta.RetString)),
	"LENGTH":           {MinArgs: 1, MaxArgs: 1, Emit: Bytes, Ret: meta.RetInt, TextArgs: true},
	"OCTET_LENGTH":     {MinArgs: 1, MaxArgs: 1, Emit: Bytes, Ret: meta.RetInt, TextArgs: true},
	"CHAR_LENGTH":      text(rename("length", 1, 1, meta.RetInt)),
	"CHARACTER_LENGTH": text(rename("length", 1, 1, meta.RetInt)),
	"CONCAT":           {MinArgs: 1, MaxArgs: -1, Emit: Concat, Ret: meta.RetString, TextArgs: true},
	// concat_ws skips NULL arguments and is NULL for a NULL separator, as in MySQL.
	"CONCAT_WS": text(rename("concat_ws", 2, -1, meta.RetString)),

	// Session.
	"LAST_INSERT_ID": {MinArgs: 0, MaxArgs: 1, Emit: LastInsertID, Session: SessionLastInsertID, Ret: meta.RetUint},
	"ROW_COUNT":      session(SessionRowCount, meta.RetInt),
	"CONNECTION_ID":  session(SessionConnectionID, meta.RetUint),
	"USER":           session(SessionUser, meta.RetAccount),
	"SESSION_USER":   session(SessionUser, meta.RetAccount),
	"SYSTEM_USER":    session(SessionUser, meta.RetAccount),
	"CURRENT_USER":   session(SessionCurrentUser, meta.RetAccount),
	"VERSION":        session(SessionVersion, meta.RetVersion),

	// Current time (ADR 0007). SYSDATE is the statement time too (MySQL: per call).
	"NOW":               clock(SessionNow, meta.RetDatetime, true),
	"CURRENT_TIMESTAMP": clock(SessionNow, meta.RetDatetime, true),
	"LOCALTIME":         clock(SessionNow, meta.RetDatetime, true),
	"LOCALTIMESTAMP":    clock(SessionNow, meta.RetDatetime, true),
	"SYSDATE":           clock(SessionNow, meta.RetDatetime, true),
	"CURDATE":           clock(SessionCurDate, meta.RetDate, false),
	"CURRENT_DATE":      clock(SessionCurDate, meta.RetDate, false),
	"CURTIME":           clock(SessionCurTime, meta.RetTime, true),
	"CURRENT_TIME":      clock(SessionCurTime, meta.RetTime, true),
	"UTC_TIMESTAMP":     clock(SessionUTCNow, meta.RetDatetime, true),
	"UTC_DATE":          clock(SessionUTCDate, meta.RetDate, false),
	"UTC_TIME":          clock(SessionUTCTime, meta.RetTime, true),
	"UNIX_TIMESTAMP":    {MaxArgs: 1, Emit: Session, Session: SessionUnixTimestamp, Ret: meta.RetInt, ArgUDF: UDFUnixTimestamp},

	// Dates. Invalid dates are NULL.
	"YEAR":          part(UDFDatePart, "YEAR", meta.RetInt),
	"MONTH":         part(UDFDatePart, "MONTH", meta.RetInt),
	"DAY":           part(UDFDatePart, "DAY", meta.RetInt),
	"DAYOFMONTH":    part(UDFDatePart, "DAY", meta.RetInt),
	"HOUR":          part(UDFDatePart, "HOUR", meta.RetInt),
	"MINUTE":        part(UDFDatePart, "MINUTE", meta.RetInt),
	"SECOND":        part(UDFDatePart, "SECOND", meta.RetInt),
	"MICROSECOND":   part(UDFDatePart, "MICROSECOND", meta.RetInt),
	"QUARTER":       part(UDFDatePart, "QUARTER", meta.RetInt),
	"DAYOFWEEK":     part(UDFDatePart, "DAYOFWEEK", meta.RetInt),
	"WEEKDAY":       part(UDFDatePart, "WEEKDAY", meta.RetInt),
	"DAYOFYEAR":     part(UDFDatePart, "DAYOFYEAR", meta.RetInt),
	"DAYNAME":       part(UDFDateName, "DAY", meta.RetString),
	"MONTHNAME":     part(UDFDateName, "MONTH", meta.RetString),
	"DATE":          part(UDFDateOnly, "DATE", meta.RetDate),
	"LAST_DAY":      part(UDFDateOnly, "LAST_DAY", meta.RetDate),
	"DATEDIFF":      rename(UDFDateDiff, 2, 2, meta.RetInt),
	"DATE_FORMAT":   rename(UDFDateFormat, 2, 2, meta.RetString),
	"FROM_UNIXTIME": {MinArgs: 1, MaxArgs: 2, Emit: Rename, SQLite: UDFFromUnixtime, Pad: 2, Zoned: true, Ret: meta.RetFromUnixtime},
}

// Lookup returns the entry of a function by its MySQL name, in any case.
func Lookup(upperName string) (Func, bool) {
	f, ok := table[upperName]
	return f, ok
}

// SQLiteName is the SQLite function a Rename call with n arguments becomes.
func (f Func) SQLiteName(n int) string {
	if name, ok := f.ByArgs[n]; ok {
		return name
	}
	return f.SQLite
}

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
)

func rename(name string, minArgs, maxArgs int, ret meta.FuncRet) Func {
	return Func{MinArgs: minArgs, MaxArgs: maxArgs, Emit: Rename, SQLite: name, Ret: ret}
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
	"RADIANS": rename("radians", 1, 1, meta.RetDouble),

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

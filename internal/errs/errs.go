// Package errs maps internal failures to MySQL errors (docs/09-conventions.md
// section 4). Raw SQLite text never reaches the client: unmapped SQLite errors become
// 1105, and the caller logs the original.
//
// Message templates follow the MySQL 8.0 error reference. They have not been compared
// with a real server yet (ADR 0002 addendum, oracle pending).
package errs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"mysqlite/internal/engine"
)

// Error is a MySQL error: number, SQLSTATE and message.
type Error struct {
	Code    uint16
	State   string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("ERROR %d (%s): %s", e.Code, e.State, e.Message)
}

func newError(code uint16, state, format string, args ...any) *Error {
	return &Error{Code: code, State: state, Message: fmt.Sprintf(format, args...)}
}

// Op is the kind of statement that failed.
type Op int

const (
	OpOther Op = iota
	OpInsert
	OpUpdate
	OpDelete
)

// FKSide says which side of a foreign key the failing statement touched.
type FKSide int

const (
	FKUnknown FKSide = iota
	FKChild          // the statement wrote a referencing row
	FKParent         // the statement removed or changed a referenced row
)

// Hint carries what the caller knows and SQLite does not report.
type Hint struct {
	// Database is the current database, for messages such as 1146.
	Database string
	// DatabaseArg is the database name the caller passed to the engine (1049, 1007, 1102).
	DatabaseArg string
	Op          Op
	// FK overrides the side derived from Op. Needed for UPDATE, which can be either.
	FK FKSide
	// FKDetail is the parenthesized constraint description of 1451/1452, if known.
	FKDetail string
	// DuplicateEntry is the conflicting value for 1062, formatted as MySQL does
	// ('-'-joined for composite keys). SQLite does not report it.
	DuplicateEntry string
	// Entry computes DuplicateEntry for the unique key on table(cols) when it is empty.
	Entry func(table string, cols []string) string
	// KeyName resolves the index name for a unique conflict on table(cols). When nil,
	// the column names joined by '_' are used (deviation until the catalog exists, M2).
	KeyName func(table string, cols []string) string
	// Clause names where an unknown column appeared (1054); default "field list".
	Clause string
}

// SQLite extended result codes (https://sqlite.org/rescode.html).
const (
	sqliteError            = 1
	sqliteBusy             = 5
	sqliteLocked           = 6
	sqliteConstraintCheck  = 275
	sqliteConstraintFK     = 787
	sqliteConstraintNotNul = 1299
	sqliteConstraintPK     = 1555
	sqliteConstraintUnique = 2067
)

// Message patterns, pinned by tests that produce the errors with real SQLite.
var (
	reUnique  = regexp.MustCompile(`UNIQUE constraint failed: (.+) \(\d+\)$`)
	reNotNull = regexp.MustCompile(`NOT NULL constraint failed: (.+) \(\d+\)$`)
	reCheck   = regexp.MustCompile(`CHECK constraint failed: (.+) \(\d+\)$`)
	reTable   = regexp.MustCompile(`no such table: (\S+) \(\d+\)$`)
	reColumn  = regexp.MustCompile(`no such column: (\S+) \(\d+\)$`)
	// "table p has 6 columns but 1 values were supplied" (no column list) and
	// "1 values for 2 columns" (column list).
	reValueCount = regexp.MustCompile(`(has \d+ columns but \d+ values were supplied|\d+ values for \d+ columns) \(\d+\)$`)
)

// Map converts err to a MySQL error. It returns nil for nil and an *Error unchanged.
func Map(err error, h Hint) *Error {
	if err == nil {
		return nil
	}
	var own *Error
	if errors.As(err, &own) {
		return own
	}
	switch {
	case errors.Is(err, engine.ErrUnknownDatabase):
		return UnknownDatabase(h.DatabaseArg)
	case errors.Is(err, engine.ErrDatabaseExists):
		return newError(1007, "HY000", "Can't create database '%s'; database exists", h.DatabaseArg)
	case errors.Is(err, engine.ErrInvalidDatabaseName):
		return newError(1102, "42000", "Incorrect database name '%s'", h.DatabaseArg)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, engine.ErrWriteQueueFull):
		// Waiting for the single writer timed out, or too many sessions already wait for
		// it (docs/06-transactions.md sections 4 and 6).
		return LockWaitTimeout()
	case errors.Is(err, context.Canceled):
		return newError(1317, "70100", "Query execution was interrupted")
	}
	if code, msg, ok := engine.ErrorCode(err); ok {
		return fromSQLite(code, msg, h)
	}
	return Internal()
}

func fromSQLite(code int, msg string, h Hint) *Error {
	switch code {
	case sqliteConstraintUnique, sqliteConstraintPK:
		m := reUnique.FindStringSubmatch(msg)
		if m == nil {
			return Internal()
		}
		table, cols := splitColumns(m[1])
		key := strings.Join(cols, "_")
		switch {
		case code == sqliteConstraintPK:
			key = "PRIMARY"
		case h.KeyName != nil:
			key = h.KeyName(table, cols)
		}
		entry := h.DuplicateEntry
		if entry == "" && h.Entry != nil {
			entry = h.Entry(table, cols)
		}
		return DuplicateEntry(entry, table+"."+key)
	case sqliteConstraintNotNul:
		m := reNotNull.FindStringSubmatch(msg)
		if m == nil {
			return Internal()
		}
		_, cols := splitColumns(m[1])
		return BadNull(cols[0])
	case sqliteConstraintCheck:
		m := reCheck.FindStringSubmatch(msg)
		if m == nil {
			return Internal()
		}
		// mysqlite's own CHECKs stand for MySQL's type checks (dialect, CREATE TABLE).
		// SQLite does not say which row failed; for INSERT the literal rows are checked
		// before SQLite runs, so this is UPDATE's first changed row.
		if col, ok := strings.CutPrefix(m[1], CheckRangePrefix); ok {
			return OutOfRange(col, 1)
		}
		if col, ok := strings.CutPrefix(m[1], CheckLengthPrefix); ok {
			return DataTooLong(col, 1)
		}
		return newError(3819, "HY000", "Check constraint '%s' is violated.", m[1])
	case sqliteConstraintFK:
		return foreignKey(h)
	}
	switch code & 0xff { // primary result code
	case sqliteBusy, sqliteLocked:
		return LockWaitTimeout()
	case sqliteError:
		if e := fromFunction(msg); e != nil {
			return e
		}
		if strings.Contains(msg, "integer overflow") {
			// SQLite's sum() and abs() stop at 64 bits; MySQL sums integers as DECIMAL.
			return Unsupported("integer result beyond the BIGINT range", "SQLite computes SUM of integers in 64 bits")
		}
		if m := reTable.FindStringSubmatch(msg); m != nil {
			table := strings.TrimPrefix(m[1], "main.")
			return NoSuchTable(h.Database, table)
		}
		if m := reColumn.FindStringSubmatch(msg); m != nil {
			clause := h.Clause
			if clause == "" {
				clause = "field list"
			}
			return UnknownColumn(m[1], clause)
		}
		if reValueCount.MatchString(msg) {
			// SQLite does not say which row; the dialect layer reports rows whose lengths
			// differ from each other, so a mismatch reaching here is in every row.
			return ColumnCount(1)
		}
	}
	return Internal()
}

// foreignKey picks 1452 or 1451. SQLite reports both directions with the same code and
// no names (docs/agent-notes/api-sqlite-driver.md, T2), so the side comes from the
// statement: INSERT writes a child row, DELETE removes a parent row. UPDATE can be
// either and needs Hint.FK; without it, 1452 is returned.
func foreignKey(h Hint) *Error {
	side := h.FK
	if side == FKUnknown {
		if h.Op == OpDelete {
			side = FKParent
		} else {
			side = FKChild
		}
	}
	detail := ""
	if h.FKDetail != "" {
		detail = " (" + h.FKDetail + ")"
	}
	if side == FKParent {
		return newError(1451, "23000", "Cannot delete or update a parent row: a foreign key constraint fails%s", detail)
	}
	return newError(1452, "23000", "Cannot add or update a child row: a foreign key constraint fails%s", detail)
}

// splitColumns turns SQLite's "t.a, t.b" into table t and columns [a b].
func splitColumns(s string) (string, []string) {
	var table string
	var cols []string
	for _, part := range strings.Split(s, ", ") {
		t, c, ok := strings.Cut(part, ".")
		if !ok {
			c = t
			t = ""
		}
		if table == "" {
			table = t
		}
		cols = append(cols, c)
	}
	return table, cols
}

// UnknownDatabase is 1049.
func UnknownDatabase(name string) *Error {
	return newError(1049, "42000", "Unknown database '%s'", name)
}

// UnknownSystemVariable is 1193.
func UnknownSystemVariable(name string) *Error {
	return newError(1193, "HY000", "Unknown system variable '%s'", name)
}

// WrongVariableScope is 1238: @@global on a SESSION-only variable, or @@session on a
// GLOBAL-only one. scope is the scope the variable has.
func WrongVariableScope(name, scope string) *Error {
	return newError(1238, "HY000", "Variable '%s' is a %s variable", name, scope)
}

// ReadOnlyVariable is 1238: SET of a read-only system variable.
func ReadOnlyVariable(name string) *Error {
	return newError(1238, "HY000", "Variable '%s' is a read only variable", name)
}

// SessionOnlyVariable is 1228: SET GLOBAL of a SESSION-only variable.
func SessionOnlyVariable(name string) *Error {
	return newError(1228, "HY000", "Variable '%s' is a SESSION variable and can't be used with SET GLOBAL", name)
}

// GlobalOnlyVariable is 1229: SET SESSION of a GLOBAL-only variable.
func GlobalOnlyVariable(name string) *Error {
	return newError(1229, "HY000", "Variable '%s' is a GLOBAL variable and should be set with SET GLOBAL", name)
}

// SessionReadOnlyVariable is 1621: SET SESSION of a variable whose session value is
// read-only.
func SessionReadOnlyVariable(name string) *Error {
	return newError(1621, "HY000", "SESSION variable '%s' is read-only. Use SET GLOBAL to assign the value", name)
}

// WrongValueForVariable is 1231.
func WrongValueForVariable(name, value string) *Error {
	return newError(1231, "42000", "Variable '%s' can't be set to the value of '%s'", name, value)
}

// WrongTypeForVariable is 1232.
func WrongTypeForVariable(name string) *Error {
	return newError(1232, "42000", "Incorrect argument type to variable '%s'", name)
}

// UnknownCharset is 1115.
func UnknownCharset(name string) *Error {
	return newError(1115, "42000", "Unknown character set: '%s'", name)
}

// UnknownCollation is 1273.
func UnknownCollation(name string) *Error {
	return newError(1273, "HY000", "Unknown collation: '%s'", name)
}

// CollationCharsetMismatch is 1253.
func CollationCharsetMismatch(collation, charset string) *Error {
	return newError(1253, "42000", "COLLATION '%s' is not valid for CHARACTER SET '%s'", collation, charset)
}

// TruncatedValue is 1292 (a warning when a SET value is clamped to the variable's range).
func TruncatedValue(name, value string) *Error {
	return newError(1292, "22007", "Truncated incorrect %s value: '%s'", name, value)
}

// UnknownTimeZone is 1298.
func UnknownTimeZone(name string) *Error {
	return newError(1298, "HY000", "Unknown or incorrect time zone: '%s'", name)
}

// NoDatabaseSelected is 1046.
func NoDatabaseSelected() *Error {
	return newError(1046, "3D000", "No database selected")
}

// Parse is 1064 with MySQL's wording.
func Parse(near string, line int) *Error {
	return newError(1064, "42000",
		"You have an error in your SQL syntax; check the manual that corresponds to your MySQL server version for the right syntax to use near '%s' at line %d",
		near, line)
}

// Unsupported is 1235 with the strict-mode message of docs/05-dialect.md section 2.
func Unsupported(construct, reason string) *Error {
	return newError(1235, "42000", "mysqlite: unsupported construct %s: %s", construct, reason)
}

// FieldSpecifiedTwice is 1110: a column listed twice in INSERT.
func FieldSpecifiedTwice(column string) *Error {
	return newError(1110, "42000", "Column '%s' specified twice", column)
}

// NoDefaultValue is 1364: strict mode, an INSERT omits a NOT NULL column without default.
func NoDefaultValue(column string) *Error {
	return newError(1364, "HY000", "Field '%s' doesn't have a default value", column)
}

// BadNull is 1048: NULL written to a NOT NULL column.
func BadNull(column string) *Error {
	return newError(1048, "23000", "Column '%s' cannot be null", column)
}

// Names of the CHECK constraints the dialect adds for MySQL's type checks.
const (
	CheckRangePrefix  = "__mysqlite_range_"
	CheckLengthPrefix = "__mysqlite_length_"
)

// ReadOnlyServer is 1290: the server runs with --readonly (docs/06-transactions.md
// section 7).
func ReadOnlyServer() *Error {
	return newError(1290, "HY000", "The MySQL server is running with the --read-only option so it cannot execute this statement")
}

// WrongArguments is 1210: a statement's arguments cannot be used, e.g. a prepared
// statement's parameter value (ADR 0009; oracle: pending).
func WrongArguments(what string) *Error {
	return newError(1210, "HY000", "Incorrect arguments to %s", what)
}

// WrongParamCount is 1582: a native function called with the wrong number of
// arguments. name is as written.
func WrongParamCount(name string) *Error {
	return newError(1582, "42000", "Incorrect parameter count in the call to native function '%s'", name)
}

// BigintOutOfRange is 1690: integer arithmetic left the 64-bit range. expr is the
// expression as the select list names it; MySQL prints it normalized and qualified
// (`oracle: pending`).
func BigintOutOfRange(unsigned bool, expr string) *Error {
	kind := "BIGINT"
	if unsigned {
		kind = "BIGINT UNSIGNED"
	}
	return newError(1690, "22003", "%s value is out of range in '%s'", kind, expr)
}

// DivisionByZero is 1365, raised in strict mode with ERROR_FOR_DIVISION_BY_ZERO when an
// INSERT or UPDATE divides by zero.
func DivisionByZero() *Error {
	return newError(1365, "22012", "Division by 0")
}

// functionMarker prefixes the message of an error a mysqlite SQL function raises: SQLite
// keeps only the message of a function's error, so the MySQL code travels in it.
const functionMarker = "mysqlite-error:"

// FunctionError is the error a SQL function returns to raise e. Only the errors
// fromFunction knows survive the trip through SQLite.
func FunctionError(e *Error) error {
	return fmt.Errorf("%s%d", functionMarker, e.Code)
}

// FunctionUnsupported is the error a SQL function returns to refuse, with 1235, a value
// whose MySQL result mysqlite does not implement.
func FunctionUnsupported(construct string) error {
	return fmt.Errorf("%s1235:%s", functionMarker, construct)
}

// fromFunction recovers the error FunctionError encoded in a SQLite message.
func fromFunction(msg string) *Error {
	i := strings.Index(msg, functionMarker)
	if i < 0 {
		return nil
	}
	code := msg[i+len(functionMarker):]
	if construct, ok := strings.CutPrefix(code, "1235:"); ok {
		return Unsupported(construct, "MySQL's result for this value is not implemented")
	}
	if j := strings.IndexFunc(code, func(r rune) bool { return r < '0' || r > '9' }); j >= 0 {
		code = code[:j]
	}
	switch code {
	case "1365":
		return DivisionByZero()
	case "1242":
		return SubqueryRows()
	}
	return Internal()
}

// OutOfRange is 1264 in strict mode.
func OutOfRange(column string, row int) *Error {
	return newError(1264, "22003", "Out of range value for column '%s' at row %d", column, row)
}

// IncorrectString is 1366 in strict mode: value is not valid UTF-8. The message shows
// the bytes from the first invalid one, at most six, as MySQL does (`oracle: pending`).
func IncorrectString(value, column string, row int) *Error {
	i := 0
	for i < len(value) {
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size <= 1 {
			break
		}
		i += size
	}
	var b strings.Builder
	for j := i; j < len(value) && j < i+6; j++ {
		fmt.Fprintf(&b, `\x%02X`, value[j])
	}
	if len(value)-i > 6 {
		b.WriteString("...")
	}
	return newError(1366, "HY000", "Incorrect string value: '%s' for column '%s' at row %d", b.String(), column, row)
}

// IncorrectTemporal is 1292 in strict mode: value is not a valid kind (date,
// datetime) for column.
func IncorrectTemporal(kind, value, column string, row int) *Error {
	return newError(1292, "22007", "Incorrect %s value: '%s' for column '%s' at row %d", kind, value, column, row)
}

// DataTooLong is 1406 in strict mode.
func DataTooLong(column string, row int) *Error {
	return newError(1406, "22001", "Data too long for column '%s' at row %d", column, row)
}

// TableExists is 1050.
func TableExists(table string) *Error {
	return newError(1050, "42S01", "Table '%s' already exists", table)
}

// DuplicateColumn is 1060.
func DuplicateColumn(column string) *Error {
	return newError(1060, "42S21", "Duplicate column name '%s'", column)
}

// DuplicateKeyName is 1061.
func DuplicateKeyName(key string) *Error {
	return newError(1061, "42000", "Duplicate key name '%s'", key)
}

// WrongColumnSpec is 1063: AUTO_INCREMENT on a column that cannot have it.
func WrongColumnSpec(column string) *Error {
	return newError(1063, "42000", "Incorrect column specifier for column '%s'", column)
}

// InvalidDefault is 1067.
func InvalidDefault(column string) *Error {
	return newError(1067, "42000", "Invalid default value for '%s'", column)
}

// MultiplePrimaryKey is 1068.
func MultiplePrimaryKey() *Error {
	return newError(1068, "42000", "Multiple primary key defined")
}

// DuplicateEntry is 1062; key is table.key as MySQL 8.0 prints it.
func DuplicateEntry(entry, key string) *Error {
	return newError(1062, "23000", "Duplicate entry '%s' for key '%s'", entry, key)
}

// CantDropKey is 1091, DROP INDEX of a key the table does not have.
func CantDropKey(key string) *Error {
	return newError(1091, "42000", "Can't DROP '%s'; check that column/key exists", key)
}

// CantRemoveAllFields is 1090, ALTER TABLE dropping every column.
func CantRemoveAllFields() *Error {
	return newError(1090, "42000", "You can't delete all columns with ALTER TABLE; use DROP TABLE instead")
}

// InvalidUseOfNull is 1138, ALTER TABLE making a column that holds NULLs NOT NULL.
func InvalidUseOfNull() *Error {
	return newError(1138, "22004", "Invalid use of NULL value")
}

// KeyDoesNotExist is 1176, RENAME INDEX of a key the table does not have.
func KeyDoesNotExist(key, table string) *Error {
	return newError(1176, "42000", "Key '%s' doesn't exist in table '%s'", key, table)
}

// KeyColumnMissing is 1072.
func KeyColumnMissing(column string) *Error {
	return newError(1072, "42000", "Key column '%s' doesn't exist in table", column)
}

// ColumnTooLong is 1074.
func ColumnTooLong(column string, max int) *Error {
	return newError(1074, "42000", "Column length too big for column '%s' (max = %d); use BLOB or TEXT instead", column, max)
}

// WrongIndexName is 1280.
func WrongIndexName(name string) *Error {
	return newError(1280, "42000", "Incorrect index name '%s'", name)
}

// PrimaryKeyNull is 1171.
func PrimaryKeyNull() *Error {
	return newError(1171, "42000", "All parts of a PRIMARY KEY must be NOT NULL; if you need NULL in a key, use UNIQUE instead")
}

// BlobDefault is 1101.
func BlobDefault(column string) *Error {
	return newError(1101, "42000", "BLOB, TEXT, GEOMETRY or JSON column '%s' can't have a default value", column)
}

// WrongAutoKey is 1075.
func WrongAutoKey() *Error {
	return newError(1075, "42000", "Incorrect table definition; there can be only one auto column and it must be defined as a key")
}

// BlobKeyWithoutLength is 1170.
func BlobKeyWithoutLength(column string) *Error {
	return newError(1170, "42000", "BLOB/TEXT column '%s' used in key specification without a key length", column)
}

// UnknownStorageEngine is 1286.
func UnknownStorageEngine(name string) *Error {
	return newError(1286, "42000", "Unknown storage engine '%s'", name)
}

// TransactionInProgress is 1568: SET TRANSACTION without a scope inside a transaction.
func TransactionInProgress() *Error {
	return newError(1568, "25001", "Transaction characteristics can't be changed while a transaction is in progress")
}

// ReadOnlyTransaction is 1792: a write inside START TRANSACTION READ ONLY.
func ReadOnlyTransaction() *Error {
	return newError(1792, "25006", "Cannot execute statement in a READ ONLY transaction.")
}

// EmptyQuery is 1065: the query has no statement, only whitespace or comments.
func EmptyQuery() *Error {
	return newError(1065, "42000", "Query was empty")
}

// NoSuchTable is 1146.
func NoSuchTable(database, table string) *Error {
	return newError(1146, "42S02", "Table '%s.%s' doesn't exist", database, table)
}

// NoSuchGrant is 1141, SHOW GRANTS FOR an account that does not exist.
func NoSuchGrant(user, host string) *Error {
	return newError(1141, "42000", "There is no such grant defined for user '%s' on host '%s'", user, host)
}

// UnknownTableIn is 1109, a table information_schema does not have.
func UnknownTableIn(table, database string) *Error {
	return newError(1109, "42S02", "Unknown table '%s' in %s", table, database)
}

// DBAccessDenied is 1044, e.g. a write to information_schema.
func DBAccessDenied(user, host, database string) *Error {
	return newError(1044, "42000", "Access denied for user '%s'@'%s' to database '%s'", user, host, database)
}

// UnknownTable is 1051, for a qualifier that names no table of the statement (t.*).
func UnknownTable(table string) *Error {
	return newError(1051, "42S02", "Unknown table '%s'", table)
}

// InvalidGroupFunc is 1111: an aggregate where none is allowed (WHERE, GROUP BY, inside
// another aggregate, UPDATE).
func InvalidGroupFunc() *Error {
	return newError(1111, "HY000", "Invalid use of group function")
}

// NotInGroupBy is 1055 under ONLY_FULL_GROUP_BY. clause is "SELECT list" or "ORDER BY
// clause"; column is database.table.column.
func NotInGroupBy(n int, clause, column string) *Error {
	return newError(1055, "42000", "Expression #%d of %s is not in GROUP BY clause and contains nonaggregated column '%s' which is not functionally dependent on columns in GROUP BY clause; this is incompatible with sql_mode=only_full_group_by", n, clause, column)
}

// MixedAggregate is 1140 under ONLY_FULL_GROUP_BY: an aggregate query without GROUP BY
// that also selects a plain column.
func MixedAggregate(n int, column string) *Error {
	return newError(1140, "42000", "In aggregated query without GROUP BY, expression #%d of SELECT list contains nonaggregated column '%s'; this is incompatible with sql_mode=only_full_group_by", n, column)
}

// OperandColumns is 1241: a subquery or row with the wrong number of columns.
func OperandColumns(n int) *Error {
	return newError(1241, "21000", "Operand should contain %d column(s)", n)
}

// SubqueryRows is 1242: a scalar subquery that returned more than one row.
func SubqueryRows() *Error {
	return newError(1242, "21000", "Subquery returns more than 1 row")
}

// LimitInSubquery is MySQL's own 1235 for LIMIT in an IN, ALL, ANY or SOME subquery.
func LimitInSubquery() *Error {
	return newError(1235, "42000", "This version of MySQL doesn't yet support 'LIMIT & IN/ALL/ANY/SOME subquery'")
}

// DerivedAlias is 1248: a derived table without an alias.
func DerivedAlias() *Error {
	return newError(1248, "42000", "Every derived table must have its own alias")
}

// TooBigPrecision is 1426: a fractional seconds precision above 6.
func TooBigPrecision(fsp int64, function string) *Error {
	return newError(1426, "42000", "Too-big precision %d specified for '%s'. Maximum is 6.", fsp, function)
}

// UnionColumns is 1222: SELECTs of a UNION with different column counts.
func UnionColumns() *Error {
	return newError(1222, "21000", "The used SELECT statements have a different number of columns")
}

// GlobalOrderTable is 1250: a table-qualified column in a UNION's ORDER BY.
func GlobalOrderTable(table string) *Error {
	return newError(1250, "42000", "Table '%s' from one of the SELECTs cannot be used in global ORDER clause", table)
}

// AmbiguousColumn is 1052: an unqualified column that more than one table has.
func AmbiguousColumn(column, clause string) *Error {
	return newError(1052, "23000", "Column '%s' in %s is ambiguous", column, clause)
}

// NonUniqueTable is 1066: two tables of a FROM clause with the same name or alias.
func NonUniqueTable(table string) *Error {
	return newError(1066, "42000", "Not unique table/alias: '%s'", table)
}

// UnknownColumn is 1054. clause is "field list", "where clause", ...
func UnknownColumn(column, clause string) *Error {
	return newError(1054, "42S22", "Unknown column '%s' in '%s'", column, clause)
}

// UnknownCommand is 1047.
func UnknownCommand() *Error {
	return newError(1047, "08S01", "Unknown command")
}

// AccessDenied is 1045. host is the client host without the port, as MySQL prints it.
func AccessDenied(user, host string, usingPassword bool) *Error {
	using := "NO"
	if usingPassword {
		using = "YES"
	}
	return newError(1045, "28000", "Access denied for user '%s'@'%s' (using password: %s)", user, host, using)
}

// CommandDenied is 1227.
func CommandDenied(privilege string) *Error {
	return newError(1227, "42000", "Access denied; you need (at least one of) the %s privilege(s) for this operation", privilege)
}

// ColumnCount is 1136: an INSERT row has the wrong number of values.
func ColumnCount(row int) *Error {
	return newError(1136, "21S01", "Column count doesn't match value count at row %d", row)
}

// MalformedPacket is 1835: a command packet that cannot be parsed.
func MalformedPacket() *Error {
	return newError(1835, "08S01", "Malformed communication packet.")
}

// UnknownStmtHandler is 1243: a prepared statement that was closed or released.
func UnknownStmtHandler(id uint32, command string) *Error {
	return newError(1243, "HY000", "Unknown prepared statement handler (%d) given to %s", id, command)
}

// LockWaitTimeout is 1205.
func LockWaitTimeout() *Error {
	return newError(1205, "HY000", "Lock wait timeout exceeded; try restarting transaction")
}

// Internal is 1105. The caller logs the original error; its text is not sent.
func Internal() *Error {
	return newError(1105, "HY000", "Unknown error")
}

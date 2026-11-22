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
	case errors.Is(err, context.DeadlineExceeded):
		// Waiting for the single writer timed out (docs/06-transactions.md section 4).
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
		return newError(1062, "23000", "Duplicate entry '%s' for key '%s.%s'", entry, table, key)
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
		return newError(3819, "HY000", "Check constraint '%s' is violated.", m[1])
	case sqliteConstraintFK:
		return foreignKey(h)
	}
	switch code & 0xff { // primary result code
	case sqliteBusy, sqliteLocked:
		return LockWaitTimeout()
	case sqliteError:
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

// UnknownTable is 1051, for a qualifier that names no table of the statement (t.*).
func UnknownTable(table string) *Error {
	return newError(1051, "42S02", "Unknown table '%s'", table)
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

// LockWaitTimeout is 1205.
func LockWaitTimeout() *Error {
	return newError(1205, "HY000", "Lock wait timeout exceeded; try restarting transaction")
}

// Internal is 1105. The caller logs the original error; its text is not sent.
func Internal() *Error {
	return newError(1105, "HY000", "Unknown error")
}

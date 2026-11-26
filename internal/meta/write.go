package meta

import (
	"regexp"
	"strconv"
	"strings"

	"mysqlite/internal/errs"
)

// InsertInfo is what CheckInsert learned about an INSERT.
type InsertInfo struct {
	// Auto is the AUTO_INCREMENT column, or "".
	Auto string
	// Generated counts the rows whose AUTO_INCREMENT value is generated (omitted or NULL).
	Generated int
	// Explicit lists the AUTO_INCREMENT values given, in row order.
	Explicit []int64
	// Values holds, per row, the literal of every column the INSERT names, keyed by the
	// lower-case stored column name. A value that is not a literal is present with nil;
	// an omitted column is absent.
	Values []map[string]*Literal
}

// CheckInsert validates an INSERT against the table before it runs, in MySQL's order:
// unknown or repeated columns, row width, columns without a default, then each row.
// columns is nil when the statement has no column list; a single empty row is
// VALUES (). A row value is nil when it is not a literal.
//
// SQLite would accept some of these statements with a different result: it generates a
// rowid for NULL in any INTEGER PRIMARY KEY and stores 0 in an AUTOINCREMENT column,
// where MySQL generates a value.
func CheckInsert(t *Table, columns []string, rows [][]*Literal) (InsertInfo, *errs.Error) {
	return CheckInsertMode(t, columns, rows, InsertMode{})
}

// InsertMode carries the sql_mode flags that change how an INSERT is checked.
type InsertMode struct {
	// NoAutoValueOnZero stores 0 in the AUTO_INCREMENT column instead of refusing it.
	NoAutoValueOnZero bool
	// Strict is STRICT_TRANS_TABLES or STRICT_ALL_TABLES; with it, temporal literals are
	// checked under NoZeroDate, NoZeroInDate and AllowInvalidDates (checkTemporal).
	Strict, NoZeroDate, NoZeroInDate, AllowInvalidDates bool
}

// CheckInsertMode is CheckInsert under the given sql_mode flags.
func CheckInsertMode(t *Table, columns []string, rows [][]*Literal, mode InsertMode) (InsertInfo, *errs.Error) {
	info := InsertInfo{Auto: t.AutoIncrement}
	defaults := len(columns) == 0 && len(rows) == 1 && len(rows[0]) == 0

	// idx[i] is the table column of value i.
	var idx []int
	if len(columns) == 0 {
		if !defaults {
			for i := range t.Columns {
				idx = append(idx, i)
			}
		}
	} else {
		seen := map[int]bool{}
		for _, name := range columns {
			c := t.columnIndex(name)
			if c < 0 {
				return info, errs.UnknownColumn(name, "field list")
			}
			if seen[c] {
				return info, errs.FieldSpecifiedTwice(name)
			}
			seen[c] = true
			idx = append(idx, c)
		}
	}
	for i, row := range rows {
		if !defaults && len(row) != len(idx) {
			return info, errs.ColumnCount(i + 1)
		}
	}

	given := map[int]bool{}
	for _, c := range idx {
		given[c] = true
	}
	for c, col := range t.Columns {
		if !given[c] && t.requiresValue(col) {
			return info, errs.NoDefaultValue(col.Name)
		}
	}

	for r, row := range rows {
		values := map[string]*Literal{}
		generated := true
		for j, v := range row {
			col := t.Columns[idx[j]]
			if e := LimitsOf(col.MySQLType()).CheckLiteral(col.Name, v, r+1); e != nil {
				return info, e
			}
			ti, _ := ParseType(col.MySQLType())
			if e := checkTemporal(ti.Type, col.Name, v, r+1, mode); e != nil {
				return info, e
			}
			values[strings.ToLower(col.Name)] = v
			if strings.EqualFold(col.Name, t.AutoIncrement) {
				n, gen, err := autoValue(v, mode.NoAutoValueOnZero)
				if err != nil {
					return info, err
				}
				if !gen {
					generated = false
					info.Explicit = append(info.Explicit, n)
				}
				continue
			}
			if v != nil && v.Kind == LiteralNull && t.isKey(col.Name) {
				// SQLite would turn NULL in a rowid key into a new rowid.
				return info, errs.BadNull(col.Name)
			}
		}
		if t.AutoIncrement != "" && generated {
			info.Generated++
		}
		info.Values = append(info.Values, values)
	}
	return info, nil
}

// autoValue reads a value given for the AUTO_INCREMENT column: NULL generates, a
// non-zero integer is stored as is, and so is 0 under NO_AUTO_VALUE_ON_ZERO. A string
// of decimal digits counts as its integer, as MySQL converts it and SQLite's INTEGER
// affinity stores it as one: clients that bind every parameter as a string (PHP PDO's
// execute(array)) send keys that way.
func autoValue(v *Literal, zeroIsValue bool) (n int64, generated bool, err *errs.Error) {
	if v != nil && v.Kind == LiteralNull {
		return 0, true, nil
	}
	if v == nil || (v.Kind != LiteralInt && !(v.Kind == LiteralString && integerText.MatchString(v.Text))) {
		return 0, false, errs.Unsupported("non-integer value for an AUTO_INCREMENT column", "only integers and strings of decimal digits are converted")
	}
	n, perr := strconv.ParseInt(v.Text, 10, 64)
	if perr != nil {
		return 0, false, errs.Unsupported("non-integer value for an AUTO_INCREMENT column", "only integers and strings of decimal digits are converted")
	}
	if n == 0 && !zeroIsValue {
		return 0, false, errs.Unsupported("value 0 for an AUTO_INCREMENT column",
			"MySQL generates a value, SQLite would store 0; use NULL, or SET sql_mode with NO_AUTO_VALUE_ON_ZERO to store 0")
	}
	return n, false, nil
}

// integerText is a string literal autoValue reads as an integer.
var integerText = regexp.MustCompile(`^-?[0-9]+$`)

// requiresValue reports a column an INSERT must name in strict mode: NOT NULL (primary
// keys are), no default, not AUTO_INCREMENT.
func (t *Table) requiresValue(c Column) bool {
	if strings.EqualFold(c.Name, t.AutoIncrement) || c.HasDefault {
		return false
	}
	return c.NotNull || t.isKey(c.Name)
}

func (t *Table) isKey(name string) bool {
	for _, k := range t.PrimaryKey {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

func (t *Table) columnIndex(name string) int {
	for i, c := range t.Columns {
		if strings.EqualFold(c.Name, name) {
			return i
		}
	}
	return -1
}

// KeyName is the MySQL name of the unique key on cols, for error 1062: PRIMARY, the
// index name from the catalog, or the column names joined by '_' when unknown.
func (t *Table) KeyName(cols []string) string {
	if sameColumns(cols, t.PrimaryKey) {
		return "PRIMARY"
	}
	for _, ix := range t.Indexes {
		if ix.Unique && ix.Name != "" && sameColumns(cols, ix.Columns) {
			return ix.Name
		}
	}
	return strings.Join(cols, "_")
}

func sameColumns(a, b []string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// KeyColumn resolves a CREATE INDEX key column: its stored name, or the error MySQL
// reports for a missing column (1072) or a TEXT/BLOB column without a prefix (1170).
func (t *Table) KeyColumn(name string, prefixed bool) (string, *errs.Error) {
	c, ok := t.column(name)
	if !ok {
		return "", errs.KeyColumnMissing(name)
	}
	ti, ok := ParseType(c.MySQLType())
	switch {
	case ok && ti.Type == TypeBlob && !prefixed:
		return "", errs.BlobKeyWithoutLength(c.Name)
	case ok && ti.Type == TypeJSON:
		return "", errs.Unsupported("JSON key column", "MySQL indexes JSON only through generated columns")
	}
	return c.Name, nil
}

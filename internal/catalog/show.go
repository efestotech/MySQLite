package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"mysqlite/internal/charset"
	"mysqlite/internal/meta"
)

// TableEntry is one table of a database listing.
type TableEntry struct {
	Name string
	// View is true for a view (Table_type VIEW).
	View bool
}

// ListTables lists the tables and views clients can see, by name: SQLite's internal
// tables and mysqlite's own are left out (docs/04-session-catalog.md section 4).
func ListTables(ctx context.Context, conn *sql.Conn) ([]TableEntry, error) {
	rows, err := conn.QueryContext(ctx,
		"SELECT name, type FROM sqlite_schema WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("catalog: list tables: %w", err)
	}
	defer rows.Close()
	var out []TableEntry
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, fmt.Errorf("catalog: list tables: %w", err)
		}
		if Hidden(name) {
			continue
		}
		out = append(out, TableEntry{Name: name, View: typ == "view"})
	}
	return out, rows.Err()
}

// NextAutoIncrement is the value the AUTO_INCREMENT column of t generates next, or 0
// when t has none.
func NextAutoIncrement(ctx context.Context, conn *sql.Conn, t *meta.Table) (int64, error) {
	if t.AutoIncrement == "" {
		return 0, nil
	}
	var seq sql.NullInt64
	err := conn.QueryRowContext(ctx, "SELECT seq FROM sqlite_sequence WHERE name = ?", t.Name).Scan(&seq)
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("catalog: sequence of %s: %w", t.Name, err)
	}
	return seq.Int64 + 1, nil
}

// DefaultValue decodes a SQLite default (meta.Column.Default) to the value MySQL shows:
// the text, or nil for NULL. ok is false for an expression mysqlite did not write.
func DefaultValue(sqlText string) (v *string, ok bool) {
	s := strings.TrimSpace(sqlText)
	switch {
	case strings.EqualFold(s, "NULL"):
		return nil, true
	case len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'':
		text := strings.ReplaceAll(s[1:len(s)-1], "''", "'")
		return &text, true
	}
	for i, c := range s {
		if !(c >= '0' && c <= '9' || c == '.' || (c == '-' || c == '+') && i == 0 || c == 'e' || c == 'E') {
			return nil, false
		}
	}
	return &s, true
}

// ColumnKey is the Key column of SHOW COLUMNS: PRI, UNI, MUL or "", in that priority.
func ColumnKey(t *meta.Table, column string) string {
	for _, k := range t.PrimaryKey {
		if strings.EqualFold(k, column) {
			return "PRI"
		}
	}
	key := ""
	for _, ix := range t.Indexes {
		if !strings.EqualFold(ix.Columns[0], column) {
			continue
		}
		if ix.Unique && len(ix.Columns) == 1 {
			return "UNI"
		}
		key = "MUL"
	}
	return key
}

// ColumnCollation is a character column's collation, "" for other columns.
func ColumnCollation(c meta.Column) string {
	return c.Options[meta.OptionCollation]
}

// TableCollation is the table's default collation.
func TableCollation(t *meta.Table) string {
	if co := t.Options[SidecarKeyCollation]; co != "" {
		return co
	}
	return "utf8mb4_general_ci"
}

// Engine is the ENGINE a table reports: as created, or SQLite for a table mysqlite did
// not create.
func Engine(t *meta.Table) string {
	if e := t.Options[SidecarKeyEngine]; e != "" {
		return e
	}
	return "SQLite"
}

// CreateTableSQL reconstructs the MySQL DDL of t as SHOW CREATE TABLE prints it. It must
// be re-executable by mysqlite (docs/04-session-catalog.md section 4); next is
// NextAutoIncrement.
func CreateTableSQL(t *meta.Table, next int64) string {
	tableCo := TableCollation(t)
	tableCS := charsetOf(tableCo)
	var lines []string
	for _, c := range t.Columns {
		lines = append(lines, "  "+columnDDL(t, c, tableCS, tableCo))
	}
	if len(t.PrimaryKey) > 0 {
		lines = append(lines, "  PRIMARY KEY ("+quoteList(t.PrimaryKey)+")")
	}
	for _, unique := range []bool{true, false} {
		for _, ix := range t.Indexes {
			if ix.Unique != unique {
				continue
			}
			kind := "KEY"
			if unique {
				kind = "UNIQUE KEY"
			}
			lines = append(lines, "  "+kind+" "+quote(ix.Name)+" ("+keyParts(ix)+")")
		}
	}
	var b strings.Builder
	b.WriteString("CREATE TABLE ")
	b.WriteString(quote(t.Name))
	b.WriteString(" (\n")
	b.WriteString(strings.Join(lines, ",\n"))
	b.WriteString("\n) ENGINE=")
	b.WriteString(Engine(t))
	if next > 1 {
		fmt.Fprintf(&b, " AUTO_INCREMENT=%d", next)
	}
	b.WriteString(" DEFAULT CHARSET=")
	b.WriteString(tableCS)
	b.WriteString(" COLLATE=")
	b.WriteString(tableCo)
	if rf := t.Options[SidecarKeyRowFormat]; rf != "" {
		b.WriteString(" ROW_FORMAT=")
		b.WriteString(rf)
	}
	if c, ok := t.Options[SidecarKeyComment]; ok {
		b.WriteString(" COMMENT=")
		b.WriteString(stringLiteral(c))
	}
	return b.String()
}

// mysqlPrimary is MySQL 8.0's default collation of each character set. It decides how
// SHOW CREATE TABLE spells a column; mysqlite's own utf8mb4 default (general_ci, ADR
// 0004) does not change what MySQL would print.
var mysqlPrimary = map[string]string{
	"binary":  "binary",
	"utf8mb3": "utf8mb3_general_ci",
	"utf8mb4": "utf8mb4_0900_ai_ci",
}

func columnDDL(t *meta.Table, c meta.Column, tableCS, tableCo string) string {
	mysqlType := c.MySQLType()
	parts := []string{quote(c.Name), mysqlType}
	if co := ColumnCollation(c); co != "" {
		// MySQL 8.0.46 (harness against mysql-ref, corpus catalog.show-create-table):
		// CHARACTER SET when the column's collation is not the table's, COLLATE when it
		// is not MySQL's primary collation of its character set. So a general_ci column
		// of a general_ci table still says COLLATE utf8mb4_general_ci, because MySQL's
		// utf8mb4 primary is utf8mb4_0900_ai_ci. A primary collation that differs from
		// the table's prints both (oracle probe o.110.1).
		cs := charsetOf(co)
		if co != tableCo {
			parts = append(parts, "CHARACTER SET "+cs)
		}
		// MySQL 8.0.46 names the collation unless it is both the table's and the
		// primary one of its character set.
		if co != mysqlPrimary[cs] || co != tableCo {
			parts = append(parts, "COLLATE "+co)
		}
	}
	auto := strings.EqualFold(c.Name, t.AutoIncrement)
	notNull := c.NotNull || auto || ColumnKey(t, c.Name) == "PRI"
	if notNull {
		parts = append(parts, "NOT NULL")
	} else if strings.HasPrefix(strings.ToLower(mysqlType), "timestamp") {
		// MySQL spells out NULL for a nullable TIMESTAMP, the one type whose columns
		// were NOT NULL by default before explicit_defaults_for_timestamp.
		parts = append(parts, "NULL")
	}
	switch {
	case auto:
		parts = append(parts, "AUTO_INCREMENT")
	case c.Default != nil:
		if v, ok := DefaultValue(*c.Default); ok {
			if v == nil {
				parts = append(parts, "DEFAULT NULL")
			} else {
				parts = append(parts, "DEFAULT "+stringLiteral(*v))
			}
		}
	case !notNull && !noImplicitDefault(mysqlType):
		parts = append(parts, "DEFAULT NULL")
	}
	if comment := c.Options[SidecarKeyComment]; comment != "" {
		parts = append(parts, "COMMENT "+stringLiteral(comment))
	}
	return strings.Join(parts, " ")
}

// noImplicitDefault: MySQL prints no DEFAULT NULL for TEXT, BLOB and JSON columns.
func noImplicitDefault(mysqlType string) bool {
	t := strings.ToLower(mysqlType)
	return strings.HasSuffix(t, "text") || strings.HasSuffix(t, "blob") || t == "json"
}

func charsetOf(collation string) string {
	if co, ok := charset.LookupCollation(collation); ok {
		return co.Charset
	}
	return "utf8mb4"
}

func quote(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// keyParts is the column list of a key, with each column's prefix length: `a`,`b`(191).
func keyParts(ix meta.Index) string {
	q := make([]string, len(ix.Columns))
	for i, n := range ix.Columns {
		q[i] = quote(n)
		if p := ix.Prefix(i); p > 0 {
			q[i] += "(" + strconv.Itoa(p) + ")"
		}
	}
	return strings.Join(q, ",")
}

func quoteList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = quote(n)
	}
	return strings.Join(q, ",")
}

// stringLiteral quotes s for a statement parsed with the default sql_mode.
func stringLiteral(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// Stats are a table's size figures.
type Stats struct{ Rows, Data, Index int64 }

// TableStats counts the rows and measures the table and its indexes with SQLite's
// dbstat table. Rows is exact, where InnoDB's is an estimate.
func TableStats(ctx context.Context, conn *sql.Conn, table string) (Stats, error) {
	var st Stats
	q := "SELECT count(*) FROM " + quoteName(table)
	if err := conn.QueryRowContext(ctx, q).Scan(&st.Rows); err != nil {
		return st, fmt.Errorf("catalog: count rows of %s: %w", table, err)
	}
	err := conn.QueryRowContext(ctx,
		"SELECT coalesce(sum(pgsize), 0) FROM dbstat WHERE name = ?", table).Scan(&st.Data)
	if err != nil {
		return st, fmt.Errorf("catalog: size of %s: %w", table, err)
	}
	err = conn.QueryRowContext(ctx,
		"SELECT coalesce(sum(pgsize), 0) FROM dbstat WHERE name IN (SELECT name FROM sqlite_schema WHERE type = 'index' AND tbl_name = ?)",
		table).Scan(&st.Index)
	if err != nil {
		return st, fmt.Errorf("catalog: index size of %s: %w", table, err)
	}
	return st, nil
}

func quoteName(name string) string {
	return "\"" + strings.ReplaceAll(name, "\"", "\"\"") + "\""
}

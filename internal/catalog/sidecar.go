package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// The sidecar table keeps the MySQL metadata SQLite cannot represent
// (docs/04-session-catalog.md section 3): table options, column types SQLite's type
// names cannot spell (ENUM), comments, collations, and the MySQL names of indexes.
// It lives in the same database file and is hidden from SHOW, information_schema and
// queries.
//
// One row per fact:
//
//	tbl     the table name ('' for database-level facts, e.g. the format version)
//	col     '' for a table-level fact, the column name for a column fact, or
//	        'index:<sqlite index name>' for an index fact
//	key     the fact (SidecarKey* constants)
//	value   the value, as MySQL would print it
//
// Rule: every write to the sidecar happens in the same transaction as a DDL statement on
// the same table, so the schema_version change that invalidates the catalog cache also
// covers the sidecar change.
const (
	SidecarTable = "__mysqlite_meta"
	// HiddenPrefix marks mysqlite's own tables; they never appear in listings.
	HiddenPrefix = "__mysqlite_"

	sidecarFormat = "1"

	sidecarDDL = `CREATE TABLE IF NOT EXISTS ` + SidecarTable + ` (
	tbl   TEXT NOT NULL COLLATE NOCASE,
	col   TEXT NOT NULL COLLATE NOCASE,
	key   TEXT NOT NULL,
	value TEXT NOT NULL,
	PRIMARY KEY (tbl, col, key)
) WITHOUT ROWID`
)

// Sidecar keys.
const (
	// Database level (tbl = '', col = '').
	SidecarKeyFormat = "format"
	// Table level: ENGINE, DEFAULT COLLATE, COMMENT, AUTO_INCREMENT=n as written,
	// ROW_FORMAT.
	SidecarKeyEngine        = "engine"
	SidecarKeyCollation     = "collation"
	SidecarKeyComment       = "comment"
	SidecarKeyAutoIncrement = "auto_increment"
	SidecarKeyRowFormat     = "row_format"
	// Column level: the MySQL column type when the SQLite declared type cannot spell
	// it, COLLATE, COMMENT.
	SidecarKeyType = "type"
	// Index level: the MySQL key name.
	SidecarKeyName = "name"

	sidecarIndexPrefix = "index:"
)

// SidecarFact is one row to write.
type SidecarFact struct {
	Column, Key, Value string
}

// SidecarIndex is the col value of an index fact.
func SidecarIndex(sqliteName string) string { return sidecarIndexPrefix + sqliteName }

// Hidden reports one of mysqlite's own tables, or one of SQLite's (sqlite_sequence,
// sqlite_stat1, ...): SQLite reserves the sqlite_ prefix, so no user table has it.
// Hidden tables do not exist for MySQL clients: listings skip them and statements
// that name them get 1146.
func Hidden(table string) bool {
	t := strings.ToLower(table)
	return strings.HasPrefix(t, HiddenPrefix) || strings.HasPrefix(t, "sqlite_")
}

// WriteSidecar creates the sidecar table if needed and replaces the facts of table. It
// must run in the transaction of the DDL that creates or changes table.
func WriteSidecar(ctx context.Context, conn *sql.Conn, table string, facts []SidecarFact) error {
	if _, err := conn.ExecContext(ctx, sidecarDDL); err != nil {
		return fmt.Errorf("catalog: create sidecar: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		"INSERT OR IGNORE INTO "+SidecarTable+" (tbl, col, key, value) VALUES ('', '', ?, ?)",
		SidecarKeyFormat, sidecarFormat); err != nil {
		return fmt.Errorf("catalog: sidecar format: %w", err)
	}
	if err := DropSidecar(ctx, conn, table); err != nil {
		return err
	}
	for _, f := range facts {
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO "+SidecarTable+" (tbl, col, key, value) VALUES (?, ?, ?, ?)",
			table, f.Column, f.Key, f.Value); err != nil {
			return fmt.Errorf("catalog: sidecar %s.%s %s: %w", table, f.Column, f.Key, err)
		}
	}
	return nil
}

// DropSidecar removes the facts of table, when the sidecar exists.
func DropSidecar(ctx context.Context, conn *sql.Conn, table string) error {
	ok, err := hasSidecar(ctx, conn)
	if err != nil || !ok {
		return err
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM "+SidecarTable+" WHERE tbl = ?", table); err != nil {
		return fmt.Errorf("catalog: sidecar of %s: %w", table, err)
	}
	return nil
}

func hasSidecar(ctx context.Context, conn *sql.Conn) (bool, error) {
	var n int
	err := conn.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?", SidecarTable).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("catalog: look for the sidecar: %w", err)
	}
	return n > 0, nil
}

// sidecar is the metadata of one table.
type sidecar struct {
	table   map[string]string
	columns map[string]map[string]string // by lower-case column name
	indexes map[string]string            // MySQL name by lower-case SQLite index name
}

func readSidecar(ctx context.Context, conn *sql.Conn, table string) (*sidecar, error) {
	ok, err := hasSidecar(ctx, conn)
	if err != nil || !ok {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, "SELECT col, key, value FROM "+SidecarTable+" WHERE tbl = ?", table)
	if err != nil {
		return nil, fmt.Errorf("catalog: sidecar of %s: %w", table, err)
	}
	defer rows.Close()
	var sc *sidecar
	for rows.Next() {
		var col, key, value string
		if err := rows.Scan(&col, &key, &value); err != nil {
			return nil, fmt.Errorf("catalog: sidecar of %s: %w", table, err)
		}
		if sc == nil {
			sc = &sidecar{table: map[string]string{}, columns: map[string]map[string]string{}, indexes: map[string]string{}}
		}
		switch {
		case col == "":
			sc.table[key] = value
		case strings.HasPrefix(col, sidecarIndexPrefix):
			if key == SidecarKeyName {
				sc.indexes[strings.ToLower(strings.TrimPrefix(col, sidecarIndexPrefix))] = value
			}
		default:
			lc := strings.ToLower(col)
			if sc.columns[lc] == nil {
				sc.columns[lc] = map[string]string{}
			}
			sc.columns[lc][key] = value
		}
	}
	return sc, rows.Err()
}

// SetSidecarFact creates the sidecar table if needed and replaces one fact of table. It
// must run in the transaction of the DDL that changes table.
func SetSidecarFact(ctx context.Context, conn *sql.Conn, table string, f SidecarFact) error {
	if _, err := conn.ExecContext(ctx, sidecarDDL); err != nil {
		return fmt.Errorf("catalog: create sidecar: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		"INSERT OR IGNORE INTO "+SidecarTable+" (tbl, col, key, value) VALUES ('', '', ?, ?)",
		SidecarKeyFormat, sidecarFormat); err != nil {
		return fmt.Errorf("catalog: sidecar format: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		"INSERT OR REPLACE INTO "+SidecarTable+" (tbl, col, key, value) VALUES (?, ?, ?, ?)",
		table, f.Column, f.Key, f.Value); err != nil {
		return fmt.Errorf("catalog: sidecar %s.%s %s: %w", table, f.Column, f.Key, err)
	}
	return nil
}

// DropSidecarColumn removes the facts of one column or index (col) of table, when the
// sidecar exists.
func DropSidecarColumn(ctx context.Context, conn *sql.Conn, table, col string) error {
	ok, err := hasSidecar(ctx, conn)
	if err != nil || !ok {
		return err
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM "+SidecarTable+" WHERE tbl = ? COLLATE NOCASE AND col = ? COLLATE NOCASE", table, col); err != nil {
		return fmt.Errorf("catalog: sidecar of %s.%s: %w", table, col, err)
	}
	return nil
}

// RenameSidecar moves the facts of table from to table to, when the sidecar exists.
func RenameSidecar(ctx context.Context, conn *sql.Conn, from, to string) error {
	ok, err := hasSidecar(ctx, conn)
	if err != nil || !ok {
		return err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE "+SidecarTable+" SET tbl = ? WHERE tbl = ? COLLATE NOCASE", to, from); err != nil {
		return fmt.Errorf("catalog: sidecar of %s: %w", from, err)
	}
	return nil
}

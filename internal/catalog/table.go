// Package catalog reads MySQL-shaped table metadata from SQLite.
//
// ReadTable reads one table from SQLite and its sidecar metadata; Cache keeps the
// results per database until the next DDL (docs/04-session-catalog.md section 3).
package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// ReadTable describes one table of the database conn is connected to. A missing table
// is error 1146 for database.table.
func ReadTable(ctx context.Context, conn *sql.Conn, database, table string) (*meta.Table, error) {
	if Hidden(table) {
		return nil, errs.NoSuchTable(database, table)
	}
	// SQLite resolves table names case-insensitively; report the stored name.
	var name string
	var ddl sql.NullString
	err := conn.QueryRowContext(ctx,
		"SELECT name, sql FROM sqlite_schema WHERE type = 'table' AND name = ? COLLATE NOCASE", table).
		Scan(&name, &ddl)
	if err == sql.ErrNoRows {
		return nil, errs.NoSuchTable(database, table)
	}
	if err != nil {
		return nil, fmt.Errorf("catalog: read table %s: %w", table, err)
	}

	sc, err := readSidecar(ctx, conn, name)
	if err != nil {
		return nil, err
	}
	t := &meta.Table{Database: database, Name: name}
	if err := readColumns(ctx, conn, t); err != nil {
		return nil, err
	}
	if err := readIndexes(ctx, conn, t, sc); err != nil {
		return nil, err
	}
	if sc != nil {
		if len(sc.table) > 0 {
			t.Options = sc.table
		}
		for i := range t.Columns {
			t.Columns[i].Options = sc.columns[strings.ToLower(t.Columns[i].Name)]
		}
	}
	// SQLite allows AUTOINCREMENT only on an INTEGER PRIMARY KEY, a single column.
	if len(t.PrimaryKey) == 1 && hasAutoIncrement(ddl.String) {
		t.AutoIncrement = t.PrimaryKey[0]
	}
	return t, nil
}

func readColumns(ctx context.Context, conn *sql.Conn, t *meta.Table) error {
	rows, err := conn.QueryContext(ctx,
		`SELECT name, type, "notnull", dflt_value, pk, hidden FROM pragma_table_xinfo(?) ORDER BY cid`, t.Name)
	if err != nil {
		return fmt.Errorf("catalog: columns of %s: %w", t.Name, err)
	}
	defer rows.Close()
	type pkPart struct {
		pos  int
		name string
	}
	var pk []pkPart
	for rows.Next() {
		var c meta.Column
		var pos, hidden int
		var dflt sql.NullString
		if err := rows.Scan(&c.Name, &c.DeclType, &c.NotNull, &dflt, &pos, &hidden); err != nil {
			return fmt.Errorf("catalog: columns of %s: %w", t.Name, err)
		}
		if dflt.Valid {
			c.HasDefault = true
			c.Default = &dflt.String
		}
		// hidden 1 is a virtual-table hidden column; 2 and 3 are generated columns,
		// which MySQL shows.
		if hidden == 1 {
			continue
		}
		t.Columns = append(t.Columns, c)
		if pos > 0 {
			pk = append(pk, pkPart{pos, c.Name})
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("catalog: columns of %s: %w", t.Name, err)
	}
	// pk is the 1-based position in the key, not the column order.
	for pos := 1; pos <= len(pk); pos++ {
		for _, p := range pk {
			if p.pos == pos {
				t.PrimaryKey = append(t.PrimaryKey, p.name)
			}
		}
	}
	return nil
}

func readIndexes(ctx context.Context, conn *sql.Conn, t *meta.Table, sc *sidecar) error {
	// pragma_index_list does not return creation order; sqlite_schema rowids do. The
	// primary-key index (origin 'pk') is described by PrimaryKey instead.
	rows, err := conn.QueryContext(ctx, `
		SELECT il.name, il."unique", il.partial, il.origin
		FROM pragma_index_list(?) AS il JOIN sqlite_schema AS s ON s.name = il.name
		WHERE il.origin <> 'pk'
		ORDER BY s.rowid`, t.Name)
	if err != nil {
		return fmt.Errorf("catalog: indexes of %s: %w", t.Name, err)
	}
	type index struct {
		name            string
		unique, partial bool
		origin          string // 'c' CREATE INDEX, 'u' UNIQUE constraint
	}
	var list []index
	for rows.Next() {
		var ix index
		if err := rows.Scan(&ix.name, &ix.unique, &ix.partial, &ix.origin); err != nil {
			rows.Close()
			return fmt.Errorf("catalog: indexes of %s: %w", t.Name, err)
		}
		list = append(list, ix)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("catalog: indexes of %s: %w", t.Name, err)
	}

	// SQLite names a UNIQUE constraint sqlite_autoindex_<table>_<n>. MySQL names an
	// unnamed key after its first column, adding _2, _3... when the name is taken
	// (sql_table.cc, make_unique_key_name). A constraint name given in the DDL is not
	// kept by SQLite; indexes mysqlite creates carry their MySQL name in the sidecar.
	mysqlName := func(ix index) string {
		if sc != nil {
			if n, ok := sc.indexes[strings.ToLower(ix.name)]; ok {
				return n
			}
		}
		return ix.name
	}
	used := map[string]bool{"primary": true}
	for _, ix := range list {
		if ix.origin != "u" {
			used[strings.ToLower(mysqlName(ix))] = true
		}
	}
	for _, ix := range list {
		cols, ok, err := indexColumns(ctx, conn, ix.name)
		if err != nil {
			return err
		}
		if !ok {
			// An index on an expression has no MySQL column flags.
			continue
		}
		name := mysqlName(ix)
		if ix.origin == "u" && name == ix.name {
			name = uniqueName(cols[0], used)
			used[strings.ToLower(name)] = true
		}
		// A partial unique index does not make the column unique for every row.
		mi := meta.Index{Name: name, Columns: cols, Unique: ix.unique && !ix.partial, SQLiteName: ix.name}
		if sc != nil {
			mi.SubPart = meta.ParseSubPart(sc.subPart[strings.ToLower(ix.name)], len(cols))
		}
		t.Indexes = append(t.Indexes, mi)
	}
	return nil
}

func uniqueName(base string, used map[string]bool) string {
	if !used[strings.ToLower(base)] {
		return base
	}
	for n := 2; ; n++ {
		name := fmt.Sprintf("%s_%d", base, n)
		if !used[strings.ToLower(name)] {
			return name
		}
	}
}

func indexColumns(ctx context.Context, conn *sql.Conn, index string) ([]string, bool, error) {
	rows, err := conn.QueryContext(ctx, "SELECT name FROM pragma_index_info(?) ORDER BY seqno", index)
	if err != nil {
		return nil, false, fmt.Errorf("catalog: index %s: %w", index, err)
	}
	defer rows.Close()
	var cols []string
	ok := true
	for rows.Next() {
		var name sql.NullString
		if err := rows.Scan(&name); err != nil {
			return nil, false, fmt.Errorf("catalog: index %s: %w", index, err)
		}
		if !name.Valid {
			ok = false
		}
		cols = append(cols, name.String)
	}
	return cols, ok, rows.Err()
}

var reAutoIncrement = regexp.MustCompile(`(?i)\bAUTOINCREMENT\b`)

// hasAutoIncrement looks for the AUTOINCREMENT keyword in CREATE TABLE text, outside
// string literals, quoted identifiers and comments. Parsing the full statement is M2.
func hasAutoIncrement(ddl string) bool {
	var b strings.Builder
	for i := 0; i < len(ddl); i++ {
		c := ddl[i]
		switch {
		case c == '\'' || c == '"' || c == '`' || c == '[':
			end := byte(c)
			if c == '[' {
				end = ']'
			}
			for i++; i < len(ddl); i++ {
				if ddl[i] == end {
					if end != ']' && i+1 < len(ddl) && ddl[i+1] == end {
						i++
						continue
					}
					break
				}
			}
			b.WriteByte(' ')
		case c == '-' && i+1 < len(ddl) && ddl[i+1] == '-':
			for i < len(ddl) && ddl[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case c == '/' && i+1 < len(ddl) && ddl[i+1] == '*':
			i += 2
			for i+1 < len(ddl) && !(ddl[i] == '*' && ddl[i+1] == '/') {
				i++
			}
			i++
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return reAutoIncrement.MatchString(b.String())
}

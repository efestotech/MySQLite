// Package infoschema serves the information_schema database (docs/04-session-catalog.md
// section 5, ADR 0005). Each table is defined with MySQL 8.0's columns; when a statement
// reads one, Materialize fills it from the catalog into a fresh in-memory SQLite
// database, and the statement runs there through the ordinary SELECT path, so WHERE,
// ORDER BY and LIMIT behave as on any table. Column lists and types are
// `oracle: pending`.
package infoschema

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"mysqlite/internal/meta"
)

// Name is the database name, as DATABASE() reports it.
const Name = "information_schema"

// Is reports whether db names information_schema; database names are compared
// case-insensitively here, as MySQL does for this one.
func Is(db string) bool { return strings.EqualFold(db, Name) }

// textCollation is the collation of every text column, as in MySQL.
const textCollation = "utf8mb3_general_ci"

type column struct {
	name, typ string
}

type table struct {
	name string
	cols []column
	// rows builds the rows; nil for a table that is always empty.
	rows func(ctx context.Context, src Source) ([][]any, error)
}

var tables = map[string]*table{}

func define(t *table) {
	tables[t.name] = t
}

// Names lists the tables, sorted.
func Names() []string {
	out := make([]string, 0, len(tables))
	for n := range tables {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Lookup describes a table as the catalog describes a stored one; names are
// case-insensitive.
func Lookup(name string) (*meta.Table, bool) {
	t, ok := tables[strings.ToUpper(name)]
	if !ok {
		return nil, false
	}
	return t.meta(), true
}

func (t *table) meta() *meta.Table {
	mt := &meta.Table{Database: Name, Name: t.name, Options: map[string]string{}}
	for _, c := range t.cols {
		col := meta.Column{Name: c.name, DeclType: c.typ, Options: map[string]string{meta.OptionType: c.typ}}
		if isText(c.typ) {
			col.Options[meta.OptionCollation] = textCollation
		}
		mt.Columns = append(mt.Columns, col)
	}
	return mt
}

func isText(typ string) bool {
	t := strings.ToLower(typ)
	return strings.HasPrefix(t, "varchar") || strings.HasPrefix(t, "char") || strings.HasPrefix(t, "enum") || strings.HasSuffix(t, "text")
}

// sqliteType is the declared type of a column in the materialized table: text columns
// compare with mysql_ci like MySQL's utf8mb3_general_ci.
func sqliteType(typ string) string {
	t := strings.ToLower(typ)
	switch {
	case isText(typ):
		return "TEXT COLLATE mysql_ci"
	case strings.Contains(t, "int"):
		return "INTEGER"
	}
	return "TEXT"
}

// Materialize creates table name in conn, an empty in-memory database, and fills it.
// The caller has checked the name with Lookup.
func Materialize(ctx context.Context, conn *sql.Conn, name string, src Source) error {
	t, ok := tables[strings.ToUpper(name)]
	if !ok {
		return fmt.Errorf("infoschema: no table %s", name)
	}
	var b strings.Builder
	b.WriteString("CREATE TABLE ")
	b.WriteString(quote(t.name))
	b.WriteString(" (")
	for i, c := range t.cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(quote(c.name))
		b.WriteString(" ")
		b.WriteString(sqliteType(c.typ))
	}
	b.WriteString(")")
	if _, err := conn.ExecContext(ctx, b.String()); err != nil {
		return fmt.Errorf("infoschema: create %s: %w", t.name, err)
	}
	if t.rows == nil {
		return nil
	}
	rows, err := t.rows(ctx, src)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	insert := "INSERT INTO " + quote(t.name) + " VALUES (" + strings.TrimSuffix(strings.Repeat("?, ", len(t.cols)), ", ") + ")"
	stmt, err := conn.PrepareContext(ctx, insert)
	if err != nil {
		return fmt.Errorf("infoschema: fill %s: %w", t.name, err)
	}
	defer stmt.Close()
	for _, r := range rows {
		if len(r) != len(t.cols) {
			return fmt.Errorf("infoschema: %s row has %d values for %d columns", t.name, len(r), len(t.cols))
		}
		if _, err := stmt.ExecContext(ctx, r...); err != nil {
			return fmt.Errorf("infoschema: fill %s: %w", t.name, err)
		}
	}
	return nil
}

func quote(name string) string {
	return "\"" + strings.ReplaceAll(name, "\"", "\"\"") + "\""
}

// Source is what the tables are built from: the databases, their catalogs, the
// connections and the current user.
type Source interface {
	// Databases lists the user databases, sorted.
	Databases() ([]string, error)
	// Read runs f with a connection that sees database db's catalog, and the catalog's
	// description of its tables.
	Read(ctx context.Context, db string, f func(conn *sql.Conn, describe func(table string) (*meta.Table, error)) error) error
	// Processes lists the connections.
	Processes() []Process
	// User is the current user name.
	User() string
}

// Process is one PROCESSLIST row.
type Process struct {
	ID                             uint32
	User, Host, DB, Command, State string
	Time                           int64
	// Info is nil for an idle connection.
	Info any
}

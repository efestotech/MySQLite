package wire

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/catalog"
	"mysqlite/internal/dialect"
	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// alterTable runs ALTER TABLE: the specs in order, in the DDL transaction, each against
// the table as the earlier ones left it. Sidecar-only changes do not move SQLite's
// schema version, so the cached catalog of the database is dropped afterwards.
func (h *handler) alterTable(st *dialect.Statement) (*mysql.Result, error) {
	defer h.catalog.Forget(st.Database)
	var rebuilding, fkOn bool
	prepare := func(ctx context.Context, conn *sql.Conn) (func(), error) {
		t, err := catalog.ReadTable(ctx, conn, st.Database, st.Alter.Table)
		if err != nil {
			return nil, err
		}
		if !mayRebuild(st.Alter, t) {
			return nil, nil
		}
		rebuilding = true
		restore, on, err := h.prepareRebuild(ctx, conn)
		fkOn = on
		return restore, err
	}
	return h.ddlWith(st, prepare, func(ctx context.Context, conn *sql.Conn) (*mysql.Result, error) {
		name := st.Alter.Table
		for _, s := range st.Alter.Specs {
			// Read past the cache: the earlier specs changed the table.
			t, err := catalog.ReadTable(ctx, conn, st.Database, name)
			if err != nil {
				return nil, h.fail(err, st.Hint())
			}
			if err := h.alterSpec(ctx, conn, t, s, rebuilding, fkOn); err != nil {
				return nil, h.fail(err, st.Hint())
			}
			if s.Kind == dialect.AlterRenameTable {
				name = s.NewName
			}
		}
		if len(st.Alter.Specs) == 0 {
			if _, err := catalog.ReadTable(ctx, conn, st.Database, name); err != nil {
				return nil, h.fail(err, st.Hint())
			}
		}
		return &mysql.Result{}, nil
	})
}

func (h *handler) alterSpec(ctx context.Context, conn *sql.Conn, t *meta.Table, s dialect.AlterSpec, rebuilding, fkOn bool) error {
	switch s.Kind {
	case dialect.AlterRebuild:
		return h.rebuild(ctx, conn, t, s.Spec, fkOn)
	case dialect.AlterAddColumn:
		return addColumn(ctx, conn, t, s)
	case dialect.AlterDropColumn:
		if containsFold(t.PrimaryKey, s.Name) {
			if !rebuilding {
				// An earlier spec of the statement put the column in the key.
				return errs.Unsupported("DROP COLUMN of a column an earlier spec added to the primary key", "the statement was not prepared for a rebuild")
			}
			return h.rebuild(ctx, conn, t, s.Spec, fkOn)
		}
		return dropColumn(ctx, conn, t, s.Name)
	case dialect.AlterRenameTable:
		return renameTable(ctx, conn, t, s.NewName)
	case dialect.AlterAddIndex:
		return addIndex(ctx, conn, t, s.Index)
	case dialect.AlterDropIndex:
		return dropIndex(ctx, conn, t, s.Name)
	case dialect.AlterRenameIndex:
		return renameIndex(ctx, conn, t, s.Name, s.NewName)
	case dialect.AlterTableOption:
		return catalog.SetSidecarFact(ctx, conn, t.Name, catalog.SidecarFact{Key: s.Name, Value: s.NewName})
	}
	return fmt.Errorf("wire: ALTER TABLE spec %d", s.Kind)
}

func exec(ctx context.Context, conn *sql.Conn, q string, args ...any) error {
	if _, err := conn.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("wire: %s: %w", q, err)
	}
	return nil
}

func addColumn(ctx context.Context, conn *sql.Conn, t *meta.Table, s dialect.AlterSpec) error {
	if _, ok := t.Column(s.Name); ok {
		return errs.DuplicateColumn(s.Name)
	}
	if err := exec(ctx, conn, "ALTER TABLE "+ident(t.Name)+" ADD COLUMN "+s.ColumnSQL); err != nil {
		return err
	}
	for _, f := range s.Facts {
		if err := catalog.SetSidecarFact(ctx, conn, t.Name, catalog.SidecarFact{Column: f.Column, Key: f.Key, Value: f.Value}); err != nil {
			return err
		}
	}
	return nil
}

// dropColumn drops a column. MySQL removes it from the keys: a key left without columns
// is dropped, the others shrink (a UNIQUE key that then holds duplicates fails the
// statement with 1062). SQLite refuses to drop an indexed column, so mysqlite drops and
// re-creates the indexes first.
func dropColumn(ctx context.Context, conn *sql.Conn, t *meta.Table, name string) error {
	c, ok := t.Column(name)
	switch {
	case !ok:
		return errs.CantDropKey(name)
	case len(t.Columns) == 1:
		return errs.CantRemoveAllFields()
	}
	for _, ix := range t.Indexes {
		var rest []string
		for _, col := range ix.Columns {
			if !strings.EqualFold(col, c.Name) {
				rest = append(rest, col)
			}
		}
		if len(rest) == len(ix.Columns) {
			continue
		}
		if strings.HasPrefix(ix.SQLiteName, "sqlite_autoindex_") {
			return errs.Unsupported("DROP COLUMN of a column in a key declared in the SQLite table", "the key is part of the table; changing it rebuilds the table")
		}
		if err := exec(ctx, conn, "DROP INDEX "+ident(ix.SQLiteName)); err != nil {
			return err
		}
		if len(rest) == 0 {
			if err := catalog.DropSidecarColumn(ctx, conn, t.Name, catalog.SidecarIndex(ix.SQLiteName)); err != nil {
				return err
			}
			continue
		}
		if err := buildIndex(ctx, conn, t, ix.SQLiteName, ix.Name, ix.Unique, rest); err != nil {
			return err
		}
	}
	if err := exec(ctx, conn, "ALTER TABLE "+ident(t.Name)+" DROP COLUMN "+ident(c.Name)); err != nil {
		return err
	}
	return catalog.DropSidecarColumn(ctx, conn, t.Name, c.Name)
}

// renameTable renames a table with its sidecar facts. SQLite moves the rows, the
// indexes and the AUTO_INCREMENT counter; the indexes mysqlite named after the table
// (rule R010) are re-created under the new name, since SQLite index names are unique
// per database and the old name must be free for a new table.
func renameTable(ctx context.Context, conn *sql.Conn, t *meta.Table, to string) error {
	if t.Name == to {
		return nil
	}
	if !strings.EqualFold(t.Name, to) {
		exists, err := tableExists(ctx, conn, to)
		if err != nil {
			return err
		}
		if exists {
			return errs.TableExists(to)
		}
	}
	from := t.Name
	if strings.EqualFold(from, to) {
		// SQLite refuses a rename that changes only the case: go through another name.
		from = catalog.HiddenPrefix + "rename"
		if err := exec(ctx, conn, "ALTER TABLE "+ident(t.Name)+" RENAME TO "+ident(from)); err != nil {
			return err
		}
	}
	if err := exec(ctx, conn, "ALTER TABLE "+ident(from)+" RENAME TO "+ident(to)); err != nil {
		return err
	}
	if err := catalog.RenameSidecar(ctx, conn, t.Name, to); err != nil {
		return err
	}
	renamed := *t
	renamed.Name = to
	for _, ix := range t.Indexes {
		if !strings.EqualFold(ix.SQLiteName, dialect.IndexSQLiteName(t.Name, ix.Name)) {
			continue
		}
		if err := moveIndex(ctx, conn, &renamed, ix, ix.Name); err != nil {
			return err
		}
	}
	return nil
}

// addIndex adds a key. An unnamed key takes its first column's name, then _2, _3...
// as MySQL names it.
func addIndex(ctx context.Context, conn *sql.Conn, t *meta.Table, ic *dialect.IndexChange) error {
	if _, ok := findIndex(t, ic.Name); ok && ic.Name != "" {
		return errs.DuplicateKeyName(ic.Name)
	}
	cols := make([]string, len(ic.Columns))
	for i, c := range ic.Columns {
		name, e := t.KeyColumn(c)
		if e != nil {
			return e
		}
		cols[i] = name
	}
	name := ic.Name
	if name == "" {
		used := map[string]bool{"primary": true}
		for _, ix := range t.Indexes {
			used[strings.ToLower(ix.Name)] = true
		}
		name = cols[0]
		for n := 2; used[strings.ToLower(name)]; n++ {
			name = fmt.Sprintf("%s_%d", cols[0], n)
		}
	}
	return buildIndex(ctx, conn, t, dialect.IndexSQLiteName(t.Name, name), name, ic.Unique, cols)
}

// buildIndex creates an index and its sidecar name. A UNIQUE index over duplicate
// values is 1062 for the first duplicate.
func buildIndex(ctx context.Context, conn *sql.Conn, t *meta.Table, sqliteName, key string, unique bool, cols []string) error {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = ident(c)
	}
	stmt := "CREATE INDEX "
	if unique {
		entry, dup, err := firstDuplicate(ctx, conn, t.Name, quoted)
		if err != nil {
			return err
		}
		if dup {
			return errs.DuplicateEntry(entry, t.Name+"."+key)
		}
		stmt = "CREATE UNIQUE INDEX "
	}
	if err := exec(ctx, conn, stmt+ident(sqliteName)+" ON "+ident(t.Name)+" ("+strings.Join(quoted, ", ")+")"); err != nil {
		return err
	}
	return catalog.SetSidecarFact(ctx, conn, t.Name, catalog.SidecarFact{Column: catalog.SidecarIndex(sqliteName), Key: catalog.SidecarKeyName, Value: key})
}

func dropIndex(ctx context.Context, conn *sql.Conn, t *meta.Table, name string) error {
	ix, ok := findIndex(t, name)
	if !ok {
		return errs.CantDropKey(name)
	}
	if strings.HasPrefix(ix.SQLiteName, "sqlite_autoindex_") {
		return errs.Unsupported("DROP INDEX of a key declared in the SQLite table", "the key is part of the table; changing it rebuilds the table (ALTER TABLE)")
	}
	if err := exec(ctx, conn, "DROP INDEX "+ident(ix.SQLiteName)); err != nil {
		return err
	}
	return catalog.DropSidecarColumn(ctx, conn, t.Name, catalog.SidecarIndex(ix.SQLiteName))
}

func renameIndex(ctx context.Context, conn *sql.Conn, t *meta.Table, from, to string) error {
	ix, ok := findIndex(t, from)
	if !ok {
		return errs.KeyDoesNotExist(from, t.Name)
	}
	if other, ok := findIndex(t, to); ok && other.SQLiteName != ix.SQLiteName {
		return errs.DuplicateKeyName(to)
	}
	if strings.HasPrefix(ix.SQLiteName, "sqlite_autoindex_") {
		return errs.Unsupported("RENAME INDEX of a key declared in the SQLite table", "the key is part of the table; changing it rebuilds the table")
	}
	return moveIndex(ctx, conn, t, ix, to)
}

// moveIndex re-creates an index as <table>__<key> (rule R010) under its new key name:
// SQLite cannot rename an index.
func moveIndex(ctx context.Context, conn *sql.Conn, t *meta.Table, ix meta.Index, key string) error {
	if err := exec(ctx, conn, "DROP INDEX "+ident(ix.SQLiteName)); err != nil {
		return err
	}
	if err := catalog.DropSidecarColumn(ctx, conn, t.Name, catalog.SidecarIndex(ix.SQLiteName)); err != nil {
		return err
	}
	// The rows already satisfy the key, so its duplicate check finds nothing.
	return buildIndex(ctx, conn, t, dialect.IndexSQLiteName(t.Name, key), key, ix.Unique, ix.Columns)
}

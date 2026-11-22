package wire

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/catalog"
	"mysqlite/internal/dialect"
	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// ddl runs one DDL statement on the database's writer in its own transaction: MySQL
// commits the open transaction first, and the statement takes effect whole or not at
// all (MySQL 8.0's DDL is atomic).
func (h *handler) ddl(st *dialect.Statement, run func(ctx context.Context, conn *sql.Conn) (*mysql.Result, error)) (*mysql.Result, error) {
	ctx := context.Background()
	conn, release, err := h.sess.DDLConn(ctx, st.Database)
	if err != nil {
		return nil, h.fail(err, errs.Hint{DatabaseArg: st.Database})
	}
	defer release()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, h.fail(fmt.Errorf("wire: begin DDL: %w", err), st.Hint())
	}
	done := false
	defer func() {
		if !done {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	res, err := run(ctx, conn)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, h.fail(fmt.Errorf("wire: commit DDL: %w", err), st.Hint())
	}
	done = true
	return res, nil
}

// ddlStatement dispatches KindDDL.
func (h *handler) ddlStatement(st *dialect.Statement) (*mysql.Result, error) {
	switch {
	case st.Create != nil:
		return h.createTable(st)
	case st.Drop != nil:
		return h.ddl(st, func(ctx context.Context, conn *sql.Conn) (*mysql.Result, error) {
			return h.dropTable(ctx, conn, st)
		})
	case st.Truncate != "":
		return h.ddl(st, func(ctx context.Context, conn *sql.Conn) (*mysql.Result, error) {
			return h.truncate(ctx, conn, st)
		})
	case st.Index != nil && st.Index.Drop:
		return h.ddl(st, func(ctx context.Context, conn *sql.Conn) (*mysql.Result, error) {
			return h.dropIndex(ctx, conn, st)
		})
	case st.Index != nil:
		return h.ddl(st, func(ctx context.Context, conn *sql.Conn) (*mysql.Result, error) {
			return h.createIndex(ctx, conn, st)
		})
	}
	return nil, h.fail(errors.New("wire: DDL statement without a change"), st.Hint())
}

// dropTable drops every table or, when one is missing and IF EXISTS is absent, none:
// 1051 lists all the missing tables. With IF EXISTS each missing table is a Note.
func (h *handler) dropTable(ctx context.Context, conn *sql.Conn, st *dialect.Statement) (*mysql.Result, error) {
	var stored, missing []string
	for _, name := range st.Drop.Tables {
		t, err := h.catalog.Table(ctx, conn, st.Database, name)
		var e *errs.Error
		if errors.As(err, &e) && e.Code == 1146 {
			missing = append(missing, st.Database+"."+name)
			continue
		}
		if err != nil {
			return nil, h.fail(err, st.Hint())
		}
		stored = append(stored, t.Name)
	}
	if len(missing) > 0 && !st.Drop.IfExists {
		return nil, toMySQL(errs.UnknownTable(strings.Join(missing, ",")))
	}
	for _, m := range missing {
		h.note(errs.UnknownTable(m))
	}
	for _, name := range stored {
		// SQLite drops the table's indexes and its sqlite_sequence row with it.
		if _, err := conn.ExecContext(ctx, "DROP TABLE "+ident(name)); err != nil {
			return nil, h.fail(fmt.Errorf("wire: drop %s: %w", name, err), st.Hint())
		}
		if err := catalog.DropSidecar(ctx, conn, name); err != nil {
			return nil, h.fail(err, st.Hint())
		}
	}
	return &mysql.Result{}, nil
}

// truncate empties the table and restarts its AUTO_INCREMENT counter at 1.
func (h *handler) truncate(ctx context.Context, conn *sql.Conn, st *dialect.Statement) (*mysql.Result, error) {
	t, err := h.catalog.Table(ctx, conn, st.Database, st.Truncate)
	if err != nil {
		return nil, h.fail(err, st.Hint())
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM "+ident(t.Name)); err != nil {
		return nil, h.fail(fmt.Errorf("wire: truncate %s: %w", t.Name, err), st.Hint())
	}
	if t.AutoIncrement != "" {
		// sqlite_sequence exists once a table has AUTOINCREMENT, as this one has.
		if _, err := conn.ExecContext(ctx, "DELETE FROM sqlite_sequence WHERE name = ?", t.Name); err != nil {
			return nil, h.fail(fmt.Errorf("wire: restart AUTO_INCREMENT of %s: %w", t.Name, err), st.Hint())
		}
	}
	return &mysql.Result{}, nil
}

// findIndex finds a table's key by its MySQL name, which is case-insensitive.
func findIndex(t *meta.Table, name string) (meta.Index, bool) {
	for _, ix := range t.Indexes {
		if strings.EqualFold(ix.Name, name) {
			return ix, true
		}
	}
	return meta.Index{}, false
}

func (h *handler) createIndex(ctx context.Context, conn *sql.Conn, st *dialect.Statement) (*mysql.Result, error) {
	ic := st.Index
	t, err := h.catalog.Table(ctx, conn, st.Database, ic.Table)
	if err != nil {
		return nil, h.fail(err, st.Hint())
	}
	if _, ok := findIndex(t, ic.Name); ok {
		return nil, toMySQL(errs.DuplicateKeyName(ic.Name))
	}
	cols := make([]string, len(ic.Columns))
	for i, c := range ic.Columns {
		name, e := t.KeyColumn(c)
		if e != nil {
			return nil, toMySQL(e)
		}
		cols[i] = name
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = ident(c)
	}
	list := strings.Join(quoted, ", ")
	sqliteName := dialect.IndexSQLiteName(t.Name, ic.Name)
	if ic.Unique {
		if entry, ok, err := firstDuplicate(ctx, conn, t.Name, quoted); err != nil {
			return nil, h.fail(err, st.Hint())
		} else if ok {
			return nil, toMySQL(errs.DuplicateEntry(entry, t.Name+"."+ic.Name))
		}
	}
	stmt := "CREATE INDEX "
	if ic.Unique {
		stmt = "CREATE UNIQUE INDEX "
	}
	if _, err := conn.ExecContext(ctx, stmt+ident(sqliteName)+" ON "+ident(t.Name)+" ("+list+")"); err != nil {
		return nil, h.fail(fmt.Errorf("wire: create index %s: %w", sqliteName, err), st.Hint())
	}
	fact := catalog.SidecarFact{Column: catalog.SidecarIndex(sqliteName), Key: catalog.SidecarKeyName, Value: ic.Name}
	if err := catalog.SetSidecarFact(ctx, conn, t.Name, fact); err != nil {
		return nil, h.fail(err, st.Hint())
	}
	return &mysql.Result{}, nil
}

// firstDuplicate finds the smallest key value that two rows share, NULLs excepted (a
// UNIQUE index admits repeated NULLs). Which duplicate MySQL reports while it builds
// the index is `oracle: pending`.
func firstDuplicate(ctx context.Context, conn *sql.Conn, table string, cols []string) (string, bool, error) {
	list := strings.Join(cols, ", ")
	q := "SELECT " + list + " FROM " + ident(table) + " WHERE " + strings.Join(cols, " IS NOT NULL AND ") +
		" IS NOT NULL GROUP BY " + list + " HAVING count(*) > 1 ORDER BY " + list + " LIMIT 1"
	vals := make([]sql.NullString, len(cols))
	dest := make([]any, len(cols))
	for i := range vals {
		dest[i] = &vals[i]
	}
	err := conn.QueryRowContext(ctx, q).Scan(dest...)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("wire: look for duplicates in %s: %w", table, err)
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = v.String
	}
	return entryText(parts), true, nil
}

func (h *handler) dropIndex(ctx context.Context, conn *sql.Conn, st *dialect.Statement) (*mysql.Result, error) {
	ic := st.Index
	t, err := h.catalog.Table(ctx, conn, st.Database, ic.Table)
	if err != nil {
		return nil, h.fail(err, st.Hint())
	}
	ix, ok := findIndex(t, ic.Name)
	if !ok {
		return nil, toMySQL(errs.CantDropKey(ic.Name))
	}
	if strings.HasPrefix(ix.SQLiteName, "sqlite_autoindex_") {
		return nil, toMySQL(errs.Unsupported("DROP INDEX of a key declared in the SQLite table", "the key is part of the table; changing it rebuilds the table (ALTER TABLE)"))
	}
	if _, err := conn.ExecContext(ctx, "DROP INDEX "+ident(ix.SQLiteName)); err != nil {
		return nil, h.fail(fmt.Errorf("wire: drop index %s: %w", ix.SQLiteName, err), st.Hint())
	}
	if err := catalog.DropSidecarColumn(ctx, conn, t.Name, catalog.SidecarIndex(ix.SQLiteName)); err != nil {
		return nil, h.fail(err, st.Hint())
	}
	return &mysql.Result{}, nil
}

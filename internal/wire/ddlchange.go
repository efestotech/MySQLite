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
	"mysqlite/internal/engine"
	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// ddl runs one DDL statement on the database's writer in its own transaction: MySQL
// commits the open transaction first, and the statement takes effect whole or not at
// all (MySQL 8.0's DDL is atomic).
func (h *handler) ddl(st *dialect.Statement, run func(ctx context.Context, conn *sql.Conn) (*mysql.Result, error)) (*mysql.Result, error) {
	return h.ddlWith(st, nil, run)
}

// ddlWith is ddl with prepare run on the writer before the transaction starts; the
// function it returns, if any, runs after the transaction ends, committed or not.
func (h *handler) ddlWith(st *dialect.Statement, prepare func(ctx context.Context, conn *sql.Conn) (func(), error),
	run func(ctx context.Context, conn *sql.Conn) (*mysql.Result, error)) (*mysql.Result, error) {
	ctx := context.Background()
	conn, release, err := h.sess.DDLConn(ctx, st.Database)
	if err != nil {
		return nil, h.fail(err, errs.Hint{DatabaseArg: st.Database})
	}
	defer release()
	if prepare != nil {
		after, err := prepare(ctx, conn)
		if err != nil {
			return nil, h.fail(err, st.Hint())
		}
		if after != nil {
			defer after()
		}
	}
	retries, err := engine.BeginImmediate(ctx, conn)
	h.retries = retries
	if err != nil {
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
	case st.Alter != nil:
		return h.alterTable(st)
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
	t, err := h.catalog.Table(ctx, conn, st.Database, st.Index.Table)
	if err == nil {
		err = addIndex(ctx, conn, t, st.Index)
	}
	if err != nil {
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
	t, err := h.catalog.Table(ctx, conn, st.Database, st.Index.Table)
	if err == nil {
		err = dropIndex(ctx, conn, t, st.Index.Name)
	}
	if err != nil {
		return nil, h.fail(err, st.Hint())
	}
	return &mysql.Result{}, nil
}

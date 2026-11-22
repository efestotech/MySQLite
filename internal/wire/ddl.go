package wire

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/catalog"
	"mysqlite/internal/dialect"
	"mysqlite/internal/errs"
)

// createTable runs a translated CREATE TABLE: the table, its indexes, the sidecar facts
// and the AUTO_INCREMENT start in one transaction on the writer, so a failure leaves
// nothing behind (MySQL's DDL is atomic in 8.0).
func (h *handler) createTable(st *dialect.Statement) (*mysql.Result, error) {
	return h.ddl(st, func(ctx context.Context, conn *sql.Conn) (*mysql.Result, error) {
		return h.createTableIn(ctx, conn, st)
	})
}

func (h *handler) createTableIn(ctx context.Context, conn *sql.Conn, st *dialect.Statement) (*mysql.Result, error) {
	ct := st.Create
	exists, err := tableExists(ctx, conn, ct.Table)
	if err != nil {
		return nil, h.fail(err, st.Hint())
	}
	if exists {
		if !ct.IfNotExists {
			return nil, toMySQL(errs.TableExists(ct.Table))
		}
		h.note(errs.TableExists(ct.Table))
		return &mysql.Result{}, nil
	}
	for _, q := range ct.SQL {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return nil, h.fail(fmt.Errorf("wire: %s: %w", q, err), st.Hint())
		}
	}
	facts := make([]catalog.SidecarFact, len(ct.Facts))
	for i, f := range ct.Facts {
		facts[i] = catalog.SidecarFact{Column: f.Column, Key: f.Key, Value: f.Value}
	}
	if err := catalog.WriteSidecar(ctx, conn, ct.Table, facts); err != nil {
		return nil, h.fail(err, st.Hint())
	}
	if ct.AutoIncrement > 1 {
		// The next generated id is seq+1.
		if _, err := conn.ExecContext(ctx, "INSERT INTO sqlite_sequence (name, seq) VALUES (?, ?)", ct.Table, ct.AutoIncrement-1); err != nil {
			return nil, h.fail(fmt.Errorf("wire: AUTO_INCREMENT start: %w", err), st.Hint())
		}
	}
	return &mysql.Result{}, nil
}

// tableExists looks for a table or view of that name. SQLite names are
// case-insensitive, so 'Items' exists when 'items' does.
func tableExists(ctx context.Context, conn *sql.Conn, name string) (bool, error) {
	var n int
	err := conn.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_schema WHERE type IN ('table', 'view') AND name = ? COLLATE NOCASE", name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("wire: look for table %s: %w", name, err)
	}
	return n > 0, nil
}

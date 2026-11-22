package wire

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"mysqlite/internal/errs"
	"mysqlite/internal/infoschema"
	"mysqlite/internal/meta"
)

// information_schema (ADR 0005): a SELECT on it runs on an ephemeral in-memory database
// holding only the table it reads, filled from the catalog just before.

// infoSchemaConn returns the connection and table description a SELECT on
// information_schema runs with. table is nil for a SELECT without FROM.
func (h *handler) infoSchemaConn(ctx context.Context, name string) (*sql.Conn, *meta.Table, error) {
	var table *meta.Table
	if name != "" {
		t, ok := infoschema.Lookup(name)
		if !ok {
			return nil, nil, toMySQL(errs.UnknownTableIn(name, infoschema.Name))
		}
		table = t
	}
	conn, err := h.eng.Ephemeral(ctx)
	if err != nil {
		return nil, nil, h.fail(err, errs.Hint{})
	}
	if table != nil {
		if err := infoschema.Materialize(ctx, conn, table.Name, infoSource{h}); err != nil {
			_ = conn.Close()
			return nil, nil, h.fail(err, errs.Hint{})
		}
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		_ = conn.Close()
		return nil, nil, h.fail(fmt.Errorf("wire: information_schema: %w", err), errs.Hint{})
	}
	return conn, table, nil
}

// infoSchemaDenied is the error of a write or DDL on information_schema.
func (h *handler) infoSchemaDenied() error {
	user := ""
	if h.conn != nil {
		user = h.conn.GetUser()
	}
	return toMySQL(errs.DBAccessDenied(user, "%", infoschema.Name))
}

// infoSource feeds information_schema from this connection's view of the catalog.
type infoSource struct{ h *handler }

func (s infoSource) Databases() ([]string, error) {
	return s.h.eng.Databases()
}

func (s infoSource) Read(ctx context.Context, db string, f func(*sql.Conn, func(string) (*meta.Table, error)) error) error {
	conn, release, err := s.h.sess.CatalogConn(ctx, db)
	if err != nil {
		return err
	}
	defer release()
	return f(conn, func(table string) (*meta.Table, error) {
		return s.h.catalog.Table(ctx, conn, db, table)
	})
}

func (s infoSource) Processes() []infoschema.Process {
	var out []infoschema.Process
	now := time.Now()
	for _, p := range s.h.procs.snapshot() {
		row := infoschema.Process{
			ID: p.ID, User: p.User, Host: p.Host, DB: p.DB, Command: p.Command,
			Time: int64(now.Sub(p.Since).Seconds()),
		}
		if p.Command == "Query" {
			row.State, row.Info = "executing", p.Info
		}
		out = append(out, row)
	}
	return out
}

func (s infoSource) User() string {
	if s.h.conn == nil {
		return ""
	}
	return s.h.conn.GetUser()
}

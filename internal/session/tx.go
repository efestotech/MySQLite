package session

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"

	"mysqlite/internal/engine"
	"mysqlite/internal/errs"
)

// Transactions (docs/06-transactions.md section 4).
//
// A transaction takes its connection with its first statement, not with BEGIN, so its
// snapshot starts at the first read, as in InnoDB REPEATABLE READ. A read-write
// transaction runs every statement, reads included, on the database's single writer:
// reads must see the transaction's own writes and keep one snapshot, which a separate
// read connection cannot give. It holds the writer until COMMIT or ROLLBACK; the idle
// transaction timeout that bounds this is M5. A read-only transaction uses a read
// connection and never takes the writer (section 6).
//
// SQLite keeps one transaction per database file, so a transaction is bound to the
// database of its first statement; statements on another database are refused until
// cross-database transactions exist.

// reasonOneDatabase explains why a statement is refused inside a transaction.
const reasonOneDatabase = "a transaction spans one database in M1"

// TxOptions describe BEGIN / START TRANSACTION.
type TxOptions struct {
	ReadOnly bool
	// Snapshot is WITH CONSISTENT SNAPSHOT: the snapshot is taken by the statement.
	Snapshot bool
}

type tx struct {
	opts TxOptions
	db   string    // the bound database, "" until the first statement
	conn *sql.Conn // nil until the first statement
}

// Autocommit reports the autocommit mode (SET autocommit).
func (s *Session) Autocommit() bool { return !s.manual }

// SetAutocommit changes the autocommit mode. Turning it on commits the open
// transaction, as MySQL does; turning it off makes the next statement that uses a
// database start a transaction.
func (s *Session) SetAutocommit(ctx context.Context, on bool) error {
	s.manual = !on
	if on {
		return s.Commit(ctx)
	}
	return nil
}

// SetReadOnly is SET [SESSION] transaction_read_only: the access mode of the following
// transactions and of autocommitted statements.
func (s *Session) SetReadOnly(on bool) { s.readOnly = on }

// SetNextReadOnly is SET TRANSACTION READ ONLY | READ WRITE without a scope: it applies
// to the next transaction only.
func (s *Session) SetNextReadOnly(on bool) { s.nextReadOnly = &on }

// DefaultReadOnly is the access mode of a transaction that does not choose one.
func (s *Session) DefaultReadOnly() bool {
	if s.nextReadOnly != nil {
		return *s.nextReadOnly
	}
	return s.readOnly
}

// InTransaction reports an open explicit transaction (SERVER_STATUS_IN_TRANS).
func (s *Session) InTransaction() bool { return s.tx != nil }

// ReadOnlyTransaction reports an open read-only transaction
// (SERVER_STATUS_IN_TRANS_READONLY).
func (s *Session) ReadOnlyTransaction() bool { return s.tx != nil && s.tx.opts.ReadOnly }

// Begin starts a transaction. MySQL commits an open transaction first; a refused BEGIN
// leaves it untouched.
func (s *Session) Begin(ctx context.Context, opts TxOptions) error {
	if opts.Snapshot && s.name == "" {
		return errs.Unsupported("WITH CONSISTENT SNAPSHOT", reasonOneDatabase+"; select it first with USE")
	}
	if err := s.Commit(ctx); err != nil {
		return err
	}
	s.nextReadOnly = nil
	t := &tx{opts: opts}
	if opts.Snapshot {
		if err := s.bind(ctx, t, s.name); err != nil {
			return err
		}
		// A read starts the WAL snapshot; BEGIN alone does not.
		if _, err := t.conn.ExecContext(ctx, "SELECT count(*) FROM sqlite_schema"); err != nil {
			s.release(ctx, t, false)
			return fmt.Errorf("session: snapshot: %w", err)
		}
	}
	s.tx = t
	return nil
}

// Commit commits the open transaction; without one it does nothing. If the commit
// fails the transaction is rolled back and the error returned.
func (s *Session) Commit(ctx context.Context) error {
	t := s.tx
	if t == nil {
		return nil
	}
	s.tx = nil
	if t.conn == nil {
		return nil
	}
	if _, err := t.conn.ExecContext(ctx, "COMMIT"); err != nil {
		s.release(ctx, t, true)
		return fmt.Errorf("session: commit: %w", err)
	}
	s.release(ctx, t, false)
	return nil
}

// Rollback rolls back the open transaction; without one it does nothing.
func (s *Session) Rollback(ctx context.Context) error {
	t := s.tx
	if t == nil {
		return nil
	}
	s.tx = nil
	if t.conn != nil {
		s.release(ctx, t, true)
	}
	return nil
}

// Close ends the session: an open transaction is rolled back, as MySQL does when a
// client disconnects.
func (s *Session) Close(ctx context.Context) error {
	return s.Rollback(ctx)
}

// Conn returns the connection a statement on database runs on, and the function to
// call when the statement is done. database is "" for a statement without tables.
// Inside a transaction the connection is the transaction's and release does nothing.
func (s *Session) Conn(ctx context.Context, database string, write bool) (*sql.Conn, func(), error) {
	if s.tx == nil && s.manual && database != "" {
		// autocommit=0: the statement starts the transaction it runs in.
		s.tx = &tx{opts: TxOptions{ReadOnly: s.DefaultReadOnly()}}
		s.nextReadOnly = nil
	}
	if write && (s.tx != nil && s.tx.opts.ReadOnly || s.tx == nil && s.readOnly) {
		return nil, nil, errs.ReadOnlyTransaction()
	}
	if database == "" {
		if write {
			return nil, nil, errors.New("session: a write needs a database")
		}
		c, err := s.eng.Scratch(ctx)
		if err != nil {
			return nil, nil, err
		}
		return c, func() { _ = c.Close() }, nil
	}
	if t := s.tx; t != nil {
		if t.conn == nil {
			if err := s.bind(ctx, t, database); err != nil {
				return nil, nil, err
			}
		} else if t.db != database {
			return nil, nil, errs.Unsupported("statement on another database inside a transaction", reasonOneDatabase)
		}
		return t.conn, func() {}, nil
	}
	db, err := s.eng.Database(database)
	if err != nil {
		return nil, nil, err
	}
	var c *sql.Conn
	if write {
		c, err = db.Writer(ctx)
	} else {
		c, err = db.Read(ctx)
	}
	if err != nil {
		return nil, nil, err
	}
	return c, func() { _ = c.Close() }, nil
}

// DDLConn returns the writer of database for a DDL statement. MySQL commits the open
// transaction before DDL and runs the statement on its own, autocommit or not.
func (s *Session) DDLConn(ctx context.Context, database string) (*sql.Conn, func(), error) {
	if err := s.Commit(ctx); err != nil {
		return nil, nil, err
	}
	if s.readOnly {
		return nil, nil, errs.ReadOnlyTransaction()
	}
	db, err := s.eng.Database(database)
	if err != nil {
		return nil, nil, err
	}
	c, err := db.Writer(ctx)
	if err != nil {
		return nil, nil, err
	}
	return c, func() { _ = c.Close() }, nil
}

// StatementFailed is called after a statement inside a transaction failed. SQLite
// rolls the whole transaction back by itself after some errors (SQLITE_FULL, IOERR,
// BUSY, NOMEM); MySQL keeps it. The driver does not expose sqlite3_get_autocommit, so
// BEGIN probes the connection: it fails only while a transaction is open.
func (s *Session) StatementFailed(ctx context.Context) {
	t := s.tx
	if t == nil || t.conn == nil {
		return
	}
	_, err := t.conn.ExecContext(ctx, "BEGIN")
	if err != nil {
		if _, msg, ok := engine.ErrorCode(err); ok && strings.Contains(msg, "within a transaction") {
			return // still open
		}
	}
	// Either SQLite had ended the transaction (the probe started a new one) or the state
	// is unknown: end it, so the status flags never claim a transaction that is gone.
	s.tx = nil
	s.release(ctx, t, true)
}

// bind takes the transaction's connection on database and begins.
func (s *Session) bind(ctx context.Context, t *tx, database string) error {
	db, err := s.eng.Database(database)
	if err != nil {
		return err
	}
	var c *sql.Conn
	if t.opts.ReadOnly {
		c, err = db.Read(ctx)
	} else {
		c, err = db.Writer(ctx)
	}
	if err != nil {
		return err
	}
	// DEFERRED: the snapshot starts at the first read; with a single writer per file in
	// this process there is no competing write lock.
	if _, err := c.ExecContext(ctx, "BEGIN"); err != nil {
		_ = c.Close()
		return fmt.Errorf("session: begin: %w", err)
	}
	t.db, t.conn = database, c
	return nil
}

// release returns the transaction's connection to its pool, rolling back first when
// asked. A connection whose rollback failed may still hold a transaction; it is
// discarded instead of being reused by another session.
func (s *Session) release(ctx context.Context, t *tx, rollback bool) {
	c := t.conn
	t.conn = nil
	if rollback {
		if _, err := c.ExecContext(ctx, "ROLLBACK"); err != nil {
			if _, msg, ok := engine.ErrorCode(err); !ok || !strings.Contains(msg, "no transaction is active") {
				_ = c.Raw(func(any) error { return driver.ErrBadConn })
			}
		}
	}
	_ = c.Close()
}

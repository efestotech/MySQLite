// Package session holds per-connection state (docs/01-architecture.md rule 6). In M1
// that is the current database and the explicit transaction; sql_mode and variables
// follow.
//
// A Session is used by one connection goroutine at a time and is not safe for
// concurrent use.
package session

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"mysqlite/internal/engine"
	"mysqlite/internal/errs"
	"mysqlite/internal/infoschema"
)

// Session is the state of one client connection.
type Session struct {
	eng  *engine.Engine
	name string
	db   *engine.Database
	tx   *tx // the open transaction, or nil
	// manual is autocommit=0.
	manual bool
	// readOnly is transaction_read_only; nextReadOnly overrides it for the next
	// transaction only (SET TRANSACTION without a scope).
	readOnly     bool
	nextReadOnly *bool
	// lockWait bounds the wait for a database's writer (innodb_lock_wait_timeout);
	// 0 waits without a bound.
	lockWait time.Duration
}

// SetLockWait sets how long a statement waits for the writer before 1205.
func (s *Session) SetLockWait(d time.Duration) { s.lockWait = d }

// writer takes db's writer, waiting at most lockWait.
func (s *Session) writer(ctx context.Context, db *engine.Database) (*sql.Conn, error) {
	if s.lockWait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.lockWait)
		defer cancel()
	}
	return db.Writer(ctx)
}

// New returns a session with no current database.
func New(eng *engine.Engine) *Session {
	return &Session{eng: eng}
}

// Database returns the current database, or "" and nil when none is selected.
func (s *Session) Database() (string, *engine.Database) {
	return s.name, s.db
}

// Use makes name the current database (COM_INIT_DB, USE). On failure the current
// database is unchanged. Known failures are *errs.Error; anything else is returned as
// is, for the caller to log and map to 1105.
func (s *Session) Use(name string) error {
	if name == "" {
		// MySQL answers an empty COM_INIT_DB with 1046.
		return errs.NoDatabaseSelected()
	}
	if infoschema.Is(name) {
		// information_schema has no file; statements on it never reach the engine.
		s.name, s.db = infoschema.Name, nil
		return nil
	}
	db, err := s.eng.Database(name)
	switch {
	case errors.Is(err, engine.ErrUnknownDatabase), errors.Is(err, engine.ErrInvalidDatabaseName):
		// A name the engine cannot store can never exist, so it is unknown here. 1102 is
		// reserved for CREATE DATABASE, where the name is being introduced.
		return errs.UnknownDatabase(name)
	case err != nil:
		return err
	}
	s.name, s.db = name, db
	return nil
}

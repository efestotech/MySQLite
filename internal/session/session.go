// Package session holds per-connection state (docs/01-architecture.md rule 6). In M1
// that is the current database and the explicit transaction; sql_mode and variables
// follow.
//
// A Session is used by one connection goroutine at a time and is not safe for
// concurrent use.
package session

import (
	"errors"

	"mysqlite/internal/engine"
	"mysqlite/internal/errs"
)

// Session is the state of one client connection.
type Session struct {
	eng  *engine.Engine
	name string
	db   *engine.Database
	tx   *tx // the open explicit transaction, or nil
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

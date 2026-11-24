package engine

import (
	"errors"

	"modernc.org/sqlite"
)

// ErrorCode extracts the SQLite extended result code and message from err, so that
// callers can map errors without importing the driver (docs/01-architecture.md rule 4).
// ok is false when err does not come from SQLite.
func ErrorCode(err error) (code int, message string, ok bool) {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return 0, "", false
	}
	return se.Code(), se.Error(), true
}

// IsUniqueConflict reports whether err is SQLite's PRIMARY KEY or UNIQUE constraint
// failure (extended codes 1555 and 2067).
func IsUniqueConflict(err error) bool {
	code, _, ok := ErrorCode(err)
	return ok && (code == 1555 || code == 2067)
}

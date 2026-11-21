package session

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"mysqlite/internal/engine"
	"mysqlite/internal/errs"
)

func newEngine(t *testing.T, dbs ...string) *engine.Engine {
	t.Helper()
	e, err := engine.Open(engine.Options{
		DataDir:      filepath.Join(t.TempDir(), "data"),
		MaxReadConns: 1,
		BusyTimeout:  50 * time.Millisecond,
		Synchronous:  "NORMAL",
	}, engine.Extensions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	for _, name := range dbs {
		if err := e.CreateDatabase(name); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func requireErr(t *testing.T, err error, code uint16, msg string) {
	t.Helper()
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("want *errs.Error %d, got %v", code, err)
	}
	if e.Code != code || e.Message != msg {
		t.Fatalf("want %d %q, got %d %q", code, msg, e.Code, e.Message)
	}
}

func TestNewSessionHasNoDatabase(t *testing.T) {
	s := New(newEngine(t))
	if name, db := s.Database(); name != "" || db != nil {
		t.Fatalf("want no database, got %q %v", name, db)
	}
}

func TestUseSwitchesDatabase(t *testing.T) {
	s := New(newEngine(t, "shop", "crm"))
	for _, name := range []string{"shop", "crm"} {
		if err := s.Use(name); err != nil {
			t.Fatalf("Use(%q): %v", name, err)
		}
		got, db := s.Database()
		if got != name || db == nil || db.Name() != name {
			t.Fatalf("want %q, got %q %v", name, got, db)
		}
	}
}

func TestUseUnknownKeepsCurrentDatabase(t *testing.T) {
	s := New(newEngine(t, "shop"))
	if err := s.Use("shop"); err != nil {
		t.Fatal(err)
	}
	requireErr(t, s.Use("missing"), 1049, "Unknown database 'missing'")
	if name, _ := s.Database(); name != "shop" {
		t.Fatalf("a failed USE must keep the current database, got %q", name)
	}
}

func TestUseNameThatCannotExistIsUnknown(t *testing.T) {
	// MySQL accepts such names when quoted; here no file can hold them, so the database
	// does not exist and the answer is 1049, not 1102 (1102 is for CREATE DATABASE).
	s := New(newEngine(t))
	requireErr(t, s.Use("a/b"), 1049, "Unknown database 'a/b'")
}

func TestUseEmptyNameIsNoDatabaseSelected(t *testing.T) {
	s := New(newEngine(t))
	requireErr(t, s.Use(""), 1046, "No database selected")
}

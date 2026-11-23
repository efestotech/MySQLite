package engine

import (
	"errors"
	"path/filepath"
	"testing"
)

// M5 criterion 5: a second instance on the same data directory refuses to start.
func TestSecondInstanceRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	opts := Options{DataDir: dir, MaxReadConns: 1, Synchronous: "NORMAL"}
	first, err := Open(opts, Extensions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(opts, Extensions{}); !errors.Is(err, ErrDataDirLocked) {
		t.Fatalf("second open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(opts, Extensions{})
	if err != nil {
		t.Fatalf("open after close: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	// The lock file is not a database.
	again2, err := Open(opts, Extensions{})
	if err != nil {
		t.Fatal(err)
	}
	defer again2.Close()
	if names, err := again2.Databases(); err != nil || len(names) != 0 {
		t.Fatalf("databases: %v %v", names, err)
	}
}

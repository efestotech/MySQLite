//go:build unix

package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// docs/06-transactions.md section 7: a world-writable data directory is refused.
func TestWorldWritableDataDirRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	opts := Options{DataDir: dir, MaxReadConns: 1, Synchronous: "NORMAL"}
	if _, err := Open(opts, Extensions{}); !errors.Is(err, ErrDataDirWorldWritable) {
		t.Fatalf("world-writable: %v", err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	e, err := Open(opts, Extensions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = e.Close()
}

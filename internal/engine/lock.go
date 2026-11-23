package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// lockFile is the advisory lock that keeps a second mysqlite instance off the data
// directory (docs/06-transactions.md section 7). The operating system releases it when
// the process ends, so a crash leaves no stale lock.
const lockFile = "mysqlite.lock"

// ErrDataDirLocked is returned by Open when another instance holds the data directory.
var ErrDataDirLocked = errors.New("engine: the data directory is in use by another mysqlite instance")

// ErrDataDirWorldWritable is returned by Open when the data directory is writable by
// every user.
var ErrDataDirWorldWritable = errors.New("engine: the data directory is world-writable")

// lockDataDir takes the instance lock of dir; the returned file holds it until closed.
func lockDataDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_RDWR|os.O_CREATE, 0o640)
	if err != nil {
		return nil, fmt.Errorf("engine: open lock file: %w", err)
	}
	locked, err := tryLock(f)
	if err != nil || !locked {
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("engine: lock %s: %w", f.Name(), err)
		}
		return nil, fmt.Errorf("%w (%s)", ErrDataDirLocked, dir)
	}
	return f, nil
}

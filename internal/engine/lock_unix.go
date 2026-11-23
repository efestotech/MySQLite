//go:build unix

package engine

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock takes an exclusive flock on f without waiting.
func tryLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

// checkPermissions refuses a data directory every user can write to.
func checkPermissions(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("engine: stat data dir: %w", err)
	}
	if fi.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("%w: %s has mode %v", ErrDataDirWorldWritable, dir, fi.Mode().Perm())
	}
	return nil
}

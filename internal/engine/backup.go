package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// Backups before an ALTER TABLE rebuild (ADR 0008): full copies of a database file,
// next to it, named <database>.pre-alter-<UTC yyyymmddThhmmssZ>.sqlite. A second copy
// within the same second takes a -2, -3... suffix. They are not databases: Databases
// lists only <name>.db files.

const backupStamp = "20060102T150405Z"

var reBackup = regexp.MustCompile(`^(.+)\.pre-alter-(\d{8}T\d{6}Z)(?:-(\d+))?\.sqlite$`)

// maxBackupSuffix bounds the search for a free name within one second.
const maxBackupSuffix = 1000

// BackupPath is a free file name for a backup of database name taken at.
func (e *Engine) BackupPath(name string, at time.Time) (string, error) {
	if _, err := e.path(name); err != nil {
		return "", err
	}
	base := name + ".pre-alter-" + at.UTC().Format(backupStamp)
	for n := 1; n <= maxBackupSuffix; n++ {
		file := base + ".sqlite"
		if n > 1 {
			file = base + "-" + strconv.Itoa(n) + ".sqlite"
		}
		p := filepath.Join(e.opts.DataDir, file)
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			return p, nil
		} else if err != nil {
			return "", fmt.Errorf("engine: backup name %s: %w", p, err)
		}
	}
	return "", fmt.Errorf("engine: no free backup name for %s at %s", name, base)
}

// PruneBackups deletes the oldest backups of database name beyond the newest keep.
// Only files matching the backup pattern are touched.
func (e *Engine) PruneBackups(name string, keep int) error {
	if _, err := e.path(name); err != nil {
		return err
	}
	entries, err := os.ReadDir(e.opts.DataDir)
	if err != nil {
		return fmt.Errorf("engine: list backups: %w", err)
	}
	type backup struct {
		file  string
		stamp string
		n     int
	}
	var backups []backup
	for _, entry := range entries {
		m := reBackup.FindStringSubmatch(entry.Name())
		if m == nil || m[1] != name || !entry.Type().IsRegular() {
			continue
		}
		n := 1
		if m[3] != "" {
			n, _ = strconv.Atoi(m[3])
		}
		backups = append(backups, backup{entry.Name(), m[2], n})
	}
	sort.Slice(backups, func(i, j int) bool {
		if backups[i].stamp != backups[j].stamp {
			return backups[i].stamp > backups[j].stamp
		}
		return backups[i].n > backups[j].n
	})
	for i := keep; i < len(backups); i++ {
		if err := os.Remove(filepath.Join(e.opts.DataDir, backups[i].file)); err != nil {
			return fmt.Errorf("engine: remove old backup: %w", err)
		}
	}
	return nil
}

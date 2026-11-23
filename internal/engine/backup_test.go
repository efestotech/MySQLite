package engine

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// Backup names carry the UTC second and a -N suffix within one second; pruning keeps
// the newest by time, then by suffix, and touches only that database's backups.
func TestBackupNames(t *testing.T) {
	dir := t.TempDir()
	e := openEngine(t, Options{DataDir: dir, MaxReadConns: 1, Synchronous: "NORMAL"})
	at := time.Date(2026, 11, 23, 10, 4, 5, 0, time.FixedZone("x", 3600))

	touch := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := e.BackupPath("shop", at)
	if err != nil || filepath.Base(p) != "shop.pre-alter-20261123T090405Z.sqlite" {
		t.Fatalf("first name %q, %v", p, err)
	}
	touch(filepath.Base(p))
	p, err = e.BackupPath("shop", at)
	if err != nil || filepath.Base(p) != "shop.pre-alter-20261123T090405Z-2.sqlite" {
		t.Fatalf("second name %q, %v", p, err)
	}
	touch(filepath.Base(p))
	touch("shop.pre-alter-20261123T090405Z-10.sqlite")
	touch("shop.pre-alter-20261122T235959Z.sqlite")
	touch("shop.pre-alter-20261124T000000Z.sqlite")
	touch("shopx.pre-alter-20200101T000000Z.sqlite")
	touch("shop.db.bak")
	if _, err := e.BackupPath("../x", at); err == nil {
		t.Error("BackupPath accepted an invalid database name")
	}

	if err := e.PruneBackups("shop", 3); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	sort.Strings(got)
	want := []string{
		"shop.db.bak",
		"shop.pre-alter-20261123T090405Z-10.sqlite",
		"shop.pre-alter-20261123T090405Z-2.sqlite",
		"shop.pre-alter-20261124T000000Z.sqlite",
		"shopx.pre-alter-20200101T000000Z.sqlite",
	}
	if len(got) != len(want) {
		t.Fatalf("files %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("files %v, want %v", got, want)
			break
		}
	}
}

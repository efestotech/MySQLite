package wire

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// backups lists the ALTER TABLE backups of shop in the data directory, sorted.
func backups(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "shop.pre-alter-*.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range matches {
		matches[i] = filepath.Base(m)
	}
	sort.Strings(matches)
	return matches
}

// A rebuild copies the database first (ADR 0008): the copy holds the table as it was,
// the newest 3 copies are kept, native forms take none, and a failed copy fails the
// ALTER.
func TestAlterBackup(t *testing.T) {
	eng, addr := shopSetup(t)
	conn := dial(t, addr)
	dir := eng.DataDir()
	execOK(t, conn, "CREATE TABLE gadgets (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(20) NOT NULL)")
	execOK(t, conn, "INSERT INTO gadgets (name) VALUES ('pen'), ('ink')")
	// Files the pruning must leave alone.
	for _, name := range []string{"shop.pre-alter-junk.sqlite", "other.pre-alter-20200101T000000Z.sqlite"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	execOK(t, conn, "ALTER TABLE gadgets ADD note TEXT, ADD INDEX (name)")
	if got := backups(t, dir); len(got) != 1 || got[0] != "shop.pre-alter-junk.sqlite" {
		t.Fatalf("native ALTER made a backup: %v", got)
	}

	execOK(t, conn, "ALTER TABLE gadgets MODIFY name VARCHAR(40) NOT NULL")
	var first string
	for _, b := range backups(t, dir) {
		if b != "shop.pre-alter-junk.sqlite" {
			first = b
		}
	}
	if first == "" {
		t.Fatal("no backup after a rebuild")
	}
	// Restoring is putting the copy in place of a database file.
	copied, err := os.ReadFile(filepath.Join(dir, first))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "restored.db"), copied, 0o600); err != nil {
		t.Fatal(err)
	}
	restored, err := eng.Database("restored")
	if err != nil {
		t.Fatal(err)
	}
	w, err := restored.Writer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var check string
	if err := w.QueryRowContext(context.Background(), "PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Errorf("backup integrity_check %q, %v", check, err)
	}
	w.Close()
	if ddl := showCreate(t, conn, "restored.gadgets"); !strings.Contains(ddl, "`name` varchar(20) COLLATE utf8mb4_general_ci NOT NULL") || !strings.Contains(ddl, "KEY `name` (`name`)") {
		t.Errorf("backup holds\n%s\nwant the table before the ALTER", ddl)
	}
	if got := rowsOf(t, execOK(t, conn, "SELECT name FROM restored.gadgets ORDER BY id")); got != "pen;ink" {
		t.Errorf("backup rows %q", got)
	}

	for _, q := range []string{
		"ALTER TABLE gadgets MODIFY name VARCHAR(41) NOT NULL",
		"ALTER TABLE gadgets MODIFY name VARCHAR(42) NOT NULL",
		"ALTER TABLE gadgets MODIFY name VARCHAR(43) NOT NULL",
	} {
		execOK(t, conn, q)
	}
	got := backups(t, dir)
	var copies []string
	for _, b := range got {
		if b != "shop.pre-alter-junk.sqlite" {
			copies = append(copies, b)
		}
	}
	if len(copies) != 3 || len(got) != 4 {
		t.Errorf("after 4 rebuilds: %v, want the junk file and 3 copies", got)
	}
	for _, c := range copies {
		if c == first {
			t.Errorf("the oldest copy %s was kept", first)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "other.pre-alter-20200101T000000Z.sqlite")); err != nil {
		t.Errorf("another database's backup: %v", err)
	}
	if res := rowsOf(t, execOK(t, conn, "SHOW DATABASES")); strings.Contains(res, "pre-alter") {
		t.Errorf("SHOW DATABASES lists backups: %s", res)
	}

	// A failed copy fails the ALTER and changes nothing.
	saved := vacuumInto
	vacuumInto = func(context.Context, *sql.Conn, string) error { return errors.New("disk full") }
	t.Cleanup(func() { vacuumInto = saved })
	_, err = conn.Execute("ALTER TABLE gadgets MODIFY name VARCHAR(50) NOT NULL")
	requireMyError(t, err, 1105, "HY000")
	vacuumInto = saved
	if ddl := showCreate(t, conn, "gadgets"); !strings.Contains(ddl, "varchar(43)") {
		t.Errorf("after the failed copy:\n%s", ddl)
	}
}

// alter_backup: false takes no copy.
func TestAlterBackupOff(t *testing.T) {
	e := newEngine(t)
	seed(t, e, "shop")
	addr := startServerWith(t, Config{
		ServerVersion:    "8.0.36-mysqlite",
		MaxAllowedPacket: 64 << 20,
		Users:            map[string]string{testUser: testPassword},
		Engine:           e,
		SkipAlterBackup:  true,
	})
	conn := dial(t, addr)
	execOK(t, conn, "CREATE TABLE gadgets (id INT PRIMARY KEY, name VARCHAR(20))")
	execOK(t, conn, "ALTER TABLE gadgets MODIFY name VARCHAR(30)")
	if got := backups(t, e.DataDir()); len(got) != 0 {
		t.Errorf("backups with alter_backup off: %v", got)
	}
}

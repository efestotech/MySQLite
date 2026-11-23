package wire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"mysqlite/internal/dialect/udf"
	"mysqlite/internal/engine"
)

// rawCheck runs PRAGMA integrity_check and foreign_key_check on a database's writer,
// past mysqlite, and reads PRAGMA foreign_keys there.
func rawCheck(t *testing.T, e *engine.Engine, db string) (integrity string, violations int, fkOn bool) {
	t.Helper()
	ctx := context.Background()
	d, err := e.Database(db)
	if err != nil {
		t.Fatal(err)
	}
	w, err := d.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	rows, err := w.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		violations++
	}
	rows.Close()
	if err := w.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fkOn); err != nil {
		t.Fatal(err)
	}
	return integrity, violations, fkOn
}

// S4: MODIFY on a table that another table references keeps every row and the
// foreign key, passes integrity_check, and leaves foreign keys on. The child table
// comes from another tool (mysqlite's CREATE TABLE has no FOREIGN KEY yet).
// `oracle: pending`.
func TestAlterTableForeignKeys(t *testing.T) {
	eng, addr := shopSetup(t)
	conn := dial(t, addr)
	execOK(t, conn, "CREATE TABLE owners (id INT PRIMARY KEY, name VARCHAR(20))")
	execOK(t, conn, "INSERT INTO owners VALUES (1, 'ann'), (2, 'bob'), (3, 'cy')")
	seed(t, eng, "shop",
		"CREATE TABLE pets (id INTEGER PRIMARY KEY, owner INT REFERENCES owners (id) ON DELETE CASCADE, name TEXT)",
		"INSERT INTO pets VALUES (1, 1, 'rex'), (2, 1, 'tom'), (3, 2, 'kit')")

	execOK(t, conn, "ALTER TABLE owners MODIFY name VARCHAR(40) NOT NULL DEFAULT '', ADD INDEX (name)")
	if got := rowsOf(t, execOK(t, conn, "SELECT id, name FROM owners ORDER BY id")); got != "1,ann;2,bob;3,cy" {
		t.Errorf("owners %q", got)
	}
	if got := rowsOf(t, execOK(t, conn, "SELECT id, owner, name FROM pets ORDER BY id")); got != "1,1,rex;2,1,tom;3,2,kit" {
		t.Errorf("pets %q", got)
	}
	if integrity, violations, fkOn := rawCheck(t, eng, "shop"); integrity != "ok" || violations != 0 || !fkOn {
		t.Errorf("after MODIFY: integrity_check %q, foreign_key_check %d rows, foreign_keys %v", integrity, violations, fkOn)
	}
	// The child's foreign key now points at the rebuilt table.
	_, err := conn.Execute("INSERT INTO pets VALUES (4, 99, 'x')")
	requireMyError(t, err, 1452, "23000")
	execOK(t, conn, "DELETE FROM owners WHERE id = 1")
	if got := rowsOf(t, execOK(t, conn, "SELECT id FROM pets ORDER BY id")); got != "3" {
		t.Errorf("pets after the cascade %q", got)
	}

	// A rebuild that would leave the foreign key dangling is refused, and foreign keys
	// stay on after the failure.
	for _, q := range []string{
		"ALTER TABLE owners RENAME COLUMN id TO oid",
		"ALTER TABLE owners CHANGE id oid INT NOT NULL",
		"ALTER TABLE owners DROP PRIMARY KEY",
		"ALTER TABLE pets MODIFY name VARCHAR(10)",
	} {
		_, err := conn.Execute(q)
		requireMyError(t, err, 1235, "42000")
	}
	if integrity, violations, fkOn := rawCheck(t, eng, "shop"); integrity != "ok" || violations != 0 || !fkOn {
		t.Errorf("after refusals: integrity_check %q, foreign_key_check %d rows, foreign_keys %v", integrity, violations, fkOn)
	}
	if got := showCreate(t, conn, "owners"); got != "CREATE TABLE `owners` (\n"+
		"  `id` int NOT NULL,\n"+
		"  `name` varchar(40) NOT NULL DEFAULT '',\n"+
		"  PRIMARY KEY (`id`),\n"+
		"  KEY `name` (`name`)\n"+
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci" {
		t.Errorf("owners after refusals:\n%s", got)
	}
}

// crashEnv names the data directory and the rebuild step at which the helper process
// dies.
const (
	crashDirEnv  = "MYSQLITE_CRASH_DIR"
	crashStepEnv = "MYSQLITE_CRASH_STEP"
)

// TestCrashHelper is the process the crash-safety test kills: it runs a rebuild on the
// data directory it is given and exits at the named step, without COMMIT, ROLLBACK or
// closing anything.
func TestCrashHelper(t *testing.T) {
	dir, step := os.Getenv(crashDirEnv), os.Getenv(crashStepEnv)
	if dir == "" {
		t.Skip("run by TestAlterTableCrash")
	}
	e, err := engine.Open(engine.Options{DataDir: dir, MaxReadConns: 1, BusyTimeout: time.Second, Synchronous: "NORMAL"}, udf.Extensions())
	if err != nil {
		t.Fatal(err)
	}
	rebuildStep = func(s string) {
		if s == step {
			os.Exit(3)
		}
	}
	addr := startServerWith(t, Config{
		ServerVersion:    "8.0.36-mysqlite",
		MaxAllowedPacket: 64 << 20,
		Users:            map[string]string{testUser: testPassword},
		Engine:           e,
	})
	conn := dial(t, addr)
	_, err = conn.Execute("ALTER TABLE gadgets MODIFY name VARCHAR(40) NOT NULL, ADD qty INT NOT NULL FIRST")
	t.Fatalf("the ALTER finished (%v) without reaching step %q", err, step)
}

// S2/S4: a process killed at any point of a rebuild leaves a database that opens with
// the table as it was before the ALTER, passes integrity_check, and can be altered
// again.
func TestAlterTableCrash(t *testing.T) {
	if testing.Short() {
		t.Skip("starts subprocesses")
	}
	for _, step := range []string{"create", "copy", "drop", "rename", "checked"} {
		t.Run(step, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			e, err := engine.Open(engine.Options{DataDir: dir, MaxReadConns: 1, BusyTimeout: time.Second, Synchronous: "NORMAL"}, udf.Extensions())
			if err != nil {
				t.Fatal(err)
			}
			if err := e.CreateDatabase("shop"); err != nil {
				t.Fatal(err)
			}
			addr := startServerWith(t, Config{
				ServerVersion:    "8.0.36-mysqlite",
				MaxAllowedPacket: 64 << 20,
				Users:            map[string]string{testUser: testPassword},
				Engine:           e,
				SkipAlterBackup:  true,
			})
			conn := dial(t, addr)
			execOK(t, conn, "CREATE TABLE gadgets (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(20) NOT NULL, KEY by_name (name))")
			for i := 0; i < 50; i++ {
				execOK(t, conn, fmt.Sprintf("INSERT INTO gadgets (name) VALUES ('g%02d')", i))
			}
			before := showCreate(t, conn, "gadgets")
			conn.Close()
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$", "-test.count=1")
			cmd.Env = append(os.Environ(), crashDirEnv+"="+dir, crashStepEnv+"="+step)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 3 {
				t.Fatalf("helper: %v\n%s", err, out)
			}

			e2, err := engine.Open(engine.Options{DataDir: dir, MaxReadConns: 1, BusyTimeout: time.Second, Synchronous: "NORMAL"}, udf.Extensions())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e2.Close() })
			if integrity, violations, _ := rawCheck(t, e2, "shop"); integrity != "ok" || violations != 0 {
				t.Errorf("after the crash: integrity_check %q, foreign_key_check %d rows", integrity, violations)
			}
			addr2 := startServerWith(t, Config{
				ServerVersion:    "8.0.36-mysqlite",
				MaxAllowedPacket: 64 << 20,
				Users:            map[string]string{testUser: testPassword},
				Engine:           e2,
				SkipAlterBackup:  true,
			})
			conn2 := dial(t, addr2)
			if got := showCreate(t, conn2, "gadgets"); got != before {
				t.Errorf("after the crash:\n%s\nwant\n%s", got, before)
			}
			if got := rowsOf(t, execOK(t, conn2, "SELECT count(*), min(name), max(name) FROM gadgets")); got != "50,g00,g49" {
				t.Errorf("rows after the crash %q", got)
			}
			if got := rowsOf(t, execOK(t, conn2, "SHOW TABLES")); got != "gadgets" {
				t.Errorf("tables after the crash %q", got)
			}
			execOK(t, conn2, "ALTER TABLE gadgets MODIFY name VARCHAR(40) NOT NULL")
			if res := execOK(t, conn2, "INSERT INTO gadgets (name) VALUES ('new')"); res.InsertId != 51 {
				t.Errorf("id after the crash and a rebuild %d, want 51", res.InsertId)
			}
		})
	}
}

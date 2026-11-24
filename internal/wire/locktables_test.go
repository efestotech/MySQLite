package wire

import (
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// Decision Q7 (docs/05-dialect.md section 6, docs/06-transactions.md section 4):
// LOCK TABLES commits the open transaction and warns that no lock is taken; UNLOCK
// TABLES commits only after a LOCK TABLES of the same session, as MySQL does, and
// warns too. `oracle: pending`.
func TestLockTables(t *testing.T) {
	addr := queueServer(t, 8)
	conn := dial(t, addr)
	other := dial(t, addr)
	count := func() string { return decodedText(execOK(t, other, "SELECT COUNT(*) FROM t")) }
	warning := func(want string) {
		t.Helper()
		res := execOK(t, conn, "SHOW WARNINGS")
		if got := rowsOf(t, res); got != "Warning,1235,"+want {
			t.Fatalf("warnings %q", got)
		}
	}
	const lockMsg = "mysqlite: LOCK TABLES takes no table locks: other sessions can still write these tables between this session's statements"
	const unlockMsg = "mysqlite: UNLOCK TABLES has no table locks to release"

	// LOCK TABLES commits.
	execOK(t, conn, "BEGIN")
	execOK(t, conn, "INSERT INTO t VALUES (1, 1)")
	res := execOK(t, conn, "LOCK TABLES t WRITE")
	if res.Warnings != 1 || res.Status&mysql.SERVER_STATUS_IN_TRANS != 0 {
		t.Fatalf("LOCK TABLES: warnings %d status %#x", res.Warnings, res.Status)
	}
	warning(lockMsg)
	if n := count(); n != "1" {
		t.Fatalf("after LOCK TABLES: %s rows", n)
	}

	// UNLOCK TABLES after LOCK TABLES commits (the mysqldump / autocommit=0 pattern).
	execOK(t, conn, "SET autocommit = 0")
	execOK(t, conn, "INSERT INTO t VALUES (2, 2)")
	if res := execOK(t, conn, "UNLOCK TABLES"); res.Warnings != 1 {
		t.Fatalf("UNLOCK TABLES: warnings %d", res.Warnings)
	}
	warning(unlockMsg)
	if n := count(); n != "2" {
		t.Fatalf("after UNLOCK TABLES: %s rows", n)
	}

	// UNLOCK TABLES without LOCK TABLES does not commit.
	execOK(t, conn, "INSERT INTO t VALUES (3, 3)")
	execOK(t, conn, "UNLOCK TABLES")
	execOK(t, conn, "ROLLBACK")
	if n := count(); n != "2" {
		t.Fatalf("UNLOCK TABLES without a lock committed: %s rows", n)
	}

	// START TRANSACTION releases the table locks: the later UNLOCK does not commit.
	execOK(t, conn, "SET autocommit = 1")
	execOK(t, conn, "LOCK TABLES t READ")
	execOK(t, conn, "START TRANSACTION")
	execOK(t, conn, "INSERT INTO t VALUES (4, 4)")
	execOK(t, conn, "UNLOCK TABLES")
	execOK(t, conn, "ROLLBACK")
	if n := count(); n != "2" {
		t.Fatalf("UNLOCK TABLES after START TRANSACTION committed: %s rows", n)
	}

	// A table that does not exist is 1146 (MySQL 8.0.46, oracle probe o.103.1).
	_, err := conn.Execute("LOCK TABLES t READ, nosuch WRITE")
	requireMyError(t, err, 1146, "42S02")
}

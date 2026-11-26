package wire

import (
	"testing"
)

// WordPress picks utf8mb4_unicode_520_ci whenever the server reports MySQL 5.6 or later,
// for the connection and for every table it creates.
func TestUnicode520Collation(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "SET NAMES 'utf8mb4' COLLATE 'utf8mb4_unicode_520_ci'")
	if got := column(t, execOK(t, conn, "SELECT @@collation_connection AS c"), 0, "c"); got != "utf8mb4_unicode_520_ci" {
		t.Errorf("collation_connection = %s", got)
	}
	execOK(t, conn, "CREATE TABLE wp_terms (term_id bigint(20) unsigned NOT NULL auto_increment, name varchar(200) NOT NULL default '', PRIMARY KEY (term_id)) DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_520_ci")
	execOK(t, conn, "INSERT INTO wp_terms (name) VALUES ('Uncategorized')")
	if n := execOK(t, conn, "SELECT term_id FROM wp_terms WHERE name = 'UNCATEGORIZED'").RowNumber(); n != 1 {
		t.Errorf("case-insensitive match: %d rows", n)
	}
	if got := column(t, execOK(t, conn, "SHOW FULL COLUMNS FROM wp_terms LIKE 'name'"), 0, "Collation"); got != "utf8mb4_unicode_520_ci" {
		t.Errorf("column collation = %s", got)
	}
	res := execOK(t, conn, "SHOW COLLATION LIKE 'utf8mb4_unicode_520_ci'")
	if column(t, res, 0, "Id") != "246" || column(t, res, 0, "Sortlen") != "8" || column(t, res, 0, "Pad_attribute") != "PAD SPACE" {
		t.Errorf("utf8mb4_unicode_520_ci: Id %s Sortlen %s", column(t, res, 0, "Id"), column(t, res, 0, "Sortlen"))
	}
	res = execOK(t, conn, "SHOW COLLATION LIKE 'utf8mb3_unicode_520_ci'")
	if column(t, res, 0, "Id") != "214" {
		t.Errorf("utf8mb3_unicode_520_ci: Id %s", column(t, res, 0, "Id"))
	}
}

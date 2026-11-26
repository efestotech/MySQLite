package wire

import (
	"strings"
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

// WordPress indexes long strings by a prefix (KEY meta_key (meta_key(191))). SQLite
// indexes the whole value, which finds the same rows for a non-unique key; the prefix is
// reported back as MySQL does, because WordPress compares it on every upgrade.
func TestKeyPrefixLength(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, `CREATE TABLE wp_postmeta (
		meta_id bigint(20) unsigned NOT NULL auto_increment,
		post_id bigint(20) unsigned NOT NULL default '0',
		meta_key varchar(255) default NULL,
		meta_value longtext,
		PRIMARY KEY (meta_id),
		KEY post_id (post_id),
		KEY meta_key (meta_key(191)),
		KEY post_meta (post_id, meta_value(20))
	) DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_520_ci`)
	execOK(t, conn, "INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, '_edit_lock', 'x'), (2, '_edit_last', 'y')")
	if n := execOK(t, conn, "SELECT meta_id FROM wp_postmeta WHERE meta_key = '_edit_lock'").RowNumber(); n != 1 {
		t.Errorf("lookup through the prefix key: %d rows", n)
	}

	subParts := func(q string, keyCol, seqCol, colCol, subCol string) map[string]string {
		t.Helper()
		res := execOK(t, conn, q)
		got := map[string]string{}
		for i := 0; i < res.RowNumber(); i++ {
			got[column(t, res, i, keyCol)+"."+column(t, res, i, colCol)] = column(t, res, i, subCol)
		}
		return got
	}
	want := map[string]string{"PRIMARY.meta_id": "NULL", "post_id.post_id": "NULL", "meta_key.meta_key": "191", "post_meta.post_id": "NULL", "post_meta.meta_value": "20"}
	for name, got := range map[string]map[string]string{
		"SHOW INDEX": subParts("SHOW INDEX FROM wp_postmeta", "Key_name", "Seq_in_index", "Column_name", "Sub_part"),
		"STATISTICS": subParts("SELECT INDEX_NAME, COLUMN_NAME, SUB_PART FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = 'shop' AND TABLE_NAME = 'wp_postmeta'", "INDEX_NAME", "", "COLUMN_NAME", "SUB_PART"),
	} {
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: Sub_part of %s = %q, want %q", name, k, got[k], v)
			}
		}
	}
	create := column(t, execOK(t, conn, "SHOW CREATE TABLE wp_postmeta"), 0, "Create Table")
	for _, k := range []string{"KEY `meta_key` (`meta_key`(191))", "KEY `post_meta` (`post_id`,`meta_value`(20))"} {
		if !strings.Contains(create, k) {
			t.Errorf("SHOW CREATE TABLE has no %s:\n%s", k, create)
		}
	}

	// CREATE INDEX and ALTER TABLE ADD KEY keep the prefix, also across a rebuild.
	execOK(t, conn, "CREATE INDEX meta_value ON wp_postmeta (meta_value(32))")
	execOK(t, conn, "ALTER TABLE wp_postmeta ADD KEY key_value (meta_key(100), meta_value(50))")
	execOK(t, conn, "ALTER TABLE wp_postmeta MODIFY meta_key varchar(250) default NULL")
	got := subParts("SHOW INDEX FROM wp_postmeta", "Key_name", "Seq_in_index", "Column_name", "Sub_part")
	for k, v := range map[string]string{"meta_key.meta_key": "191", "meta_value.meta_value": "32", "key_value.meta_key": "100", "key_value.meta_value": "50", "post_id.post_id": "NULL"} {
		if got[k] != v {
			t.Errorf("after ALTER: Sub_part of %s = %q, want %q", k, got[k], v)
		}
	}

	// A prefix changes what a unique key means: still refused.
	for _, q := range []string{
		"CREATE TABLE u1 (s varchar(100), UNIQUE KEY s (s(10)))",
		"CREATE TABLE u2 (s varchar(100), PRIMARY KEY (s(10)))",
		"CREATE UNIQUE INDEX s ON wp_postmeta (meta_key(10))",
		"ALTER TABLE wp_postmeta ADD UNIQUE KEY s (meta_key(10))",
	} {
		_, err := conn.Execute(q)
		requireMyError(t, err, 1235, "42000")
	}
	// A TEXT key still needs a prefix (1170).
	_, err := conn.Execute("CREATE TABLE t1 (s text, KEY s (s))")
	requireMyError(t, err, 1170, "42000")
}

// REGEXP and RLIKE match anywhere in the string, case-insensitively unless an operand is
// a binary string, as MySQL's ICU regular expressions do. WordPress deletes old feed
// caches with DELETE ... WHERE option_name REGEXP.
func TestRegexp(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "CREATE TABLE wp_options (option_id bigint unsigned NOT NULL auto_increment, option_name varchar(191) NOT NULL, option_value longtext NOT NULL, PRIMARY KEY (option_id), UNIQUE KEY option_name (option_name))")
	execOK(t, conn, "INSERT INTO wp_options (option_name, option_value) VALUES ('rss_0123456789abcdef0123456789abcdef', 'a'), ('rss_0123456789abcdef0123456789abcdef_ts', 'b'), ('rss_short', 'c'), ('siteurl', 'd')")
	res := execOK(t, conn, "DELETE FROM wp_options WHERE option_name REGEXP '^rss_[0-9a-f]{32}(_ts)?$'")
	if res.AffectedRows != 2 {
		t.Errorf("DELETE ... REGEXP affected %d rows, want 2", res.AffectedRows)
	}
	if got := firstColumn(execOK(t, conn, "SELECT option_name FROM wp_options ORDER BY option_name")); strings.Join(got, ",") != "rss_short,siteurl" {
		t.Errorf("rows left: %v", got)
	}
	for q, want := range map[string]string{
		"SELECT 'Hello' REGEXP 'ell'":                                "1",
		"SELECT 'Hello' REGEXP '^HEL'":                               "1",
		"SELECT 'Hello' RLIKE 'x'":                                   "0",
		"SELECT 'Hello' NOT REGEXP 'x'":                              "1",
		"SELECT 'Città' REGEXP 'CITTÀ'":                              "1",
		"SELECT NULL REGEXP 'a'":                                     "NULL",
		"SELECT 'a' REGEXP NULL":                                     "NULL",
		"SELECT 123 REGEXP '^1[0-9]+$'":                              "1",
		"SELECT (SELECT COUNT(*) FROM items WHERE name REGEXP '^p')": "1",
	} {
		if got := column(t, execOK(t, conn, q+" AS r"), 0, "r"); got != want {
			t.Errorf("%s = %s, want %s", q, got, want)
		}
	}
	// A case-sensitive collation matches case-sensitively.
	execOK(t, conn, "CREATE TABLE cs (v varchar(10) COLLATE utf8mb4_bin)")
	execOK(t, conn, "INSERT INTO cs VALUES ('Hello')")
	if n := execOK(t, conn, "SELECT v FROM cs WHERE v REGEXP '^hel'").RowNumber(); n != 0 {
		t.Errorf("utf8mb4_bin REGEXP '^hel': %d rows, want 0", n)
	}
	if n := execOK(t, conn, "SELECT v FROM cs WHERE v REGEXP '^Hel'").RowNumber(); n != 1 {
		t.Errorf("utf8mb4_bin REGEXP '^Hel': %d rows, want 1", n)
	}
	_, err := conn.Execute("SELECT 'a' REGEXP '('")
	requireMyError(t, err, 3685, "HY000")
}

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

// A multi-table DELETE deletes, from each target table, the rows the join matches; the
// rows are chosen before any is deleted. WordPress deletes expired transients with a
// self-join on wp_options.
func TestMultiTableDelete(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "CREATE TABLE wp_options (option_id bigint unsigned NOT NULL auto_increment, option_name varchar(191) NOT NULL, option_value longtext NOT NULL, PRIMARY KEY (option_id), UNIQUE KEY option_name (option_name))")
	execOK(t, conn, `INSERT INTO wp_options (option_name, option_value) VALUES
		('_transient_old', 'x'), ('_transient_timeout_old', '100'),
		('_transient_new', 'y'), ('_transient_timeout_new', '9999999999'),
		('siteurl', 'http://example.test')`)
	res := execOK(t, conn, `DELETE a, b FROM wp_options a, wp_options b
		WHERE a.option_name LIKE '\_transient\_%'
		AND a.option_name NOT LIKE '\_transient\_timeout\_%'
		AND b.option_name = CONCAT( '_transient_timeout_', SUBSTRING( a.option_name, 12 ) )
		AND b.option_value < 1000`)
	if res.AffectedRows != 2 {
		t.Errorf("transient cleanup affected %d rows, want 2", res.AffectedRows)
	}
	got := firstColumn(execOK(t, conn, "SELECT option_name FROM wp_options ORDER BY option_id"))
	if strings.Join(got, ",") != "_transient_new,_transient_timeout_new,siteurl" {
		t.Errorf("rows left: %v", got)
	}

	execOK(t, conn, "CREATE TABLE posts (id int PRIMARY KEY, title varchar(20))")
	execOK(t, conn, "CREATE TABLE meta (id int PRIMARY KEY, post_id int, k varchar(20))")
	execOK(t, conn, "INSERT INTO posts VALUES (1, 'a'), (2, 'b'), (3, 'c')")
	execOK(t, conn, "INSERT INTO meta VALUES (10, 1, 'x'), (11, 1, 'y'), (12, 2, 'z'), (13, 3, 'w')")
	// Both tables; a post matched twice is deleted (and counted) once.
	res = execOK(t, conn, "DELETE p, m FROM posts p JOIN meta m ON m.post_id = p.id WHERE p.id = 1")
	if res.AffectedRows != 3 {
		t.Errorf("DELETE p, m affected %d rows, want 3", res.AffectedRows)
	}
	// One target of a join, and the USING form.
	res = execOK(t, conn, "DELETE m FROM meta m JOIN posts p ON p.id = m.post_id WHERE p.title = 'b'")
	if res.AffectedRows != 1 {
		t.Errorf("DELETE m affected %d rows, want 1", res.AffectedRows)
	}
	res = execOK(t, conn, "DELETE FROM meta USING meta, posts WHERE meta.post_id = posts.id AND posts.title = 'c'")
	if res.AffectedRows != 1 {
		t.Errorf("DELETE ... USING affected %d rows, want 1", res.AffectedRows)
	}
	if got := firstColumn(execOK(t, conn, "SELECT id FROM posts ORDER BY id")); strings.Join(got, ",") != "2,3" {
		t.Errorf("posts left: %v", got)
	}
	if n := execOK(t, conn, "SELECT id FROM meta").RowNumber(); n != 0 {
		t.Errorf("meta left: %d rows", n)
	}

	_, err := conn.Execute("DELETE x FROM posts p WHERE p.id = 2")
	e := requireMyError(t, err, 1109, "42S02")
	if e.Message != "Unknown table 'x' in MULTI DELETE" {
		t.Errorf("message %q", e.Message)
	}
	_, err = conn.Execute("DELETE p FROM posts p WHERE p.nope = 2")
	requireMyError(t, err, 1054, "42S22")
	if n := execOK(t, conn, "SELECT id FROM posts").RowNumber(); n != 2 {
		t.Errorf("a failed DELETE removed rows: %d left", n)
	}
}

// String functions WordPress queries use, with MySQL's positions: 1-based, negative from
// the end, 0 or a length below 1 is ”.
func TestSubstringFunctions(t *testing.T) {
	conn := shopServer(t)
	for q, want := range map[string]string{
		"SELECT SUBSTRING('_transient_abc', 12)":     "abc",
		"SELECT SUBSTRING('Quadratically', 5, 6)":    "ratica",
		"SELECT SUBSTRING('Sakila', -3)":             "ila",
		"SELECT SUBSTRING('Sakila', -5, 3)":          "aki",
		"SELECT SUBSTRING('Sakila', 0)":              "",
		"SELECT SUBSTRING('Sakila', 2, 0)":           "",
		"SELECT SUBSTRING('Sakila', 2, -1)":          "",
		"SELECT SUBSTRING('Sakila', 10)":             "",
		"SELECT SUBSTRING('Sakila', -10)":            "",
		"SELECT SUBSTRING('Sakila' FROM 4)":          "ila",
		"SELECT SUBSTRING('Sakila' FROM 2 FOR 3)":    "aki",
		"SELECT SUBSTR('città', 4)":                  "tà",
		"SELECT MID('Sakila', 2, 2)":                 "ak",
		"SELECT SUBSTRING(12345, 2, 3)":              "234",
		"SELECT SUBSTRING(NULL, 1)":                  "NULL",
		"SELECT SUBSTRING('abc', NULL)":              "NULL",
		"SELECT LEFT('foobarbar', 5)":                "fooba",
		"SELECT LEFT('città', 4)":                    "citt",
		"SELECT LEFT('abc', -1)":                     "",
		"SELECT RIGHT('foobarbar', 4)":               "rbar",
		"SELECT RIGHT('abc', 10)":                    "abc",
		"SELECT RIGHT(NULL, 1)":                      "NULL",
		"SELECT REPLACE('www.mysql.com', 'w', 'Ww')": "WwWwWw.mysql.com",
		"SELECT REPLACE('abc', 'B', 'x')":            "abc",
		"SELECT LTRIM('  barbar ')":                  "barbar ",
		"SELECT RTRIM(' barbar   ')":                 " barbar",
	} {
		if got := column(t, execOK(t, conn, q+" AS r"), 0, "r"); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
	if got := column(t, execOK(t, conn, "SELECT name FROM items WHERE SUBSTRING(name, 1, 1) = 'p'"), 0, "name"); got != "pen" {
		t.Errorf("SUBSTRING in WHERE: %s", got)
	}
}

// SELECT SQL_CALC_FOUND_ROWS ... LIMIT counts the rows the statement would return
// without its LIMIT; FOUND_ROWS() returns that count. After a SELECT without it,
// FOUND_ROWS() is the number of rows that SELECT returned. WordPress paginates posts and
// comments this way.
func TestFoundRows(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "CREATE TABLE posts (id int PRIMARY KEY, status varchar(10))")
	execOK(t, conn, "INSERT INTO posts VALUES (1, 'publish'), (2, 'publish'), (3, 'draft'), (4, 'publish'), (5, 'publish')")
	found := func() string {
		t.Helper()
		return column(t, execOK(t, conn, "SELECT FOUND_ROWS() AS n"), 0, "n")
	}
	res := execOK(t, conn, "SELECT SQL_CALC_FOUND_ROWS posts.id FROM posts WHERE status = 'publish' ORDER BY id DESC LIMIT 0, 2")
	if got := strings.Join(firstColumn(res), ","); got != "5,4" {
		t.Errorf("page 1: %s", got)
	}
	if got := found(); got != "4" {
		t.Errorf("FOUND_ROWS() after SQL_CALC_FOUND_ROWS = %s, want 4", got)
	}
	// FOUND_ROWS() itself returned one row.
	if got := found(); got != "1" {
		t.Errorf("FOUND_ROWS() after SELECT FOUND_ROWS() = %s, want 1", got)
	}
	execOK(t, conn, "SELECT id FROM posts WHERE id > 2 LIMIT 2")
	if got := found(); got != "2" {
		t.Errorf("FOUND_ROWS() after a plain SELECT = %s, want 2", got)
	}
	execOK(t, conn, "SELECT SQL_CALC_FOUND_ROWS status, COUNT(*) FROM posts GROUP BY status LIMIT 1")
	if got := found(); got != "2" {
		t.Errorf("FOUND_ROWS() after a grouped SELECT = %s, want 2", got)
	}
	// Parameters bound in the statement are bound in the count too.
	stmt, err := conn.Prepare("SELECT SQL_CALC_FOUND_ROWS id FROM posts WHERE id > ? LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	if _, err := stmt.Execute(1); err != nil {
		t.Fatal(err)
	}
	if got := found(); got != "4" {
		t.Errorf("FOUND_ROWS() after a prepared SQL_CALC_FOUND_ROWS = %s, want 4", got)
	}
	// SQL_CALC_FOUND_ROWS is only for the outermost SELECT.
	_, err = conn.Execute("SELECT id FROM posts WHERE id IN (SELECT SQL_CALC_FOUND_ROWS id FROM posts)")
	if err == nil {
		t.Error("SQL_CALC_FOUND_ROWS in a subquery must be an error")
	}
}

// LIKE BINARY and REGEXP BINARY compare bytes: case- and accent-sensitive. WP-CLI's
// search-replace finds rows with LIKE BINARY.
func TestLikeBinary(t *testing.T) {
	conn := shopServer(t)
	for q, want := range map[string]string{
		"SELECT 'Contenuto di prova' LIKE BINARY '%di prova%'": "1",
		"SELECT 'Contenuto di prova' LIKE BINARY '%DI PROVA%'": "0",
		"SELECT 'Contenuto di prova' LIKE '%DI PROVA%'":        "1",
		"SELECT BINARY 'abc' LIKE 'ABC'":                       "0",
		"SELECT 'città' NOT LIKE BINARY 'citta'":               "1",
		"SELECT 'Hello' REGEXP BINARY '^hel'":                  "0",
		"SELECT 'Hello' REGEXP BINARY '^Hel'":                  "1",
		"SELECT NULL LIKE BINARY 'a'":                          "NULL",
	} {
		if got := column(t, execOK(t, conn, q+" AS r"), 0, "r"); got != want {
			t.Errorf("%s = %s, want %s", q, got, want)
		}
	}
	if n := execOK(t, conn, "SELECT id FROM items WHERE name LIKE BINARY 'Pen'").RowNumber(); n != 0 {
		t.Errorf("LIKE BINARY 'Pen' matched %d rows", n)
	}
	// BINARY elsewhere is still refused.
	_, err := conn.Execute("SELECT BINARY 'a' = 'A'")
	requireMyError(t, err, 1235, "42000")
}

// A key prefix must fit a string column (1089); a prefix as long as the column is the
// whole column; an ALTER that shortens the column below its prefix drops the prefix,
// as MySQL does, so SHOW CREATE TABLE stays valid for MySQL.
func TestKeyPrefixChecks(t *testing.T) {
	conn := shopServer(t)
	for _, q := range []string{
		"CREATE TABLE bad1 (a int, KEY (a(10)))",
		"CREATE TABLE bad2 (a varchar(10), KEY (a(20)))",
		"CREATE TABLE bad3 (a char(4), KEY (a(5)))",
	} {
		_, err := conn.Execute(q)
		requireMyError(t, err, 1089, "HY000")
	}
	execOK(t, conn, "CREATE TABLE s (a varchar(50), n int, b text, c varbinary(20), KEY ka (a(7)), KEY kb (b(100)), KEY kc (c(4)))")
	for _, q := range []string{
		"CREATE INDEX kn ON s (n(2))",
		"ALTER TABLE s ADD KEY ka2 (a(51))",
	} {
		_, err := conn.Execute(q)
		requireMyError(t, err, 1089, "HY000")
	}
	execOK(t, conn, "CREATE TABLE eq (a varchar(10), KEY ka (a(10)))")
	if got := column(t, execOK(t, conn, "SHOW INDEX FROM eq"), 0, "Sub_part"); got != "NULL" {
		t.Errorf("a prefix as long as the column: Sub_part %s, want NULL", got)
	}
	execOK(t, conn, "ALTER TABLE s MODIFY a varchar(3)")
	res := execOK(t, conn, "SHOW INDEX FROM s")
	for i := 0; i < res.RowNumber(); i++ {
		want := map[string]string{"ka": "NULL", "kb": "100", "kc": "4"}[column(t, res, i, "Key_name")]
		if got := column(t, res, i, "Sub_part"); got != want {
			t.Errorf("after shrinking a: %s Sub_part %s, want %s", column(t, res, i, "Key_name"), got, want)
		}
	}
	if create := column(t, execOK(t, conn, "SHOW CREATE TABLE s"), 0, "Create Table"); !strings.Contains(create, "KEY `ka` (`a`)") {
		t.Errorf("SHOW CREATE TABLE after shrinking a:\n%s", create)
	}
}

// The tables of a multi-table DELETE are named once (1066).
func TestMultiTableDeleteDuplicateTarget(t *testing.T) {
	conn := shopServer(t)
	_, err := conn.Execute("DELETE a, a FROM items a WHERE a.id = 1")
	e := requireMyError(t, err, 1066, "42000")
	if e.Message != "Not unique table/alias: 'a'" {
		t.Errorf("message %q", e.Message)
	}
}

// A target on the nullable side of an outer join skips the rows it did not match, as in
// MySQL, instead of failing on a NULL rowid.
func TestMultiTableDeleteOuterJoin(t *testing.T) {
	conn := shopServer(t)
	execOK(t, conn, "CREATE TABLE posts (id int PRIMARY KEY, title varchar(20))")
	execOK(t, conn, "CREATE TABLE meta (id int PRIMARY KEY, post_id int)")
	execOK(t, conn, "INSERT INTO posts VALUES (1, 'a'), (2, 'b'), (3, 'c')")
	execOK(t, conn, "INSERT INTO meta VALUES (10, 1), (11, 9)")
	res := execOK(t, conn, "DELETE p, m FROM posts p LEFT JOIN meta m ON m.post_id = p.id WHERE p.id < 3")
	if res.AffectedRows != 3 {
		t.Errorf("DELETE p, m affected %d rows, want 3", res.AffectedRows)
	}
	if got := firstColumn(execOK(t, conn, "SELECT id FROM posts")); strings.Join(got, ",") != "3" {
		t.Errorf("posts left: %v", got)
	}
	if got := firstColumn(execOK(t, conn, "SELECT id FROM meta")); strings.Join(got, ",") != "11" {
		t.Errorf("meta left: %v", got)
	}
}

// A multi-table DELETE that fails after deleting some targets' rows leaves no row
// deleted, in autocommit and inside a transaction, where the transaction stays open.
func TestMultiTableDeleteAtomic(t *testing.T) {
	conn := shopServer(t,
		"CREATE TABLE parent (id INTEGER PRIMARY KEY)",
		"CREATE TABLE child (id INTEGER PRIMARY KEY, pid INTEGER REFERENCES parent(id))",
		"CREATE TABLE other (id INTEGER PRIMARY KEY)",
		"INSERT INTO parent VALUES (1), (2)",
		"INSERT INTO child VALUES (1, 1)",
		"INSERT INTO other VALUES (1), (2)",
	)
	// "other" is deleted first; deleting parent 1 then fails on child's foreign key.
	const q = "DELETE o, p FROM other o JOIN parent p ON p.id = o.id"
	_, err := conn.Execute(q)
	requireMyError(t, err, 1451, "23000")
	if n := execOK(t, conn, "SELECT id FROM other").RowNumber(); n != 2 {
		t.Errorf("autocommit: other has %d rows, want 2", n)
	}

	execOK(t, conn, "BEGIN")
	execOK(t, conn, "INSERT INTO other VALUES (3)")
	_, err = conn.Execute(q)
	requireMyError(t, err, 1451, "23000")
	if !conn.IsInTransaction() {
		t.Fatal("the failed statement ended the transaction")
	}
	if got := firstColumn(execOK(t, conn, "SELECT id FROM other ORDER BY id")); strings.Join(got, ",") != "1,2,3" {
		t.Errorf("transaction: other has %v, want 1,2,3", got)
	}
	execOK(t, conn, "COMMIT")
	if n := execOK(t, conn, "SELECT id FROM parent").RowNumber(); n != 2 {
		t.Errorf("parent has %d rows, want 2", n)
	}
}

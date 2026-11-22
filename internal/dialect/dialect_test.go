package dialect

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

func newTranslator(t *testing.T) *Translator {
	t.Helper()
	return New(Config{ServerVersion: 80036})
}

type okCase struct {
	name  string
	query string
	db    string // current database; "shop" unless set to "-" (none)
	kind  Kind
	tdb   string // database the statement runs in
	sql   string
	args  []any
	rules []string
	op    errs.Op
}

func runOK(t *testing.T, cases []okCase) {
	t.Helper()
	tr := newTranslator(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			current := "shop"
			if tc.db == "-" {
				current = ""
			}
			st, e := tr.Translate(tc.query, current)
			if e != nil {
				t.Fatalf("unexpected error: %v", e)
			}
			if st.Kind != tc.kind || st.Database != tc.tdb || st.SQL != tc.sql || st.Op != tc.op {
				t.Fatalf("want kind=%d db=%q op=%d\n  %s\ngot kind=%d db=%q op=%d\n  %s",
					tc.kind, tc.tdb, tc.op, tc.sql, st.Kind, st.Database, st.Op, st.SQL)
			}
			if len(tc.args) != 0 || len(st.Args) != 0 {
				if !reflect.DeepEqual(st.Args, tc.args) {
					t.Fatalf("want args %#v, got %#v", tc.args, st.Args)
				}
			}
			if len(tc.rules) != 0 || len(st.Rules) != 0 {
				if !reflect.DeepEqual(st.Rules, tc.rules) {
					t.Fatalf("want rules %v, got %v", tc.rules, st.Rules)
				}
			}
		})
	}
}

type errCase struct {
	name  string
	query string
	db    string
	code  uint16
	state string
	msg   string // exact message, or a substring when contains is set
	sub   bool
}

func runErr(t *testing.T, cases []errCase) {
	t.Helper()
	tr := newTranslator(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			current := "shop"
			if tc.db == "-" {
				current = ""
			}
			st, e := tr.Translate(tc.query, current)
			if e == nil {
				t.Fatalf("want error %d, got %+v", tc.code, st)
			}
			if e.Code != tc.code || e.State != tc.state {
				t.Fatalf("want %d/%s, got %d/%s %q", tc.code, tc.state, e.Code, e.State, e.Message)
			}
			if tc.sub && !strings.Contains(e.Message, tc.msg) || !tc.sub && e.Message != tc.msg {
				t.Fatalf("want message %q, got %q", tc.msg, e.Message)
			}
		})
	}
}

func TestSelect(t *testing.T) {
	runOK(t, []okCase{
		{name: "no table", query: "SELECT 1", db: "-", kind: KindRead, sql: "SELECT 1"},
		{name: "star", query: "SELECT * FROM t", kind: KindRead, tdb: "shop", sql: "SELECT * FROM `t`"},
		{name: "full", query: "SELECT a, t.b AS x FROM t WHERE a = 'it''s' AND b <=> NULL ORDER BY a DESC LIMIT 5, 10",
			kind: KindRead, tdb: "shop",
			sql:   "SELECT `a`, `t`.`b` AS `x` FROM `t` WHERE ((`a` = ?) AND (`b` IS NULL)) ORDER BY `a` DESC LIMIT 10 OFFSET 5",
			args:  []any{"it's"},
			rules: []string{"R002", "R003", "R005"}},
		{name: "limit offset keyword", query: "SELECT a FROM t LIMIT 10 OFFSET 5", kind: KindRead, tdb: "shop",
			sql: "SELECT `a` FROM `t` LIMIT 10 OFFSET 5", rules: []string{"R005"}},
		{name: "limit max means all rows", query: "SELECT a FROM t LIMIT 5, 18446744073709551615", kind: KindRead, tdb: "shop",
			sql: "SELECT `a` FROM `t` LIMIT -1 OFFSET 5", rules: []string{"R005"}},
		{name: "backslash escape", query: `SELECT a FROM t WHERE a = 'C:\\dir\n'`, kind: KindRead, tdb: "shop",
			sql: "SELECT `a` FROM `t` WHERE (`a` = ?)", args: []any{"C:\\dir\n"}, rules: []string{"R002"}},
		{name: "double-quoted string", query: `SELECT a FROM t WHERE a = "x"`, kind: KindRead, tdb: "shop",
			sql: "SELECT `a` FROM `t` WHERE (`a` = ?)", args: []any{"x"}, rules: []string{"R002"}},
		{name: "adjacent literals and introducers", query: "SELECT a FROM t WHERE a IN ('a' 'b', N'x', _utf8mb4'y')", kind: KindRead, tdb: "shop",
			sql: "SELECT `a` FROM `t` WHERE (`a` IN (?, ?, ?))", args: []any{"ab", "x", "y"}, rules: []string{"R002"}},
		{name: "database qualifier without current database", query: "SELECT shop.t.a, t.* FROM shop.t", db: "-", kind: KindRead, tdb: "shop",
			sql: "SELECT `t`.`a`, `t`.* FROM `t`", rules: []string{"R004"}},
		{name: "qualifier of another database", query: "SELECT a FROM crm.t", kind: KindRead, tdb: "crm",
			sql: "SELECT `a` FROM `t`", rules: []string{"R004"}},
		{name: "numeric literals", query: "SELECT a FROM t WHERE a > 1e3 AND b = 1.50 AND c = -5 AND d = - -5 AND e = TRUE", kind: KindRead, tdb: "shop",
			sql: "SELECT `a` FROM `t` WHERE (((((`a` > 1e+03) AND (`b` = 1.50)) AND (`c` = (- 5))) AND (`d` = (- (- 5)))) AND (`e` = 1))"},
		{name: "predicates", query: "SELECT a FROM t WHERE a IN (1, 2) AND b NOT BETWEEN 1 AND 3 AND c IS NOT NULL AND NOT d AND (e != 1 OR f <> 2 OR g || h)", kind: KindRead, tdb: "shop",
			sql: "SELECT `a` FROM `t` WHERE (((((`a` IN (1, 2)) AND (`b` NOT BETWEEN 1 AND 3)) AND (`c` IS NOT NULL)) AND (NOT `d`)) AND ((((`e` <> 1) OR (`f` <> 2)) OR `g`) OR `h`))"},
		{name: "arithmetic in where", query: "SELECT a FROM t WHERE a + 1 * b - 2 < 3", kind: KindRead, tdb: "shop",
			sql: "SELECT `a` FROM `t` WHERE (((`a` + (1 * `b`)) - 2) < 3)"},
		{name: "hints stripped", query: "SELECT /*+ MAX_EXECUTION_TIME(1) */ SQL_NO_CACHE a FROM t USE INDEX (i)", kind: KindRead, tdb: "shop",
			sql: "SELECT `a` FROM `t`", rules: []string{"R008"}},
		{name: "distinct, alias, position", query: "SELECT DISTINCT x.a AS `we``ird` FROM t AS x ORDER BY 1", kind: KindRead, tdb: "shop",
			sql: "SELECT DISTINCT `x`.`a` AS `we``ird` FROM `t` AS `x` ORDER BY 1"},
		{name: "literals in select list", query: "SELECT 'x', -1, 2.5, NULL", db: "-", kind: KindRead,
			sql: "SELECT ?, (- 1), 2.5, NULL", args: []any{"x"}, rules: []string{"R002"}},
		{name: "trailing semicolon", query: "SELECT 1;", db: "-", kind: KindRead, sql: "SELECT 1"},
	})
}

func TestVersionComments(t *testing.T) {
	runOK(t, []okCase{
		{name: "version above server is skipped", query: "SELECT 1 /*!99999 , 2 */", db: "-", kind: KindRead,
			sql: "SELECT 1", rules: []string{"R001"}},
		{name: "version at server runs", query: "SELECT /*!80036 1, */ 2", db: "-", kind: KindRead, sql: "SELECT 1, 2"},
		{name: "no version runs", query: "SELECT /*! 1, */ 2", db: "-", kind: KindRead, sql: "SELECT 1, 2"},
		{name: "tidb comment is a plain comment", query: "SELECT 1 /*T![x] , 2 */", db: "-", kind: KindRead,
			sql: "SELECT 1", rules: []string{"R001"}},
		{name: "inside a string untouched", query: "SELECT '/*!99999 x */'", db: "-", kind: KindRead,
			sql: "SELECT ?", args: []any{"/*!99999 x */"}, rules: []string{"R002"}},
		{name: "inside a comment untouched", query: "SELECT 1 -- /*!99999 , 2 */\n", db: "-", kind: KindRead, sql: "SELECT 1"},
		{name: "escaped quote before comment", query: `SELECT 'a\'' /*!99999 , 2 */`, db: "-", kind: KindRead,
			sql: "SELECT ?", args: []any{"a'"}, rules: []string{"R001", "R002"}},
	})
}

func TestInsert(t *testing.T) {
	runOK(t, []okCase{
		{name: "values", query: "INSERT INTO t (a, b) VALUES (1, 'x'), (2, NULL)", kind: KindWrite, tdb: "shop", op: errs.OpInsert,
			sql: "INSERT INTO `t` (`a`, `b`) VALUES (1, ?), (2, NULL)", args: []any{"x"}, rules: []string{"R002"}},
		{name: "set form", query: "INSERT INTO t SET a = 1, b = 2 + 3", kind: KindWrite, tdb: "shop", op: errs.OpInsert,
			sql: "INSERT INTO `t` (`a`, `b`) VALUES (1, (2 + 3))"},
		{name: "no column list", query: "INSERT INTO t VALUES (1, 2)", kind: KindWrite, tdb: "shop", op: errs.OpInsert,
			sql: "INSERT INTO `t` VALUES (1, 2)"},
		{name: "empty values", query: "INSERT INTO t VALUES ()", kind: KindWrite, tdb: "shop", op: errs.OpInsert,
			sql: "INSERT INTO `t` DEFAULT VALUES", rules: []string{"R006"}},
		{name: "qualified", query: "INSERT LOW_PRIORITY INTO shop.t VALUES (1)", db: "-", kind: KindWrite, tdb: "shop", op: errs.OpInsert,
			sql: "INSERT INTO `t` VALUES (1)", rules: []string{"R004", "R008"}},
	})
}

func TestUpdate(t *testing.T) {
	runOK(t, []okCase{
		{name: "simple", query: "UPDATE t SET a = a + 1, b = 'x' WHERE id = 3", kind: KindWrite, tdb: "shop", op: errs.OpUpdate,
			sql: "UPDATE `t` SET `a` = (`a` + 1), `b` = ? WHERE (`id` = 3)", args: []any{"x"}, rules: []string{"R002"}},
		{name: "later column reads itself", query: "UPDATE t SET a = 1, b = b + 1", kind: KindWrite, tdb: "shop", op: errs.OpUpdate,
			sql: "UPDATE `t` SET `a` = 1, `b` = (`b` + 1)"},
		{name: "alias", query: "UPDATE t AS x SET x.a = 1 WHERE x.id = 1", kind: KindWrite, tdb: "shop", op: errs.OpUpdate,
			sql: "UPDATE `t` AS `x` SET `a` = 1 WHERE (`x`.`id` = 1)"},
		{name: "order and limit", query: "UPDATE t SET a = 1 WHERE b > 0 ORDER BY id DESC LIMIT 2", kind: KindWrite, tdb: "shop", op: errs.OpUpdate,
			sql: "UPDATE `t` SET `a` = 1 WHERE rowid IN (SELECT rowid FROM `t` WHERE (`b` > 0) ORDER BY `id` DESC LIMIT 2)", rules: []string{"R007"}},
	})
}

func TestDelete(t *testing.T) {
	runOK(t, []okCase{
		{name: "simple", query: "DELETE FROM t WHERE id = 1", kind: KindWrite, tdb: "shop", op: errs.OpDelete,
			sql: "DELETE FROM `t` WHERE (`id` = 1)"},
		{name: "all rows", query: "DELETE QUICK FROM t", kind: KindWrite, tdb: "shop", op: errs.OpDelete,
			sql: "DELETE FROM `t`", rules: []string{"R008"}},
		{name: "limit", query: "DELETE FROM t AS x WHERE x.a = 1 LIMIT 1", kind: KindWrite, tdb: "shop", op: errs.OpDelete,
			sql: "DELETE FROM `t` AS `x` WHERE rowid IN (SELECT rowid FROM `t` AS `x` WHERE (`x`.`a` = 1) LIMIT 1)", rules: []string{"R007"}},
	})
}

func TestUse(t *testing.T) {
	tr := newTranslator(t)
	for q, want := range map[string]string{"USE shop": "shop", "use `a b`": "a b"} {
		st, e := tr.Translate(q, "")
		if e != nil {
			t.Fatalf("%s: %v", q, e)
		}
		if st.Kind != KindUse || st.UseDB != want {
			t.Fatalf("%s: want USE %q, got %+v", q, want, st)
		}
	}
}

func TestParseErrors(t *testing.T) {
	const prefix = "You have an error in your SQL syntax; check the manual that corresponds to your MySQL server version for the right syntax to use near "
	runErr(t, []errCase{
		{name: "end of input", query: "SELECT * FROM", code: 1064, state: "42000", msg: prefix + "'' at line 1"},
		{name: "bad keyword", query: "SELEC 1", code: 1064, state: "42000", msg: prefix + "'SELEC 1' at line 1"},
		{name: "second line", query: "SELECT 1\nFROM t WHERE", code: 1064, state: "42000", msg: prefix + "'' at line 2"},
		{name: "placeholder in text protocol", query: "SELECT a FROM t WHERE a = ?", code: 1064, state: "42000", msg: prefix + "'?' at line 1"},
		{name: "several statements", query: "SELECT 1; SELECT 2", code: 1064, state: "42000", msg: prefix + "'SELECT 2' at line 1"},
	})
}

func TestStatementErrors(t *testing.T) {
	runErr(t, []errCase{
		{name: "no database", query: "SELECT * FROM t", db: "-", code: 1046, state: "3D000", msg: "No database selected"},
		{name: "empty query", query: "  ", code: 1065, state: "42000", msg: "Query was empty"},
		{name: "only a comment", query: "/* x */", code: 1065, state: "42000", msg: "Query was empty"},
		{name: "insert row length", query: "INSERT INTO t (a, b) VALUES (1, 2), (3)", code: 1136, state: "21S01",
			msg: "Column count doesn't match value count at row 2"},
		{name: "insert rows differ", query: "INSERT INTO t VALUES (1, 2), (3, 4), (5)", code: 1136, state: "21S01",
			msg: "Column count doesn't match value count at row 3"},
		{name: "grant", query: "GRANT SELECT ON *.* TO u", code: 1227, state: "42000", msg: "GRANT OPTION", sub: true},
		{name: "create user", query: "CREATE USER u", code: 1227, state: "42000", msg: "CREATE USER", sub: true},
	})
}

func TestUnsupported(t *testing.T) {
	cases := map[string]string{
		"SELECT * FROM t JOIN u ON t.a = u.a":                    "JOIN",
		"SELECT * FROM t, u":                                     "JOIN",
		"SELECT * FROM (SELECT 1) AS d":                          "derived table",
		"SELECT a FROM t GROUP BY a":                             "GROUP BY",
		"SELECT a FROM t HAVING a > 1":                           "HAVING",
		"SELECT a FROM t WHERE upper(a) = 'X'":                   "function UPPER",
		"SELECT count(*) FROM t":                                 "aggregate COUNT",
		"SELECT a FROM t WHERE a LIKE 'x%'":                      "LIKE",
		"SELECT a FROM t WHERE a / 2 = 1":                        "operator /",
		"SELECT a FROM t WHERE a DIV 2 = 1":                      "operator DIV",
		"SELECT a FROM t WHERE a XOR b":                          "operator XOR",
		"SELECT a + 1 FROM t":                                    "expression in select list",
		"SELECT a FROM t WHERE a = 0x41":                         "hexadecimal or bit literal",
		"SELECT a FROM t WHERE a = b'1'":                         "hexadecimal or bit literal",
		"SELECT a FROM t WHERE a = _binary'x'":                   "binary string literal",
		"SELECT a FROM t WHERE a = _latin1'x'":                   "character set introducer _latin1",
		"SELECT a FROM t WHERE a = 18446744073709551615":         "integer literal",
		"SELECT a FROM t WHERE a = 0.12345678901234567":          "decimal literal",
		"SELECT a FROM t WHERE a IN (SELECT 1)":                  "subquery",
		"SELECT a FROM t WHERE a IS TRUE":                        "IS TRUE",
		"SELECT a FROM t FOR UPDATE":                             "FOR UPDATE",
		"SELECT SQL_CALC_FOUND_ROWS a FROM t":                    "SQL_CALC_FOUND_ROWS",
		"SELECT a FROM t UNION SELECT a FROM u":                  "UNION, EXCEPT or INTERSECT",
		"WITH c AS (SELECT 1) SELECT * FROM c":                   "WITH",
		"INSERT INTO t (a) VALUES (DEFAULT)":                     "DEFAULT",
		"INSERT INTO t (a, b) VALUES (1, a)":                     "column reference in VALUES",
		"INSERT INTO t VALUES (), ()":                            "VALUES () with several rows",
		"REPLACE INTO t VALUES (1)":                              "REPLACE",
		"INSERT IGNORE INTO t VALUES (1)":                        "INSERT IGNORE",
		"INSERT INTO t VALUES (1) ON DUPLICATE KEY UPDATE a = 2": "ON DUPLICATE KEY UPDATE",
		"INSERT INTO t SELECT * FROM u":                          "INSERT ... SELECT",
		"UPDATE t SET a = a + 1, b = a":                          "UPDATE assignment order",
		"UPDATE t SET A = 1, b = a":                              "UPDATE assignment order",
		"UPDATE t SET a = 1, a = 2":                              "column assigned twice",
		"UPDATE t, u SET t.a = u.a":                              "multi-table UPDATE",
		"UPDATE IGNORE t SET a = 1":                              "UPDATE IGNORE",
		"DELETE t FROM t JOIN u ON t.a = u.a":                    "multi-table DELETE",
		"DELETE IGNORE FROM t":                                   "DELETE IGNORE",
		"SET @a = 1":                                             "SET",
		"LOCK TABLES t READ":                                     "LOCK TABLES",
		"SHOW TABLES":                                            "SHOW",
		"CREATE TABLE x (a INT)":                                 "CREATE TABLE",
	}
	tr := newTranslator(t)
	for q, construct := range cases {
		_, e := tr.Translate(q, "shop")
		if e == nil {
			t.Errorf("%s: want 1235, got success", q)
			continue
		}
		want := "mysqlite: unsupported construct " + construct + ": "
		if e.Code != 1235 || e.State != "42000" || !strings.HasPrefix(e.Message, want) {
			t.Errorf("%s: want 1235 %q..., got %d %q", q, want, e.Code, e.Message)
		}
	}
}

func TestVersionNumber(t *testing.T) {
	for in, want := range map[string]int{"8.0.36-mysqlite": 80036, "8.4.2": 80402, "5.7.44-log": 50744} {
		got, err := VersionNumber(in)
		if err != nil || got != want {
			t.Errorf("%q: want %d, got %d %v", in, want, got, err)
		}
	}
	for _, bad := range []string{"", "8", "8.0", "x.y.z", "8.100.1"} {
		if _, err := VersionNumber(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

// The registry order is the rule order. rule-order.md must list the same rules in the
// same order, so documentation and code cannot drift apart.
func TestRuleOrder(t *testing.T) {
	want := []string{
		"R001 version-comment-gate",
		"R002 string-literal-param",
		"R003 null-safe-equal",
		"R004 database-qualifier",
		"R005 limit-offset",
		"R006 insert-default-values",
		"R007 update-delete-limit",
		"R008 hint-strip",
	}
	var got []string
	for _, r := range Rules() {
		got = append(got, r.ID+" "+r.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rule registry changed:\nwant %v\ngot  %v", want, got)
	}

	doc, err := os.ReadFile("../../docs/agent-notes/rule-order.md")
	if err != nil {
		t.Fatal(err)
	}
	last := -1
	for _, r := range Rules() {
		row := "| " + r.ID + " | `" + r.Name + "` |"
		i := strings.Index(string(doc), row)
		if i < 0 {
			t.Fatalf("rule-order.md has no row starting %q", row)
		}
		if i < last {
			t.Fatalf("rule-order.md lists %s out of order", r.ID)
		}
		last = i
	}
}

func TestSelectShape(t *testing.T) {
	tr := newTranslator(t)
	st, e := tr.Translate("SELECT *, x.*, A, x.b AS bee, 1, -12, 2.50 /* c */, 1e3, 'città', 'a' 'b', NULL FROM items AS x", "shop")
	if e != nil {
		t.Fatal(e)
	}
	want := &Shape{
		Table: "items",
		Alias: "x",
		Items: []meta.Item{
			{Star: true},
			{Star: true, Qualifier: "x"},
			{Column: "A", Name: "A"},
			{Qualifier: "x", Column: "b", Name: "bee"},
			{Literal: &meta.Literal{Kind: meta.LiteralInt, Text: "1"}, Name: "1"},
			{Literal: &meta.Literal{Kind: meta.LiteralInt, Text: "-12"}, Name: "-12"},
			{Literal: &meta.Literal{Kind: meta.LiteralDecimal, Text: "2.50"}, Name: "2.50"},
			{Literal: &meta.Literal{Kind: meta.LiteralFloat, Text: "1e3"}, Name: "1e3"},
			{Literal: &meta.Literal{Kind: meta.LiteralString, Text: "città"}, Name: "città"},
			{Literal: &meta.Literal{Kind: meta.LiteralString, Text: "ab"}, Name: "ab"},
			{Literal: &meta.Literal{Kind: meta.LiteralNull, Text: "NULL"}, Name: "NULL"},
		},
	}
	if !reflect.DeepEqual(st.Shape, want) {
		t.Fatalf("want\n%+v\ngot\n%+v", want, st.Shape)
	}

	st, e = tr.Translate("SELECT 1 AS one", "")
	if e != nil {
		t.Fatal(e)
	}
	if st.Shape == nil || st.Shape.Table != "" || st.Shape.Items[0].Name != "one" {
		t.Fatalf("a SELECT without FROM has a shape with no table: %+v", st.Shape)
	}

	st, e = tr.Translate("SELECT name AS n FROM items AS x WHERE x.id > 1 AND (price = 2 OR NOT code) ORDER BY n, 1, `x`.a DESC", "shop")
	if e != nil {
		t.Fatal(e)
	}
	refs := []meta.Ref{
		{Qualifier: "x", Column: "id", Clause: meta.ClauseWhere},
		{Column: "price", Clause: meta.ClauseWhere},
		{Column: "code", Clause: meta.ClauseWhere},
		{Column: "n", Clause: meta.ClauseOrder},
		{Qualifier: "x", Column: "a", Clause: meta.ClauseOrder},
	}
	if !reflect.DeepEqual(st.Shape.Refs, refs) {
		t.Fatalf("want refs %+v, got %+v", refs, st.Shape.Refs)
	}

	st, e = tr.Translate("DELETE FROM items", "shop")
	if e != nil {
		t.Fatal(e)
	}
	if st.Shape != nil {
		t.Fatalf("only SELECT has a result shape, got %+v", st.Shape)
	}
}

func TestTransactionStatements(t *testing.T) {
	tr := newTranslator(t)
	cases := []struct {
		query string
		want  Transaction
	}{
		{"BEGIN", Transaction{Op: TxBegin}},
		{"start transaction", Transaction{Op: TxBegin}},
		{"START TRANSACTION READ WRITE", Transaction{Op: TxBegin}},
		{"START TRANSACTION READ ONLY", Transaction{Op: TxBegin, ReadOnly: true}},
		{"START TRANSACTION WITH CONSISTENT SNAPSHOT", Transaction{Op: TxBegin, Snapshot: true}},
		// mysqldump --single-transaction sends the clause in a version comment.
		{"START TRANSACTION /*!40100 WITH CONSISTENT SNAPSHOT */", Transaction{Op: TxBegin, Snapshot: true}},
		{"START TRANSACTION /* WITH CONSISTENT SNAPSHOT */", Transaction{Op: TxBegin}},
		{"COMMIT", Transaction{Op: TxCommit}},
		{"COMMIT AND NO CHAIN", Transaction{Op: TxCommit}},
		{"COMMIT NO RELEASE", Transaction{Op: TxCommit}},
		{"ROLLBACK", Transaction{Op: TxRollback}},
		{"ROLLBACK AND NO CHAIN NO RELEASE", Transaction{Op: TxRollback}},
	}
	for _, tc := range cases {
		for _, current := range []string{"shop", ""} {
			st, e := tr.Translate(tc.query, current)
			if e != nil {
				t.Fatalf("%s: %v", tc.query, e)
			}
			if st.Kind != KindTransaction || st.Tx == nil || *st.Tx != tc.want {
				t.Fatalf("%s: want %+v, got kind %d tx %+v", tc.query, tc.want, st.Kind, st.Tx)
			}
		}
	}
}

func TestTransactionRefusals(t *testing.T) {
	const prefix = "You have an error in your SQL syntax; check the manual that corresponds to your MySQL server version for the right syntax to use near "
	runErr(t, []errCase{
		// TiDB extensions are syntax errors in MySQL.
		{name: "pessimistic", query: "BEGIN PESSIMISTIC", code: 1064, state: "42000", msg: prefix + "'PESSIMISTIC' at line 1"},
		{name: "optimistic", query: "begin optimistic", code: 1064, state: "42000", msg: prefix + "'optimistic' at line 1"},
		{name: "causal", query: "START TRANSACTION WITH CAUSAL CONSISTENCY ONLY", code: 1064, state: "42000",
			msg: prefix + "'CAUSAL CONSISTENCY ONLY' at line 1"},
		{name: "as of", query: "START TRANSACTION READ ONLY AS OF TIMESTAMP '2024-01-01'", code: 1064, state: "42000",
			msg: prefix + "'AS OF TIMESTAMP '2024-01-01'' at line 1"},
		{name: "chain", query: "COMMIT AND CHAIN", code: 1235, state: "42000", msg: "AND CHAIN", sub: true},
		{name: "release", query: "ROLLBACK RELEASE", code: 1235, state: "42000", msg: "RELEASE", sub: true},
		{name: "savepoint", query: "SAVEPOINT a", code: 1235, state: "42000", msg: "SAVEPOINT", sub: true},
		{name: "rollback to", query: "ROLLBACK TO a", code: 1235, state: "42000", msg: "SAVEPOINT", sub: true},
		{name: "release savepoint", query: "RELEASE SAVEPOINT a", code: 1235, state: "42000", msg: "SAVEPOINT", sub: true},
	})
}

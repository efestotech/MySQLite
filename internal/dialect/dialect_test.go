package dialect

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
	"mysqlite/internal/vars"
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
			sql:  "UPDATE `t` SET `a` = (`a` + 1), `b` = ? WHERE (`id` = 3) AND NOT ((`a` IS ((`a` + 1)) COLLATE BINARY) AND (`b` IS (?) COLLATE BINARY))",
			args: []any{"x", "x"}, rules: []string{"R002"}},
		{name: "later column reads itself", query: "UPDATE t SET a = 1, b = b + 1", kind: KindWrite, tdb: "shop", op: errs.OpUpdate,
			sql: "UPDATE `t` SET `a` = 1, `b` = (`b` + 1) WHERE NOT ((`a` IS (1) COLLATE BINARY) AND (`b` IS ((`b` + 1)) COLLATE BINARY))"},
		{name: "alias", query: "UPDATE t AS x SET x.a = 1 WHERE x.id = 1", kind: KindWrite, tdb: "shop", op: errs.OpUpdate,
			sql: "UPDATE `t` AS `x` SET `a` = 1 WHERE (`x`.`id` = 1) AND NOT ((`a` IS (1) COLLATE BINARY))"},
		{name: "order and limit", query: "UPDATE t SET a = 1 WHERE b > 0 ORDER BY id DESC LIMIT 2", kind: KindWrite, tdb: "shop", op: errs.OpUpdate,
			sql:   "UPDATE `t` SET `a` = 1 WHERE rowid IN (SELECT rowid FROM `t` WHERE (`b` > 0) ORDER BY `id` DESC LIMIT 2) AND NOT ((`a` IS (1) COLLATE BINARY))",
			rules: []string{"R007"}},
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
		"SELECT * FROM t NATURAL JOIN u":                         "NATURAL JOIN",
		"SELECT * FROM t RIGHT JOIN u USING (a)":                 "RIGHT JOIN with USING",
		"SELECT * FROM t JOIN other.u ON t.a = u.a":              "cross-database reference",
		"SELECT * FROM t, LATERAL (SELECT 1 AS a) AS d":          "LATERAL",
		"SELECT a FROM t GROUP BY a WITH ROLLUP":                 "WITH ROLLUP",
		"SELECT sum(a) OVER () FROM t":                           "window function",
		"SELECT a FROM t WHERE format(a, 1) = 1":                 "function FORMAT",
		"SELECT std(a) FROM t":                                   "aggregate STDDEV_POP",
		"SELECT a FROM t WHERE a ILIKE 'x%'":                     "ILIKE",
		"SELECT a FROM t WHERE a & 2 = 1":                        "operator &",
		"SELECT a FROM t WHERE a << 2 = 1":                       "operator <<",
		"SELECT a FROM t WHERE a ^ b":                            "operator ^",
		"SELECT a | 1 FROM t":                                    "operator |",
		"SELECT a FROM t WHERE a = 0x41":                         "hexadecimal or bit literal",
		"SELECT a FROM t WHERE a = b'1'":                         "hexadecimal or bit literal",
		"SELECT a FROM t WHERE a = _binary'x'":                   "binary string literal",
		"SELECT a FROM t WHERE a = _latin1'x'":                   "character set introducer _latin1",
		"SELECT a FROM t WHERE a = 18446744073709551615":         "integer literal",
		"SELECT a FROM t WHERE a = 0.12345678901234567":          "decimal literal",
		"SELECT a FROM t WHERE a > ALL (SELECT 1)":               "> ALL subquery",
		"SELECT a FROM t FOR UPDATE":                             "FOR UPDATE",
		"SELECT SQL_CALC_FOUND_ROWS a FROM t":                    "SQL_CALC_FOUND_ROWS",
		"SELECT a FROM t INTERSECT ALL SELECT a FROM u":          "INTERSECT ALL",
		"WITH c AS (SELECT 1) SELECT * FROM c":                   "WITH",
		"INSERT INTO t (a) VALUES (DEFAULT)":                     "DEFAULT",
		"INSERT INTO t (a, b) VALUES (1, a)":                     "column reference in VALUES",
		"INSERT INTO t VALUES (), ()":                            "VALUES () with several rows",
		"REPLACE INTO t VALUES (1)":                              "REPLACE",
		"INSERT IGNORE INTO t VALUES (1)":                        "INSERT IGNORE",
		"INSERT INTO t VALUES (1) ON DUPLICATE KEY UPDATE a = 2": "ON DUPLICATE KEY UPDATE",
		"INSERT INTO t TABLE u":                                  "TABLE or VALUES statement",
		"UPDATE t SET a = a + 1, b = a":                          "UPDATE assignment order",
		"UPDATE t SET A = 1, b = a":                              "UPDATE assignment order",
		"UPDATE t SET a = 1, a = 2":                              "column assigned twice",
		"UPDATE t, u SET t.a = u.a":                              "multi-table UPDATE",
		"UPDATE IGNORE t SET a = 1":                              "UPDATE IGNORE",
		"DELETE t FROM t JOIN u ON t.a = u.a":                    "multi-table DELETE",
		"DELETE IGNORE FROM t":                                   "DELETE IGNORE",
		"SET @a = 1":                                             "user variable",
		"LOCK TABLES t READ":                                     "LOCK TABLES",
		"SHOW PROCEDURE STATUS":                                  "SHOW",
		"CREATE TEMPORARY TABLE x (a INT)":                       "CREATE TEMPORARY TABLE",
		"RENAME TABLE x TO y":                                    "RENAME TABLE",
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
		"R009 create-table",
		"R010 create-table-keys",
		"R011 current-database",
		"R012 function-table",
		"R013 subquery",
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
	}
	want.Items = []meta.Item{
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
	}
	st.Shape.Exprs = nil          // arithmetic checks are covered by the wire tests
	st.Shape.From = meta.From{}   // FROM clauses are covered by TestJoins
	st.Shape.Group = meta.Group{} // grouping is covered by TestAggregates
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
		{"START TRANSACTION READ WRITE", Transaction{Op: TxBegin, ReadWrite: true}},
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

// Write describes INSERT, UPDATE and DELETE for the checks and counts that need the
// catalog (meta.CheckInsert, meta.CheckRefs, matched rows).
func TestWriteShape(t *testing.T) {
	tr := newTranslator(t)
	l := func(k meta.LiteralKind, text string) *meta.Literal { return &meta.Literal{Kind: k, Text: text} }
	cases := []struct {
		query string
		want  Write
	}{
		{"INSERT INTO t (a, b) VALUES (1, 'x'), (-2, NULL), (1 + 1, 2.5)", Write{Table: "t", Columns: []string{"a", "b"},
			Rows: [][]*meta.Literal{{l(meta.LiteralInt, "1"), l(meta.LiteralString, "x")}, {l(meta.LiteralInt, "-2"), l(meta.LiteralNull, "NULL")}, {nil, l(meta.LiteralDecimal, "2.5")}}}},
		{"INSERT INTO t VALUES (1)", Write{Table: "t", Rows: [][]*meta.Literal{{l(meta.LiteralInt, "1")}}}},
		{"INSERT INTO t VALUES ()", Write{Table: "t", Rows: [][]*meta.Literal{{}}}},
		{"INSERT INTO t SET a = 1, b = 2 + 3", Write{Table: "t", Columns: []string{"a", "b"}, Rows: [][]*meta.Literal{{l(meta.LiteralInt, "1"), nil}}}},
		{"UPDATE t AS x SET a = 'v', b = b + 1 WHERE x.id = 1 ORDER BY c", Write{Table: "t", Alias: "x",
			Set:       map[string]*meta.Literal{"a": l(meta.LiteralString, "v"), "b": nil},
			Refs:      []meta.Ref{{Qualifier: "x", Column: "id", Clause: meta.ClauseWhere}, {Column: "c", Clause: meta.ClauseOrder}},
			MatchSQL:  "SELECT count(*) FROM (SELECT rowid FROM `t` AS `x` WHERE (`x`.`id` = 1) ORDER BY `c`)",
			MatchArgs: nil}},
		{"UPDATE t SET A = 'v' WHERE b = 'w'", Write{Table: "t",
			Set:       map[string]*meta.Literal{"a": l(meta.LiteralString, "v")},
			Refs:      []meta.Ref{{Column: "b", Clause: meta.ClauseWhere}},
			MatchSQL:  "SELECT count(*) FROM `t` WHERE (`b` = ?)",
			MatchArgs: []any{"w"}}},
		{"UPDATE t SET a = 1", Write{Table: "t", Set: map[string]*meta.Literal{"a": l(meta.LiteralInt, "1")}, MatchSQL: "SELECT count(*) FROM `t`"}},
		{"DELETE FROM t AS x WHERE x.a = 1 ORDER BY b LIMIT 1", Write{Table: "t", Alias: "x",
			Refs: []meta.Ref{{Qualifier: "x", Column: "a", Clause: meta.ClauseWhere}, {Column: "b", Clause: meta.ClauseOrder}}}},
	}
	for _, tc := range cases {
		st, e := tr.Translate(tc.query, "shop")
		if e != nil {
			t.Fatalf("%s: %v", tc.query, e)
		}
		if st.Write != nil {
			st.Write.Exprs = nil // arithmetic checks are covered by the wire tests
		}
		if st.Write == nil || !reflect.DeepEqual(*st.Write, tc.want) {
			t.Fatalf("%s:\nwant %+v\ngot  %+v", tc.query, tc.want, st.Write)
		}
	}
}

// System variables become *Variable arguments, read by the executor
// (docs/04-session-catalog.md section 1).
func TestSystemVariables(t *testing.T) {
	tr := newTranslator(t)
	st, e := tr.Translate("SELECT @@version, @@GLOBAL.max_allowed_packet, @@local.sql_mode AS m FROM items WHERE @@session.autocommit = 1", "shop")
	if e != nil {
		t.Fatal(e)
	}
	if want := "SELECT ?, ?, ? AS `m` FROM `items` WHERE (? = 1)"; st.SQL != want {
		t.Fatalf("sql: want %q, got %q", want, st.SQL)
	}
	wantArgs := []any{
		&Variable{Name: "version", Scope: vars.ReadDefault},
		&Variable{Name: "max_allowed_packet", Scope: vars.ReadGlobal},
		&Variable{Name: "sql_mode", Scope: vars.ReadSession},
		&Variable{Name: "autocommit", Scope: vars.ReadSession},
	}
	if !reflect.DeepEqual(st.Args, wantArgs) {
		t.Fatalf("args: got %#v", st.Args)
	}
	wantItems := []meta.Item{
		{Var: &meta.Variable{Name: "version", Scope: vars.ReadDefault}, Name: "@@version"},
		{Var: &meta.Variable{Name: "max_allowed_packet", Scope: vars.ReadGlobal}, Name: "@@GLOBAL.max_allowed_packet"},
		{Var: &meta.Variable{Name: "sql_mode", Scope: vars.ReadSession}, Name: "m"},
	}
	if !reflect.DeepEqual(st.Shape.Items, wantItems) {
		t.Fatalf("items: got %+v", st.Shape.Items)
	}

	if _, e := tr.Translate("SELECT @x", "shop"); e == nil || e.Code != 1235 {
		t.Fatalf("user variable: want 1235, got %v", e)
	}
}

func TestSetStatements(t *testing.T) {
	tr := newTranslator(t)
	cases := []struct {
		query string
		want  []Assignment
	}{
		{"SET autocommit = OFF, @@session.sql_mode = 'ANSI'", []Assignment{
			{Kind: AssignVariable, Name: "autocommit", Value: SetValue{Word: "OFF"}},
			{Kind: AssignVariable, Name: "sql_mode", Value: SetValue{Literal: &meta.Literal{Kind: meta.LiteralString, Text: "ANSI"}}},
		}},
		{"SET GLOBAL wait_timeout = DEFAULT", []Assignment{
			{Kind: AssignVariable, Name: "wait_timeout", Global: true, Value: SetValue{Default: true}},
		}},
		{"SET NAMES utf8mb4 COLLATE utf8mb4_bin", []Assignment{
			{Kind: AssignNames, Collation: "utf8mb4_bin", Value: SetValue{Literal: &meta.Literal{Kind: meta.LiteralString, Text: "utf8mb4"}}},
		}},
		{"SET TRANSACTION READ ONLY", []Assignment{
			{Kind: AssignVariable, Name: "transaction_read_only", OneShot: true, Value: SetValue{Literal: &meta.Literal{Kind: meta.LiteralInt, Text: "1"}}},
		}},
		{"SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED", []Assignment{
			{Kind: AssignVariable, Name: "transaction_isolation", Value: SetValue{Literal: &meta.Literal{Kind: meta.LiteralString, Text: "READ-COMMITTED"}}},
		}},
	}
	for _, c := range cases {
		st, e := tr.Translate(c.query, "shop")
		if e != nil {
			t.Errorf("%s: %v", c.query, e)
			continue
		}
		if st.Kind != KindSet || !reflect.DeepEqual(st.Set, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.query, st.Set, c.want)
		}
	}
	if _, e := tr.Translate("SET NAMES klingon", "shop"); e == nil || e.Code != 1115 {
		t.Errorf("SET NAMES klingon: got %v", e)
	}
}

// Column types as MySQL 8.0 prints them (the sidecar type) and the SQLite declared type.
func TestCreateTableTypes(t *testing.T) {
	tr := newTranslator(t)
	cases := []struct{ in, mysqlType, decl string }{
		{"INT", "int", "int"},
		{"INTEGER(11)", "int", "int"},
		{"INT(5) UNSIGNED ZEROFILL", "int(5) unsigned zerofill", "int unsigned zerofill(5)"},
		{"BOOL", "tinyint(1)", "tinyint(1)"},
		{"BIGINT UNSIGNED", "bigint unsigned", "bigint unsigned"},
		{"DECIMAL", "decimal(10,0)", "decimal(10,0)"},
		{"NUMERIC(8,2) UNSIGNED", "decimal(8,2) unsigned", "decimal unsigned(8,2)"},
		{"REAL", "double", "double"},
		{"FLOAT(7,4)", "float(7,4)", "float(7,4)"},
		{"CHAR", "char(1)", "char(1)"},
		{"VARBINARY(4)", "varbinary(4)", "varbinary BLOB(4)"},
		{"TEXT(300)", "text", "text"},
		{"TEXT(60)", "tinytext", "tinytext"}, // 60 x 4 bytes fit TINYTEXT
		{"TEXT(70)", "text", "text"},
		{"LONGBLOB", "longblob", "longblob"},
		{"DATETIME(3)", "datetime(3)", "datetime TEXT(3)"},
		{"TIMESTAMP", "timestamp", "timestamp TEXT"},
		{"TIME", "time", "time TEXT"},
		{"YEAR", "year", "year"},
		{"JSON", "json", "json TEXT"},
	}
	for _, c := range cases {
		st, e := tr.Translate("CREATE TABLE t (a "+c.in+")", "shop")
		if e != nil {
			t.Errorf("%s: %v", c.in, e)
			continue
		}
		var got string
		for _, f := range st.Create.Facts {
			if f.Column == "a" && f.Key == "type" {
				got = f.Value
			}
		}
		if got != c.mysqlType {
			t.Errorf("%s: MySQL type %q, want %q", c.in, got, c.mysqlType)
		}
		if !strings.Contains(st.Create.SQL[0], "`a` "+c.decl) {
			t.Errorf("%s: declared type in %s, want %q", c.in, st.Create.SQL[0], c.decl)
		}
	}
}

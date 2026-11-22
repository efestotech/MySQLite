// Package dialect translates MySQL statements to SQLite (docs/05-dialect.md).
//
// Every query is parsed with the TiDB parser (ADR 0001) and SQLite SQL is emitted from
// the AST; the parser's Restore output is never used. Anything outside the supported
// whitelist is an error (strict mode, docs/05-dialect.md section 2): a statement is
// either fully translated or rejected.
//
// Emitter invariants, which are conventions rather than rules:
//   - identifiers are quoted with backticks, never double quotes: SQLite treats an
//     unknown double-quoted identifier as a string literal (DQS), which would turn an
//     unknown column into a value instead of error 1054;
//   - every operator is parenthesized, so MySQL and SQLite precedence never matter;
//   - floating-point literals keep an exponent, so SQLite reads them as REAL.
package dialect

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	// Registers the value-expression implementation the parser needs (ADR 0001).
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"

	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
	"mysqlite/internal/vars"
)

// defaultSQLMode is the MySQL 8.0 default; SET sql_mode changes it (SetSQLMode).
const defaultSQLMode = "ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION"

// Config configures a Translator.
type Config struct {
	// ServerVersion is the advertised version as a number, 8.0.36 -> 80036, which gates
	// /*!NNNNN ... */ comments.
	ServerVersion int
}

// Kind says how a translated statement is executed.
type Kind int

const (
	// KindRead runs on a read connection (SELECT).
	KindRead Kind = iota + 1
	// KindWrite runs on the writer (INSERT, UPDATE, DELETE).
	KindWrite
	// KindUse changes the session's current database (USE db).
	KindUse
	// KindTransaction is BEGIN, START TRANSACTION, COMMIT or ROLLBACK (Statement.Tx).
	KindTransaction
	// KindShow is a SHOW statement (Statement.Show).
	KindShow
	// KindSet is SET (Statement.Set).
	KindSet
	// KindDDL is CREATE TABLE (Statement.Create).
	KindDDL
)

// String is the router decision recorded in the query journal.
func (k Kind) String() string {
	switch k {
	case KindRead:
		return "read"
	case KindWrite:
		return "write"
	case KindUse:
		return "use"
	case KindTransaction:
		return "transaction"
	case KindShow:
		return "show"
	case KindDDL:
		return "ddl"
	case KindSet:
		return "set"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// SessionValue is an argument the executor binds from the session: the value of a
// session function (Name is one of the functions.Session* names), or the sink number
// LAST_INSERT_ID(expr) writes to (functions.SessionSink).
type SessionValue struct {
	Name string
	// Fsp is the fractional seconds precision of a current-time value.
	Fsp int
}

// Variable is a statement argument that stands for a system variable (@@name). The
// executor replaces it with the session's value before running the statement, so a
// variable is a value anywhere an expression is.
type Variable struct {
	Name  string
	Scope vars.ReadScope
}

// Statement is a fully translated statement.
type Statement struct {
	Kind Kind
	// Database is the database the statement runs in: the table's qualifier, else the
	// current database. Empty for a SELECT without FROM.
	Database string
	SQL      string
	Args     []any
	Op       errs.Op
	// Rules lists the IDs of the rules applied, in registry order (journal tags).
	Rules []string
	// UseDB is the target of KindUse.
	UseDB string
	// Shape describes the result columns of a SELECT, for internal/meta.
	Shape *Shape
	// Explain is set for EXPLAIN of the read: the executor shows the query's plan.
	Explain bool
	// DecimalAsReal is set when a DECIMAL takes part in arithmetic, which mysqlite
	// computes in binary floating point (ADR 0006); the journal records it.
	DecimalAsReal bool
	// Tx is the transaction control of KindTransaction.
	Tx *Transaction
	// Write describes INSERT, UPDATE and DELETE.
	Write *Write
	// Show describes KindShow.
	Show *Show
	// Set lists the assignments of KindSet, in statement order.
	Set []Assignment
	// Create is the CREATE TABLE of KindDDL.
	Create *CreateTable
}

// Write is what the executor needs about INSERT, UPDATE and DELETE beyond the SQL: the
// checks that need the catalog and MySQL's affected-rows counts.
type Write struct {
	Table string // as written
	Alias string
	// INSERT: Columns as written (nil without a column list) and one literal per value,
	// nil when the value is an expression. VALUES () is one empty row.
	Columns []string
	Rows    [][]*meta.Literal
	// UPDATE: the literal assigned to each column, by lower-case name (nil for an
	// expression).
	Set map[string]*meta.Literal
	// UPDATE and DELETE: WHERE and ORDER BY column references, for meta.CheckRefs.
	Refs []meta.Ref
	// UPDATE and DELETE: arithmetic nodes, for meta.CheckExprs.
	Exprs []*meta.Expr
	// Meta is the target table, when the statement was translated with a catalog.
	Meta *meta.Table
	// UPDATE: counts the rows the statement matches. Statement.SQL updates only the rows
	// whose values change, so its changes() is MySQL's affected rows; with
	// CLIENT_FOUND_ROWS the matched count is reported instead.
	MatchSQL  string
	MatchArgs []any
}

// Shape is the select list of a single-table SELECT.
type Shape struct {
	// Table, Alias and Meta are the first FROM table, for the single-table readers
	// (information_schema); From has every table.
	Table string // as written; "" without FROM
	Alias string
	// Meta is the FROM table, when the statement was translated with a catalog.
	Meta *meta.Table
	// Select is what meta.CheckSelect checks once the catalog is read.
	meta.Select
	// Union are the other SELECTs of a UNION, INTERSECT or EXCEPT (meta.CheckUnion).
	Union []*meta.Select
}

// Hint returns what the error mapper needs to know about this statement.
func (s *Statement) Hint() errs.Hint {
	return errs.Hint{Database: s.Database, Op: s.Op}
}

// Translator translates the statements of one connection. The parser it holds is not
// safe for concurrent use, so each connection needs its own Translator.
type Translator struct {
	cfg  Config
	p    *parser.Parser
	mode mysql.SQLMode
}

// New returns a Translator using the MySQL 8.0 default sql_mode.
func New(cfg Config) *Translator {
	mode, err := mysql.GetSQLMode(defaultSQLMode)
	if err != nil {
		panic(fmt.Sprintf("dialect: default sql_mode: %v", err))
	}
	p := parser.New()
	p.SetSQLMode(mode)
	return &Translator{cfg: cfg, p: p, mode: mode}
}

// SetSQLMode is SET sql_mode: the parser follows the modes that change the grammar
// (ANSI_QUOTES, PIPES_AS_CONCAT, IGNORE_SPACE, NO_BACKSLASH_ESCAPES, ...). mode is a
// normalized MySQL sql_mode; names the parser does not know do not affect parsing.
func (t *Translator) SetSQLMode(mode string) {
	var m mysql.SQLMode
	for _, name := range strings.Split(mode, ",") {
		if name == "" {
			continue
		}
		if bit, err := mysql.GetSQLMode(name); err == nil {
			m |= bit
		}
	}
	t.mode = m
	t.p.SetSQLMode(m)
}

// Translate translates one COM_QUERY text. currentDB is the session's database, or "".
// Catalog describes the tables the translator meets (docs/01-architecture.md section 3:
// the translator's input includes the catalog). The executor implements it over the
// session, so a statement in an open transaction sees that transaction's schema.
type Catalog interface {
	Table(database, table string) (*meta.Table, *errs.Error)
}

// Translate translates without a catalog: no table metadata is attached. Tests and
// statements that read no table use it.
func (t *Translator) Translate(query, currentDB string) (*Statement, *errs.Error) {
	return t.TranslateIn(query, currentDB, nil)
}

// TranslateIn translates query, reading the tables it names from cat. A table that does
// not exist fails the translation with the catalog's error.
func (t *Translator) TranslateIn(query, currentDB string, cat Catalog) (*Statement, *errs.Error) {
	gated, gatedAny := gateVersionComments(query, t.cfg.ServerVersion, t.mode.HasNoBackslashEscapesMode())
	stmts, _, err := t.p.ParseSQL(gated)
	if err != nil {
		return nil, parseError(err, gated)
	}
	switch len(stmts) {
	case 0:
		return nil, errs.EmptyQuery()
	case 1:
	default:
		// CLIENT_MULTI_STATEMENTS is not advertised (docs/03-protocol.md section 2), so
		// MySQL rejects the second statement as a syntax error.
		return nil, parseErrorAt(gated, secondStatementOffset(gated, stmts))
	}

	e := &emitter{query: gated, current: currentDB, cat: cat, rules: map[string]bool{}, mode: t.mode}
	if gatedAny {
		e.apply(ruleVersionComment)
	}
	st := e.statement(stmts[0])
	if e.err != nil {
		return nil, e.err
	}
	for _, r := range rules {
		if e.rules[r.ID] {
			st.Rules = append(st.Rules, r.ID)
		}
	}
	return st, nil
}

// VersionNumber converts "8.0.36-mysqlite" to 80036.
func VersionNumber(version string) (int, error) {
	base, _, _ := strings.Cut(version, "-")
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return 0, fmt.Errorf("dialect: version %q is not major.minor.patch", version)
	}
	n := 0
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || (i > 0 && v > 99) {
			return 0, fmt.Errorf("dialect: version %q is not major.minor.patch", version)
		}
		n = n*100 + v
	}
	return n, nil
}

// TiDB reports syntax errors as `line 1 column 13 near "FROM t" `.
var reParseError = regexp.MustCompile(`(?s)^line (\d+) column \d+ near "(.*)"\s*$`)

var reUnknownCollation = regexp.MustCompile(`\[ddl:1273\]Unknown collation: '([^']*)'`)

var reUnknownCharset = regexp.MustCompile(`\[parser:1115\]Unknown character set: '([^']*)'`)

func parseError(err error, query string) *errs.Error {
	// The parser checks the character set of SET NAMES / SET CHARACTER SET itself;
	// MySQL reports it as 1115 too, not as a syntax error.
	if m := reUnknownCharset.FindStringSubmatch(err.Error()); m != nil {
		if w := words(query); len(w) > 0 && (strings.EqualFold(w[0].text, "SET") || strings.EqualFold(w[0].text, "CREATE")) {
			return errs.UnknownCharset(m[1])
		}
	}
	// The parser also checks collation names; MySQL reports them as 1273 too.
	if m := reUnknownCollation.FindStringSubmatch(err.Error()); m != nil {
		return errs.UnknownCollation(m[1])
	}
	if m := reParseError.FindStringSubmatch(err.Error()); m != nil {
		line, _ := strconv.Atoi(m[1])
		return errs.Parse(truncateNear(m[2]), line)
	}
	// Errors without a position (an unknown character set in an introducer, for
	// example) are reported from the start of the query.
	return errs.Parse(truncateNear(query), 1)
}

// parseErrorAt is a syntax error at a byte offset of the query.
func parseErrorAt(query string, offset int) *errs.Error {
	if offset < 0 || offset > len(query) {
		offset = 0
	}
	line := 1 + strings.Count(query[:offset], "\n")
	return errs.Parse(truncateNear(query[offset:]), line)
}

// secondStatementOffset finds where the second statement starts. The parser does not
// record node offsets (OriginTextPosition is 0), but each statement keeps its text.
func secondStatementOffset(query string, stmts []ast.StmtNode) int {
	first := stmts[0].Text()
	start := strings.Index(query, first)
	if start < 0 {
		return 0
	}
	from := start + len(first)
	i := strings.Index(query[from:], stmts[1].Text())
	if i < 0 {
		return 0
	}
	// The statement text may start with the whitespace after ';'; MySQL's "near" text
	// starts at the token.
	off := from + i
	for off < len(query) && strings.ContainsRune(" \t\r\n", rune(query[off])) {
		off++
	}
	return off
}

// truncateNear keeps the first 80 bytes, as MySQL's "%-.80s", without splitting a
// UTF-8 sequence.
func truncateNear(s string) string {
	const limit = 80
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// statementName turns *ast.CreateTableStmt into "CREATE TABLE".
func statementName(n ast.Node) string {
	name := reflect.TypeOf(n).Elem().Name()
	name = strings.TrimSuffix(name, "Stmt")
	var b strings.Builder
	for i, r := range name {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte(' ')
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

package dialect

import (
	"math"
	"strconv"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/opcode"
	"github.com/pingcap/tidb/pkg/parser/test_driver"

	"mysqlite/internal/dialect/functions"
	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
	"mysqlite/internal/vars"
)

// Reasons attached to 1235. They tell the user whether the construct is planned.
const (
	reasonM1Scope   = "M1 supports single-table statements only"
	reasonLater     = "not implemented yet"
	reasonCatalog   = "its type depends on the target column, which needs the catalog"
	reasonSemantics = "SQLite cannot preserve the MySQL semantics"
)

// emitter writes SQLite SQL for one statement. The first failure is kept in err and
// the output is discarded, so a statement is never partially translated.
type emitter struct {
	query   string  // the gated query text, for error offsets
	current string  // the session's database
	cat     Catalog // nil: translate without table metadata
	target  string  // the database the statement runs in
	shape   *Shape
	b       strings.Builder
	args    []any
	rules   map[string]bool
	err     *errs.Error

	// inValues forbids column references, which SQLite does not allow in VALUES.
	inValues bool
	// upsert is set in ON DUPLICATE KEY UPDATE, where VALUES(col) is the inserted value.
	upsert bool
	// clause, when set, records column references in refs (meta.CheckRefs).
	clause string
	refs   *[]meta.Ref
	// checks collects the arithmetic nodes of the statement for meta.CheckExprs.
	checks *[]*meta.Expr
	wr     *Write
	// mode is the session's sql_mode.
	mode mysql.SQLMode
	// changes is set in INSERT and UPDATE, where strict mode turns some NULL results
	// into errors.
	changes bool
	// inAggregate is set inside an aggregate's arguments.
	inAggregate bool
	// decimalReal is set when a DECIMAL takes part in arithmetic (Statement.DecimalAsReal).
	decimalReal bool
	// outer are the FROM clauses of the enclosing queries, outermost first.
	outer []meta.From
	// aliasFirst makes the next select item a scalar subquery's column: aliased
	// scalarColumn when it has no alias; scalarName is the name it got.
	aliasFirst bool
	scalarName string
}

func (e *emitter) apply(rule string) { e.rules[rule] = true }

func (e *emitter) fail(err *errs.Error) {
	if e.err == nil {
		e.err = err
	}
}

// lookup reads table from the catalog, in the statement's database. It is nil without
// a catalog or after a failure.
func (e *emitter) lookup(table string) *meta.Table {
	if e.cat == nil || table == "" || e.err != nil {
		return nil
	}
	t, err := e.cat.Table(e.target, table)
	if err != nil {
		e.fail(err)
		return nil
	}
	return t
}

func (e *emitter) unsupported(construct, reason string) {
	e.fail(errs.Unsupported(construct, reason))
}

func (e *emitter) write(s ...string) {
	for _, x := range s {
		e.b.WriteString(x)
	}
}

func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func (e *emitter) statement(n ast.StmtNode) *Statement {
	switch n := n.(type) {
	case *ast.SelectStmt:
		e.selectStmt(n)
		return e.result(KindRead, errs.OpOther)
	case *ast.InsertStmt:
		e.insertStmt(n)
		return e.result(KindWrite, errs.OpInsert)
	case *ast.UpdateStmt:
		e.updateStmt(n)
		return e.result(KindWrite, errs.OpUpdate)
	case *ast.DeleteStmt:
		e.deleteStmt(n)
		return e.result(KindWrite, errs.OpDelete)
	case *ast.UseStmt:
		return &Statement{Kind: KindUse, UseDB: n.DBName}
	case *ast.BeginStmt:
		return e.begin(n)
	case *ast.CommitStmt:
		return e.completion(TxCommit, n.CompletionType)
	case *ast.RollbackStmt:
		if n.SavepointName != "" {
			e.unsupported("ROLLBACK TO SAVEPOINT", reasonLater)
			return nil
		}
		return e.completion(TxRollback, n.CompletionType)
	case *ast.ShowStmt:
		return e.show(n)
	case *ast.SetStmt:
		return e.set(n)
	case *ast.CreateTableStmt:
		return e.createTable(n)
	case *ast.DropTableStmt:
		return e.dropTable(n)
	case *ast.TruncateTableStmt:
		return e.truncate(n)
	case *ast.CreateIndexStmt:
		return e.createIndex(n)
	case *ast.DropIndexStmt:
		return e.dropIndex(n)
	case *ast.AlterTableStmt:
		return e.alterTable(n)
	case *ast.ExplainStmt:
		// DESCRIBE t / EXPLAIN t is SHOW COLUMNS.
		if s, ok := n.Stmt.(*ast.ShowStmt); ok && s.Tp == ast.ShowColumns {
			return e.show(s)
		}
		return e.explain(n)
	case *ast.SetOprStmt:
		e.setOpr(n)
		return e.result(KindRead, errs.OpOther)
	case *ast.GrantStmt, *ast.RevokeStmt, *ast.GrantRoleStmt, *ast.RevokeRoleStmt:
		e.fail(errs.CommandDenied("GRANT OPTION"))
	case *ast.CreateUserStmt, *ast.AlterUserStmt, *ast.DropUserStmt, *ast.RenameUserStmt, *ast.SetPwdStmt:
		// Single user from the configuration file, no privilege model (docs/00-brief.md).
		e.fail(errs.CommandDenied("CREATE USER"))
	default:
		e.unsupported(statementName(n), reasonLater)
	}
	return nil
}

func (e *emitter) result(kind Kind, op errs.Op) *Statement {
	return &Statement{Kind: kind, Database: e.target, SQL: e.b.String(), Args: e.args, Op: op, Shape: e.shape, Write: e.wr, DecimalAsReal: e.decimalReal}
}

// table resolves the single table of a statement and sets the target database.
func (e *emitter) table(refs *ast.TableRefsClause, multi string) (name, alias string) {
	if refs == nil || refs.TableRefs == nil {
		e.unsupported(multi, reasonM1Scope)
		return "", ""
	}
	j := refs.TableRefs
	if j.Right != nil {
		e.unsupported(multi, reasonM1Scope)
		return "", ""
	}
	ts, ok := j.Left.(*ast.TableSource)
	if !ok {
		e.unsupported(multi, reasonM1Scope)
		return "", ""
	}
	name, alias, db, ok := e.tableName(ts)
	if !ok {
		return "", ""
	}
	e.target = db
	return name, alias
}

// tableName checks a table of a FROM clause and returns it with its database; ok is
// false when it failed (e.err is set).
func (e *emitter) tableName(ts *ast.TableSource) (name, alias, db string, ok bool) {
	tn, isName := ts.Source.(*ast.TableName)
	if !isName || ts.Lateral || len(ts.ColumnNames) > 0 {
		e.unsupported("derived table", reasonLater)
		return "", "", "", false
	}
	switch {
	case len(tn.PartitionNames) > 0:
		e.unsupported("PARTITION", reasonSemantics)
	case tn.TableSample != nil:
		e.unsupported("TABLESAMPLE", reasonSemantics)
	case tn.AsOf != nil:
		e.unsupported("AS OF", reasonSemantics)
	}
	if len(tn.IndexHints) > 0 {
		e.apply(ruleHintStrip)
	}
	db = tn.Schema.O
	if db != "" {
		e.apply(ruleDBQualifier)
	} else {
		db = e.current
	}
	if db == "" {
		e.fail(errs.NoDatabaseSelected())
		return "", "", "", false
	}
	return tn.Name.O, ts.AsName.O, db, e.err == nil
}

func (e *emitter) writeTable(name, alias string) {
	e.write(quoteIdent(name))
	if alias != "" {
		e.write(" AS ", quoteIdent(alias))
	}
}

// checkSchema accepts a database qualifier only for the statement's own database: one
// statement runs in one database file.
func (e *emitter) checkSchema(schema ast.CIStr) {
	if schema.O == "" {
		return
	}
	if schema.O != e.target {
		e.unsupported("cross-database reference", reasonM1Scope)
		return
	}
	e.apply(ruleDBQualifier)
}

func (e *emitter) selectStmt(n *ast.SelectStmt) {
	switch {
	case n.With != nil:
		e.unsupported("WITH", reasonLater)
	case n.Kind != ast.SelectStmtKindSelect:
		e.unsupported("TABLE or VALUES statement", reasonLater)
	case n.SelectIntoOpt != nil:
		e.unsupported("SELECT ... INTO", reasonSemantics)
	case len(n.WindowSpecs) > 0:
		e.unsupported("WINDOW", reasonLater)
	}
	if n.LockInfo != nil {
		switch n.LockInfo.LockType {
		case ast.SelectLockNone:
		case ast.SelectLockForUpdate:
			e.unsupported("FOR UPDATE", reasonLater)
		default:
			e.unsupported("locking read", reasonLater)
		}
	}
	if o := n.SelectStmtOpts; o != nil {
		if o.CalcFoundRows {
			e.unsupported("SQL_CALC_FOUND_ROWS", reasonLater)
		}
		if len(o.TableHints) > 0 || o.StraightJoin || o.Priority != mysql.NoPriority {
			e.apply(ruleHintStrip)
		}
	}
	if len(n.TableHints) > 0 {
		e.apply(ruleHintStrip)
	}

	e.shape = &Shape{}
	e.shape.Group.Full = e.mode.HasOnlyFullGroupBy()
	e.refs = &e.shape.Refs
	e.checks = &e.shape.Exprs
	var fromText string
	var fromArgs []any
	if n.From != nil {
		fromText, fromArgs = e.fromClause(n.From)
	}
	e.write("SELECT ")
	if n.Distinct {
		e.write("DISTINCT ")
	}
	for i, f := range n.Fields.Fields {
		if i > 0 {
			e.write(", ")
		}
		e.field(f)
	}
	if n.From != nil {
		e.write(" FROM ", fromText)
		e.args = append(e.args, fromArgs...)
	}
	if n.Where != nil {
		e.write(" WHERE ")
		e.clause = meta.ClauseWhere
		e.expr(n.Where)
	}
	e.clause = ""
	if n.GroupBy != nil {
		e.groupBy(n.GroupBy)
	}
	if n.Having != nil {
		e.having(n.Having)
	}
	e.clause = meta.ClauseOrder
	e.orderBy(n.OrderBy)
	e.clause = ""
	e.limit(n.Limit)
}

// field emits one select-list item and records it in the shape. Result-column
// metadata is inferred only for table columns and literals in M1
// (docs/08-milestones.md), so other expressions are refused.
func (e *emitter) field(f *ast.SelectField) {
	first := e.aliasFirst
	e.aliasFirst = false
	if w := f.WildCard; w != nil && first {
		e.unsupported("* in a scalar subquery", reasonLater)
		return
	}
	if first {
		defer func() {
			if f.AsName.O != "" {
				e.scalarName = f.AsName.O
				return
			}
			e.write(" AS ", quoteIdent(scalarColumn))
			e.scalarName = scalarColumn
		}()
	}
	if w := f.WildCard; w != nil {
		if w.Table.O == "" && len(e.shape.From.Using) > 0 {
			// MySQL lists a USING column once, first; SQLite's * keeps the left table's
			// column order.
			e.unsupported("* with USING", reasonLater)
			return
		}
		e.checkSchema(w.Schema)
		switch {
		case e.starNeedsList(w.Table.O):
			e.starList(w.Table.O)
		case w.Table.O != "":
			e.write(quoteIdent(w.Table.O), ".*")
		default:
			e.write("*")
		}
		e.shape.Items = append(e.shape.Items, meta.Item{Star: true, Qualifier: w.Table.O})
		return
	}
	x := e.expr(f.Expr)
	item, ok := selectItem(f, e.current)
	if !ok {
		if x == nil {
			// The expression failed (e.err is set) or has no type of its own.
			e.unsupported("expression in select list", reasonLater)
			return
		}
		// MySQL names a computed column by its text as written.
		item = meta.Item{Expr: x, Name: itemText(f.Text())}
	}
	if f.AsName.O != "" {
		e.write(" AS ", quoteIdent(f.AsName.O))
		item.Name = f.AsName.O
	}
	e.shape.Items = append(e.shape.Items, item)
}

// selectItem describes a column or literal select item, named as MySQL names it: a
// column by the name as written, a string by its value, NULL as NULL, a number by its
// text as written.
func selectItem(f *ast.SelectField, current string) (meta.Item, bool) {
	switch x := f.Expr.(type) {
	case *ast.FuncCallExpr:
		if !isCurrentDatabase(x) {
			return meta.Item{}, false
		}
		lit := &meta.Literal{Kind: meta.LiteralString, Text: current}
		if current == "" {
			lit = &meta.Literal{Kind: meta.LiteralNull, Text: "NULL"}
		}
		return meta.Item{Literal: lit, Name: itemText(f.Text())}, true
	case *ast.ColumnNameExpr:
		return meta.Item{Qualifier: x.Name.Table.O, Column: x.Name.Name.O, Name: x.Name.Name.O}, true
	case *test_driver.ValueExpr:
		return literalItem(x, false, f)
	case *ast.VariableExpr:
		if !x.IsSystem {
			return meta.Item{}, false
		}
		// MySQL names the column as the item is written: "@@session.sql_mode".
		return meta.Item{Var: &meta.Variable{Name: x.Name, Scope: variableScope(x)}, Name: itemText(f.Text())}, true
	case *ast.UnaryOperationExpr:
		if v, ok := x.V.(*test_driver.ValueExpr); ok && x.Op == opcode.Minus {
			return literalItem(v, true, f)
		}
	}
	return meta.Item{}, false
}

func literalItem(v *test_driver.ValueExpr, negative bool, f *ast.SelectField) (meta.Item, bool) {
	var lit meta.Literal
	sign := ""
	if negative {
		sign = "-"
	}
	switch v.Kind() {
	case test_driver.KindNull:
		if negative {
			return meta.Item{}, false
		}
		lit = meta.Literal{Kind: meta.LiteralNull, Text: "NULL"}
	case test_driver.KindString:
		if negative {
			return meta.Item{}, false
		}
		lit = meta.Literal{Kind: meta.LiteralString, Text: v.GetString()}
	case test_driver.KindInt64:
		lit = meta.Literal{Kind: meta.LiteralInt, Text: sign + strconv.FormatInt(v.GetInt64(), 10)}
	case test_driver.KindUint64:
		lit = meta.Literal{Kind: meta.LiteralInt, Text: sign + strconv.FormatUint(v.GetUint64(), 10)}
	case test_driver.KindMysqlDecimal:
		lit = meta.Literal{Kind: meta.LiteralDecimal, Text: sign + v.GetMysqlDecimal().String()}
	case test_driver.KindFloat64:
		// The value cannot give back "1e3"; the item text can, unless it carries an
		// alias, in which case the shortest form is the best approximation.
		text := sign + strconv.FormatFloat(v.GetFloat64(), 'g', -1, 64)
		if f.AsName.O == "" {
			text = itemText(f.Text())
		}
		lit = meta.Literal{Kind: meta.LiteralFloat, Text: text}
	default:
		return meta.Item{}, false
	}
	name := lit.Text
	if lit.Kind != meta.LiteralString && lit.Kind != meta.LiteralNull && f.AsName.O == "" {
		name = itemText(f.Text())
	}
	return meta.Item{Literal: &lit, Name: name}, true
}

// variableScope is the scope written before a system variable: @@global.x, @@session.x
// or @@local.x, else none.
func variableScope(n *ast.VariableExpr) vars.ReadScope {
	switch {
	case !n.ExplicitScope:
		return vars.ReadDefault
	case n.IsGlobal:
		return vars.ReadGlobal
	}
	return vars.ReadSession
}

// itemText removes a trailing comment from the text the parser keeps for a select
// item ("2.50 /* c */" -> "2.50"). Only used for numbers, which contain no quotes.
func itemText(s string) string {
	for _, marker := range []string{"/*", "#", "--"} {
		if i := strings.Index(s, marker); i >= 0 {
			s = s[:i]
		}
	}
	return strings.TrimSpace(s)
}

func (e *emitter) orderBy(o *ast.OrderByClause) {
	if o == nil {
		return
	}
	e.write(" ORDER BY ")
	for i, item := range o.Items {
		if i > 0 {
			e.write(", ")
		}
		var x *meta.Expr
		if pos := e.selectPosition(item.Expr); pos > 0 {
			e.write(strconv.Itoa(pos))
		} else {
			x = e.expr(item.Expr)
			if c, ok := item.Expr.(*ast.ColumnNameExpr); ok && c.Name.Table.O == "" && e.shape != nil && e.itemPosition(c.Name.Name.O) > 0 {
				x = nil // a select-list name, checked with its item
			}
		}
		if e.shape != nil && e.wr == nil {
			e.shape.Group.Order = append(e.shape.Group.Order, x)
		}
		if item.Desc {
			e.write(" DESC")
		}
	}
}

// selectPosition is the select-list position an unqualified ORDER BY name stands for
// when the FROM clause has several tables, else 0. MySQL looks a name up in the select
// list before the tables, so "ORDER BY id" sorts by "SELECT i.id"; SQLite would find
// the name ambiguous. Items after a * have no fixed position.
func (e *emitter) selectPosition(x ast.ExprNode) int {
	c, ok := x.(*ast.ColumnNameExpr)
	if !ok || c.Name.Table.O != "" || e.shape == nil || len(e.shape.From.Sources) < 2 {
		return 0
	}
	for i, it := range e.shape.Items {
		if it.Star {
			return 0
		}
		if strings.EqualFold(it.Name, c.Name.Name.O) {
			return i + 1
		}
	}
	return 0
}

func (e *emitter) limit(l *ast.Limit) {
	if l == nil {
		return
	}
	e.write(" LIMIT ")
	// LIMIT 18446744073709551615 is the documented MySQL idiom for "all rows"; SQLite
	// spells it -1 and cannot represent the unsigned value.
	if v, ok := l.Count.(*test_driver.ValueExpr); ok && v.Kind() == test_driver.KindUint64 && v.GetUint64() > math.MaxInt64 {
		e.apply(ruleLimitOffset)
		e.write("-1")
	} else {
		e.expr(l.Count)
	}
	if l.Offset != nil {
		e.apply(ruleLimitOffset)
		e.write(" OFFSET ")
		e.expr(l.Offset)
	}
}

func (e *emitter) insertStmt(n *ast.InsertStmt) {
	switch {
	case n.IgnoreErr && n.Select != nil:
		e.unsupported("INSERT IGNORE ... SELECT", "MySQL reports each skipped row with its key value")
	case n.IgnoreErr && n.IsReplace:
		e.fail(errs.Internal())
	case len(n.OnDuplicate) > 0 && n.Select != nil:
		e.unsupported("INSERT ... SELECT ... ON DUPLICATE KEY UPDATE", reasonLater)
	case len(n.OnDuplicate) > 0 && n.IgnoreErr:
		e.unsupported("INSERT IGNORE ... ON DUPLICATE KEY UPDATE", reasonLater)
	case len(n.PartitionNames) > 0:
		e.unsupported("PARTITION", reasonSemantics)
	}
	if n.Priority != mysql.NoPriority || len(n.TableHints) > 0 {
		e.apply(ruleHintStrip)
	}
	name, _ := e.table(n.Table, "multi-table INSERT")
	if e.err != nil {
		return
	}

	e.wr = &Write{Table: name, Meta: e.lookup(name), Replace: n.IsReplace, Ignore: n.IgnoreErr, Upsert: len(n.OnDuplicate) > 0}
	e.changes = true
	for _, c := range n.Columns {
		e.wr.Columns = append(e.wr.Columns, c.Name.O)
	}
	for _, row := range n.Lists {
		values := make([]*meta.Literal, 0, len(row))
		for _, v := range row {
			values = append(values, valueLiteral(v))
		}
		e.wr.Rows = append(e.wr.Rows, values)
	}

	if n.Select != nil {
		e.insertSelect(n, name)
		return
	}
	e.write(insertVerb(n), quoteIdent(name))
	if len(n.Columns) == 0 && len(n.Lists) == 1 && len(n.Lists[0]) == 0 {
		e.apply(ruleDefaultValues)
		e.write(" DEFAULT VALUES")
		if e.wr.Upsert {
			e.unsupported("VALUES () with ON DUPLICATE KEY UPDATE", "SQLite has no upsert with DEFAULT VALUES")
			return
		}
		if e.wr.Ignore {
			e.wr.RowSQL = []string{e.b.String()}
			e.wr.RowArgs = [][]any{e.args}
		}
		return
	}
	width := len(n.Columns)
	if width == 0 && len(n.Lists) > 0 {
		width = len(n.Lists[0])
	}
	for i, row := range n.Lists {
		if len(row) != width {
			e.fail(errs.ColumnCount(i + 1))
			return
		}
	}
	if width == 0 {
		e.unsupported("VALUES () with several rows", reasonSemantics)
		return
	}
	if len(n.Columns) > 0 {
		e.write(" (")
		for i, c := range n.Columns {
			if i > 0 {
				e.write(", ")
			}
			e.checkSchema(c.Schema)
			e.write(quoteIdent(c.Name.O))
		}
		e.write(")")
	}
	e.write(" VALUES ")
	prefix, prefixArgs := e.b.String(), e.args
	e.inValues = true
	for i, row := range n.Lists {
		text, args := e.sub(func() {
			e.write("(")
			for j, v := range row {
				if j > 0 {
					e.write(", ")
				}
				e.writeValue(e.insertColumn(n, j), v)
			}
			e.write(")")
		})
		if i > 0 {
			e.write(", ")
		}
		e.write(text)
		e.args = append(e.args, args...)
		if e.wr.Ignore || e.wr.Upsert {
			e.wr.RowSQL = append(e.wr.RowSQL, prefix+text)
			e.wr.RowArgs = append(e.wr.RowArgs, append(append([]any(nil), prefixArgs...), args...))
		}
	}
	e.inValues = false
	if e.wr.Upsert {
		e.onDuplicate(n)
	}
}

// insertVerb starts an INSERT or REPLACE. SQLite's REPLACE deletes the rows the new row
// conflicts with, as MySQL's does.
func insertVerb(n *ast.InsertStmt) string {
	if n.IsReplace {
		return "REPLACE INTO "
	}
	return "INSERT INTO "
}

// insertColumn is the column the j-th value of an INSERT row goes to: the column list's,
// else the table's j-th column; "" when unknown.
func (e *emitter) insertColumn(n *ast.InsertStmt, j int) string {
	if len(n.Columns) > 0 {
		return n.Columns[j].Name.O
	}
	if e.wr.Meta != nil && j < len(e.wr.Meta.Columns) {
		return e.wr.Meta.Columns[j].Name
	}
	return ""
}

func (e *emitter) updateStmt(n *ast.UpdateStmt) {
	switch {
	case n.MultipleTable:
		e.unsupported("multi-table UPDATE", reasonLater)
	case n.IgnoreErr:
		e.unsupported("UPDATE IGNORE", reasonLater)
	case n.With != nil:
		e.unsupported("WITH", reasonLater)
	}
	if n.Priority != mysql.NoPriority || len(n.TableHints) > 0 {
		e.apply(ruleHintStrip)
	}
	name, alias := e.table(n.TableRefs, "multi-table UPDATE")
	if e.err != nil {
		return
	}

	e.wr = &Write{Table: name, Alias: alias, Set: map[string]*meta.Literal{}, Meta: e.lookup(name)}
	e.changes = true
	e.refs = &e.wr.Refs
	e.checks = &e.wr.Exprs
	e.write("UPDATE ")
	e.writeTable(name, alias)
	e.write(" SET ")
	if !e.assignments(n.List, "UPDATE") {
		return
	}
	e.filter(name, alias, n.Where, n.Order, n.Limit)

	pred, predArgs := e.sub(func() { e.changed(n.List) })
	if n.Where != nil || n.Order != nil || n.Limit != nil {
		e.write(" AND ", pred)
	} else {
		e.write(" WHERE ", pred)
	}
	e.args = append(e.args, predArgs...)

	e.wr.MatchSQL, e.wr.MatchArgs = e.sub(func() {
		if n.Order == nil && n.Limit == nil {
			e.write("SELECT count(*) FROM ")
			e.writeTable(name, alias)
			if n.Where != nil {
				e.write(" WHERE ")
				e.expr(n.Where)
			}
			return
		}
		e.write("SELECT count(*) FROM (SELECT rowid FROM ")
		e.writeTable(name, alias)
		if n.Where != nil {
			e.write(" WHERE ")
			e.expr(n.Where)
		}
		e.orderBy(n.Order)
		e.limit(n.Limit)
		e.write(")")
	})
}

// sub emits into a separate buffer and argument list and returns them. A
// strings.Builder cannot be copied once written, so the text so far is saved and
// written back.
func (e *emitter) sub(f func()) (string, []any) {
	prefix, args := e.b.String(), e.args
	e.b.Reset()
	e.args = nil
	f()
	out, outArgs := e.b.String(), e.args
	e.b.Reset()
	e.b.WriteString(prefix)
	e.args = args
	return out, outArgs
}

// valueLiteral describes a VALUES or SET value that is a literal, else nil.
func valueLiteral(x ast.ExprNode) *meta.Literal {
	sign := ""
	if u, ok := x.(*ast.UnaryOperationExpr); ok && u.Op == opcode.Minus {
		sign = "-"
		x = u.V
	}
	v, ok := x.(*test_driver.ValueExpr)
	if !ok {
		return nil
	}
	switch v.Kind() {
	case test_driver.KindNull:
		if sign == "" {
			return &meta.Literal{Kind: meta.LiteralNull, Text: "NULL"}
		}
	case test_driver.KindString:
		if sign == "" {
			return &meta.Literal{Kind: meta.LiteralString, Text: v.GetString()}
		}
	case test_driver.KindInt64:
		return &meta.Literal{Kind: meta.LiteralInt, Text: sign + strconv.FormatInt(v.GetInt64(), 10)}
	case test_driver.KindUint64:
		return &meta.Literal{Kind: meta.LiteralInt, Text: sign + strconv.FormatUint(v.GetUint64(), 10)}
	case test_driver.KindMysqlDecimal:
		return &meta.Literal{Kind: meta.LiteralDecimal, Text: sign + v.GetMysqlDecimal().String()}
	case test_driver.KindFloat64:
		return &meta.Literal{Kind: meta.LiteralFloat, Text: sign + strconv.FormatFloat(v.GetFloat64(), 'g', -1, 64)}
	}
	return nil
}

func (e *emitter) deleteStmt(n *ast.DeleteStmt) {
	switch {
	case n.IsMultiTable || n.Tables != nil:
		e.unsupported("multi-table DELETE", reasonLater)
	case n.IgnoreErr:
		e.unsupported("DELETE IGNORE", reasonLater)
	case n.With != nil:
		e.unsupported("WITH", reasonLater)
	}
	if n.Priority != mysql.NoPriority || n.Quick || len(n.TableHints) > 0 {
		e.apply(ruleHintStrip)
	}
	name, alias := e.table(n.TableRefs, "multi-table DELETE")
	if e.err != nil {
		return
	}
	e.wr = &Write{Table: name, Alias: alias, Meta: e.lookup(name)}
	e.refs = &e.wr.Refs
	e.checks = &e.wr.Exprs
	e.write("DELETE FROM ")
	e.writeTable(name, alias)
	e.filter(name, alias, n.Where, n.Order, n.Limit)
}

// filter emits the WHERE of UPDATE and DELETE. SQLite has no UPDATE/DELETE ... ORDER BY
// ... LIMIT in this build (docs/agent-notes/api-sqlite-driver.md), so those select the
// rows by rowid in a subquery. Tables are created by mysqlite, never WITHOUT ROWID.
func (e *emitter) filter(name, alias string, where ast.ExprNode, order *ast.OrderByClause, limit *ast.Limit) {
	defer func() { e.clause = "" }()
	if order == nil && limit == nil {
		if where != nil {
			e.write(" WHERE ")
			e.clause = meta.ClauseWhere
			e.expr(where)
		}
		return
	}
	e.apply(ruleUpdateLimit)
	e.write(" WHERE rowid IN (SELECT rowid FROM ")
	e.writeTable(name, alias)
	if where != nil {
		e.write(" WHERE ")
		e.clause = meta.ClauseWhere
		e.expr(where)
	}
	e.clause = meta.ClauseOrder
	e.orderBy(order)
	e.clause = ""
	e.limit(limit)
	e.write(")")
}

// columnRefs lists the lower-case names of the columns an expression reads.
func columnRefs(x ast.ExprNode) []string {
	v := &refCollector{}
	x.Accept(v)
	return v.names
}

type refCollector struct{ names []string }

func (v *refCollector) Enter(n ast.Node) (ast.Node, bool) {
	if c, ok := n.(*ast.ColumnNameExpr); ok {
		v.names = append(v.names, c.Name.Name.L)
	}
	return n, false
}

func (v *refCollector) Leave(n ast.Node) (ast.Node, bool) { return n, true }

var binaryOps = map[opcode.Op]string{
	opcode.LogicAnd: "AND",
	opcode.LogicOr:  "OR",
	opcode.EQ:       "=",
	opcode.NE:       "<>",
	opcode.LT:       "<",
	opcode.LE:       "<=",
	opcode.GT:       ">",
	opcode.GE:       ">=",
	opcode.Plus:     "+",
	opcode.Minus:    "-",
	opcode.Mul:      "*",
}

// Operators refused in M1, by the name shown to the user. Division and DIV need the
// rewrites of docs/05-dialect.md section 4 (M3); the bit operators need unsigned
// 64-bit arithmetic.
var refusedOps = map[opcode.Op]string{
	opcode.And:        "&",
	opcode.Or:         "|",
	opcode.Xor:        "^",
	opcode.LeftShift:  "<<",
	opcode.RightShift: ">>",
}

// expr emits x and returns its typed shape (meta.Expr), from which meta infers the
// result type once the catalog is read. It returns nil when x failed, or for nodes that
// carry no type of their own (a placeholder, ORDER BY position).
func (e *emitter) expr(x ast.ExprNode) *meta.Expr {
	switch n := x.(type) {
	case *ast.ColumnNameExpr:
		if e.inValues {
			e.unsupported("column reference in VALUES", reasonSemantics)
			return nil
		}
		e.checkSchema(n.Name.Schema)
		if e.clause != "" && e.refs != nil {
			*e.refs = append(*e.refs, meta.Ref{Qualifier: n.Name.Table.O, Column: n.Name.Name.O, Clause: e.clause})
		}
		ref := quoteIdent(n.Name.Name.O)
		if n.Name.Table.O != "" {
			ref = quoteIdent(n.Name.Table.O) + "." + ref
		}
		if e.timestampRef(n.Name.Table.O, n.Name.Name.O) {
			e.fromUTC(func() { e.write(ref) })
		} else {
			e.write(ref)
		}
		return &meta.Expr{Kind: meta.ExprColumn, Qualifier: n.Name.Table.O, Column: n.Name.Name.O}
	case *ast.ValuesExpr:
		col := n.Column.Name
		if !e.upsert {
			e.unsupported("VALUES() outside ON DUPLICATE KEY UPDATE", "MySQL returns NULL there and deprecates it")
			return nil
		}
		if col.Table.O != "" && !strings.EqualFold(col.Table.O, e.wr.Table) {
			e.fail(errs.UnknownColumn(col.Table.O+"."+col.Name.O, "field list"))
			return nil
		}
		*e.refs = append(*e.refs, meta.Ref{Column: col.Name.O, Clause: "field list"})
		ref := "excluded." + quoteIdent(col.Name.O)
		if e.timestampRef("", col.Name.O) {
			e.fromUTC(func() { e.write(ref) })
		} else {
			e.write(ref)
		}
		return &meta.Expr{Kind: meta.ExprColumn, Column: col.Name.O}
	case *ast.TimeUnitExpr:
		// An INTERVAL, EXTRACT or TIMESTAMPDIFF unit, passed to the date UDFs as text.
		unit := n.Unit.String()
		e.write("'", unit, "'")
		return &meta.Expr{Kind: meta.ExprLiteral, Literal: &meta.Literal{Kind: meta.LiteralString, Text: unit}}
	case *test_driver.ParamMarkerExpr:
		// '?' is not valid in a COM_QUERY text; MySQL reports a syntax error there.
		e.fail(parseErrorAt(e.query, n.Offset))
	case *test_driver.ValueExpr:
		e.value(n)
		if lit := valueLiteral(n); lit != nil {
			return &meta.Expr{Kind: meta.ExprLiteral, Literal: lit}
		}
	case *ast.PositionExpr:
		// ORDER BY 1: a select-list position, the same in SQLite.
		if n.P != nil {
			e.fail(parseErrorAt(e.query, 0))
			return nil
		}
		e.write(strconv.Itoa(n.N))
	case *ast.ParenthesesExpr:
		// Every emitted operator is already parenthesized.
		return e.expr(n.Expr)
	case *ast.UnaryOperationExpr:
		switch n.Op {
		case opcode.Minus:
			// "(- x)": the space keeps "- -5" from becoming a "--" comment.
			e.write("(- ")
			a := e.expr(n.V)
			e.write(")")
			return e.check(node(meta.ExprNeg, "", a))
		case opcode.Plus:
			return e.expr(n.V)
		case opcode.Not, opcode.Not2:
			e.write("(NOT ")
			a := e.expr(n.V)
			e.write(")")
			return node(meta.ExprBool, "NOT", a)
		default:
			e.unsupported("operator ~", reasonLater)
		}
	case *ast.BinaryOperationExpr:
		switch n.Op {
		case opcode.Mod:
			// MOD(a, b) and a % b (the parser turns MOD into %).
			l, r := e.division(functions.UDFMod, n)
			x := node(meta.ExprFunc, "MOD", l, r)
			if x != nil {
				x.Ret = meta.RetMod
			}
			return e.check(x)
		case opcode.Div:
			l, r := e.division(functions.UDFDivide, n)
			return e.check(node(meta.ExprArith, "/", l, r))
		case opcode.IntDiv:
			l, r := e.division(functions.UDFIntDiv, n)
			return e.check(node(meta.ExprArith, "DIV", l, r))
		case opcode.LogicXor:
			e.apply(ruleFunction)
			e.write(functions.UDFXor, "(")
			l := e.expr(n.L)
			e.write(", ")
			r := e.expr(n.R)
			e.write(")")
			return node(meta.ExprBool, "XOR", l, r)
		}
		op, ok := binaryOps[n.Op]
		if n.Op == opcode.NullEQ {
			e.apply(ruleNullSafeEqual)
			op, ok = "IS", true
		}
		if !ok {
			name := refusedOps[n.Op]
			if name == "" {
				name = n.Op.String()
			}
			e.unsupported("operator "+name, reasonLater)
			return nil
		}
		e.write("(")
		l := e.expr(n.L)
		e.write(" ", op, " ")
		r := e.expr(n.R)
		e.write(")")
		switch op {
		case "+", "-", "*":
			return e.check(node(meta.ExprArith, op, l, r))
		}
		x := node(meta.ExprBool, op, l, r)
		if x != nil && op == "IS" {
			x.NotNull = true
		}
		return x
	case *ast.IsNullExpr:
		e.write("(")
		a := e.expr(n.Expr)
		if n.Not {
			e.write(" IS NOT NULL)")
		} else {
			e.write(" IS NULL)")
		}
		x := node(meta.ExprBool, "IS NULL", a)
		if x != nil {
			x.NotNull = true
		}
		return x
	case *ast.PatternInExpr:
		if n.Sel != nil {
			q, ok := n.Sel.(*ast.SubqueryExpr)
			if !ok {
				e.fail(errs.Internal())
				return nil
			}
			return e.inSubquery(n.Expr, q, n.Not)
		}
		e.write("(")
		args := []*meta.Expr{e.expr(n.Expr)}
		if n.Not {
			e.write(" NOT")
		}
		e.write(" IN (")
		for i, item := range n.List {
			if i > 0 {
				e.write(", ")
			}
			args = append(args, e.expr(item))
		}
		e.write("))")
		return node(meta.ExprBool, "IN", args...)
	case *ast.BetweenExpr:
		e.write("(")
		a := e.expr(n.Expr)
		if n.Not {
			e.write(" NOT")
		}
		e.write(" BETWEEN ")
		l := e.expr(n.Left)
		e.write(" AND ")
		r := e.expr(n.Right)
		e.write(")")
		return node(meta.ExprBool, "BETWEEN", a, l, r)
	case *ast.IsTruthExpr:
		// SQLite's IS TRUE takes a string's leading number, as MySQL does.
		e.write("(")
		a := e.expr(n.Expr)
		e.write(" IS ")
		if n.Not {
			e.write("NOT ")
		}
		if n.True != 0 {
			e.write("TRUE)")
		} else {
			e.write("FALSE)")
		}
		x := node(meta.ExprBool, "IS TRUE", a)
		if x != nil {
			x.NotNull = true
		}
		return x
	case *ast.PatternLikeOrIlikeExpr:
		return e.like(n)
	case *ast.FuncCallExpr:
		if isCurrentDatabase(n) {
			e.apply(ruleCurrentDB)
			e.write("?")
			e.args = append(e.args, currentDatabaseArg(e.current))
			lit := &meta.Literal{Kind: meta.LiteralString, Text: e.current}
			if e.current == "" {
				lit = &meta.Literal{Kind: meta.LiteralNull, Text: "NULL"}
			}
			return &meta.Expr{Kind: meta.ExprLiteral, Literal: lit}
		}
		return e.call(n)
	case *ast.AggregateFuncExpr:
		return e.aggregate(n)
	case *ast.WindowFuncExpr:
		e.unsupported("window function", reasonLater)
	case *ast.SubqueryExpr:
		return e.scalarSubquery(n)
	case *ast.ExistsSubqueryExpr:
		return e.exists(n)
	case *ast.CompareSubqueryExpr:
		return e.compareSubquery(n)
	case *ast.DefaultExpr:
		e.unsupported("DEFAULT", reasonCatalog)
	case *ast.RowExpr:
		e.unsupported("row constructor", reasonLater)
	case *ast.VariableExpr:
		if !n.IsSystem {
			e.unsupported("user variable", reasonLater)
			return nil
		}
		e.write("?")
		e.args = append(e.args, &Variable{Name: n.Name, Scope: variableScope(n)})
		return &meta.Expr{Kind: meta.ExprVar, Var: &meta.Variable{Name: n.Name, Scope: variableScope(n)}}
	case *ast.CaseExpr:
		return e.caseExpr(n)
	default:
		e.unsupported(strings.TrimSuffix(statementName(x), " EXPR")+" expression", reasonLater)
	}
	return nil
}

// check records an arithmetic node for meta.CheckExprs, which refuses the operand
// types SQLite would compute differently.
func (e *emitter) check(x *meta.Expr) *meta.Expr {
	if x != nil && e.checks != nil {
		*e.checks = append(*e.checks, x)
	}
	if x != nil && (x.Kind == meta.ExprArith || x.Kind == meta.ExprFunc && x.Op == "MOD") {
		e.noteDecimal(x.Args...)
	}
	return x
}

// noteDecimal flags the statement when a DECIMAL takes part in arithmetic: mysqlite
// computes it as REAL (ADR 0006), and the journal records it.
func (e *emitter) noteDecimal(args ...*meta.Expr) {
	for _, a := range args {
		if d, err := meta.Infer(a, e.scope()); err == nil && meta.IsDecimal(d) {
			e.decimalReal = true
			return
		}
	}
}

// node builds an operator node; it is nil when an operand failed.
func node(kind meta.ExprKind, op string, args ...*meta.Expr) *meta.Expr {
	for _, a := range args {
		if a == nil {
			return nil
		}
	}
	return &meta.Expr{Kind: kind, Op: op, Args: args}
}

func (e *emitter) value(v *test_driver.ValueExpr) {
	switch v.Kind() {
	case test_driver.KindNull:
		e.write("NULL")
	case test_driver.KindInt64:
		e.write(strconv.FormatInt(v.GetInt64(), 10))
	case test_driver.KindUint64:
		if v.GetUint64() > math.MaxInt64 {
			e.unsupported("integer literal", "above the signed 64-bit range of SQLite")
			return
		}
		e.write(strconv.FormatUint(v.GetUint64(), 10))
	case test_driver.KindFloat64:
		e.write(strconv.FormatFloat(v.GetFloat64(), 'e', -1, 64))
	case test_driver.KindMysqlDecimal:
		s := v.GetMysqlDecimal().String()
		if significantDigits(s) > 15 {
			e.unsupported("decimal literal", "more than 15 significant digits do not fit a REAL exactly")
			return
		}
		e.write(s)
	case test_driver.KindString:
		switch cs := v.Type.GetCharset(); cs {
		case "", mysql.UTF8MB4Charset, mysql.UTF8Charset, "utf8mb3":
		case "binary":
			e.unsupported("binary string literal", reasonCatalog)
			return
		default:
			e.unsupported("character set introducer _"+cs, reasonSemantics)
			return
		}
		e.apply(ruleStringParam)
		e.args = append(e.args, v.GetString())
		e.write("?")
	case test_driver.KindBinaryLiteral:
		e.unsupported("hexadecimal or bit literal", reasonCatalog)
	default:
		e.unsupported("literal", reasonLater)
	}
}

// significantDigits counts the digits of a decimal literal without leading zeros and
// without trailing zeros after the point.
func significantDigits(s string) int {
	intPart, frac, _ := strings.Cut(strings.TrimLeft(s, "-+"), ".")
	frac = strings.TrimRight(frac, "0")
	digits := strings.TrimLeft(intPart+frac, "0")
	return len(digits)
}

// isCurrentDatabase is DATABASE() or its synonym SCHEMA().
func isCurrentDatabase(n *ast.FuncCallExpr) bool {
	return (n.FnName.L == "database" || n.FnName.L == "schema") && len(n.Args) == 0
}

// currentDatabaseArg is the value of DATABASE(): the current database, NULL without one.
func currentDatabaseArg(current string) any {
	if current == "" {
		return nil
	}
	return current
}

// insertSelect emits INSERT ... SELECT. Values for TIMESTAMP columns go through a
// common table expression that converts them to UTC; the executor checks the SELECT,
// its width and the column list against the catalog.
func (e *emitter) insertSelect(n *ast.InsertStmt, table string) {
	switch n.Select.(type) {
	case *ast.SelectStmt, *ast.SetOprStmt:
	default:
		e.unsupported("INSERT ... TABLE or VALUES statement", reasonLater)
		return
	}
	text, args, shape := e.nested(n.Select, false)
	if e.err != nil {
		return
	}
	if shape == nil {
		e.fail(errs.Internal())
		return
	}
	e.wr.Select = shape

	var targets []string
	for _, c := range n.Columns {
		e.checkSchema(c.Schema)
		targets = append(targets, c.Name.O)
	}
	if len(targets) == 0 && e.wr.Meta != nil {
		for _, c := range e.wr.Meta.Columns {
			targets = append(targets, c.Name)
		}
	}
	zoned := false
	for _, c := range targets {
		if e.wr.Meta != nil && e.wr.Meta.IsTimestamp(c) {
			zoned = true
		}
	}
	list := ""
	if len(n.Columns) > 0 {
		list = " (" + identList(targets) + ")"
	}
	if !zoned {
		e.write(insertVerb(n), quoteIdent(table), list, " ", text)
		e.args = append(e.args, args...)
		return
	}
	names := make([]string, len(targets))
	for i := range targets {
		names[i] = "c" + strconv.Itoa(i)
	}
	e.write("WITH `__mysqlite_rows`(", identList(names), ") AS (", text, ") ")
	e.args = append(e.args, args...)
	e.write(insertVerb(n), quoteIdent(table), " (", identList(targets), ") SELECT ")
	for i, c := range targets {
		if i > 0 {
			e.write(", ")
		}
		if e.wr.Meta.IsTimestamp(c) {
			e.write(functions.UDFTzToUTC, "(", quoteIdent(names[i]), ", ?)")
			e.args = append(e.args, &SessionValue{Name: functions.SessionTimeZone})
			continue
		}
		e.write(quoteIdent(names[i]))
	}
	e.write(" FROM `__mysqlite_rows`")
}

// assignments emits the SET list of UPDATE or ON DUPLICATE KEY UPDATE. MySQL applies
// single-table assignments left to right, so a later expression sees the new value of
// an earlier column; SQLite evaluates every expression on the old row. The statements
// where the two differ are refused.
func (e *emitter) assignments(list []*ast.Assignment, stmt string) bool {
	assigned := map[string]bool{}
	for i, a := range list {
		col := a.Column.Name.L
		if assigned[col] {
			e.unsupported("column assigned twice", reasonSemantics)
			return false
		}
		for _, ref := range columnRefs(a.Expr) {
			if assigned[ref] {
				e.unsupported(stmt+" assignment order", "MySQL assigns left to right, SQLite reads the old row")
				return false
			}
		}
		assigned[col] = true
		if i > 0 {
			e.write(", ")
		}
		e.checkSchema(a.Column.Schema)
		e.write(quoteIdent(a.Column.Name.O), " = ")
		e.writeValue(a.Column.Name.O, a.Expr)
		if e.wr.Set != nil {
			e.wr.Set[col] = valueLiteral(a.Expr)
		}
		if e.err != nil {
			return false
		}
	}
	return true
}

// changed emits the predicate of a row the assignments change. MySQL's affected rows
// are the rows whose values change (docs/05-dialect.md section 6): a row is changed
// unless every assigned column already holds the new value, compared byte for byte as
// MySQL compares records (COLLATE BINARY; IS for NULL). The expressions read the old
// row, as the assignments do.
func (e *emitter) changed(list []*ast.Assignment) {
	e.write("NOT (")
	for i, a := range list {
		if i > 0 {
			e.write(" AND ")
		}
		e.write("(", quoteIdent(a.Column.Name.O), " IS (")
		e.writeValue(a.Column.Name.O, a.Expr)
		e.write(") COLLATE BINARY)")
	}
	e.write(")")
}

// onDuplicate emits ON DUPLICATE KEY UPDATE as SQLite upserts, one row at a time: each
// row is first inserted with ON CONFLICT DO NOTHING; when that inserts nothing, the
// row conflicts and ON CONFLICT DO UPDATE ... WHERE <changed> updates the existing row
// only when the values change, so the executor can count MySQL's 1, 2 or 0 affected
// rows. VALUES(col) is SQLite's excluded.col.
func (e *emitter) onDuplicate(n *ast.InsertStmt) {
	for _, a := range n.OnDuplicate {
		if q := a.Column.Table.O; q != "" && !strings.EqualFold(q, e.wr.Table) {
			e.fail(errs.UnknownColumn(q+"."+a.Column.Name.O, "field list"))
			return
		}
	}
	e.refs, e.checks, e.clause = &e.wr.Refs, &e.wr.Exprs, "field list"
	e.upsert = true
	e.wr.Set = map[string]*meta.Literal{}
	defer func() { e.refs, e.checks, e.clause, e.upsert = nil, nil, "", false }()
	for _, a := range n.OnDuplicate {
		*e.refs = append(*e.refs, meta.Ref{Column: a.Column.Name.O, Clause: "field list"})
	}
	set, setArgs := e.sub(func() { e.assignments(n.OnDuplicate, "ON DUPLICATE KEY UPDATE") })
	pred, predArgs := e.sub(func() { e.changed(n.OnDuplicate) })
	if e.err != nil {
		return
	}
	suffix := " ON CONFLICT DO UPDATE SET " + set + " WHERE " + pred
	suffixArgs := append(setArgs, predArgs...)
	e.write(suffix)
	e.args = append(e.args, suffixArgs...)
	for i := range e.wr.RowSQL {
		e.wr.UpdateSQL = append(e.wr.UpdateSQL, e.wr.RowSQL[i]+suffix)
		e.wr.UpdateArgs = append(e.wr.UpdateArgs, append(append([]any(nil), e.wr.RowArgs[i]...), suffixArgs...))
		e.wr.RowSQL[i] += " ON CONFLICT DO NOTHING"
	}
}

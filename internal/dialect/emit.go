package dialect

import (
	"math"
	"strconv"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/opcode"
	"github.com/pingcap/tidb/pkg/parser/test_driver"

	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
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
	query   string // the gated query text, for error offsets
	current string // the session's database
	target  string // the database the statement runs in
	shape   *Shape
	b       strings.Builder
	args    []any
	rules   map[string]bool
	err     *errs.Error

	// inValues forbids column references, which SQLite does not allow in VALUES.
	inValues bool
}

func (e *emitter) apply(rule string) { e.rules[rule] = true }

func (e *emitter) fail(err *errs.Error) {
	if e.err == nil {
		e.err = err
	}
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
	case *ast.SetOprStmt:
		e.unsupported("UNION, EXCEPT or INTERSECT", reasonLater)
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
	return &Statement{Kind: kind, Database: e.target, SQL: e.b.String(), Args: e.args, Op: op, Shape: e.shape}
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
	tn, ok := ts.Source.(*ast.TableName)
	if !ok || ts.Lateral || len(ts.ColumnNames) > 0 {
		e.unsupported("derived table", reasonLater)
		return "", ""
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
	db := tn.Schema.O
	if db != "" {
		e.apply(ruleDBQualifier)
	} else {
		db = e.current
	}
	if db == "" {
		e.fail(errs.NoDatabaseSelected())
	}
	e.target = db
	return tn.Name.O, ts.AsName.O
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
	case n.GroupBy != nil:
		e.unsupported("GROUP BY", reasonLater)
	case n.Having != nil:
		e.unsupported("HAVING", reasonLater)
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

	var name, alias string
	if n.From != nil {
		name, alias = e.table(n.From, "JOIN")
	}
	e.shape = &Shape{Table: name, Alias: alias}
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
		e.write(" FROM ")
		e.writeTable(name, alias)
	}
	if n.Where != nil {
		e.write(" WHERE ")
		e.expr(n.Where)
	}
	e.orderBy(n.OrderBy)
	e.limit(n.Limit)
}

// field emits one select-list item and records it in the shape. Result-column
// metadata is inferred only for table columns and literals in M1
// (docs/08-milestones.md), so other expressions are refused.
func (e *emitter) field(f *ast.SelectField) {
	if w := f.WildCard; w != nil {
		e.checkSchema(w.Schema)
		if w.Table.O != "" {
			e.write(quoteIdent(w.Table.O), ".")
		}
		e.write("*")
		e.shape.Items = append(e.shape.Items, meta.Item{Star: true, Qualifier: w.Table.O})
		return
	}
	e.expr(f.Expr)
	item, ok := selectItem(f)
	if !ok {
		e.unsupported("expression in select list", "result metadata for expressions arrives in M3")
		return
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
func selectItem(f *ast.SelectField) (meta.Item, bool) {
	switch x := f.Expr.(type) {
	case *ast.ColumnNameExpr:
		return meta.Item{Qualifier: x.Name.Table.O, Column: x.Name.Name.O, Name: x.Name.Name.O}, true
	case *test_driver.ValueExpr:
		return literalItem(x, false, f)
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
		e.expr(item.Expr)
		if item.Desc {
			e.write(" DESC")
		}
	}
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
	case n.IsReplace:
		e.unsupported("REPLACE", reasonLater)
	case n.IgnoreErr:
		e.unsupported("INSERT IGNORE", reasonLater)
	case len(n.OnDuplicate) > 0:
		e.unsupported("ON DUPLICATE KEY UPDATE", reasonLater)
	case n.Select != nil:
		e.unsupported("INSERT ... SELECT", reasonLater)
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

	e.write("INSERT INTO ", quoteIdent(name))
	if len(n.Columns) == 0 && len(n.Lists) == 1 && len(n.Lists[0]) == 0 {
		e.apply(ruleDefaultValues)
		e.write(" DEFAULT VALUES")
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
	e.inValues = true
	for i, row := range n.Lists {
		if i > 0 {
			e.write(", ")
		}
		e.write("(")
		for j, v := range row {
			if j > 0 {
				e.write(", ")
			}
			e.expr(v)
		}
		e.write(")")
	}
	e.inValues = false
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

	e.write("UPDATE ")
	e.writeTable(name, alias)
	e.write(" SET ")
	// MySQL applies single-table assignments left to right, so a later expression sees
	// the new value of an earlier column; SQLite evaluates every expression on the old
	// row. Refuse the statements where the two differ.
	assigned := map[string]bool{}
	for i, a := range n.List {
		col := a.Column.Name.L
		if assigned[col] {
			e.unsupported("column assigned twice", reasonSemantics)
			return
		}
		for _, ref := range columnRefs(a.Expr) {
			if assigned[ref] {
				e.unsupported("UPDATE assignment order", "MySQL assigns left to right, SQLite reads the old row")
				return
			}
		}
		assigned[col] = true
		if i > 0 {
			e.write(", ")
		}
		e.checkSchema(a.Column.Schema)
		e.write(quoteIdent(a.Column.Name.O), " = ")
		e.expr(a.Expr)
	}
	e.filter(name, alias, n.Where, n.Order, n.Limit)
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
	e.write("DELETE FROM ")
	e.writeTable(name, alias)
	e.filter(name, alias, n.Where, n.Order, n.Limit)
}

// filter emits the WHERE of UPDATE and DELETE. SQLite has no UPDATE/DELETE ... ORDER BY
// ... LIMIT in this build (docs/agent-notes/api-sqlite-driver.md), so those select the
// rows by rowid in a subquery. Tables are created by mysqlite, never WITHOUT ROWID.
func (e *emitter) filter(name, alias string, where ast.ExprNode, order *ast.OrderByClause, limit *ast.Limit) {
	if order == nil && limit == nil {
		if where != nil {
			e.write(" WHERE ")
			e.expr(where)
		}
		return
	}
	e.apply(ruleUpdateLimit)
	e.write(" WHERE rowid IN (SELECT rowid FROM ")
	e.writeTable(name, alias)
	if where != nil {
		e.write(" WHERE ")
		e.expr(where)
	}
	e.orderBy(order)
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
	opcode.Div:        "/",
	opcode.IntDiv:     "DIV",
	opcode.Mod:        "%",
	opcode.LogicXor:   "XOR",
	opcode.And:        "&",
	opcode.Or:         "|",
	opcode.Xor:        "^",
	opcode.LeftShift:  "<<",
	opcode.RightShift: ">>",
}

func (e *emitter) expr(x ast.ExprNode) {
	switch n := x.(type) {
	case *ast.ColumnNameExpr:
		if e.inValues {
			e.unsupported("column reference in VALUES", reasonSemantics)
			return
		}
		e.checkSchema(n.Name.Schema)
		if n.Name.Table.O != "" {
			e.write(quoteIdent(n.Name.Table.O), ".")
		}
		e.write(quoteIdent(n.Name.Name.O))
	case *test_driver.ParamMarkerExpr:
		// '?' is not valid in a COM_QUERY text; MySQL reports a syntax error there.
		e.fail(parseErrorAt(e.query, n.Offset))
	case *test_driver.ValueExpr:
		e.value(n)
	case *ast.PositionExpr:
		// ORDER BY 1: a select-list position, the same in SQLite.
		if n.P != nil {
			e.fail(parseErrorAt(e.query, 0))
			return
		}
		e.write(strconv.Itoa(n.N))
	case *ast.ParenthesesExpr:
		// Every emitted operator is already parenthesized.
		e.expr(n.Expr)
	case *ast.UnaryOperationExpr:
		switch n.Op {
		case opcode.Minus:
			// "(- x)": the space keeps "- -5" from becoming a "--" comment.
			e.write("(- ")
			e.expr(n.V)
			e.write(")")
		case opcode.Plus:
			e.expr(n.V)
		case opcode.Not, opcode.Not2:
			e.write("(NOT ")
			e.expr(n.V)
			e.write(")")
		default:
			e.unsupported("operator ~", reasonLater)
		}
	case *ast.BinaryOperationExpr:
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
			return
		}
		e.write("(")
		e.expr(n.L)
		e.write(" ", op, " ")
		e.expr(n.R)
		e.write(")")
	case *ast.IsNullExpr:
		e.write("(")
		e.expr(n.Expr)
		if n.Not {
			e.write(" IS NOT NULL)")
		} else {
			e.write(" IS NULL)")
		}
	case *ast.PatternInExpr:
		if n.Sel != nil {
			e.unsupported("subquery", reasonLater)
			return
		}
		e.write("(")
		e.expr(n.Expr)
		if n.Not {
			e.write(" NOT")
		}
		e.write(" IN (")
		for i, item := range n.List {
			if i > 0 {
				e.write(", ")
			}
			e.expr(item)
		}
		e.write("))")
	case *ast.BetweenExpr:
		e.write("(")
		e.expr(n.Expr)
		if n.Not {
			e.write(" NOT")
		}
		e.write(" BETWEEN ")
		e.expr(n.Left)
		e.write(" AND ")
		e.expr(n.Right)
		e.write(")")
	case *ast.IsTruthExpr:
		name := "IS "
		if n.Not {
			name += "NOT "
		}
		if n.True != 0 {
			name += "TRUE"
		} else {
			name += "FALSE"
		}
		e.unsupported(name, reasonLater)
	case *ast.PatternLikeOrIlikeExpr:
		e.unsupported("LIKE", "MySQL collations need the custom LIKE of M3")
	case *ast.FuncCallExpr:
		e.unsupported("function "+strings.ToUpper(n.FnName.L), reasonLater)
	case *ast.AggregateFuncExpr:
		e.unsupported("aggregate "+strings.ToUpper(n.F), reasonLater)
	case *ast.SubqueryExpr, *ast.ExistsSubqueryExpr, *ast.CompareSubqueryExpr:
		e.unsupported("subquery", reasonLater)
	case *ast.DefaultExpr:
		e.unsupported("DEFAULT", reasonCatalog)
	case *ast.RowExpr:
		e.unsupported("row constructor", reasonLater)
	case *ast.VariableExpr:
		e.unsupported("variable", reasonLater)
	case *ast.CaseExpr:
		e.unsupported("CASE", reasonLater)
	default:
		e.unsupported(strings.TrimSuffix(statementName(x), " EXPR")+" expression", reasonLater)
	}
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

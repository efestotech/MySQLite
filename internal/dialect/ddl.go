package dialect

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/parser/types"

	"mysqlite/internal/charset"
	"mysqlite/internal/collate"
	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// CREATE TABLE (docs/05-dialect.md section 7, the M2 subset of Q4): the M1 column types,
// NOT NULL, literal DEFAULT, PRIMARY KEY, AUTO_INCREMENT, KEY/UNIQUE KEY, COLLATE, and
// the table options ENGINE, CHARSET, COLLATE, COMMENT, AUTO_INCREMENT, ROW_FORMAT.
// Everything else is 1235.

// CreateTable is a translated CREATE TABLE. The executor runs SQL in one transaction on
// the writer, with the sidecar facts.
type CreateTable struct {
	Table       string
	IfNotExists bool
	// SQL is the CREATE TABLE and then one CREATE INDEX per key.
	SQL []string
	// Facts are the sidecar rows (package catalog) for the table.
	Facts []Fact
	// AutoIncrement is the AUTO_INCREMENT=n table option; 0 when absent or when the
	// table has no AUTO_INCREMENT column (MySQL ignores it then).
	AutoIncrement uint64

	// The translated definition, for an ALTER TABLE rebuild.
	cols []*ddlColumn
	pk   []string
	keys []IndexChange
}

// Fact is one sidecar row: Column is "" for a table fact, a column name, or
// "index:<sqlite name>".
type Fact struct {
	Column, Key, Value string
}

// Sidecar keys; they match package catalog.
const (
	factEngine    = "engine"
	factCollation = "collation"
	factComment   = "comment"
	factRowFormat = "row_format"
	factType      = "type"
	factName      = "name"
	factSubPart   = "sub_part"
)

// binaryCharset is the parser's character set of BINARY, VARBINARY and BLOB types.
const binaryCharset = "binary"

// defaultCollation is the database default (docs/00-brief.md section 6).
const defaultCollation = "utf8mb4_general_ci"

// reasonCollation0900 explains the refused 0900 collations (ADR 0004).
const reasonCollation0900 = "mysqlite implements utf8mb4_0900_ai_ci and utf8mb4_0900_bin only (ADR 0004)"

type ddlColumn struct {
	name         string
	mysqlType    string
	ft           *types.FieldType
	text         bool // a character type: takes a collation
	lob          bool // TEXT or BLOB
	collation    string
	notNull      bool
	explicitNull bool
	auto         bool
	dflt         *meta.Literal
	comment      string
	pk           bool
}

type ddlKey struct {
	name    string
	unique  bool
	columns []string
	subPart []int // key prefix lengths, nil when no column has one
}

func (e *emitter) createTable(n *ast.CreateTableStmt) *Statement {
	switch {
	case n.TemporaryKeyword != ast.TemporaryNone:
		e.unsupported("CREATE TEMPORARY TABLE", reasonLater)
	case n.ReferTable != nil:
		e.unsupported("CREATE TABLE ... LIKE", reasonLater)
	case n.Select != nil:
		e.unsupported("CREATE TABLE ... SELECT", reasonLater)
	case n.Partition != nil:
		e.unsupported("PARTITION BY", reasonSemantics)
	case len(n.SplitIndex) > 0:
		e.fail(e.parseErrorAtWord("SPLIT"))
	}
	if e.err != nil {
		return nil
	}
	db := n.Table.Schema.O
	if db != "" {
		e.apply(ruleDBQualifier)
	} else {
		db = e.current
	}
	if db == "" {
		e.fail(errs.NoDatabaseSelected())
		return nil
	}
	e.target = db
	table := n.Table.Name.O
	if strings.HasPrefix(strings.ToLower(table), "__mysqlite_") {
		e.unsupported("table name starting with __mysqlite_", "the prefix is reserved for mysqlite's metadata")
		return nil
	}

	ct := &CreateTable{Table: table, IfNotExists: n.IfNotExists}
	tableCollation, autoStart := e.tableOptions(n.Options, ct)
	if e.err != nil {
		return nil
	}

	var cols []*ddlColumn
	byName := map[string]*ddlColumn{}
	var keys []ddlKey
	var pk []string
	for _, c := range n.Cols {
		col := e.column(c, tableCollation)
		if e.err != nil {
			return nil
		}
		lc := strings.ToLower(col.name)
		if byName[lc] != nil {
			e.fail(errs.DuplicateColumn(col.name))
			return nil
		}
		byName[lc] = col
		cols = append(cols, col)
		for _, o := range c.Options {
			switch o.Tp {
			case ast.ColumnOptionPrimaryKey:
				if pk != nil {
					e.fail(errs.MultiplePrimaryKey())
					return nil
				}
				pk = []string{col.name}
			case ast.ColumnOptionUniqKey:
				keys = append(keys, ddlKey{unique: true, columns: []string{col.name}})
			}
		}
	}

	for _, k := range n.Constraints {
		var unique bool
		switch k.Tp {
		case ast.ConstraintPrimaryKey:
			if pk != nil {
				e.fail(errs.MultiplePrimaryKey())
				return nil
			}
		case ast.ConstraintKey, ast.ConstraintIndex:
		case ast.ConstraintUniq, ast.ConstraintUniqKey, ast.ConstraintUniqIndex:
			unique = true
		case ast.ConstraintForeignKey:
			e.unsupported("FOREIGN KEY", reasonLater)
		case ast.ConstraintCheck:
			e.unsupported("CHECK", reasonLater)
		case ast.ConstraintFulltext:
			e.unsupported("FULLTEXT", reasonSemantics)
		default:
			e.unsupported("index type", reasonSemantics)
		}
		if k.Option != nil && !onlyIndexType(k.Option) {
			e.unsupported("index option", reasonLater)
		}
		if e.err != nil {
			return nil
		}
		names, sub := e.keyColumns(k.Keys, byName, unique || k.Tp == ast.ConstraintPrimaryKey)
		if e.err != nil {
			return nil
		}
		if k.Tp == ast.ConstraintPrimaryKey {
			pk = names
			continue
		}
		if strings.EqualFold(k.Name, "PRIMARY") {
			e.fail(errs.WrongIndexName(k.Name))
			return nil
		}
		keys = append(keys, ddlKey{name: k.Name, unique: unique, columns: names, subPart: sub})
	}
	for _, name := range pk {
		col := byName[strings.ToLower(name)]
		if col.explicitNull {
			e.fail(errs.PrimaryKeyNull())
			return nil
		}
		if col.lob {
			e.fail(errs.BlobKeyWithoutLength(col.name))
			return nil
		}
		col.pk = true
		col.notNull = true
	}

	// AUTO_INCREMENT: one column, which must be a key. SQLite has it only as the
	// INTEGER PRIMARY KEY of a single-column primary key.
	var auto *ddlColumn
	for _, c := range cols {
		if !c.auto {
			continue
		}
		if auto != nil {
			e.fail(errs.WrongAutoKey())
			return nil
		}
		auto = c
	}
	if auto != nil {
		switch {
		case len(pk) == 1 && strings.EqualFold(pk[0], auto.name):
		case firstKeyColumn(keys, pk, auto.name):
			e.unsupported("AUTO_INCREMENT column that is not the whole primary key", "SQLite has AUTOINCREMENT only on a single-column INTEGER PRIMARY KEY")
			return nil
		default:
			e.fail(errs.WrongAutoKey())
			return nil
		}
		ct.AutoIncrement = autoStart
	}

	// MySQL names an unnamed key after its first column, then _2, _3...
	used := map[string]bool{"primary": true}
	for _, k := range keys {
		if k.name == "" {
			continue
		}
		if used[strings.ToLower(k.name)] {
			e.fail(errs.DuplicateKeyName(k.name))
			return nil
		}
		used[strings.ToLower(k.name)] = true
	}
	for i := range keys {
		if keys[i].name != "" {
			continue
		}
		base, name := keys[i].columns[0], keys[i].columns[0]
		for n := 2; used[strings.ToLower(name)]; n++ {
			name = fmt.Sprintf("%s_%d", base, n)
		}
		keys[i].name = name
		used[strings.ToLower(name)] = true
	}

	e.apply(ruleCreateTable)
	var b strings.Builder
	b.WriteString("CREATE TABLE ")
	b.WriteString(quoteIdent(table))
	b.WriteString(" (")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(e.columnSQL(c, c == auto))
		ct.Facts = append(ct.Facts, columnFacts(c)...)
	}
	if len(pk) > 0 && auto == nil {
		b.WriteString(", PRIMARY KEY (")
		b.WriteString(identList(pk))
		b.WriteString(")")
	}
	b.WriteString(")")
	ct.SQL = append(ct.SQL, b.String())

	if len(keys) > 0 {
		e.apply(ruleCreateIndexes)
	}
	for _, k := range keys {
		sqliteName := IndexSQLiteName(table, k.name)
		stmt := "CREATE "
		if k.unique {
			stmt += "UNIQUE "
		}
		stmt += "INDEX " + quoteIdent(sqliteName) + " ON " + quoteIdent(table) + " (" + identList(k.columns) + ")"
		ct.SQL = append(ct.SQL, stmt)
		ct.Facts = append(ct.Facts, Fact{Column: "index:" + sqliteName, Key: factName, Value: k.name})
		if s := meta.FormatSubPart(k.subPart); s != "" {
			ct.Facts = append(ct.Facts, Fact{Column: "index:" + sqliteName, Key: factSubPart, Value: s})
		}
		ct.keys = append(ct.keys, IndexChange{Table: table, Name: k.name, Unique: k.unique, Columns: k.columns, SubPart: k.subPart})
	}
	ct.cols, ct.pk = cols, pk
	return &Statement{Kind: KindDDL, Database: db, Op: errs.OpOther, Create: ct}
}

func identList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = quoteIdent(n)
	}
	return strings.Join(q, ", ")
}

// firstKeyColumn reports a column that starts some key, which MySQL accepts for
// AUTO_INCREMENT.
func firstKeyColumn(keys []ddlKey, pk []string, name string) bool {
	if len(pk) > 0 && strings.EqualFold(pk[0], name) {
		return true
	}
	for _, k := range keys {
		if strings.EqualFold(k.columns[0], name) {
			return true
		}
	}
	return false
}

func onlyIndexType(o *ast.IndexOption) bool {
	// USING BTREE / HASH changes nothing visible on SQLite's B-trees.
	c := *o
	c.Tp = ast.IndexTypeInvalid
	return c.IsEmpty()
}

// keyColumns resolves the columns of a key and their prefix lengths (nil when no
// column has one). SQLite indexes whole values: for a non-unique key that finds the
// same rows as a prefix, for a unique key it changes which values collide, so a prefix
// there is 1235.
func (e *emitter) keyColumns(parts []*ast.IndexPartSpecification, byName map[string]*ddlColumn, unique bool) ([]string, []int) {
	var names []string
	sub := make([]int, len(parts))
	for i, p := range parts {
		e.keyPart(p, unique)
		if e.err != nil {
			return nil, nil
		}
		col := byName[strings.ToLower(p.Column.Name.O)]
		if col == nil {
			e.fail(errs.KeyColumnMissing(p.Column.Name.O))
			return nil, nil
		}
		if col.lob && p.Length <= 0 {
			e.fail(errs.BlobKeyWithoutLength(col.name))
			return nil, nil
		}
		names = append(names, col.name)
		sub[i] = max(p.Length, 0)
	}
	if meta.FormatSubPart(sub) == "" {
		sub = nil
	}
	return names, sub
}

// keyPart refuses the key parts SQLite cannot index as MySQL does.
func (e *emitter) keyPart(p *ast.IndexPartSpecification, unique bool) {
	switch {
	case p.Expr != nil:
		e.unsupported("functional key part", reasonLater)
	case p.Length > 0 && unique:
		e.unsupported("key prefix length on a unique key", "SQLite indexes whole values, so the prefix would not decide uniqueness")
	case p.Desc:
		e.unsupported("descending key part", reasonLater)
	}
}

// tableOptions reads the table options into sidecar facts and returns the default
// collation of the table's text columns and the AUTO_INCREMENT start.
func (e *emitter) tableOptions(opts []*ast.TableOption, ct *CreateTable) (string, uint64) {
	engine := "InnoDB"
	var cs, collation string
	var autoStart uint64
	for _, o := range opts {
		switch o.Tp {
		case ast.TableOptionEngine:
			engine = e.engineName(o.StrValue)
		case ast.TableOptionCharset:
			cs = o.StrValue
			if o.Default && cs == "" {
				cs = "utf8mb4"
			}
		case ast.TableOptionCollate:
			collation = o.StrValue
		case ast.TableOptionComment:
			ct.Facts = append(ct.Facts, Fact{Key: factComment, Value: o.StrValue})
		case ast.TableOptionAutoIncrement:
			autoStart = o.UintValue
		case ast.TableOptionRowFormat:
			name, ok := rowFormats[o.UintValue]
			if !ok {
				e.unsupported("ROW_FORMAT", reasonSemantics)
			}
			ct.Facts = append(ct.Facts, Fact{Key: factRowFormat, Value: name})
		default:
			e.unsupported("table option", reasonLater)
		}
		if e.err != nil {
			return "", 0
		}
	}
	co := e.resolveCollation(cs, collation, defaultCollation)
	if e.err != nil {
		return "", 0
	}
	ct.Facts = append(ct.Facts, Fact{Key: factEngine, Value: engine}, Fact{Key: factCollation, Value: co})
	return co, autoStart
}

var rowFormats = map[uint64]string{
	ast.RowFormatDefault: "DEFAULT", ast.RowFormatDynamic: "DYNAMIC", ast.RowFormatFixed: "FIXED",
	ast.RowFormatCompressed: "COMPRESSED", ast.RowFormatRedundant: "REDUNDANT", ast.RowFormatCompact: "COMPACT",
}

// engineName accepts the engines whose behaviour a SQLite table keeps: InnoDB and MyISAM
// are stored as written, so dumps re-create the same option.
func (e *emitter) engineName(name string) string {
	switch strings.ToLower(name) {
	case "innodb":
		return "InnoDB"
	case "myisam":
		return "MyISAM"
	case "sqlite":
		return "SQLite"
	case "memory", "heap", "csv", "archive", "blackhole", "merge", "mrg_myisam", "federated", "ndb", "ndbcluster", "example", "performance_schema":
		e.unsupported("ENGINE="+name, "tables are stored in the SQLite database file")
		return ""
	}
	e.fail(errs.UnknownStorageEngine(name))
	return ""
}

// resolveCollation picks the collation of a character set and/or collation clause, or
// inherited when both are empty. The result is a MySQL collation mysqlite can apply.
func (e *emitter) resolveCollation(cs, collation, inherited string) string {
	if cs == "" && collation == "" {
		return inherited
	}
	var set *charset.Charset
	if cs != "" {
		var ok bool
		if set, ok = charset.Lookup(cs); !ok {
			if charset.Known(cs) {
				e.unsupported("CHARACTER SET "+strings.ToLower(cs), "mysqlite stores UTF-8 only")
			} else {
				e.fail(errs.UnknownCharset(cs))
			}
			return ""
		}
	}
	if collation == "" {
		collation = set.DefaultCollation
	}
	co, ok := charset.LookupCollation(collation)
	if !ok {
		e.fail(errs.UnknownCollation(collation))
		return ""
	}
	if set != nil && co.Charset != set.Name {
		e.fail(errs.CollationCharsetMismatch(co.Name, set.Name))
		return ""
	}
	switch {
	case sqliteCollation(co.Name) == "":
		e.unsupported("COLLATE "+co.Name, reasonCollation0900)
	case co.Charset == "binary":
		e.unsupported("CHARACTER SET binary for a table", reasonLater)
	}
	return co.Name
}

// sqliteCollation is the registered SQLite collation that implements a MySQL one.
// It is empty for a 0900 collation mysqlite does not implement.
func sqliteCollation(mysqlCollation string) string {
	switch {
	case mysqlCollation == "utf8mb4_0900_ai_ci":
		return collate.AI0900
	case mysqlCollation == "utf8mb4_0900_bin":
		return collate.Bin0900
	case strings.Contains(mysqlCollation, "_0900_"):
		return ""
	case strings.HasSuffix(mysqlCollation, "_bin"):
		return collate.Bin
	}
	return collate.CI
}

func (e *emitter) column(c *ast.ColumnDef, tableCollation string) *ddlColumn {
	col := &ddlColumn{name: c.Name.Name.O, ft: c.Tp}
	if c.Name.Table.O != "" || c.Name.Schema.O != "" {
		e.unsupported("qualified column name", reasonLater)
		return nil
	}
	col.mysqlType, col.text, col.lob = e.mysqlType(col.name, c.Tp)
	if e.err != nil {
		return nil
	}
	var collateOpt string
	for _, o := range c.Options {
		switch o.Tp {
		case ast.ColumnOptionNotNull:
			col.notNull = true
		case ast.ColumnOptionNull:
			col.explicitNull = true
		case ast.ColumnOptionAutoIncrement:
			col.auto = true
		case ast.ColumnOptionDefaultValue:
			col.dflt = e.defaultLiteral(col, o.Expr)
		case ast.ColumnOptionComment:
			if lit := valueLiteral(o.Expr); lit != nil && lit.Kind == meta.LiteralString {
				col.comment = lit.Text
			}
		case ast.ColumnOptionCollate:
			collateOpt = o.StrValue
		case ast.ColumnOptionPrimaryKey, ast.ColumnOptionUniqKey:
			// Handled with the keys.
		case ast.ColumnOptionOnUpdate:
			e.unsupported("ON UPDATE", reasonLater)
		case ast.ColumnOptionGenerated:
			e.unsupported("generated column", reasonLater)
		case ast.ColumnOptionReference:
			e.unsupported("REFERENCES", reasonLater)
		case ast.ColumnOptionCheck:
			e.unsupported("CHECK", reasonLater)
		default:
			e.unsupported("column option", reasonLater)
		}
		if e.err != nil {
			return nil
		}
	}
	if col.notNull && col.explicitNull {
		// MySQL keeps the last of NULL / NOT NULL; mixing them is refused, not guessed.
		e.unsupported("NULL and NOT NULL on one column", reasonSemantics)
		return nil
	}
	if col.text {
		cs := c.Tp.GetCharset()
		col.collation = e.resolveCollation(cs, collateOpt, tableCollation)
	} else if collateOpt != "" {
		e.unsupported("COLLATE on a column that is not a character type", reasonSemantics)
	}
	if e.err != nil {
		return nil
	}
	if col.auto {
		switch c.Tp.GetType() {
		case mysql.TypeTiny, mysql.TypeShort, mysql.TypeInt24, mysql.TypeLong, mysql.TypeLonglong:
		case mysql.TypeFloat, mysql.TypeDouble:
			e.unsupported("AUTO_INCREMENT on a floating-point column", reasonSemantics)
			return nil
		default:
			e.fail(errs.WrongColumnSpec(col.name))
			return nil
		}
		if col.dflt != nil {
			e.fail(errs.InvalidDefault(col.name))
			return nil
		}
	}
	if col.dflt != nil && col.dflt.Kind == meta.LiteralNull && col.notNull {
		e.fail(errs.InvalidDefault(col.name))
		return nil
	}
	return col
}

var (
	reDate     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	reDatetime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(\.\d{1,6})?$`)
	reTime     = regexp.MustCompile(`^-?\d{1,3}:\d{2}:\d{2}(\.\d{1,6})?$`)
	reInteger  = regexp.MustCompile(`^-?\d+$`)
	reDecimal  = regexp.MustCompile(`^-?\d+\.\d+$`)
)

// decimalDefault writes a DECIMAL default with the column's scale, as MySQL stores it
// (DEFAULT 1.5 on DECIMAL(8,2) is 1.50). nil when the literal has more decimals than
// the scale, or is not a plain decimal number.
func decimalDefault(lit *meta.Literal, scale int) *meta.Literal {
	if lit.Kind != meta.LiteralInt && lit.Kind != meta.LiteralDecimal {
		return nil
	}
	intPart, frac, _ := strings.Cut(lit.Text, ".")
	if len(frac) > scale || strings.ContainsAny(lit.Text, "eE") {
		return nil
	}
	if scale == 0 {
		return &meta.Literal{Kind: meta.LiteralInt, Text: intPart}
	}
	return &meta.Literal{Kind: meta.LiteralDecimal, Text: intPart + "." + frac + strings.Repeat("0", scale-len(frac))}
}

// defaultLiteral checks a DEFAULT. Only constants are accepted; a value that would need
// a type conversion is refused until M3, an invalid one is 1067.
func (e *emitter) defaultLiteral(col *ddlColumn, x ast.ExprNode) *meta.Literal {
	lit := valueLiteral(x)
	if lit == nil {
		e.unsupported("DEFAULT expression", reasonLater)
		return nil
	}
	if lit.Kind == meta.LiteralNull {
		return lit
	}
	if col.lob || col.ft.GetType() == mysql.TypeJSON {
		e.fail(errs.BlobDefault(col.name))
		return nil
	}
	if strings.ContainsRune(lit.Text, 0) {
		e.unsupported("NUL byte in DEFAULT", reasonSemantics)
		return nil
	}
	numeric := lit.Kind != meta.LiteralString
	switch col.ft.GetType() {
	case mysql.TypeTiny, mysql.TypeShort, mysql.TypeInt24, mysql.TypeLong, mysql.TypeLonglong,
		mysql.TypeNewDecimal, mysql.TypeFloat, mysql.TypeDouble, mysql.TypeYear:
		if !numeric {
			// SHOW CREATE TABLE prints numeric defaults as strings (DEFAULT '1'), so an
			// exact number in a string is that number; any other string needs M3's
			// conversion.
			switch {
			case reInteger.MatchString(lit.Text):
				lit = &meta.Literal{Kind: meta.LiteralInt, Text: lit.Text}
			case reDecimal.MatchString(lit.Text):
				lit = &meta.Literal{Kind: meta.LiteralDecimal, Text: lit.Text}
			default:
				e.unsupported("string DEFAULT for a numeric column", "the conversion arrives with M3")
				return nil
			}
		}
		integer := col.ft.GetType() != mysql.TypeNewDecimal && col.ft.GetType() != mysql.TypeFloat && col.ft.GetType() != mysql.TypeDouble
		if integer && lit.Kind != meta.LiteralInt {
			e.unsupported("fractional DEFAULT for an integer column", "the rounding arrives with M3")
			return nil
		}
		if col.ft.GetType() == mysql.TypeNewDecimal {
			if lit = decimalDefault(lit, col.ft.GetDecimal()); lit == nil {
				e.unsupported("DEFAULT with more decimals than the column", "the rounding arrives with M3")
				return nil
			}
		}
	case mysql.TypeDate, mysql.TypeDatetime, mysql.TypeTimestamp, mysql.TypeDuration:
		re := map[byte]*regexp.Regexp{mysql.TypeDate: reDate, mysql.TypeDatetime: reDatetime, mysql.TypeTimestamp: reDatetime, mysql.TypeDuration: reTime}[col.ft.GetType()]
		if numeric || !re.MatchString(lit.Text) {
			e.unsupported("DEFAULT '"+lit.Text+"' for a temporal column", "temporal conversion and validation arrive with M3")
			return nil
		}
		if col.ft.GetType() == mysql.TypeTimestamp && !strings.HasPrefix(lit.Text, "0000-00-00") {
			// MySQL converts it from the session zone to UTC (ADR 0007); a SQLite DEFAULT
			// is fixed text, written as is.
			e.unsupported("literal DEFAULT for a TIMESTAMP column", "TIMESTAMP is stored in UTC and the default is not converted yet")
			return nil
		}
	case mysql.TypeBit:
		e.unsupported("DEFAULT for a BIT column", reasonLater)
		return nil
	default:
		if numeric {
			// A number for a string column is its text.
			lit = &meta.Literal{Kind: meta.LiteralString, Text: lit.Text}
		}
	}
	if err := meta.LimitsOf(col.mysqlType).CheckLiteral(col.name, lit, 1); err != nil {
		e.fail(errs.InvalidDefault(col.name))
		return nil
	}
	return lit
}

// columnSQL is the SQLite column definition. The declared type keeps the MySQL type
// for readability where its SQLite affinity is right, and adds the affinity word where
// it is not; the sidecar's type is what mysqlite reads back.
func (e *emitter) columnSQL(c *ddlColumn, auto bool) string {
	var b strings.Builder
	b.WriteString(quoteIdent(c.name))
	b.WriteString(" ")
	if auto {
		b.WriteString("INTEGER PRIMARY KEY AUTOINCREMENT")
	} else {
		b.WriteString(declaredType(c.mysqlType, c.ft))
	}
	if c.notNull {
		b.WriteString(" NOT NULL")
	}
	if c.dflt != nil {
		b.WriteString(" DEFAULT ")
		b.WriteString(literalSQL(c.dflt))
	}
	if c.text {
		b.WriteString(" COLLATE ")
		b.WriteString(sqliteCollation(c.collation))
	}
	if check := meta.LimitsOf(c.mysqlType).CheckSQL(c.name); check != "" {
		b.WriteString(" ")
		b.WriteString(check)
	}
	return b.String()
}

// declaredType adds an affinity word where SQLite's name-based affinity would convert
// values MySQL keeps as written: '0012' in a VARBINARY or '10' in a TIME would become
// numbers under NUMERIC affinity.
func declaredType(mysqlType string, ft *types.FieldType) string {
	affinity := ""
	switch ft.GetType() {
	case mysql.TypeDate, mysql.TypeDatetime, mysql.TypeTimestamp, mysql.TypeDuration, mysql.TypeJSON:
		affinity = "TEXT"
	case mysql.TypeString, mysql.TypeVarchar, mysql.TypeVarString:
		if ft.GetCharset() == binaryCharset {
			affinity = "BLOB"
		}
	case mysql.TypeBit:
		affinity = "INTEGER"
	}
	// SQLite's type name is words followed by at most one parenthesized argument list,
	// so trailing words (unsigned, zerofill) and the affinity move before it:
	// decimal(8,2) unsigned -> decimal unsigned(8,2).
	open := strings.IndexByte(mysqlType, '(')
	if open < 0 {
		return strings.TrimSpace(mysqlType + " " + affinity)
	}
	end := strings.IndexByte(mysqlType, ')')
	words := strings.TrimSpace(mysqlType[:open] + " " + mysqlType[end+1:] + " " + affinity)
	return strings.Join(strings.Fields(words), " ") + mysqlType[open:end+1]
}

func literalSQL(l *meta.Literal) string {
	if l.Kind == meta.LiteralString {
		return "'" + strings.ReplaceAll(l.Text, "'", "''") + "'"
	}
	return l.Text
}

// mysqlType is the column type as MySQL 8.0 prints it in SHOW CREATE TABLE: integer
// display widths only for TINYINT(1) and ZEROFILL, utf8mb4 lengths in characters.
func (e *emitter) mysqlType(column string, ft *types.FieldType) (name string, text, lob bool) {
	flen, dec := ft.GetFlen(), ft.GetDecimal()
	unsigned := mysql.HasUnsignedFlag(ft.GetFlag())
	zerofill := mysql.HasZerofillFlag(ft.GetFlag())
	sign := func(s string) string {
		switch {
		case zerofill:
			return s + " unsigned zerofill"
		case unsigned:
			return s + " unsigned"
		}
		return s
	}
	binary := ft.GetCharset() == binaryCharset
	switch ft.GetType() {
	case mysql.TypeTiny, mysql.TypeShort, mysql.TypeInt24, mysql.TypeLong, mysql.TypeLonglong:
		base := map[byte]string{mysql.TypeTiny: "tinyint", mysql.TypeShort: "smallint", mysql.TypeInt24: "mediumint", mysql.TypeLong: "int", mysql.TypeLonglong: "bigint"}[ft.GetType()]
		switch {
		case zerofill:
			if flen < 0 {
				flen = map[byte]int{mysql.TypeTiny: 3, mysql.TypeShort: 5, mysql.TypeInt24: 8, mysql.TypeLong: 10, mysql.TypeLonglong: 20}[ft.GetType()]
			}
			return sign(fmt.Sprintf("%s(%d)", base, flen)), false, false
		case ft.GetType() == mysql.TypeTiny && flen == 1:
			return sign("tinyint(1)"), false, false
		}
		return sign(base), false, false
	case mysql.TypeFloat, mysql.TypeDouble:
		base := "float"
		if ft.GetType() == mysql.TypeDouble {
			base = "double"
		}
		if flen > 0 && dec >= 0 {
			base = fmt.Sprintf("%s(%d,%d)", base, flen, dec)
		}
		return sign(base), false, false
	case mysql.TypeNewDecimal:
		if flen < 0 {
			flen = 10
		}
		if dec < 0 {
			dec = 0
		}
		return sign(fmt.Sprintf("decimal(%d,%d)", flen, dec)), false, false
	case mysql.TypeBit:
		if flen < 0 {
			flen = 1
		}
		return fmt.Sprintf("bit(%d)", flen), false, false
	case mysql.TypeYear:
		return "year", false, false
	case mysql.TypeDate:
		return "date", false, false
	case mysql.TypeDuration, mysql.TypeDatetime, mysql.TypeTimestamp:
		base := map[byte]string{mysql.TypeDuration: "time", mysql.TypeDatetime: "datetime", mysql.TypeTimestamp: "timestamp"}[ft.GetType()]
		if dec > 0 {
			base = fmt.Sprintf("%s(%d)", base, dec)
		}
		return base, false, false
	case mysql.TypeString, mysql.TypeVarchar, mysql.TypeVarString:
		varying := ft.GetType() != mysql.TypeString
		if flen < 0 {
			flen = 1
		}
		limit := 255
		if varying {
			limit = 65535
			if !binary {
				limit = e.varcharLimit(ft)
			}
		}
		if flen > limit {
			e.fail(errs.ColumnTooLong(column, limit))
			return "", false, false
		}
		base := map[[2]bool]string{{false, false}: "char", {true, false}: "varchar", {false, true}: "binary", {true, true}: "varbinary"}[[2]bool{varying, binary}]
		return fmt.Sprintf("%s(%d)", base, flen), !binary, false
	case mysql.TypeTinyBlob, mysql.TypeBlob, mysql.TypeMediumBlob, mysql.TypeLongBlob:
		size := map[byte]string{mysql.TypeTinyBlob: "tiny", mysql.TypeBlob: "", mysql.TypeMediumBlob: "medium", mysql.TypeLongBlob: "long"}[ft.GetType()]
		if ft.GetType() == mysql.TypeBlob && flen > 0 {
			// TEXT(n) / BLOB(n): the smallest type that holds n characters / bytes.
			bytes := flen
			if !binary {
				bytes = flen * maxBytes(ft.GetCharset())
			}
			switch {
			case bytes <= 255:
				size = "tiny"
			case bytes <= 65535:
				size = ""
			case bytes <= 16777215:
				size = "medium"
			default:
				size = "long"
			}
		}
		if binary {
			return size + "blob", false, true
		}
		return size + "text", true, true
	case mysql.TypeJSON:
		return "json", false, false
	case mysql.TypeEnum:
		e.unsupported("ENUM", reasonLater)
	case mysql.TypeSet:
		e.unsupported("SET", reasonLater)
	default:
		e.unsupported(strings.ToUpper(types.TypeToStr(ft.GetType(), ft.GetCharset()))+" column", reasonLater)
	}
	return "", false, false
}

func maxBytes(cs string) int {
	if c, ok := charset.Lookup(cs); ok {
		return c.MaxLen
	}
	return 4
}

// varcharLimit is the largest VARCHAR length of the column's character set: 65535 bytes.
func (e *emitter) varcharLimit(ft *types.FieldType) int {
	return 65535 / maxBytes(ft.GetCharset())
}

// Package meta produces MySQL column definitions for result sets
// (docs/03-protocol.md section 6). It is pure: it works on table facts supplied by the
// catalog and on select items supplied by the dialect, so every rule is unit-testable
// without a database.
//
// M1 covers direct table columns and literals. Values follow the MySQL 8.0 server
// source as understood; they have not been compared with a real server yet (ADR 0002
// addendum, oracle pending).
package meta

import (
	"math"
	"strings"
	"unicode/utf8"

	"mysqlite/internal/charset"
	"mysqlite/internal/errs"
	"mysqlite/internal/vars"
)

// Column type codes.
const (
	TypeDecimal    uint8 = 0x00
	TypeTiny       uint8 = 0x01
	TypeShort      uint8 = 0x02
	TypeLong       uint8 = 0x03
	TypeFloat      uint8 = 0x04
	TypeDouble     uint8 = 0x05
	TypeNull       uint8 = 0x06
	TypeTimestamp  uint8 = 0x07
	TypeLongLong   uint8 = 0x08
	TypeInt24      uint8 = 0x09
	TypeDate       uint8 = 0x0a
	TypeTime       uint8 = 0x0b
	TypeDatetime   uint8 = 0x0c
	TypeYear       uint8 = 0x0d
	TypeVarchar    uint8 = 0x0f
	TypeBit        uint8 = 0x10
	TypeJSON       uint8 = 0xf5
	TypeNewDecimal uint8 = 0xf6
	TypeEnum       uint8 = 0xf7
	TypeSet        uint8 = 0xf8
	TypeMediumBlob uint8 = 0xfa
	TypeBlob       uint8 = 0xfc
	TypeVarString  uint8 = 0xfd
	TypeString     uint8 = 0xfe
)

// Column flags.
const (
	FlagNotNull       uint16 = 1
	FlagPriKey        uint16 = 2
	FlagUniqueKey     uint16 = 4
	FlagMultipleKey   uint16 = 8
	FlagBlob          uint16 = 16
	FlagUnsigned      uint16 = 32
	FlagZerofill      uint16 = 64
	FlagBinary        uint16 = 128
	FlagEnum          uint16 = 256
	FlagAutoIncrement uint16 = 512
	FlagTimestamp     uint16 = 1024
	FlagSet           uint16 = 2048
	FlagNoDefault     uint16 = 4096
	FlagPartKey       uint16 = 16384
	FlagNum           uint16 = 32768
)

// Charset / collation ids.
const (
	CharsetBinary    uint16 = 63
	CollationDefault uint16 = 45 // utf8mb4_general_ci (docs/00-brief.md section 6)
	CollationUTF8MB3 uint16 = 33 // utf8mb3_general_ci, for NATIONAL CHAR/VARCHAR
)

// NotFixedDecimals is MySQL's NOT_FIXED_DEC: FLOAT and DOUBLE without a declared scale.
const NotFixedDecimals = 31

// degradedLength is the length reported for a column whose declared type is unknown;
// it is described as utf8mb4 text (65535 characters).
const degradedLength = 65535 * 4

// Table is what the catalog knows about one table.
type Table struct {
	Database string
	Name     string // as stored
	Columns  []Column
	// PrimaryKey lists the primary-key columns in key order.
	PrimaryKey []string
	// Indexes lists the other indexes, unique or not, in creation order.
	Indexes []Index
	// AutoIncrement names the AUTO_INCREMENT column, or "".
	AutoIncrement string
	// Options are the MySQL table options kept in the sidecar metadata (ENGINE,
	// COLLATE, COMMENT, ...), by sidecar key; nil for a table mysqlite did not create.
	Options map[string]string
}

// Column is one table column.
type Column struct {
	Name       string
	DeclType   string // declared type as stored by SQLite
	NotNull    bool
	HasDefault bool
	// Default is the default as SQLite stores it: SQL text such as 'abc', 1 or NULL;
	// nil without a DEFAULT clause.
	Default *string
	// Options are the column's sidecar metadata (MySQL type, COLLATE, COMMENT), by
	// sidecar key; nil when there is none.
	Options map[string]string
	// Def is a derived table column's result column; nil for a stored column.
	Def *ColumnDef
}

// Index is a non-primary index.
type Index struct {
	// Name is the MySQL key name, as error 1062 reports it.
	Name    string
	Columns []string
	Unique  bool
	// SQLiteName is the index name in the database file. SQLite index names are unique
	// per database, MySQL key names per table; the sidecar maps one to the other.
	SQLiteName string
}

// Item is one select-list item, as written in the query.
type Item struct {
	// Star is * or, with Qualifier, t.*.
	Star bool
	// Qualifier is the table name or alias written before the column.
	Qualifier string
	// Column is the column name as written. Empty for literals and stars.
	Column string
	// Literal is set for a constant item.
	Literal *Literal
	// Var is set for a system variable (@@name). Def and Value are filled by the
	// executor once the variable is read, before Columns.
	Var *Variable
	// Expr is set for a computed item; its type is inferred once the catalog is read.
	Expr *Expr
	// Name is the result column name: the alias, else the item as MySQL names it.
	Name string
}

// Variable is a system variable in the select list.
type Variable struct {
	Name  string
	Scope vars.ReadScope
	Def   *vars.Def
	Value vars.Value
}

// LiteralKind is the kind of a constant select item.
type LiteralKind int

const (
	LiteralNull LiteralKind = iota + 1
	LiteralInt
	LiteralDecimal
	LiteralFloat
	LiteralString
)

// Literal is a constant select item. Text is the number as written ("-12", "2.50",
// "1e3") or the string value.
type Literal struct {
	Kind LiteralKind
	Text string
}

// ColumnDef is a MySQL column definition.
type ColumnDef struct {
	Schema   string
	Table    string
	OrgTable string
	Name     string
	OrgName  string
	Charset  uint16
	Length   uint32
	Type     uint8
	Flags    uint16
	Decimals uint8
	// Degraded marks a column whose declared type was not recognized. It is described
	// as text; the caller journals the missing metadata (docs/04-session-catalog.md).
	Degraded bool
}

// Columns describes the result columns of a SELECT; from has no sources for a SELECT
// without FROM.
func Columns(items []Item, from From) ([]ColumnDef, *errs.Error) {
	return newScope(from).columns(items)
}

func (s scope) columns(items []Item) ([]ColumnDef, *errs.Error) {
	var defs []ColumnDef
	from := s.from
	for _, it := range items {
		switch {
		case it.Expr != nil:
			d, e := s.exprColumn(it.Expr, it.Name)
			if e != nil {
				return nil, e
			}
			defs = append(defs, d)
		case it.Literal != nil:
			defs = append(defs, literalColumn(*it.Literal, it.Name))
		case it.Var != nil:
			if it.Var.Def == nil {
				return nil, errs.UnknownSystemVariable(it.Var.Name)
			}
			defs = append(defs, variableColumn(*it.Var, it.Name))
		case it.Star:
			if len(from.Sources) == 0 {
				// MySQL: SELECT * without FROM is a syntax error caught by the parser.
				return nil, errs.Parse("*", 1)
			}
			srcs := from.Sources
			if it.Qualifier != "" {
				src, e := s.source(it.Qualifier)
				if e != nil {
					return nil, e
				}
				srcs = []Source{src}
			}
			for _, src := range srcs {
				if src.Table == nil {
					continue
				}
				for _, c := range src.Table.Columns {
					defs = append(defs, src.column(c, c.Name))
				}
			}
		default:
			src, c, e := s.resolve(it.Qualifier, it.Column, ClauseField)
			if e != nil {
				return nil, e
			}
			defs = append(defs, src.column(c, it.Name))
		}
	}
	return defs, nil
}

// Clauses named by error 1054, as MySQL names them.
const (
	ClauseWhere  = "where clause"
	ClauseOrder  = "order clause"
	ClauseOn     = "on clause"
	ClauseGroup  = "group statement"
	ClauseHaving = "having clause"
)

// Ref is a column reference outside the select list.
type Ref struct {
	Qualifier, Column string
	Clause            string // ClauseWhere or ClauseOrder
}

// CheckRefs resolves the column references outside the select list (ON, WHERE, ORDER
// BY), after Columns resolved the select list, as MySQL does. SQLite reports an unknown
// column without its clause and resolves select-list aliases in WHERE, which MySQL
// rejects, so the references are checked here before the query runs. ORDER BY may name
// a select-list alias.
func CheckRefs(refs []Ref, items []Item, from From) *errs.Error {
	return newScope(from).checkRefs(refs, items)
}

func (s scope) checkRefs(refs []Ref, items []Item) *errs.Error {
	for _, r := range refs {
		alias := r.Qualifier == "" && selectAlias(items, r.Column)
		// ORDER BY and HAVING look a name up in the select list first.
		if alias && (r.Clause == ClauseOrder || r.Clause == ClauseHaving) {
			continue
		}
		_, _, e := s.resolve(r.Qualifier, r.Column, r.Clause)
		// GROUP BY looks it up in the tables first.
		if e != nil && !(alias && r.Clause == ClauseGroup && e.Code == errs.UnknownColumn("", "").Code) {
			return e
		}
	}
	return nil
}

func selectAlias(items []Item, name string) bool {
	for _, it := range items {
		if !it.Star && strings.EqualFold(it.Name, name) {
			return true
		}
	}
	return false
}

// column finds a column by name; MySQL column names are case-insensitive.
// Column finds a column by name, which is case-insensitive.
func (t *Table) Column(name string) (Column, bool) {
	return t.column(name)
}

func (t *Table) column(name string) (Column, bool) {
	for _, c := range t.Columns {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return Column{}, false
}

// MySQLType is the column's MySQL type: the sidecar's when mysqlite created the table
// (the SQLite declared type may differ, e.g. INTEGER for AUTO_INCREMENT), else the
// declared type.
func (c Column) MySQLType() string {
	if t := c.Options[OptionType]; t != "" {
		return t
	}
	return c.DeclType
}

// Sidecar keys meta reads (the full list is in package catalog).
const (
	OptionType      = "type"
	OptionCollation = "collation"
)

func tableColumn(t *Table, c Column, visible, name string) ColumnDef {
	if c.Def != nil {
		return derivedColumn(t, c, visible, name)
	}
	ti, ok := ParseType(c.MySQLType())
	if !ok {
		ti, ok = enumType(c.MySQLType())
	}
	if !ok {
		ti = TypeInfo{Type: TypeVarString, Length: degradedLength, Charset: CollationDefault}
	}
	if co, found := charset.LookupCollation(c.Options[OptionCollation]); found && ti.Charset != CharsetBinary {
		// Lengths are in bytes of the column's character set.
		if co.Charset == "utf8mb3" && ti.Charset != CollationUTF8MB3 && ti.Length != math.MaxUint32 {
			ti.Length = ti.Length / 4 * 3
		}
		ti.Charset = co.ID
	}
	d := ColumnDef{
		Schema:   t.Database,
		Table:    visible,
		OrgTable: t.Name,
		Name:     name,
		OrgName:  c.Name,
		Charset:  ti.Charset,
		Length:   ti.Length,
		Type:     ti.Type,
		Flags:    ti.Flags,
		Decimals: ti.Decimals,
		Degraded: !ok,
	}
	d.Flags |= keyFlags(t, c.Name)
	auto := strings.EqualFold(t.AutoIncrement, c.Name)
	if auto {
		d.Flags |= FlagAutoIncrement
	}
	if c.NotNull || d.Flags&FlagPriKey != 0 {
		d.Flags |= FlagNotNull
		if !c.HasDefault && !auto {
			d.Flags |= FlagNoDefault
		}
	}
	return d
}

// keyFlags follows MySQL's table.cc: every primary-key part is PRI_KEY; the first part
// of another index is UNIQUE_KEY when the index is unique on one column, otherwise
// MULTIPLE_KEY; every indexed column is PART_KEY.
func keyFlags(t *Table, col string) uint16 {
	var f uint16
	for _, k := range t.PrimaryKey {
		if strings.EqualFold(k, col) {
			f |= FlagPriKey | FlagPartKey
		}
	}
	for _, ix := range t.Indexes {
		for i, k := range ix.Columns {
			if !strings.EqualFold(k, col) {
				continue
			}
			f |= FlagPartKey
			if i == 0 {
				if ix.Unique && len(ix.Columns) == 1 {
					f |= FlagUniqueKey
				} else {
					f |= FlagMultipleKey
				}
			}
		}
	}
	return f
}

// variableColumn describes @@name. MySQL types system variables by their kind: strings
// in the system charset (utf8mb3), integers as BIGINT, unsigned where the variable is.
// Lengths and flags are `oracle: pending`.
func variableColumn(v Variable, name string) ColumnDef {
	d := ColumnDef{Name: name, Charset: CharsetBinary}
	const numeric = FlagBinary | FlagNum
	switch v.Def.Kind {
	case vars.KindInt, vars.KindBool:
		d.Type, d.Flags, d.Length = TypeLongLong, numeric, 21
	case vars.KindUint:
		d.Type, d.Flags, d.Length = TypeLongLong, numeric|FlagUnsigned, 20
	case vars.KindDouble:
		d.Type, d.Flags, d.Length, d.Decimals = TypeDouble, numeric, 23, 6
	default:
		d.Type, d.Charset = TypeVarString, CollationUTF8MB3
		d.Length = uint32(utf8.RuneCountInString(v.Value.Text) * 3)
	}
	return d
}

// literalColumn follows MySQL's Item::make_field for constants: numbers have the
// binary collation (hence BINARY), strings the connection collation.
func literalColumn(l Literal, name string) ColumnDef {
	d := ColumnDef{Name: name, Charset: CharsetBinary}
	const numeric = FlagNotNull | FlagBinary | FlagNum
	switch l.Kind {
	case LiteralNull:
		d.Type, d.Flags = TypeNull, FlagBinary
	case LiteralInt:
		d.Type, d.Flags, d.Length = TypeLongLong, numeric, uint32(len(l.Text))
	case LiteralFloat:
		d.Type, d.Flags, d.Length, d.Decimals = TypeDouble, numeric, uint32(len(l.Text)), NotFixedDecimals
	case LiteralDecimal:
		precision, scale := decimalShape(l.Text)
		d.Type, d.Flags, d.Decimals = TypeNewDecimal, numeric, uint8(scale)
		d.Length = uint32(decimalLength(precision, scale, false))
	case LiteralString:
		// The literal takes the connection collation; SET NAMES arrives with M2.
		d.Type, d.Flags, d.Charset = TypeVarString, FlagNotNull, CollationDefault
		d.Length = uint32(utf8.RuneCountInString(l.Text) * 4)
	}
	return d
}

// decimalShape returns the precision and scale of a decimal literal such as "-0.50".
func decimalShape(text string) (precision, scale int) {
	intPart, frac, _ := strings.Cut(strings.TrimLeft(text, "-+"), ".")
	intDigits := len(strings.TrimLeft(intPart, "0"))
	return intDigits + len(frac), len(frac)
}

// decimalLength is MySQL's my_decimal_precision_to_length: digits, the point when there
// is a scale, and the sign unless unsigned.
func decimalLength(precision, scale int, unsigned bool) int {
	if precision == 0 {
		precision = 1
	}
	n := precision
	if scale > 0 {
		n++
	}
	if !unsigned {
		n++
	}
	return n
}

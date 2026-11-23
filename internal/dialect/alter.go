package dialect

import (
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"

	"mysqlite/internal/errs"
)

// ALTER TABLE (docs/05-dialect.md section 7). The specs run in order in one
// transaction; each sees the table as the earlier ones left it, so the executor checks
// them against the catalog one at a time.

// Alter is a translated ALTER TABLE.
type Alter struct {
	Table string // as written
	Specs []AlterSpec
}

// AlterKind is the kind of one ALTER TABLE spec.
type AlterKind int

// ALTER TABLE spec kinds.
const (
	AlterAddColumn AlterKind = iota + 1
	AlterDropColumn
	AlterRenameColumn
	AlterRenameTable
	AlterAddIndex
	AlterDropIndex
	AlterRenameIndex
	AlterTableOption
)

// AlterSpec is one ALTER TABLE spec.
type AlterSpec struct {
	Kind AlterKind
	// Name is the column (ADD, DROP, RENAME COLUMN), the key (DROP, RENAME INDEX) or the
	// table option's sidecar key.
	Name string
	// NewName is the new name of RENAME COLUMN, TABLE or INDEX; the option's value.
	NewName string
	// ColumnSQL is the SQLite column definition of ADD COLUMN, Facts its sidecar rows.
	ColumnSQL string
	Facts     []Fact
	// Index is the key of ADD INDEX; an empty Name is named by the executor after its
	// first column, as MySQL names it.
	Index *IndexChange
}

func (e *emitter) alterTable(n *ast.AlterTableStmt) *Statement {
	db, table, ok := e.ddlTable(n.Table)
	if !ok {
		return nil
	}
	e.target = db
	tableCollation := defaultCollation
	if t := e.lookup(table); t != nil && t.Options[factCollation] != "" {
		tableCollation = t.Options[factCollation]
	}
	if e.err != nil {
		return nil
	}
	a := &Alter{Table: table}
	for _, s := range n.Specs {
		e.alterSpec(s, db, table, tableCollation, a)
		if e.err != nil {
			return nil
		}
	}
	return &Statement{Kind: KindDDL, Database: db, Op: errs.OpOther, Alter: a}
}

// reasonRebuild explains the forms that need SQLite's table rebuild.
const reasonRebuild = "SQLite changes it only by rebuilding the table, which arrives later in M3"

func (e *emitter) alterSpec(s *ast.AlterTableSpec, db, table, tableCollation string, a *Alter) {
	switch s.Tp {
	case ast.AlterTableAlgorithm, ast.AlterTableLock:
		// How MySQL runs the change; SQLite has one way.
	case ast.AlterTableOption:
		e.alterOptions(s.Options, a)
	case ast.AlterTableAddColumns:
		e.addColumns(s, tableCollation, a)
	case ast.AlterTableAddConstraint:
		e.addConstraint(s.Constraint, table, a)
	case ast.AlterTableDropColumn:
		if s.IfExists {
			// MySQL 8.0 has no DROP COLUMN IF EXISTS.
			e.fail(e.parseErrorAtWord("IF"))
			return
		}
		a.Specs = append(a.Specs, AlterSpec{Kind: AlterDropColumn, Name: s.OldColumnName.Name.O})
	case ast.AlterTableDropIndex:
		switch {
		case s.IfExists:
			e.fail(e.parseErrorAtWord("IF"))
		case strings.EqualFold(s.Name, "PRIMARY"):
			e.unsupported("DROP INDEX `PRIMARY`", reasonRebuild)
		default:
			a.Specs = append(a.Specs, AlterSpec{Kind: AlterDropIndex, Name: s.Name})
		}
	case ast.AlterTableDropPrimaryKey:
		e.unsupported("DROP PRIMARY KEY", reasonRebuild)
	case ast.AlterTableRenameColumn:
		a.Specs = append(a.Specs, AlterSpec{Kind: AlterRenameColumn, Name: s.OldColumnName.Name.O, NewName: s.NewColumnName.Name.O})
	case ast.AlterTableRenameTable:
		if d := s.NewTable.Schema.O; d != "" && d != db {
			e.unsupported("ALTER TABLE ... RENAME TO another database", "each database is its own SQLite file")
			return
		}
		name := s.NewTable.Name.O
		if catalogReserved(name) {
			e.unsupported("table name starting with __mysqlite_ or sqlite_", "the prefixes are reserved for mysqlite's and SQLite's own tables")
			return
		}
		a.Specs = append(a.Specs, AlterSpec{Kind: AlterRenameTable, NewName: name})
	case ast.AlterTableRenameIndex:
		for _, k := range []string{s.FromKey.O, s.ToKey.O} {
			if strings.EqualFold(k, "PRIMARY") {
				e.fail(errs.WrongIndexName(k))
				return
			}
		}
		a.Specs = append(a.Specs, AlterSpec{Kind: AlterRenameIndex, Name: s.FromKey.O, NewName: s.ToKey.O})
	case ast.AlterTableModifyColumn:
		e.unsupported("MODIFY COLUMN", reasonRebuild)
	case ast.AlterTableChangeColumn:
		e.unsupported("CHANGE COLUMN", reasonRebuild)
	case ast.AlterTableAlterColumn:
		e.unsupported("ALTER COLUMN", reasonRebuild)
	default:
		e.unsupported("ALTER TABLE spec", reasonLater)
	}
}

// catalogReserved reports a table name mysqlite (__mysqlite_) or SQLite (sqlite_)
// reserves.
func catalogReserved(name string) bool {
	l := strings.ToLower(name)
	return strings.HasPrefix(l, "__mysqlite_") || strings.HasPrefix(l, "sqlite_")
}

// alterOptions keeps the table options the sidecar holds. The character set and
// collation would change the default of later columns and AUTO_INCREMENT the counter;
// they arrive later.
func (e *emitter) alterOptions(opts []*ast.TableOption, a *Alter) {
	for _, o := range opts {
		var key, value string
		switch o.Tp {
		case ast.TableOptionEngine:
			key, value = factEngine, e.engineName(o.StrValue)
		case ast.TableOptionComment:
			key, value = factComment, o.StrValue
		case ast.TableOptionRowFormat:
			name, ok := rowFormats[o.UintValue]
			if !ok {
				e.unsupported("ROW_FORMAT", reasonSemantics)
			}
			key, value = factRowFormat, name
		default:
			e.unsupported("ALTER TABLE option", reasonLater)
		}
		if e.err != nil {
			return
		}
		a.Specs = append(a.Specs, AlterSpec{Kind: AlterTableOption, Name: key, NewName: value})
	}
}

// addColumns translates ADD COLUMN. SQLite appends a column with a constant default; a
// position, a key of its own or NOT NULL without a default (MySQL fills the rows with
// the type's implicit default) need the rebuild.
func (e *emitter) addColumns(s *ast.AlterTableSpec, tableCollation string, a *Alter) {
	if s.Position != nil && s.Position.Tp != ast.ColumnPositionNone {
		e.unsupported("ADD COLUMN ... FIRST/AFTER", reasonRebuild)
		return
	}
	if len(s.NewConstraints) > 0 {
		e.unsupported("ADD (column, key)", reasonLater)
		return
	}
	for _, c := range s.NewColumns {
		col := e.column(c, tableCollation)
		if e.err != nil {
			return
		}
		unique := false
		for _, o := range c.Options {
			switch o.Tp {
			case ast.ColumnOptionPrimaryKey:
				e.unsupported("ADD COLUMN ... PRIMARY KEY", reasonRebuild)
			case ast.ColumnOptionUniqKey:
				unique = true
			}
		}
		switch {
		case e.err != nil:
			return
		case col.auto:
			e.unsupported("ADD COLUMN ... AUTO_INCREMENT", reasonRebuild)
			return
		case col.notNull && col.dflt == nil:
			e.unsupported("ADD COLUMN ... NOT NULL without DEFAULT", reasonRebuild)
			return
		}
		e.apply(ruleCreateTable)
		spec := AlterSpec{Kind: AlterAddColumn, Name: col.name, ColumnSQL: e.columnSQL(col, false), Facts: columnFacts(col)}
		a.Specs = append(a.Specs, spec)
		if unique {
			a.Specs = append(a.Specs, AlterSpec{Kind: AlterAddIndex, Index: &IndexChange{Unique: true, Columns: []string{col.name}}})
		}
	}
}

// columnFacts are the sidecar rows of one column.
func columnFacts(c *ddlColumn) []Fact {
	facts := []Fact{{Column: c.name, Key: factType, Value: c.mysqlType}}
	if c.text {
		facts = append(facts, Fact{Column: c.name, Key: factCollation, Value: c.collation})
	}
	if c.comment != "" {
		facts = append(facts, Fact{Column: c.name, Key: factComment, Value: c.comment})
	}
	return facts
}

// addConstraint translates ADD INDEX, ADD KEY and ADD UNIQUE.
func (e *emitter) addConstraint(k *ast.Constraint, table string, a *Alter) {
	var unique bool
	switch k.Tp {
	case ast.ConstraintKey, ast.ConstraintIndex:
	case ast.ConstraintUniq, ast.ConstraintUniqKey, ast.ConstraintUniqIndex:
		unique = true
	case ast.ConstraintPrimaryKey:
		e.unsupported("ADD PRIMARY KEY", reasonRebuild)
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
		return
	}
	if strings.EqualFold(k.Name, "PRIMARY") {
		e.fail(errs.WrongIndexName(k.Name))
		return
	}
	cols := e.indexParts(k.Keys)
	if e.err != nil {
		return
	}
	e.apply(ruleCreateIndexes)
	a.Specs = append(a.Specs, AlterSpec{Kind: AlterAddIndex, Index: &IndexChange{Table: table, Name: k.Name, Unique: unique, Columns: cols}})
}

// indexParts are the columns of a key; key parts SQLite cannot index as MySQL does are
// 1235.
func (e *emitter) indexParts(parts []*ast.IndexPartSpecification) []string {
	var cols []string
	for _, p := range parts {
		switch {
		case p.Expr != nil:
			e.unsupported("functional key part", reasonLater)
		case p.Length > 0:
			e.unsupported("key prefix length", "SQLite indexes whole values")
		case p.Desc:
			e.unsupported("descending key part", reasonLater)
		}
		if e.err != nil {
			return nil
		}
		cols = append(cols, p.Column.Name.O)
	}
	return cols
}

package dialect

import (
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"

	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// ALTER TABLE (docs/05-dialect.md section 7). The specs run in order in one
// transaction; each sees the table as the earlier ones left it, so the executor checks
// them against the catalog one at a time.

// Alter is a translated ALTER TABLE.
type Alter struct {
	Table string // as written
	Specs []AlterSpec
	// Rebuild is set when a spec rebuilds the table (AlterRebuild): the executor turns
	// foreign keys off and copies the database first (ADR 0008).
	Rebuild bool
}

// AlterKind is the kind of one ALTER TABLE spec.
type AlterKind int

// ALTER TABLE spec kinds.
const (
	AlterAddColumn AlterKind = iota + 1
	AlterDropColumn
	AlterRenameTable
	AlterAddIndex
	AlterDropIndex
	AlterRenameIndex
	AlterTableOption
	// AlterRebuild re-creates the table with Spec applied (Translator.Rebuild).
	AlterRebuild
)

// AlterSpec is one ALTER TABLE spec.
type AlterSpec struct {
	Kind AlterKind
	// Name is the column (ADD, DROP COLUMN), the key (DROP, RENAME INDEX) or the table
	// option's sidecar key.
	Name string
	// NewName is the new name of RENAME TABLE or INDEX; the option's value.
	NewName string
	// ColumnSQL is the SQLite column definition of ADD COLUMN, Facts its sidecar rows.
	ColumnSQL string
	Facts     []Fact
	// Index is the key of ADD INDEX; an empty Name is named by the executor after its
	// first column, as MySQL names it.
	Index *IndexChange
	// Spec is the parsed spec of AlterRebuild, and of DROP COLUMN, which rebuilds the
	// table when the column is in the primary key.
	Spec *ast.AlterTableSpec
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
	for _, s := range a.Specs {
		if s.Kind == AlterRebuild {
			a.Rebuild = true
		}
	}
	return &Statement{Kind: KindDDL, Database: db, Op: errs.OpOther, Alter: a}
}

func (e *emitter) alterSpec(s *ast.AlterTableSpec, db, table, tableCollation string, a *Alter) {
	switch s.Tp {
	case ast.AlterTableAlgorithm, ast.AlterTableLock:
		// How MySQL runs the change; SQLite has one way.
	case ast.AlterTableOption:
		e.alterOptions(s.Options, a)
	case ast.AlterTableAddColumns:
		e.addColumns(s, tableCollation, a)
	case ast.AlterTableAddConstraint:
		if s.Constraint.Tp == ast.ConstraintPrimaryKey {
			a.Specs = append(a.Specs, AlterSpec{Kind: AlterRebuild, Spec: s})
			return
		}
		e.addConstraint(s.Constraint, table, a)
	case ast.AlterTableDropColumn:
		if s.IfExists {
			// MySQL 8.0 has no DROP COLUMN IF EXISTS.
			e.fail(e.parseErrorAtWord("IF"))
			return
		}
		a.Specs = append(a.Specs, AlterSpec{Kind: AlterDropColumn, Name: s.OldColumnName.Name.O, Spec: s})
	case ast.AlterTableDropIndex:
		switch {
		case s.IfExists:
			e.fail(e.parseErrorAtWord("IF"))
		case strings.EqualFold(s.Name, "PRIMARY"):
			a.Specs = append(a.Specs, AlterSpec{Kind: AlterRebuild, Spec: s})
		default:
			a.Specs = append(a.Specs, AlterSpec{Kind: AlterDropIndex, Name: s.Name})
		}
	case ast.AlterTableDropPrimaryKey, ast.AlterTableModifyColumn, ast.AlterTableChangeColumn, ast.AlterTableAlterColumn:
		a.Specs = append(a.Specs, AlterSpec{Kind: AlterRebuild, Spec: s})
	case ast.AlterTableRenameColumn:
		// SQLite renames a column in place, but mysqlite's range and length CHECKs are
		// named after their column (errors report it), so the table is rebuilt.
		a.Specs = append(a.Specs, AlterSpec{Kind: AlterRebuild, Spec: s})
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

// addColumns translates ADD COLUMN. SQLite appends a column with a constant default in
// place; a position, a key of its own, AUTO_INCREMENT or NOT NULL without a default
// (MySQL fills the rows with the type's implicit default) rebuild the table.
func (e *emitter) addColumns(s *ast.AlterTableSpec, tableCollation string, a *Alter) {
	if len(s.NewConstraints) > 0 {
		e.unsupported("ADD (column, key)", reasonLater)
		return
	}
	rebuild := s.Position != nil && s.Position.Tp != ast.ColumnPositionNone
	var native []AlterSpec
	for _, c := range s.NewColumns {
		col := e.column(c, tableCollation)
		if e.err != nil {
			return
		}
		unique := false
		for _, o := range c.Options {
			switch o.Tp {
			case ast.ColumnOptionPrimaryKey:
				rebuild = true
			case ast.ColumnOptionUniqKey:
				unique = true
			}
		}
		if col.auto || (col.notNull && col.dflt == nil) {
			rebuild = true
		}
		e.apply(ruleCreateTable)
		native = append(native, AlterSpec{Kind: AlterAddColumn, Name: col.name, ColumnSQL: e.columnSQL(col, false), Facts: columnFacts(col)})
		if unique {
			native = append(native, AlterSpec{Kind: AlterAddIndex, Index: &IndexChange{Unique: true, Columns: []string{col.name}}})
		}
	}
	if rebuild {
		a.Specs = append(a.Specs, AlterSpec{Kind: AlterRebuild, Spec: s})
		return
	}
	a.Specs = append(a.Specs, native...)
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
	cols, sub := e.indexParts(k.Keys, unique)
	if e.err != nil {
		return
	}
	e.apply(ruleCreateIndexes)
	a.Specs = append(a.Specs, AlterSpec{Kind: AlterAddIndex, Index: &IndexChange{Table: table, Name: k.Name, Unique: unique, Columns: cols, SubPart: sub}})
}

// indexParts are the columns of a key and their prefix lengths (nil when no column has
// one); key parts SQLite cannot index as MySQL does are 1235.
func (e *emitter) indexParts(parts []*ast.IndexPartSpecification, unique bool) ([]string, []int) {
	var cols []string
	sub := make([]int, len(parts))
	for i, p := range parts {
		e.keyPart(p, unique)
		if e.err != nil {
			return nil, nil
		}
		cols = append(cols, p.Column.Name.O)
		sub[i] = max(p.Length, 0)
	}
	if meta.FormatSubPart(sub) == "" {
		sub = nil
	}
	return cols, sub
}

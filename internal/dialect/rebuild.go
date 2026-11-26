package dialect

import (
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"

	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// Rebuild is a table re-created by SQLite's 12-step procedure
// (https://sqlite.org/lang_altertable.html, "Making Other Kinds Of Table Schema
// Changes") for one ALTER TABLE spec SQLite cannot run in place. The new definition is
// the table's SHOW CREATE TABLE text with the spec applied, translated by the same
// CREATE TABLE path, so CREATE and ALTER produce the same SQLite tables.
type Rebuild struct {
	// Body is the new CREATE TABLE after the table name: "(<columns>, ...)".
	Body string
	// Columns are the new columns the copy fills, Select their values over the old
	// table (column references or literals), in the same order. Columns left out take
	// their default, or the next AUTO_INCREMENT id in Order.
	Columns []string
	Select  []string
	// Order is the copy's ORDER BY over the old table: the order AUTO_INCREMENT ids are
	// handed to existing rows.
	Order string
	// NotNull are old-table expressions the new definition makes NOT NULL: a NULL in
	// one fails the ALTER with 1138.
	NotNull []string
	// PrimaryKey are old-table expressions of the new primary key, for the 1062 check;
	// nil when the key is an added AUTO_INCREMENT column.
	PrimaryKey []string
	// Keys are the new table's other keys, created after the copy.
	Keys []IndexChange
	// Facts are the new table's sidecar rows.
	Facts []Fact
	// Auto is set when the new table has an AUTO_INCREMENT column; KeepCounter when it
	// is the old one, whose counter carries over.
	Auto, KeepCounter bool
	// Kept maps each old column that survives to its new name.
	Kept map[string]string
	// Copy is set when MySQL runs this change with ALGORITHM=COPY, a column type change:
	// the copied rows are the statement's affected rows and its info string's Records.
	Copy bool
	// ZeroDates are added DATE / DATETIME / TIMESTAMP NOT NULL columns without a
	// default: existing rows would take the zero date, which strict mode with
	// NO_ZERO_DATE refuses (1292 for row 1). A table without rows takes them.
	ZeroDates []ZeroDate
}

// ZeroDate is an added column whose implicit default is the zero date.
type ZeroDate struct {
	Column, Kind, Value string
}

// Rebuild plans the rebuild of old for spec. createSQL is old's SHOW CREATE TABLE text
// (catalog.CreateTableSQL without AUTO_INCREMENT).
func (t *Translator) Rebuild(createSQL string, old *meta.Table, spec *ast.AlterTableSpec) (*Rebuild, *errs.Error) {
	stmts, _, err := t.p.ParseSQL(createSQL)
	if err != nil || len(stmts) != 1 {
		return nil, errs.Unsupported("ALTER TABLE rebuild of this table", "mysqlite cannot re-read its definition")
	}
	n, ok := stmts[0].(*ast.CreateTableStmt)
	if !ok {
		return nil, errs.Unsupported("ALTER TABLE rebuild of this table", "mysqlite cannot re-read its definition")
	}
	e := &emitter{query: createSQL, current: old.Database, rules: map[string]bool{}, mode: t.mode, rebuilding: true}
	p := &rebuildPlan{e: e, n: n, table: old.Name, origin: map[*ast.ColumnDef]string{}}
	for _, c := range n.Cols {
		p.origin[c] = c.Name.Name.O
	}
	p.apply(spec)
	if e.err != nil {
		return nil, e.err
	}
	st := e.createTable(n)
	if e.err != nil {
		return nil, e.err
	}
	return p.plan(st.Create, old)
}

type rebuildPlan struct {
	e      *emitter
	n      *ast.CreateTableStmt
	table  string
	origin map[*ast.ColumnDef]string // the old column each new one copies; "" when added
}

func (p *rebuildPlan) find(name string) int {
	for i, c := range p.n.Cols {
		if strings.EqualFold(c.Name.Name.O, name) {
			return i
		}
	}
	return -1
}

func (p *rebuildPlan) apply(s *ast.AlterTableSpec) {
	e := p.e
	switch s.Tp {
	case ast.AlterTableModifyColumn:
		def := s.NewColumns[0]
		i := p.find(def.Name.Name.O)
		if i < 0 {
			e.fail(errs.UnknownColumn(def.Name.Name.O, p.table))
			return
		}
		p.origin[def] = p.origin[p.n.Cols[i]]
		p.n.Cols[i] = def
		p.position(i, s.Position)
	case ast.AlterTableChangeColumn:
		old, def := s.OldColumnName.Name.O, s.NewColumns[0]
		i := p.find(old)
		if i < 0 {
			e.fail(errs.UnknownColumn(old, p.table))
			return
		}
		if j := p.find(def.Name.Name.O); j >= 0 && j != i {
			e.fail(errs.DuplicateColumn(def.Name.Name.O))
			return
		}
		p.origin[def] = p.origin[p.n.Cols[i]]
		p.n.Cols[i] = def
		p.renameRefs(old, def.Name.Name.O)
		p.position(i, s.Position)
	case ast.AlterTableRenameColumn:
		old, to := s.OldColumnName.Name.O, s.NewColumnName.Name.O
		i := p.find(old)
		if i < 0 {
			e.fail(errs.UnknownColumn(old, p.table))
			return
		}
		if j := p.find(to); j >= 0 && j != i {
			e.fail(errs.DuplicateColumn(to))
			return
		}
		p.n.Cols[i].Name = &ast.ColumnName{Name: ast.NewCIStr(to)}
		p.renameRefs(old, to)
	case ast.AlterTableAlterColumn:
		def := s.NewColumns[0]
		i := p.find(def.Name.Name.O)
		if i < 0 {
			e.fail(errs.UnknownColumn(def.Name.Name.O, p.table))
			return
		}
		col := p.n.Cols[i]
		var opts []*ast.ColumnOption
		for _, o := range col.Options {
			if o.Tp != ast.ColumnOptionDefaultValue {
				opts = append(opts, o)
			}
		}
		// SET DEFAULT gives an option without a type; DROP DEFAULT gives none.
		for _, o := range def.Options {
			opts = append(opts, &ast.ColumnOption{Tp: ast.ColumnOptionDefaultValue, Expr: o.Expr})
		}
		col.Options = opts
	case ast.AlterTableAddColumns:
		for _, def := range s.NewColumns {
			if p.find(def.Name.Name.O) >= 0 {
				e.fail(errs.DuplicateColumn(def.Name.Name.O))
				return
			}
			p.origin[def] = ""
			p.n.Cols = append(p.n.Cols, def)
			p.position(len(p.n.Cols)-1, s.Position)
			if e.err != nil {
				return
			}
		}
	case ast.AlterTableDropColumn:
		name := s.OldColumnName.Name.O
		i := p.find(name)
		switch {
		case i < 0:
			e.fail(errs.CantDropKey(name))
			return
		case len(p.n.Cols) == 1:
			e.fail(errs.CantRemoveAllFields())
			return
		}
		p.n.Cols = append(p.n.Cols[:i], p.n.Cols[i+1:]...)
		p.dropRefs(name)
	case ast.AlterTableAddConstraint:
		if s.Constraint.Tp != ast.ConstraintPrimaryKey {
			e.unsupported("ALTER TABLE spec", reasonLater)
			return
		}
		p.n.Constraints = append(p.n.Constraints, s.Constraint)
		// A key column becomes NOT NULL; the DEFAULT NULL SHOW CREATE TABLE printed for a
		// nullable column goes, as MySQL leaves the column without a default.
		for _, part := range s.Constraint.Keys {
			if part.Column == nil {
				continue
			}
			if i := p.find(part.Column.Name.O); i >= 0 {
				p.dropNullDefault(p.n.Cols[i])
			}
		}
	case ast.AlterTableDropPrimaryKey, ast.AlterTableDropIndex:
		if !p.dropPrimaryKey() {
			e.fail(errs.CantDropKey("PRIMARY"))
		}
	default:
		e.unsupported("ALTER TABLE spec", reasonLater)
	}
}

// dropNullDefault removes a DEFAULT NULL option.
func (p *rebuildPlan) dropNullDefault(c *ast.ColumnDef) {
	var opts []*ast.ColumnOption
	for _, o := range c.Options {
		if o.Tp == ast.ColumnOptionDefaultValue {
			if lit := valueLiteral(o.Expr); lit != nil && lit.Kind == meta.LiteralNull {
				continue
			}
		}
		opts = append(opts, o)
	}
	c.Options = opts
}

// position moves column i as FIRST or AFTER another column.
func (p *rebuildPlan) position(i int, pos *ast.ColumnPosition) {
	if pos == nil || pos.Tp == ast.ColumnPositionNone {
		return
	}
	col := p.n.Cols[i]
	cols := append(append([]*ast.ColumnDef(nil), p.n.Cols[:i]...), p.n.Cols[i+1:]...)
	at := 0
	if pos.Tp == ast.ColumnPositionAfter {
		rel := pos.RelativeColumn.Name.O
		at = -1
		for j, c := range cols {
			if strings.EqualFold(c.Name.Name.O, rel) {
				at = j + 1
			}
		}
		if at < 0 {
			p.e.fail(errs.UnknownColumn(rel, p.table))
			return
		}
	}
	p.n.Cols = append(cols[:at], append([]*ast.ColumnDef{col}, cols[at:]...)...)
}

// renameRefs renames a column in the table's keys.
func (p *rebuildPlan) renameRefs(from, to string) {
	for _, k := range p.n.Constraints {
		for _, part := range k.Keys {
			if part.Column != nil && strings.EqualFold(part.Column.Name.O, from) {
				part.Column = &ast.ColumnName{Name: ast.NewCIStr(to)}
			}
		}
	}
}

// dropRefs removes a dropped column from the keys; a key left without columns goes
// with it, as MySQL drops it.
func (p *rebuildPlan) dropRefs(name string) {
	var kept []*ast.Constraint
	for _, k := range p.n.Constraints {
		var parts []*ast.IndexPartSpecification
		for _, part := range k.Keys {
			if part.Column == nil || !strings.EqualFold(part.Column.Name.O, name) {
				parts = append(parts, part)
			}
		}
		k.Keys = parts
		if len(parts) > 0 {
			kept = append(kept, k)
		}
	}
	p.n.Constraints = kept
}

func (p *rebuildPlan) dropPrimaryKey() bool {
	found := false
	var kept []*ast.Constraint
	for _, k := range p.n.Constraints {
		if k.Tp == ast.ConstraintPrimaryKey {
			found = true
			continue
		}
		kept = append(kept, k)
	}
	p.n.Constraints = kept
	for _, c := range p.n.Cols {
		var opts []*ast.ColumnOption
		for _, o := range c.Options {
			if o.Tp == ast.ColumnOptionPrimaryKey {
				found = true
				continue
			}
			opts = append(opts, o)
		}
		c.Options = opts
	}
	return found
}

// plan derives the copy from the translated new table.
func (p *rebuildPlan) plan(ct *CreateTable, old *meta.Table) (*Rebuild, *errs.Error) {
	r := &Rebuild{Body: strings.TrimPrefix(ct.SQL[0], "CREATE TABLE "+quoteIdent(ct.Table)+" "), Facts: ct.Facts, Keys: ct.keys, Kept: map[string]string{}}
	byName := map[string]*ast.ColumnDef{}
	for _, c := range p.n.Cols {
		byName[strings.ToLower(c.Name.Name.O)] = c
	}
	source := map[string]string{} // new column (lower case) -> old-table expression
	for _, c := range ct.cols {
		def := byName[strings.ToLower(c.name)]
		from := p.origin[def]
		if from == "" {
			// An added column: its default, or the next AUTO_INCREMENT id, or NOT NULL's
			// implicit default for existing rows.
			switch {
			case c.auto:
				r.Auto = true
			case c.dflt != nil || !c.notNull:
			default:
				v, ok := implicitDefault(c.mysqlType)
				if z, zero := zeroDate(c.mysqlType); !ok && zero && p.e.mode.HasStrictMode() && p.e.mode.HasNoZeroDateMode() {
					z.Column = c.name
					r.ZeroDates = append(r.ZeroDates, z)
					v, ok = "NULL", true // never copied: the table has no rows
				}
				if !ok {
					return nil, errs.Unsupported("ADD COLUMN "+c.mysqlType+" NOT NULL without DEFAULT", "the implicit default of the type is not implemented")
				}
				r.Columns = append(r.Columns, c.name)
				r.Select = append(r.Select, v)
				source[strings.ToLower(c.name)] = v
			}
			continue
		}
		oc, _ := old.Column(from)
		if !sameStorage(oc.MySQLType(), c.mysqlType) {
			return nil, errs.Unsupported("changing a column from "+oc.MySQLType()+" to "+c.mysqlType, "the conversion of stored values is not implemented")
		}
		if copiesRows(oc.MySQLType(), c.mysqlType) {
			r.Copy = true
		}
		expr := quoteIdent(oc.Name)
		r.Kept[oc.Name] = c.name
		r.Columns = append(r.Columns, c.name)
		r.Select = append(r.Select, expr)
		source[strings.ToLower(c.name)] = expr
		if c.notNull && !oc.NotNull && !strings.EqualFold(oc.Name, old.AutoIncrement) && !inKey(old.PrimaryKey, oc.Name) {
			r.NotNull = append(r.NotNull, expr)
		}
		if c.auto {
			r.Auto = true
			r.KeepCounter = strings.EqualFold(oc.Name, old.AutoIncrement)
		}
	}
	newCols := map[string]*ddlColumn{}
	for _, c := range ct.cols {
		newCols[strings.ToLower(c.name)] = c
	}
	for _, k := range ct.pk {
		expr, ok := source[strings.ToLower(k)]
		if !ok {
			r.PrimaryKey = nil
			break
		}
		// Duplicates are looked for under the new collation, which may fold values the
		// old one kept apart.
		if c := newCols[strings.ToLower(k)]; c != nil && c.text {
			expr += " COLLATE " + sqliteCollation(c.collation)
		}
		r.PrimaryKey = append(r.PrimaryKey, expr)
	}
	if len(old.PrimaryKey) > 0 {
		var order []string
		for _, k := range old.PrimaryKey {
			order = append(order, quoteIdent(k))
		}
		r.Order = strings.Join(order, ", ")
	} else {
		r.Order = "rowid"
	}
	return r, nil
}

func inKey(key []string, col string) bool {
	for _, k := range key {
		if strings.EqualFold(k, col) {
			return true
		}
	}
	return false
}

// sameStorage reports a type change the copy can make without converting values:
// the same type, or another type of the same family whose values SQLite stores alike
// (integers; VARCHAR and TEXT; VARBINARY and BLOB). The new type's range or length
// CHECK verifies the values. Other changes are 1235 rather than a guessed conversion.
func sameStorage(from, to string) bool {
	if strings.EqualFold(from, to) {
		return true
	}
	f, t := storageFamily(from), storageFamily(to)
	return f != "" && f == t
}

// copiesRows reports whether MySQL changes a column from type from to type to with
// ALGORITHM=COPY: any type change, except a VARCHAR that grows within the same size of
// length prefix (one byte below 256 bytes), which runs in place.
func copiesRows(from, to string) bool {
	if strings.EqualFold(from, to) {
		return false
	}
	f, fok := meta.ParseType(from)
	t, tok := meta.ParseType(to)
	varchar := func(ti meta.TypeInfo) bool { return ti.Type == meta.TypeVarString || ti.Type == meta.TypeVarchar }
	if fok && tok && varchar(f) && varchar(t) && f.Charset == t.Charset &&
		t.Length >= f.Length && (f.Length < 256) == (t.Length < 256) {
		return false
	}
	return true
}

func storageFamily(mysqlType string) string {
	ti, ok := meta.ParseType(mysqlType)
	if !ok {
		return ""
	}
	switch ti.Type {
	case meta.TypeTiny, meta.TypeShort, meta.TypeInt24, meta.TypeLong, meta.TypeLongLong:
		return "integer"
	case meta.TypeVarString, meta.TypeVarchar, meta.TypeBlob:
		if ti.Charset == meta.CharsetBinary {
			return "binary"
		}
		return "text"
	}
	return ""
}

// implicitDefault is MySQL's implicit default of a NOT NULL column for the rows that
// exist when it is added: 0 for integers and floating point, the empty string for strings. Other
// types (DECIMAL's scale, the zero dates) are refused for now.
// zeroDate is the zero value MySQL gives existing rows of an added temporal column.
func zeroDate(mysqlType string) (ZeroDate, bool) {
	ti, ok := meta.ParseType(mysqlType)
	if !ok {
		return ZeroDate{}, false
	}
	switch ti.Type {
	case meta.TypeDate:
		return ZeroDate{Kind: "date", Value: "0000-00-00"}, true
	case meta.TypeDatetime, meta.TypeTimestamp:
		return ZeroDate{Kind: "datetime", Value: "0000-00-00 00:00:00"}, true
	}
	return ZeroDate{}, false
}

func implicitDefault(mysqlType string) (string, bool) {
	ti, ok := meta.ParseType(mysqlType)
	if !ok {
		return "", false
	}
	switch ti.Type {
	case meta.TypeTiny, meta.TypeShort, meta.TypeInt24, meta.TypeLong, meta.TypeLongLong, meta.TypeFloat, meta.TypeDouble:
		return "0", true
	case meta.TypeVarString, meta.TypeVarchar, meta.TypeBlob, meta.TypeString:
		if ti.Charset == meta.CharsetBinary {
			return "", false
		}
		return "''", true
	}
	return "", false
}

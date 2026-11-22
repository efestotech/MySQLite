package dialect

import (
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"

	"mysqlite/internal/errs"
)

// DropTable is DROP TABLE: the executor checks the tables in one transaction and drops
// them all or none (MySQL 8.0's DDL is atomic).
type DropTable struct {
	Tables   []string // as written
	IfExists bool
}

// IndexChange is CREATE INDEX or DROP INDEX. The executor checks it against the
// catalog: columns, key names, TEXT/BLOB columns.
type IndexChange struct {
	Table   string
	Name    string // the MySQL key name
	Drop    bool
	Unique  bool
	Columns []string // as written
}

// IndexSQLiteName is the SQLite name of the index a table's MySQL key is stored as
// (rule R010).
func IndexSQLiteName(table, key string) string {
	return table + "__" + key
}

// ddlTable resolves the database and name of a table a DDL statement changes.
func (e *emitter) ddlTable(tn *ast.TableName) (db, table string, ok bool) {
	db = tn.Schema.O
	if db != "" {
		e.apply(ruleDBQualifier)
	} else {
		db = e.current
	}
	if db == "" {
		e.fail(errs.NoDatabaseSelected())
		return "", "", false
	}
	table = tn.Name.O
	if strings.HasPrefix(strings.ToLower(table), "__mysqlite_") {
		e.unsupported("table name starting with __mysqlite_", "the prefix is reserved for mysqlite's metadata")
		return "", "", false
	}
	return db, table, true
}

func (e *emitter) dropTable(n *ast.DropTableStmt) *Statement {
	switch {
	case n.IsView:
		e.unsupported("DROP VIEW", reasonLater)
	case n.TemporaryKeyword != ast.TemporaryNone:
		e.unsupported("DROP TEMPORARY TABLE", reasonLater)
	}
	if e.err != nil {
		return nil
	}
	dt := &DropTable{IfExists: n.IfExists}
	var db string
	for _, tn := range n.Tables {
		d, table, ok := e.ddlTable(tn)
		if !ok {
			return nil
		}
		if db != "" && d != db {
			e.unsupported("DROP TABLE across databases", "each database is its own SQLite file, so the drop could not be atomic")
			return nil
		}
		db = d
		dt.Tables = append(dt.Tables, table)
	}
	return &Statement{Kind: KindDDL, Database: db, Op: errs.OpOther, Drop: dt}
}

func (e *emitter) truncate(n *ast.TruncateTableStmt) *Statement {
	db, table, ok := e.ddlTable(n.Table)
	if !ok {
		return nil
	}
	return &Statement{Kind: KindDDL, Database: db, Op: errs.OpOther, Truncate: table}
}

func (e *emitter) createIndex(n *ast.CreateIndexStmt) *Statement {
	switch {
	case n.KeyType == ast.IndexKeyTypeFulltext:
		e.unsupported("FULLTEXT", reasonSemantics)
	case n.KeyType != ast.IndexKeyTypeNone && n.KeyType != ast.IndexKeyTypeUnique:
		e.unsupported("index type", reasonSemantics)
	case n.IfNotExists:
		// MySQL 8.0 has no CREATE INDEX IF NOT EXISTS.
		e.fail(e.parseErrorAtWord("IF"))
	case n.IndexOption != nil && !onlyIndexType(n.IndexOption):
		e.unsupported("index option", reasonLater)
	}
	if e.err != nil {
		return nil
	}
	db, table, ok := e.ddlTable(n.Table)
	if !ok {
		return nil
	}
	if strings.EqualFold(n.IndexName, "PRIMARY") {
		e.fail(errs.WrongIndexName(n.IndexName))
		return nil
	}
	ic := &IndexChange{Table: table, Name: n.IndexName, Unique: n.KeyType == ast.IndexKeyTypeUnique}
	for _, p := range n.IndexPartSpecifications {
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
		ic.Columns = append(ic.Columns, p.Column.Name.O)
	}
	e.apply(ruleCreateIndexes)
	return &Statement{Kind: KindDDL, Database: db, Op: errs.OpOther, Index: ic}
}

func (e *emitter) dropIndex(n *ast.DropIndexStmt) *Statement {
	switch {
	case n.IfExists:
		// MySQL 8.0 has no DROP INDEX IF EXISTS.
		e.fail(e.parseErrorAtWord("IF"))
	case n.IsHypo:
		e.unsupported("hypothetical index", reasonSemantics)
	case strings.EqualFold(n.IndexName, "PRIMARY"):
		e.unsupported("DROP INDEX `PRIMARY`", "the primary key is part of the SQLite table; changing it rebuilds the table (ALTER TABLE)")
	}
	if e.err != nil {
		return nil
	}
	db, table, ok := e.ddlTable(n.Table)
	if !ok {
		return nil
	}
	return &Statement{Kind: KindDDL, Database: db, Op: errs.OpOther, Index: &IndexChange{Table: table, Name: n.IndexName, Drop: true}}
}

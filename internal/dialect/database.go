package dialect

import (
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
)

// CreateDatabase is the CREATE DATABASE (or CREATE SCHEMA) of KindDDL.
type CreateDatabase struct {
	Name        string
	IfNotExists bool
}

// The only character set and collation a database can have: the server's, which
// SHOW CREATE DATABASE reports. mysqlite stores no per-database options, so another
// default collation would silently not apply to the tables created later.
const (
	databaseCharset   = "utf8mb4"
	databaseCollation = "utf8mb4_general_ci"
)

// createDatabase translates CREATE DATABASE: a new database file (docs/05-dialect.md
// section 4). The options mysqldump writes are accepted when they name the server's
// defaults.
func (e *emitter) createDatabase(n *ast.CreateDatabaseStmt) *Statement {
	for _, o := range n.Options {
		switch o.Tp {
		case ast.DatabaseOptionCharset:
			if !strings.EqualFold(o.Value, databaseCharset) {
				e.unsupported("CREATE DATABASE CHARACTER SET "+o.Value, "every database uses the server's utf8mb4")
				return nil
			}
		case ast.DatabaseOptionCollate:
			if !strings.EqualFold(o.Value, databaseCollation) {
				e.unsupported("CREATE DATABASE COLLATE "+o.Value, "every database uses the server's "+databaseCollation)
				return nil
			}
		case ast.DatabaseOptionEncryption:
			if !strings.EqualFold(o.Value, "N") {
				e.unsupported("CREATE DATABASE ENCRYPTION", reasonSemantics)
				return nil
			}
		default:
			e.unsupported("CREATE DATABASE option", reasonSemantics)
			return nil
		}
	}
	return &Statement{Kind: KindDDL, CreateDB: &CreateDatabase{Name: n.Name.O, IfNotExists: n.IfNotExists}}
}

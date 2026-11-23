package wire

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"

	"mysqlite/internal/catalog"
	"mysqlite/internal/dialect"
	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
)

// The ALTER TABLE rebuild: SQLite's 12-step procedure
// (https://sqlite.org/lang_altertable.html, "Making Other Kinds Of Table Schema
// Changes"), with the steps numbered as there:
//
//  1. foreign keys off, before the transaction (prepareRebuild);
//  2. BEGIN (handler.ddl);
//  3. the table has no triggers and the database no views, else 1235;
//  4. CREATE TABLE under a temporary name;
//  5. INSERT ... SELECT the rows;
//  6. DROP TABLE the old one;
//  7. RENAME the new one;
//  8. CREATE INDEX its keys (and the sidecar facts);
//  9. no views to re-create;
//  10. PRAGMA foreign_key_check finds no new violation, else the ALTER fails;
//  11. COMMIT (handler.ddl);
//  12. foreign keys back on, after the transaction.

// rebuildTable is the temporary name of step 4; mysqlite's prefix keeps it from any
// user table.
const rebuildTable = catalog.HiddenPrefix + "rebuild"

// prepareRebuild runs before the DDL transaction of an ALTER TABLE that may rebuild:
// PRAGMA foreign_keys is a no-op inside a transaction, so it is turned off here and
// back on after COMMIT or ROLLBACK. fkOn reports whether foreign keys were on.
func (h *handler) prepareRebuild(ctx context.Context, conn *sql.Conn) (restore func(), fkOn bool, err error) {
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fkOn); err != nil {
		return nil, false, fmt.Errorf("wire: read foreign_keys: %w", err)
	}
	if !fkOn {
		return nil, false, nil
	}
	if err := exec(ctx, conn, "PRAGMA foreign_keys = OFF"); err != nil {
		return nil, false, err
	}
	return func() {
		if err := exec(context.Background(), conn, "PRAGMA foreign_keys = ON"); err != nil {
			h.log.Error("internal error", "error", err.Error())
		}
	}, true, nil
}

// mayRebuild reports an ALTER TABLE that may rebuild t: a rebuild spec, or DROP COLUMN
// of a primary-key column.
func mayRebuild(a *dialect.Alter, t *meta.Table) bool {
	if a.Rebuild {
		return true
	}
	for _, s := range a.Specs {
		if s.Kind == dialect.AlterDropColumn && t != nil && containsFold(t.PrimaryKey, s.Name) {
			return true
		}
	}
	return false
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// rebuild re-creates t with spec applied, in the DDL transaction (steps 3 to 10).
func (h *handler) rebuild(ctx context.Context, conn *sql.Conn, t *meta.Table, spec *ast.AlterTableSpec, fkOn bool) error {
	if err := rebuildable(ctx, conn, t); err != nil {
		return err
	}
	r, e := h.tr.Rebuild(catalog.CreateTableSQL(t, 0), t, spec)
	if e != nil {
		return e
	}
	if err := keepsReferences(ctx, conn, t, r); err != nil {
		return err
	}
	before := 0
	if fkOn {
		var err error
		if before, err = fkViolations(ctx, conn); err != nil {
			return err
		}
	}
	for _, expr := range r.NotNull {
		var null bool
		if err := conn.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM "+ident(t.Name)+" WHERE "+expr+" IS NULL)").Scan(&null); err != nil {
			return fmt.Errorf("wire: look for NULLs in %s: %w", t.Name, err)
		}
		if null {
			return errs.InvalidUseOfNull()
		}
	}
	if len(r.PrimaryKey) > 0 {
		entry, dup, err := firstDuplicate(ctx, conn, t.Name, r.PrimaryKey)
		if err != nil {
			return err
		}
		if dup {
			return errs.DuplicateEntry(entry, t.Name+".PRIMARY")
		}
	}
	if len(r.Columns) == 0 {
		return errs.Unsupported("ALTER TABLE that keeps no column's values", "the copy has nothing to select")
	}
	var oldSeq int64
	if t.AutoIncrement != "" {
		err := conn.QueryRowContext(ctx, "SELECT seq FROM sqlite_sequence WHERE name = ?", t.Name).Scan(&oldSeq)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("wire: AUTO_INCREMENT of %s: %w", t.Name, err)
		}
	}

	cols := make([]string, len(r.Columns))
	for i, c := range r.Columns {
		cols[i] = ident(c)
	}
	steps := []string{
		"CREATE TABLE " + ident(rebuildTable) + " " + r.Body,
		"INSERT INTO " + ident(rebuildTable) + " (" + strings.Join(cols, ", ") + ") SELECT " + strings.Join(r.Select, ", ") +
			" FROM " + ident(t.Name) + " ORDER BY " + r.Order,
		"DROP TABLE " + ident(t.Name),
		"ALTER TABLE " + ident(rebuildTable) + " RENAME TO " + ident(t.Name),
	}
	for _, q := range steps {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			// A value the new type's CHECK refuses, mapped like any write.
			return fmt.Errorf("wire: rebuild %s: %w", t.Name, err)
		}
	}
	if r.KeepCounter {
		// The copy left the counter at the largest id; MySQL keeps the old one when it is
		// higher (ids lost, AUTO_INCREMENT=n).
		if err := exec(ctx, conn, "UPDATE sqlite_sequence SET seq = max(seq, ?) WHERE name = ?", oldSeq, t.Name); err != nil {
			return err
		}
		if err := exec(ctx, conn, "INSERT INTO sqlite_sequence (name, seq) SELECT ?, ? WHERE ? > 0 AND NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = ?)",
			t.Name, oldSeq, oldSeq, t.Name); err != nil {
			return err
		}
	}
	facts := make([]catalog.SidecarFact, len(r.Facts))
	for i, f := range r.Facts {
		facts[i] = catalog.SidecarFact{Column: f.Column, Key: f.Key, Value: f.Value}
	}
	if err := catalog.WriteSidecar(ctx, conn, t.Name, facts); err != nil {
		return err
	}
	for _, k := range r.Keys {
		if err := buildIndex(ctx, conn, t, dialect.IndexSQLiteName(t.Name, k.Name), k.Name, k.Unique, k.Columns); err != nil {
			return err
		}
	}
	if fkOn {
		after, err := fkViolations(ctx, conn)
		if err != nil {
			return errs.Unsupported("ALTER TABLE that breaks a FOREIGN KEY", err.Error())
		}
		if after > before {
			return errs.Unsupported("ALTER TABLE that breaks a FOREIGN KEY", "PRAGMA foreign_key_check finds rows without their parent")
		}
	}
	return nil
}

// rebuildable refuses the tables the rebuild could damage: one mysqlite did not create
// (its SQLite definition may hold what MySQL DDL cannot describe), a default mysqlite
// cannot re-read, foreign keys of its own (CREATE TABLE has no FOREIGN KEY yet), and
// triggers or views, which the 12 steps would have to re-create (steps 3 and 9).
func rebuildable(ctx context.Context, conn *sql.Conn, t *meta.Table) error {
	const why = "ALTER TABLE rebuild of this table"
	if t.Options == nil {
		return errs.Unsupported(why, "mysqlite did not create the table, so its definition may hold what MySQL DDL cannot describe")
	}
	for _, c := range t.Columns {
		if c.Default != nil {
			if _, ok := catalog.DefaultValue(*c.Default); !ok {
				return errs.Unsupported(why, "the default of column "+c.Name+" is not a literal")
			}
		}
	}
	var fks, others int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM pragma_foreign_key_list(?)", t.Name).Scan(&fks); err != nil {
		return fmt.Errorf("wire: foreign keys of %s: %w", t.Name, err)
	}
	if fks > 0 {
		return errs.Unsupported(why, "the table has FOREIGN KEY constraints, which CREATE TABLE does not support yet")
	}
	if err := conn.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_schema WHERE type = 'view' OR (type = 'trigger' AND tbl_name = ? COLLATE NOCASE)", t.Name).Scan(&others); err != nil {
		return fmt.Errorf("wire: triggers and views of %s: %w", t.Name, err)
	}
	if others > 0 {
		return errs.Unsupported(why, "the database has views or the table has triggers, which the rebuild would have to re-create")
	}
	return nil
}

// keepsReferences refuses a rebuild that drops or renames a column another table's
// FOREIGN KEY references: with foreign keys off, SQLite would not follow the change.
func keepsReferences(ctx context.Context, conn *sql.Conn, t *meta.Table, r *dialect.Rebuild) error {
	rows, err := conn.QueryContext(ctx,
		`SELECT p."to" FROM sqlite_schema m, pragma_foreign_key_list(m.name) p
		 WHERE m.type = 'table' AND p."table" = ? COLLATE NOCASE AND m.name <> ? COLLATE NOCASE`, t.Name, t.Name)
	if err != nil {
		return fmt.Errorf("wire: foreign keys referencing %s: %w", t.Name, err)
	}
	defer rows.Close()
	var referenced []string
	for rows.Next() {
		var to sql.NullString
		if err := rows.Scan(&to); err != nil {
			return fmt.Errorf("wire: foreign keys referencing %s: %w", t.Name, err)
		}
		if to.Valid {
			referenced = append(referenced, to.String)
		} else {
			referenced = append(referenced, t.PrimaryKey...)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("wire: foreign keys referencing %s: %w", t.Name, err)
	}
	for _, col := range referenced {
		kept := false
		for from, to := range r.Kept {
			if strings.EqualFold(from, col) && strings.EqualFold(to, col) {
				kept = true
			}
		}
		if !kept {
			return errs.Unsupported("ALTER TABLE that drops or renames a column a FOREIGN KEY references", "SQLite would leave the reference dangling")
		}
	}
	return nil
}

// fkViolations counts the rows PRAGMA foreign_key_check reports. It fails when a
// foreign key no longer matches its parent (SQLite's "foreign key mismatch").
func fkViolations(ctx context.Context, conn *sql.Conn) (int, error) {
	rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return 0, fmt.Errorf("wire: foreign_key_check: %w", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("wire: foreign_key_check: %w", err)
	}
	return n, nil
}

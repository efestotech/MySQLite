package wire

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

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
//  0. the backup copy of ADR 0008, before the transaction (backup);
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
	if err := execSQL(ctx, conn, "PRAGMA foreign_keys = OFF"); err != nil {
		return nil, false, err
	}
	return func() {
		if err := execSQL(context.Background(), conn, "PRAGMA foreign_keys = ON"); err != nil {
			h.log.Error("internal error", "error", err.Error())
		}
	}, true, nil
}

// rebuildSteps name the statements of steps 4 to 7, for rebuildStep.
var rebuildSteps = []string{"create", "copy", "drop", "rename"}

// rebuildStep runs after each rebuild step ("create", "copy", "drop", "rename", and
// "checked" before COMMIT); the crash-safety test replaces it to kill the process
// there.
var rebuildStep = func(string) {}

// keepBackups is how many backups of a database ADR 0008 keeps.
const keepBackups = 3

// vacuumInto copies the database conn is connected to into a new file; tests replace it
// to make the copy fail.
var vacuumInto = func(ctx context.Context, conn *sql.Conn, path string) error {
	return execSQL(ctx, conn, "VACUUM INTO ?", path)
}

// backup copies the whole database before a rebuild (ADR 0008), on the writer and
// outside a transaction (VACUUM cannot run in one); the writer is held for the whole
// statement, so no other write lands between the copy and the rebuild. A failed copy
// fails the ALTER. Only the newest keepBackups copies are kept.
func (h *handler) backup(ctx context.Context, conn *sql.Conn, database string) error {
	path, err := h.eng.BackupPath(database, time.Now())
	if err != nil {
		return err
	}
	if err := vacuumInto(ctx, conn, path); err != nil {
		return fmt.Errorf("wire: backup before ALTER TABLE: %w", err)
	}
	h.log.Info("backup before ALTER TABLE", "database", database, "file", path)
	return h.eng.PruneBackups(database, keepBackups)
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
	if len(r.ZeroDates) > 0 {
		var rows bool
		if err := conn.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM "+ident(t.Name)+")").Scan(&rows); err != nil {
			return fmt.Errorf("wire: look for rows in %s: %w", t.Name, err)
		}
		if z := r.ZeroDates[0]; rows {
			return errs.IncorrectTemporal(z.Kind, z.Value, z.Column, 1)
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
	for i, q := range steps {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			if i == 1 && errs.CopyRowError(err, 1) != nil {
				// A value the new type's CHECK refuses: MySQL names the row.
				return copyRowError(ctx, conn, q, err)
			}
			return fmt.Errorf("wire: rebuild %s: %w", t.Name, err)
		}
		rebuildStep(rebuildSteps[i])
	}
	if r.KeepCounter {
		// The copy left the counter at the largest id; MySQL keeps the old one when it is
		// higher (ids lost, AUTO_INCREMENT=n).
		if err := execSQL(ctx, conn, "UPDATE sqlite_sequence SET seq = max(seq, ?) WHERE name = ?", oldSeq, t.Name); err != nil {
			return err
		}
		if err := execSQL(ctx, conn, "INSERT INTO sqlite_sequence (name, seq) SELECT ?, ? WHERE ? > 0 AND NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = ?)",
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
	rebuildStep("checked")
	return nil
}

// copyRowError finds the first row, in copy order, that the copy statement refuses, by
// copying the rows one at a time, and returns MySQL's error for it. A failed statement
// leaves nothing behind, and the rebuild is undone by the ALTER's savepoint.
func copyRowError(ctx context.Context, conn *sql.Conn, copySQL string, first error) error {
	for row := 1; ; row++ {
		r, err := conn.ExecContext(ctx, copySQL+" LIMIT 1 OFFSET "+strconv.Itoa(row-1))
		if err != nil {
			if e := errs.CopyRowError(err, row); e != nil {
				return e
			}
			return err
		}
		if n, err := r.RowsAffected(); err != nil || n == 0 {
			return errs.CopyRowError(first, 1)
		}
	}
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
	return indexesRebuildable(ctx, conn, t)
}

// indexesRebuildable refuses a table with an index the rebuild would not re-create as
// it is: one the catalog does not list, a partial index, or a key part that is an
// expression or descending (another tool's CREATE INDEX).
func indexesRebuildable(ctx context.Context, conn *sql.Conn, t *meta.Table) error {
	const why = "ALTER TABLE rebuild of a table with an index mysqlite cannot re-create"
	type index struct {
		name    string
		partial bool
	}
	rows, err := conn.QueryContext(ctx, "SELECT name, partial FROM pragma_index_list(?) WHERE origin <> 'pk'", t.Name)
	if err != nil {
		return fmt.Errorf("wire: indexes of %s: %w", t.Name, err)
	}
	var list []index
	for rows.Next() {
		var ix index
		if err := rows.Scan(&ix.name, &ix.partial); err != nil {
			rows.Close()
			return fmt.Errorf("wire: indexes of %s: %w", t.Name, err)
		}
		list = append(list, ix)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("wire: indexes of %s: %w", t.Name, err)
	}
	for _, ix := range list {
		known := false
		for _, k := range t.Indexes {
			if strings.EqualFold(k.SQLiteName, ix.name) {
				known = true
			}
		}
		if !known || ix.partial {
			return errs.Unsupported(why, "index "+ix.name+" is partial or not one mysqlite describes")
		}
		var odd int
		if err := conn.QueryRowContext(ctx,
			"SELECT count(*) FROM pragma_index_xinfo(?) WHERE key = 1 AND (cid < 0 OR desc = 1)", ix.name).Scan(&odd); err != nil {
			return fmt.Errorf("wire: columns of index %s: %w", ix.name, err)
		}
		if odd > 0 {
			return errs.Unsupported(why, "index "+ix.name+" has an expression or a descending key part")
		}
	}
	return nil
}

// keepsReferences refuses a rebuild that drops or renames a column another table's
// FOREIGN KEY references: with foreign keys off, SQLite would not follow the change.
func keepsReferences(ctx context.Context, conn *sql.Conn, t *meta.Table, r *dialect.Rebuild) error {
	referenced, err := referencedColumns(ctx, conn, t)
	if err != nil {
		return err
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

// foreignKeyColumns are t's columns a foreign key uses: its own child columns and the
// columns other tables' foreign keys reference.
func foreignKeyColumns(ctx context.Context, conn *sql.Conn, t *meta.Table) ([]string, error) {
	cols, err := referencedColumns(ctx, conn, t)
	if err != nil {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT "from" FROM pragma_foreign_key_list(?)`, t.Name)
	if err != nil {
		return nil, fmt.Errorf("wire: foreign keys of %s: %w", t.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var from string
		if err := rows.Scan(&from); err != nil {
			return nil, fmt.Errorf("wire: foreign keys of %s: %w", t.Name, err)
		}
		cols = append(cols, from)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("wire: foreign keys of %s: %w", t.Name, err)
	}
	return cols, nil
}

// referencedColumns are the columns of t other tables' foreign keys reference; a key
// without a column list references t's primary key.
func referencedColumns(ctx context.Context, conn *sql.Conn, t *meta.Table) ([]string, error) {
	rows, err := conn.QueryContext(ctx,
		`SELECT p."to" FROM sqlite_schema m, pragma_foreign_key_list(m.name) p
		 WHERE m.type = 'table' AND p."table" = ? COLLATE NOCASE AND m.name <> ? COLLATE NOCASE`, t.Name, t.Name)
	if err != nil {
		return nil, fmt.Errorf("wire: foreign keys referencing %s: %w", t.Name, err)
	}
	defer rows.Close()
	var referenced []string
	for rows.Next() {
		var to sql.NullString
		if err := rows.Scan(&to); err != nil {
			return nil, fmt.Errorf("wire: foreign keys referencing %s: %w", t.Name, err)
		}
		if to.Valid {
			referenced = append(referenced, to.String)
		} else {
			referenced = append(referenced, t.PrimaryKey...)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("wire: foreign keys referencing %s: %w", t.Name, err)
	}
	return referenced, nil
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

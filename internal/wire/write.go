package wire

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/dialect"
	"mysqlite/internal/engine"
	"mysqlite/internal/errs"
	"mysqlite/internal/meta"
	"mysqlite/internal/vars"
)

// write runs INSERT, UPDATE or DELETE on the writer (or the session's transaction) and
// reports MySQL's affected rows, insert id and info string (docs/05-dialect.md
// section 6). Values are `oracle: pending`.
func (h *handler) write(st *dialect.Statement) (res *mysql.Result, err error) {
	ctx := context.Background()
	conn, release, err := h.sess.Conn(ctx, st.Database, true)
	if err != nil {
		return nil, h.fail(err, errs.Hint{DatabaseArg: st.Database})
	}
	defer release()
	defer func() {
		if err != nil {
			h.sess.StatementFailed(ctx)
		}
	}()
	if !h.sess.InTransaction() {
		// Autocommit: take the write lock first, retrying a busy database while nothing
		// has been written yet (docs/06-transactions.md section 5). Inside a transaction
		// a busy error goes to the client, which restarts the transaction.
		retries, berr := engine.BeginImmediate(ctx, conn)
		h.retries = retries
		if berr != nil {
			return nil, h.fail(fmt.Errorf("wire: begin write: %w", berr), errs.Hint{DatabaseArg: st.Database})
		}
		defer func() {
			if err != nil {
				_, _ = conn.ExecContext(ctx, "ROLLBACK")
				return
			}
			if _, cerr := conn.ExecContext(ctx, "COMMIT"); cerr != nil {
				_, _ = conn.ExecContext(ctx, "ROLLBACK")
				res, err = nil, h.fail(fmt.Errorf("wire: commit write: %w", cerr), st.Hint())
			}
		}()
	}

	w := st.Write
	table, err := h.catalog.Table(ctx, conn, st.Database, w.Table)
	if err != nil {
		return nil, h.fail(err, st.Hint())
	}
	hint := st.Hint()
	hint.KeyName = func(_ string, cols []string) string { return table.KeyName(cols) }

	switch st.Op {
	case errs.OpInsert:
		return h.insert(ctx, conn, st, table, hint)
	case errs.OpUpdate:
		return h.update(ctx, conn, st, table, hint)
	default:
		if e := meta.CheckRefs(w.Refs, nil, meta.Single(table, w.Alias)); e != nil {
			return nil, toMySQL(e)
		}
		if e := meta.CheckExprs(w.Exprs, meta.Single(table, w.Alias)); e != nil {
			return nil, toMySQL(e)
		}
		r, err := conn.ExecContext(ctx, st.SQL, st.Args...)
		if err != nil {
			return nil, h.fail(err, hint)
		}
		n, err := r.RowsAffected()
		if err != nil {
			return nil, h.fail(err, hint)
		}
		return &mysql.Result{AffectedRows: uint64(n)}, nil
	}
}

func (h *handler) insert(ctx context.Context, conn *sql.Conn, st *dialect.Statement, table *meta.Table, hint errs.Hint) (*mysql.Result, error) {
	w := st.Write
	if w.Replace {
		return h.replace(ctx, conn, st, table, hint)
	}
	if w.Select != nil {
		return h.insertSelect(ctx, conn, st, table, hint)
	}
	mode, _, _ := h.vars.Get("sql_mode", vars.ReadSession)
	info, e := meta.CheckInsertMode(table, w.Columns, w.Rows, insertMode(mode.Text))
	if e != nil {
		if w.Ignore && adjustedByIgnore(e) {
			return nil, toMySQL(ignoreAdjusts(e))
		}
		return nil, toMySQL(e)
	}
	if w.Ignore {
		return h.insertIgnore(ctx, conn, st, table, hint, info)
	}
	if w.Upsert {
		return h.insertUpsert(ctx, conn, st, table, hint, info)
	}
	hint.Entry = func(_ string, cols []string) string { return insertEntry(ctx, conn, table, info, cols) }

	res := &mysql.Result{}
	if info.Auto == "" {
		r, err := conn.ExecContext(ctx, st.SQL, st.Args...)
		if err != nil {
			return nil, h.fail(err, hint)
		}
		n, err := r.RowsAffected()
		if err != nil {
			return nil, h.fail(err, hint)
		}
		res.AffectedRows = uint64(n)
	} else {
		// RETURNING gives every inserted id; last_insert_rowid() only has the last one.
		ids, err := returning(ctx, conn, st.SQL+" RETURNING "+ident(info.Auto), st.Args)
		if err != nil {
			return nil, h.fail(err, hint)
		}
		res.AffectedRows = uint64(len(ids))
		id, generated := insertID(ids, info.Explicit)
		res.InsertId = uint64(id)
		if generated {
			// LAST_INSERT_ID() changes only when the statement generated an id.
			h.lastInsertID = id
		}
	}
	if len(w.Rows) > 1 {
		res.StatusMessage = fmt.Sprintf("Records: %d  Duplicates: 0  Warnings: 0", res.AffectedRows)
	}
	return res, nil
}

func (h *handler) update(ctx context.Context, conn *sql.Conn, st *dialect.Statement, table *meta.Table, hint errs.Hint) (*mysql.Result, error) {
	w := st.Write
	if e := meta.CheckRefs(w.Refs, nil, meta.Single(table, w.Alias)); e != nil {
		return nil, toMySQL(e)
	}
	if e := meta.CheckExprs(w.Exprs, meta.Single(table, w.Alias)); e != nil {
		return nil, toMySQL(e)
	}
	hint.Entry = func(_ string, cols []string) string { return updateEntry(w.Set, cols) }

	// Both statements run on the writer this session holds, so no other write can
	// come between them.
	var matched int64
	if err := conn.QueryRowContext(ctx, w.MatchSQL, w.MatchArgs...).Scan(&matched); err != nil {
		return nil, h.fail(err, hint)
	}
	r, err := conn.ExecContext(ctx, st.SQL, st.Args...)
	if err != nil {
		return nil, h.fail(err, hint)
	}
	changed, err := r.RowsAffected()
	if err != nil {
		return nil, h.fail(err, hint)
	}
	affected := changed
	if h.foundRows() {
		affected = matched
	}
	return &mysql.Result{
		AffectedRows:  uint64(affected),
		StatusMessage: fmt.Sprintf("Rows matched: %d  Changed: %d  Warnings: 0", matched, changed),
	}, nil
}

// foundRows reports CLIENT_FOUND_ROWS in the client's flags. go-mysql cannot advertise
// it, but returns the client's flags as sent (ADR 0003, G2).
func (h *handler) foundRows() bool {
	return h.conn != nil && h.conn.Capability()&mysql.CLIENT_FOUND_ROWS != 0
}

func returning(ctx context.Context, conn *sql.Conn, query string, args []any) ([]int64, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// insertID is the id MySQL puts in the OK packet: the first generated value of the
// statement; without one, the explicit value of the last row (sql_insert.cc).
// Generated values grow within a statement, so the first is the smallest of the ids
// that were not given explicitly. generated reports that the id was generated.
func insertID(ids, explicit []int64) (id int64, generated bool) {
	given := map[int64]int{}
	for _, v := range explicit {
		given[v]++
	}
	first, found := int64(0), false
	for _, id := range ids {
		if given[id] > 0 {
			given[id]--
			continue
		}
		if !found || id < first {
			first, found = id, true
		}
	}
	switch {
	case found:
		return first, true
	case len(explicit) > 0:
		return explicit[len(explicit)-1], false
	}
	return 0, false
}

// insertEntry finds the value MySQL reports in 1062: the key of the first row that
// conflicts with an existing row or with an earlier row of the statement. The failed
// statement left no rows, so the table is as before it. It gives up ("") when a key
// value is not a literal.
func insertEntry(ctx context.Context, conn *sql.Conn, table *meta.Table, info meta.InsertInfo, cols []string) string {
	var earlier [][]string
	for _, row := range info.Values {
		vals, args, ok := keyValues(row, cols)
		if !ok {
			return ""
		}
		if vals == nil {
			continue // a NULL or generated key part cannot conflict
		}
		for _, prev := range earlier {
			if equalStrings(prev, vals) {
				return entryText(vals)
			}
		}
		earlier = append(earlier, vals)

		var where []string
		for _, c := range cols {
			where = append(where, ident(c)+" = ?")
		}
		var one int
		err := conn.QueryRowContext(ctx, "SELECT 1 FROM "+ident(table.Name)+" WHERE "+strings.Join(where, " AND ")+" LIMIT 1", args...).Scan(&one)
		if err == nil {
			return entryText(vals)
		}
	}
	return ""
}

// keyValues returns the key values of one row; nil values when the row cannot
// conflict, ok false when a value is unknown.
func keyValues(row map[string]*meta.Literal, cols []string) (vals []string, args []any, ok bool) {
	for _, c := range cols {
		v, present := row[strings.ToLower(c)]
		if !present || (v != nil && v.Kind == meta.LiteralNull) {
			// Omitted: generated (AUTO_INCREMENT) or a default we do not know.
			if !present {
				return nil, nil, false
			}
			return nil, nil, true
		}
		if v == nil {
			return nil, nil, false
		}
		vals = append(vals, v.Text)
		args = append(args, literalArg(v))
	}
	return vals, args, true
}

func literalArg(v *meta.Literal) any {
	switch v.Kind {
	case meta.LiteralInt:
		if n, err := strconv.ParseInt(v.Text, 10, 64); err == nil {
			return n
		}
	case meta.LiteralDecimal, meta.LiteralFloat:
		if f, err := strconv.ParseFloat(v.Text, 64); err == nil {
			return f
		}
	}
	return v.Text
}

// updateEntry is the key an UPDATE wrote when every key column is assigned a literal.
func updateEntry(set map[string]*meta.Literal, cols []string) string {
	var vals []string
	for _, c := range cols {
		v := set[strings.ToLower(c)]
		if v == nil || v.Kind == meta.LiteralNull {
			return ""
		}
		vals = append(vals, v.Text)
	}
	return entryText(vals)
}

// entryText joins key parts with '-' and keeps MySQL's 192-byte limit ('%-.192s').
func entryText(vals []string) string {
	s := strings.Join(vals, "-")
	const limit = 192
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ident(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// insertSelect runs INSERT ... SELECT: the SELECT is checked as a read, its width
// against the column list (1136), then the list as one row of unknown values (1054,
// 1110, 1364). Naming the AUTO_INCREMENT column is refused: SQLite does not say which
// ids it generated, and MySQL's insert id depends on it. MySQL always reports the
// Records info string here.
func (h *handler) insertSelect(ctx context.Context, conn *sql.Conn, st *dialect.Statement, table *meta.Table, hint errs.Hint) (*mysql.Result, error) {
	w := st.Write
	sel := w.Select.Select
	var defs []meta.ColumnDef
	var e *errs.Error
	if len(w.Select.Union) > 0 {
		defs, e = meta.CheckUnion(&sel, w.Select.Union)
	} else {
		defs, e = meta.CheckSelect(&sel)
	}
	if e != nil {
		return nil, toMySQL(e)
	}
	width := len(w.Columns)
	if width == 0 {
		width = len(table.Columns)
	}
	if len(defs) != width {
		return nil, toMySQL(errs.ColumnCount(1))
	}
	row := make([]*meta.Literal, width)
	if table.AutoIncrement != "" {
		named := len(w.Columns) == 0
		for _, c := range w.Columns {
			named = named || strings.EqualFold(c, table.AutoIncrement)
		}
		if named {
			// Columns are checked first, as MySQL does.
			if _, e := meta.CheckInsertMode(table, w.Columns, nil, meta.InsertMode{}); e != nil {
				return nil, toMySQL(e)
			}
			return nil, toMySQL(errs.Unsupported("INSERT ... SELECT into the AUTO_INCREMENT column",
				"mysqlite cannot tell generated ids from given ones for the insert id; list the other columns"))
		}
	}
	if _, e := meta.CheckInsertMode(table, w.Columns, [][]*meta.Literal{row}, meta.InsertMode{}); e != nil {
		return nil, toMySQL(e)
	}

	res := &mysql.Result{}
	if table.AutoIncrement == "" {
		r, err := conn.ExecContext(ctx, st.SQL, st.Args...)
		if err != nil {
			return nil, h.fail(err, hint)
		}
		n, err := r.RowsAffected()
		if err != nil {
			return nil, h.fail(err, hint)
		}
		res.AffectedRows = uint64(n)
	} else {
		ids, err := returning(ctx, conn, st.SQL+" RETURNING "+ident(table.AutoIncrement), st.Args)
		if err != nil {
			return nil, h.fail(err, hint)
		}
		res.AffectedRows = uint64(len(ids))
		if id, generated := insertID(ids, nil); generated {
			res.InsertId = uint64(id)
			h.lastInsertID = id
		}
	}
	res.StatusMessage = fmt.Sprintf("Records: %d  Duplicates: 0  Warnings: 0", res.AffectedRows)
	return res, nil
}

// adjustedByIgnore reports an error MySQL's IGNORE turns into a warning while it
// stores an adjusted value (NULL as the implicit default, a clipped number, a cut
// string). Unknown columns, repeated columns and row widths stay errors.
func adjustedByIgnore(e *errs.Error) bool {
	switch e.Code {
	case 1054, 1110, 1136, 1235:
		return false
	}
	return true
}

// ignoreAdjusts refuses an INSERT IGNORE whose row MySQL would store adjusted: mysqlite
// does not compute the adjusted value.
func ignoreAdjusts(e *errs.Error) *errs.Error {
	return errs.Unsupported("INSERT IGNORE of a value MySQL adjusts with a warning", "without IGNORE the row fails with: "+e.Message)
}

// insertIgnore runs INSERT IGNORE one row at a time: a row that hits a unique key is
// rolled back and reported as warning 1062, the others are inserted. Any other failure
// undoes the whole statement.
func (h *handler) insertIgnore(ctx context.Context, conn *sql.Conn, st *dialect.Statement, table *meta.Table, hint errs.Hint, info meta.InsertInfo) (res *mysql.Result, err error) {
	w := st.Write
	if _, err := conn.ExecContext(ctx, "SAVEPOINT mysqlite_stmt"); err != nil {
		return nil, h.fail(err, hint)
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(ctx, "ROLLBACK TO mysqlite_stmt")
		}
		_, _ = conn.ExecContext(ctx, "RELEASE mysqlite_stmt")
	}()

	var ids, explicit []int64
	duplicates := 0
	for i, q := range w.RowSQL {
		seq, err := rowSequence(ctx, conn, table, info, i)
		if err != nil {
			return nil, h.fail(err, hint)
		}
		if _, err := conn.ExecContext(ctx, "SAVEPOINT mysqlite_row"); err != nil {
			return nil, h.fail(err, hint)
		}
		row := meta.InsertInfo{Auto: info.Auto, Values: info.Values[i : i+1]}
		rowHint := hint
		rowHint.Entry = func(_ string, cols []string) string { return insertEntry(ctx, conn, table, row, cols) }
		var rowIDs []int64
		var execErr error
		if info.Auto == "" {
			_, execErr = conn.ExecContext(ctx, q, w.RowArgs[i]...)
		} else {
			rowIDs, execErr = returning(ctx, conn, q+" RETURNING "+ident(info.Auto), w.RowArgs[i])
		}
		if execErr != nil {
			m := errs.Map(execErr, rowHint)
			_, _ = conn.ExecContext(ctx, "ROLLBACK TO mysqlite_row")
			_, _ = conn.ExecContext(ctx, "RELEASE mysqlite_row")
			if m.Code != 1062 {
				if adjustedByIgnore(m) {
					return nil, toMySQL(ignoreAdjusts(m))
				}
				return nil, h.fail(execErr, rowHint)
			}
			h.warn(m)
			duplicates++
			if err := loseID(ctx, conn, table, seq); err != nil {
				return nil, h.fail(err, hint)
			}
			continue
		}
		if _, err := conn.ExecContext(ctx, "RELEASE mysqlite_row"); err != nil {
			return nil, h.fail(err, hint)
		}
		ids = append(ids, rowIDs...)
		if v, ok := info.Values[i][strings.ToLower(info.Auto)]; ok && v != nil && v.Kind != meta.LiteralNull && len(rowIDs) == 1 {
			explicit = append(explicit, rowIDs[0])
		}
	}

	res = &mysql.Result{AffectedRows: uint64(len(w.RowSQL) - duplicates)}
	if info.Auto != "" {
		res.AffectedRows = uint64(len(ids))
		id, generated := insertID(ids, explicit)
		res.InsertId = uint64(id)
		if generated {
			h.lastInsertID = id
		}
	}
	if len(w.RowSQL) > 1 {
		res.StatusMessage = fmt.Sprintf("Records: %d  Duplicates: %d  Warnings: %d", len(w.RowSQL), duplicates, duplicates)
	}
	return res, nil
}

// replace runs REPLACE. SQLite's changes() leaves out the rows REPLACE deletes, which
// MySQL counts as affected, so they come from the row count before and after.
func (h *handler) replace(ctx context.Context, conn *sql.Conn, st *dialect.Statement, table *meta.Table, hint errs.Hint) (*mysql.Result, error) {
	w := st.Write
	count := func() (int64, error) {
		var n int64
		err := conn.QueryRowContext(ctx, "SELECT count(*) FROM "+ident(table.Name)).Scan(&n)
		return n, err
	}
	before, err := count()
	if err != nil {
		return nil, h.fail(err, hint)
	}
	plain := *st
	wr := *w
	wr.Replace = false
	plain.Write = &wr
	var res *mysql.Result
	if w.Select != nil {
		res, err = h.insertSelect(ctx, conn, &plain, table, hint)
	} else {
		res, err = h.insert(ctx, conn, &plain, table, hint)
	}
	if err != nil {
		return nil, err
	}
	after, err := count()
	if err != nil {
		return nil, h.fail(err, hint)
	}
	deleted := uint64(before + int64(res.AffectedRows) - after)
	records := res.AffectedRows
	res.AffectedRows += deleted
	if res.StatusMessage != "" {
		res.StatusMessage = fmt.Sprintf("Records: %d  Duplicates: %d  Warnings: 0", records, deleted)
	}
	return res, nil
}

// insertUpsert runs INSERT ... ON DUPLICATE KEY UPDATE one row at a time: the row is
// inserted (1 affected row) or, when it conflicts, the existing row is updated (2) or
// left as it is because the values do not change (0, or 1 with CLIENT_FOUND_ROWS). The
// insert id is the first generated id; without one, the id of the last updated row.
// A row conflicting with several rows updates the one MySQL finds first (uniqueKeys).
// Checked against MySQL 8.0.46. Any failure undoes the whole statement.
func (h *handler) insertUpsert(ctx context.Context, conn *sql.Conn, st *dialect.Statement, table *meta.Table, hint errs.Hint, info meta.InsertInfo) (res *mysql.Result, err error) {
	w := st.Write
	from := meta.Single(table, "")
	if e := meta.CheckRefs(w.Refs, nil, from); e != nil {
		return nil, toMySQL(e)
	}
	if e := meta.CheckExprs(w.Exprs, from); e != nil {
		return nil, toMySQL(e)
	}
	if _, err := conn.ExecContext(ctx, "SAVEPOINT mysqlite_stmt"); err != nil {
		return nil, h.fail(err, hint)
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(ctx, "ROLLBACK TO mysqlite_stmt")
		}
		_, _ = conn.ExecContext(ctx, "RELEASE mysqlite_stmt")
	}()

	// run executes q and reports whether it wrote a row, with the row's id.
	run := func(q string, args []any, rowHint errs.Hint) (bool, []int64, error) {
		if info.Auto == "" {
			r, err := conn.ExecContext(ctx, q, args...)
			if err != nil {
				return false, nil, h.fail(err, rowHint)
			}
			n, err := r.RowsAffected()
			if err != nil {
				return false, nil, h.fail(err, rowHint)
			}
			return n > 0, nil, nil
		}
		ids, err := returning(ctx, conn, q+" RETURNING "+ident(info.Auto), args)
		if err != nil {
			return false, nil, h.fail(err, rowHint)
		}
		return len(ids) > 0, ids, nil
	}

	var ids, explicit []int64
	var updatedID int64
	var affected uint64
	duplicates := 0
	lost := int64(-1) // the AUTO_INCREMENT counter before an id given to an updating row
	for i := range w.RowSQL {
		row := meta.InsertInfo{Auto: info.Auto, Values: info.Values[i : i+1]}
		rowHint := hint
		rowHint.Entry = func(_ string, cols []string) string { return insertEntry(ctx, conn, table, row, cols) }
		seq, err := rowSequence(ctx, conn, table, info, i)
		if err != nil {
			return nil, h.fail(err, hint)
		}
		inserted, rowIDs, err := run(w.RowSQL[i], w.RowArgs[i], rowHint)
		if err != nil {
			return nil, err
		}
		if inserted {
			affected++
			ids = append(ids, rowIDs...)
			if v, ok := info.Values[i][strings.ToLower(info.Auto)]; ok && v != nil && v.Kind != meta.LiteralNull && len(rowIDs) == 1 {
				explicit = append(explicit, rowIDs[0])
			}
			if seq >= 0 {
				lost = -1
			}
			continue
		}
		// The update's own conflicts name the key it hits with the assigned values.
		updateHint := hint
		updateHint.Entry = func(_ string, cols []string) string { return updateEntry(w.Set, cols) }
		updateSQL := w.UpdateSQL[i]
		if keys := uniqueKeys(table); len(keys) > 1 {
			key, err := conflictKey(ctx, conn, w.UpsertBase[i], w.RowArgs[i], keys)
			if err != nil {
				return nil, h.fail(err, rowHint)
			}
			updateSQL = w.UpsertBase[i] + " ON CONFLICT (" + identList(key) + ")" +
				strings.TrimPrefix(updateSQL, w.UpsertBase[i]+" ON CONFLICT")
		}
		updated, rowIDs, err := run(updateSQL, w.UpdateArgs[i], updateHint)
		if err != nil {
			return nil, err
		}
		// The id this row was given goes to the next row of the statement that
		// generates one; it is lost only when no such row follows (MySQL 8.0.46,
		// oracle probe o.80.2).
		if seq >= 0 {
			// SQLite used up a value on each attempt: give them back.
			if err := setSequence(ctx, conn, table, seq); err != nil {
				return nil, h.fail(err, hint)
			}
			lost = seq
		}
		switch {
		case updated:
			affected += 2
			duplicates++
			if len(rowIDs) == 1 {
				updatedID = rowIDs[0]
			}
		case h.foundRows():
			affected++
			duplicates++
		}
	}

	if err := loseID(ctx, conn, table, lost); err != nil {
		return nil, h.fail(err, hint)
	}
	res = &mysql.Result{AffectedRows: affected}
	if id, generated := insertID(ids, explicit); generated {
		res.InsertId = uint64(id)
		h.lastInsertID = id
	} else if len(explicit) > 0 {
		res.InsertId = uint64(id)
	} else if updatedID != 0 {
		res.InsertId = uint64(updatedID)
	}
	if len(w.RowSQL) > 1 {
		res.StatusMessage = fmt.Sprintf("Records: %d  Duplicates: %d  Warnings: 0", len(w.RowSQL), duplicates)
	}
	return res, nil
}

// rowSequence is the AUTO_INCREMENT counter before row i of an INSERT run one row at a
// time, or -1 when the row does not generate an id (no AUTO_INCREMENT column, or the
// row gives the value).
func rowSequence(ctx context.Context, conn *sql.Conn, table *meta.Table, info meta.InsertInfo, i int) (int64, error) {
	if info.Auto == "" {
		return -1, nil
	}
	if v, ok := info.Values[i][strings.ToLower(info.Auto)]; ok && (v == nil || v.Kind != meta.LiteralNull) {
		return -1, nil
	}
	var seq int64
	err := conn.QueryRowContext(ctx, "SELECT seq FROM sqlite_sequence WHERE name = ?", table.Name).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("wire: AUTO_INCREMENT of %s: %w", table.Name, err)
	}
	return seq, nil
}

// loseID leaves the AUTO_INCREMENT counter one past seq after a row that generated an
// id but was not inserted (INSERT IGNORE skipped it, ON DUPLICATE KEY UPDATE updated
// another row). InnoDB allocates one id per such row of a simple INSERT and loses it
// (innodb_autoinc_lock_mode 2, MySQL 8.0's default); SQLite either gives it back on
// rollback or uses one per upsert attempt. Checked for ON DUPLICATE KEY UPDATE against
// MySQL 8.0.46; INSERT IGNORE with several rows is not checked yet.
func loseID(ctx context.Context, conn *sql.Conn, table *meta.Table, seq int64) error {
	if seq < 0 {
		return nil
	}
	return writeSequence(ctx, conn, table, "max(seq, ?)", seq+1)
}

// setSequence sets the AUTO_INCREMENT counter of table to seq.
func setSequence(ctx context.Context, conn *sql.Conn, table *meta.Table, seq int64) error {
	return writeSequence(ctx, conn, table, "?", seq)
}

// writeSequence sets sqlite_sequence.seq of table to expr, where ? is v, creating the
// row with v when the table has none.
func writeSequence(ctx context.Context, conn *sql.Conn, table *meta.Table, expr string, v int64) error {
	r, err := conn.ExecContext(ctx, "UPDATE sqlite_sequence SET seq = "+expr+" WHERE name = ?", v, table.Name)
	if err == nil {
		var n int64
		if n, err = r.RowsAffected(); err == nil && n == 0 {
			_, err = conn.ExecContext(ctx, "INSERT INTO sqlite_sequence (name, seq) VALUES (?, ?)", table.Name, v)
		}
	}
	if err != nil {
		return fmt.Errorf("wire: AUTO_INCREMENT of %s: %w", table.Name, err)
	}
	return nil
}

// uniqueKeys lists the table's unique keys in the order MySQL looks for the row an ON
// DUPLICATE KEY UPDATE row conflicts with: PRIMARY, then unique indexes in creation order.
func uniqueKeys(table *meta.Table) [][]string {
	var keys [][]string
	if len(table.PrimaryKey) > 0 {
		keys = append(keys, table.PrimaryKey)
	}
	for _, ix := range table.Indexes {
		if ix.Unique {
			keys = append(keys, ix.Columns)
		}
	}
	return keys
}

// conflictKey returns the first of keys the row of insert conflicts with. The row is
// known to conflict with some key; inserting it with ON CONFLICT (key) DO NOTHING
// inserts nothing when that key conflicts and fails on another key's conflict.
func conflictKey(ctx context.Context, conn *sql.Conn, insert string, args []any, keys [][]string) ([]string, error) {
	for _, key := range keys {
		r, err := conn.ExecContext(ctx, insert+" ON CONFLICT ("+identList(key)+") DO NOTHING", args...)
		if err != nil {
			if engine.IsUniqueConflict(err) {
				continue
			}
			return nil, err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n != 0 {
			return nil, fmt.Errorf("wire: ON DUPLICATE KEY UPDATE row inserted while looking for its conflict")
		}
		return key, nil
	}
	return nil, fmt.Errorf("wire: ON DUPLICATE KEY UPDATE row conflicts with no unique key")
}

// identList quotes and joins column names.
func identList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = ident(c)
	}
	return strings.Join(q, ", ")
}

// insertMode reads the sql_mode flags meta.CheckInsertMode needs.
func insertMode(mode string) meta.InsertMode {
	return meta.InsertMode{
		NoAutoValueOnZero: vars.HasMode(mode, "NO_AUTO_VALUE_ON_ZERO"),
		Strict:            vars.HasMode(mode, "STRICT_TRANS_TABLES") || vars.HasMode(mode, "STRICT_ALL_TABLES"),
		NoZeroDate:        vars.HasMode(mode, "NO_ZERO_DATE"),
		NoZeroInDate:      vars.HasMode(mode, "NO_ZERO_IN_DATE"),
		AllowInvalidDates: vars.HasMode(mode, "ALLOW_INVALID_DATES"),
	}
}

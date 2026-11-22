package wire

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/catalog"
	"mysqlite/internal/dialect"
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

	w := st.Write
	table, err := catalog.ReadTable(ctx, conn, st.Database, w.Table)
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
		if e := meta.CheckRefs(w.Refs, nil, table, w.Alias); e != nil {
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
	mode, _, _ := h.vars.Get("sql_mode", vars.ReadSession)
	info, e := meta.CheckInsertMode(table, w.Columns, w.Rows, meta.InsertMode{
		NoAutoValueOnZero: vars.HasMode(mode.Text, "NO_AUTO_VALUE_ON_ZERO"),
	})
	if e != nil {
		return nil, toMySQL(e)
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
		res.InsertId = uint64(insertID(ids, info.Explicit))
	}
	if len(w.Rows) > 1 {
		res.StatusMessage = fmt.Sprintf("Records: %d  Duplicates: 0  Warnings: 0", res.AffectedRows)
	}
	return res, nil
}

func (h *handler) update(ctx context.Context, conn *sql.Conn, st *dialect.Statement, table *meta.Table, hint errs.Hint) (*mysql.Result, error) {
	w := st.Write
	if e := meta.CheckRefs(w.Refs, nil, table, w.Alias); e != nil {
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
// that were not given explicitly.
func insertID(ids, explicit []int64) int64 {
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
		return first
	case len(explicit) > 0:
		return explicit[len(explicit)-1]
	}
	return 0
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

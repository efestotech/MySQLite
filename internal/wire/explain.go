package wire

import (
	"context"
	"database/sql"
	"regexp"
	"slices"
	"strings"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/dialect"
	"mysqlite/internal/meta"
)

// EXPLAIN shows SQLite's query plan (EXPLAIN QUERY PLAN) in MySQL's traditional EXPLAIN
// columns. The plan is SQLite's: one row per table access, its access type and key read
// from the plan line; rows, filtered, key_len and ref are unknown and NULL.
// `oracle: pending` for the column definitions.

func explainText(name string, chars uint32) meta.ColumnDef {
	return meta.ColumnDef{Name: name, Charset: meta.CollationUTF8MB3, Type: meta.TypeVarString, Length: chars * 3}
}

var explainDefs = []meta.ColumnDef{
	{Name: "id", Charset: meta.CharsetBinary, Type: meta.TypeLongLong, Flags: meta.FlagBinary | meta.FlagNum, Length: 3},
	func() meta.ColumnDef {
		d := explainText("select_type", 19)
		d.Flags |= meta.FlagNotNull
		return d
	}(),
	explainText("table", 64),
	explainText("partitions", 8192),
	explainText("type", 10),
	explainText("possible_keys", 4096),
	explainText("key", 64),
	explainText("key_len", 4096),
	explainText("ref", 1024),
	{Name: "rows", Charset: meta.CharsetBinary, Type: meta.TypeLongLong, Flags: meta.FlagBinary | meta.FlagNum, Length: 10},
	{Name: "filtered", Charset: meta.CharsetBinary, Type: meta.TypeDouble, Flags: meta.FlagBinary | meta.FlagNum, Length: 4, Decimals: 2},
	explainText("Extra", 255),
}

// planLine is one row of EXPLAIN QUERY PLAN.
type planLine struct {
	id, parent int
	detail     string
}

func (h *handler) explain(ctx context.Context, conn *sql.Conn, st *dialect.Statement, sel *meta.Select) (*mysql.Result, error) {
	rows, err := conn.QueryContext(ctx, "EXPLAIN QUERY PLAN "+st.SQL, st.Args...)
	if err != nil {
		return nil, h.fail(err, st.Hint())
	}
	defer rows.Close()
	var plan []planLine
	for rows.Next() {
		var l planLine
		var unused int
		if err := rows.Scan(&l.id, &l.parent, &unused, &l.detail); err != nil {
			return nil, h.fail(err, st.Hint())
		}
		plan = append(plan, l)
	}
	if err := rows.Err(); err != nil {
		return nil, h.fail(err, st.Hint())
	}
	tables := statementTables(sel, st.Shape.Union)
	return staticResult(explainDefs, explainRows(plan, tables))
}

// statementTables lists the catalog tables a statement reads, at any depth.
func statementTables(sel *meta.Select, union []*meta.Select) []*meta.Table {
	var out []*meta.Table
	var walkSel func(*meta.Select)
	var walkExpr func(*meta.Expr)
	walkSel = func(s *meta.Select) {
		if s == nil {
			return
		}
		for _, src := range s.From.Sources {
			if src.Derived != nil {
				walkSel(&src.Derived.Select)
				for _, u := range src.Derived.Union {
					walkSel(u)
				}
			} else if src.Table != nil {
				out = append(out, src.Table)
			}
		}
		for _, x := range s.Exprs {
			walkExpr(x)
		}
	}
	walkExpr = func(x *meta.Expr) {
		if x == nil {
			return
		}
		walkSel(x.Sub)
		for _, a := range x.Args {
			walkExpr(a)
		}
	}
	walkSel(sel)
	for _, u := range union {
		walkSel(u)
	}
	return out
}

// keyInfo describes the key a plan line reads.
type keyInfo struct {
	name   string
	unique bool
	cols   int
}

// lookupKey maps a SQLite index to its MySQL key: the catalog's index name, PRIMARY for
// a primary key's automatic index, else the SQLite name.
func lookupKey(sqliteName string, tables []*meta.Table) keyInfo {
	for _, t := range tables {
		for _, ix := range t.Indexes {
			if strings.EqualFold(ix.SQLiteName, sqliteName) {
				return keyInfo{name: ix.Name, unique: ix.Unique, cols: len(ix.Columns)}
			}
		}
	}
	if rest, ok := strings.CutPrefix(sqliteName, "sqlite_autoindex_"); ok {
		if i := strings.LastIndexByte(rest, '_'); i > 0 {
			for _, t := range tables {
				if strings.EqualFold(t.Name, rest[:i]) && len(t.PrimaryKey) > 0 {
					return keyInfo{name: "PRIMARY", unique: true, cols: len(t.PrimaryKey)}
				}
			}
		}
	}
	return keyInfo{name: sqliteName}
}

var (
	accessLine = regexp.MustCompile(`^(SCAN|SEARCH) (\S+)(.*)$`)
	indexUse   = regexp.MustCompile(`USING (AUTOMATIC )?(PARTIAL )?(COVERING )?INDEX(?: ([^\s(]+))?`)
	equality   = regexp.MustCompile(`=\?`)
)

// blockKind is the MySQL select_type of a plan node that starts a query block, or "".
func blockKind(detail string) string {
	switch {
	case strings.HasPrefix(detail, "CORRELATED "):
		return "DEPENDENT SUBQUERY"
	case strings.HasPrefix(detail, "LIST SUBQUERY"), strings.HasPrefix(detail, "SCALAR SUBQUERY"):
		return "SUBQUERY"
	case strings.HasPrefix(detail, "CO-ROUTINE"), strings.HasPrefix(detail, "MATERIALIZE"):
		return "DERIVED"
	case detail == "LEFT", detail == "RIGHT", strings.HasPrefix(detail, "LEFT-MOST SUBQUERY"),
		strings.HasPrefix(detail, "UNION "), strings.HasPrefix(detail, "EXCEPT "), strings.HasPrefix(detail, "INTERSECT "):
		return "UNION"
	}
	return ""
}

// tempExtra is the MySQL Extra of a SQLite temporary b-tree line, or "".
func tempExtra(detail string) string {
	switch {
	case !strings.HasPrefix(detail, "USE TEMP B-TREE FOR "):
		return ""
	case strings.Contains(detail, "ORDER BY"):
		return "Using filesort"
	}
	return "Using temporary"
}

type explainRow struct {
	block int
	vals  []any
	extra []string
}

// explainRows maps a plan to EXPLAIN rows. Query blocks are the plan's subquery,
// derived-table and set-operation nodes; the statement's own block comes first.
func explainRows(plan []planLine, tables []*meta.Table) [][]any {
	byID := map[int]planLine{}
	for _, l := range plan {
		byID[l.id] = l
	}
	// blockOf is the plan id of the node that starts l's query block, 0 for the top.
	blockOf := func(l planLine) (int, string) {
		for p := l.parent; p != 0; {
			n, ok := byID[p]
			if !ok {
				break
			}
			if k := blockKind(n.detail); k != "" {
				return p, k
			}
			p = n.parent
		}
		return 0, ""
	}
	var rows []*explainRow
	kinds := map[int]string{}
	var order []int
	extras := map[int][]string{}
	for _, l := range plan {
		block, kind := blockOf(l)
		if x := tempExtra(l.detail); x != "" {
			extras[block] = append(extras[block], x)
			continue
		}
		m := accessLine.FindStringSubmatch(l.detail)
		if m == nil || l.detail == "SCAN CONSTANT ROW" {
			continue
		}
		if _, seen := kinds[block]; !seen {
			kinds[block] = kind
			order = append(order, block)
		}
		r := &explainRow{block: block, vals: make([]any, len(explainDefs))}
		r.vals[2] = m[2]
		access, rest := m[1], m[3]
		var key keyInfo
		covering := false
		if strings.Contains(rest, "USING INTEGER PRIMARY KEY") {
			key = keyInfo{name: "PRIMARY", unique: true, cols: 1}
		} else if u := indexUse.FindStringSubmatch(rest); u != nil {
			covering = u[3] != ""
			switch {
			case u[1] != "":
				key = keyInfo{name: "<auto_key0>"}
			default:
				key = lookupKey(u[4], tables)
			}
		}
		switch {
		case access == "SCAN" && key.name == "":
			r.vals[4] = "ALL"
		case access == "SCAN":
			r.vals[4] = "index"
		case strings.ContainsAny(rest, "<>"):
			r.vals[4] = "range"
		case key.unique && len(equality.FindAllString(rest, -1)) >= key.cols:
			r.vals[4] = "eq_ref"
		default:
			r.vals[4] = "ref"
		}
		if key.name != "" {
			r.vals[5], r.vals[6] = key.name, key.name
		}
		if covering {
			r.extra = append(r.extra, "Using index")
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		vals := make([]any, len(explainDefs))
		vals[0], vals[1], vals[11] = int64(1), "SIMPLE", "No tables used"
		return [][]any{vals}
	}
	// The statement's own block is id 1 when it reads a table.
	if i := slices.Index(order, 0); i > 0 {
		order = append([]int{0}, slices.Delete(order, i, i+1)...)
	}
	ids := map[int]int{}
	for i, b := range order {
		ids[b] = i + 1
	}
	slices.SortStableFunc(rows, func(a, b *explainRow) int { return ids[a.block] - ids[b.block] })
	first := map[int]bool{}
	out := make([][]any, len(rows))
	for i, r := range rows {
		id := ids[r.block]
		r.vals[0] = int64(id)
		switch {
		case len(order) == 1:
			r.vals[1] = "SIMPLE"
		case id == 1:
			r.vals[1] = "PRIMARY"
		default:
			r.vals[1] = kinds[r.block]
		}
		if !first[r.block] {
			// MySQL reports a block's temporary table and sort on its first table.
			first[r.block] = true
			r.extra = append(r.extra, extras[r.block]...)
		}
		var extra []string
		for _, x := range r.extra {
			if !slices.Contains(extra, x) {
				extra = append(extra, x)
			}
		}
		if len(extra) > 0 {
			r.vals[11] = strings.Join(extra, "; ")
		}
		out[i] = r.vals
	}
	return out
}

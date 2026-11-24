package wire

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/catalog"
	"mysqlite/internal/collate"
	"mysqlite/internal/dialect"
	"mysqlite/internal/errs"
	"mysqlite/internal/infoschema"
	"mysqlite/internal/meta"
)

// Catalog SHOW statements (docs/04-session-catalog.md section 4). mysqlite builds the
// rows from the catalog, then runs LIKE and WHERE over them as a real SQL predicate on
// the scratch connection, so WHERE behaves like a WHERE. Column lengths and flags are
// `oracle: pending`.

// show dispatches every SHOW statement.
func (h *handler) show(s *dialect.Show) (*mysql.Result, error) {
	ctx := context.Background()
	var defs []meta.ColumnDef
	var rows [][]any
	var err error
	switch s.Type {
	case dialect.ShowWarnings, dialect.ShowErrors:
		return h.showDiagnostics(s)
	case dialect.ShowDatabases:
		defs, rows, err = h.showDatabases(s)
	case dialect.ShowCreateDatabase:
		defs, rows, err = h.showCreateDatabase(ctx, s)
	case dialect.ShowVariables, dialect.ShowStatus, dialect.ShowEngines, dialect.ShowCharset,
		dialect.ShowCollation, dialect.ShowProcessList, dialect.ShowGrants:
		defs, rows, err = h.showServer(s)
	default:
		defs, rows, err = h.showCatalog(ctx, s)
	}
	if err != nil {
		return nil, err
	}
	rows, err = h.filterRows(ctx, s, defs, rows)
	if err != nil {
		return nil, err
	}
	return staticResult(defs, rows)
}

func textCol(name string, chars int) meta.ColumnDef {
	return meta.ColumnDef{Name: name, Charset: meta.CollationUTF8MB3, Type: meta.TypeVarString, Length: uint32(chars * 3)}
}

// enumCol is an ENUM column of the data dictionary: MySQL sends it as STRING with the
// ENUM flag (Key, Table_type, Row_format).
func enumCol(name string, chars int) meta.ColumnDef {
	return meta.ColumnDef{Name: name, Charset: meta.CollationUTF8MB3, Type: meta.TypeString, Flags: meta.FlagEnum, Length: uint32(chars * 3)}
}

// blobCol is a TEXT column of the data dictionary (Type, Default, Comment): MySQL sends
// it as BLOB in a text character set.
func blobCol(name string) meta.ColumnDef {
	return meta.ColumnDef{Name: name, Charset: meta.CollationUTF8MB3, Type: meta.TypeBlob, Flags: meta.FlagBlob, Length: 196605}
}

// timeCol is a TIMESTAMP or DATETIME column (the table status times).
func timeCol(name string, typ uint8) meta.ColumnDef {
	return meta.ColumnDef{Name: name, Charset: meta.CharsetBinary, Type: typ, Flags: meta.FlagBinary, Length: 19}
}

// longCol is an unsigned INT column (SHOW CHARACTER SET Maxlen).
func longCol(name string) meta.ColumnDef {
	return meta.ColumnDef{
		Name: name, Charset: meta.CharsetBinary, Type: meta.TypeLong,
		Flags: meta.FlagBinary | meta.FlagNum | meta.FlagUnsigned, Length: 10,
	}
}

func numCol(name string) meta.ColumnDef {
	return meta.ColumnDef{
		Name: name, Charset: meta.CharsetBinary, Type: meta.TypeLongLong,
		Flags: meta.FlagBinary | meta.FlagNum | meta.FlagUnsigned, Length: 21,
	}
}

func (h *handler) showDatabases(s *dialect.Show) ([]meta.ColumnDef, [][]any, error) {
	names, err := h.eng.Databases()
	if err != nil {
		return nil, nil, h.fail(err, errs.Hint{})
	}
	names = append(names, "information_schema")
	sort.Strings(names)
	rows := make([][]any, len(names))
	for i, n := range names {
		rows[i] = []any{n}
	}
	return []meta.ColumnDef{textCol(dialect.LikeColumn("Database", s.Like), 64)}, rows, nil
}

// showCatalog runs the SHOW statements that read one database. It reads through the
// open transaction when it uses that database, so it sees the transaction's tables.
func (h *handler) showCatalog(ctx context.Context, s *dialect.Show) ([]meta.ColumnDef, [][]any, error) {
	if s.Type == dialect.ShowTriggers || s.Type == dialect.ShowEvents {
		return h.showNoObjects(ctx, s)
	}
	if infoschema.Is(s.Database) {
		return showInfoSchema(s)
	}
	conn, release, err := h.sess.CatalogConn(ctx, s.Database)
	if err != nil {
		return nil, nil, h.fail(err, errs.Hint{DatabaseArg: s.Database})
	}
	defer release()
	if s.Type == dialect.ShowTables || s.Type == dialect.ShowTableStatus {
		tables, err := catalog.ListTables(ctx, conn)
		if err != nil {
			return nil, nil, h.fail(err, errs.Hint{})
		}
		if s.Type == dialect.ShowTables {
			return showTables(s, tables)
		}
		return h.showTableStatus(ctx, conn, s, tables)
	}
	t, err := h.catalog.Table(ctx, conn, s.Database, s.Table)
	if err != nil {
		return nil, nil, h.fail(err, errs.Hint{Database: s.Database})
	}
	if t.Options == nil {
		h.log.Warn("table without mysqlite metadata: SHOW output is reconstructed", "database", s.Database, "table", t.Name)
	}
	switch s.Type {
	case dialect.ShowColumns:
		defs, rows := showColumns(s, t)
		return defs, rows, nil
	case dialect.ShowIndex:
		defs, rows := showIndex(t)
		return defs, rows, nil
	}
	next, err := catalog.NextAutoIncrement(ctx, conn, t)
	if err != nil {
		return nil, nil, h.fail(err, errs.Hint{})
	}
	defs := []meta.ColumnDef{textCol("Table", 64), textCol("Create Table", 1024)}
	return defs, [][]any{{t.Name, catalog.CreateTableSQL(t, next)}}, nil
}

// showInfoSchema lists information_schema's tables and describes their columns. Its
// other SHOW forms are refused: its tables have no storage to report.
// showCreateDatabase answers SHOW CREATE DATABASE. Every database uses the server
// default charset and collation; mysqlite stores no per-database options.
func (h *handler) showCreateDatabase(ctx context.Context, s *dialect.Show) ([]meta.ColumnDef, [][]any, error) {
	name := s.Database
	charsetName, collation := "utf8mb4", "utf8mb4_general_ci"
	if infoschema.Is(name) {
		name, charsetName, collation = infoschema.Name, "utf8mb3", "utf8mb3_general_ci"
	} else {
		_, release, err := h.sess.CatalogConn(ctx, name)
		if err != nil {
			return nil, nil, h.fail(err, errs.Hint{DatabaseArg: name})
		}
		release()
	}
	ifNotExists := ""
	if s.IfNotExists {
		ifNotExists = "/*!32312 IF NOT EXISTS*/ "
	}
	ddl := fmt.Sprintf("CREATE DATABASE %s%s /*!40100 DEFAULT CHARACTER SET %s COLLATE %s */ /*!80016 DEFAULT ENCRYPTION='N' */",
		ifNotExists, ident(name), charsetName, collation)
	return []meta.ColumnDef{textCol("Database", 64), textCol("Create Database", 1024)}, [][]any{{name, ddl}}, nil
}

// showNoObjects answers SHOW TRIGGERS and SHOW EVENTS: mysqlite has neither, so the
// result is empty, with MySQL's columns. The database must still exist (1049).
func (h *handler) showNoObjects(ctx context.Context, s *dialect.Show) ([]meta.ColumnDef, [][]any, error) {
	var names []string
	if s.Type == dialect.ShowTriggers {
		names = []string{"Trigger", "Event", "Table", "Statement", "Timing", "Created", "sql_mode",
			"Definer", "character_set_client", "collation_connection", "Database Collation"}
	} else {
		names = []string{"Db", "Name", "Definer", "Time zone", "Type", "Execute at", "Interval value",
			"Interval field", "Starts", "Ends", "Status", "Originator", "character_set_client",
			"collation_connection", "Database Collation"}
	}
	defs := make([]meta.ColumnDef, len(names))
	for i, n := range names {
		defs[i] = textCol(n, 64)
	}
	if !infoschema.Is(s.Database) {
		_, release, err := h.sess.CatalogConn(ctx, s.Database)
		if err != nil {
			return nil, nil, h.fail(err, errs.Hint{DatabaseArg: s.Database})
		}
		release()
	}
	return defs, nil, nil
}

func showInfoSchema(s *dialect.Show) ([]meta.ColumnDef, [][]any, error) {
	switch s.Type {
	case dialect.ShowTables:
		var entries []catalog.TableEntry
		for _, n := range infoschema.Names() {
			entries = append(entries, catalog.TableEntry{Name: n})
		}
		defs, rows, err := showTables(s, entries)
		for _, r := range rows {
			if len(r) > 1 {
				r[1] = "SYSTEM VIEW"
			}
		}
		return defs, rows, err
	case dialect.ShowColumns:
		t, ok := infoschema.Lookup(s.Table)
		if !ok {
			return nil, nil, toMySQL(errs.UnknownTableIn(s.Table, infoschema.Name))
		}
		defs, rows := showColumns(s, t)
		return defs, rows, nil
	}
	return nil, nil, toMySQL(errs.Unsupported("SHOW on information_schema", "it has no stored tables"))
}

func showTables(s *dialect.Show, tables []catalog.TableEntry) ([]meta.ColumnDef, [][]any, error) {
	defs := []meta.ColumnDef{textCol(dialect.LikeColumn("Tables_in_"+s.Database, s.Like), 64)}
	if s.Full {
		defs = append(defs, enumCol("Table_type", 11))
	}
	rows := make([][]any, len(tables))
	for i, t := range tables {
		rows[i] = []any{t.Name}
		if s.Full {
			typ := "BASE TABLE"
			if t.View {
				typ = "VIEW"
			}
			rows[i] = append(rows[i], typ)
		}
	}
	return defs, rows, nil
}

func showColumns(s *dialect.Show, t *meta.Table) ([]meta.ColumnDef, [][]any) {
	defs := []meta.ColumnDef{textCol("Field", 64), blobCol("Type")}
	if s.Full {
		defs = append(defs, textCol("Collation", 64))
	}
	defs = append(defs, textCol("Null", 3), enumCol("Key", 3), blobCol("Default"), textCol("Extra", 256))
	if s.Full {
		defs = append(defs, textCol("Privileges", 80), blobCol("Comment"))
	}
	var rows [][]any
	for _, c := range t.Columns {
		auto := strings.EqualFold(c.Name, t.AutoIncrement)
		key := catalog.ColumnKey(t, c.Name)
		row := []any{c.Name, c.MySQLType()}
		if s.Full {
			row = append(row, nullIfEmpty(catalog.ColumnCollation(c)))
		}
		null := "YES"
		if c.NotNull || auto || key == "PRI" {
			null = "NO"
		}
		var def any
		if c.Default != nil {
			if v, ok := catalog.DefaultValue(*c.Default); ok && v != nil {
				def = *v
			}
		}
		extra := ""
		if auto {
			extra = "auto_increment"
		}
		row = append(row, null, key, def, extra)
		if s.Full {
			row = append(row, "select,insert,update,references", c.Options[catalog.SidecarKeyComment])
		}
		rows = append(rows, row)
	}
	return defs, rows
}

func showIndex(t *meta.Table) ([]meta.ColumnDef, [][]any) {
	defs := []meta.ColumnDef{
		textCol("Table", 64), numCol("Non_unique"), textCol("Key_name", 64), numCol("Seq_in_index"),
		textCol("Column_name", 64), textCol("Collation", 1), numCol("Cardinality"), numCol("Sub_part"),
		textCol("Packed", 10), textCol("Null", 3), textCol("Index_type", 16), textCol("Comment", 16),
		textCol("Index_comment", 1024), textCol("Visible", 3), textCol("Expression", 64),
	}
	var rows [][]any
	add := func(name string, unique bool, cols []string) {
		nonUnique := int64(1)
		if unique {
			nonUnique = 0
		}
		for i, col := range cols {
			null := ""
			if c, ok := findColumn(t, col); ok && !c.NotNull && name != "PRIMARY" {
				null = "YES"
			}
			rows = append(rows, []any{
				t.Name, nonUnique, name, int64(i + 1), col, "A", nil, nil,
				nil, null, "BTREE", "", "", "YES", nil,
			})
		}
	}
	if len(t.PrimaryKey) > 0 {
		add("PRIMARY", true, t.PrimaryKey)
	}
	for _, ix := range t.Indexes {
		add(ix.Name, ix.Unique, ix.Columns)
	}
	return defs, rows
}

func (h *handler) showTableStatus(ctx context.Context, conn *sql.Conn, s *dialect.Show, tables []catalog.TableEntry) ([]meta.ColumnDef, [][]any, error) {
	defs := []meta.ColumnDef{
		textCol("Name", 64), textCol("Engine", 64), numCol("Version"), enumCol("Row_format", 10),
		numCol("Rows"), numCol("Avg_row_length"), numCol("Data_length"), numCol("Max_data_length"),
		numCol("Index_length"), numCol("Data_free"), numCol("Auto_increment"),
		timeCol("Create_time", meta.TypeTimestamp), timeCol("Update_time", meta.TypeDatetime), timeCol("Check_time", meta.TypeDatetime),
		textCol("Collation", 64), numCol("Checksum"), textCol("Create_options", 256), textCol("Comment", 2048),
	}
	var rows [][]any
	for _, entry := range tables {
		if entry.View {
			rows = append(rows, []any{entry.Name, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, "VIEW"})
			continue
		}
		t, err := h.catalog.Table(ctx, conn, s.Database, entry.Name)
		if err != nil {
			return nil, nil, h.fail(err, errs.Hint{Database: s.Database})
		}
		st, err := catalog.TableStats(ctx, conn, t.Name)
		if err != nil {
			return nil, nil, h.fail(err, errs.Hint{})
		}
		next, err := catalog.NextAutoIncrement(ctx, conn, t)
		if err != nil {
			return nil, nil, h.fail(err, errs.Hint{})
		}
		var auto any
		if t.AutoIncrement != "" {
			auto = next
		}
		avg := int64(0)
		if st.Rows > 0 {
			avg = st.Data / st.Rows
		}
		rowFormat := "Dynamic"
		if rf := t.Options[catalog.SidecarKeyRowFormat]; rf != "" {
			rowFormat = rf
		}
		rows = append(rows, []any{
			t.Name, catalog.Engine(t), int64(10), rowFormat,
			st.Rows, avg, st.Data, int64(0), st.Index, int64(0), auto,
			nil, nil, nil,
			catalog.TableCollation(t), nil, "", t.Options[catalog.SidecarKeyComment],
		})
	}
	return defs, rows, nil
}

// filterRows applies LIKE (on the first column) or WHERE. For WHERE the rows become a
// VALUES list on the scratch connection, text columns compared with mysql_ci as MySQL
// compares utf8mb3_general_ci, and the predicate selects the row numbers to keep.
func (h *handler) filterRows(ctx context.Context, s *dialect.Show, defs []meta.ColumnDef, rows [][]any) ([][]any, error) {
	if s.Like != nil {
		// LIKE matches in Go: SQLite's LIKE folds ASCII case only, MySQL's follows the
		// collation ('a' matches 'à').
		var out [][]any
		for _, row := range rows {
			if v, ok := row[0].(string); ok && collate.LikeCI(v, *s.Like, '\\') {
				out = append(out, row)
			}
		}
		return out, nil
	}
	if s.Where == "" {
		return rows, nil
	}
	for _, r := range s.Refs {
		name := r.Column
		if r.Qualifier != "" {
			name = r.Qualifier + "." + r.Column
		}
		if r.Qualifier != "" || !hasColumn(defs, r.Column) {
			return nil, toMySQL(errs.UnknownColumn(name, r.Clause))
		}
	}
	if len(rows) == 0 {
		return rows, nil
	}
	var b strings.Builder
	b.WriteString("SELECT \"__row\" FROM (SELECT column1 AS \"__row\"")
	for i, d := range defs {
		fmt.Fprintf(&b, ", column%d", i+2)
		if d.Type == meta.TypeVarString {
			b.WriteString(" COLLATE mysql_ci")
		}
		b.WriteString(" AS ")
		b.WriteString(quoteName(d.Name))
	}
	b.WriteString(" FROM (VALUES ")
	placeholders := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(defs)+1), ", ") + ")"
	args := make([]any, 0, len(rows)*(len(defs)+1)+len(s.Args)+1)
	for i, row := range rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(placeholders)
		args = append(args, int64(i))
		args = append(args, row...)
	}
	b.WriteString(")) AS s")
	b.WriteString(s.Where)
	args = append(args, s.Args...)

	conn, err := h.eng.Scratch(ctx)
	if err != nil {
		return nil, h.fail(err, errs.Hint{})
	}
	defer conn.Close()
	res, err := conn.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, h.fail(err, errs.Hint{})
	}
	defer res.Close()
	var out [][]any
	for res.Next() {
		var i int64
		if err := res.Scan(&i); err != nil {
			return nil, h.fail(err, errs.Hint{})
		}
		out = append(out, rows[i])
	}
	if err := res.Err(); err != nil {
		return nil, h.fail(err, errs.Hint{})
	}
	return out, nil
}

func hasColumn(defs []meta.ColumnDef, name string) bool {
	for _, d := range defs {
		if strings.EqualFold(d.Name, name) {
			return true
		}
	}
	return false
}

func findColumn(t *meta.Table, name string) (meta.Column, bool) {
	for _, c := range t.Columns {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return meta.Column{}, false
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func quoteName(name string) string {
	return "\"" + strings.ReplaceAll(name, "\"", "\"\"") + "\""
}

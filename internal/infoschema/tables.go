package infoschema

import (
	"context"
	"database/sql"
	"regexp"
	"strconv"
	"strings"

	"mysqlite/internal/catalog"
	"mysqlite/internal/charset"
	"mysqlite/internal/meta"
)

// Column types, as MySQL 8.0 declares them; enums are described as VARCHAR.
const (
	name64  = "varchar(64)"
	uint64T = "bigint unsigned"
	uint32T = "int unsigned"
	longtxt = "longtext"
)

func cols(spec ...string) []column {
	out := make([]column, 0, len(spec)/2)
	for i := 0; i+1 < len(spec); i += 2 {
		out = append(out, column{name: spec[i], typ: spec[i+1]})
	}
	return out
}

func init() {
	define(&table{name: "SCHEMATA", cols: cols(
		"CATALOG_NAME", name64, "SCHEMA_NAME", name64, "DEFAULT_CHARACTER_SET_NAME", name64,
		"DEFAULT_COLLATION_NAME", name64, "SQL_PATH", "varchar(512)", "DEFAULT_ENCRYPTION", "varchar(3)",
	), rows: schemata})
	define(&table{name: "TABLES", cols: cols(
		"TABLE_CATALOG", name64, "TABLE_SCHEMA", name64, "TABLE_NAME", name64, "TABLE_TYPE", name64,
		"ENGINE", name64, "VERSION", "int", "ROW_FORMAT", "varchar(10)", "TABLE_ROWS", uint64T,
		"AVG_ROW_LENGTH", uint64T, "DATA_LENGTH", uint64T, "MAX_DATA_LENGTH", uint64T,
		"INDEX_LENGTH", uint64T, "DATA_FREE", uint64T, "AUTO_INCREMENT", uint64T,
		"CREATE_TIME", "datetime", "UPDATE_TIME", "datetime", "CHECK_TIME", "datetime",
		"TABLE_COLLATION", name64, "CHECKSUM", "bigint", "CREATE_OPTIONS", "varchar(256)",
		"TABLE_COMMENT", "text",
	), rows: tablesRows})
	define(&table{name: "COLUMNS", cols: cols(
		"TABLE_CATALOG", name64, "TABLE_SCHEMA", name64, "TABLE_NAME", name64, "COLUMN_NAME", name64,
		"ORDINAL_POSITION", uint32T, "COLUMN_DEFAULT", "text", "IS_NULLABLE", "varchar(3)",
		"DATA_TYPE", longtxt, "CHARACTER_MAXIMUM_LENGTH", "bigint", "CHARACTER_OCTET_LENGTH", "bigint",
		"NUMERIC_PRECISION", uint64T, "NUMERIC_SCALE", uint64T, "DATETIME_PRECISION", uint32T,
		"CHARACTER_SET_NAME", name64, "COLLATION_NAME", name64, "COLUMN_TYPE", "mediumtext",
		"COLUMN_KEY", "varchar(3)", "EXTRA", "varchar(256)", "PRIVILEGES", "varchar(154)",
		"COLUMN_COMMENT", "text", "GENERATION_EXPRESSION", longtxt, "SRS_ID", uint32T,
	), rows: columnsRows})
	define(&table{name: "STATISTICS", cols: cols(
		"TABLE_CATALOG", name64, "TABLE_SCHEMA", name64, "TABLE_NAME", name64, "NON_UNIQUE", "int",
		"INDEX_SCHEMA", name64, "INDEX_NAME", name64, "SEQ_IN_INDEX", uint32T, "COLUMN_NAME", name64,
		"COLLATION", "varchar(1)", "CARDINALITY", "bigint", "SUB_PART", "bigint", "PACKED", "varchar(10)",
		"NULLABLE", "varchar(3)", "INDEX_TYPE", "varchar(11)", "COMMENT", "varchar(8)",
		"INDEX_COMMENT", "varchar(2048)", "IS_VISIBLE", "varchar(3)", "EXPRESSION", longtxt,
	), rows: statisticsRows})
	define(&table{name: "TABLE_CONSTRAINTS", cols: cols(
		"CONSTRAINT_CATALOG", name64, "CONSTRAINT_SCHEMA", name64, "CONSTRAINT_NAME", name64,
		"TABLE_SCHEMA", name64, "TABLE_NAME", name64, "CONSTRAINT_TYPE", "varchar(11)", "ENFORCED", "varchar(3)",
	), rows: constraintRows})
	define(&table{name: "KEY_COLUMN_USAGE", cols: cols(
		"CONSTRAINT_CATALOG", name64, "CONSTRAINT_SCHEMA", name64, "CONSTRAINT_NAME", name64,
		"TABLE_CATALOG", name64, "TABLE_SCHEMA", name64, "TABLE_NAME", name64, "COLUMN_NAME", name64,
		"ORDINAL_POSITION", uint32T, "POSITION_IN_UNIQUE_CONSTRAINT", uint32T,
		"REFERENCED_TABLE_SCHEMA", name64, "REFERENCED_TABLE_NAME", name64, "REFERENCED_COLUMN_NAME", name64,
	), rows: keyColumnRows})
	define(&table{name: "REFERENTIAL_CONSTRAINTS", cols: cols(
		"CONSTRAINT_CATALOG", name64, "CONSTRAINT_SCHEMA", name64, "CONSTRAINT_NAME", name64,
		"UNIQUE_CONSTRAINT_CATALOG", name64, "UNIQUE_CONSTRAINT_SCHEMA", name64, "UNIQUE_CONSTRAINT_NAME", name64,
		"MATCH_OPTION", name64, "UPDATE_RULE", name64, "DELETE_RULE", name64,
		"TABLE_NAME", name64, "REFERENCED_TABLE_NAME", name64,
	)})
	define(&table{name: "CHECK_CONSTRAINTS", cols: cols(
		"CONSTRAINT_CATALOG", name64, "CONSTRAINT_SCHEMA", name64, "CONSTRAINT_NAME", name64,
		"CHECK_CLAUSE", longtxt,
	)})
	define(&table{name: "VIEWS", cols: cols(
		"TABLE_CATALOG", name64, "TABLE_SCHEMA", name64, "TABLE_NAME", name64, "VIEW_DEFINITION", longtxt,
		"CHECK_OPTION", "varchar(8)", "IS_UPDATABLE", "varchar(3)", "DEFINER", "varchar(288)",
		"SECURITY_TYPE", "varchar(7)", "CHARACTER_SET_CLIENT", name64, "COLLATION_CONNECTION", name64,
	)})
	define(&table{name: "TRIGGERS", cols: cols(
		"TRIGGER_CATALOG", name64, "TRIGGER_SCHEMA", name64, "TRIGGER_NAME", name64,
		"EVENT_MANIPULATION", "varchar(6)", "EVENT_OBJECT_CATALOG", name64, "EVENT_OBJECT_SCHEMA", name64,
		"EVENT_OBJECT_TABLE", name64, "ACTION_ORDER", uint32T, "ACTION_CONDITION", "binary(0)",
		"ACTION_STATEMENT", longtxt, "ACTION_ORIENTATION", "varchar(3)", "ACTION_TIMING", "varchar(6)",
		"ACTION_REFERENCE_OLD_TABLE", "binary(0)", "ACTION_REFERENCE_NEW_TABLE", "binary(0)",
		"ACTION_REFERENCE_OLD_ROW", "varchar(3)", "ACTION_REFERENCE_NEW_ROW", "varchar(3)",
		"CREATED", "datetime", "SQL_MODE", "varchar(520)", "DEFINER", "varchar(288)",
		"CHARACTER_SET_CLIENT", name64, "COLLATION_CONNECTION", name64, "DATABASE_COLLATION", name64,
	)})
	define(&table{name: "ROUTINES", cols: cols(
		"SPECIFIC_NAME", name64, "ROUTINE_CATALOG", name64, "ROUTINE_SCHEMA", name64, "ROUTINE_NAME", name64,
		"ROUTINE_TYPE", "varchar(9)", "DATA_TYPE", longtxt, "CHARACTER_MAXIMUM_LENGTH", "bigint",
		"CHARACTER_OCTET_LENGTH", "bigint", "NUMERIC_PRECISION", uint32T, "NUMERIC_SCALE", uint32T,
		"DATETIME_PRECISION", uint32T, "CHARACTER_SET_NAME", name64, "COLLATION_NAME", name64,
		"DTD_IDENTIFIER", longtxt, "ROUTINE_BODY", "varchar(8)", "ROUTINE_DEFINITION", longtxt,
		"EXTERNAL_NAME", "binary(0)", "EXTERNAL_LANGUAGE", name64, "PARAMETER_STYLE", "varchar(3)",
		"IS_DETERMINISTIC", "varchar(3)", "SQL_DATA_ACCESS", "varchar(17)", "SQL_PATH", "binary(0)",
		"SECURITY_TYPE", "varchar(7)", "CREATED", "datetime", "LAST_ALTERED", "datetime",
		"SQL_MODE", "varchar(520)", "ROUTINE_COMMENT", "text", "DEFINER", "varchar(288)",
		"CHARACTER_SET_CLIENT", name64, "COLLATION_CONNECTION", name64, "DATABASE_COLLATION", name64,
	)})
	define(&table{name: "PARAMETERS", cols: cols(
		"SPECIFIC_CATALOG", name64, "SPECIFIC_SCHEMA", name64, "SPECIFIC_NAME", name64,
		"ORDINAL_POSITION", uint64T, "PARAMETER_MODE", "varchar(5)", "PARAMETER_NAME", name64,
		"DATA_TYPE", longtxt, "CHARACTER_MAXIMUM_LENGTH", "bigint", "CHARACTER_OCTET_LENGTH", "bigint",
		"NUMERIC_PRECISION", uint32T, "NUMERIC_SCALE", "bigint", "DATETIME_PRECISION", uint32T,
		"CHARACTER_SET_NAME", name64, "COLLATION_NAME", name64, "DTD_IDENTIFIER", "mediumtext",
		"ROUTINE_TYPE", "varchar(9)",
	)})
	define(&table{name: "EVENTS", cols: cols(
		"EVENT_CATALOG", name64, "EVENT_SCHEMA", name64, "EVENT_NAME", name64, "DEFINER", "varchar(288)",
		"TIME_ZONE", name64, "EVENT_BODY", "varchar(3)", "EVENT_DEFINITION", longtxt,
		"EVENT_TYPE", "varchar(9)", "EXECUTE_AT", "datetime", "INTERVAL_VALUE", "varchar(256)",
		"INTERVAL_FIELD", "varchar(18)", "SQL_MODE", "varchar(520)", "STARTS", "datetime", "ENDS", "datetime",
		"STATUS", "varchar(21)", "ON_COMPLETION", "varchar(12)", "CREATED", "datetime",
		"LAST_ALTERED", "datetime", "LAST_EXECUTED", "datetime", "EVENT_COMMENT", "varchar(2048)",
		"ORIGINATOR", uint32T, "CHARACTER_SET_CLIENT", name64, "COLLATION_CONNECTION", name64,
		"DATABASE_COLLATION", name64,
	)})
	define(&table{name: "PARTITIONS", cols: cols(
		"TABLE_CATALOG", name64, "TABLE_SCHEMA", name64, "TABLE_NAME", name64, "PARTITION_NAME", name64,
		"SUBPARTITION_NAME", name64, "PARTITION_ORDINAL_POSITION", uint32T,
		"SUBPARTITION_ORDINAL_POSITION", uint32T, "PARTITION_METHOD", "varchar(13)",
		"SUBPARTITION_METHOD", "varchar(13)", "PARTITION_EXPRESSION", "varchar(2048)",
		"SUBPARTITION_EXPRESSION", "varchar(2048)", "PARTITION_DESCRIPTION", "text",
		"TABLE_ROWS", uint64T, "AVG_ROW_LENGTH", uint64T, "DATA_LENGTH", uint64T,
		"MAX_DATA_LENGTH", uint64T, "INDEX_LENGTH", uint64T, "DATA_FREE", uint64T,
		"CREATE_TIME", "datetime", "UPDATE_TIME", "datetime", "CHECK_TIME", "datetime",
		"CHECKSUM", "bigint", "PARTITION_COMMENT", "text", "NODEGROUP", "varchar(256)",
		"TABLESPACE_NAME", "varchar(268)",
	)})
	define(&table{name: "ENGINES", cols: cols(
		"ENGINE", name64, "SUPPORT", "varchar(8)", "COMMENT", "varchar(80)",
		"TRANSACTIONS", "varchar(3)", "XA", "varchar(3)", "SAVEPOINTS", "varchar(3)",
	), rows: func(context.Context, Source) ([][]any, error) {
		return [][]any{{"SQLite", "DEFAULT", "SQLite storage through mysqlite", "YES", "NO", "NO"}}, nil
	}})
	define(&table{name: "CHARACTER_SETS", cols: cols(
		"CHARACTER_SET_NAME", name64, "DEFAULT_COLLATE_NAME", name64, "DESCRIPTION", "varchar(2048)", "MAXLEN", uint32T,
	), rows: func(context.Context, Source) ([][]any, error) {
		var rows [][]any
		for _, c := range charset.Charsets {
			rows = append(rows, []any{c.Name, c.DefaultCollation, c.Description, int64(c.MaxLen)})
		}
		return rows, nil
	}})
	define(&table{name: "COLLATIONS", cols: cols(
		"COLLATION_NAME", name64, "CHARACTER_SET_NAME", name64, "ID", uint64T, "IS_DEFAULT", "varchar(3)",
		"IS_COMPILED", "varchar(3)", "SORTLEN", uint32T, "PAD_ATTRIBUTE", "varchar(9)",
	), rows: func(context.Context, Source) ([][]any, error) {
		var rows [][]any
		for _, c := range charset.Collations {
			def := ""
			if c.Default {
				def = "Yes"
			}
			rows = append(rows, []any{c.Name, c.Charset, int64(c.ID), def, "Yes", SortLen(c.Name), c.PadAttribute})
		}
		return rows, nil
	}})
	define(&table{name: "COLLATION_CHARACTER_SET_APPLICABILITY", cols: cols(
		"COLLATION_NAME", name64, "CHARACTER_SET_NAME", name64,
	), rows: func(context.Context, Source) ([][]any, error) {
		var rows [][]any
		for _, c := range charset.Collations {
			rows = append(rows, []any{c.Name, c.Charset})
		}
		return rows, nil
	}})
	define(&table{name: "PROCESSLIST", cols: cols(
		"ID", uint64T, "USER", "varchar(32)", "HOST", "varchar(261)", "DB", name64,
		"COMMAND", "varchar(16)", "TIME", "int", "STATE", name64, "INFO", longtxt,
	), rows: func(_ context.Context, src Source) ([][]any, error) {
		var rows [][]any
		for _, p := range src.Processes() {
			var db, state any
			if p.DB != "" {
				db = p.DB
			}
			if p.State != "" {
				state = p.State
			}
			rows = append(rows, []any{int64(p.ID), p.User, p.Host, db, p.Command, p.Time, state, p.Info})
		}
		return rows, nil
	}})
	define(&table{name: "USER_PRIVILEGES", cols: cols(
		"GRANTEE", "varchar(292)", "TABLE_CATALOG", "varchar(512)", "PRIVILEGE_TYPE", name64, "IS_GRANTABLE", "varchar(3)",
	), rows: func(_ context.Context, src Source) ([][]any, error) {
		grantee := "'" + src.User() + "'@'%'"
		var rows [][]any
		for _, p := range privileges {
			rows = append(rows, []any{grantee, "def", p, "YES"})
		}
		return rows, nil
	}})
	define(&table{name: "SCHEMA_PRIVILEGES", cols: cols(
		"GRANTEE", "varchar(292)", "TABLE_CATALOG", "varchar(512)", "TABLE_SCHEMA", name64,
		"PRIVILEGE_TYPE", name64, "IS_GRANTABLE", "varchar(3)",
	)})
	define(&table{name: "TABLE_PRIVILEGES", cols: cols(
		"GRANTEE", "varchar(292)", "TABLE_CATALOG", "varchar(512)", "TABLE_SCHEMA", name64,
		"TABLE_NAME", name64, "PRIVILEGE_TYPE", name64, "IS_GRANTABLE", "varchar(3)",
	)})
}

// privileges are the static global privileges of MySQL 8.0, all held by every
// configured user: mysqlite has no privilege system.
var privileges = []string{
	"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP", "RELOAD", "SHUTDOWN", "PROCESS",
	"FILE", "REFERENCES", "INDEX", "ALTER", "SHOW DATABASES", "SUPER", "CREATE TEMPORARY TABLES",
	"LOCK TABLES", "EXECUTE", "REPLICATION SLAVE", "REPLICATION CLIENT", "CREATE VIEW", "SHOW VIEW",
	"CREATE ROUTINE", "ALTER ROUTINE", "CREATE USER", "EVENT", "TRIGGER", "CREATE TABLESPACE",
	"CREATE ROLE", "DROP ROLE",
}

// SortLen is MySQL's Sortlen: 1 for the simple collations, 8 for unicode_ci, 0 for 0900.
func SortLen(collation string) int64 {
	switch {
	case strings.HasSuffix(collation, "_unicode_ci"):
		return 8
	case strings.Contains(collation, "_0900_"):
		return 0
	}
	return 1
}

func schemata(_ context.Context, src Source) ([][]any, error) {
	dbs, err := src.Databases()
	if err != nil {
		return nil, err
	}
	rows := [][]any{{"def", Name, "utf8mb3", "utf8mb3_general_ci", nil, "NO"}}
	for _, db := range dbs {
		rows = append(rows, []any{"def", db, "utf8mb4", "utf8mb4_general_ci", nil, "NO"})
	}
	return rows, nil
}

// eachTable calls f for every table of every user database.
func eachTable(ctx context.Context, src Source, f func(conn *sql.Conn, db string, entry catalog.TableEntry, describe func(string) (*meta.Table, error)) error) error {
	dbs, err := src.Databases()
	if err != nil {
		return err
	}
	for _, db := range dbs {
		err := src.Read(ctx, db, func(conn *sql.Conn, describe func(string) (*meta.Table, error)) error {
			entries, err := catalog.ListTables(ctx, conn)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if err := f(conn, db, e, describe); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func tablesRows(ctx context.Context, src Source) ([][]any, error) {
	var rows [][]any
	for _, name := range Names() {
		rows = append(rows, []any{"def", Name, name, "SYSTEM VIEW", nil, int64(10), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, ""})
	}
	err := eachTable(ctx, src, func(conn *sql.Conn, db string, e catalog.TableEntry, describe func(string) (*meta.Table, error)) error {
		if e.View {
			rows = append(rows, []any{"def", db, e.Name, "VIEW", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, "VIEW"})
			return nil
		}
		t, err := describe(e.Name)
		if err != nil {
			return err
		}
		st, err := catalog.TableStats(ctx, conn, t.Name)
		if err != nil {
			return err
		}
		var auto any
		if t.AutoIncrement != "" {
			next, err := catalog.NextAutoIncrement(ctx, conn, t)
			if err != nil {
				return err
			}
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
			"def", db, t.Name, "BASE TABLE", catalog.Engine(t), int64(10), rowFormat,
			st.Rows, avg, st.Data, int64(0), st.Index, int64(0), auto,
			nil, nil, nil, catalog.TableCollation(t), nil, "", t.Options[catalog.SidecarKeyComment],
		})
		return nil
	})
	return rows, err
}

func columnsRows(ctx context.Context, src Source) ([][]any, error) {
	var rows [][]any
	err := eachTable(ctx, src, func(_ *sql.Conn, db string, e catalog.TableEntry, describe func(string) (*meta.Table, error)) error {
		if e.View {
			return nil
		}
		t, err := describe(e.Name)
		if err != nil {
			return err
		}
		for i, c := range t.Columns {
			rows = append(rows, columnRow(db, t, i, c))
		}
		return nil
	})
	return rows, err
}

var reType = regexp.MustCompile(`^([a-z]+)(?:\((\d+)(?:,(\d+))?\))?`)

func columnRow(db string, t *meta.Table, i int, c meta.Column) []any {
	colType := strings.ToLower(c.MySQLType())
	m := reType.FindStringSubmatch(colType)
	dataType := colType
	var n, d int64 = -1, -1
	if m != nil {
		dataType = m[1]
		if m[2] != "" {
			n, _ = strconv.ParseInt(m[2], 10, 64)
		}
		if m[3] != "" {
			d, _ = strconv.ParseInt(m[3], 10, 64)
		}
	}
	unsigned := strings.Contains(colType, "unsigned")
	var charLen, octetLen, precision, scale, dtPrecision, cs, co any
	collation := catalog.ColumnCollation(c)
	maxLen := int64(4)
	if co, ok := charset.LookupCollation(collation); ok {
		if ch, ok := charset.Lookup(co.Charset); ok {
			maxLen = int64(ch.MaxLen)
		}
	}
	switch dataType {
	case "char", "varchar":
		if n < 0 {
			n = 1
		}
		charLen, octetLen = n, n*maxLen
		cs, co = charsetOf(collation), collation
	case "tinytext", "text", "mediumtext", "longtext":
		l := map[string]int64{"tinytext": 255, "text": 65535, "mediumtext": 16777215, "longtext": 4294967295}[dataType]
		charLen, octetLen = l, l
		cs, co = charsetOf(collation), collation
	case "binary", "varbinary":
		if n < 0 {
			n = 1
		}
		charLen, octetLen = n, n
	case "tinyblob", "blob", "mediumblob", "longblob":
		l := map[string]int64{"tinyblob": 255, "blob": 65535, "mediumblob": 16777215, "longblob": 4294967295}[dataType]
		charLen, octetLen = l, l
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint":
		p := map[string]int64{"tinyint": 3, "smallint": 5, "mediumint": 7, "int": 10, "integer": 10, "bigint": 19}[dataType]
		if dataType == "bigint" && unsigned {
			p = 20
		}
		precision, scale = p, int64(0)
	case "decimal":
		if n < 0 {
			n = 10
		}
		if d < 0 {
			d = 0
		}
		precision, scale = n, d
	case "float":
		precision = int64(12)
	case "double":
		precision = int64(22)
	case "datetime", "timestamp", "time":
		if n < 0 {
			n = 0
		}
		dtPrecision = n
	}
	if d >= 0 && (dataType == "float" || dataType == "double") {
		precision, scale = n, d
	}
	auto := strings.EqualFold(c.Name, t.AutoIncrement)
	key := catalog.ColumnKey(t, c.Name)
	nullable := "YES"
	if c.NotNull || auto || key == "PRI" {
		nullable = "NO"
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
	return []any{
		"def", db, t.Name, c.Name, int64(i + 1), def, nullable, dataType, charLen, octetLen,
		precision, scale, dtPrecision, cs, co, colType, key, extra,
		"select,insert,update,references", c.Options[catalog.SidecarKeyComment], "", nil,
	}
}

func charsetOf(collation string) any {
	if co, ok := charset.LookupCollation(collation); ok {
		return co.Charset
	}
	return nil
}

type key struct {
	name    string
	unique  bool
	primary bool
	columns []string
}

func keysOf(t *meta.Table) []key {
	var out []key
	if len(t.PrimaryKey) > 0 {
		out = append(out, key{name: "PRIMARY", unique: true, primary: true, columns: t.PrimaryKey})
	}
	for _, ix := range t.Indexes {
		out = append(out, key{name: ix.Name, unique: ix.Unique, columns: ix.Columns})
	}
	return out
}

func statisticsRows(ctx context.Context, src Source) ([][]any, error) {
	var rows [][]any
	err := eachTable(ctx, src, func(_ *sql.Conn, db string, e catalog.TableEntry, describe func(string) (*meta.Table, error)) error {
		if e.View {
			return nil
		}
		t, err := describe(e.Name)
		if err != nil {
			return err
		}
		for _, k := range keysOf(t) {
			nonUnique := int64(1)
			if k.unique {
				nonUnique = 0
			}
			for i, col := range k.columns {
				nullable := ""
				for _, c := range t.Columns {
					if strings.EqualFold(c.Name, col) && !c.NotNull && !k.primary {
						nullable = "YES"
					}
				}
				rows = append(rows, []any{
					"def", db, t.Name, nonUnique, db, k.name, int64(i + 1), col, "A", nil, nil, nil,
					nullable, "BTREE", "", "", "YES", nil,
				})
			}
		}
		return nil
	})
	return rows, err
}

func constraintRows(ctx context.Context, src Source) ([][]any, error) {
	var rows [][]any
	err := eachTable(ctx, src, func(_ *sql.Conn, db string, e catalog.TableEntry, describe func(string) (*meta.Table, error)) error {
		if e.View {
			return nil
		}
		t, err := describe(e.Name)
		if err != nil {
			return err
		}
		for _, k := range keysOf(t) {
			switch {
			case k.primary:
				rows = append(rows, []any{"def", db, k.name, db, t.Name, "PRIMARY KEY", "YES"})
			case k.unique:
				rows = append(rows, []any{"def", db, k.name, db, t.Name, "UNIQUE", "YES"})
			}
		}
		return nil
	})
	return rows, err
}

func keyColumnRows(ctx context.Context, src Source) ([][]any, error) {
	var rows [][]any
	err := eachTable(ctx, src, func(_ *sql.Conn, db string, e catalog.TableEntry, describe func(string) (*meta.Table, error)) error {
		if e.View {
			return nil
		}
		t, err := describe(e.Name)
		if err != nil {
			return err
		}
		for _, k := range keysOf(t) {
			if !k.unique {
				continue
			}
			for i, col := range k.columns {
				rows = append(rows, []any{"def", db, k.name, "def", db, t.Name, col, int64(i + 1), nil, nil, nil, nil})
			}
		}
		return nil
	})
	return rows, err
}

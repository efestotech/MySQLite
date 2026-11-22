// Package vars is the system variable registry (docs/04-session-catalog.md section 1):
// every variable's name, scope, type and default, and the values one session reads.
// Defaults follow MySQL 8.0 and are `oracle: pending` until mysql-ref checks them.
package vars

import (
	"math"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"mysqlite/internal/errs"
)

// Kind is how a variable's value is typed when it is read.
type Kind int

const (
	KindString Kind = iota + 1
	KindInt
	KindUint
	// KindBool is read as the integer 0 or 1, as MySQL returns ON/OFF variables.
	KindBool
	KindDouble
)

// Scope says where a variable exists.
type Scope uint8

const (
	ScopeSession Scope = 1 << iota
	ScopeGlobal
	ScopeBoth = ScopeSession | ScopeGlobal
)

// Def is one registry entry.
type Def struct {
	Name     string
	Scope    Scope
	Kind     Kind
	Nullable bool
}

// ReadScope is the scope a statement names: @@x, @@session.x / @@local.x, @@global.x.
type ReadScope int

const (
	ReadDefault ReadScope = iota
	ReadSession
	ReadGlobal
)

// Value is a variable's value: Text is its text form, as the text protocol sends it.
type Value struct {
	Null bool
	Text string
}

// defs is the registry. tx_isolation, tx_read_only, storage_engine and the query cache
// variables are not here: MySQL 8.0 removed them, so reading them is 1193 there too.
var defs = []Def{
	{Name: "auto_increment_increment", Scope: ScopeBoth, Kind: KindUint},
	{Name: "auto_increment_offset", Scope: ScopeBoth, Kind: KindUint},
	{Name: "autocommit", Scope: ScopeBoth, Kind: KindBool},
	{Name: "basedir", Scope: ScopeGlobal, Kind: KindString},
	{Name: "binlog_format", Scope: ScopeBoth, Kind: KindString},
	{Name: "character_set_client", Scope: ScopeBoth, Kind: KindString},
	{Name: "character_set_connection", Scope: ScopeBoth, Kind: KindString},
	{Name: "character_set_database", Scope: ScopeBoth, Kind: KindString},
	{Name: "character_set_results", Scope: ScopeBoth, Kind: KindString, Nullable: true},
	{Name: "character_set_server", Scope: ScopeBoth, Kind: KindString},
	{Name: "collation_connection", Scope: ScopeBoth, Kind: KindString},
	{Name: "collation_database", Scope: ScopeBoth, Kind: KindString},
	{Name: "collation_server", Scope: ScopeBoth, Kind: KindString},
	{Name: "connect_timeout", Scope: ScopeGlobal, Kind: KindUint},
	{Name: "datadir", Scope: ScopeGlobal, Kind: KindString},
	{Name: "default_storage_engine", Scope: ScopeBoth, Kind: KindString},
	{Name: "div_precision_increment", Scope: ScopeBoth, Kind: KindUint},
	{Name: "explicit_defaults_for_timestamp", Scope: ScopeBoth, Kind: KindBool},
	{Name: "foreign_key_checks", Scope: ScopeBoth, Kind: KindBool},
	{Name: "group_concat_max_len", Scope: ScopeBoth, Kind: KindUint},
	{Name: "gtid_mode", Scope: ScopeGlobal, Kind: KindString},
	{Name: "hostname", Scope: ScopeGlobal, Kind: KindString},
	{Name: "information_schema_stats_expiry", Scope: ScopeBoth, Kind: KindUint},
	{Name: "init_connect", Scope: ScopeGlobal, Kind: KindString},
	{Name: "interactive_timeout", Scope: ScopeBoth, Kind: KindUint},
	{Name: "lc_messages", Scope: ScopeBoth, Kind: KindString},
	{Name: "lc_time_names", Scope: ScopeBoth, Kind: KindString},
	{Name: "license", Scope: ScopeGlobal, Kind: KindString},
	{Name: "lower_case_file_system", Scope: ScopeGlobal, Kind: KindBool},
	{Name: "lower_case_table_names", Scope: ScopeGlobal, Kind: KindUint},
	{Name: "max_allowed_packet", Scope: ScopeBoth, Kind: KindUint},
	{Name: "net_buffer_length", Scope: ScopeBoth, Kind: KindUint},
	{Name: "net_read_timeout", Scope: ScopeBoth, Kind: KindUint},
	{Name: "net_write_timeout", Scope: ScopeBoth, Kind: KindUint},
	{Name: "optimizer_switch", Scope: ScopeBoth, Kind: KindString},
	{Name: "performance_schema", Scope: ScopeGlobal, Kind: KindBool},
	{Name: "pid_file", Scope: ScopeGlobal, Kind: KindString},
	{Name: "protocol_version", Scope: ScopeGlobal, Kind: KindUint},
	{Name: "socket", Scope: ScopeGlobal, Kind: KindString},
	{Name: "sql_auto_is_null", Scope: ScopeBoth, Kind: KindBool},
	{Name: "sql_mode", Scope: ScopeBoth, Kind: KindString},
	{Name: "sql_require_primary_key", Scope: ScopeBoth, Kind: KindBool},
	{Name: "sql_select_limit", Scope: ScopeBoth, Kind: KindUint},
	{Name: "system_time_zone", Scope: ScopeGlobal, Kind: KindString},
	{Name: "thread_stack", Scope: ScopeGlobal, Kind: KindUint},
	{Name: "time_zone", Scope: ScopeBoth, Kind: KindString},
	{Name: "timestamp", Scope: ScopeSession, Kind: KindDouble},
	{Name: "transaction_isolation", Scope: ScopeBoth, Kind: KindString},
	{Name: "transaction_read_only", Scope: ScopeBoth, Kind: KindBool},
	{Name: "unique_checks", Scope: ScopeBoth, Kind: KindBool},
	{Name: "version", Scope: ScopeGlobal, Kind: KindString},
	{Name: "version_comment", Scope: ScopeGlobal, Kind: KindString},
	{Name: "version_compile_os", Scope: ScopeGlobal, Kind: KindString},
	{Name: "wait_timeout", Scope: ScopeBoth, Kind: KindUint},
}

var byName = func() map[string]*Def {
	m := make(map[string]*Def, len(defs))
	for i := range defs {
		m[defs[i].Name] = &defs[i]
	}
	return m
}()

// Lookup finds a variable; names are case-insensitive.
func Lookup(name string) (*Def, bool) {
	d, ok := byName[strings.ToLower(name)]
	return d, ok
}

// Names returns every registered name, sorted.
func Names() []string {
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	return names
}

// defaultSQLMode is MySQL 8.0's default sql_mode.
const defaultSQLMode = "ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION"

// defaultOptimizerSwitch is MySQL 8.0.36's default optimizer_switch.
const defaultOptimizerSwitch = "index_merge=on,index_merge_union=on,index_merge_sort_union=on,index_merge_intersection=on,engine_condition_pushdown=on,index_condition_pushdown=on,mrr=on,mrr_cost_based=on,block_nested_loop=on,batched_key_access=off,materialization=on,semijoin=on,loosescan=on,firstmatch=on,duplicateweedout=on,subquery_materialization_cost_based=on,use_index_extensions=on,condition_fanout_filter=on,derived_merge=on,use_invisible_indexes=off,skip_scan=on,hash_join=on,subquery_to_derived=off,prefer_ordering_index=on,hypergraph_optimizer=off,derived_condition_pushdown=on"

// Config is what the server-wide values come from.
type Config struct {
	Version          string
	MaxAllowedPacket int
	// DataDir is reported with a trailing separator, as MySQL does.
	DataDir  string
	BaseDir  string
	Hostname string
	// Now gives the time for @@timestamp and system_time_zone; time.Now when nil.
	Now func() time.Time
}

// Server holds the global values, shared by every session.
type Server struct {
	now     func() time.Time
	globals map[string]Value
}

// NewServer builds the global values from cfg and MySQL 8.0's defaults.
func NewServer(cfg Config) *Server {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	zone, _ := now().Zone()
	dataDir := cfg.DataDir
	if dataDir != "" && !strings.HasSuffix(dataDir, "/") && !strings.HasSuffix(dataDir, `\`) {
		dataDir += string(sepFor(dataDir))
	}
	text := map[string]string{
		"auto_increment_increment":        "1",
		"auto_increment_offset":           "1",
		"autocommit":                      "1",
		"basedir":                         cfg.BaseDir,
		"binlog_format":                   "ROW",
		"character_set_client":            "utf8mb4",
		"character_set_connection":        "utf8mb4",
		"character_set_database":          "utf8mb4",
		"character_set_results":           "utf8mb4",
		"character_set_server":            "utf8mb4",
		"collation_connection":            "utf8mb4_general_ci",
		"collation_database":              "utf8mb4_general_ci",
		"collation_server":                "utf8mb4_general_ci",
		"connect_timeout":                 "10",
		"datadir":                         dataDir,
		"default_storage_engine":          "SQLite",
		"div_precision_increment":         "4",
		"explicit_defaults_for_timestamp": "1",
		"foreign_key_checks":              "1",
		"group_concat_max_len":            "1024",
		"gtid_mode":                       "OFF",
		"hostname":                        cfg.Hostname,
		"information_schema_stats_expiry": "86400",
		"init_connect":                    "",
		"interactive_timeout":             "28800",
		"lc_messages":                     "en_US",
		"lc_time_names":                   "en_US",
		"license":                         "",
		"lower_case_file_system":          "0",
		"lower_case_table_names":          "0",
		"max_allowed_packet":              strconv.Itoa(cfg.MaxAllowedPacket),
		"net_buffer_length":               "16384",
		"net_read_timeout":                "30",
		"net_write_timeout":               "60",
		"optimizer_switch":                defaultOptimizerSwitch,
		"performance_schema":              "0",
		"pid_file":                        "",
		"protocol_version":                "10",
		"socket":                          "",
		"sql_auto_is_null":                "0",
		"sql_mode":                        defaultSQLMode,
		"sql_require_primary_key":         "0",
		"sql_select_limit":                "18446744073709551615",
		"system_time_zone":                zone,
		"thread_stack":                    "1048576",
		"time_zone":                       "SYSTEM",
		"transaction_isolation":           "REPEATABLE-READ",
		"transaction_read_only":           "0",
		"unique_checks":                   "1",
		"version":                         cfg.Version,
		"version_comment":                 "mysqlite",
		"version_compile_os":              compileOS(),
		"wait_timeout":                    "28800",
	}
	globals := make(map[string]Value, len(text))
	for name, t := range text {
		globals[name] = Value{Text: t}
	}
	return &Server{now: now, globals: globals}
}

func sepFor(path string) byte {
	if strings.Contains(path, `\`) {
		return '\\'
	}
	return '/'
}

// compileOS is @@version_compile_os as MySQL builds name it.
func compileOS() string {
	switch runtime.GOOS {
	case "linux":
		return "Linux"
	case "windows":
		return "Win64"
	case "darwin":
		return "macos"
	}
	return runtime.GOOS
}

// Session is one connection's view of the variables.
type Session struct {
	srv    *Server
	values map[string]Value
	// autocommit reports the session's autocommit mode, owned by the session layer.
	autocommit func() bool
}

// NewSession starts a session whose values are the globals. autocommit, when not nil,
// supplies @@session.autocommit.
func (s *Server) NewSession(autocommit func() bool) *Session {
	values := make(map[string]Value, len(s.globals))
	for _, d := range defs {
		if d.Scope&ScopeSession != 0 {
			values[d.Name] = s.globals[d.Name]
		}
	}
	return &Session{srv: s, values: values, autocommit: autocommit}
}

// Get reads a variable as `@@[scope.]name` does: 1193 for an unknown name, 1238 when
// the scope does not exist for it. Without a scope, the session value when the
// variable has one, else the global value.
func (s *Session) Get(name string, scope ReadScope) (Value, *Def, *errs.Error) {
	d, ok := Lookup(name)
	if !ok {
		return Value{}, nil, errs.UnknownSystemVariable(name)
	}
	switch scope {
	case ReadGlobal:
		if d.Scope&ScopeGlobal == 0 {
			return Value{}, nil, errs.WrongVariableScope(d.Name, "SESSION")
		}
		return s.global(d), d, nil
	case ReadSession:
		if d.Scope&ScopeSession == 0 {
			return Value{}, nil, errs.WrongVariableScope(d.Name, "GLOBAL")
		}
	default:
		if d.Scope&ScopeSession == 0 {
			return s.global(d), d, nil
		}
	}
	return s.session(d), d, nil
}

func (s *Session) global(d *Def) Value {
	return s.srv.globals[d.Name]
}

func (s *Session) session(d *Def) Value {
	switch d.Name {
	case "autocommit":
		if s.autocommit != nil {
			return boolValue(s.autocommit())
		}
	case "timestamp":
		t := s.srv.now()
		return Value{Text: strconv.FormatFloat(float64(t.UnixMicro())/1e6, 'f', 6, 64)}
	}
	return s.values[d.Name]
}

func boolValue(b bool) Value {
	if b {
		return Value{Text: "1"}
	}
	return Value{Text: "0"}
}

// Arg is v as a SQLite statement argument: integers as int64 (an unsigned value above
// the int64 range as its text), doubles as float64, NULL as nil.
func Arg(d *Def, v Value) any {
	if v.Null {
		return nil
	}
	switch d.Kind {
	case KindInt, KindBool:
		if n, err := strconv.ParseInt(v.Text, 10, 64); err == nil {
			return n
		}
	case KindUint:
		if n, err := strconv.ParseUint(v.Text, 10, 64); err == nil && n <= math.MaxInt64 {
			return int64(n)
		}
	case KindDouble:
		if f, err := strconv.ParseFloat(v.Text, 64); err == nil {
			return f
		}
	}
	return v.Text
}

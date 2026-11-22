package vars

import (
	"testing"
	"time"
)

func newSession(t *testing.T, autocommit func() bool) *Session {
	t.Helper()
	at := time.Date(2026, 11, 22, 10, 0, 0, 123456000, time.FixedZone("CET", 3600))
	srv := NewServer(Config{
		Version: "8.0.36-mysqlite", MaxAllowedPacket: 64 << 20,
		DataDir: "/var/lib/mysqlite", Hostname: "host1", Now: func() time.Time { return at },
	})
	return srv.NewSession(Hooks{Autocommit: autocommit})
}

func TestMinimumSetIsRegistered(t *testing.T) {
	// docs/04-session-catalog.md section 1, minus what MySQL 8.0 removed.
	for _, name := range []string{
		"version", "version_comment", "version_compile_os", "protocol_version",
		"max_allowed_packet", "autocommit", "sql_mode", "character_set_client",
		"character_set_connection", "character_set_results", "character_set_server",
		"character_set_database", "collation_server", "collation_connection",
		"collation_database", "init_connect", "interactive_timeout", "wait_timeout",
		"net_write_timeout", "net_read_timeout", "net_buffer_length",
		"lower_case_table_names", "lower_case_file_system", "transaction_isolation",
		"transaction_read_only", "auto_increment_increment", "auto_increment_offset",
		"div_precision_increment", "group_concat_max_len", "sql_select_limit", "license",
		"lc_messages", "lc_time_names", "time_zone", "system_time_zone", "timestamp",
		"hostname", "datadir", "basedir", "socket", "performance_schema",
		"information_schema_stats_expiry", "default_storage_engine", "foreign_key_checks",
		"unique_checks", "sql_auto_is_null", "binlog_format", "gtid_mode",
		"explicit_defaults_for_timestamp", "optimizer_switch", "sql_require_primary_key",
		"connect_timeout", "thread_stack", "pid_file",
	} {
		if _, ok := Lookup(name); !ok {
			t.Errorf("%s is not registered", name)
		}
	}
	// Removed in MySQL 8.0: reading them is 1193 there.
	for _, name := range []string{"tx_isolation", "tx_read_only", "storage_engine", "query_cache_size", "query_cache_type", "have_query_cache"} {
		if _, ok := Lookup(name); ok {
			t.Errorf("%s was removed in MySQL 8.0 and must not be registered", name)
		}
	}
}

func TestGetValuesAndScopes(t *testing.T) {
	s := newSession(t, func() bool { return false })
	cases := []struct {
		name  string
		scope ReadScope
		want  string
	}{
		{"version", ReadDefault, "8.0.36-mysqlite"},
		{"VERSION", ReadGlobal, "8.0.36-mysqlite"},
		{"max_allowed_packet", ReadGlobal, "67108864"},
		{"max_allowed_packet", ReadSession, "67108864"},
		{"sql_mode", ReadSession, "ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION"},
		// The session's autocommit comes from the session layer, the global default stays 1.
		{"autocommit", ReadDefault, "0"},
		{"autocommit", ReadGlobal, "1"},
		{"datadir", ReadDefault, "/var/lib/mysqlite/"},
		{"hostname", ReadDefault, "host1"},
		{"system_time_zone", ReadDefault, "CET"},
		{"timestamp", ReadSession, "1795338000.123456"},
		{"sql_select_limit", ReadDefault, "18446744073709551615"},
	}
	for _, c := range cases {
		v, d, e := s.Get(c.name, c.scope)
		if e != nil {
			t.Errorf("%s: %v", c.name, e)
			continue
		}
		if v.Null || v.Text != c.want || d == nil {
			t.Errorf("%s (scope %d): want %q, got %+v", c.name, c.scope, c.want, v)
		}
	}
}

func TestGetErrors(t *testing.T) {
	s := newSession(t, nil)
	cases := []struct {
		name  string
		scope ReadScope
		code  uint16
		msg   string
	}{
		{"nope", ReadDefault, 1193, "Unknown system variable 'nope'"},
		{"tx_isolation", ReadDefault, 1193, "Unknown system variable 'tx_isolation'"},
		{"version", ReadSession, 1238, "Variable 'version' is a GLOBAL variable"},
		{"timestamp", ReadGlobal, 1238, "Variable 'timestamp' is a SESSION variable"},
	}
	for _, c := range cases {
		_, _, e := s.Get(c.name, c.scope)
		if e == nil || e.Code != c.code || e.State != "HY000" || e.Message != c.msg {
			t.Errorf("%s: want %d %q, got %v", c.name, c.code, c.msg, e)
		}
	}
}

func TestArg(t *testing.T) {
	cases := []struct {
		name string
		v    Value
		want any
	}{
		{"autocommit", Value{Text: "1"}, int64(1)},
		{"max_allowed_packet", Value{Text: "67108864"}, int64(67108864)},
		{"sql_select_limit", Value{Text: "18446744073709551615"}, "18446744073709551615"},
		{"timestamp", Value{Text: "1.5"}, 1.5},
		{"version", Value{Text: "8.0"}, "8.0"},
		{"character_set_results", Value{Null: true}, nil},
	}
	for _, c := range cases {
		d, _ := Lookup(c.name)
		if got := Arg(d, c.v); got != c.want {
			t.Errorf("%s: want %#v, got %#v", c.name, c.want, got)
		}
	}
}

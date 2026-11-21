package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mysqlite.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const minimal = `
users:
  - name: test
    password: test
`

func TestDefaultsApplyWhenOnlyUsersAreSet(t *testing.T) {
	cfg, err := Load([]string{"--config", writeFile(t, minimal)})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1" || cfg.Port != 3307 {
		t.Errorf("want 127.0.0.1:3307, got %s:%d", cfg.Listen, cfg.Port)
	}
	if cfg.ServerVersion != "8.0.36-mysqlite" {
		t.Errorf("want server version 8.0.36-mysqlite, got %q", cfg.ServerVersion)
	}
	if cfg.MaxAllowedPacket != 64<<20 {
		t.Errorf("want max_allowed_packet 64 MiB, got %d", cfg.MaxAllowedPacket)
	}
	if cfg.Log.Level != "info" || cfg.Log.MaxSizeMB <= 0 || cfg.Log.MaxFiles <= 0 || cfg.Log.Dir == "" {
		t.Errorf("unexpected log defaults: %+v", cfg.Log)
	}
	if len(cfg.Users) != 1 || cfg.Users[0].Name != "test" || cfg.Users[0].Password != "test" {
		t.Errorf("unexpected users: %+v", cfg.Users)
	}
	// docs/06-transactions.md sections 1, 5, 7.
	if cfg.MaxReadConns != 4 || cfg.BusyTimeoutMS != 5000 || cfg.Synchronous != "NORMAL" {
		t.Errorf("want max_read_conns 4, busy_timeout_ms 5000, synchronous NORMAL, got %d, %d, %q",
			cfg.MaxReadConns, cfg.BusyTimeoutMS, cfg.Synchronous)
	}
}

func TestEngineKeysFromFile(t *testing.T) {
	cfg, err := Load([]string{"--config", writeFile(t, minimal+"max_read_conns: 8\nbusy_timeout_ms: 250\nsynchronous: FULL\n")})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxReadConns != 8 || cfg.BusyTimeoutMS != 250 || cfg.Synchronous != "FULL" {
		t.Errorf("unexpected engine keys: %d, %d, %q", cfg.MaxReadConns, cfg.BusyTimeoutMS, cfg.Synchronous)
	}
}

func TestPrecedenceFlagsOverFileOverDefaults(t *testing.T) {
	path := writeFile(t, minimal+`
port: 4000
listen: 127.0.0.2
log:
  level: warn
`)
	cfg, err := Load([]string{"--config", path, "--port", "5000"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 5000 {
		t.Errorf("flag must win: want port 5000, got %d", cfg.Port)
	}
	if cfg.Listen != "127.0.0.2" {
		t.Errorf("file must win over default: want 127.0.0.2, got %s", cfg.Listen)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("file must win over default: want warn, got %s", cfg.Log.Level)
	}
}

func TestInvalidConfigurationsFailWithClearMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		args []string
		want string
	}{
		{"unknown key", minimal + "colour: blue\n", nil, "colour"},
		{"no users", "port: 3307\n", nil, "users"},
		{"empty user name", "users:\n  - name: ''\n    password: x\n", nil, "user name"},
		{"duplicate user", minimal + "  - name: test\n    password: other\n", nil, "duplicate user"},
		{"port out of range", minimal + "port: 70000\n", nil, "port"},
		{"bad listen address", minimal + "listen: localhost\n", nil, "listen"},
		{"version without suffix", minimal + "server_version: 8.0.36\n", nil, "-mysqlite"},
		{"packet too small", minimal + "max_allowed_packet: 10\n", nil, "max_allowed_packet"},
		{"packet too large", minimal + "max_allowed_packet: 2147483648\n", nil, "max_allowed_packet"},
		{"bad log level", minimal + "log:\n  level: loud\n", nil, "log.level"},
		{"bad log size", minimal + "log:\n  max_size_mb: 0\n", nil, "log.max_size_mb"},
		{"bad log files", minimal + "log:\n  max_files: 0\n", nil, "log.max_files"},
		{"remote bind not allowed", minimal + "listen: 0.0.0.0\n", nil, "allow_remote"},
		{"bad flag", minimal, []string{"--port", "abc"}, "port"},
		{"zero readers", minimal + "max_read_conns: 0\n", nil, "max_read_conns"},
		{"too many readers", minimal + "max_read_conns: 65\n", nil, "max_read_conns"},
		{"negative busy timeout", minimal + "busy_timeout_ms: -1\n", nil, "busy_timeout_ms"},
		{"bad synchronous", minimal + "synchronous: OFF\n", nil, "synchronous"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--config", writeFile(t, tc.body)}, tc.args...)
			_, err := Load(args)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error mentioning %q, got %q", tc.want, err)
			}
		})
	}
}

func TestMissingConfigFileIsError(t *testing.T) {
	_, err := Load([]string{"--config", filepath.Join(t.TempDir(), "absent.yaml")})
	if err == nil || !strings.Contains(err.Error(), "absent.yaml") {
		t.Fatalf("want error naming the file, got %v", err)
	}
}

func TestRemoteBindAllowedExplicitly(t *testing.T) {
	cfg, err := Load([]string{"--config", writeFile(t, minimal+"listen: 0.0.0.0\nallow_remote: true\n")})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.IsLoopback() {
		t.Fatal("0.0.0.0 must not be reported as loopback")
	}
}

func TestRedactedMasksPasswords(t *testing.T) {
	cfg, err := Load([]string{"--config", writeFile(t, minimal+"  - name: anon\n    password: ''\n")})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	red := cfg.Redacted()
	if red.Users[0].Password != Mask {
		t.Errorf("want masked password, got %q", red.Users[0].Password)
	}
	if red.Users[1].Password != "" {
		t.Errorf("an empty password stays empty, got %q", red.Users[1].Password)
	}
	if cfg.Users[0].Password != "test" {
		t.Error("Redacted must not modify the original configuration")
	}
}

func TestAddrAndUserMap(t *testing.T) {
	cfg, err := Load([]string{"--config", writeFile(t, minimal)})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr() != "127.0.0.1:3307" {
		t.Errorf("want 127.0.0.1:3307, got %s", cfg.Addr())
	}
	if got := cfg.UserPasswords(); got["test"] != "test" || len(got) != 1 {
		t.Errorf("unexpected user map: %v", got)
	}
}

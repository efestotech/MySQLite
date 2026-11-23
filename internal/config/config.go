// Package config loads the server configuration: defaults, then the YAML file, then
// command-line flags (docs/09-conventions.md section 2). No environment variables.
//
// M0 implements the keys the handshake and logging need. The remaining keys listed in
// docs/09-conventions.md are added with the milestone that gives them meaning; until
// then they are rejected as unknown, so a misspelled key never passes silently.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Mask replaces non-empty passwords in Redacted output.
const Mask = "***"

const (
	versionSuffix       = "-mysqlite"
	minAllowedPacket    = 1 << 10 // 1 KiB
	maxAllowedPacketCap = 1 << 30 // 1 GiB, the MySQL maximum
)

// User is one configured account. There is no privilege model (docs/00-brief.md).
type User struct {
	Name      string `yaml:"name"`
	Password  string `yaml:"password"`
	DefaultDB string `yaml:"default_db"`
}

// Log configures the server log and the query journal. Both rotate with the same size
// limit and number of files kept.
type Log struct {
	Level     string `yaml:"level"`
	Dir       string `yaml:"dir"`
	MaxSizeMB int    `yaml:"max_size_mb"`
	MaxFiles  int    `yaml:"max_files"`
	// Journal enables the query journal (docs/09-conventions.md section 3).
	Journal bool `yaml:"journal"`
}

// Config is the effective server configuration.
type Config struct {
	Listen           string `yaml:"listen"`
	Port             int    `yaml:"port"`
	DataDir          string `yaml:"data_dir"`
	Users            []User `yaml:"users"`
	ServerVersion    string `yaml:"server_version"`
	MaxAllowedPacket int    `yaml:"max_allowed_packet"`
	// AllowRemote must be true to bind a non-loopback address: there is no TLS and no
	// privilege model (docs/09-conventions.md section 8).
	AllowRemote bool `yaml:"allow_remote"`
	Log         Log  `yaml:"log"`

	// Engine keys (docs/06-transactions.md).
	MaxReadConns  int    `yaml:"max_read_conns"`
	BusyTimeoutMS int    `yaml:"busy_timeout_ms"`
	Synchronous   string `yaml:"synchronous"`

	// AlterBackup copies a database before an ALTER TABLE rebuilds one of its tables
	// (ADR 0008).
	AlterBackup bool `yaml:"alter_backup"`
}

// Defaults returns the configuration used for every key the file and flags leave unset.
func Defaults() Config {
	return Config{
		Listen:           "127.0.0.1",
		Port:             3307,
		DataDir:          "data",
		ServerVersion:    "8.0.36-mysqlite",
		MaxAllowedPacket: 64 << 20,
		MaxReadConns:     4,
		BusyTimeoutMS:    5000,
		Synchronous:      "NORMAL",
		AlterBackup:      true,
		Log: Log{
			Level:     "info",
			Dir:       "logs",
			MaxSizeMB: 100,
			MaxFiles:  10,
			Journal:   true,
		},
	}
}

// Load parses args (without the program name), reads the config file, and validates
// the result. Precedence: flags > config file > defaults.
func Load(args []string) (Config, error) {
	fs := flag.NewFlagSet("mysqlite", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("config", "mysqlite.yaml", "path to the YAML configuration file")
	listen := fs.String("listen", "", "listen address (overrides the file)")
	port := fs.String("port", "", "listen port (overrides the file)")
	dataDir := fs.String("data-dir", "", "data directory (overrides the file)")
	logLevel := fs.String("log-level", "", "log level: debug, info, warn, error (overrides the file)")
	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("config: unexpected argument %q", fs.Arg(0))
	}

	cfg := Defaults()
	if err := readFile(*path, &cfg); err != nil {
		return Config{}, err
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["listen"] {
		cfg.Listen = *listen
	}
	if set["port"] {
		p, err := strconv.Atoi(*port)
		if err != nil {
			return Config{}, fmt.Errorf("config: flag --port: %q is not a number", *port)
		}
		cfg.Port = p
	}
	if set["data-dir"] {
		cfg.DataDir = *dataDir
	}
	if set["log-level"] {
		cfg.Log.Level = *logLevel
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func readFile(path string, cfg *Config) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	return nil
}

// Validate checks every key and returns the first problem found.
func (c Config) Validate() error {
	ip := net.ParseIP(c.Listen)
	if ip == nil {
		return fmt.Errorf("config: listen %q is not an IP address", c.Listen)
	}
	if !ip.IsLoopback() && !c.AllowRemote {
		return fmt.Errorf("config: listen %q is not a loopback address; set allow_remote: true to accept remote clients", c.Listen)
	}
	// Port 0 asks the OS for a free port; tests rely on it.
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("config: port %d is out of range 0-65535", c.Port)
	}
	if c.DataDir == "" {
		return errors.New("config: data_dir must not be empty")
	}
	if !strings.HasSuffix(c.ServerVersion, versionSuffix) || len(c.ServerVersion) == len(versionSuffix) {
		return fmt.Errorf("config: server_version %q must end with %q (docs/03-protocol.md section 1)", c.ServerVersion, versionSuffix)
	}
	if c.MaxAllowedPacket < minAllowedPacket || c.MaxAllowedPacket > maxAllowedPacketCap {
		return fmt.Errorf("config: max_allowed_packet %d is out of range %d-%d", c.MaxAllowedPacket, minAllowedPacket, maxAllowedPacketCap)
	}
	if len(c.Users) == 0 {
		return errors.New("config: users must list at least one user")
	}
	seen := make(map[string]bool, len(c.Users))
	for i, u := range c.Users {
		if u.Name == "" {
			return fmt.Errorf("config: users[%d]: user name must not be empty", i)
		}
		if seen[u.Name] {
			return fmt.Errorf("config: duplicate user %q", u.Name)
		}
		seen[u.Name] = true
	}
	if c.MaxReadConns < 1 || c.MaxReadConns > 64 {
		return fmt.Errorf("config: max_read_conns %d is out of range 1-64", c.MaxReadConns)
	}
	if c.BusyTimeoutMS < 0 {
		return fmt.Errorf("config: busy_timeout_ms %d must not be negative", c.BusyTimeoutMS)
	}
	if c.Synchronous != "NORMAL" && c.Synchronous != "FULL" {
		return fmt.Errorf("config: synchronous %q must be NORMAL or FULL", c.Synchronous)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: log.level %q must be one of debug, info, warn, error", c.Log.Level)
	}
	if c.Log.Dir == "" {
		return errors.New("config: log.dir must not be empty")
	}
	if c.Log.MaxSizeMB < 1 {
		return fmt.Errorf("config: log.max_size_mb %d must be at least 1", c.Log.MaxSizeMB)
	}
	if c.Log.MaxFiles < 1 {
		return fmt.Errorf("config: log.max_files %d must be at least 1", c.Log.MaxFiles)
	}
	return nil
}

// Addr returns the host:port to listen on.
func (c Config) Addr() string {
	return net.JoinHostPort(c.Listen, strconv.Itoa(c.Port))
}

// IsLoopback reports whether the listen address only accepts local clients.
func (c Config) IsLoopback() bool {
	ip := net.ParseIP(c.Listen)
	return ip != nil && ip.IsLoopback()
}

// UserPasswords returns user name -> password for the wire layer.
func (c Config) UserPasswords() map[string]string {
	m := make(map[string]string, len(c.Users))
	for _, u := range c.Users {
		m[u.Name] = u.Password
	}
	return m
}

// Redacted returns a copy safe to print: non-empty passwords are replaced by Mask.
// Empty passwords stay empty, which tells the operator the account needs no password.
func (c Config) Redacted() Config {
	out := c
	out.Users = make([]User, len(c.Users))
	for i, u := range c.Users {
		if u.Password != "" {
			u.Password = Mask
		}
		out.Users[i] = u
	}
	return out
}

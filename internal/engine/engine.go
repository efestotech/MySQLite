// Package engine owns every SQLite handle (docs/01-architecture.md rule 4): no other
// package imports the driver. Callers get database/sql connections, so the driver can
// be swapped for a CGO one without touching them.
//
// Model (docs/06-transactions.md): one file per MySQL database, a read pool of
// query-only connections, and exactly one writer connection. Every physical connection
// is initialized by a connection hook, never once per pool.
package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/sqlite"
)

// fileExt is the extension of a database file in the data directory.
const fileExt = ".db"

var (
	// ErrUnknownDatabase means no file exists for the database name.
	ErrUnknownDatabase = errors.New("engine: unknown database")
	// ErrDatabaseExists means CreateDatabase found an existing file.
	ErrDatabaseExists = errors.New("engine: database exists")
	// ErrInvalidDatabaseName means the name cannot be mapped safely to a file name.
	ErrInvalidDatabaseName = errors.New("engine: invalid database name")
)

// validName restricts database names to characters that are safe in a file name on
// every platform. MySQL accepts more when the name is quoted; those names are rejected
// rather than escaped (documented in docs/agent-notes/contracts.md).
var validName = regexp.MustCompile(`^[A-Za-z0-9_$]{1,64}$`)

var synchronousLevels = map[string]int{"NORMAL": 1, "FULL": 2}

// Options configures the engine.
type Options struct {
	DataDir      string
	MaxReadConns int
	BusyTimeout  time.Duration
	// Synchronous is NORMAL or FULL (docs/06-transactions.md section 7).
	Synchronous string
}

func (o Options) validate() error {
	if o.DataDir == "" {
		return errors.New("engine: data dir is required")
	}
	// The driver strips everything after '?' from a plain file name.
	if strings.ContainsRune(o.DataDir, '?') {
		return fmt.Errorf("engine: data dir %q must not contain '?'", o.DataDir)
	}
	if o.MaxReadConns < 1 {
		return fmt.Errorf("engine: max read connections %d must be at least 1", o.MaxReadConns)
	}
	if o.BusyTimeout < 0 {
		return fmt.Errorf("engine: busy timeout %s must not be negative", o.BusyTimeout)
	}
	if _, ok := synchronousLevels[o.Synchronous]; !ok {
		return fmt.Errorf("engine: synchronous %q must be NORMAL or FULL", o.Synchronous)
	}
	return nil
}

// ScalarFunction is a SQL function made available on every connection.
type ScalarFunction struct {
	Name string
	// NumArgs is the number of arguments; -1 means variadic.
	NumArgs       int32
	Deterministic bool
	Fn            func(args []any) (any, error)
}

// Collation is a collating sequence made available on every connection.
type Collation struct {
	Name    string
	Compare func(a, b string) int
}

// Extensions lists the functions and collations installed on every connection.
type Extensions struct {
	Functions  []ScalarFunction
	Collations []Collation
}

// Engine manages the databases of one data directory.
type Engine struct {
	opts Options
	ext  Extensions

	mu     sync.Mutex
	dbs    map[string]*Database
	closed bool
	// scratch serves statements that read no table, opened on first use.
	scratch *sql.DB
	// ephemeral serves information_schema, opened on first use.
	ephemeral *sql.DB
}

// DataDir is the directory that holds the database files.
func (e *Engine) DataDir() string {
	return e.opts.DataDir
}

// Open validates opts and creates the data directory if needed.
func Open(opts Options, ext Extensions) (*Engine, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("engine: create data dir %s: %w", opts.DataDir, err)
	}
	return &Engine{opts: opts, ext: ext, dbs: make(map[string]*Database)}, nil
}

func (e *Engine) path(name string) (string, error) {
	if !validName.MatchString(name) {
		return "", fmt.Errorf("%w: %q", ErrInvalidDatabaseName, name)
	}
	return filepath.Join(e.opts.DataDir, name+fileExt), nil
}

// Databases returns the database names in the data directory, sorted.
func (e *Engine) Databases() ([]string, error) {
	entries, err := os.ReadDir(e.opts.DataDir)
	if err != nil {
		return nil, fmt.Errorf("engine: list data dir: %w", err)
	}
	var names []string
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), fileExt)
		if ok && entry.Type().IsRegular() && validName.MatchString(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// CreateDatabase creates an empty database file in WAL mode.
func (e *Engine) CreateDatabase(name string) error {
	path, err := e.path(name)
	if err != nil {
		return err
	}
	// O_EXCL makes the existence check and the creation one atomic step.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o640)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%w: %q", ErrDatabaseExists, name)
	}
	if err != nil {
		return fmt.Errorf("engine: create %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("engine: create %s: %w", path, err)
	}
	// Opening the writer converts the empty file to WAL.
	_, err = e.Database(name)
	return err
}

// Database returns the open database, opening it on first use. It never creates a file.
func (e *Engine) Database(name string) (*Database, error) {
	path, err := e.path(name)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errors.New("engine: closed")
	}
	if db, ok := e.dbs[name]; ok {
		return db, nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %q", ErrUnknownDatabase, name)
	}
	if err != nil {
		return nil, fmt.Errorf("engine: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %q is not a regular file", ErrUnknownDatabase, name)
	}
	db, err := openDatabase(name, path, e.opts, e.ext)
	if err != nil {
		return nil, err
	}
	e.dbs[name] = db
	return db, nil
}

// SQLiteVersion reports the embedded SQLite version.
func (e *Engine) SQLiteVersion(ctx context.Context) (string, error) {
	d, err := newDriver(e.ext, nil, nil)
	if err != nil {
		return "", err
	}
	db := sql.OpenDB(connector{driver: d, dsn: ":memory:"})
	defer db.Close()
	var v string
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&v); err != nil {
		return "", fmt.Errorf("engine: sqlite version: %w", err)
	}
	return v, nil
}

// Scratch returns a query-only connection to a private in-memory database, for
// statements that read no table (SELECT without FROM). It has the same functions and
// collations as every database connection. The caller must close it.
func (e *Engine) Scratch(ctx context.Context) (*sql.Conn, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, errors.New("engine: closed")
	}
	if e.scratch == nil {
		d, err := newDriver(e.ext, []string{"PRAGMA query_only=ON"}, nil)
		if err != nil {
			e.mu.Unlock()
			return nil, err
		}
		e.scratch = sql.OpenDB(connector{driver: d, dsn: ":memory:"})
		e.scratch.SetMaxOpenConns(e.opts.MaxReadConns)
		e.scratch.SetMaxIdleConns(e.opts.MaxReadConns)
	}
	db := e.scratch
	e.mu.Unlock()
	c, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: scratch connection: %w", err)
	}
	return c, nil
}

// Ephemeral returns a writable connection to a new, empty in-memory database with the
// same functions and collations as every database connection. The database is
// discarded when the connection is closed: the pool keeps no idle connection. mysqlite
// builds information_schema tables in it (ADR 0005). The caller must close it.
func (e *Engine) Ephemeral(ctx context.Context) (*sql.Conn, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, errors.New("engine: closed")
	}
	if e.ephemeral == nil {
		d, err := newDriver(e.ext, nil, nil)
		if err != nil {
			e.mu.Unlock()
			return nil, err
		}
		e.ephemeral = sql.OpenDB(connector{driver: d, dsn: ":memory:"})
		e.ephemeral.SetMaxOpenConns(e.opts.MaxReadConns)
		e.ephemeral.SetMaxIdleConns(0)
	}
	db := e.ephemeral
	e.mu.Unlock()
	c, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: ephemeral connection: %w", err)
	}
	return c, nil
}

// Close closes every open database.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	var errs []error
	for _, db := range e.dbs {
		errs = append(errs, db.close())
	}
	e.dbs = nil
	if e.ephemeral != nil {
		errs = append(errs, e.ephemeral.Close())
	}
	if e.scratch != nil {
		errs = append(errs, e.scratch.Close())
	}
	return errors.Join(errs...)
}

// Database is one SQLite file with its read pool and its single writer.
type Database struct {
	name  string
	read  *sql.DB
	write *sql.DB
	// opened counts physical connections initialized by the hook, for tests.
	opened atomic.Int64
}

func openDatabase(name, path string, opts Options, ext Extensions) (*Database, error) {
	db := &Database{name: name}
	common := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=" + opts.Synchronous,
		"PRAGMA busy_timeout=" + strconv.FormatInt(opts.BusyTimeout.Milliseconds(), 10),
		// SQLite ships with foreign keys off; InnoDB enforces them.
		"PRAGMA foreign_keys=ON",
	}

	writeDriver, err := newDriver(ext, common, &db.opened)
	if err != nil {
		return nil, err
	}
	// query_only guarantees a statement routed to the read pool can never write.
	readDriver, err := newDriver(ext, append(append([]string{}, common...), "PRAGMA query_only=ON"), &db.opened)
	if err != nil {
		return nil, err
	}

	db.write = sql.OpenDB(connector{driver: writeDriver, dsn: path})
	db.write.SetMaxOpenConns(1)
	db.write.SetMaxIdleConns(1)
	db.write.SetConnMaxLifetime(0)
	// Open the writer first: it converts a new file to WAL before readers attach.
	if err := db.write.Ping(); err != nil {
		_ = db.write.Close()
		return nil, fmt.Errorf("engine: open %s: %w", path, err)
	}

	db.read = sql.OpenDB(connector{driver: readDriver, dsn: path})
	db.read.SetMaxOpenConns(opts.MaxReadConns)
	db.read.SetMaxIdleConns(opts.MaxReadConns)
	if err := db.read.Ping(); err != nil {
		return nil, errors.Join(fmt.Errorf("engine: open %s: %w", path, err), db.close())
	}
	return db, nil
}

// Name returns the MySQL database name.
func (db *Database) Name() string { return db.name }

// Read returns a pooled query-only connection. The caller must close it.
func (db *Database) Read(ctx context.Context) (*sql.Conn, error) {
	c, err := db.read.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: read connection: %w", err)
	}
	return c, nil
}

// Writer returns the single writer connection, waiting while another session holds
// it. Connection-scoped state (last_insert_rowid, changes, transactions) stays valid
// until the caller closes it (docs/06-transactions.md section 3).
func (db *Database) Writer(ctx context.Context) (*sql.Conn, error) {
	c, err := db.write.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: writer connection: %w", err)
	}
	return c, nil
}

func (db *Database) close() error {
	var errs []error
	if db.read != nil {
		errs = append(errs, db.read.Close())
	}
	if db.write != nil {
		errs = append(errs, db.write.Close())
	}
	return errors.Join(errs...)
}

// newDriver builds a private driver: its extensions and hook apply only to the
// connections it opens, never to the package-level "sqlite" driver.
func newDriver(ext Extensions, pragmas []string, opened *atomic.Int64) (*sqlite.Driver, error) {
	d := &sqlite.Driver{}
	for _, f := range ext.Functions {
		fn := f.Fn
		impl := func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			in := make([]any, len(args))
			for i, a := range args {
				in[i] = a
			}
			return fn(in)
		}
		register := d.RegisterScalarFunction
		if f.Deterministic {
			register = d.RegisterDeterministicScalarFunction
		}
		if err := register(f.Name, f.NumArgs, impl); err != nil {
			return nil, fmt.Errorf("engine: register function %s: %w", f.Name, err)
		}
	}
	for _, c := range ext.Collations {
		if err := d.RegisterCollationUtf8(c.Name, c.Compare); err != nil {
			return nil, fmt.Errorf("engine: register collation %s: %w", c.Name, err)
		}
	}
	if len(pragmas) > 0 {
		d.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
			for _, p := range pragmas {
				if _, err := conn.ExecContext(context.Background(), p, nil); err != nil {
					return fmt.Errorf("%s: %w", p, err)
				}
			}
			if opened != nil {
				opened.Add(1)
			}
			return nil
		})
	}
	return d, nil
}

// connector opens connections through a private driver; sqlite.NewConnector only
// serves the package-level driver.
type connector struct {
	driver *sqlite.Driver
	dsn    string
}

func (c connector) Connect(context.Context) (driver.Conn, error) { return c.driver.Open(c.dsn) }
func (c connector) Driver() driver.Driver                        { return c.driver }

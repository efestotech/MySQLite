package obs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

	"mysqlite/internal/config"
)

const journalName = "journal.jsonl"

// Outcomes of a journal entry.
const (
	OutcomeOK    = "ok"
	OutcomeError = "error"
)

// Entry is one statement received from a client (docs/09-conventions.md section 3).
// The journal feeds the corpus (docs/07-testing.md section 5), so literals are kept.
type Entry struct {
	Time     time.Time
	Session  uint32 // connection id
	User     string
	Database string // current database when the statement arrived; "" for none
	SQL      string // as received
	// Kind is the router decision: read, write, use, transaction; "" when the statement
	// was not translated.
	Kind     string
	Rules    []string // rule IDs applied, registry order
	Emitted  string   // SQL sent to SQLite
	Params   []any    // arguments bound to Emitted
	Duration time.Duration
	Outcome  string // OutcomeOK or OutcomeError
	Rows     uint64 // rows returned by a read
	Affected uint64 // affected rows of a write
	// The error sent to the client.
	ErrorCode uint16
	SQLState  string
	Error     string
	// Unsupported is the construct name of a 1235 error, for the roadmap report.
	Unsupported string
	// DecimalAsReal marks a statement whose DECIMAL arithmetic ran as REAL (ADR 0006).
	DecimalAsReal bool
}

type record struct {
	Time        string   `json:"ts"`
	Session     uint32   `json:"session"`
	User        string   `json:"user"`
	Database    string   `json:"database,omitempty"`
	SQL         string   `json:"sql"`
	Kind        string   `json:"kind,omitempty"`
	Rules       []string `json:"rules,omitempty"`
	Emitted     string   `json:"emitted,omitempty"`
	Params      []any    `json:"params,omitempty"`
	DurationUS  int64    `json:"duration_us"`
	Outcome     string   `json:"outcome"`
	Rows        uint64   `json:"rows"`
	Affected    uint64   `json:"affected"`
	ErrorCode   uint16   `json:"error_code,omitempty"`
	SQLState    string   `json:"sqlstate,omitempty"`
	Error       string   `json:"error,omitempty"`
	Unsupported string   `json:"unsupported,omitempty"`
	DecimalReal bool     `json:"decimal_as_real,omitempty"`
}

// Journal is the merged query journal: JSON Lines in <dir>/journal.jsonl, rotated by
// size like the server log. It is safe for concurrent use.
type Journal struct {
	mu   sync.Mutex
	file *lumberjack.Logger // nil when disabled
}

// NewJournal opens the journal configured by cfg. With cfg.Journal false, Record does
// nothing and no file is created.
func NewJournal(cfg config.Log) (*Journal, error) {
	if !cfg.Journal {
		return &Journal{}, nil
	}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("obs: create log dir %s: %w", cfg.Dir, err)
	}
	return &Journal{file: &lumberjack.Logger{
		Filename:   filepath.Join(cfg.Dir, journalName),
		MaxSize:    cfg.MaxSizeMB,
		MaxBackups: cfg.MaxFiles,
	}}, nil
}

// Record appends e as one line. lumberjack rotates before a write that would exceed
// the size limit, so a line is never split across files.
func (j *Journal) Record(e Entry) error {
	if j == nil || j.file == nil {
		return nil
	}
	line, err := json.Marshal(record{
		Time:        e.Time.UTC().Format(time.RFC3339Nano),
		Session:     e.Session,
		User:        e.User,
		Database:    e.Database,
		SQL:         e.SQL,
		Kind:        e.Kind,
		Rules:       e.Rules,
		Emitted:     e.Emitted,
		Params:      e.Params,
		DurationUS:  e.Duration.Microseconds(),
		Outcome:     e.Outcome,
		Rows:        e.Rows,
		Affected:    e.Affected,
		ErrorCode:   e.ErrorCode,
		SQLState:    e.SQLState,
		Error:       e.Error,
		DecimalReal: e.DecimalAsReal,
		Unsupported: e.Unsupported,
	})
	if err != nil {
		return fmt.Errorf("obs: encode journal entry: %w", err)
	}
	line = append(line, '\n')
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.file.Write(line); err != nil {
		return fmt.Errorf("obs: write journal: %w", err)
	}
	return nil
}

// Close flushes and closes the journal file.
func (j *Journal) Close() error {
	if j == nil || j.file == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.file.Close()
}

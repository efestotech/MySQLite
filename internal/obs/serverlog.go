// Package obs holds the structured log streams (docs/09-conventions.md section 3).
// M0 provides the server log; the query journal arrives with M1.
package obs

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"

	"mysqlite/internal/config"
)

const serverLogName = "server.log"

// NewServerLogger returns a JSON Lines logger that writes to <dir>/server.log with
// size-based rotation, keeping at most cfg.MaxFiles rotated files, and mirrors every
// record to console. The closer flushes and closes the log file.
func NewServerLogger(cfg config.Log, console io.Writer) (*slog.Logger, io.Closer, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		return nil, nil, fmt.Errorf("obs: log level %q: %w", cfg.Level, err)
	}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, nil, fmt.Errorf("obs: create log dir %s: %w", cfg.Dir, err)
	}
	file := &lumberjack.Logger{
		Filename:   filepath.Join(cfg.Dir, serverLogName),
		MaxSize:    cfg.MaxSizeMB,
		MaxBackups: cfg.MaxFiles,
	}
	handler := slog.NewJSONHandler(io.MultiWriter(file, console), &slog.HandlerOptions{Level: level})
	return slog.New(handler), file, nil
}

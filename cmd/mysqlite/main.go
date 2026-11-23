// Command mysqlite is a MySQL protocol server backed by SQLite.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"mysqlite/internal/config"
	"mysqlite/internal/dialect/udf"
	"mysqlite/internal/engine"
	"mysqlite/internal/obs"
	"mysqlite/internal/wire"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "mysqlite:", err)
		os.Exit(1)
	}
}

// run starts the server and blocks until ctx is cancelled. onListen, when not nil,
// receives the bound address once the listener is ready.
func run(ctx context.Context, args []string, console io.Writer, onListen func(net.Addr)) (err error) {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}

	log, closer, err := obs.NewServerLogger(cfg.Log, console)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := closer.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close server log: %w", cerr)
		}
	}()

	journal, err := obs.NewJournal(cfg.Log)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := journal.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close query journal: %w", cerr)
		}
	}()

	log.Info("effective configuration", "config", cfg.Redacted())
	if !cfg.IsLoopback() {
		log.Warn("listening on a non-loopback address: no TLS, no privilege model, single user",
			"listen", cfg.Listen)
	}

	eng, err := engine.Open(engine.Options{
		DataDir:         cfg.DataDir,
		MaxReadConns:    cfg.MaxReadConns,
		BusyTimeout:     time.Duration(cfg.BusyTimeoutMS) * time.Millisecond,
		Synchronous:     cfg.Synchronous,
		WriteQueueDepth: cfg.WriteQueueDepth,
	}, udf.Extensions())
	if err != nil {
		return err
	}
	defer func() {
		if cerr := eng.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close engine: %w", cerr)
		}
	}()
	sqliteVersion, err := eng.SQLiteVersion(ctx)
	if err != nil {
		return err
	}
	log.Info("sqlite engine ready", "sqlite_version", sqliteVersion, "data_dir", cfg.DataDir)

	srv, err := wire.NewServer(wire.Config{
		ServerVersion:    cfg.ServerVersion,
		MaxAllowedPacket: cfg.MaxAllowedPacket,
		Users:            cfg.UserPasswords(),
		Engine:           eng,
		Journal:          journal,
		SkipAlterBackup:  !cfg.AlterBackup,
		ReadOnly:         cfg.ReadOnly,
		IdleTxTimeout:    time.Duration(cfg.IdleTxTimeoutS) * time.Second,
	}, log)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Addr(), err)
	}
	if onListen != nil {
		onListen(ln.Addr())
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
		if err := srv.Close(); err != nil {
			return err
		}
		return <-serveErr
	case err := <-serveErr:
		closeErr := srv.Close()
		return errors.Join(err, closeErr)
	}
}

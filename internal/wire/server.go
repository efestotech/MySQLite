// Package wire speaks the MySQL classic protocol on top of go-mysql's server package.
//
// Library behaviour this package depends on is recorded in
// docs/agent-notes/api-go-mysql.md; deviations from docs/03-protocol.md are in
// docs/adr/0003-go-mysql-capability-gaps.md.
package wire

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"

	"mysqlite/internal/catalog"
	"mysqlite/internal/dialect"
	"mysqlite/internal/engine"
	"mysqlite/internal/obs"
	"mysqlite/internal/session"
	"mysqlite/internal/vars"
)

// collationUTF8MB4GeneralCI is the default collation id (docs/00-brief.md section 6).
const collationUTF8MB4GeneralCI = 45

// Config holds what the wire layer needs from the server configuration.
type Config struct {
	ServerVersion    string
	MaxAllowedPacket int
	// Users maps user name to plaintext password. An empty password is allowed.
	Users map[string]string
	// Engine serves the databases; each connection gets its own session on it.
	Engine *engine.Engine
	// Journal receives one entry per COM_QUERY; nil journals nothing.
	Journal *obs.Journal
}

// Server accepts MySQL client connections.
type Server struct {
	cfg     Config
	version int // ServerVersion as a number, for /*!NNNNN */ comments
	log     *slog.Logger
	conf    *server.Server
	auth    *authHandler
	vars    *vars.Server
	catalog *catalog.Cache
	procs   *processList

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// NewServer validates cfg and builds a server. It does not listen yet.
func NewServer(cfg Config, log *slog.Logger) (*Server, error) {
	if cfg.ServerVersion == "" {
		return nil, errors.New("wire: server version is required")
	}
	if cfg.MaxAllowedPacket <= 0 {
		return nil, errors.New("wire: max_allowed_packet must be positive")
	}
	if len(cfg.Users) == 0 {
		return nil, errors.New("wire: at least one user is required")
	}
	if cfg.Engine == nil {
		return nil, errors.New("wire: engine is required")
	}
	version, err := dialect.VersionNumber(cfg.ServerVersion)
	if err != nil {
		return nil, fmt.Errorf("wire: %w", err)
	}

	auth := newAuthHandler(cfg.Users)
	conf := server.NewServerWithAuth(cfg.ServerVersion, collationUTF8MB4GeneralCI,
		mysql.AUTH_NATIVE_PASSWORD, nil, nil, nativeOnlyProvider{users: auth})
	// Multi-result support arrives with M4/M5 (docs/03-protocol.md section 2).
	if err := conf.UnsetCapability(mysql.CLIENT_MULTI_RESULTS | mysql.CLIENT_PS_MULTI_RESULTS); err != nil {
		return nil, fmt.Errorf("wire: unset multi-result capabilities: %w", err)
	}

	hostname, _ := os.Hostname()
	baseDir := ""
	if exe, err := os.Executable(); err == nil {
		baseDir = filepath.Dir(exe)
	}
	dataDir := cfg.Engine.DataDir()
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}

	return &Server{
		catalog: catalog.NewCache(),
		procs:   newProcessList(),
		vars: vars.NewServer(vars.Config{
			Version:          cfg.ServerVersion,
			MaxAllowedPacket: cfg.MaxAllowedPacket,
			DataDir:          dataDir,
			BaseDir:          baseDir,
			Hostname:         hostname,
		}),
		cfg:     cfg,
		version: version,
		log:     log,
		conf:    conf,
		auth:    auth,
		conns:   make(map[net.Conn]struct{}),
	}, nil
}

// Capability returns the capability flags advertised in the initial handshake.
func (s *Server) Capability() uint32 {
	return s.conf.Capability()
}

// Serve accepts connections on ln until Close is called. It returns nil after Close.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("wire: server closed")
	}
	s.ln = ln
	s.mu.Unlock()

	s.log.Info("listening", "addr", ln.Addr().String())
	for {
		raw, err := ln.Accept()
		if err != nil {
			if s.isClosed() {
				s.wg.Wait()
				return nil
			}
			return fmt.Errorf("wire: accept: %w", err)
		}
		if !s.track(raw) {
			_ = raw.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.untrack(raw)
			s.serveConn(raw)
		}()
	}
}

// Close stops accepting, closes every open connection, and waits for them to finish.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ln := s.ln
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()

	if ln == nil {
		return nil
	}
	if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("wire: close listener: %w", err)
	}
	return nil
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	_ = c.Close()
}

func (s *Server) serveConn(raw net.Conn) {
	remote := raw.RemoteAddr().String()
	// A panic inside go-mysql must cost one connection, not the whole server
	// (see W5 in docs/agent-notes/api-go-mysql.md for one that was found).
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("connection panic", "remote", remote, "panic", fmt.Sprint(r))
		}
	}()
	tr := dialect.New(dialect.Config{ServerVersion: s.version})
	h := newHandler(s.cfg.Engine, session.New(s.cfg.Engine), tr, s.log.With("remote", remote))
	h.journal = s.cfg.Journal
	h.version = s.cfg.ServerVersion
	h.catalog = s.catalog
	h.procs = s.procs
	h.users = s.cfg.Users
	h.vars = s.vars.NewSession(vars.Hooks{Autocommit: h.sess.Autocommit, Diagnostics: h.diagCounts})
	h.applyGlobals()
	h.filter = &responseFilter{Conn: newPacketGuard(raw, s.cfg.MaxAllowedPacket)}
	guarded := hostOnlyConn{h.filter}
	conn, err := s.conf.NewCustomizedConn(guarded, connAuth{authHandler: s.auth, h: h}, h)
	if err != nil {
		s.log.Info("handshake failed", "remote", remote, "error", err.Error())
		return
	}
	log := s.log.With("conn_id", conn.ConnectionID(), "user", conn.GetUser(), "remote", remote)
	h.log = log
	log.Info("connection opened")
	db, _ := h.sess.Database()
	h.proc = s.procs.add(conn.ConnectionID(), conn.GetUser(), remote, db)
	defer s.procs.remove(h.proc)
	defer h.close()
	for !conn.Closed() {
		if err := conn.HandleCommand(); err != nil {
			if !conn.Closed() || !errors.Is(err, net.ErrClosed) {
				log.Debug("command loop ended", "error", err.Error())
			}
			break
		}
	}
	log.Info("connection closed")
}

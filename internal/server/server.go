// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
)

const (
	// ServerName defines the identifier reported in health probes and model ownership metadata.
	ServerName = "antigravity-gateway"

	// DefaultReadTimeout defines the maximum duration for reading request headers and bodies.
	DefaultReadTimeout = 60 * time.Second

	// DefaultIdleTimeout defines the maximum amount of time to wait for the next request when keep-alives are enabled.
	DefaultIdleTimeout = 120 * time.Second

	// MaxRequestBodyBytes defines the maximum permissible incoming HTTP request body size (64 MB).
	MaxRequestBodyBytes = 64 * 1024 * 1024

	// EmptyStreamRetries defines how many times to retry an upstream request if Google returns an empty body.
	EmptyStreamRetries = 2
)

// Server coordinates the local HTTP reverse proxy daemon, request routing, and upstream dispatching.
type Server struct {
	// mu guards mutable daemon lifecycle states.
	mu sync.RWMutex

	// cfg holds an immutable snapshot of application-level settings. Runtime changes replace the
	// pointer atomically (copy-on-write), so handlers always observe a fully consistent struct.
	cfg atomic.Pointer[config.Config]

	// rotator coordinates multi-account execution and failover.
	rotator *rotator.Rotator

	// catalog manages the dynamic model catalog and caching.
	catalog *catalog.ModelCatalog

	// logger is the structured logger instance.
	logger *slog.Logger

	// httpServer is the underlying net/http Server.
	httpServer *http.Server

	// strategyMu serializes runtime rotation strategy updates and config disk writes.
	strategyMu sync.Mutex

	// legacySchemas tracks whether upstream rejected parametersJsonSchema, triggering legacy fallback.
	legacySchemas atomic.Bool

	// listener is the bound network listener.
	listener net.Listener

	// pidFile is the optional path where the daemon records its own process ID once listening.
	pidFile string

	// configPath is the configuration file the daemon was started with. Runtime settings changed
	// through the WebUI are persisted here instead of always rewriting the default location.
	configPath string

	// recipeCache replays recent thought signatures for sessions whose client stripped
	// reasoning_details between turns.
	recipeCache *recipeCache
}

// Options encapsulates configuration parameters for initializing a Server instance.
type Options struct {
	// Config is the mandatory application configuration pointer.
	Config *config.Config

	// Rotator is the mandatory multi-account request dispatcher.
	Rotator *rotator.Rotator

	// Catalog is the mandatory model catalog and quota tracker.
	Catalog *catalog.ModelCatalog

	// Logger is the optional structured slog logger.
	Logger *slog.Logger

	// Listener is an optional pre-bound network listener (useful for tests).
	Listener net.Listener

	// PIDFile is an optional path where the daemon records its own process ID once it reaches the
	// listening state. When empty, no PID file is managed (useful for tests and embedded usage).
	PIDFile string

	// ConfigPath is the optional configuration file path the daemon was started with. When empty, the
	// default configuration path is resolved lazily on persistence (useful for tests).
	ConfigPath string
}

// NewServer initializes a new Server with configured routes, middleware, and loopback enforcement.
func NewServer(opts Options) (*Server, error) {
	if opts.Config == nil {
		return nil, errors.New("server options require non-nil Config")
	}
	if opts.Rotator == nil {
		return nil, errors.New("server options require non-nil Rotator")
	}
	if opts.Catalog == nil {
		return nil, errors.New("server options require non-nil Catalog")
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{
		rotator:     opts.Rotator,
		catalog:     opts.Catalog,
		logger:      logger,
		listener:    opts.Listener,
		pidFile:     opts.PIDFile,
		configPath:  opts.ConfigPath,
		recipeCache: newRecipeCache(time.Now),
	}
	// Clone so later mutation of the caller's configuration cannot change what handlers observe.
	s.cfg.Store(opts.Config.Clone())

	handler := s.buildHandler()

	// Enforce strict local loopback binding address
	cfg := s.config()
	host := config.LoopbackHost(cfg.Host)
	port := cfg.Port
	if port <= 0 {
		port = config.DefaultPort
	}
	addr := config.ListenAddress(host, port)

	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       DefaultReadTimeout,
		IdleTimeout:       DefaultIdleTimeout,
	}

	return s, nil
}

// config returns the current immutable configuration snapshot.
func (s *Server) config() *config.Config {
	return s.cfg.Load()
}

// emptyStreamRetryCount returns the configured empty-stream retry budget, falling back to the
// package default when no configuration is attached.
func (s *Server) emptyStreamRetryCount() int {
	if cfg := s.config(); cfg != nil {
		return cfg.EmptyStreamRetryCount()
	}
	return EmptyStreamRetries
}

// setConfig applies mutate to a deep copy of the current configuration and publishes the result
// atomically, so no handler can ever observe a partially mutated struct. The copy also keeps the
// caller-owned configuration passed to NewServer untouched.
func (s *Server) setConfig(mutate func(*config.Config)) {
	current := s.cfg.Load()
	if current == nil || mutate == nil {
		return
	}

	updated := current.Clone()
	mutate(updated)
	s.cfg.Store(updated)
}

// Addr returns the configured or actively listening network address of the server.
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	if s.httpServer != nil {
		return s.httpServer.Addr
	}
	return ""
}

// Start initiates listening and serving incoming HTTP requests.
// This call blocks until the server is shut down or encounters an unrecoverable listener error.
func (s *Server) Start() error {
	s.logger.Info("starting gateway server", "addr", s.Addr(), "version", config.Version)

	// Surface the active configuration file and any API-key shape anomaly before serving: a key
	// that does not match the generated format makes every client request fail with HTTP 401,
	// which is hard to diagnose from the client side alone.
	if cfgPath, err := config.ConfigFilePath(); err == nil {
		s.logger.Info("active configuration file", "path", cfgPath, "strategy", s.rotator.Strategy())
	}
	if err := config.ValidateAPIKey(s.config().APIKey); err != nil {
		s.logger.Warn("gateway api key is not in the generated format; clients holding a different key will receive HTTP 401",
			"reason", err, "hint", "run 'agw setup <client>' with the key stored in config.json")
	}

	s.mu.RLock()
	ln := s.listener
	s.mu.RUnlock()

	if ln == nil {
		createdLn, err := net.Listen("tcp", s.httpServer.Addr)
		if err != nil {
			return s.bindError(err)
		}

		s.mu.Lock()
		s.listener = createdLn
		s.mu.Unlock()
		ln = createdLn
	}

	// The PID file is written only after the listener is bound, so a failed bind can never leave a
	// stale PID file behind for a daemon that never reached the listening state.
	if err := s.recordPID(); err != nil {
		s.logger.Warn("failed recording daemon pid file", "path", s.pidFile, "error", err)
	}

	if err := s.httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving http requests: %w", err)
	}

	return nil
}

// bindError decorates a listener failure with the process currently holding the port and the exact
// command that releases it, so an operator does not have to hunt the owning PID. Port ownership
// lookups are best-effort: the original bind error is always preserved for errors.Is/As callers.
func (s *Server) bindError(err error) error {
	addr := s.httpServer.Addr
	if !isAddrInUse(err) {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}

	_, portText, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		return fmt.Errorf("listening on %s: port is already in use; run 'agw stop' or start with a different --port: %w", addr, err)
	}

	port, convErr := strconv.Atoi(portText)
	if convErr != nil {
		return fmt.Errorf("listening on %s: port is already in use; run 'agw stop' or start with a different --port: %w", addr, err)
	}

	if pid, pidErr := processListeningOnPort(port); pidErr == nil {
		return fmt.Errorf("listening on %s: port is already in use by pid %d; run 'agw stop' to terminate a running gateway daemon or start with a different --port: %w", addr, pid, err)
	}

	return fmt.Errorf("listening on %s: port is already in use; run 'agw stop' to terminate a running gateway daemon or start with a different --port: %w", addr, err)
}

// recordPID writes the current process ID to the configured PID file so an external `agw stop` can
// find a daemon that was started without --detach, and so the file always describes a daemon that
// actually reached the listening state.
func (s *Server) recordPID() error {
	if s.pidFile == "" {
		return nil
	}
	return WritePID(s.pidFile, os.Getpid())
}

// clearPID removes the PID file on shutdown, but only while it still points at this process, so a
// newer daemon that already reused the path is never disowned.
func (s *Server) clearPID() {
	if s.pidFile == "" {
		return
	}

	pid, err := ReadPID(s.pidFile)
	if err != nil || pid != os.Getpid() {
		return
	}

	if err := ClearPID(s.pidFile); err != nil {
		s.logger.Warn("failed removing daemon pid file", "path", s.pidFile, "error", err)
	}
}

// Shutdown gracefully terminates the HTTP server, allowing active connections and streams to complete.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("shutting down gateway server")
	defer s.clearPID()

	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

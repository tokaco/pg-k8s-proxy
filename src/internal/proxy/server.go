// Package proxy implements the PostgreSQL gateway data plane: it reads the
// startup message of each incoming connection, looks the requested database up
// in the routing table, and splices the connection through to that backend.
package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tokaco/pg-k8s-proxy/internal/registry"
)

// Defaults applied when a Config field is left at its zero value.
const (
	DefaultStartupTimeout      = 30 * time.Second
	DefaultDialTimeout         = 5 * time.Second
	DefaultShutdownGracePeriod = 30 * time.Second
	DefaultKeepAlivePeriod     = 30 * time.Second
)

// ServerTLS configures the client-facing side of the gateway.
//
// TLS cannot be passed through to the backend: the gateway has to read the
// startup message to learn which database a client wants, and that message is
// inside the encrypted stream. So TLS is terminated here and, if the route asks
// for it, re-originated towards the backend.
type ServerTLS struct {
	// Config is the server-side TLS configuration. Required when Mode is not Disable.
	Config *tls.Config
	// Required rejects clients that decline to negotiate TLS.
	Required bool
}

// Config configures a Server.
type Config struct {
	// ListenAddress is the address the gateway accepts PostgreSQL clients on.
	ListenAddress string
	// Routes supplies the current routing table. Required.
	Routes *registry.Store
	// TLS terminates client connections. Nil disables TLS.
	TLS *ServerTLS
	// MaxConnections caps concurrent client sessions. Zero means unlimited.
	MaxConnections int
	// StartupTimeout bounds the handshake, from accept to forwarded startup packet.
	StartupTimeout time.Duration
	// DialTimeout bounds establishing the backend connection.
	DialTimeout time.Duration
	// ShutdownGracePeriod is how long established sessions may run after the
	// listener stops before they are closed.
	ShutdownGracePeriod time.Duration
	// KeepAlivePeriod sets TCP keepalives, so half-open sessions are reaped.
	KeepAlivePeriod time.Duration
	// Logger receives connection-level logs. Defaults to slog.Default.
	Logger *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.StartupTimeout <= 0 {
		c.StartupTimeout = DefaultStartupTimeout
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = DefaultDialTimeout
	}
	if c.ShutdownGracePeriod <= 0 {
		c.ShutdownGracePeriod = DefaultShutdownGracePeriod
	}
	if c.KeepAlivePeriod <= 0 {
		c.KeepAlivePeriod = DefaultKeepAlivePeriod
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Server accepts PostgreSQL clients and proxies them to their routed backend.
// It satisfies controller-runtime's manager.Runnable, so it shuts down together
// with the rest of the process.
type Server struct {
	cfg     Config
	log     *slog.Logger
	cancels *cancelRegistry

	// slots limits concurrent sessions; nil when MaxConnections is zero.
	slots chan struct{}

	listening atomic.Bool
	addr      atomic.Pointer[net.TCPAddr]

	sessions   sync.WaitGroup
	mu         sync.Mutex
	activeConn map[net.Conn]struct{}
}

// New validates cfg and returns a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Routes == nil {
		return nil, errors.New("proxy: Config.Routes is required")
	}
	if cfg.ListenAddress == "" {
		return nil, errors.New("proxy: Config.ListenAddress is required")
	}
	if cfg.TLS != nil && cfg.TLS.Config == nil {
		return nil, errors.New("proxy: Config.TLS.Config is required when TLS is enabled")
	}
	if cfg.MaxConnections < 0 {
		return nil, fmt.Errorf("proxy: MaxConnections must not be negative, got %d", cfg.MaxConnections)
	}
	cfg.applyDefaults()

	s := &Server{
		cfg:        cfg,
		log:        cfg.Logger,
		cancels:    newCancelRegistry(),
		activeConn: make(map[net.Conn]struct{}),
	}
	if cfg.MaxConnections > 0 {
		s.slots = make(chan struct{}, cfg.MaxConnections)
	}
	return s, nil
}

// NeedLeaderElection reports false: every replica serves traffic, only the
// control-plane reconcilers are leader-gated.
func (s *Server) NeedLeaderElection() bool { return false }

// Start listens and serves until ctx is cancelled. It returns once the listener
// is closed and either all sessions have finished or the grace period expired.
func (s *Server) Start(ctx context.Context) error {
	lc := net.ListenConfig{KeepAlive: s.cfg.KeepAlivePeriod}
	listener, err := lc.Listen(ctx, "tcp", s.cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("proxy: listening on %s: %w", s.cfg.ListenAddress, err)
	}

	if tcpAddr, ok := listener.Addr().(*net.TCPAddr); ok {
		s.addr.Store(tcpAddr)
	}
	s.listening.Store(true)
	defer s.listening.Store(false)

	s.log.Info("postgresql gateway listening",
		"address", listener.Addr().String(),
		"tls", s.cfg.TLS != nil,
		"maxConnections", s.cfg.MaxConnections,
	)

	// Unblock Accept when the manager shuts us down.
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	acceptErr := s.acceptLoop(ctx, listener)
	s.drain()
	return acceptErr
}

func (s *Server) acceptLoop(ctx context.Context, listener net.Listener) error {
	var backoff time.Duration
	for {
		conn, err := listener.Accept()
		if err != nil {
			// Shutdown closes the listener out from under Accept, so this
			// error is the normal exit path rather than a failure.
			if ctx.Err() != nil {
				s.log.Info("gateway listener closed, draining sessions")
				return nil //nolint:nilerr // the accept error is the listener we closed ourselves
			}
			// A temporary accept failure — file descriptor exhaustion is the
			// usual cause — should not take the listener down permanently.
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				backoff = nextBackoff(backoff)
				s.log.Warn("temporary accept failure, retrying", "error", err, "retryIn", backoff)
				select {
				case <-time.After(backoff):
					continue
				case <-ctx.Done():
					return nil
				}
			}
			return fmt.Errorf("proxy: accepting connections: %w", err)
		}
		backoff = 0

		s.sessions.Add(1)
		go func() {
			defer s.sessions.Done()
			s.serve(ctx, conn)
		}()
	}
}

func nextBackoff(current time.Duration) time.Duration {
	const (
		minBackoff = 5 * time.Millisecond
		maxBackoff = time.Second
	)
	if current == 0 {
		return minBackoff
	}
	if doubled := current * 2; doubled < maxBackoff {
		return doubled
	}
	return maxBackoff
}

// drain waits for in-flight sessions, then force-closes whatever is left.
func (s *Server) drain() {
	done := make(chan struct{})
	go func() {
		s.sessions.Wait()
		close(done)
	}()

	select {
	case <-done:
		return
	case <-time.After(s.cfg.ShutdownGracePeriod):
	}

	s.mu.Lock()
	remaining := len(s.activeConn)
	for conn := range s.activeConn {
		_ = conn.Close()
	}
	s.mu.Unlock()

	if remaining > 0 {
		s.log.Warn("grace period expired, closing sessions still in flight", "sessions", remaining)
	}
	<-done
}

func (s *Server) track(conn net.Conn) {
	s.mu.Lock()
	s.activeConn[conn] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) untrack(conn net.Conn) {
	s.mu.Lock()
	delete(s.activeConn, conn)
	s.mu.Unlock()
}

// Addr returns the address the gateway is bound to, or nil before Start.
// It is mainly useful when listening on port 0 in tests.
func (s *Server) Addr() net.Addr {
	if addr := s.addr.Load(); addr != nil {
		return addr
	}
	return nil
}

// Listening reports whether the gateway is currently accepting connections.
func (s *Server) Listening() bool { return s.listening.Load() }

// TrackedCancelKeys returns how many sessions can currently be cancelled.
// Exposed for diagnostics.
func (s *Server) TrackedCancelKeys() int { return s.cancels.len() }

// SetRouteCount publishes the routable-database gauge. The control plane calls
// it whenever it installs a new table.
func SetRouteCount(n int) { routesGauge.Set(float64(n)) }

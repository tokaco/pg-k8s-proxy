// Package health serves the liveness and readiness probes for the gateway.
//
// Metrics deliberately live on their own port, served by the manager's metrics
// server. Exposing the same registry twice would let a scrape configuration
// pick up both endpoints and silently double every series, and the probe port
// has to be reachable from the node, which is wider than metrics need.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Check reports whether one aspect of the process is healthy.
type Check func() error

// Config configures the probe server.
type Config struct {
	// BindAddress is the address to serve probes on.
	BindAddress string
	// Live reports process liveness. A failing liveness probe restarts the pod,
	// so this must not depend on cluster state.
	Live Check
	// Ready reports whether the gateway can serve traffic.
	Ready Check
	// RouteCount reports the number of routable databases, for the probe body.
	RouteCount func() int
}

// Server serves the probe endpoints. It satisfies manager.Runnable.
type Server struct {
	cfg Config
	srv *http.Server
}

// New builds a probe server.
func New(cfg Config) *Server {
	if cfg.Live == nil {
		cfg.Live = func() error { return nil }
	}
	if cfg.Ready == nil {
		cfg.Ready = func() error { return nil }
	}
	if cfg.RouteCount == nil {
		cfg.RouteCount = func() int { return 0 }
	}

	s := &Server{cfg: cfg}
	mux := http.NewServeMux()

	// The canonical Kubernetes probe paths.
	mux.HandleFunc("GET /healthz", s.handle(cfg.Live))
	mux.HandleFunc("GET /readyz", s.handle(cfg.Ready))

	// Retained so that probes written against the pre-operator deployment keep
	// working. Note that /health is now liveness only: making it depend on the
	// backend count meant an empty cluster restart-looped the gateway forever.
	mux.HandleFunc("GET /health", s.handle(cfg.Live))
	mux.HandleFunc("GET /ready", s.handle(cfg.Ready))

	s.srv = &http.Server{
		Addr:              cfg.BindAddress,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s
}

// NeedLeaderElection reports false: probes must answer on every replica.
func (s *Server) NeedLeaderElection() bool { return false }

// Start serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Start(ctx context.Context) error {
	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", s.cfg.BindAddress)
	if err != nil {
		return fmt.Errorf("health: listening on %s: %w", s.cfg.BindAddress, err)
	}

	errs := make(chan error, 1)
	go func() {
		if err := s.srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			errs <- fmt.Errorf("health: serving probes: %w", err)
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(shutdownCtx)
	return <-errs
}

func (s *Server) handle(check Check) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{"routes": s.cfg.RouteCount()}

		status := http.StatusOK
		if err := check(); err != nil {
			status = http.StatusServiceUnavailable
			body["status"] = "unhealthy"
			body["error"] = err.Error()
		} else {
			body["status"] = "ok"
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

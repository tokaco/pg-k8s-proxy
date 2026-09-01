package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	pgproxyv1alpha1 "github.com/tokaco/pg-k8s-proxy/api/v1alpha1"
	"github.com/tokaco/pg-k8s-proxy/internal/pgwire"
	"github.com/tokaco/pg-k8s-proxy/internal/registry"
)

// Negotiation replies to an SSLRequest or GSSENCRequest packet.
const (
	negotiationAccepted = 'S'
	negotiationRejected = 'N'
)

// maxNegotiationRounds bounds the pre-startup handshake. A well-behaved client
// sends at most one SSLRequest and one GSSENCRequest before its startup packet.
const maxNegotiationRounds = 4

// serve owns one client connection from accept to close.
func (s *Server) serve(ctx context.Context, conn net.Conn) {
	s.track(conn)
	defer func() {
		s.untrack(conn)
		_ = conn.Close()
	}()

	log := s.log.With("client", conn.RemoteAddr().String())

	if !s.acquireSlot() {
		connectionsTotal.WithLabelValues("<unrouted>", outcomeRejected).Inc()
		log.Warn("rejecting connection, gateway is at its connection limit", "limit", s.cfg.MaxConnections)
		writeFatal(conn, pgwire.SQLStateTooManyConnections,
			"too many connections for the PostgreSQL gateway",
			fmt.Sprintf("The gateway is configured to allow at most %d concurrent connections.", s.cfg.MaxConnections))
		return
	}
	defer s.releaseSlot()

	if err := s.handle(ctx, conn, log); err != nil && !isBenignClose(err) {
		log.Warn("session ended with an error", "error", err)
	}
}

func (s *Server) acquireSlot() bool {
	if s.slots == nil {
		return true
	}
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) releaseSlot() {
	if s.slots != nil {
		<-s.slots
	}
}

func (s *Server) handle(ctx context.Context, rawConn net.Conn, log *slog.Logger) error {
	// The handshake must not be allowed to pin a connection open indefinitely.
	deadline := time.Now().Add(s.cfg.StartupTimeout)
	if err := rawConn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("setting handshake deadline: %w", err)
	}

	client, startup, err := s.negotiate(ctx, rawConn, log)
	if err != nil {
		// A peer that connects and hangs up without speaking the protocol —
		// a TCP health check or a port scanner — is not a failed handshake.
		if !isBenignClose(err) {
			connectionsTotal.WithLabelValues("<unrouted>", outcomeHandshake).Inc()
		}
		return err
	}

	if startup.IsCancelRequest() {
		s.forwardCancel(ctx, startup.Cancel, log)
		return nil
	}

	database, ok := startup.Parameter("database")
	if !ok {
		// Per the protocol the database defaults to the user name.
		database, ok = startup.Parameter("user")
	}
	if !ok || database == "" {
		connectionsTotal.WithLabelValues("<unrouted>", outcomeHandshake).Inc()
		writeFatal(client, pgwire.SQLStateProtocolViolation,
			"startup message carried neither a database nor a user parameter", "")
		return errors.New("startup message carried neither a database nor a user parameter")
	}

	route, routed := s.cfg.Routes.Routes().Lookup(database)
	if !routed {
		connectionsTotal.WithLabelValues("<unrouted>", outcomeNoRoute).Inc()
		log.Info("no route for requested database", "database", database)
		writeFatal(client, pgwire.SQLStateUndefinedDatabase,
			fmt.Sprintf("database %q does not exist", database),
			"No PostgresRoute in this cluster claims that database name.")
		return nil
	}

	label := databaseLabel(database, true)
	log = log.With("database", database, "route", route.Source.String(), "backend", route.Address())

	backend, err := s.dialBackend(ctx, route)
	if err != nil {
		connectionsTotal.WithLabelValues(label, outcomeBackendFailed).Inc()
		log.Error("backend is unreachable", "error", err)
		writeFatal(client, pgwire.SQLStateConnectionFailure,
			fmt.Sprintf("the gateway could not reach the backend for database %q", database),
			"Check the status of the PostgresRoute and of the backend Service.")
		return nil
	}
	defer func() { _ = backend.Close() }()

	// Rewrite the database name only when the route asks for it, so the packet
	// forwarded to the backend is otherwise byte-identical to the client's.
	if target := route.TargetDatabase; target != "" && target != database {
		startup.SetParameter("database", target)
		log.Debug("rewriting database name for the backend", "targetDatabase", target)
	}
	if _, err := backend.Write(startup.Encode()); err != nil {
		connectionsTotal.WithLabelValues(label, outcomeBackendFailed).Inc()
		return fmt.Errorf("forwarding startup packet to backend: %w", err)
	}

	// The handshake is done; a session may now idle for as long as it likes.
	// TCP keepalives, not deadlines, are what reap dead peers from here on.
	if err := rawConn.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clearing handshake deadline: %w", err)
	}
	if err := backend.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clearing backend deadline: %w", err)
	}

	connectionsTotal.WithLabelValues(label, outcomeProxied).Inc()
	activeConnections.WithLabelValues(label).Inc()
	started := time.Now()
	defer func() {
		activeConnections.WithLabelValues(label).Dec()
		sessionDuration.WithLabelValues(label).Observe(time.Since(started).Seconds())
	}()

	log.Info("session established")
	return s.relay(client, backend, route, label, log)
}

// negotiate walks the pre-startup handshake, upgrading to TLS if both sides
// want it, and returns the connection to use plus the client's startup packet.
func (s *Server) negotiate(ctx context.Context, rawConn net.Conn, log *slog.Logger) (net.Conn, *pgwire.StartupPacket, error) {
	conn := rawConn
	encrypted := false

	for round := 0; round < maxNegotiationRounds; round++ {
		packet, err := pgwire.ReadStartupPacket(conn)
		if err != nil {
			if isBenignClose(err) {
				// Port probes and TCP health checks connect and hang up
				// without ever speaking the protocol. That is not an error.
				log.Debug("connection closed before the PostgreSQL handshake")
				return nil, nil, err
			}
			return nil, nil, fmt.Errorf("reading startup packet: %w", err)
		}

		switch {
		case packet.IsSSLRequest():
			if s.cfg.TLS == nil {
				if _, err := conn.Write([]byte{negotiationRejected}); err != nil {
					return nil, nil, fmt.Errorf("declining TLS: %w", err)
				}
				continue
			}
			if encrypted {
				return nil, nil, errors.New("client sent a second SSLRequest inside an established TLS session")
			}
			if _, err := conn.Write([]byte{negotiationAccepted}); err != nil {
				return nil, nil, fmt.Errorf("accepting TLS: %w", err)
			}
			tlsConn := tls.Server(conn, s.cfg.TLS.Config)
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				return nil, nil, fmt.Errorf("client TLS handshake: %w", err)
			}
			conn, encrypted = tlsConn, true

		case packet.IsGSSEncRequest():
			// GSSAPI encryption is not supported; declining is a valid answer
			// and clients fall back to TLS or plaintext.
			if _, err := conn.Write([]byte{negotiationRejected}); err != nil {
				return nil, nil, fmt.Errorf("declining GSSAPI encryption: %w", err)
			}

		case packet.IsCancelRequest():
			return conn, packet, nil

		default:
			if s.cfg.TLS != nil && s.cfg.TLS.Required && !encrypted {
				writeFatal(conn, pgwire.SQLStateProtocolViolation,
					"the gateway requires a TLS connection",
					"Connect with sslmode=require or stronger.")
				return nil, nil, errors.New("client declined the required TLS upgrade")
			}
			return conn, packet, nil
		}
	}

	return nil, nil, fmt.Errorf("client sent more than %d negotiation packets without a startup message", maxNegotiationRounds)
}

// dialBackend opens the backend connection, negotiating TLS when the route asks.
func (s *Server) dialBackend(ctx context.Context, route registry.Route) (net.Conn, error) {
	started := time.Now()
	dialer := net.Dialer{Timeout: s.cfg.DialTimeout, KeepAlive: s.cfg.KeepAlivePeriod}

	dialCtx, cancel := context.WithTimeout(ctx, s.cfg.DialTimeout)
	defer cancel()

	conn, err := dialer.DialContext(dialCtx, "tcp", route.Address())
	if err != nil {
		return nil, err
	}
	backendDialDuration.WithLabelValues(route.Database).Observe(time.Since(started).Seconds())

	if err := conn.SetDeadline(time.Now().Add(s.cfg.DialTimeout)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("setting backend handshake deadline: %w", err)
	}

	if !route.TLS.Enabled() {
		return conn, nil
	}

	secured, err := upgradeBackendTLS(ctx, conn, route)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return secured, nil
}

// upgradeBackendTLS performs the server-side SSLRequest exchange and hands back
// the encrypted connection.
func upgradeBackendTLS(ctx context.Context, conn net.Conn, route registry.Route) (net.Conn, error) {
	request := binary.BigEndian.AppendUint32(make([]byte, 0, 8), 8)
	request = binary.BigEndian.AppendUint32(request, uint32(pgwire.SSLRequestCode))
	if _, err := conn.Write(request); err != nil {
		return nil, fmt.Errorf("sending SSLRequest to backend: %w", err)
	}

	var reply [1]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return nil, fmt.Errorf("reading the backend's TLS negotiation reply: %w", err)
	}
	if reply[0] != negotiationAccepted {
		return nil, fmt.Errorf("backend declined TLS but the route requires tls.mode=%s", route.TLS.Mode)
	}

	serverName := route.TLS.ServerName
	if serverName == "" {
		serverName = route.Host
	}
	cfg := &tls.Config{
		ServerName: serverName,
		RootCAs:    route.TLS.RootCAs,
		MinVersion: tls.VersionTLS12,
		// Require mode intentionally skips verification: it buys encryption on
		// the wire without a trust anchor, which is what libpq's sslmode=require
		// does too. VerifyCA and VerifyFull below do check the chain.
		InsecureSkipVerify: route.TLS.Mode == pgproxyv1alpha1.BackendTLSRequire ||
			route.TLS.Mode == pgproxyv1alpha1.BackendTLSVerifyCA,
	}

	tlsConn := tls.Client(conn, cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("backend TLS handshake: %w", err)
	}

	if route.TLS.Mode == pgproxyv1alpha1.BackendTLSVerifyCA {
		if err := verifyChainOnly(tlsConn, route.TLS.RootCAs); err != nil {
			return nil, err
		}
	}
	return tlsConn, nil
}

// verifyChainOnly validates the backend certificate against the trust anchors
// while ignoring the hostname, which is what VerifyCA means. Backends fronted by
// a Service rarely carry the Service's DNS name in their certificate.
func verifyChainOnly(conn *tls.Conn, roots *x509.CertPool) error {
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return errors.New("backend presented no certificate")
	}

	intermediates := x509.NewCertPool()
	for _, cert := range state.PeerCertificates[1:] {
		intermediates.AddCert(cert)
	}
	_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
	})
	if err != nil {
		return fmt.Errorf("verifying the backend certificate chain: %w", err)
	}
	return nil
}

// writeFatal reports a diagnosable failure to the client. Closing the socket
// without one leaves psql printing "server closed the connection unexpectedly".
func writeFatal(conn net.Conn, sqlState, message, detail string) {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write(pgwire.EncodeErrorResponse(sqlState, message, detail))
}

// isBenignClose reports whether an error is just the peer hanging up.
func isBenignClose(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, context.Canceled)
}

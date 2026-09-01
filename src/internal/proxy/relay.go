package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/tokaco/pg-k8s-proxy/internal/pgwire"
	"github.com/tokaco/pg-k8s-proxy/internal/registry"
)

// Direction labels for the bytes counter.
const (
	directionToBackend = "to_backend"
	directionToClient  = "to_client"
)

// relay splices an authenticated session in both directions.
//
// The client-to-backend direction is copied verbatim. The backend-to-client
// direction is framed only until BackendKeyData arrives, because the gateway
// substitutes its own cancellation key there; after ReadyForQuery it degrades to
// a plain byte copy so that bulk traffic such as COPY costs nothing extra.
func (s *Server) relay(client, backend net.Conn, route registry.Route, label string, log *slog.Logger) error {
	var wg sync.WaitGroup
	wg.Add(1)

	var upstreamErr error
	go func() {
		defer wg.Done()
		n, err := io.Copy(backend, client)
		bytesTotal.WithLabelValues(label, directionToBackend).Add(float64(n))
		if err != nil && !isBenignClose(err) {
			upstreamErr = fmt.Errorf("relaying client to backend: %w", err)
		}
		// Half-close so the backend sees the client's EOF and tears down
		// cleanly, instead of waiting for its own read to time out.
		closeWrite(backend)
	}()

	downstreamErr := s.relayFromBackend(client, backend, route, label, log)
	closeWrite(client)

	// The backend has stopped talking, so the session is over. The upstream
	// copy may still be blocked reading from an idle client, and only closing
	// the client unblocks it — closing the backend would not, since the copy is
	// waiting on a read rather than a write. serve() closes it again on return,
	// which is a harmless no-op.
	_ = client.Close()
	wg.Wait()

	return errors.Join(downstreamErr, upstreamErr)
}

// relayFromBackend forwards backend messages, rewriting BackendKeyData, then
// hands the rest of the stream to a raw copy.
func (s *Server) relayFromBackend(client, backend net.Conn, route registry.Route, label string, log *slog.Logger) error {
	reader := pgwire.NewMessageReader(backend)

	var issued pgwire.CancelRequest
	var haveKey bool
	defer func() {
		if haveKey {
			s.cancels.release(issued)
		}
	}()

	var framed int64
	for {
		msg, err := reader.Next()
		if err != nil {
			bytesTotal.WithLabelValues(label, directionToClient).Add(float64(framed))
			if isBenignClose(err) {
				return nil
			}
			return fmt.Errorf("framing backend messages: %w", err)
		}

		out := msg.Encode()

		if msg.Type == pgwire.MsgBackendKeyData {
			backendKey, err := pgwire.DecodeBackendKeyData(msg.Body)
			if err != nil {
				return err
			}
			issued = s.cancels.issue(cancelEntry{
				backendAddr: route.Address(),
				backendKey:  backendKey,
				tls:         route.TLS,
			})
			haveKey = true
			// The client must never learn the backend's real key: two backends
			// can hand out the same process ID, and a leaked key lets anyone
			// who can reach the backend cancel that session directly.
			out = pgwire.EncodeBackendKeyData(issued)
			log.Debug("issued a gateway cancellation key for the session")
		}

		n, err := client.Write(out)
		framed += int64(n)
		if err != nil {
			bytesTotal.WithLabelValues(label, directionToClient).Add(float64(framed))
			if isBenignClose(err) {
				return nil
			}
			return fmt.Errorf("writing backend message to client: %w", err)
		}

		if msg.Type == pgwire.MsgReadyForQuery {
			// Startup is complete. Everything after this is opaque to the
			// gateway, so stop parsing and splice the raw bytes.
			bytesTotal.WithLabelValues(label, directionToClient).Add(float64(framed))
			return s.spliceToClient(client, backend, label)
		}
	}
}

func (s *Server) spliceToClient(client, backend net.Conn, label string) error {
	n, err := io.Copy(client, backend)
	bytesTotal.WithLabelValues(label, directionToClient).Add(float64(n))
	if err != nil && !isBenignClose(err) {
		return fmt.Errorf("relaying backend to client: %w", err)
	}
	return nil
}

// forwardCancel handles a CancelRequest, which arrives on its own connection
// carrying no database name — only the key the gateway issued earlier.
func (s *Server) forwardCancel(ctx context.Context, request pgwire.CancelRequest, log *slog.Logger) {
	entry, ok := s.cancels.lookup(request)
	if !ok {
		// Either the session already ended, or the key belongs to a different
		// replica. Staying silent is what a real server does with a bad key.
		cancelRequestsTotal.WithLabelValues("unknown_key").Inc()
		log.Debug("discarding a cancellation request for an unknown key")
		return
	}

	dialCtx, cancel := context.WithTimeout(ctx, s.cfg.DialTimeout)
	defer cancel()

	dialer := net.Dialer{Timeout: s.cfg.DialTimeout}
	conn, err := dialer.DialContext(dialCtx, "tcp", entry.backendAddr)
	if err != nil {
		cancelRequestsTotal.WithLabelValues("backend_unreachable").Inc()
		log.Warn("could not reach the backend to forward a cancellation", "backend", entry.backendAddr, "error", err)
		return
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(s.cfg.DialTimeout))

	target := conn
	if entry.tls.Enabled() {
		secured, err := upgradeBackendTLS(dialCtx, conn, registry.Route{
			Host: hostOf(entry.backendAddr),
			TLS:  entry.tls,
		})
		if err != nil {
			cancelRequestsTotal.WithLabelValues("tls_failed").Inc()
			log.Warn("could not secure the cancellation connection", "backend", entry.backendAddr, "error", err)
			return
		}
		target = secured
	}

	// A cancel connection is write-only: the server acts on the packet and
	// closes without replying.
	if _, err := target.Write(pgwire.EncodeCancelRequest(entry.backendKey)); err != nil {
		cancelRequestsTotal.WithLabelValues("write_failed").Inc()
		log.Warn("could not deliver a cancellation request", "backend", entry.backendAddr, "error", err)
		return
	}

	cancelRequestsTotal.WithLabelValues("forwarded").Inc()
	log.Debug("forwarded a cancellation request", "backend", entry.backendAddr)
}

func hostOf(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	return host
}

// closeWriter is implemented by *net.TCPConn and, since Go 1.11, by *tls.Conn.
type closeWriter interface{ CloseWrite() error }

func closeWrite(conn net.Conn) {
	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

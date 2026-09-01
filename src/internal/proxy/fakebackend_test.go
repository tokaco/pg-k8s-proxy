package proxy

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/tokaco/pg-k8s-proxy/internal/pgwire"
)

// fakeBackend is a PostgreSQL server that speaks just enough of the protocol
// for the gateway to route to it: it completes the startup handshake, hands out
// a BackendKeyData, and records what it was sent.
type fakeBackend struct {
	t        *testing.T
	listener net.Listener
	wg       sync.WaitGroup

	// key is the BackendKeyData this server issues.
	key pgwire.CancelRequest

	mu       sync.Mutex
	startups []*pgwire.StartupPacket
	cancels  []pgwire.CancelRequest
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("starting the fake backend: %v", err)
	}

	b := &fakeBackend{
		t:        t,
		listener: listener,
		key:      pgwire.CancelRequest{ProcessID: 4242, SecretKey: 999},
	}

	b.wg.Add(1)
	go b.serve()

	t.Cleanup(func() {
		_ = listener.Close()
		b.wg.Wait()
	})
	return b
}

func (b *fakeBackend) addr() string { return b.listener.Addr().String() }

func (b *fakeBackend) host() string {
	host, _, _ := net.SplitHostPort(b.addr())
	return host
}

func (b *fakeBackend) port() int32 {
	addr := b.listener.Addr().(*net.TCPAddr)
	return int32(addr.Port)
}

func (b *fakeBackend) serve() {
	defer b.wg.Done()
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			return
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer func() { _ = conn.Close() }()
			b.handle(conn)
		}()
	}
}

func (b *fakeBackend) handle(conn net.Conn) {
	packet, err := pgwire.ReadStartupPacket(conn)
	if err != nil {
		return
	}

	if packet.IsCancelRequest() {
		b.mu.Lock()
		b.cancels = append(b.cancels, packet.Cancel)
		b.mu.Unlock()
		return
	}

	b.mu.Lock()
	b.startups = append(b.startups, packet)
	b.mu.Unlock()

	// AuthenticationOk, then the key, then ReadyForQuery.
	_, _ = conn.Write(pgwire.Message{Type: 'R', Body: binary.BigEndian.AppendUint32(nil, 0)}.Encode())
	_, _ = conn.Write(pgwire.EncodeBackendKeyData(b.key))
	_, _ = conn.Write(pgwire.Message{Type: 'Z', Body: []byte{'I'}}.Encode())

	// Echo everything else so the test can prove the splice is bidirectional.
	_, _ = io.Copy(conn, conn)
}

func (b *fakeBackend) recordedStartups() []*pgwire.StartupPacket {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*pgwire.StartupPacket(nil), b.startups...)
}

func (b *fakeBackend) recordedCancels() []pgwire.CancelRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]pgwire.CancelRequest(nil), b.cancels...)
}

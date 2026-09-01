package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/tokaco/pg-k8s-proxy/internal/pgwire"
	"github.com/tokaco/pg-k8s-proxy/internal/registry"
)

// startGateway boots a Server on an ephemeral port with the given routes and
// returns its address.
func startGateway(t *testing.T, routes map[string]registry.Route, tune func(*Config)) (string, *Server) {
	t.Helper()

	store := registry.NewStore()
	store.Store(&registry.Snapshot{Table: registry.NewTable(routes)})

	cfg := Config{
		ListenAddress:       "127.0.0.1:0",
		Routes:              store,
		StartupTimeout:      5 * time.Second,
		DialTimeout:         2 * time.Second,
		ShutdownGracePeriod: time.Second,
		// Discard gateway logs; a failing test reports through t, not stderr.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if tune != nil {
		tune(&cfg)
	}

	server, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("gateway exited with an error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("gateway did not shut down within 10s")
		}
	})

	// Start binds the listener asynchronously; wait for the address.
	deadline := time.Now().Add(5 * time.Second)
	for server.Addr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("gateway did not begin listening within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return server.Addr().String(), server
}

func routeTo(b *fakeBackend, database, target string) registry.Route {
	return registry.Route{
		Source:         types.NamespacedName{Namespace: "test", Name: database},
		Database:       database,
		TargetDatabase: target,
		Host:           b.host(),
		Port:           b.port(),
	}
}

// connect opens a client connection and sends a startup packet for database.
func connect(t *testing.T, address, database string) net.Conn {
	t.Helper()

	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		t.Fatalf("dialling the gateway: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("setting the client deadline: %v", err)
	}

	startup := &pgwire.StartupPacket{
		Code: pgwire.ProtocolVersion3,
		Parameters: []pgwire.Parameter{
			{Key: "user", Value: "alice"},
			{Key: "database", Value: database},
		},
	}
	if _, err := conn.Write(startup.Encode()); err != nil {
		t.Fatalf("sending the startup packet: %v", err)
	}
	return conn
}

// readUntilReady consumes messages up to and including ReadyForQuery, returning
// the BackendKeyData the client was given.
func readUntilReady(t *testing.T, conn net.Conn) pgwire.CancelRequest {
	t.Helper()

	reader := pgwire.NewMessageReader(conn)
	var key pgwire.CancelRequest

	for {
		msg, err := reader.Next()
		if err != nil {
			t.Fatalf("reading the handshake: %v", err)
		}
		switch msg.Type {
		case pgwire.MsgBackendKeyData:
			key, err = pgwire.DecodeBackendKeyData(msg.Body)
			if err != nil {
				t.Fatalf("decoding BackendKeyData: %v", err)
			}
		case pgwire.MsgErrorResponse:
			t.Fatalf("gateway returned an error during the handshake: %q", msg.Body)
		case pgwire.MsgReadyForQuery:
			return key
		}
	}
}

// readErrorResponse expects the gateway to reject the connection and returns
// the ErrorResponse body.
func readErrorResponse(t *testing.T, conn net.Conn) string {
	t.Helper()

	msg, err := pgwire.NewMessageReader(conn).Next()
	if err != nil {
		t.Fatalf("expected an ErrorResponse, got: %v", err)
	}
	if msg.Type != pgwire.MsgErrorResponse {
		t.Fatalf("message type = %q, want %q", msg.Type, pgwire.MsgErrorResponse)
	}
	return string(msg.Body)
}

func TestGatewayRoutesByDatabaseName(t *testing.T) {
	billing := newFakeBackend(t)
	analytics := newFakeBackend(t)

	address, _ := startGateway(t, map[string]registry.Route{
		"billing":   routeTo(billing, "billing", "billing"),
		"analytics": routeTo(analytics, "analytics", "analytics"),
	}, nil)

	conn := connect(t, address, "analytics")
	readUntilReady(t, conn)

	if got := len(analytics.recordedStartups()); got != 1 {
		t.Errorf("the analytics backend saw %d startups, want 1", got)
	}
	if got := len(billing.recordedStartups()); got != 0 {
		t.Errorf("the billing backend saw %d startups, want 0", got)
	}
}

// The gateway must forward the client's packet unchanged apart from the
// database name, since the backend authenticates against those parameters.
func TestGatewayForwardsStartupParametersUnchanged(t *testing.T) {
	backend := newFakeBackend(t)
	address, _ := startGateway(t, map[string]registry.Route{
		"billing": routeTo(backend, "billing", "billing"),
	}, nil)

	conn := connect(t, address, "billing")
	readUntilReady(t, conn)

	startups := backend.recordedStartups()
	if len(startups) != 1 {
		t.Fatalf("backend saw %d startups, want 1", len(startups))
	}
	if user, ok := startups[0].Parameter("user"); !ok || user != "alice" {
		t.Errorf(`backend saw user=%q, want "alice"`, user)
	}
	if db, _ := startups[0].Parameter("database"); db != "billing" {
		t.Errorf(`backend saw database=%q, want "billing"`, db)
	}
}

func TestGatewayRewritesTheTargetDatabase(t *testing.T) {
	backend := newFakeBackend(t)
	address, _ := startGateway(t, map[string]registry.Route{
		"public-name": routeTo(backend, "public-name", "internal_name"),
	}, nil)

	conn := connect(t, address, "public-name")
	readUntilReady(t, conn)

	startups := backend.recordedStartups()
	if len(startups) != 1 {
		t.Fatalf("backend saw %d startups, want 1", len(startups))
	}
	if db, _ := startups[0].Parameter("database"); db != "internal_name" {
		t.Errorf(`backend saw database=%q, want "internal_name"`, db)
	}
}

// Closing the socket on an unknown database leaves clients reporting "server
// closed the connection unexpectedly"; a real ErrorResponse is diagnosable.
func TestGatewayReportsAnUnknownDatabaseAsSQLState3D000(t *testing.T) {
	address, _ := startGateway(t, nil, nil)

	conn := connect(t, address, "nonexistent")
	body := readErrorResponse(t, conn)

	if !strings.Contains(body, pgwire.SQLStateUndefinedDatabase) {
		t.Errorf("error does not carry SQLSTATE %s: %q", pgwire.SQLStateUndefinedDatabase, body)
	}
	if !strings.Contains(body, "nonexistent") {
		t.Errorf("error does not name the database: %q", body)
	}
}

func TestGatewayReportsAnUnreachableBackend(t *testing.T) {
	// Bind and immediately release a port so the address is almost certainly dead.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	deadPort := int32(listener.Addr().(*net.TCPAddr).Port)
	_ = listener.Close()

	address, _ := startGateway(t, map[string]registry.Route{
		"billing": {Database: "billing", TargetDatabase: "billing", Host: "127.0.0.1", Port: deadPort},
	}, nil)

	conn := connect(t, address, "billing")
	body := readErrorResponse(t, conn)

	if !strings.Contains(body, pgwire.SQLStateConnectionFailure) {
		t.Errorf("error does not carry SQLSTATE %s: %q", pgwire.SQLStateConnectionFailure, body)
	}
}

// Two backends can hand out the same process ID, and a real backend key would
// let anyone who can reach the backend cancel that session directly.
func TestGatewaySubstitutesItsOwnCancellationKey(t *testing.T) {
	backend := newFakeBackend(t)
	address, server := startGateway(t, map[string]registry.Route{
		"billing": routeTo(backend, "billing", "billing"),
	}, nil)

	conn := connect(t, address, "billing")
	issued := readUntilReady(t, conn)

	if issued == backend.key {
		t.Error("the client received the backend's real key")
	}
	if issued == (pgwire.CancelRequest{}) {
		t.Error("the client received no BackendKeyData")
	}
	if got := server.TrackedCancelKeys(); got != 1 {
		t.Errorf("the gateway tracks %d cancel keys, want 1", got)
	}
}

// Ctrl-C in psql opens a second connection carrying only the key; without
// translation it reaches no backend at all.
func TestGatewayTranslatesAndForwardsCancellation(t *testing.T) {
	backend := newFakeBackend(t)
	address, _ := startGateway(t, map[string]registry.Route{
		"billing": routeTo(backend, "billing", "billing"),
	}, nil)

	session := connect(t, address, "billing")
	issued := readUntilReady(t, session)

	cancelConn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		t.Fatalf("opening the cancel connection: %v", err)
	}
	defer func() { _ = cancelConn.Close() }()

	if _, err := cancelConn.Write(pgwire.EncodeCancelRequest(issued)); err != nil {
		t.Fatalf("sending the cancel request: %v", err)
	}

	var cancels []pgwire.CancelRequest
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cancels = backend.recordedCancels(); len(cancels) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(cancels) != 1 {
		t.Fatalf("the backend received %d cancel requests, want 1", len(cancels))
	}
	if cancels[0] != backend.key {
		t.Errorf("the backend received %+v, want its own key %+v", cancels[0], backend.key)
	}
}

func TestGatewayIgnoresCancellationForAnUnknownKey(t *testing.T) {
	backend := newFakeBackend(t)
	address, _ := startGateway(t, map[string]registry.Route{
		"billing": routeTo(backend, "billing", "billing"),
	}, nil)

	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		t.Fatalf("dialling the gateway: %v", err)
	}
	defer func() { _ = conn.Close() }()

	stray := pgwire.CancelRequest{ProcessID: 1, SecretKey: 2}
	if _, err := conn.Write(pgwire.EncodeCancelRequest(stray)); err != nil {
		t.Fatalf("sending the cancel request: %v", err)
	}

	// The gateway must close without replying, exactly as a real server does.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(conn); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("reading after a stray cancel: %v", err)
	}
	if got := len(backend.recordedCancels()); got != 0 {
		t.Errorf("the backend received %d cancel requests, want 0", got)
	}
}

// Passing TLS through is impossible: the gateway must read the startup message
// to learn the database. With no certificate configured it must decline.
func TestGatewayDeclinesTLSWhenNotConfigured(t *testing.T) {
	backend := newFakeBackend(t)
	address, _ := startGateway(t, map[string]registry.Route{
		"billing": routeTo(backend, "billing", "billing"),
	}, nil)

	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		t.Fatalf("dialling the gateway: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	sslRequest := binary.BigEndian.AppendUint32(nil, 8)
	sslRequest = binary.BigEndian.AppendUint32(sslRequest, uint32(pgwire.SSLRequestCode))
	if _, err := conn.Write(sslRequest); err != nil {
		t.Fatalf("sending the SSLRequest: %v", err)
	}

	var reply [1]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		t.Fatalf("reading the negotiation reply: %v", err)
	}
	if reply[0] != 'N' {
		t.Fatalf("negotiation reply = %q, want 'N'", reply[0])
	}

	// The client must then be able to continue in plaintext, as libpq does.
	startup := &pgwire.StartupPacket{
		Code:       pgwire.ProtocolVersion3,
		Parameters: []pgwire.Parameter{{Key: "user", Value: "alice"}, {Key: "database", Value: "billing"}},
	}
	if _, err := conn.Write(startup.Encode()); err != nil {
		t.Fatalf("sending the startup packet: %v", err)
	}
	readUntilReady(t, conn)
}

func TestGatewayDeclinesGSSAPIEncryption(t *testing.T) {
	address, _ := startGateway(t, nil, nil)

	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		t.Fatalf("dialling the gateway: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	request := binary.BigEndian.AppendUint32(nil, 8)
	request = binary.BigEndian.AppendUint32(request, uint32(pgwire.GSSEncRequestCode))
	if _, err := conn.Write(request); err != nil {
		t.Fatalf("sending the GSSENCRequest: %v", err)
	}

	var reply [1]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		t.Fatalf("reading the negotiation reply: %v", err)
	}
	if reply[0] != 'N' {
		t.Errorf("negotiation reply = %q, want 'N'", reply[0])
	}
}

func TestGatewayRejectsConnectionsBeyondTheLimit(t *testing.T) {
	backend := newFakeBackend(t)
	address, _ := startGateway(t, map[string]registry.Route{
		"billing": routeTo(backend, "billing", "billing"),
	}, func(cfg *Config) { cfg.MaxConnections = 1 })

	held := connect(t, address, "billing")
	readUntilReady(t, held)

	// The second connection is refused before the handshake even starts.
	overflow, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		t.Fatalf("dialling the gateway: %v", err)
	}
	defer func() { _ = overflow.Close() }()
	_ = overflow.SetDeadline(time.Now().Add(10 * time.Second))

	body := readErrorResponse(t, overflow)
	if !strings.Contains(body, pgwire.SQLStateTooManyConnections) {
		t.Errorf("error does not carry SQLSTATE %s: %q", pgwire.SQLStateTooManyConnections, body)
	}
}

func TestGatewayRelaysTrafficBothWays(t *testing.T) {
	backend := newFakeBackend(t)
	address, _ := startGateway(t, map[string]registry.Route{
		"billing": routeTo(backend, "billing", "billing"),
	}, nil)

	conn := connect(t, address, "billing")
	readUntilReady(t, conn)

	// The fake backend echoes everything after the handshake, so a round trip
	// proves both directions of the splice are live.
	payload := []byte("Q\x00\x00\x00\x0bSELECT 1\x00")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("writing to the session: %v", err)
	}

	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatalf("reading the echo: %v", err)
	}
	if string(echoed) != string(payload) {
		t.Errorf("echo = %q, want %q", echoed, payload)
	}
}

func TestGatewayReleasesTheCancelKeyWhenTheSessionEnds(t *testing.T) {
	backend := newFakeBackend(t)
	address, server := startGateway(t, map[string]registry.Route{
		"billing": routeTo(backend, "billing", "billing"),
	}, nil)

	conn := connect(t, address, "billing")
	readUntilReady(t, conn)
	if got := server.TrackedCancelKeys(); got != 1 {
		t.Fatalf("the gateway tracks %d cancel keys, want 1", got)
	}

	_ = conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if server.TrackedCancelKeys() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("the cancel key was not released; %d still tracked", server.TrackedCancelKeys())
}

func TestNewRejectsAnInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "no routes", cfg: Config{ListenAddress: ":5432"}},
		{name: "no listen address", cfg: Config{Routes: registry.NewStore()}},
		{name: "negative connection limit", cfg: Config{ListenAddress: ":5432", Routes: registry.NewStore(), MaxConnections: -1}},
		{name: "TLS without a config", cfg: Config{ListenAddress: ":5432", Routes: registry.NewStore(), TLS: &ServerTLS{}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Error("expected an error, got none")
			}
		})
	}
}

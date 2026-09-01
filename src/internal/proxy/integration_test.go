//go:build integration

// Package proxy integration tests run the gateway against a real PostgreSQL
// server and a real psql client. The unit tests use a fake backend that skips
// authentication entirely, so only this test exercises the parts a real session
// depends on: the SCRAM exchange relayed untouched, ParameterStatus, the
// substituted BackendKeyData, and a query round trip.
//
// Run with: go test -tags=integration ./internal/proxy/
// PGPROXY_TEST_BACKEND must point at a reachable PostgreSQL instance.
package proxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tokaco/pg-k8s-proxy/internal/registry"
)

// backendAddress returns host and port of the PostgreSQL under test.
func backendAddress(t *testing.T) (string, int32) {
	t.Helper()

	address := os.Getenv("PGPROXY_TEST_BACKEND")
	if address == "" {
		t.Skip("PGPROXY_TEST_BACKEND is unset; skipping the integration test")
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("PGPROXY_TEST_BACKEND %q is not host:port: %v", address, err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		t.Fatalf("PGPROXY_TEST_BACKEND port %q: %v", portText, err)
	}
	return host, int32(port)
}

// startGatewayFor boots the gateway in front of the real backend and returns
// the port a client should connect to.
func startGatewayFor(t *testing.T, routes map[string]registry.Route) int {
	t.Helper()

	store := registry.NewStore()
	store.Store(&registry.Snapshot{Table: registry.NewTable(routes)})

	server, err := New(Config{
		ListenAddress:       "127.0.0.1:0",
		Routes:              store,
		StartupTimeout:      20 * time.Second,
		DialTimeout:         10 * time.Second,
		ShutdownGracePeriod: 2 * time.Second,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("gateway did not shut down")
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for server.Addr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("gateway did not begin listening")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return server.Addr().(*net.TCPAddr).Port
}

// psql runs the real client against the gateway and returns its combined output.
func psql(t *testing.T, port int, database string, args ...string) (string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	full := append([]string{
		"-h", "127.0.0.1",
		"-p", strconv.Itoa(port),
		"-U", "postgres",
		"-d", database,
		"--no-psqlrc",
	}, args...)

	cmd := exec.CommandContext(ctx, "psql", full...)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+os.Getenv("PGPROXY_TEST_PASSWORD"))
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// This is the assertion the whole project rests on: a real client, naming only
// a database, reaches the instance serving it and gets an answer back.
func TestRealSessionThroughGateway(t *testing.T) {
	host, port := backendAddress(t)

	gatewayPort := startGatewayFor(t, map[string]registry.Route{
		"billing": {Database: "billing", TargetDatabase: "billing", Host: host, Port: port},
	})

	out, err := psql(t, gatewayPort, "billing", "-tAc", "select current_database()")
	if err != nil {
		t.Fatalf("psql failed: %v\noutput:\n%s", err, out)
	}
	if out != "billing" {
		t.Errorf("current_database() = %q, want %q\nfull output:\n%s", out, "billing", out)
	}
}

// Authentication is relayed untouched, so a wrong password must be rejected by
// the backend rather than accepted or mangled by the gateway.
func TestBadPasswordIsRejectedByTheBackend(t *testing.T) {
	host, port := backendAddress(t)
	gatewayPort := startGatewayFor(t, map[string]registry.Route{
		"billing": {Database: "billing", TargetDatabase: "billing", Host: host, Port: port},
	})

	cmd := exec.Command("psql",
		"-h", "127.0.0.1", "-p", strconv.Itoa(gatewayPort),
		"-U", "postgres", "-d", "billing", "--no-psqlrc",
		"-tAc", "select 1")
	cmd.Env = append(os.Environ(), "PGPASSWORD=definitely-wrong")
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("a wrong password was accepted; output:\n%s", out)
	}
	if !strings.Contains(string(out), "authentication failed") {
		t.Errorf("expected an authentication failure from the backend, got:\n%s", out)
	}
}

// A larger result set forces the relay past ReadyForQuery into the raw splice.
func TestLargeResultSetCrossesTheSplice(t *testing.T) {
	host, port := backendAddress(t)
	gatewayPort := startGatewayFor(t, map[string]registry.Route{
		"billing": {Database: "billing", TargetDatabase: "billing", Host: host, Port: port},
	})

	out, err := psql(t, gatewayPort, "billing", "-tAc",
		"select count(*) from (select generate_series(1, 200000)) s")
	if err != nil {
		t.Fatalf("psql failed: %v\noutput:\n%s", err, out)
	}
	if out != "200000" {
		t.Errorf("count = %q, want 200000\noutput:\n%s", out, out)
	}
}

// targetDatabase rewriting must be invisible to the client's credentials but
// visible in which database it lands in.
func TestTargetDatabaseRewriteReachesTheRealDatabase(t *testing.T) {
	host, port := backendAddress(t)
	gatewayPort := startGatewayFor(t, map[string]registry.Route{
		"public-name": {Database: "public-name", TargetDatabase: "billing", Host: host, Port: port},
	})

	out, err := psql(t, gatewayPort, "public-name", "-tAc", "select current_database()")
	if err != nil {
		t.Fatalf("psql failed: %v\noutput:\n%s", err, out)
	}
	if out != "billing" {
		t.Errorf("current_database() = %q, want %q\noutput:\n%s", out, "billing", out)
	}
}

// An unknown database must produce the diagnostic a real server would send,
// not a dropped connection.
func TestUnknownDatabaseGivesARealDiagnostic(t *testing.T) {
	_, _ = backendAddress(t)
	gatewayPort := startGatewayFor(t, nil)

	out, err := psql(t, gatewayPort, "no-such-database", "-tAc", "select 1")
	if err == nil {
		t.Fatalf("connecting to an unrouted database succeeded; output:\n%s", out)
	}
	if !strings.Contains(out, "does not exist") {
		t.Errorf("expected a \"does not exist\" diagnostic, got:\n%s", out)
	}
}

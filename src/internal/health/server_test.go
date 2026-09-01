package health

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

// startServer boots a probe server on an ephemeral port and returns its base URL.
func startServer(t *testing.T, cfg Config) string {
	t.Helper()

	// Reserve a port, then hand the address to the server. Listening on :0
	// inside the server would not tell the test where to connect.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	address := listener.Addr().String()
	_ = listener.Close()

	cfg.BindAddress = address
	server := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("probe server exited with an error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("probe server did not shut down within 10s")
		}
	})

	base := "http://" + address
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := probe(base + "/healthz"); err == nil {
			return base
		}
		if time.Now().After(deadline) {
			t.Fatal("probe server did not start listening within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// probe issues a GET purely to see whether the server answers, closing the body
// so a polling loop does not leak connections.
func probe(url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func get(t *testing.T, url string) (int, map[string]any) {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body map[string]any
	// A 404 carries no JSON, so a decode failure is not fatal here.
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func TestProbesReportHealthy(t *testing.T) {
	base := startServer(t, Config{RouteCount: func() int { return 3 }})

	// The canonical paths and the aliases kept for the pre-operator deployment
	// must all answer identically.
	for _, path := range []string{"/healthz", "/readyz", "/health", "/ready"} {
		t.Run(path, func(t *testing.T) {
			status, body := get(t, base+path)
			if status != http.StatusOK {
				t.Errorf("status = %d, want %d", status, http.StatusOK)
			}
			if body["status"] != "ok" {
				t.Errorf(`status field = %v, want "ok"`, body["status"])
			}
			if body["routes"] != float64(3) {
				t.Errorf("routes = %v, want 3", body["routes"])
			}
		})
	}
}

func TestReadinessReportsUnready(t *testing.T) {
	base := startServer(t, Config{
		Ready: func() error { return errors.New("the gateway is not accepting connections yet") },
	})

	status, body := get(t, base+"/readyz")
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", status, http.StatusServiceUnavailable)
	}
	if body["status"] != "unhealthy" {
		t.Errorf(`status field = %v, want "unhealthy"`, body["status"])
	}
	if body["error"] == nil {
		t.Error("the failure reason was not reported")
	}

	// Liveness must not follow readiness: a gateway that is merely not ready
	// yet has to keep its pod alive.
	if status, _ := get(t, base+"/healthz"); status != http.StatusOK {
		t.Errorf("liveness status = %d, want %d", status, http.StatusOK)
	}
}

// Metrics live on the manager's own port. Serving the same registry here too
// would let a scrape configuration pick up both endpoints and double every
// series, so the probe port must not answer /metrics at all.
func TestProbePortDoesNotServeMetrics(t *testing.T) {
	base := startServer(t, Config{RouteCount: func() int { return 1 }})

	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d; the probe port is serving metrics again",
			resp.StatusCode, http.StatusNotFound)
	}
}

func TestDefaultsAreSafe(t *testing.T) {
	// A Config with no checks supplied must still answer, rather than
	// dereferencing a nil func and taking the process down.
	base := startServer(t, Config{})

	for _, path := range []string{"/healthz", "/readyz"} {
		status, body := get(t, base+path)
		if status != http.StatusOK {
			t.Errorf("%s status = %d, want %d", path, status, http.StatusOK)
		}
		if body["routes"] != float64(0) {
			t.Errorf("%s routes = %v, want 0", path, body["routes"])
		}
	}
}

func TestShutdownIsGraceful(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	address := listener.Addr().String()
	_ = listener.Close()

	server := New(Config{BindAddress: address})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := probe("http://" + address + "/healthz"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("probe server did not start listening within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start returned %v, want nil on a cancelled context", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return within 10s of cancellation")
	}

	// The port must actually be released, or a restarting process would fail
	// to bind.
	if err := probe("http://" + address + "/healthz"); err == nil {
		t.Error("the probe server is still answering after shutdown")
	}
}

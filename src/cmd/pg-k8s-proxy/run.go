package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	pgproxyv1alpha1 "github.com/tokaco/pg-k8s-proxy/api/v1alpha1"
	"github.com/tokaco/pg-k8s-proxy/internal/config"
	"github.com/tokaco/pg-k8s-proxy/internal/controller"
	"github.com/tokaco/pg-k8s-proxy/internal/health"
	"github.com/tokaco/pg-k8s-proxy/internal/proxy"
	"github.com/tokaco/pg-k8s-proxy/internal/registry"
	"github.com/tokaco/pg-k8s-proxy/internal/version"
)

// run wires the manager, the controllers, and the gateway together and blocks
// until the process is signalled to stop.
func run(ctx context.Context, cfg config.Config, warnings []string) error {
	logger := newLogger(cfg)
	slog.SetDefault(logger)
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))

	for _, warning := range warnings {
		logger.Warn(warning)
	}
	logger.Info("starting pg-k8s-proxy",
		"version", version.String(),
		"role", cfg.Role,
		"scope", scopeDescription(cfg),
		"serviceDiscovery", cfg.Discovery.Enabled,
	)

	scheme, err := buildScheme()
	if err != nil {
		return err
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), manager.Options{
		Scheme:  scheme,
		Cache:   cacheOptions(cfg),
		Metrics: metricsserver.Options{BindAddress: cfg.MetricsBindAddress},
		// Probes are served by our own runnable so that the legacy /health and
		// /ready paths keep working alongside /healthz and /readyz.
		HealthProbeBindAddress: "0",

		LeaderElection:                cfg.Leader.Enabled,
		LeaderElectionID:              cfg.Leader.ID,
		LeaderElectionNamespace:       cfg.Leader.Namespace,
		LeaderElectionReleaseOnCancel: true,
		LeaseDuration:                 &cfg.Leader.LeaseDuration,
		RenewDeadline:                 &cfg.Leader.RenewDeadline,
		RetryPeriod:                   &cfg.Leader.RetryPeriod,

		GracefulShutdownTimeout: &cfg.Proxy.ShutdownGracePeriod,
	})
	if err != nil {
		return fmt.Errorf("creating the controller manager: %w", err)
	}

	store := registry.NewStore()

	tableBuilder := &controller.RouteTableBuilder{
		Store:         store,
		ClusterDomain: cfg.Watch.ClusterDomain,
		WatchSecrets:  cfg.Watch.WatchSecrets,
	}
	if err := tableBuilder.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the routing table builder: %w", err)
	}

	if cfg.Role.RunsManager() {
		routeReconciler := &controller.PostgresRouteReconciler{Client: mgr.GetClient(), Store: store}
		if err := routeReconciler.SetupWithManager(mgr); err != nil {
			return fmt.Errorf("setting up the PostgresRoute controller: %w", err)
		}

		if cfg.Discovery.Enabled {
			discovery := &controller.ServiceDiscoveryReconciler{
				Client:             mgr.GetClient(),
				Scheme:             mgr.GetScheme(),
				Selector:           cfg.Discovery.Selector(),
				DatabaseAnnotation: cfg.Discovery.DatabaseAnnotation,
			}
			if err := discovery.SetupWithManager(mgr); err != nil {
				return fmt.Errorf("setting up the service discovery controller: %w", err)
			}
			logger.Info("service discovery enabled", "selector", cfg.Discovery.LabelSelector)
		}
	}

	var gateway *proxy.Server
	if cfg.Role.RunsProxy() {
		gateway, err = newGateway(cfg, store, logger)
		if err != nil {
			return err
		}
		if err := mgr.Add(gateway); err != nil {
			return fmt.Errorf("registering the gateway: %w", err)
		}
	}

	probes := health.New(health.Config{
		BindAddress: cfg.HealthBindAddress,
		Live:        func() error { return nil },
		Ready:       readinessCheck(gateway),
		RouteCount:  func() int { return store.Routes().Len() },
	})
	if err := mgr.Add(probes); err != nil {
		return fmt.Errorf("registering the probe server: %w", err)
	}

	logger.Info("handing off to the controller manager")
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("running the controller manager: %w", err)
	}
	logger.Info("shutdown complete")
	return nil
}

func buildScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering the built-in Kubernetes types: %w", err)
	}
	if err := pgproxyv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering the pgproxy.io types: %w", err)
	}
	return scheme, nil
}

// cacheOptions scopes the informers. Namespaced mode keeps the cache and the
// required RBAC inside the listed namespaces; cluster mode watches everything.
//
// Secrets are cached only when a label marks them as a backend CA bundle, so
// enabling that feature never pulls every Secret in the cluster into memory.
func cacheOptions(cfg config.Config) cache.Options {
	opts := cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {
				Label: labels.SelectorFromSet(labels.Set{registry.CABundleLabel: "true"}),
			},
		},
	}

	if !cfg.Watch.ClusterScoped() {
		opts.DefaultNamespaces = make(map[string]cache.Config, len(cfg.Watch.Namespaces))
		for _, ns := range cfg.Watch.Namespaces {
			opts.DefaultNamespaces[ns] = cache.Config{}
		}
	}
	return opts
}

func newGateway(cfg config.Config, store *registry.Store, logger *slog.Logger) (*proxy.Server, error) {
	serverTLS, err := buildServerTLS(cfg.Proxy)
	if err != nil {
		return nil, err
	}

	return proxy.New(proxy.Config{
		ListenAddress:       cfg.Proxy.BindAddress,
		Routes:              store,
		TLS:                 serverTLS,
		MaxConnections:      cfg.Proxy.MaxConnections,
		StartupTimeout:      cfg.Proxy.StartupTimeout,
		DialTimeout:         cfg.Proxy.DialTimeout,
		ShutdownGracePeriod: cfg.Proxy.ShutdownGracePeriod,
		KeepAlivePeriod:     cfg.Proxy.KeepAlivePeriod,
		Logger:              logger.With("component", "gateway"),
	})
}

func buildServerTLS(cfg config.ProxyConfig) (*proxy.ServerTLS, error) {
	if cfg.TLSMode == config.TLSDisable {
		return nil, nil
	}

	certificate, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading the client-facing TLS keypair: %w", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	}

	if cfg.TLSClientCAFile != "" {
		pem, err := os.ReadFile(cfg.TLSClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("reading the client CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("client CA bundle %s holds no PEM certificates", cfg.TLSClientCAFile)
		}
		tlsConfig.ClientCAs = pool
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return &proxy.ServerTLS{Config: tlsConfig, Required: cfg.TLSMode == config.TLSRequire}, nil
}

// readinessCheck reports whether the gateway can serve traffic.
//
// Cache sync needs no explicit check: the manager only starts non-leader-elected
// runnables, this probe server among them, once the informers have synced. Until
// then the probe port is closed, which the kubelet already reads as not ready.
//
// It deliberately does not require any routes to exist. A cluster with no
// PostgresRoute yet is a correctly running gateway, not an unhealthy one — the
// previous implementation returned 503 in that case and restart-looped forever.
func readinessCheck(gateway *proxy.Server) health.Check {
	return func() error {
		if gateway != nil && !gateway.Listening() {
			return errors.New("the gateway is not accepting connections yet")
		}
		return nil
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

func scopeDescription(cfg config.Config) string {
	if cfg.Watch.ClusterScoped() {
		return "cluster"
	}
	return fmt.Sprintf("namespaces=%v", cfg.Watch.Namespaces)
}

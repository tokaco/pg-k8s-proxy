// Package config defines the gateway's runtime configuration and its flags.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/labels"
)

// Role selects which halves of the binary run in this process. A single image
// with two roles keeps the control plane and the data plane on exactly the same
// build, and lets small installations run both in one pod.
type Role string

const (
	// RoleAll runs the control plane and the gateway together. This is the default.
	RoleAll Role = "all"
	// RoleManager runs only the reconcilers, for a split deployment.
	RoleManager Role = "manager"
	// RoleProxy runs only the gateway, watching routes read-only.
	RoleProxy Role = "proxy"
)

// RunsProxy reports whether the role serves PostgreSQL traffic.
func (r Role) RunsProxy() bool { return r == RoleAll || r == RoleProxy }

// RunsManager reports whether the role reconciles and writes status.
func (r Role) RunsManager() bool { return r == RoleAll || r == RoleManager }

// TLSMode controls the client-facing side of the gateway.
type TLSMode string

const (
	// TLSDisable accepts only plaintext connections.
	TLSDisable TLSMode = "disable"
	// TLSAllow accepts TLS when the client asks for it.
	TLSAllow TLSMode = "allow"
	// TLSRequire rejects clients that will not negotiate TLS.
	TLSRequire TLSMode = "require"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	Role Role

	// Proxy is the client-facing data plane.
	Proxy ProxyConfig
	// Watch scopes what the controllers see.
	Watch WatchConfig
	// Discovery configures label-based Service adoption.
	Discovery DiscoveryConfig
	// Leader configures leader election for the control plane.
	Leader LeaderConfig

	// HealthBindAddress serves the liveness and readiness probes.
	HealthBindAddress string
	// MetricsBindAddress serves Prometheus metrics. "0" disables it.
	MetricsBindAddress string

	// LogLevel is one of debug, info, warn, error.
	LogLevel string
	// LogFormat is json or text.
	LogFormat string
}

// ProxyConfig configures the data plane.
type ProxyConfig struct {
	BindAddress         string
	MaxConnections      int
	StartupTimeout      time.Duration
	DialTimeout         time.Duration
	ShutdownGracePeriod time.Duration
	KeepAlivePeriod     time.Duration

	TLSMode     TLSMode
	TLSCertFile string
	TLSKeyFile  string
	// TLSClientCAFile enables verification of client certificates.
	TLSClientCAFile string
}

// WatchConfig scopes the controllers.
type WatchConfig struct {
	// Namespaces limits the controllers to these namespaces. Empty means the
	// whole cluster, which requires cluster-scoped RBAC.
	Namespaces []string
	// ClusterDomain is the cluster's DNS suffix.
	ClusterDomain string
	// WatchSecrets enables the Secret informer used for backend CA bundles.
	WatchSecrets bool
}

// ClusterScoped reports whether the controllers watch the whole cluster.
func (w WatchConfig) ClusterScoped() bool { return len(w.Namespaces) == 0 }

// DiscoveryConfig configures label-based Service adoption.
type DiscoveryConfig struct {
	Enabled            bool
	LabelSelector      string
	DatabaseAnnotation string

	selector labels.Selector
}

// Selector returns the parsed label selector. Valid only after Validate.
func (d DiscoveryConfig) Selector() labels.Selector { return d.selector }

// LeaderConfig configures leader election.
type LeaderConfig struct {
	Enabled       bool
	ID            string
	Namespace     string
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

// Default returns the configuration before flags and environment are applied.
func Default() Config {
	return Config{
		Role: RoleAll,
		Proxy: ProxyConfig{
			BindAddress:         ":5432",
			MaxConnections:      0,
			StartupTimeout:      30 * time.Second,
			DialTimeout:         5 * time.Second,
			ShutdownGracePeriod: 30 * time.Second,
			KeepAlivePeriod:     30 * time.Second,
			TLSMode:             TLSDisable,
		},
		Watch: WatchConfig{
			ClusterDomain: "cluster.local",
		},
		Discovery: DiscoveryConfig{
			Enabled:            true,
			LabelSelector:      "app.kubernetes.io/name=postgresql",
			DatabaseAnnotation: "pgproxy.io/database-name",
		},
		Leader: LeaderConfig{
			Enabled:       true,
			ID:            "pg-k8s-proxy.pgproxy.io",
			LeaseDuration: 15 * time.Second,
			RenewDeadline: 10 * time.Second,
			RetryPeriod:   2 * time.Second,
		},
		HealthBindAddress:  ":8080",
		MetricsBindAddress: ":9090",
		LogLevel:           "info",
		LogFormat:          "json",
	}
}

// BindFlags registers every flag onto fs.
func (c *Config) BindFlags(fs *pflag.FlagSet) {
	fs.Var((*roleValue)(&c.Role), "role", "Which halves to run: all, manager, or proxy.")

	fs.StringVar(&c.Proxy.BindAddress, "proxy-bind-address", c.Proxy.BindAddress,
		"Address the PostgreSQL gateway listens on.")
	fs.IntVar(&c.Proxy.MaxConnections, "proxy-max-connections", c.Proxy.MaxConnections,
		"Maximum concurrent client sessions. 0 means unlimited.")
	fs.DurationVar(&c.Proxy.StartupTimeout, "proxy-startup-timeout", c.Proxy.StartupTimeout,
		"How long a client has to complete the PostgreSQL handshake.")
	fs.DurationVar(&c.Proxy.DialTimeout, "proxy-dial-timeout", c.Proxy.DialTimeout,
		"How long to wait when connecting to a backend.")
	fs.DurationVar(&c.Proxy.ShutdownGracePeriod, "proxy-shutdown-grace-period", c.Proxy.ShutdownGracePeriod,
		"How long established sessions may continue after shutdown begins.")
	fs.DurationVar(&c.Proxy.KeepAlivePeriod, "proxy-keepalive-period", c.Proxy.KeepAlivePeriod,
		"TCP keepalive interval on client and backend connections.")

	fs.Var((*tlsModeValue)(&c.Proxy.TLSMode), "proxy-tls-mode",
		"Client-facing TLS: disable, allow, or require.")

	fs.StringVar(&c.Proxy.TLSCertFile, "proxy-tls-cert-file", c.Proxy.TLSCertFile,
		"PEM certificate presented to clients.")
	fs.StringVar(&c.Proxy.TLSKeyFile, "proxy-tls-key-file", c.Proxy.TLSKeyFile,
		"PEM private key for the client-facing certificate.")
	fs.StringVar(&c.Proxy.TLSClientCAFile, "proxy-tls-client-ca-file", c.Proxy.TLSClientCAFile,
		"PEM CA bundle used to require and verify client certificates.")

	fs.StringSliceVar(&c.Watch.Namespaces, "watch-namespaces", c.Watch.Namespaces,
		"Namespaces to watch. Empty watches the whole cluster and needs cluster-scoped RBAC.")
	fs.StringVar(&c.Watch.ClusterDomain, "cluster-domain", c.Watch.ClusterDomain,
		"Cluster DNS suffix used to build Service addresses.")
	fs.BoolVar(&c.Watch.WatchSecrets, "watch-secrets", c.Watch.WatchSecrets,
		"Watch labelled Secrets so routes can verify backend certificates against a CA bundle.")

	fs.BoolVar(&c.Discovery.Enabled, "service-discovery", c.Discovery.Enabled,
		"Generate PostgresRoutes from Services matching the label selector.")
	fs.StringVar(&c.Discovery.LabelSelector, "service-discovery-selector", c.Discovery.LabelSelector,
		"Label selector picking the Services to adopt.")
	fs.StringVar(&c.Discovery.DatabaseAnnotation, "service-discovery-database-annotation", c.Discovery.DatabaseAnnotation,
		"Service annotation carrying the database name.")

	fs.BoolVar(&c.Leader.Enabled, "leader-elect", c.Leader.Enabled,
		"Elect a leader so that only one replica writes status.")
	fs.StringVar(&c.Leader.ID, "leader-elect-id", c.Leader.ID,
		"Name of the lease used for leader election.")
	fs.StringVar(&c.Leader.Namespace, "leader-elect-namespace", c.Leader.Namespace,
		"Namespace holding the lease. Defaults to the pod's own namespace.")
	fs.DurationVar(&c.Leader.LeaseDuration, "leader-elect-lease-duration", c.Leader.LeaseDuration,
		"How long a lease is valid without renewal.")
	fs.DurationVar(&c.Leader.RenewDeadline, "leader-elect-renew-deadline", c.Leader.RenewDeadline,
		"How long the leader retries renewal before giving up.")
	fs.DurationVar(&c.Leader.RetryPeriod, "leader-elect-retry-period", c.Leader.RetryPeriod,
		"How often non-leaders retry acquisition.")

	fs.StringVar(&c.HealthBindAddress, "health-bind-address", c.HealthBindAddress,
		"Address serving the liveness and readiness probes.")
	fs.StringVar(&c.MetricsBindAddress, "metrics-bind-address", c.MetricsBindAddress,
		"Address serving Prometheus metrics. Set to 0 to disable.")

	fs.StringVar(&c.LogLevel, "log-level", c.LogLevel, "Log level: debug, info, warn, or error.")
	fs.StringVar(&c.LogFormat, "log-format", c.LogFormat, "Log format: json or text.")
}

// Validate resolves derived fields and rejects contradictory settings.
func (c *Config) Validate() error {
	switch c.Role {
	case RoleAll, RoleManager, RoleProxy:
	default:
		return fmt.Errorf("--role must be all, manager, or proxy, got %q", c.Role)
	}

	switch c.Proxy.TLSMode {
	case TLSDisable:
	case TLSAllow, TLSRequire:
		if c.Proxy.TLSCertFile == "" || c.Proxy.TLSKeyFile == "" {
			return fmt.Errorf("--proxy-tls-mode=%s needs both --proxy-tls-cert-file and --proxy-tls-key-file", c.Proxy.TLSMode)
		}
	default:
		return fmt.Errorf("--proxy-tls-mode must be disable, allow, or require, got %q", c.Proxy.TLSMode)
	}

	if c.Proxy.MaxConnections < 0 {
		return fmt.Errorf("--proxy-max-connections must not be negative, got %d", c.Proxy.MaxConnections)
	}
	if c.Watch.ClusterDomain == "" {
		return fmt.Errorf("--cluster-domain must not be empty")
	}

	if c.Discovery.Enabled {
		if !c.Role.RunsManager() {
			// Discovery writes PostgresRoutes, which only the manager may do.
			c.Discovery.Enabled = false
		} else {
			selector, err := labels.Parse(c.Discovery.LabelSelector)
			if err != nil {
				return fmt.Errorf("--service-discovery-selector %q: %w", c.Discovery.LabelSelector, err)
			}
			if selector.Empty() {
				return fmt.Errorf("--service-discovery-selector must not be empty when service discovery is enabled; " +
					"an empty selector would adopt every Service in scope")
			}
			c.Discovery.selector = selector
		}
	}

	if c.Leader.Enabled && !c.Role.RunsManager() {
		// A proxy-only replica writes nothing, so a lease would only add churn.
		c.Leader.Enabled = false
	}
	if c.Leader.Namespace == "" {
		c.Leader.Namespace = currentNamespace()
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("--log-level must be debug, info, warn, or error, got %q", c.LogLevel)
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("--log-format must be json or text, got %q", c.LogFormat)
	}

	// Trim empty entries so that --watch-namespaces="" reads as cluster scope.
	namespaces := c.Watch.Namespaces[:0]
	for _, ns := range c.Watch.Namespaces {
		if trimmed := strings.TrimSpace(ns); trimmed != "" {
			namespaces = append(namespaces, trimmed)
		}
	}
	c.Watch.Namespaces = namespaces

	return nil
}

// currentNamespace reads the namespace from the projected service account
// token, falling back to the POD_NAMESPACE environment variable.
func currentNamespace() string {
	const tokenNamespace = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	if data, err := os.ReadFile(tokenNamespace); err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	return os.Getenv("POD_NAMESPACE")
}

// roleValue and tlsModeValue adapt the typed enums to pflag.Value. Neither Set
// validates: reporting "must be all, manager, or proxy" from Validate, together
// with every other configuration error, beats failing on the first bad flag.
type roleValue Role

// String renders the current role.
func (r *roleValue) String() string { return string(*r) }

// Set records the flag value verbatim; Validate rejects unknown roles.
func (r *roleValue) Set(s string) error { *r = roleValue(s); return nil }

// Type names the value in generated usage text.
func (r *roleValue) Type() string { return "role" }

type tlsModeValue TLSMode

// String renders the current TLS mode.
func (t *tlsModeValue) String() string { return string(*t) }

// Set records the flag value verbatim; Validate rejects unknown modes.
func (t *tlsModeValue) Set(s string) error { *t = tlsModeValue(s); return nil }

// Type names the value in generated usage text.
func (t *tlsModeValue) Type() string { return "tlsMode" }

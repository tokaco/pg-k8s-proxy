package config

import (
	"testing"

	"github.com/spf13/pflag"
)

func parseFlags(t *testing.T, args ...string) (Config, *pflag.FlagSet) {
	t.Helper()

	cfg := Default()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	cfg.BindFlags(fs)

	if err := fs.Parse(args); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}
	return cfg, fs
}

func TestDefaultsValidate(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the default configuration is invalid: %v", err)
	}
	if !cfg.Watch.ClusterScoped() {
		t.Error("the default configuration is not cluster-scoped")
	}
	if cfg.Discovery.Selector() == nil {
		t.Error("the discovery selector was not parsed")
	}
}

func TestRoleFlagParsesTheEnum(t *testing.T) {
	cfg, _ := parseFlags(t, "--role=proxy")
	if cfg.Role != RoleProxy {
		t.Fatalf("Role = %q, want %q", cfg.Role, RoleProxy)
	}
	if cfg.Role.RunsManager() {
		t.Error("the proxy role reports that it runs the manager")
	}
	if !cfg.Role.RunsProxy() {
		t.Error("the proxy role reports that it does not run the proxy")
	}
}

// A proxy-only replica has no write permissions, so both writing features must
// switch themselves off rather than crash-looping on Forbidden.
func TestValidateDisablesWritingFeaturesForTheProxyRole(t *testing.T) {
	cfg, _ := parseFlags(t, "--role=proxy")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if cfg.Discovery.Enabled {
		t.Error("service discovery stayed on for a proxy-only replica")
	}
	if cfg.Leader.Enabled {
		t.Error("leader election stayed on for a proxy-only replica")
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "unknown role", args: []string{"--role=nonsense"}},
		{name: "unknown TLS mode", args: []string{"--proxy-tls-mode=maybe"}},
		{name: "TLS without a certificate", args: []string{"--proxy-tls-mode=require"}},
		{name: "negative connection limit", args: []string{"--proxy-max-connections=-1"}},
		{name: "empty cluster domain", args: []string{"--cluster-domain="}},
		{name: "malformed selector", args: []string{"--service-discovery-selector=!!!"}},
		{name: "empty selector", args: []string{"--service-discovery-selector="}},
		{name: "unknown log level", args: []string{"--log-level=verbose"}},
		{name: "unknown log format", args: []string{"--log-format=xml"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := parseFlags(t, tc.args...)
			if err := cfg.Validate(); err == nil {
				t.Error("expected an error, got none")
			}
		})
	}
}

func TestValidateAcceptsTLSWithAKeypair(t *testing.T) {
	cfg, _ := parseFlags(t,
		"--proxy-tls-mode=require",
		"--proxy-tls-cert-file=/tls/tls.crt",
		"--proxy-tls-key-file=/tls/tls.key",
	)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Proxy.TLSMode != TLSRequire {
		t.Errorf("TLSMode = %q, want %q", cfg.Proxy.TLSMode, TLSRequire)
	}
}

func TestWatchNamespacesSwitchTheScope(t *testing.T) {
	cfg, _ := parseFlags(t, "--watch-namespaces=apps,databases")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if cfg.Watch.ClusterScoped() {
		t.Error("listing namespaces did not narrow the scope")
	}
	if len(cfg.Watch.Namespaces) != 2 {
		t.Errorf("Namespaces = %v, want two entries", cfg.Watch.Namespaces)
	}
}

// The chart renders --watch-namespaces unconditionally in some paths, so an
// empty value has to read as cluster scope rather than as a namespace named "".
func TestEmptyWatchNamespacesMeansClusterScope(t *testing.T) {
	cfg, _ := parseFlags(t, "--watch-namespaces=")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !cfg.Watch.ClusterScoped() {
		t.Errorf("Namespaces = %v, want cluster scope", cfg.Watch.Namespaces)
	}
}

func TestApplyEnvironmentHonoursLegacyVariables(t *testing.T) {
	t.Setenv("POSTGRES_PORT", "6432")
	t.Setenv("HEALTH_PORT", "9999")
	t.Setenv("LABEL_SELECTOR", "app=postgres")
	t.Setenv("KUBERNETES_NAMESPACE", "legacy")
	t.Setenv("POLL_INTERVAL", "30s")

	cfg, fs := parseFlags(t)
	warnings := cfg.ApplyEnvironment(func(name string) bool { return fs.Changed(name) })

	if cfg.Proxy.BindAddress != ":6432" {
		t.Errorf("BindAddress = %q, want %q", cfg.Proxy.BindAddress, ":6432")
	}
	if cfg.HealthBindAddress != ":9999" {
		t.Errorf("HealthBindAddress = %q, want %q", cfg.HealthBindAddress, ":9999")
	}
	if cfg.Discovery.LabelSelector != "app=postgres" {
		t.Errorf("LabelSelector = %q, want %q", cfg.Discovery.LabelSelector, "app=postgres")
	}
	if len(cfg.Watch.Namespaces) != 1 || cfg.Watch.Namespaces[0] != "legacy" {
		t.Errorf("Namespaces = %v, want [legacy]", cfg.Watch.Namespaces)
	}
	if len(warnings) != 5 {
		t.Errorf("got %d deprecation warnings, want 5: %v", len(warnings), warnings)
	}
}

// An operator who sets a flag has been explicit; a leftover environment
// variable from the old Deployment must not silently override it.
func TestExplicitFlagsBeatTheEnvironment(t *testing.T) {
	t.Setenv("POSTGRES_PORT", "6432")

	cfg, fs := parseFlags(t, "--proxy-bind-address=:15432")
	cfg.ApplyEnvironment(func(name string) bool { return fs.Changed(name) })

	if cfg.Proxy.BindAddress != ":15432" {
		t.Errorf("BindAddress = %q, want the flag value %q", cfg.Proxy.BindAddress, ":15432")
	}
}

func TestApplyEnvironmentIgnoresAMalformedPort(t *testing.T) {
	t.Setenv("POSTGRES_PORT", "not-a-port")

	cfg, fs := parseFlags(t)
	warnings := cfg.ApplyEnvironment(func(name string) bool { return fs.Changed(name) })

	if cfg.Proxy.BindAddress != ":5432" {
		t.Errorf("BindAddress = %q, want the default %q", cfg.Proxy.BindAddress, ":5432")
	}
	if len(warnings) != 1 {
		t.Errorf("got %d warnings, want 1: %v", len(warnings), warnings)
	}
}

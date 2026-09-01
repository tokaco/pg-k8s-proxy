package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// ApplyEnvironment maps the environment variables the pre-operator deployment
// used onto the current configuration, returning a deprecation warning for each
// one it honoured. Flags always win: the environment only changes a default, so
// an explicitly set flag is never overridden.
//
// Honouring these means an existing Deployment can be pointed at the new image
// without editing its env block first.
func (c *Config) ApplyEnvironment(explicitlySet func(flag string) bool) []string {
	var warnings []string

	setPort := func(env, flag string, target *string) {
		if explicitlySet(flag) {
			return
		}
		raw, ok := os.LookupEnv(env)
		if !ok || raw == "" {
			return
		}
		if _, err := strconv.Atoi(raw); err != nil {
			warnings = append(warnings, fmt.Sprintf("ignoring %s=%q: not a port number", env, raw))
			return
		}
		*target = net.JoinHostPort("", raw)
		warnings = append(warnings, fmt.Sprintf("%s is deprecated, use --%s", env, flag))
	}

	setPort("POSTGRES_PORT", "proxy-bind-address", &c.Proxy.BindAddress)
	setPort("HEALTH_PORT", "health-bind-address", &c.HealthBindAddress)

	if raw, ok := os.LookupEnv("LABEL_SELECTOR"); ok && raw != "" && !explicitlySet("service-discovery-selector") {
		c.Discovery.LabelSelector = raw
		warnings = append(warnings, "LABEL_SELECTOR is deprecated, use --service-discovery-selector")
	}

	if raw, ok := os.LookupEnv("KUBERNETES_NAMESPACE"); ok && raw != "" && !explicitlySet("watch-namespaces") {
		c.Watch.Namespaces = strings.Split(raw, ",")
		warnings = append(warnings, "KUBERNETES_NAMESPACE is deprecated, use --watch-namespaces")
	}

	if _, ok := os.LookupEnv("POLL_INTERVAL"); ok {
		warnings = append(warnings,
			"POLL_INTERVAL is ignored: the gateway now watches the API server instead of polling it")
	}

	return warnings
}

package proxy

import (
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Outcome labels for connectionsTotal.
const (
	outcomeProxied       = "proxied"
	outcomeNoRoute       = "no_route"
	outcomeBackendFailed = "backend_unreachable"
	outcomeRejected      = "rejected"
	outcomeHandshake     = "handshake_failed"
	outcomeCancel        = "cancel"
)

var (
	connectionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgproxy_connections_total",
		Help: "Client connections by requested database and outcome.",
	}, []string{"database", "outcome"})

	activeConnections = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgproxy_active_connections",
		Help: "Client connections currently being proxied, by database.",
	}, []string{"database"})

	bytesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgproxy_bytes_total",
		Help: "Bytes relayed, by database and direction.",
	}, []string{"database", "direction"})

	backendDialDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pgproxy_backend_dial_duration_seconds",
		Help:    "Time spent establishing the backend connection.",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 12),
	}, []string{"database"})

	sessionDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pgproxy_session_duration_seconds",
		Help:    "Lifetime of a proxied session.",
		Buckets: prometheus.ExponentialBuckets(0.01, 4, 10),
	}, []string{"database"})

	routesGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pgproxy_routes",
		Help: "Databases currently routable by this replica.",
	})

	cancelRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgproxy_cancel_requests_total",
		Help: "Query cancellation requests by outcome.",
	}, []string{"outcome"})
)

func init() {
	crmetrics.Registry.MustRegister(
		connectionsTotal,
		activeConnections,
		bytesTotal,
		backendDialDuration,
		sessionDuration,
		routesGauge,
		cancelRequestsTotal,
	)
}

// databaseLabel bounds metric cardinality. An unrouted database name is
// attacker-controlled, so it is only ever reported as a single bucket.
func databaseLabel(database string, routed bool) string {
	if !routed {
		return "<unrouted>"
	}
	return database
}

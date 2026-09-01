// Package registry turns a set of PostgresRoute objects into the flat lookup
// table the proxy consults on every incoming connection.
//
// Building the table is a deterministic, side-effect-free function of the route
// set. Both the elected leader (which writes the result to status) and every
// non-leader replica (which routes traffic) run it and reach the same answer,
// so the data plane never has to wait for a leader before serving.
package registry

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	pgproxyv1alpha1 "github.com/tokaco/pg-k8s-proxy/api/v1alpha1"
)

// Classes of resolution failure. The controller reports each as its own status
// reason, so an operator can tell a missing Service from a bad port reference
// from an unreadable CA bundle without parsing the message.
var (
	// ErrBackendNotFound means the referenced Service does not exist.
	ErrBackendNotFound = errors.New("backend not found")
	// ErrPortNotFound means the Service exists but exposes no such port.
	ErrPortNotFound = errors.New("port not found")
	// ErrInvalidBackend means the backend reference is internally inconsistent.
	ErrInvalidBackend = errors.New("invalid backend")
	// ErrCABundle means the backend CA bundle could not be loaded.
	ErrCABundle = errors.New("CA bundle unavailable")
)

// ResolveTimeout bounds the resolution of a single route.
//
// A cache read can block: controller-runtime starts an informer lazily on first
// use and waits for it to sync, which never happens if RBAC forbids the list.
// Without this bound one route pointing at an unreadable object would wedge the
// rebuild loop and freeze the routing table for every other route as well.
const ResolveTimeout = 10 * time.Second

// Resolver supplies the cluster state needed to turn a backend reference into a
// concrete address. It is backed by the shared controller-runtime cache.
type Resolver interface {
	// ResolveServicePort maps a Service port reference, which may be a number
	// or a port name, onto a concrete port number.
	ResolveServicePort(ctx context.Context, namespace, name string, port intstr.IntOrString) (int32, error)
	// LoadCABundle reads a CA bundle out of a Secret key.
	LoadCABundle(ctx context.Context, namespace, secretName, key string) (*x509.CertPool, error)
}

// TLSConfig is the resolved proxy-to-backend TLS policy for one route.
type TLSConfig struct {
	Mode       pgproxyv1alpha1.BackendTLSMode
	RootCAs    *x509.CertPool
	ServerName string
}

// Enabled reports whether the proxy should negotiate TLS with the backend.
func (t TLSConfig) Enabled() bool {
	return t.Mode != "" && t.Mode != pgproxyv1alpha1.BackendTLSDisable
}

// Route is a fully resolved routing decision, ready for the data plane.
type Route struct {
	// Source is the PostgresRoute this entry came from.
	Source types.NamespacedName
	// Database is the name clients connect with.
	Database string
	// TargetDatabase is the name forwarded to the backend.
	TargetDatabase string
	// Host and Port address the backend.
	Host string
	Port int32
	// TLS governs the proxy-to-backend leg.
	TLS TLSConfig
}

// Address returns the dial target in host:port form.
func (r Route) Address() string {
	return net.JoinHostPort(r.Host, strconv.Itoa(int(r.Port)))
}

// Table is an immutable snapshot of the routing decisions. Snapshots are
// replaced wholesale rather than mutated, so lookups need no locking.
type Table struct {
	routes map[string]Route
}

// NewTable builds a snapshot from a database-name keyed map. The caller must
// not retain or mutate routes afterwards.
func NewTable(routes map[string]Route) *Table {
	if routes == nil {
		routes = map[string]Route{}
	}
	return &Table{routes: routes}
}

// Lookup returns the route serving a database name.
func (t *Table) Lookup(database string) (Route, bool) {
	route, ok := t.routes[database]
	return route, ok
}

// Len returns the number of routable databases.
func (t *Table) Len() int { return len(t.routes) }

// Databases returns the routable database names in sorted order. It is meant
// for diagnostics, not the connection hot path.
func (t *Table) Databases() []string {
	names := make([]string, 0, len(t.routes))
	for name := range t.routes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Store holds the current Snapshot behind an atomic pointer so the control
// plane can publish a new one without ever blocking a connection handler.
type Store struct {
	current atomic.Pointer[Snapshot]
}

// NewStore returns a Store holding an empty snapshot.
func NewStore() *Store {
	s := &Store{}
	s.current.Store(&Snapshot{Table: NewTable(nil)})
	return s
}

// Load returns the current snapshot. It never returns nil.
func (s *Store) Load() *Snapshot { return s.current.Load() }

// Routes returns just the lookup table. This is the connection hot path.
func (s *Store) Routes() *Table { return s.current.Load().Table }

// Store publishes a new snapshot.
func (s *Store) Store(snapshot *Snapshot) {
	if snapshot == nil {
		snapshot = &Snapshot{}
	}
	if snapshot.Table == nil {
		snapshot.Table = NewTable(nil)
	}
	s.current.Store(snapshot)
}

// Decision records the outcome for a single PostgresRoute, so the controller
// can report it in status.
type Decision struct {
	// Database is the name the route claims.
	Database string
	// Accepted reports whether the route won the claim on that name.
	Accepted bool
	// ConflictsWith names the route that won, when Accepted is false.
	ConflictsWith types.NamespacedName
	// Endpoint is the resolved backend address, empty when resolution failed.
	Endpoint string
	// ResolveErr records why the backend could not be resolved.
	ResolveErr error
}

// Programmed reports whether the route is actually serving traffic.
func (d Decision) Programmed() bool { return d.Accepted && d.ResolveErr == nil }

// Snapshot pairs a routing table with the per-route reasoning behind it.
type Snapshot struct {
	// Table is what the data plane consults.
	Table *Table
	// Decisions explains the outcome for every route, for status reporting.
	Decisions map[types.NamespacedName]Decision
}

// Decision returns the recorded outcome for one route.
func (s *Snapshot) Decision(key types.NamespacedName) (Decision, bool) {
	decision, ok := s.Decisions[key]
	return decision, ok
}

// Build resolves a set of routes into a table plus the reasoning for each one.
//
// When several routes claim the same database name, the winner is the one with
// the highest spec.priority; ties are broken by the oldest creationTimestamp and
// finally by namespace/name, so every replica independently agrees on the winner.
//
// A route that wins its name but whose backend cannot be resolved is still
// Accepted — it owns the name — but is left out of the table.
func Build(ctx context.Context, routes []pgproxyv1alpha1.PostgresRoute, resolver Resolver, clusterDomain string) *Snapshot {
	ordered := make([]*pgproxyv1alpha1.PostgresRoute, len(routes))
	for i := range routes {
		ordered[i] = &routes[i]
	}
	sort.SliceStable(ordered, func(i, j int) bool { return less(ordered[i], ordered[j]) })

	result := &Snapshot{
		Table:     NewTable(nil),
		Decisions: make(map[types.NamespacedName]Decision, len(ordered)),
	}
	table := make(map[string]Route, len(ordered))
	winners := make(map[string]types.NamespacedName, len(ordered))

	for _, route := range ordered {
		key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
		database := route.EffectiveDatabase()

		if winner, taken := winners[database]; taken {
			result.Decisions[key] = Decision{
				Database:      database,
				Accepted:      false,
				ConflictsWith: winner,
			}
			continue
		}
		winners[database] = key

		resolved, err := resolveBounded(ctx, route, resolver, clusterDomain)
		if err != nil {
			result.Decisions[key] = Decision{Database: database, Accepted: true, ResolveErr: err}
			continue
		}

		table[database] = resolved
		result.Decisions[key] = Decision{
			Database: database,
			Accepted: true,
			Endpoint: resolved.Address(),
		}
	}

	result.Table = NewTable(table)
	return result
}

// less orders two routes by descending priority, then ascending age, then by
// namespace and name. The ordering is total, which is what makes the winner of
// a name conflict identical across replicas.
func less(a, b *pgproxyv1alpha1.PostgresRoute) bool {
	if a.Spec.Priority != b.Spec.Priority {
		return a.Spec.Priority > b.Spec.Priority
	}
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	return a.Name < b.Name
}

// resolveBounded applies ResolveTimeout so a single slow or hung lookup cannot
// hold up the rest of the table.
func resolveBounded(ctx context.Context, route *pgproxyv1alpha1.PostgresRoute, resolver Resolver, clusterDomain string) (Route, error) {
	ctx, cancel := context.WithTimeout(ctx, ResolveTimeout)
	defer cancel()
	return resolve(ctx, route, resolver, clusterDomain)
}

func resolve(ctx context.Context, route *pgproxyv1alpha1.PostgresRoute, resolver Resolver, clusterDomain string) (Route, error) {
	out := Route{
		Source:         types.NamespacedName{Namespace: route.Namespace, Name: route.Name},
		Database:       route.EffectiveDatabase(),
		TargetDatabase: route.EffectiveTargetDatabase(),
	}

	switch route.Spec.Backend.Type {
	case pgproxyv1alpha1.BackendTypeService:
		backend := route.Spec.Backend.Service
		if backend == nil {
			return Route{}, fmt.Errorf("%w: backend.type is Service but backend.service is unset", ErrInvalidBackend)
		}
		namespace := backend.Namespace
		if namespace == "" {
			namespace = route.Namespace
		}
		port, err := resolver.ResolveServicePort(ctx, namespace, backend.Name, backend.Port)
		if err != nil {
			return Route{}, err
		}
		out.Host = fmt.Sprintf("%s.%s.svc.%s", backend.Name, namespace, clusterDomain)
		out.Port = port

	case pgproxyv1alpha1.BackendTypeAddress:
		backend := route.Spec.Backend.Address
		if backend == nil {
			return Route{}, fmt.Errorf("%w: backend.type is Address but backend.address is unset", ErrInvalidBackend)
		}
		out.Host = backend.Host
		out.Port = backend.Port
		if out.Port == 0 {
			out.Port = 5432
		}

	default:
		return Route{}, fmt.Errorf("%w: unsupported backend type %q", ErrInvalidBackend, route.Spec.Backend.Type)
	}

	tls, err := resolveTLS(ctx, route, resolver)
	if err != nil {
		return Route{}, err
	}
	out.TLS = tls

	return out, nil
}

func resolveTLS(ctx context.Context, route *pgproxyv1alpha1.PostgresRoute, resolver Resolver) (TLSConfig, error) {
	spec := route.Spec.TLS
	if spec == nil || spec.Mode == "" || spec.Mode == pgproxyv1alpha1.BackendTLSDisable {
		return TLSConfig{Mode: pgproxyv1alpha1.BackendTLSDisable}, nil
	}

	out := TLSConfig{Mode: spec.Mode, ServerName: spec.ServerName}

	needsCA := spec.Mode == pgproxyv1alpha1.BackendTLSVerifyCA || spec.Mode == pgproxyv1alpha1.BackendTLSVerifyFull
	if !needsCA {
		return out, nil
	}
	if spec.CASecretRef == nil {
		// Fall back to the system trust store, which is what a cluster using
		// publicly signed or webhook-injected certificates will want.
		return out, nil
	}

	key := spec.CASecretRef.Key
	if key == "" {
		key = "ca.crt"
	}
	pool, err := resolver.LoadCABundle(ctx, route.Namespace, spec.CASecretRef.Name, key)
	if err != nil {
		return TLSConfig{}, fmt.Errorf("%w: %w", ErrCABundle, err)
	}
	out.RootCAs = pool
	return out, nil
}

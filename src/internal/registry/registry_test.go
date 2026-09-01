package registry

import (
	"context"
	"crypto/x509"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	pgproxyv1alpha1 "github.com/tokaco/pg-k8s-proxy/api/v1alpha1"
)

const clusterDomain = "cluster.local"

// stubResolver answers from a fixed table so the tests exercise Build's own
// logic rather than the Kubernetes client.
type stubResolver struct {
	ports map[string]int32
	err   error
}

func (s stubResolver) ResolveServicePort(_ context.Context, namespace, name string, port intstr.IntOrString) (int32, error) {
	if s.err != nil {
		return 0, s.err
	}
	if resolved, ok := s.ports[namespace+"/"+name]; ok {
		return resolved, nil
	}
	if port.Type == intstr.Int && port.IntVal != 0 {
		return port.IntVal, nil
	}
	return 0, fmt.Errorf("service %s/%s not found", namespace, name)
}

func (s stubResolver) LoadCABundle(context.Context, string, string, string) (*x509.CertPool, error) {
	return x509.NewCertPool(), nil
}

func serviceRoute(namespace, name, database, serviceName string, age time.Duration, priority int32) pgproxyv1alpha1.PostgresRoute {
	return pgproxyv1alpha1.PostgresRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(time.Unix(0, 0).Add(age)),
		},
		Spec: pgproxyv1alpha1.PostgresRouteSpec{
			Database: database,
			Priority: priority,
			Backend: pgproxyv1alpha1.Backend{
				Type: pgproxyv1alpha1.BackendTypeService,
				Service: &pgproxyv1alpha1.ServiceBackend{
					Name: serviceName,
					Port: intstr.FromInt32(5432),
				},
			},
		},
	}
}

func TestBuildResolvesServiceBackendsToClusterDNS(t *testing.T) {
	routes := []pgproxyv1alpha1.PostgresRoute{
		serviceRoute("databases", "billing", "billing", "billing-rw", time.Hour, 0),
	}

	snapshot := Build(context.Background(), routes, stubResolver{}, clusterDomain)

	route, ok := snapshot.Table.Lookup("billing")
	if !ok {
		t.Fatalf("billing is not routable; table has %v", snapshot.Table.Databases())
	}
	if want := "billing-rw.databases.svc.cluster.local"; route.Host != want {
		t.Errorf("Host = %q, want %q", route.Host, want)
	}
	if route.Port != 5432 {
		t.Errorf("Port = %d, want 5432", route.Port)
	}
	if want := "billing-rw.databases.svc.cluster.local:5432"; route.Address() != want {
		t.Errorf("Address() = %q, want %q", route.Address(), want)
	}
	if route.TargetDatabase != "billing" {
		t.Errorf("TargetDatabase = %q, want %q", route.TargetDatabase, "billing")
	}
}

func TestBuildDefaultsTheDatabaseNameToTheObjectName(t *testing.T) {
	routes := []pgproxyv1alpha1.PostgresRoute{
		serviceRoute("apps", "analytics", "", "analytics-db", time.Hour, 0),
	}

	snapshot := Build(context.Background(), routes, stubResolver{}, clusterDomain)

	if _, ok := snapshot.Table.Lookup("analytics"); !ok {
		t.Errorf("route did not claim its object name; table has %v", snapshot.Table.Databases())
	}
}

func TestBuildRewritesTheTargetDatabase(t *testing.T) {
	route := serviceRoute("apps", "public-name", "public-name", "pg", time.Hour, 0)
	route.Spec.TargetDatabase = "internal_name"

	snapshot := Build(context.Background(), []pgproxyv1alpha1.PostgresRoute{route}, stubResolver{}, clusterDomain)

	resolved, ok := snapshot.Table.Lookup("public-name")
	if !ok {
		t.Fatal("route is not routable")
	}
	if resolved.TargetDatabase != "internal_name" {
		t.Errorf("TargetDatabase = %q, want %q", resolved.TargetDatabase, "internal_name")
	}
}

func TestBuildResolvesAddressBackends(t *testing.T) {
	routes := []pgproxyv1alpha1.PostgresRoute{{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "apps"},
		Spec: pgproxyv1alpha1.PostgresRouteSpec{
			Backend: pgproxyv1alpha1.Backend{
				Type:    pgproxyv1alpha1.BackendTypeAddress,
				Address: &pgproxyv1alpha1.AddressBackend{Host: "pg.example.com", Port: 6432},
			},
		},
	}}

	snapshot := Build(context.Background(), routes, stubResolver{}, clusterDomain)

	route, ok := snapshot.Table.Lookup("legacy")
	if !ok {
		t.Fatal("address-backed route is not routable")
	}
	if want := "pg.example.com:6432"; route.Address() != want {
		t.Errorf("Address() = %q, want %q", route.Address(), want)
	}
}

// Two routes claiming the same name must resolve the same way on every
// replica, or clients would reach different backends depending on which pod
// they landed on.
func TestBuildResolvesNameConflictsDeterministically(t *testing.T) {
	older := serviceRoute("a", "first", "shared", "svc-a", time.Hour, 0)
	newer := serviceRoute("b", "second", "shared", "svc-b", 2*time.Hour, 0)

	forward := Build(context.Background(), []pgproxyv1alpha1.PostgresRoute{older, newer}, stubResolver{}, clusterDomain)
	reversed := Build(context.Background(), []pgproxyv1alpha1.PostgresRoute{newer, older}, stubResolver{}, clusterDomain)

	for name, snapshot := range map[string]*Snapshot{"listed oldest first": forward, "listed newest first": reversed} {
		t.Run(name, func(t *testing.T) {
			route, ok := snapshot.Table.Lookup("shared")
			if !ok {
				t.Fatal("the shared name is not routable")
			}
			if want := "svc-a.a.svc.cluster.local"; route.Host != want {
				t.Errorf("the older route did not win: Host = %q, want %q", route.Host, want)
			}

			winner, _ := snapshot.Decision(types.NamespacedName{Namespace: "a", Name: "first"})
			if !winner.Accepted {
				t.Error("the older route was not marked Accepted")
			}

			loser, _ := snapshot.Decision(types.NamespacedName{Namespace: "b", Name: "second"})
			if loser.Accepted {
				t.Error("the newer route was marked Accepted despite losing")
			}
			if want := (types.NamespacedName{Namespace: "a", Name: "first"}); loser.ConflictsWith != want {
				t.Errorf("ConflictsWith = %v, want %v", loser.ConflictsWith, want)
			}
		})
	}
}

// Priority is what lets a hand-written route override a discovered one, which
// is generated with a negative priority.
func TestBuildPrefersHigherPriorityOverAge(t *testing.T) {
	discovered := serviceRoute("a", "discovered", "shared", "svc-a", time.Hour, -100)
	handWritten := serviceRoute("b", "explicit", "shared", "svc-b", 5*time.Hour, 0)

	snapshot := Build(context.Background(), []pgproxyv1alpha1.PostgresRoute{discovered, handWritten}, stubResolver{}, clusterDomain)

	route, ok := snapshot.Table.Lookup("shared")
	if !ok {
		t.Fatal("the shared name is not routable")
	}
	if want := "svc-b.b.svc.cluster.local"; route.Host != want {
		t.Errorf("the higher-priority route did not win: Host = %q, want %q", route.Host, want)
	}
}

// Identical priority and identical timestamps still need a total order, or
// replicas could disagree.
func TestBuildBreaksExactTiesByNamespaceAndName(t *testing.T) {
	first := serviceRoute("aaa", "route", "shared", "svc-1", time.Hour, 0)
	second := serviceRoute("bbb", "route", "shared", "svc-2", time.Hour, 0)

	snapshot := Build(context.Background(), []pgproxyv1alpha1.PostgresRoute{second, first}, stubResolver{}, clusterDomain)

	route, _ := snapshot.Table.Lookup("shared")
	if want := "svc-1.aaa.svc.cluster.local"; route.Host != want {
		t.Errorf("Host = %q, want %q", route.Host, want)
	}
}

// A route that wins its name but cannot be resolved keeps the name — yielding
// it would hand traffic to a route the operator did not choose.
func TestBuildKeepsAnUnresolvableRouteAcceptedButUnrouted(t *testing.T) {
	broken := serviceRoute("a", "broken", "billing", "missing-svc", time.Hour, 0)
	broken.Spec.Backend.Service.Port = intstr.FromString("nonexistent-port")
	fallback := serviceRoute("b", "fallback", "billing", "svc-b", 2*time.Hour, 0)

	snapshot := Build(context.Background(),
		[]pgproxyv1alpha1.PostgresRoute{broken, fallback},
		stubResolver{err: fmt.Errorf("no such port")}, clusterDomain)

	if _, ok := snapshot.Table.Lookup("billing"); ok {
		t.Error("an unresolvable route should not be routable")
	}

	decision, _ := snapshot.Decision(types.NamespacedName{Namespace: "a", Name: "broken"})
	if !decision.Accepted {
		t.Error("the winning route lost its name claim because resolution failed")
	}
	if decision.ResolveErr == nil {
		t.Error("ResolveErr was not recorded")
	}
	if decision.Programmed() {
		t.Error("Programmed() is true despite a resolution failure")
	}
}

func TestBuildRejectsAMismatchedBackendDiscriminator(t *testing.T) {
	routes := []pgproxyv1alpha1.PostgresRoute{{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "a"},
		Spec: pgproxyv1alpha1.PostgresRouteSpec{
			// The CRD's CEL rules reject this, but the table builder must not
			// depend on admission having run.
			Backend: pgproxyv1alpha1.Backend{Type: pgproxyv1alpha1.BackendTypeService},
		},
	}}

	snapshot := Build(context.Background(), routes, stubResolver{}, clusterDomain)

	decision, _ := snapshot.Decision(types.NamespacedName{Namespace: "a", Name: "bad"})
	if decision.ResolveErr == nil {
		t.Error("a Service backend with no service block was accepted")
	}
}

func TestBuildResolvesBackendTLSModes(t *testing.T) {
	tests := []struct {
		name        string
		tls         *pgproxyv1alpha1.BackendTLS
		wantEnabled bool
		wantRootCAs bool
	}{
		{name: "unset", tls: nil, wantEnabled: false},
		{name: "disable", tls: &pgproxyv1alpha1.BackendTLS{Mode: pgproxyv1alpha1.BackendTLSDisable}, wantEnabled: false},
		{name: "require", tls: &pgproxyv1alpha1.BackendTLS{Mode: pgproxyv1alpha1.BackendTLSRequire}, wantEnabled: true},
		{
			name: "verify-full with a CA secret",
			tls: &pgproxyv1alpha1.BackendTLS{
				Mode:        pgproxyv1alpha1.BackendTLSVerifyFull,
				CASecretRef: &pgproxyv1alpha1.SecretKeyReference{Name: "ca"},
			},
			wantEnabled: true,
			wantRootCAs: true,
		},
		{
			name:        "verify-full without a CA secret falls back to the system trust store",
			tls:         &pgproxyv1alpha1.BackendTLS{Mode: pgproxyv1alpha1.BackendTLSVerifyFull},
			wantEnabled: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			route := serviceRoute("a", "r", "db", "svc", time.Hour, 0)
			route.Spec.TLS = tc.tls

			snapshot := Build(context.Background(), []pgproxyv1alpha1.PostgresRoute{route}, stubResolver{}, clusterDomain)
			resolved, ok := snapshot.Table.Lookup("db")
			if !ok {
				t.Fatal("route is not routable")
			}
			if got := resolved.TLS.Enabled(); got != tc.wantEnabled {
				t.Errorf("TLS.Enabled() = %v, want %v", got, tc.wantEnabled)
			}
			if got := resolved.TLS.RootCAs != nil; got != tc.wantRootCAs {
				t.Errorf("RootCAs present = %v, want %v", got, tc.wantRootCAs)
			}
		})
	}
}

func TestStoreStartsEmptyAndPublishesSnapshots(t *testing.T) {
	store := NewStore()

	if store.Routes() == nil {
		t.Fatal("a fresh Store returned a nil table")
	}
	if store.Routes().Len() != 0 {
		t.Errorf("a fresh Store has %d routes, want 0", store.Routes().Len())
	}

	snapshot := Build(context.Background(),
		[]pgproxyv1alpha1.PostgresRoute{serviceRoute("a", "r", "db", "svc", time.Hour, 0)},
		stubResolver{}, clusterDomain)
	store.Store(snapshot)

	if _, ok := store.Routes().Lookup("db"); !ok {
		t.Error("the published snapshot is not visible through the Store")
	}

	// A nil snapshot must degrade to an empty table rather than panic the
	// connection handlers reading it.
	store.Store(nil)
	if store.Routes() == nil || store.Routes().Len() != 0 {
		t.Error("storing nil did not yield an empty table")
	}
}

// blockingResolver never returns until its context is done, which is what a
// controller-runtime cache read does when the informer it starts can never
// sync because RBAC forbids the list.
type blockingResolver struct{ started chan struct{} }

func (b blockingResolver) ResolveServicePort(ctx context.Context, _, _ string, _ intstr.IntOrString) (int32, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return 0, ctx.Err()
}

func (b blockingResolver) LoadCABundle(ctx context.Context, _, _, _ string) (*x509.CertPool, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// One route pointing at something unreadable must not freeze the routing table
// for every other route. Before this bound, a single such route wedged the
// rebuild loop and no route anywhere got a status again.
func TestBuildIsNotStalledByABlockingResolver(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the resolve timeout")
	}

	routes := []pgproxyv1alpha1.PostgresRoute{
		serviceRoute("a", "stuck", "stuck", "unreadable", time.Hour, 0),
	}

	done := make(chan *Snapshot, 1)
	go func() {
		done <- Build(context.Background(), routes, blockingResolver{started: make(chan struct{}, 1)}, clusterDomain)
	}()

	select {
	case snapshot := <-done:
		decision, _ := snapshot.Decision(types.NamespacedName{Namespace: "a", Name: "stuck"})
		if decision.ResolveErr == nil {
			t.Error("a resolution that timed out was not recorded as an error")
		}
	case <-time.After(ResolveTimeout + 20*time.Second):
		t.Fatal("Build never returned; a blocking resolver stalls the whole table")
	}
}

// The timeout applies per route, so a healthy route resolves normally even when
// another one in the same build is hung.
func TestBuildResolvesHealthyRoutesAlongsideABlockedOne(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the resolve timeout")
	}

	mixed := mixedResolver{
		blocked: "unreadable",
		ports:   map[string]int32{"b/good": 5432},
	}
	routes := []pgproxyv1alpha1.PostgresRoute{
		serviceRoute("a", "stuck", "stuck", "unreadable", time.Hour, 0),
		serviceRoute("b", "healthy", "healthy", "good", time.Hour, 0),
	}

	snapshot := Build(context.Background(), routes, mixed, clusterDomain)

	if _, ok := snapshot.Table.Lookup("healthy"); !ok {
		t.Errorf("the healthy route was not routable; table has %v", snapshot.Table.Databases())
	}
	if _, ok := snapshot.Table.Lookup("stuck"); ok {
		t.Error("the unresolvable route was added to the table")
	}
}

// mixedResolver blocks for one Service name and answers normally otherwise.
type mixedResolver struct {
	blocked string
	ports   map[string]int32
}

func (m mixedResolver) ResolveServicePort(ctx context.Context, namespace, name string, _ intstr.IntOrString) (int32, error) {
	if name == m.blocked {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if port, ok := m.ports[namespace+"/"+name]; ok {
		return port, nil
	}
	return 0, fmt.Errorf("service %s/%s not found", namespace, name)
}

func (m mixedResolver) LoadCABundle(context.Context, string, string, string) (*x509.CertPool, error) {
	return x509.NewCertPool(), nil
}

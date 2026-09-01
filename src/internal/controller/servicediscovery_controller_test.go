package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pgproxyv1alpha1 "github.com/tokaco/pg-k8s-proxy/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("registering the built-in types: %v", err)
	}
	if err := pgproxyv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering the pgproxy.io types: %v", err)
	}
	return scheme
}

func postgresService(name, namespace string, mutate func(*corev1.Service)) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(namespace + "/" + name),
			Labels:    map[string]string{"app.kubernetes.io/name": "postgresql"},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Name: "postgresql", Port: 5432}},
		},
	}
	if mutate != nil {
		mutate(svc)
	}
	return svc
}

func newDiscoveryReconciler(t *testing.T, objects ...client.Object) (*ServiceDiscoveryReconciler, client.Client) {
	t.Helper()

	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	selector, err := labels.Parse("app.kubernetes.io/name=postgresql")
	if err != nil {
		t.Fatalf("parsing the selector: %v", err)
	}

	return &ServiceDiscoveryReconciler{
		Client:             fakeClient,
		Scheme:             scheme,
		Selector:           selector,
		DatabaseAnnotation: DefaultDatabaseAnnotation,
	}, fakeClient
}

func reconcileService(t *testing.T, r *ServiceDiscoveryReconciler, namespace, name string) {
	t.Helper()

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: namespace, Name: name},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func getRoute(t *testing.T, c client.Client, namespace, name string) *pgproxyv1alpha1.PostgresRoute {
	t.Helper()

	var route pgproxyv1alpha1.PostgresRoute
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, &route); err != nil {
		t.Fatalf("getting route %s/%s: %v", namespace, name, err)
	}
	return &route
}

func TestDiscoveryGeneratesARouteOwnedByTheService(t *testing.T) {
	svc := postgresService("billing-db", "databases", nil)
	r, c := newDiscoveryReconciler(t, svc)

	reconcileService(t, r, "databases", "billing-db")

	route := getRoute(t, c, "databases", "billing-db-discovered")

	if route.Spec.Database != "billing-db" {
		t.Errorf("Database = %q, want %q", route.Spec.Database, "billing-db")
	}
	if route.Spec.Backend.Type != pgproxyv1alpha1.BackendTypeService {
		t.Errorf("Backend.Type = %q, want Service", route.Spec.Backend.Type)
	}
	if route.Spec.Backend.Service.Name != "billing-db" {
		t.Errorf("Backend.Service.Name = %q, want %q", route.Spec.Backend.Service.Name, "billing-db")
	}
	if route.Labels[ManagedByLabel] != ManagedByServiceDiscovery {
		t.Errorf("%s = %q, want %q", ManagedByLabel, route.Labels[ManagedByLabel], ManagedByServiceDiscovery)
	}

	// A negative priority is what keeps a hand-written route ahead of this one.
	if route.Spec.Priority != discoveredRoutePriority {
		t.Errorf("Priority = %d, want %d", route.Spec.Priority, discoveredRoutePriority)
	}

	owner := metav1.GetControllerOf(route)
	if owner == nil {
		t.Fatal("the generated route has no controller reference")
	}
	if owner.Kind != "Service" || owner.Name != "billing-db" {
		t.Errorf("owner = %s/%s, want Service/billing-db", owner.Kind, owner.Name)
	}
}

func TestDiscoveryHonoursTheDatabaseNameAnnotation(t *testing.T) {
	svc := postgresService("pg-primary", "databases", func(s *corev1.Service) {
		s.Annotations = map[string]string{DefaultDatabaseAnnotation: "billing"}
	})
	r, c := newDiscoveryReconciler(t, svc)

	reconcileService(t, r, "databases", "pg-primary")

	route := getRoute(t, c, "databases", "pg-primary-discovered")
	if route.Spec.Database != "billing" {
		t.Errorf("Database = %q, want %q", route.Spec.Database, "billing")
	}
}

func TestDiscoveryPicksThePostgreSQLPort(t *testing.T) {
	tests := []struct {
		name     string
		ports    []corev1.ServicePort
		wantPort string
	}{
		{
			name:     "port named postgresql",
			ports:    []corev1.ServicePort{{Name: "metrics", Port: 9187}, {Name: "postgresql", Port: 5433}},
			wantPort: "5433",
		},
		{
			name:     "port 5432 among several",
			ports:    []corev1.ServicePort{{Name: "metrics", Port: 9187}, {Name: "db", Port: 5432}},
			wantPort: "5432",
		},
		{
			name:     "the only port",
			ports:    []corev1.ServicePort{{Name: "tcp", Port: 6432}},
			wantPort: "6432",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := postgresService("pg", "apps", func(s *corev1.Service) { s.Spec.Ports = tc.ports })
			r, c := newDiscoveryReconciler(t, svc)

			reconcileService(t, r, "apps", "pg")

			route := getRoute(t, c, "apps", "pg-discovered")
			if got := route.Spec.Backend.Service.Port.String(); got != tc.wantPort {
				t.Errorf("Port = %q, want %q", got, tc.wantPort)
			}
		})
	}
}

func TestDiscoveryHonoursThePortAnnotation(t *testing.T) {
	svc := postgresService("pg", "apps", func(s *corev1.Service) {
		s.Annotations = map[string]string{PortAnnotation: "pgbouncer"}
		s.Spec.Ports = []corev1.ServicePort{{Name: "pgbouncer", Port: 6432}, {Name: "postgresql", Port: 5432}}
	})
	r, c := newDiscoveryReconciler(t, svc)

	reconcileService(t, r, "apps", "pg")

	route := getRoute(t, c, "apps", "pg-discovered")
	if got := route.Spec.Backend.Service.Port.String(); got != "pgbouncer" {
		t.Errorf("Port = %q, want %q", got, "pgbouncer")
	}
}

func TestDiscoverySkipsAnIgnoredService(t *testing.T) {
	svc := postgresService("pg", "apps", func(s *corev1.Service) {
		s.Annotations = map[string]string{IgnoreAnnotation: "true"}
	})
	r, c := newDiscoveryReconciler(t, svc)

	reconcileService(t, r, "apps", "pg")

	var route pgproxyv1alpha1.PostgresRoute
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "pg-discovered"}, &route)
	if !apierrors.IsNotFound(err) {
		t.Errorf("a route was generated for an ignored Service: err = %v", err)
	}
}

// Dropping the label is how a user takes an instance out of the gateway; the
// generated route must go with it, since no deletion event fires for the Service.
func TestDiscoveryRemovesTheRouteWhenTheLabelIsDropped(t *testing.T) {
	svc := postgresService("pg", "apps", nil)
	r, c := newDiscoveryReconciler(t, svc)

	reconcileService(t, r, "apps", "pg")
	getRoute(t, c, "apps", "pg-discovered") // must exist first

	current := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "pg"}, current); err != nil {
		t.Fatalf("getting the service: %v", err)
	}
	current.Labels = map[string]string{}
	if err := c.Update(context.Background(), current); err != nil {
		t.Fatalf("updating the service: %v", err)
	}

	reconcileService(t, r, "apps", "pg")

	var route pgproxyv1alpha1.PostgresRoute
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "pg-discovered"}, &route)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the generated route outlived the label: err = %v", err)
	}
}

// Overwriting a hand-written route would silently discard its configuration.
func TestDiscoveryRefusesToAdoptAHandWrittenRoute(t *testing.T) {
	svc := postgresService("pg", "apps", nil)
	handWritten := &pgproxyv1alpha1.PostgresRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pg-discovered",
			Namespace: "apps",
			Labels:    map[string]string{ManagedByLabel: "a-human"},
		},
		Spec: pgproxyv1alpha1.PostgresRouteSpec{
			Database: "do-not-touch",
			Backend: pgproxyv1alpha1.Backend{
				Type:    pgproxyv1alpha1.BackendTypeAddress,
				Address: &pgproxyv1alpha1.AddressBackend{Host: "elsewhere", Port: 5432},
			},
		},
	}

	r, c := newDiscoveryReconciler(t, svc, handWritten)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "apps", Name: "pg"},
	}); err == nil {
		t.Error("expected the reconciler to refuse the adoption, got no error")
	}

	route := getRoute(t, c, "apps", "pg-discovered")
	if route.Spec.Database != "do-not-touch" {
		t.Errorf("the hand-written route was overwritten: Database = %q", route.Spec.Database)
	}
}

func TestDiscoveryIsIdempotent(t *testing.T) {
	svc := postgresService("pg", "apps", nil)
	r, c := newDiscoveryReconciler(t, svc)

	reconcileService(t, r, "apps", "pg")
	first := getRoute(t, c, "apps", "pg-discovered")

	reconcileService(t, r, "apps", "pg")
	second := getRoute(t, c, "apps", "pg-discovered")

	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("a second reconcile rewrote the route: %s then %s",
			first.ResourceVersion, second.ResourceVersion)
	}
}

func TestDiscoveryIgnoresAMissingService(t *testing.T) {
	r, _ := newDiscoveryReconciler(t)

	// A deleted Service takes its generated route with it through ownership,
	// so the reconciler has nothing to do and must not error.
	reconcileService(t, r, "apps", "gone")
}

func TestDiscoveryPriorityAnnotation(t *testing.T) {
	tests := []struct {
		name       string
		annotation string
		want       int32
	}{
		{name: "absent", annotation: "", want: discoveredRoutePriority},
		{name: "explicit override", annotation: "50", want: 50},
		{name: "negative", annotation: "-5", want: -5},
		{name: "malformed falls back", annotation: "high", want: discoveredRoutePriority},
		{name: "out of range falls back", annotation: "99999999999", want: discoveredRoutePriority},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := postgresService("pg", "apps", func(s *corev1.Service) {
				if tc.annotation != "" {
					s.Annotations = map[string]string{PriorityAnnotation: tc.annotation}
				}
			})
			r, c := newDiscoveryReconciler(t, svc)

			reconcileService(t, r, "apps", "pg")

			route := getRoute(t, c, "apps", "pg-discovered")
			if route.Spec.Priority != tc.want {
				t.Errorf("Priority = %d, want %d", route.Spec.Priority, tc.want)
			}
		})
	}
}

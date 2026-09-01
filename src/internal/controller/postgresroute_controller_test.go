package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pgproxyv1alpha1 "github.com/tokaco/pg-k8s-proxy/api/v1alpha1"
	"github.com/tokaco/pg-k8s-proxy/internal/registry"
)

func routeObject(namespace, name, database string) *pgproxyv1alpha1.PostgresRoute {
	return &pgproxyv1alpha1.PostgresRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Generation: 3},
		Spec: pgproxyv1alpha1.PostgresRouteSpec{
			Database: database,
			Backend: pgproxyv1alpha1.Backend{
				Type:    pgproxyv1alpha1.BackendTypeAddress,
				Address: &pgproxyv1alpha1.AddressBackend{Host: "pg", Port: 5432},
			},
		},
	}
}

// reconcileRoute runs the status reconciler against a store primed with one decision.
func reconcileRoute(t *testing.T, route *pgproxyv1alpha1.PostgresRoute, decision *registry.Decision) *pgproxyv1alpha1.PostgresRoute {
	t.Helper()

	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(route).
		WithStatusSubresource(&pgproxyv1alpha1.PostgresRoute{}).
		Build()

	key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
	store := registry.NewStore()
	if decision != nil {
		store.Store(&registry.Snapshot{
			Table:     registry.NewTable(nil),
			Decisions: map[types.NamespacedName]registry.Decision{key: *decision},
		})
	}

	r := &PostgresRouteReconciler{Client: fakeClient, Store: store}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var updated pgproxyv1alpha1.PostgresRoute
	if err := fakeClient.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("getting the reconciled route: %v", err)
	}
	return &updated
}

func conditionStatus(t *testing.T, route *pgproxyv1alpha1.PostgresRoute, conditionType string) metav1.ConditionStatus {
	t.Helper()

	condition := meta.FindStatusCondition(route.Status.Conditions, conditionType)
	if condition == nil {
		t.Fatalf("condition %q is missing; have %+v", conditionType, route.Status.Conditions)
	}
	return condition.Status
}

func TestReconcileMarksAProgrammedRoute(t *testing.T) {
	route := routeObject("apps", "billing", "billing")

	updated := reconcileRoute(t, route, &registry.Decision{
		Database: "billing",
		Accepted: true,
		Endpoint: "pg:5432",
	})

	if updated.Status.Database != "billing" {
		t.Errorf("Status.Database = %q, want %q", updated.Status.Database, "billing")
	}
	if updated.Status.Endpoint != "pg:5432" {
		t.Errorf("Status.Endpoint = %q, want %q", updated.Status.Endpoint, "pg:5432")
	}
	if updated.Status.ObservedGeneration != 3 {
		t.Errorf("ObservedGeneration = %d, want 3", updated.Status.ObservedGeneration)
	}

	for _, conditionType := range []string{
		pgproxyv1alpha1.ConditionAccepted,
		pgproxyv1alpha1.ConditionResolved,
		pgproxyv1alpha1.ConditionProgrammed,
	} {
		if got := conditionStatus(t, updated, conditionType); got != metav1.ConditionTrue {
			t.Errorf("condition %s = %s, want True", conditionType, got)
		}
	}
}

// A conflict must be distinguishable from a broken backend, or an operator
// cannot tell which of the two problems they have.
func TestReconcileReportsANameConflict(t *testing.T) {
	route := routeObject("b", "loser", "shared")
	winner := types.NamespacedName{Namespace: "a", Name: "winner"}

	updated := reconcileRoute(t, route, &registry.Decision{
		Database:      "shared",
		Accepted:      false,
		ConflictsWith: winner,
	})

	if got := conditionStatus(t, updated, pgproxyv1alpha1.ConditionAccepted); got != metav1.ConditionFalse {
		t.Errorf("Accepted = %s, want False", got)
	}
	if updated.Status.ConflictingRoute != winner.String() {
		t.Errorf("ConflictingRoute = %q, want %q", updated.Status.ConflictingRoute, winner.String())
	}

	accepted := meta.FindStatusCondition(updated.Status.Conditions, pgproxyv1alpha1.ConditionAccepted)
	if accepted.Reason != pgproxyv1alpha1.ReasonDatabaseConflict {
		t.Errorf("Accepted reason = %q, want %q", accepted.Reason, pgproxyv1alpha1.ReasonDatabaseConflict)
	}
	if got := conditionStatus(t, updated, pgproxyv1alpha1.ConditionProgrammed); got != metav1.ConditionFalse {
		t.Errorf("Programmed = %s, want False", got)
	}
}

func TestReconcileReportsAnUnresolvableBackend(t *testing.T) {
	route := routeObject("apps", "billing", "billing")
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "services"}, "missing")

	updated := reconcileRoute(t, route, &registry.Decision{
		Database:   "billing",
		Accepted:   true,
		ResolveErr: notFound,
	})

	if got := conditionStatus(t, updated, pgproxyv1alpha1.ConditionAccepted); got != metav1.ConditionTrue {
		t.Errorf("Accepted = %s, want True: winning the name does not depend on resolvability", got)
	}
	if got := conditionStatus(t, updated, pgproxyv1alpha1.ConditionResolved); got != metav1.ConditionFalse {
		t.Errorf("Resolved = %s, want False", got)
	}

	resolved := meta.FindStatusCondition(updated.Status.Conditions, pgproxyv1alpha1.ConditionResolved)
	if resolved.Reason != pgproxyv1alpha1.ReasonBackendNotFound {
		t.Errorf("Resolved reason = %q, want %q", resolved.Reason, pgproxyv1alpha1.ReasonBackendNotFound)
	}
}

// A missing Service, a bad port reference, and an unreadable CA bundle are
// three different incidents with three different fixes, so status has to tell
// them apart rather than lumping them under one catch-all reason.
func TestReconcileClassifiesResolutionFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "missing service",
			err:  fmt.Errorf("%w: service apps/pg", registry.ErrBackendNotFound),
			want: pgproxyv1alpha1.ReasonBackendNotFound,
		},
		{
			name: "api server not-found",
			err:  apierrors.NewNotFound(schema.GroupResource{Resource: "services"}, "pg"),
			want: pgproxyv1alpha1.ReasonBackendNotFound,
		},
		{
			name: "bad port reference",
			err:  fmt.Errorf(`%w: service apps/pg has no port named "postgresql"`, registry.ErrPortNotFound),
			want: pgproxyv1alpha1.ReasonPortNotFound,
		},
		{
			name: "unreadable CA bundle",
			err:  fmt.Errorf("%w: reading Secrets is disabled", registry.ErrCABundle),
			want: pgproxyv1alpha1.ReasonCABundleFailed,
		},
		{
			name: "malformed backend",
			err:  fmt.Errorf("%w: backend.service is unset", registry.ErrInvalidBackend),
			want: pgproxyv1alpha1.ReasonInvalidBackend,
		},
		{
			name: "anything else",
			err:  errors.New("something unexpected"),
			want: pgproxyv1alpha1.ReasonInvalidBackend,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			updated := reconcileRoute(t, routeObject("apps", "billing", "billing"), &registry.Decision{
				Database:   "billing",
				Accepted:   true,
				ResolveErr: tc.err,
			})

			resolved := meta.FindStatusCondition(updated.Status.Conditions, pgproxyv1alpha1.ConditionResolved)
			if resolved.Reason != tc.want {
				t.Errorf("Resolved reason = %q, want %q", resolved.Reason, tc.want)
			}
		})
	}
}

// The table builder debounces, so a route can reach the reconciler before the
// snapshot that contains it. Requeueing beats writing a wrong status.
func TestReconcileRequeuesUntilADecisionExists(t *testing.T) {
	route := routeObject("apps", "billing", "billing")
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(route).
		WithStatusSubresource(&pgproxyv1alpha1.PostgresRoute{}).
		Build()

	r := &PostgresRouteReconciler{Client: fakeClient, Store: registry.NewStore()}
	key := types.NamespacedName{Namespace: "apps", Name: "billing"}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter = %v, want a positive delay", result.RequeueAfter)
	}

	var updated pgproxyv1alpha1.PostgresRoute
	if err := fakeClient.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("getting the route: %v", err)
	}
	if len(updated.Status.Conditions) != 0 {
		t.Errorf("status was written without a decision: %+v", updated.Status.Conditions)
	}
}

func TestReconcileIgnoresADeletedRoute(t *testing.T) {
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := &PostgresRouteReconciler{Client: fakeClient, Store: registry.NewStore()}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "apps", Name: "gone"},
	}); err != nil {
		t.Errorf("Reconcile on a deleted route: %v", err)
	}
}

// Rewriting an unchanged status on every resync would churn resourceVersions
// and re-trigger every watcher in the cluster.
func TestReconcileDoesNotRewriteAnUnchangedStatus(t *testing.T) {
	route := routeObject("apps", "billing", "billing")
	decision := &registry.Decision{Database: "billing", Accepted: true, Endpoint: "pg:5432"}

	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(route).
		WithStatusSubresource(&pgproxyv1alpha1.PostgresRoute{}).
		Build()

	key := types.NamespacedName{Namespace: "apps", Name: "billing"}
	store := registry.NewStore()
	store.Store(&registry.Snapshot{
		Table:     registry.NewTable(nil),
		Decisions: map[types.NamespacedName]registry.Decision{key: *decision},
	})

	r := &PostgresRouteReconciler{Client: fakeClient, Store: store}
	request := ctrl.Request{NamespacedName: key}

	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	var first pgproxyv1alpha1.PostgresRoute
	if err := fakeClient.Get(context.Background(), key, &first); err != nil {
		t.Fatalf("getting the route: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	var second pgproxyv1alpha1.PostgresRoute
	if err := fakeClient.Get(context.Background(), key, &second); err != nil {
		t.Fatalf("getting the route: %v", err)
	}

	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("status was rewritten on an unchanged reconcile: %s then %s",
			first.ResourceVersion, second.ResourceVersion)
	}
}

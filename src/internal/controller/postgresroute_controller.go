package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	pgproxyv1alpha1 "github.com/tokaco/pg-k8s-proxy/api/v1alpha1"
	"github.com/tokaco/pg-k8s-proxy/internal/registry"
)

// PostgresRouteReconciler publishes the routing decision for each route into
// its status. It runs on the elected leader only: the decisions themselves are
// computed identically on every replica by RouteTableBuilder, and there is no
// reason for all of them to write the same status.
type PostgresRouteReconciler struct {
	client.Client
	// Store is the snapshot the RouteTableBuilder publishes.
	Store *registry.Store
}

// +kubebuilder:rbac:groups=pgproxy.io,resources=postgresroutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgproxy.io,resources=postgresroutes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// SetupWithManager registers the reconciler.
func (r *PostgresRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Store == nil {
		return fmt.Errorf("PostgresRouteReconciler.Store is required")
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("postgresroute").
		For(&pgproxyv1alpha1.PostgresRoute{}).
		Complete(r)
}

// Reconcile writes the decision for one route into its status.
func (r *PostgresRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var route pgproxyv1alpha1.PostgresRoute
	if err := r.Get(ctx, req.NamespacedName, &route); err != nil {
		// A deleted route needs no status; the table rebuild already dropped it.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !route.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	decision, known := r.Store.Load().Decision(req.NamespacedName)
	if !known {
		// The builder debounces, so a freshly created route can land here
		// before the first snapshot that contains it. Come back shortly.
		logger.V(1).Info("no routing decision yet, requeueing")
		return ctrl.Result{RequeueAfter: 2 * DefaultDebounce}, nil
	}

	desired := route.Status.DeepCopy()
	desired.ObservedGeneration = route.Generation
	desired.Database = decision.Database
	desired.Endpoint = decision.Endpoint
	desired.ConflictingRoute = ""

	applyConditions(desired, &route, decision)

	if equalStatus(&route.Status, desired) {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(route.DeepCopy())
	route.Status = *desired
	if err := r.Status().Patch(ctx, &route, patch); err != nil {
		if apierrors.IsConflict(err) {
			// Someone else wrote first; the resulting watch event brings us back.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("patching PostgresRoute status: %w", err)
	}

	logger.V(1).Info("route status updated",
		"database", decision.Database,
		"accepted", decision.Accepted,
		"programmed", decision.Programmed(),
	)
	return ctrl.Result{}, nil
}

// applyConditions maps a decision onto the Accepted, Resolved, and Programmed
// conditions. Splitting them apart tells an operator whether a route is broken
// because another route stole its name or because its backend is missing.
func applyConditions(status *pgproxyv1alpha1.PostgresRouteStatus, route *pgproxyv1alpha1.PostgresRoute, decision registry.Decision) {
	generation := route.Generation
	set := func(c metav1.Condition) {
		c.ObservedGeneration = generation
		meta.SetStatusCondition(&status.Conditions, c)
	}

	if decision.Accepted {
		set(metav1.Condition{
			Type:    pgproxyv1alpha1.ConditionAccepted,
			Status:  metav1.ConditionTrue,
			Reason:  pgproxyv1alpha1.ReasonAccepted,
			Message: fmt.Sprintf("This route owns the database name %q.", decision.Database),
		})
	} else {
		status.ConflictingRoute = decision.ConflictsWith.String()
		status.Endpoint = ""
		set(metav1.Condition{
			Type:   pgproxyv1alpha1.ConditionAccepted,
			Status: metav1.ConditionFalse,
			Reason: pgproxyv1alpha1.ReasonDatabaseConflict,
			Message: fmt.Sprintf("Database name %q is already claimed by %s, which has a higher priority or is older.",
				decision.Database, decision.ConflictsWith),
		})
	}

	switch {
	case !decision.Accepted:
		set(metav1.Condition{
			Type:    pgproxyv1alpha1.ConditionResolved,
			Status:  metav1.ConditionFalse,
			Reason:  pgproxyv1alpha1.ReasonDatabaseConflict,
			Message: "The backend was not resolved because the route did not win its database name.",
		})
	case decision.ResolveErr != nil:
		set(metav1.Condition{
			Type:    pgproxyv1alpha1.ConditionResolved,
			Status:  metav1.ConditionFalse,
			Reason:  resolveFailureReason(decision.ResolveErr),
			Message: decision.ResolveErr.Error(),
		})
	default:
		set(metav1.Condition{
			Type:    pgproxyv1alpha1.ConditionResolved,
			Status:  metav1.ConditionTrue,
			Reason:  pgproxyv1alpha1.ReasonResolved,
			Message: fmt.Sprintf("The backend resolved to %s.", decision.Endpoint),
		})
	}

	if decision.Programmed() {
		set(metav1.Condition{
			Type:    pgproxyv1alpha1.ConditionProgrammed,
			Status:  metav1.ConditionTrue,
			Reason:  pgproxyv1alpha1.ReasonProgrammed,
			Message: fmt.Sprintf("Connections to database %q are routed to %s.", decision.Database, decision.Endpoint),
		})
	} else {
		set(metav1.Condition{
			Type:    pgproxyv1alpha1.ConditionProgrammed,
			Status:  metav1.ConditionFalse,
			Reason:  pgproxyv1alpha1.ReasonNotProgrammed,
			Message: "The gateway is not routing connections for this route.",
		})
	}
}

// resolveFailureReason classifies a resolution error so that status readers can
// branch on the reason rather than parse the message. IsNotFound unwraps, so a
// Service the resolver could not find is recognised through the wrapping.
func resolveFailureReason(err error) string {
	if apierrors.IsNotFound(err) {
		return pgproxyv1alpha1.ReasonBackendNotFound
	}
	return pgproxyv1alpha1.ReasonPortNotFound
}

// equalStatus reports whether two statuses are equivalent, ignoring condition
// transition timestamps so that an unchanged route is not patched every resync.
func equalStatus(a, b *pgproxyv1alpha1.PostgresRouteStatus) bool {
	if a.ObservedGeneration != b.ObservedGeneration ||
		a.Database != b.Database ||
		a.Endpoint != b.Endpoint ||
		a.ConflictingRoute != b.ConflictingRoute ||
		len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for _, want := range b.Conditions {
		got := meta.FindStatusCondition(a.Conditions, want.Type)
		if got == nil ||
			got.Status != want.Status ||
			got.Reason != want.Reason ||
			got.Message != want.Message ||
			got.ObservedGeneration != want.ObservedGeneration {
			return false
		}
	}
	return true
}

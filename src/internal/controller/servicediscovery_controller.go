package controller

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	pgproxyv1alpha1 "github.com/tokaco/pg-k8s-proxy/api/v1alpha1"
)

// Labels and annotations the discovery controller reads and writes.
const (
	// ManagedByLabel marks the routes this controller owns, so it never
	// touches a route a user wrote by hand.
	ManagedByLabel = "pgproxy.io/managed-by"
	// ManagedByServiceDiscovery is the value of ManagedByLabel on generated routes.
	ManagedByServiceDiscovery = "service-discovery"
	// SourceServiceLabel records which Service a generated route came from.
	SourceServiceLabel = "pgproxy.io/source-service"

	// DefaultDatabaseAnnotation lets a Service publish a database name that
	// differs from its own name.
	DefaultDatabaseAnnotation = "pgproxy.io/database-name"
	// PortAnnotation overrides which Service port the gateway dials.
	PortAnnotation = "pgproxy.io/port"
	// PriorityAnnotation overrides the generated route's conflict priority.
	PriorityAnnotation = "pgproxy.io/priority"
	// IgnoreAnnotation opts a Service out of discovery even if it matches the selector.
	IgnoreAnnotation = "pgproxy.io/ignore"
)

// discoveredRoutePriority keeps hand-written routes ahead of generated ones, so
// declaring a PostgresRoute is always enough to override discovery.
const discoveredRoutePriority = -100

// ServiceDiscoveryReconciler turns Services carrying a configured label into
// PostgresRoute objects owned by that Service.
//
// Materialising discovery as real API objects, rather than routing off Services
// directly, means the gateway has exactly one source of truth. Every route is
// visible to kubectl, carries status, and takes part in the same conflict
// resolution as a hand-written one.
type ServiceDiscoveryReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Selector picks the Services to adopt.
	Selector labels.Selector
	// DatabaseAnnotation names the annotation carrying the database name.
	// Defaults to DefaultDatabaseAnnotation.
	DatabaseAnnotation string
}

// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=pgproxy.io,resources=postgresroutes,verbs=get;list;watch;create;update;patch;delete

// SetupWithManager registers the reconciler, filtering the Service watch down
// to the selector so the work queue never sees unrelated Services.
func (r *ServiceDiscoveryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Selector == nil || r.Selector.Empty() {
		return fmt.Errorf("ServiceDiscoveryReconciler.Selector must not be empty")
	}
	if r.DatabaseAnnotation == "" {
		r.DatabaseAnnotation = DefaultDatabaseAnnotation
	}
	if r.Scheme == nil {
		r.Scheme = mgr.GetScheme()
	}

	matchesSelector := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return r.Selector.Matches(labels.Set(obj.GetLabels()))
	})

	return ctrl.NewControllerManagedBy(mgr).
		Named("servicediscovery").
		For(&corev1.Service{}, builder.WithPredicates(matchesSelector)).
		Owns(&pgproxyv1alpha1.PostgresRoute{}).
		Complete(r)
}

// Reconcile creates, updates, or removes the route generated for one Service.
func (r *ServiceDiscoveryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var svc corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		// The generated route is owned by the Service, so Kubernetes garbage
		// collection removes it when the Service goes away.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A Service can drop out of scope by losing the label, by being annotated
	// out, or by being deleted. The first two need an explicit cleanup.
	if !svc.DeletionTimestamp.IsZero() ||
		!r.Selector.Matches(labels.Set(svc.Labels)) ||
		svc.Annotations[IgnoreAnnotation] == "true" {
		return ctrl.Result{}, r.deleteGeneratedRoute(ctx, &svc)
	}

	route := &pgproxyv1alpha1.PostgresRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      generatedRouteName(svc.Name),
			Namespace: svc.Namespace,
		},
	}

	outcome, err := controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
		// Refuse to adopt a route somebody else owns; overwriting a
		// hand-written route would silently discard their configuration.
		if route.Labels[ManagedByLabel] != "" && route.Labels[ManagedByLabel] != ManagedByServiceDiscovery {
			return fmt.Errorf("route %s/%s is managed by %q, not by service discovery",
				route.Namespace, route.Name, route.Labels[ManagedByLabel])
		}

		if route.Labels == nil {
			route.Labels = map[string]string{}
		}
		route.Labels[ManagedByLabel] = ManagedByServiceDiscovery
		route.Labels[SourceServiceLabel] = svc.Name

		route.Spec.Database = r.databaseName(&svc)
		route.Spec.Priority = r.priority(&svc)
		route.Spec.Backend = pgproxyv1alpha1.Backend{
			Type: pgproxyv1alpha1.BackendTypeService,
			Service: &pgproxyv1alpha1.ServiceBackend{
				Name:      svc.Name,
				Namespace: svc.Namespace,
				Port:      r.port(&svc),
			},
		}

		return controllerutil.SetControllerReference(&svc, route, r.Scheme)
	})
	if err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reconciling the generated route for service %s: %w", req.NamespacedName, err)
	}

	if outcome != controllerutil.OperationResultNone {
		logger.Info("generated route for a discovered service",
			"service", req.String(),
			"route", route.Name,
			"database", route.Spec.Database,
			"operation", outcome,
		)
	}
	return ctrl.Result{}, nil
}

// deleteGeneratedRoute removes the route for a Service that left the selector.
func (r *ServiceDiscoveryReconciler) deleteGeneratedRoute(ctx context.Context, svc *corev1.Service) error {
	route := &pgproxyv1alpha1.PostgresRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      generatedRouteName(svc.Name),
			Namespace: svc.Namespace,
		},
	}

	// Delete only routes this controller generated, and only if this Service
	// is still their owner.
	var existing pgproxyv1alpha1.PostgresRoute
	if err := r.Get(ctx, client.ObjectKeyFromObject(route), &existing); err != nil {
		return client.IgnoreNotFound(err)
	}
	if existing.Labels[ManagedByLabel] != ManagedByServiceDiscovery {
		return nil
	}
	if owner := metav1.GetControllerOf(&existing); owner == nil || owner.UID != svc.UID {
		return nil
	}

	log.FromContext(ctx).Info("removing the route for a service that left discovery scope",
		"service", client.ObjectKeyFromObject(svc).String(), "route", existing.Name)

	return client.IgnoreNotFound(r.Delete(ctx, &existing))
}

func (r *ServiceDiscoveryReconciler) databaseName(svc *corev1.Service) string {
	if name := svc.Annotations[r.DatabaseAnnotation]; name != "" {
		return name
	}
	return svc.Name
}

// priority reads the override annotation, falling back to the discovered-route
// default. A malformed annotation is ignored rather than failing the reconcile:
// the route is still better published at its default priority than not at all.
func (r *ServiceDiscoveryReconciler) priority(svc *corev1.Service) int32 {
	if raw, ok := svc.Annotations[PriorityAnnotation]; ok {
		if priority, err := strconv.ParseInt(raw, 10, 32); err == nil {
			return int32(priority)
		}
	}
	return discoveredRoutePriority
}

// port picks the Service port to route to: an explicit annotation first, then a
// port named "postgresql" or "postgres", then 5432, then the sole port.
func (r *ServiceDiscoveryReconciler) port(svc *corev1.Service) intstr.IntOrString {
	if raw, ok := svc.Annotations[PortAnnotation]; ok && raw != "" {
		return intstr.Parse(raw)
	}
	for _, p := range svc.Spec.Ports {
		if p.Name == "postgresql" || p.Name == "postgres" {
			return intstr.FromInt32(p.Port)
		}
	}
	for _, p := range svc.Spec.Ports {
		if p.Port == 5432 {
			return intstr.FromInt32(5432)
		}
	}
	if len(svc.Spec.Ports) == 1 {
		return intstr.FromInt32(svc.Spec.Ports[0].Port)
	}
	// Leave it at the default and let the resolver report the mismatch in status.
	return intstr.FromInt32(5432)
}

// generatedRouteName derives a stable, collision-resistant route name. Service
// names are already valid DNS labels, so the suffix is all that is needed.
func generatedRouteName(serviceName string) string {
	const suffix = "-discovered"
	// Object names may be up to 253 characters; a Service name is at most 63,
	// so the suffix can always be appended.
	return serviceName + suffix
}

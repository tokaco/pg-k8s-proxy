// Package controller holds the reconcilers that keep the routing table and the
// PostgresRoute statuses in step with the cluster.
package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	pgproxyv1alpha1 "github.com/tokaco/pg-k8s-proxy/api/v1alpha1"
	"github.com/tokaco/pg-k8s-proxy/internal/proxy"
	"github.com/tokaco/pg-k8s-proxy/internal/registry"
)

// DefaultDebounce is the window used to coalesce bursts of cluster events into
// a single routing-table rebuild.
const DefaultDebounce = 200 * time.Millisecond

// RouteTableBuilder keeps the in-memory routing table in step with the cluster.
//
// It deliberately does not take part in leader election: every replica serves
// traffic, so every replica needs a current table. Because registry.Build is a
// pure function of the route set, all replicas reach the same table without
// coordinating, and the data plane never waits for a leader.
//
// The table is global state rather than per-object state, so this is a plain
// informer-driven rebuild loop instead of a work-queue reconciler.
type RouteTableBuilder struct {
	// Cache supplies both the informers to watch and the reads to build from.
	Cache cache.Cache
	// Store receives each new snapshot.
	Store *registry.Store
	// Resolver turns backend references into addresses.
	Resolver registry.Resolver
	// ClusterDomain is the cluster's DNS suffix, e.g. cluster.local.
	ClusterDomain string
	// WatchSecrets enables the Secret informer, needed only when routes verify
	// backend certificates against a CA bundle stored in a Secret.
	WatchSecrets bool
	// Debounce coalesces event bursts. Defaults to DefaultDebounce.
	Debounce time.Duration

	// StatusEvents receives one event per route whose routing decision changed.
	//
	// A route only ever gets a watch event for changes to itself, so a route
	// that loses its database name to a newly created one is never reconciled
	// and its status goes on advertising a claim it no longer holds. Feeding
	// the affected routes back to the status reconciler is what keeps the
	// reported state honest. Optional; nil disables the notifications.
	StatusEvents chan<- event.GenericEvent

	trigger chan struct{}
	// onRebuild, when set, is invoked after every successful rebuild.
	onRebuild func(*registry.Snapshot)
}

// NeedLeaderElection reports false so that every replica keeps a live table.
func (b *RouteTableBuilder) NeedLeaderElection() bool { return false }

// SetupWithManager validates the builder and registers it with the manager.
func (b *RouteTableBuilder) SetupWithManager(mgr ctrl.Manager) error {
	if b.Store == nil {
		return fmt.Errorf("RouteTableBuilder.Store is required")
	}
	if b.Cache == nil {
		b.Cache = mgr.GetCache()
	}
	if b.Resolver == nil {
		b.Resolver = registry.NewClientResolver(b.Cache).WithCABundles(b.WatchSecrets)
	}
	if b.ClusterDomain == "" {
		return fmt.Errorf("RouteTableBuilder.ClusterDomain is required")
	}
	if b.Debounce <= 0 {
		b.Debounce = DefaultDebounce
	}
	b.trigger = make(chan struct{}, 1)
	return mgr.Add(b)
}

// Start watches the inputs and rebuilds the table until ctx is cancelled.
// The manager only starts it once the caches have synced.
func (b *RouteTableBuilder) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("routetable")

	// Routes are the table itself; Services resolve named ports and confirm
	// that a referenced backend exists; Secrets carry backend CA bundles.
	watched := []client.Object{&pgproxyv1alpha1.PostgresRoute{}, &corev1.Service{}}
	if b.WatchSecrets {
		watched = append(watched, &corev1.Secret{})
	}
	for _, obj := range watched {
		if err := b.watch(ctx, obj); err != nil {
			return err
		}
	}

	// Build once up front so the data plane is ready immediately.
	if err := b.rebuild(ctx); err != nil {
		logger.Error(err, "initial routing table build failed")
	}

	timer := time.NewTimer(b.Debounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	pending := false

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-b.trigger:
			if !pending {
				pending = true
				timer.Reset(b.Debounce)
			}

		case <-timer.C:
			pending = false
			if err := b.rebuild(ctx); err != nil {
				logger.Error(err, "rebuilding the routing table failed")
			}
		}
	}
}

func (b *RouteTableBuilder) watch(ctx context.Context, obj client.Object) error {
	informer, err := b.Cache.GetInformer(ctx, obj)
	if err != nil {
		return fmt.Errorf("getting informer for %T: %w", obj, err)
	}
	_, err = informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { b.schedule() },
		UpdateFunc: func(any, any) { b.schedule() },
		DeleteFunc: func(any) { b.schedule() },
	})
	if err != nil {
		return fmt.Errorf("watching %T: %w", obj, err)
	}
	return nil
}

// schedule requests a rebuild without blocking. The channel has a depth of one,
// so a burst of events collapses into a single pending rebuild.
func (b *RouteTableBuilder) schedule() {
	select {
	case b.trigger <- struct{}{}:
	default:
	}
}

// rebuild lists every route from the cache and publishes a fresh snapshot.
func (b *RouteTableBuilder) rebuild(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("routetable")

	var routes pgproxyv1alpha1.PostgresRouteList
	if err := b.Cache.List(ctx, &routes); err != nil {
		return fmt.Errorf("listing PostgresRoutes: %w", err)
	}

	snapshot := registry.Build(ctx, routes.Items, b.Resolver, b.ClusterDomain)
	previous := b.Store.Load()

	b.Store.Store(snapshot)
	proxy.SetRouteCount(snapshot.Table.Len())
	b.notifyChanged(previous, snapshot)

	// Only log at info level when the set of routable databases actually moved,
	// so an idle cluster stays quiet.
	if previous.Table.Len() != snapshot.Table.Len() {
		logger.Info("routing table updated",
			"routes", len(routes.Items),
			"routable", snapshot.Table.Len(),
			"databases", snapshot.Table.Databases(),
		)
	} else {
		logger.V(1).Info("routing table rebuilt", "routes", len(routes.Items), "routable", snapshot.Table.Len())
	}

	if b.onRebuild != nil {
		b.onRebuild(snapshot)
	}
	return nil
}

// notifyChanged enqueues every route whose decision differs from the previous
// snapshot, so the status reconciler revisits routes that a change to some
// other route affected.
//
// Sends are non-blocking. On a replica that is not the leader nothing drains
// the channel, and dropping there is correct: that replica writes no status,
// and when it is elected the informer's initial sync reconciles every route
// anyway.
func (b *RouteTableBuilder) notifyChanged(previous, current *registry.Snapshot) {
	if b.StatusEvents == nil {
		return
	}

	for key, now := range current.Decisions {
		before, existed := previous.Decisions[key]
		if existed && !decisionChanged(before, now) {
			continue
		}
		b.notify(key)
	}
}

func (b *RouteTableBuilder) notify(key types.NamespacedName) {
	evt := event.GenericEvent{Object: &pgproxyv1alpha1.PostgresRoute{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
	}}
	select {
	case b.StatusEvents <- evt:
	default:
	}
}

// decisionChanged reports whether two decisions differ in anything the status
// records. Resolution errors are compared by message: a fresh error value is
// allocated on every rebuild, so comparing them by identity would report a
// change every time and reconcile an unresolvable route forever.
func decisionChanged(before, now registry.Decision) bool {
	if before.Database != now.Database ||
		before.Accepted != now.Accepted ||
		before.ConflictsWith != now.ConflictsWith ||
		before.Endpoint != now.Endpoint {
		return true
	}
	return errorMessage(before.ResolveErr) != errorMessage(now.ResolveErr)
}

func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var _ manager.Runnable = &RouteTableBuilder{}
var _ manager.LeaderElectionRunnable = &RouteTableBuilder{}

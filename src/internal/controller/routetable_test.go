package controller

import (
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/tokaco/pg-k8s-proxy/internal/registry"
)

func decision(database string, accepted bool, endpoint string) registry.Decision {
	return registry.Decision{Database: database, Accepted: accepted, Endpoint: endpoint}
}

func snapshotOf(decisions map[types.NamespacedName]registry.Decision) *registry.Snapshot {
	return &registry.Snapshot{Table: registry.NewTable(nil), Decisions: decisions}
}

// drain collects the routes the builder asked the reconciler to revisit.
func drain(events chan event.GenericEvent) []types.NamespacedName {
	var got []types.NamespacedName
	for {
		select {
		case e := <-events:
			got = append(got, types.NamespacedName{
				Namespace: e.Object.GetNamespace(),
				Name:      e.Object.GetName(),
			})
		default:
			return got
		}
	}
}

// The bug this guards against: a route only receives watch events for changes
// to itself, so when a newly created route takes over its database name the
// loser is never reconciled and its status keeps advertising a claim it no
// longer holds.
func TestBuilderNotifiesTheRouteThatLostItsName(t *testing.T) {
	events := make(chan event.GenericEvent, 16)
	builder := &RouteTableBuilder{StatusEvents: events}

	loser := types.NamespacedName{Namespace: "apps", Name: "discovered"}
	winner := types.NamespacedName{Namespace: "apps", Name: "explicit"}

	before := snapshotOf(map[types.NamespacedName]registry.Decision{
		loser: decision("billing", true, "old:5432"),
	})
	after := snapshotOf(map[types.NamespacedName]registry.Decision{
		loser:  {Database: "billing", Accepted: false, ConflictsWith: winner},
		winner: decision("billing", true, "new:5432"),
	})

	builder.notifyChanged(before, after)

	got := drain(events)
	if len(got) != 2 {
		t.Fatalf("notified %d routes, want 2: %v", len(got), got)
	}
	seen := map[types.NamespacedName]bool{}
	for _, key := range got {
		seen[key] = true
	}
	if !seen[loser] {
		t.Error("the route that lost its database name was not notified")
	}
	if !seen[winner] {
		t.Error("the newly accepted route was not notified")
	}
}

// An idle cluster must stay idle: re-notifying unchanged routes would rewrite
// identical statuses and churn resourceVersions across the whole cluster.
func TestBuilderStaysQuietWhenNothingChanged(t *testing.T) {
	events := make(chan event.GenericEvent, 16)
	builder := &RouteTableBuilder{StatusEvents: events}

	key := types.NamespacedName{Namespace: "apps", Name: "billing"}
	decisions := map[types.NamespacedName]registry.Decision{key: decision("billing", true, "pg:5432")}

	builder.notifyChanged(snapshotOf(decisions), snapshotOf(decisions))

	if got := drain(events); len(got) != 0 {
		t.Errorf("notified %v on an unchanged snapshot, want nothing", got)
	}
}

// A resolution error is a fresh value on every rebuild, so comparing errors by
// identity would reconcile an unresolvable route forever.
func TestBuilderComparesResolutionErrorsByMessage(t *testing.T) {
	events := make(chan event.GenericEvent, 16)
	builder := &RouteTableBuilder{StatusEvents: events}

	key := types.NamespacedName{Namespace: "apps", Name: "billing"}
	same := func() *registry.Snapshot {
		return snapshotOf(map[types.NamespacedName]registry.Decision{
			key: {Database: "billing", Accepted: true, ResolveErr: errors.New("service apps/pg not found")},
		})
	}

	builder.notifyChanged(same(), same())
	if got := drain(events); len(got) != 0 {
		t.Errorf("notified %v for an unchanged error, want nothing", got)
	}

	changed := snapshotOf(map[types.NamespacedName]registry.Decision{
		key: {Database: "billing", Accepted: true, ResolveErr: errors.New("service apps/pg has no port 5432")},
	})
	builder.notifyChanged(same(), changed)
	if got := drain(events); len(got) != 1 {
		t.Errorf("notified %v for a changed error, want one event", got)
	}
}

func TestBuilderNotifiesNewlyAppearedRoutes(t *testing.T) {
	events := make(chan event.GenericEvent, 16)
	builder := &RouteTableBuilder{StatusEvents: events}

	key := types.NamespacedName{Namespace: "apps", Name: "new"}
	builder.notifyChanged(
		snapshotOf(map[types.NamespacedName]registry.Decision{}),
		snapshotOf(map[types.NamespacedName]registry.Decision{key: decision("new", true, "pg:5432")}),
	)

	if got := drain(events); len(got) != 1 || got[0] != key {
		t.Errorf("notified %v, want [%v]", got, key)
	}
}

// A replica that is not the leader runs no status reconciler, so nothing drains
// the channel. The builder must not wedge behind a full one.
func TestBuilderDoesNotBlockOnAFullChannel(t *testing.T) {
	events := make(chan event.GenericEvent, 1)
	builder := &RouteTableBuilder{StatusEvents: events}

	decisions := map[types.NamespacedName]registry.Decision{}
	for i := range 50 {
		decisions[types.NamespacedName{Namespace: "apps", Name: string(rune('a'+i%26)) + string(rune('a'+i/26))}] =
			decision("db", true, "pg:5432")
	}

	done := make(chan struct{})
	go func() {
		builder.notifyChanged(snapshotOf(nil), snapshotOf(decisions))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("notifyChanged blocked on a channel nobody drains")
	}
}

// A nil channel is the configuration a proxy-only replica runs with.
func TestBuilderToleratesNoEventChannel(t *testing.T) {
	builder := &RouteTableBuilder{}
	key := types.NamespacedName{Namespace: "apps", Name: "billing"}
	builder.notifyChanged(
		snapshotOf(nil),
		snapshotOf(map[types.NamespacedName]registry.Decision{key: decision("billing", true, "pg:5432")}),
	)
}

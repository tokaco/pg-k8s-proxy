# Architecture

## Overview

```
                       ┌─────────────── pod (replica) ───────────────┐
                       │                                             │
   client ────────────▶│  proxy: read startup → Lookup → splice      │
   :5432               │            ▲                                │
                       │            │ atomic.Pointer[Snapshot]       │
                       │            │                                │
                       │  registry.Store ◀── RouteTableBuilder       │
                       │                          ▲                  │
                       └──────────────────────────┼──────────────────┘
                                                  │ informer watch
                                    ┌─────────────┴─────────────┐
                                    │  PostgresRoute, Service   │
                                    │  (controller-runtime cache)│
                                    └─────────────┬─────────────┘
                                                  │
                       ┌──────── leader only ─────┴─────────┐
                       │ PostgresRouteReconciler → status    │
                       │ ServiceDiscoveryReconciler → routes │
                       └─────────────────────────────────────┘
```

## One binary, two roles

The `--role` flag selects which halves of the process run:

| Role | Data plane | Reconcilers | Leader election |
|---|---|---|---|
| `all` (default) | yes | yes | yes |
| `manager` | no | yes | yes |
| `proxy` | yes | no | forced off |

The chart deploys a single Deployment running `all`: every pod both accepts
traffic and takes part in leader election. The `manager` and `proxy` roles exist
for installations that want to scale the control plane and the data plane
independently.

## Why the routing table is built on every replica

`registry.Build` is a pure function of the `PostgresRoute` set. It sorts routes by
`(priority ↓, creationTimestamp ↑, namespace, name)` and awards each database name
to the first claimant. The ordering is total, so every replica independently
arrives at the same table.

Two properties follow from that:

- **The data plane never waits for a leader.** A freshly started pod routes
  correctly as soon as its cache syncs, without waiting for a leader to write
  statuses first.
- **Status is observability, not the source of truth.** If the leader falls behind
  or disappears entirely, traffic still follows the right routes; only what
  `kubectl get postgresroutes` shows goes stale.

`RouteTableBuilder` deliberately opts out of leader election
(`NeedLeaderElection() == false`) and is an informer-driven rebuild loop rather
than a work-queue reconciler: the table is global state, not per-object state, so
any event simply means "rebuild the whole thing". Events are coalesced in a 200 ms
window, so a rollout touching dozens of Services causes one rebuild rather than
dozens.

## Publishing a snapshot

`registry.Store` holds a `*Snapshot` behind an `atomic.Pointer`. Snapshots are
immutable and replaced wholesale, so a connection handler needs neither a mutex
nor a copy: `store.Routes().Lookup(db)` is a pointer load and a map read.

The previous implementation wrote and read a `map[string]Backend` from different
goroutines with no synchronisation at all, which was a data race in the literal
sense.

## Connection lifecycle

1. **Accept.** A deadline covers the whole handshake (`--proxy-startup-timeout`),
   so a stalled client cannot hold a socket open indefinitely.
2. **Negotiation.** `SSLRequest` (upgrade to TLS, or decline with `N`),
   `GSSENCRequest` (always `N`), and `CancelRequest` are handled here. The number
   of rounds is bounded.
3. **Startup.** The `database` parameter is read from the packet; if absent, the
   `user` parameter is used, which is what a real server does.
4. **Lookup.** With no route, the client receives an `ErrorResponse` carrying
   SQLSTATE `3D000` and `database "x" does not exist` — exactly what a real server
   would return. The previous implementation simply closed the socket.
5. **Dial.** Connect to the backend, re-originating TLS if the route asks for it.
6. **Forward.** The startup packet is re-encoded with its parameter order intact;
   only `database` changes, and only when `targetDatabase` is set.
7. **Relay.** Client-to-backend is copied verbatim. Backend-to-client is framed
   only until `ReadyForQuery`, then also degrades to a raw splice. Deadlines are
   cleared; TCP keepalives are what reap dead peers from there on.
8. **Close.** Half-close in both directions and release the cancellation key.

## Query cancellation

This is where a naive proxy fails most quietly. A `CancelRequest` arrives on a
**new** connection and carries only a `(pid, secret)` pair — no database name,
nothing else. There is nowhere to forward it unless the proxy remembers which
session it belongs to.

The gateway intercepts `BackendKeyData` in the server's reply, records the real
key alongside the backend address, and hands the client a key of its own drawn
from `crypto/rand`. When a `CancelRequest` arrives, the key is looked up and the
backend's own key is delivered to it.

The substitution matters for two reasons: two different backends can hand out the
same process ID, and a leaked real key would let anyone who can reach the backend
cancel that session directly, bypassing the gateway.

The key lives in one replica's memory, which is why the chart's Service defaults
to `sessionAffinity: ClientIP` — otherwise the cancel connection may land on a pod
that has never heard of that key.

## TLS

TLS cannot be passed through: learning the database name means reading the startup
message, and that message is inside the encrypted stream. So TLS is terminated at
the gateway (`proxy.tls.mode`) and, where a route asks for it, established afresh
towards the backend (`spec.tls.mode`):

| `spec.tls.mode` | Encrypted | Chain verified | Hostname verified |
|---|---|---|---|
| `Disable` (default) | no | — | — |
| `Require` | yes | no | no |
| `VerifyCA` | yes | yes | no |
| `VerifyFull` | yes | yes | yes |

`VerifyCA` exists because a PostgreSQL certificate behind a Service almost never
carries that Service's DNS name.

## What is read from the cluster

| Resource | Purpose | Verbs |
|---|---|---|
| `PostgresRoute` | the routing table itself | get, list, watch, create, update, patch, delete |
| `Service` | resolving named ports, discovery | get, list, watch |
| `Secret` | CA bundles for backend certificate verification | get, list, watch (optional) |
| `Lease` | leader election, release namespace only | full access |
| `Event` | reconciler events | create, patch |

The `Secret` cache is narrowed by the selector `pgproxy.io/ca-bundle=true`, so
enabling that feature never pulls every Secret in the cluster into memory.

<div align="center">

# pg-k8s-proxy

**A PostgreSQL gateway and Kubernetes operator that routes connections by database name.**

[![CI](https://github.com/tokaco/pg-k8s-proxy/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/tokaco/pg-k8s-proxy/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/tokaco/pg-k8s-proxy?sort=semver)](https://github.com/tokaco/pg-k8s-proxy/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/tokaco/pg-k8s-proxy)](https://goreportcard.com/report/github.com/tokaco/pg-k8s-proxy)
[![License](https://img.shields.io/github/license/tokaco/pg-k8s-proxy)](LICENSE)

</div>

One `postgres:5432` address for the whole cluster. A client connects to it naming
the database it wants; the gateway reads the PostgreSQL startup message, looks the
name up in its routing table, and splices the connection through to whichever
instance serves that database.

```
                    ┌──────────────────────────────────────┐
  psql -d billing ──┤                                      ├──▶ billing-rw.db.svc:5432
                    │  pg-k8s-proxy                        │
  psql -d orders  ──┤   read startup → look up route       ├──▶ analytics.apps.svc:5432
                    │   → splice TCP                       │
  psql -d legacy  ──┤                                      ├──▶ pg.legacy.example.com:5432
                    └───────────────┬──────────────────────┘
                                    │ watch
                     PostgresRoute + labelled Services
```

No credentials pass through the gateway and none are stored in it. Authentication
stays entirely with PostgreSQL.

## Why

Every PostgreSQL instance in a cluster normally means another host name for
applications to learn, another Service to plumb through config, and another
migration when an instance moves. This gateway collapses that to one address: the
database name in the connection string is what selects the backend, so moving a
database between instances is a change to one `PostgresRoute`, not to every client.

## Features

- **Routing by database name** — no proxy-specific syntax in the connection string.
- **`PostgresRoute` CRD** — declarative routes with status conditions, plus optional
  label-based discovery of existing Services.
- **Deterministic conflict resolution** — when two routes claim a database name, every
  replica independently picks the same winner and the loser says why in its status.
- **Query cancellation that works** — the gateway issues its own `BackendKeyData` and
  translates `CancelRequest` back to the real backend, so Ctrl-C in `psql` cancels.
- **Diagnosable failures** — an unknown database gets `FATAL: database "x" does not
  exist` with SQLSTATE `3D000`, not a dropped socket.
- **TLS termination and re-origination** — including `VerifyCA` and `VerifyFull` to the
  backend.
- **Namespace or cluster scope** — one Helm value decides whether the operator gets a
  `ClusterRole` or a `Role` per watched namespace.
- **Production shape** — distroless non-root image, leader election, graceful session
  draining, Prometheus metrics, PodDisruptionBudget, NetworkPolicy.

## Quick start

```bash
helm install pg-k8s-proxy oci://ghcr.io/tokaco/charts/pg-k8s-proxy \
  --namespace pgproxy --create-namespace
```

Point the gateway at an existing instance:

```yaml
apiVersion: pgproxy.io/v1alpha1
kind: PostgresRoute
metadata:
  name: billing
  namespace: databases
spec:
  backend:
    type: Service
    service:
      name: billing-rw     # e.g. the CloudNativePG read-write Service
```

Then connect, naming the database:

```bash
kubectl port-forward -n pgproxy svc/pg-k8s-proxy 5432:5432
psql -h 127.0.0.1 -p 5432 -U alice -d billing
```

To try it end to end with throwaway instances:

```bash
make install examples
make routes
```

```console
$ kubectl get postgresroutes -A
NAMESPACE  NAME                           DATABASE   ENDPOINT                                   ACCEPTED  PROGRAMMED  AGE
default    orders                         orders     postgres-analytics.default.svc...:5432     True      True        30s
default    postgres-analytics-discovered  analytics  postgres-analytics.default.svc...:5432     True      True        30s
default    postgres-billing-discovered    billing    postgres-billing.default.svc...:5432       False     False       30s
default    billing-override               billing    postgres-analytics.default.svc...:5432     True      True        30s
```

## Scope: one namespace or the whole cluster

A single chart value decides how much of the cluster the operator provisions
routing for, and therefore which RBAC objects the chart creates.

```bash
# Whole cluster: ClusterRole + ClusterRoleBinding.
helm install pg-k8s-proxy oci://ghcr.io/tokaco/charts/pg-k8s-proxy \
  -n pgproxy --create-namespace

# Release namespace only: Role + RoleBinding.
helm install pg-k8s-proxy oci://ghcr.io/tokaco/charts/pg-k8s-proxy \
  -n pgproxy --create-namespace \
  --set scope.type=Namespaced

# A specific set of namespaces: Role + RoleBinding in each.
helm install pg-k8s-proxy oci://ghcr.io/tokaco/charts/pg-k8s-proxy \
  -n pgproxy --create-namespace \
  --set scope.type=Namespaced \
  --set 'scope.namespaces={apps,databases}'
```

A ServiceAccount is created either way; only the breadth of its permissions
differs. Namespaced scope also narrows the informer cache to those namespaces, so
the operator neither can nor does read anything outside them.

See [docs/scopes.md](docs/scopes.md).

## Routing

Two sources of routes work side by side, and both converge on the same
`PostgresRoute` objects, so the whole routing table is visible to `kubectl` and
carries status.

**Explicit routes** are what you write by hand. **Label-based discovery** is on by
default: every Service carrying `app.kubernetes.io/name=postgresql` gets a
`PostgresRoute` generated for it, owned by that Service. Generated routes are given
`priority: -100`, so a hand-written route always overrides a discovered one.

See [docs/routing.md](docs/routing.md).

## Architecture

One binary, two roles — `--role=all` (the default), `manager`, or `proxy`.

- **Route table builder** (every replica, no leader election) watches the informers
  and rebuilds the routing table. `registry.Build` is a pure function of the route
  set, so every replica reaches the same table independently and the data plane
  never waits for a leader.
- **Status reconciler** (leader only) writes the `Accepted`, `Resolved`, and
  `Programmed` conditions.
- **Discovery reconciler** (leader only) materialises labelled Services into routes.
- **Data plane** (every replica) reads an immutable snapshot through an
  `atomic.Pointer` and proxies connections.

See [docs/architecture.md](docs/architecture.md).

## Observability

Prometheus metrics on `:9090/metrics`. That is the only endpoint serving them —
the probe port carries probes and nothing else, so no scrape configuration can
pick the same registry up twice and double every series.

| Metric | Type | Meaning |
|---|---|---|
| `pgproxy_connections_total{database,outcome}` | counter | Connections by outcome: `proxied`, `no_route`, `backend_unreachable`, `rejected`, `handshake_failed` |
| `pgproxy_active_connections{database}` | gauge | Sessions currently proxied |
| `pgproxy_bytes_total{database,direction}` | counter | Bytes relayed |
| `pgproxy_backend_dial_duration_seconds` | histogram | Time to establish the backend connection |
| `pgproxy_session_duration_seconds` | histogram | Session lifetime |
| `pgproxy_cancel_requests_total{outcome}` | counter | Query cancellations |
| `pgproxy_routes` | gauge | Routable databases |

Probes on `:8080`: `/healthz` for liveness, `/readyz` for readiness, with
`/health` and `/ready` kept as aliases. Nothing else is served there.

The two ports are split because their audiences differ and only one of them can
be locked down. The kubelet reaches probes from the node, which no pod selector
matches, so that port stays open; metrics carry a `database` label and therefore
enumerate every routed database name, which is worth restricting to your
scrapers via `networkPolicy.allowedScrapers`. NetworkPolicy separates them by
port, not by path.

## Supported and not supported

**Supported.** Any PostgreSQL 3.0 protocol traffic over the splice, including
`COPY`, prepared statements, and `LISTEN`/`NOTIFY`. TLS termination and
re-origination. Query cancellation. Graceful shutdown with session draining.
Connection limits.

**Not supported.** Connection pooling — one client session is one backend session.
Load balancing across replicas of one database. Protocol 2.0. GSSAPI encryption.
Splitting reads and writes across different backends.

TLS cannot be passed through untouched: the gateway must read the startup message
to learn which database is wanted, and that message is inside the encrypted stream.

## Requirements

- Kubernetes 1.29 or newer (tested against 1.34)
- Helm 3.8 or newer, for OCI chart support

## Development

```bash
make help              # list targets
make check             # fmt, vet, lint, tests, chart lint
make test              # go test -race
make generate manifests # regenerate DeepCopy methods and CRDs
make run               # run locally against the current kubeconfig
```

```
src/                    Go module
  api/v1alpha1/         CRD types
  cmd/pg-k8s-proxy/     entry point
  internal/pgwire/      PostgreSQL wire protocol
  internal/registry/    routing table
  internal/controller/  reconcilers
  internal/proxy/       data plane
charts/pg-k8s-proxy/    Helm chart
deploy/crds/            generated CRDs
docker/                 Dockerfile
examples/               example manifests
docs/                   documentation
```

## Documentation

- [Architecture](docs/architecture.md)
- [Routing](docs/routing.md)
- [Scopes and RBAC](docs/scopes.md)
- [Chart configuration](charts/pg-k8s-proxy/README.md)
- [Migrating from the previous proxy](docs/migration.md)
- [Contributing](CONTRIBUTING.md)
- [Security policy](SECURITY.md)

## License

[Apache License 2.0](LICENSE).

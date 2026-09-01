# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Entries below 1.0.0 may contain breaking changes in minor releases.

<!--
This file is maintained by release-please. Do not edit released sections by hand;
they are regenerated from Conventional Commit messages.
-->

## [0.1.1](https://github.com/tokaco/pg-k8s-proxy/compare/v0.1.0...v0.1.1) (2026-09-01)


### Bug Fixes

* revisit routes that lose a database name to another route ([248c02f](https://github.com/tokaco/pg-k8s-proxy/commit/248c02f903a86f1a2d29c56ac9b2d426906b7b8d))
* stop one unreadable backend from freezing the routing table ([3a4c5e4](https://github.com/tokaco/pg-k8s-proxy/commit/3a4c5e443ff533d0c86d221b4d32259185fb392f))

## 0.1.0

Initial release: the proxy is rebuilt as a Kubernetes operator with a Helm chart.

### Features

- **`PostgresRoute` CRD** for declarative routing, with `Accepted`, `Resolved`,
  and `Programmed` status conditions.
- **Deterministic conflict resolution.** When several routes claim one database
  name the winner is decided by priority, then age, then name, so every replica
  agrees without coordinating. The losing route reports the conflict in status
  instead of failing silently.
- **Label-based Service discovery**, materialised into real `PostgresRoute`
  objects owned by the Service, so both routing sources share one source of truth.
- **Query cancellation.** The gateway issues its own `BackendKeyData` and
  translates `CancelRequest` back to the originating backend, so Ctrl-C in `psql`
  now cancels the query.
- **Diagnosable rejections.** An unknown database returns `FATAL: database "x"
  does not exist` with SQLSTATE `3D000` rather than a closed socket, and an
  unreachable backend returns `08006`.
- **TLS termination and re-origination**, with `Require`, `VerifyCA`, and
  `VerifyFull` modes towards the backend, plus optional client certificate
  verification.
- **Namespace or cluster scope** selected by `scope.type`, which decides whether
  the chart creates a `ClusterRole` or a `Role` per watched namespace and narrows
  the informer cache to match.
- **Helm chart** with a values schema, PodDisruptionBudget, NetworkPolicy,
  ServiceMonitor, HorizontalPodAutoscaler, topology spread, and a `helm test`.
- **Prometheus metrics** for connections by outcome, active sessions, bytes
  relayed, backend dial latency, session duration, and cancellations.
- **Connection limits** and graceful shutdown that drains in-flight sessions.

### Changes from the pre-operator proxy

- Routes now come from a watch on the API server rather than a 30-second poll.
  `POLL_INTERVAL` is ignored.
- `/metrics` moved to port 9090 and is served there only. Repoint any scrape
  configuration that targeted port 8080; the probe port now carries probes
  alone, so nothing can scrape the same registry twice.
- `/health` is liveness only and no longer returns 503 when no backends exist.
  The previous behaviour put a gateway in an empty cluster into a permanent
  restart loop.
- The image is now `distroless/static`, non-root, and multi-architecture. The
  bash entry point and health-check scripts are gone; probes are HTTP.
- `POSTGRES_PORT`, `HEALTH_PORT`, `LABEL_SELECTOR`, and `KUBERNETES_NAMESPACE`
  are still honoured, with a deprecation warning naming the flag that replaces
  each one.

See [docs/migration.md](docs/migration.md) for the upgrade path.

### Fixes

- The backend table was read and written from different goroutines with no
  synchronisation; it is now an immutable snapshot behind an `atomic.Pointer`.
- Startup packets were reassembled by iterating a map, so parameter order changed
  from connection to connection. Order is now preserved byte for byte.
- SSL negotiation could loop indefinitely on a malicious client; the handshake is
  now bounded in both rounds and time.

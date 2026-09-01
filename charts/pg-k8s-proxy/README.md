# pg-k8s-proxy

Helm chart for the pg-k8s-proxy PostgreSQL gateway and Kubernetes operator.

## Installing

```bash
# Whole cluster
helm install pg-k8s-proxy oci://ghcr.io/tokaco/charts/pg-k8s-proxy \
  -n pgproxy --create-namespace

# Release namespace only
helm install pg-k8s-proxy oci://ghcr.io/tokaco/charts/pg-k8s-proxy \
  -n pgproxy --create-namespace \
  --set scope.type=Namespaced

# A specific set of namespaces (they must already exist)
helm install pg-k8s-proxy oci://ghcr.io/tokaco/charts/pg-k8s-proxy \
  -n pgproxy --create-namespace \
  --set scope.type=Namespaced \
  --set 'scope.namespaces={apps,databases}'
```

Requires Kubernetes 1.29+ and Helm 3.8+.

## Values

### Scope

| Key | Default | Description |
|---|---|---|
| `scope.type` | `Cluster` | `Cluster` creates a ClusterRole and ClusterRoleBinding; `Namespaced` creates a Role and RoleBinding in each watched namespace |
| `scope.namespaces` | `[]` | Extra namespaces for `Namespaced`. The release namespace is always included |

See [docs/scopes.md](../../docs/scopes.md).

### Image

| Key | Default |
|---|---|
| `image.repository` | `ghcr.io/tokaco/pg-k8s-proxy` |
| `image.tag` | `""` (falls back to the chart's `appVersion`) |
| `image.digest` | `""` (takes precedence over `tag`) |
| `image.pullPolicy` | `IfNotPresent` |
| `imagePullSecrets` | `[]` |

### Gateway

| Key | Default | Description |
|---|---|---|
| `replicaCount` | `2` | All replicas serve traffic; only the leader writes status |
| `proxy.port` | `5432` | Port clients connect to |
| `proxy.maxConnections` | `0` | Session limit per replica; `0` means unlimited |
| `proxy.startupTimeout` | `30s` | Time allowed for the handshake |
| `proxy.dialTimeout` | `5s` | Time allowed to reach a backend |
| `proxy.shutdownGracePeriod` | `30s` | How long sessions may continue after shutdown begins |
| `proxy.keepAlivePeriod` | `30s` | TCP keepalive interval |

### TLS

| Key | Default | Description |
|---|---|---|
| `proxy.tls.mode` | `disable` | `disable`, `allow`, or `require` |
| `proxy.tls.secretName` | `""` | Secret holding `tls.crt` and `tls.key`; required unless mode is `disable` |
| `proxy.tls.clientCASecretName` | `""` | Secret holding `ca.crt`, to require and verify client certificates |

TLS cannot be passed through to the backend: the gateway has to read the startup
message to learn which database is wanted. It is terminated here and, where a
route's `spec.tls` asks for it, established afresh towards the backend.

```bash
helm install pg-k8s-proxy oci://ghcr.io/tokaco/charts/pg-k8s-proxy -n pgproxy \
  --set proxy.tls.mode=require \
  --set proxy.tls.secretName=pg-k8s-proxy-tls
```

### Discovery

| Key | Default | Description |
|---|---|---|
| `serviceDiscovery.enabled` | `true` | Generate a `PostgresRoute` for each matching Service |
| `serviceDiscovery.labelSelector` | `app.kubernetes.io/name=postgresql` | Which Services to adopt |
| `serviceDiscovery.databaseAnnotation` | `pgproxy.io/database-name` | Annotation carrying the database name |

### Service

| Key | Default | Description |
|---|---|---|
| `service.type` | `ClusterIP` | `ClusterIP`, `NodePort`, or `LoadBalancer` |
| `service.port` | `5432` | Published port |
| `service.sessionAffinity` | `ClientIP` | Pins a client to one replica; query cancellation does not work without it |
| `service.sessionAffinityTimeoutSeconds` | `10800` | Affinity lifetime |
| `service.externalTrafficPolicy` | `""` | `Local` preserves the client source IP |

A cancellation arrives on a new connection carrying only the key one particular
replica issued. If that connection lands on a different replica, the cancellation
is lost — hence the `ClientIP` default.

### Metrics and probes

| Key | Default | Description |
|---|---|---|
| `metrics.port` | `9090` | The one port serving `/metrics` |
| `metrics.serviceMonitor.enabled` | `false` | Create a Prometheus Operator ServiceMonitor |
| `health.port` | `8080` | Port serving `/healthz` and `/readyz`, and nothing else |

Probes and metrics are on separate ports because they have different audiences
and only one of them can be restricted. The kubelet connects from the node, so
no pod selector matches it and the probe port has to stay open; the metrics
carry a `database` label, so that endpoint enumerates every routed database name
and is worth restricting to your scrapers. A NetworkPolicy distinguishes them by
port, not by HTTP path, so a single shared port would make that impossible.

### Network policy

| Key | Default | Description |
|---|---|---|
| `networkPolicy.enabled` | `false` | Create a NetworkPolicy for the gateway pods |
| `networkPolicy.allowedClients` | `[]` | Selectors allowed to reach the PostgreSQL port. Empty allows every pod |
| `networkPolicy.allowedScrapers` | `[]` | Selectors allowed to reach the metrics port. Empty allows every pod |
| `networkPolicy.egress.enabled` | `false` | Also emit egress rules. Leave off unless the cluster denies egress by default |

```yaml
networkPolicy:
  enabled: true
  allowedClients:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: apps
  allowedScrapers:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: monitoring
```

### CRDs

| Key | Default | Description |
|---|---|---|
| `crds.install` | `true` | Install the CRD. Turn it off for a second release in the same cluster |
| `crds.keep` | `true` | Keep the CRD and its `PostgresRoute` objects when the release is uninstalled |

The CRD lives in `templates/` rather than `crds/`: Helm never upgrades anything in
`crds/`, so a chart upgrade would silently leave an old schema in place.

### RBAC

| Key | Default | Description |
|---|---|---|
| `rbac.create` | `true` | Create the Role or ClusterRole and its binding |
| `rbac.readCABundleSecrets` | `false` | Grant access to Secrets holding backend CA bundles |

### Reliability

| Key | Default |
|---|---|
| `podDisruptionBudget.enabled` | `true` |
| `podDisruptionBudget.minAvailable` | `1` |
| `topologySpreadConstraints` | spread across `kubernetes.io/hostname` |
| `terminationGracePeriodSeconds` | `60` |
| `autoscaling.enabled` | `false` |

`terminationGracePeriodSeconds` must exceed `proxy.shutdownGracePeriod`, or the
kubelet kills the pod before its sessions can drain.

### Routes in the release

Routes can ship with the chart:

```yaml
routes:
  - name: billing
    spec:
      database: billing
      backend:
        type: Service
        service:
          name: cnpg-billing-rw
          namespace: databases
          port: "5432"
```

The full set of values is in `values.yaml`; types and permitted values are
enforced by `values.schema.json` at install time.

## Verifying

```bash
helm test pg-k8s-proxy -n pgproxy
kubectl get postgresroutes -A
```

## Uninstalling

```bash
helm uninstall pg-k8s-proxy -n pgproxy
```

With `crds.keep=true` (the default) the CRD and every `PostgresRoute` survive. To
remove them too:

```bash
kubectl delete crd postgresroutes.pgproxy.io
```

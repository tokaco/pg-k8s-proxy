# Scopes and RBAC

The operator can serve either the whole cluster or an explicit set of namespaces.
One chart value decides which, and it governs both the RBAC objects created and
what enters the informer cache.

## Cluster

```bash
helm install pg-k8s-proxy oci://ghcr.io/tokaco/charts/pg-k8s-proxy \
  -n pgproxy --create-namespace
```

This creates:

- a `ServiceAccount` in the release namespace;
- a `ClusterRole` and `ClusterRoleBinding` granting cluster-wide access;
- a `Role` and `RoleBinding` for `Lease` objects in the release namespace, for
  leader election.

The cache is not namespace-scoped, so the operator sees `PostgresRoute` and
`Service` objects anywhere and can route databases from any namespace.

Cluster-scoped object names include the release namespace
(`<release>-<chart>-<namespace>`), so two releases in different namespaces do not
collide.

## Namespaced

```bash
helm install pg-k8s-proxy oci://ghcr.io/tokaco/charts/pg-k8s-proxy \
  -n pgproxy --create-namespace \
  --set scope.type=Namespaced \
  --set 'scope.namespaces={apps,databases}'
```

This creates:

- a `ServiceAccount` in the release namespace;
- a `Role` and `RoleBinding` in each watched namespace;
- a `Role` and `RoleBinding` for `Lease` objects in the release namespace.

The watched namespaces are the release namespace plus everything listed under
`scope.namespaces`. The container receives `--watch-namespaces=<list>`, which
narrows the informer cache: the operator not only lacks permission outside those
namespaces, it holds nothing from them in memory either.

No cluster-scoped object is created at all. This is the option that passes in
multi-tenant clusters where a `ClusterRole` will not be granted.

### The namespaces must already exist

Helm does not create the namespaces its `Role` and `RoleBinding` objects go into,
so create them first:

```bash
kubectl create namespace apps
kubectl create namespace databases
```

### Adding a namespace

This is an ordinary upgrade:

```bash
helm upgrade pg-k8s-proxy oci://ghcr.io/tokaco/charts/pg-k8s-proxy -n pgproxy \
  --set scope.type=Namespaced \
  --set 'scope.namespaces={apps,databases,reporting}'
```

The `--watch-namespaces` argument changes, so the pods restart: a
controller-runtime cache is configured when the manager is built and does not
widen at runtime.

## Which permissions, and why

| Resource | Verbs | Why |
|---|---|---|
| `pgproxy.io/postgresroutes` | get, list, watch, create, update, patch, delete | The routing table. Write access is what lets discovery materialise Services into routes |
| `pgproxy.io/postgresroutes/status` | get, update, patch | The `Accepted`, `Resolved`, and `Programmed` conditions |
| `pgproxy.io/postgresroutes/finalizers` | update | Required to set an `ownerReference` on routes |
| `services` | get, list, watch | Resolving named ports, and discovery |
| `events` | create, patch | Reconciler events |
| `secrets` | get, list, watch | Only with `rbac.readCABundleSecrets=true` |
| `coordination.k8s.io/leases` | full access in the release namespace | Leader election |

If discovery is not wanted, the operator only ever reads hand-written routes:

```bash
helm install pg-k8s-proxy oci://ghcr.io/tokaco/charts/pg-k8s-proxy -n pgproxy \
  --set serviceDiscovery.enabled=false
```

The chart still grants the full set of verbs. To narrow them, set
`rbac.create=false` and supply your own Role or ClusterRole.

## CA bundle Secrets

`rbac.readCABundleSecrets=true` grants `get`, `list`, and `watch` on `secrets`
within the release's scope. That is a noticeably broader permission than anything
else here, and it is worth enabling only if routes really do verify backend
certificates through `spec.tls.caSecretRef`.

The mitigation is that the informer cache is narrowed by the selector
`pgproxy.io/ca-bundle=true`, so the operator only reads and holds Secrets that
were explicitly marked. Kubernetes RBAC cannot restrict access by label, so the
permission granted is wider than the access actually used — worth knowing during
an audit.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: legacy-postgres-ca
  namespace: databases
  labels:
    pgproxy.io/ca-bundle: "true"
data:
  ca.crt: <base64>
```

## How many releases to install

One cluster-scoped release serves the whole cluster and gives clients a single
address, which is usually the point of installing a gateway at all.

Several namespaced releases make sense when teams need isolation: each gets its
own gateway, its own quota, and its own blast radius. Any number can coexist in
one cluster, because namespaced releases create no cluster-scoped objects and
therefore cannot collide.

The CRD is shared cluster-wide either way. Install it with the first release and
set `crds.install=false` on the others, so Helm does not contend over ownership of
the same object.

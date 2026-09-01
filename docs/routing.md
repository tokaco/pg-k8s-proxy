# Routing

A client connects to the gateway naming the database it wants; the gateway reads
that name out of the startup message and uses it to pick a backend. No prefixes,
suffixes, or proxy-specific syntax appear in the connection string:

```bash
psql -h pg-k8s-proxy.pgproxy.svc.cluster.local -p 5432 -U alice -d billing
```

## PostgresRoute

```yaml
apiVersion: pgproxy.io/v1alpha1
kind: PostgresRoute
metadata:
  name: billing
  namespace: databases
spec:
  # The name clients connect with. Defaults to the object's name.
  database: billing

  # The name forwarded to the backend. Defaults to the value of database.
  # Use it when the externally visible name differs from the one on the instance.
  targetDatabase: billing_prod

  # Tie-breaker when several routes claim the same name. Highest wins.
  priority: 0

  backend:
    type: Service           # Service or Address
    service:
      name: billing-rw
      namespace: databases  # defaults to the route's namespace
      port: "5432"          # a port number, or a port name on the Service
```

There are five fields and everything but `backend` has a sensible default. A
minimal working route:

```yaml
apiVersion: pgproxy.io/v1alpha1
kind: PostgresRoute
metadata:
  name: billing            # clients connect with -d billing
  namespace: databases
spec:
  backend:
    type: Service
    service:
      name: billing-rw     # port defaults to 5432
```

### Backends outside the cluster

```yaml
spec:
  backend:
    type: Address
    address:
      host: postgres.legacy.example.com
      port: 5432
  tls:
    mode: VerifyFull
    caSecretRef:
      name: legacy-postgres-ca   # needs the label pgproxy.io/ca-bundle=true
      key: ca.crt
```

The host is resolved at connection time rather than at reconcile time, so a DNS
change is picked up without operator intervention.

For the gateway to read the CA Secret, the chart must be installed with
`rbac.readCABundleSecrets=true` and the Secret must carry the label
`pgproxy.io/ca-bundle=true` — the informer cache is narrowed by that label.

## Label-based discovery

Discovery is on by default (`serviceDiscovery.enabled=true`). Every Service
matching the selector gets a generated `PostgresRoute` named
`<service>-discovered`, owned by that Service.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: postgres-billing
  labels:
    app.kubernetes.io/name: postgresql       # serviceDiscovery.labelSelector
  annotations:
    pgproxy.io/database-name: billing        # otherwise the Service name
    pgproxy.io/port: postgresql              # otherwise see below
    pgproxy.io/priority: "-100"              # otherwise -100
    pgproxy.io/ignore: "false"               # "true" opts the Service out
spec:
  ports:
    - name: postgresql
      port: 5432
```

The port is chosen in this order: the `pgproxy.io/port` annotation, then a port
named `postgresql` or `postgres`, then port `5432`, then the Service's only port.

Discovery materialises into real `PostgresRoute` objects rather than routing
around them. That means the entire routing table is visible in one command, every
entry carries status, and both sources go through the same conflict resolution:

```bash
kubectl get postgresroutes -A
```

There are three ways to take an instance out of the gateway: remove the label, add
`pgproxy.io/ignore: "true"`, or delete the Service. In the first two cases the
controller deletes the generated route; in the third, Kubernetes garbage
collection does, through the `ownerReference`.

## Name conflicts

Two databases cannot share a name, because the name is the routing key. When
several routes claim one name, the winner is:

1. the highest `spec.priority`;
2. on a tie, the oldest `creationTimestamp`;
3. on an exact tie, the lowest `namespace/name`.

The ordering is total and does not depend on the order the API server returned the
list in, so every replica picks the same winner.

The loser is neither deleted nor broken. It gets `Accepted=False` with reason
`DatabaseNameConflict` and a `status.conflictingRoute` field:

```console
$ kubectl describe postgresroute postgres-billing-discovered -n databases
Status:
  Conflicting Route:  databases/billing
  Conditions:
    Type:     Accepted
    Status:   False
    Reason:   DatabaseNameConflict
    Message:  Database name "billing" is already claimed by databases/billing,
              which has a higher priority or is older.
```

Generated routes are created with `priority: -100`, so a hand-written route at the
default priority (`0`) always overrides a discovered one. That is the intended way
to override discovery for a single database.

## Status conditions

| Condition | `True` means |
|---|---|
| `Accepted` | The route won its database name |
| `Resolved` | The backend address could be determined |
| `Programmed` | The gateway is actually routing traffic here |

The conditions are split apart deliberately. `Accepted=True, Resolved=False` means
"the name is yours, but the Service is missing or has no such port", whereas
`Accepted=False` means "someone else took the name". Those are two different
incidents calling for two different fixes.

A route that wins its name but fails to resolve keeps the name. Yielding it would
quietly send traffic to a backend the operator did not choose.

## Troubleshooting

```bash
# What is routable at all
kubectl get postgresroutes -A

# Why one route is not working
kubectl describe postgresroute <name> -n <namespace>

# What the gateway itself sees
kubectl port-forward -n pgproxy svc/pg-k8s-proxy-metrics 8080:8080
curl -s localhost:8080/readyz

# Rejections by reason
curl -s localhost:8080/metrics | grep pgproxy_connections_total
```

If a client gets `database "x" does not exist` while the route plainly exists, it
almost certainly lost a name conflict or failed to resolve; `kubectl describe`
will say which.

# Migrating from the previous proxy

The previous version was a single Deployment that listed Services by label every
30 seconds and kept the result in a map. This version is deployed by a Helm chart
and takes its routes from a CRD.

## Breaking changes

| | Before | Now |
|---|---|---|
| `/metrics` | port 8080 | port 9090 only; repoint your scrape configuration |
| `/health` | 503 when no backends existed | always 200 while the process is alive |
| Image entry point | `entrypoint.sh` and bash | the binary directly; distroless, no shell |
| Default selector | `app.kubernetes.io/name=postgresql` | unchanged |
| `POLL_INTERVAL` | polling interval | ignored; the gateway watches instead of polling |

The `/health` change is a fix, not a regression. A cluster with no PostgreSQL
instances used to put the gateway into a permanent restart loop: the liveness
probe returned 503, the kubelet killed the pod, it came back, and still found no
backends. Liveness no longer depends on cluster state; whether there is anywhere
to route is reported by the `pgproxy_routes` metric instead.

## Environment variables

The old variables are still read, and each one logs a warning naming its
replacement, so an existing Deployment can simply be pointed at the new image:

| Variable | Replacement |
|---|---|
| `POSTGRES_PORT` | `--proxy-bind-address` |
| `HEALTH_PORT` | `--health-bind-address` |
| `LABEL_SELECTOR` | `--service-discovery-selector` |
| `KUBERNETES_NAMESPACE` | `--watch-namespaces` |
| `POLL_INTERVAL` | nothing; there is no polling any more |

An explicitly set flag always beats an environment variable.

## Migration steps

1. **Install the chart alongside the old Deployment**, under a temporary name so
   existing clients keep using the old path:

   ```bash
   helm install pg-k8s-proxy-new oci://ghcr.io/tokaco/charts/pg-k8s-proxy \
     -n pgproxy --create-namespace \
     --set serviceDiscovery.labelSelector='app.kubernetes.io/name=postgresql'
   ```

2. **Compare the routing table.** The selector is the same as before, so discovery
   should produce exactly the databases the old proxy saw:

   ```bash
   kubectl get postgresroutes -A
   ```

   Where two Services claimed the same name, the old implementation silently kept
   whichever came last in the list. The new one reports the conflict explicitly;
   resolve any it finds with a priority or a rename.

3. **Test connectivity** through the temporary Service.

4. **Cut clients over**, either by renaming the chart's Service to the old name
   (`--set fullnameOverride=pg-k8s-proxy`) or by changing the address clients use.

5. **Remove the old release:**

   ```bash
   kubectl delete deployment pg-k8s-proxy
   kubectl delete service pg-k8s-proxy
   kubectl delete clusterrolebinding pg-k8s-proxy
   kubectl delete clusterrole pg-k8s-proxy
   kubectl delete serviceaccount pg-k8s-proxy
   ```

## What started working on its own

None of this needs any change on the client side:

- **Ctrl-C in psql.** A cancellation request used to arrive with no database name,
  match no route, and get its connection closed while the query kept running.
- **A real error instead of a dropped connection.** An unknown database used to
  leave clients printing `server closed the connection unexpectedly`. Now it is
  `FATAL: database "x" does not exist` with SQLSTATE `3D000`.
- **Stable startup parameter order.** The packet used to be rebuilt by iterating a
  map, so its byte order changed from connection to connection.
- **No data race.** The backend table was read and written from different
  goroutines without synchronisation.

## Rolling back

The old manifests under `k8s/` are gone in this version, but rolling back is a
`helm uninstall` plus `kubectl apply` of the previous manifests from git history.
The CRD survives an uninstall (`crds.keep=true`), so `PostgresRoute` objects
remain and a later reinstall picks them up as they are.

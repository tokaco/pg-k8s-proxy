# Security Policy

## Supported versions

While the project is pre-1.0, security fixes go onto the latest minor release
only. Once 1.0.0 ships, this table will list the supported branches.

| Version | Supported |
|---|---|
| Latest minor | Yes |
| Anything older | No |

## Reporting a vulnerability

Please report vulnerabilities privately, not through a public issue.

Use GitHub's private reporting:
[Report a vulnerability](https://github.com/tokaco/pg-k8s-proxy/security/advisories/new).

Please include:

- what the issue is and what an attacker gains from it;
- the versions affected;
- how to reproduce it, ideally as manifests or a test;
- any mitigation you already know of.

You should get an acknowledgement within a few days. Once a fix is ready it will
be released along with an advisory crediting you, unless you would rather not be
named.

## Threat model

Some properties of the design are worth stating, because they shape what counts
as a vulnerability.

**The gateway never handles credentials.** It does not authenticate clients, does
not store passwords, and does not inspect authentication messages. Those are
relayed untouched between the client and the backend, which authenticates exactly
as it would without a proxy.

**Any client that can reach the gateway can reach any routed database.** The
gateway is a router, not an authorisation boundary: it will connect a client to
whatever backend serves the database it names, and the backend then decides
whether that client may connect. Restrict who can open a TCP connection to the gateway
with a NetworkPolicy (`networkPolicy.enabled=true`) and keep the backends'
`pg_hba.conf` doing the real work.

**Cancellation keys are unguessable.** The gateway issues its own
`BackendKeyData` from `crypto/rand` and never hands a client the backend's real
key, so knowing one session's key does not let an attacker cancel another's, nor
cancel anything by talking to the backend directly.

**Plaintext is the default on the client side.** `proxy.tls.mode` defaults to
`disable`. TLS cannot be passed through to the backend, because the gateway must
read the startup message to learn which database is wanted, so deployments that
need encryption in transit have to terminate it at the gateway
(`proxy.tls.mode=require`) and, if the backend leg also matters, re-originate it
per route with `spec.tls`.

**Reading CA Secrets is opt-in.** `rbac.readCABundleSecrets` is off by default.
Turning it on grants `get`, `list`, and `watch` on Secrets within the release's
scope. The operator only ever reads Secrets labelled `pgproxy.io/ca-bundle=true`,
and caches nothing else, but Kubernetes RBAC cannot express a label restriction,
so the permission granted is broader than the access used.

**Unrouted database names are not used as metric labels.** A database name from
an unrouted connection is attacker-controlled, so it is reported in a single
`<unrouted>` bucket rather than becoming unbounded metric cardinality.

**Metrics enumerate routed database names.** `pgproxy_connections_total` and its
neighbours carry a `database` label, so anything that can reach the metrics port
learns every database the gateway routes. The endpoint is unauthenticated, which
is why it lives on its own port: probes must stay reachable from the node, but
metrics can be restricted with `networkPolicy.allowedScrapers`. Set it if that
enumeration matters in your cluster.

## Supply chain

Released images are:

- built by GitHub Actions from a tagged commit, never from a workstation;
- published to `ghcr.io/tokaco/pg-k8s-proxy` for `linux/amd64` and
  `linux/arm64`;
- signed keylessly with [cosign](https://github.com/sigstore/cosign), bound to
  the workflow's OIDC identity;
- shipped with an SBOM and SLSA provenance attached to the image index.

Verify a release before deploying it:

```bash
cosign verify ghcr.io/tokaco/pg-k8s-proxy:<version> \
  --certificate-identity-regexp '^https://github.com/tokaco/pg-k8s-proxy/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The runtime image is `gcr.io/distroless/static-debian13:nonroot`: no shell, no
package manager, and a non-root user by default. The chart's pod and container
security contexts satisfy the restricted Pod Security Standard.

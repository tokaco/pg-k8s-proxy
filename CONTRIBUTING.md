# Contributing

Thanks for taking the time. This document covers how to get a change built,
tested, and merged.

## Getting set up

You need Go 1.25+, Docker, Helm 3.8+, and `make`. Everything else the build needs
is installed into `./bin` on demand.

```bash
git clone https://github.com/tokaco/pg-k8s-proxy.git
cd pg-k8s-proxy
make tools     # controller-gen and golangci-lint into ./bin
make check     # fmt, vet, lint, tests, chart lint
```

The Go module lives in `src/`, so `go` commands run from there. Every `make`
target already handles that.

## Repository layout

```
src/                    Go module
  api/v1alpha1/         CRD types; the source of truth for the schema
  cmd/pg-k8s-proxy/     entry point and wiring
  internal/pgwire/      PostgreSQL wire protocol
  internal/registry/    routing table and conflict resolution
  internal/controller/  reconcilers
  internal/proxy/       data plane
  internal/config/      flags and configuration
  internal/health/      probe server
charts/pg-k8s-proxy/    Helm chart
deploy/crds/            generated CRDs, committed
docker/                 Dockerfile
examples/               example manifests
hack/                   build scripts
docs/                   documentation
```

## Making a change

### Changing the API

`src/api/v1alpha1` is the source of truth for the CRD. After editing it,
regenerate everything that derives from it:

```bash
make generate manifests
```

That rewrites the DeepCopy methods and `deploy/crds/`, then regenerates the chart
template from the CRD. All of it is committed, and CI fails if it is stale.

### Running it

Against the cluster in your current kubeconfig context, on ports that will not
collide with a local PostgreSQL:

```bash
make run
```

To exercise the whole thing in a cluster:

```bash
make image
make install
make examples
make routes
```

### Tests

```bash
make test              # go test -race
make test-integration  # against a throwaway PostgreSQL in Docker
make cover             # coverage report in a browser
```

The integration tests sit behind a build tag and run the gateway in front of a
real PostgreSQL with a real `psql` client. They cover what a fake backend
cannot: the SCRAM exchange relayed untouched, ParameterStatus, the substituted
BackendKeyData, and a query round trip. Anything touching the data plane's
handling of the authentication phase belongs there.

New behaviour needs a test. The existing suites are worth reading first, because
they set the expectations:

- `internal/pgwire` — table-driven tests over the protocol, including malformed
  input. Anything parsing bytes off a socket needs a hostile-input case.
- `internal/registry` — conflict resolution is tested for determinism by building
  the same route set in both orders and asserting the same winner. Any change to
  the ordering rules has to keep that property.
- `internal/proxy` — end-to-end against a fake PostgreSQL backend that speaks the
  real handshake. Data-plane changes belong here rather than in a unit test of an
  internal helper.
- `internal/controller` — reconcilers against a fake client, asserting on status
  conditions and on idempotency.

CI additionally runs the whole thing on a kind cluster and connects with a real
`psql`, so a change that passes locally but breaks the deployed path will be
caught before merge.

## Commit messages

The project releases with [release-please](https://github.com/googleapis/release-please),
which derives the next version from
[Conventional Commits](https://www.conventionalcommits.org/). The prefix is what
decides the version bump, so it matters:

| Prefix | Effect | Use for |
|---|---|---|
| `fix:` | patch | Bug fixes |
| `feat:` | minor | New capability |
| `feat!:` or a `BREAKING CHANGE:` footer | major | Anything requiring a user to change their manifests, values, or connection setup |
| `perf:` | patch | Performance work |
| `docs:`, `test:`, `refactor:`, `ci:`, `build:`, `chore:` | none | Everything else |

```
feat(routing): support port names in Service backends

Named ports are how CloudNativePG and most operators expose PostgreSQL, so
requiring a number forced users to hard-code one that could change.
```

Before 1.0.0 a breaking change bumps the minor version, not the major.

## Pull requests

1. Branch from `master`.
2. Make the change, with tests.
3. Run `make check`.
4. Open the PR against `master`, with a Conventional Commit title — the title is
   what release-please reads when the PR is squashed.

CI runs Go tests with the race detector, the linter, a generated-files check,
chart linting and schema validation across every supported value combination, a
multi-architecture image build, and the kind end-to-end suite.

## Releasing

Releases are automated; see [docs/releasing.md](docs/releasing.md). In short:
merging to `master` updates a release pull request, and merging that pull request
tags the version and publishes the image and chart.

## Design notes

Two decisions come up often enough to state up front, because changes that
contradict them will be sent back:

**The routing table is a pure function of the route set.** `registry.Build` must
stay deterministic and side-effect free. Every replica runs it independently and
has to reach the same answer, which is what lets the data plane serve traffic
without waiting for a leader. Do not introduce randomness, wall-clock reads, or
API calls into it.

**Status is observability, not the source of truth.** Nothing on the data path
may read a `PostgresRoute` status. If the leader is down, traffic must keep
flowing correctly on stale status.

## Reporting security issues

Do not open a public issue. See [SECURITY.md](SECURITY.md).

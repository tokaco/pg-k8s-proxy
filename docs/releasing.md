# Releasing

Releases are automated. Nobody tags by hand, and nobody pushes an image or a
chart from a workstation.

## How a version is decided

[release-please](https://github.com/googleapis/release-please) reads the
[Conventional Commit](https://www.conventionalcommits.org/) prefixes of every
commit merged since the last release and derives the next semantic version from
them:

| Commit prefix | Bump |
|---|---|
| `fix:`, `perf:` | patch |
| `feat:` | minor |
| `feat!:`, or any commit with a `BREAKING CHANGE:` footer | major |
| `docs:`, `test:`, `refactor:`, `ci:`, `build:`, `chore:` | none |

While the version is below `1.0.0`, a breaking change bumps the minor rather than
the major, per semver's rules for initial development.

Three things move together and must never drift apart:

- the git tag, `vX.Y.Z`;
- `charts/pg-k8s-proxy/Chart.yaml` `version` and `appVersion`;
- `version.txt`.

release-please updates all of them in the release pull request, and the release
workflow refuses to publish if `Chart.yaml` disagrees with the tag. That check is
what makes a hand-cut tag fail loudly instead of shipping a chart that installs
an image which does not exist.

## The flow

```
 commit to master
        │
        ▼
 release-please.yml ──▶ opens/updates a "chore(master): release X.Y.Z" PR
        │                 (CHANGELOG.md, Chart.yaml, version.txt)
        │
   merge that PR
        │
        ▼
 release-please.yml ──▶ tags vX.Y.Z and publishes a GitHub Release
        │
        ▼
    release.yml
        ├─ resolve  derive the version from the tag
        ├─ verify   re-run tests and chart lint on the tagged tree
        ├─ image    build, push, and sign the multi-arch image
        └─ chart    package, push to the OCI registry, attach to the release
```

The `verify` job re-runs the suite against exactly the tagged tree rather than
trusting the CI run of the commit the tag was cut from. A tag is permanent, so it
is worth the extra minutes.

## What gets published

**Image**, at `ghcr.io/tokaco/pg-k8s-proxy`, for `linux/amd64` and
`linux/arm64`:

| Tag | When |
|---|---|
| `X.Y.Z` | every release, including prereleases |
| `X.Y`, `X`, `latest` | stable releases only |

A prerelease deliberately gets only its exact version. Moving `latest` or a
floating major onto an untested build is how consumers get surprised.

The image is signed keylessly with cosign and carries an SBOM and SLSA
provenance. See [SECURITY.md](../SECURITY.md) for how to verify it.

**Chart**, at `oci://ghcr.io/tokaco/charts/pg-k8s-proxy`, and also attached to
the GitHub Release as a `.tgz`. After pushing, the workflow pulls the chart back
and renders it, so a release fails if the published artefact is not actually
consumable.

## Installing a specific version

```bash
helm install pg-k8s-proxy oci://ghcr.io/tokaco/charts/pg-k8s-proxy \
  --version 0.1.0 \
  --namespace pgproxy --create-namespace
```

The chart resolves the image from its `appVersion`, so a pinned chart version
pins the image with it. Override with `image.tag` or, for a stricter pin,
`image.digest`.

## Republishing a tag

If a publish fails halfway — a registry outage, an expired token — re-run it
without cutting a new version:

```bash
gh workflow run release.yml -f tag=v0.1.0
```

This is for republishing an existing tag only. Changing what a released version
contains is not something to do; cut a patch release instead.

## Prereleases

Merging a release PR whose version carries a prerelease suffix (`v0.2.0-rc.1`)
publishes the image and chart under that exact version and leaves `latest` alone.
Install one explicitly:

```bash
helm install pg-k8s-proxy oci://ghcr.io/tokaco/charts/pg-k8s-proxy \
  --version 0.2.0-rc.1 --devel \
  --namespace pgproxy --create-namespace
```

`--devel` is required: Helm ignores prerelease versions otherwise.

## Required repository settings

The workflows use only `GITHUB_TOKEN`; no secrets need to be configured. Two
settings have to be right:

- **Settings → Actions → General → Workflow permissions**: "Read and write
  permissions", and "Allow GitHub Actions to create and approve pull requests".

  Without the second checkbox release-please fails with `GitHub Actions is not
  permitted to create or approve pull requests` — but only once there is
  actually something to release. While the newest tag sits on `HEAD` it finds
  no releasable commits, opens nothing, and passes, so the misconfiguration
  stays hidden until the first commit after a release.

  As an alternative, set a `RELEASE_PLEASE_TOKEN` secret holding a personal
  access token with `contents: write` and `pull-requests: write`. The workflow
  prefers it when present. That lifts the checkbox requirement and additionally
  lets the release pull request trigger CI, which a pull request opened by the
  built-in token never does. Either way coverage is intact, because the release
  workflow re-runs the whole suite against the tagged tree before publishing.
- **Packages**: the first publish creates `pg-k8s-proxy` and
  `charts/pg-k8s-proxy` as private packages. Make them public if the release is
  meant to be installable without authentication.

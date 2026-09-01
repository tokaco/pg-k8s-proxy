## What this changes

<!-- What does this do, and why is it needed? Link any related issue. -->

## Type of change

<!-- The PR title must be a Conventional Commit; it is what decides the version
     bump when this is squashed. See CONTRIBUTING.md. -->

- [ ] `fix:` — bug fix (patch release)
- [ ] `feat:` — new capability (minor release)
- [ ] `feat!:` / `BREAKING CHANGE:` — requires users to change manifests, values,
      or connection setup (major release)
- [ ] `docs:` / `test:` / `refactor:` / `ci:` / `chore:` — no release

## Checklist

- [ ] `make check` passes locally
- [ ] Tests cover the new behaviour
- [ ] `make generate manifests` was run, if `src/api/` changed
- [ ] Chart values that changed are documented in `charts/pg-k8s-proxy/README.md`
      and constrained in `values.schema.json`
- [ ] Breaking changes are described below, with the upgrade path

## Breaking changes and upgrade notes

<!-- Leave "None" if there are none. -->

None.

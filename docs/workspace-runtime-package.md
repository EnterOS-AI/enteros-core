# Workspace Runtime Package

The repository is named `molecule-ai-workspace-runtime`. Its published Python
distribution is `molecules-workspace-runtime`, imported as `molecule_runtime`.
Workspace template images and external integrations must use the distribution
name when installing it.

## Source Of Truth

The source of truth is the standalone Gitea repo:

```text
https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-runtime
```

Do not add runtime source back under `molecule-core/workspace/`. The core repo
owns the platform server, canvas, provisioning, and tests around the installed
runtime package.

## Package Registry

The runtime package is published to the Molecule AI Gitea package registry:

```text
https://git.moleculesai.app/api/packages/molecule-ai/pypi/simple/
```

PyPI is intentionally not part of the critical path. Template Dockerfiles,
external-runtime snippets, and CI install checks should use the Gitea registry.

## Release Flow

1. Land a reviewed, green PR in the `molecule-ai-workspace-runtime` repository.
2. The exact-main `auto-release` workflow gates the commit and creates the next
   `runtime-vX.Y.Z` patch tag. `pyproject.toml` on `main` intentionally lags;
   the tag is the release-version source of truth.
3. The tag-triggered `publish-runtime` workflow stamps that version, builds the
   wheel and sdist, publishes `molecules-workspace-runtime` to the Gitea
   registry, and verifies a clean install from that registry.
4. The workflow opens controlled `.runtime-version`/requirements bump PRs on
   consumer templates. Eligible exact-scope bumps are merged by the dedicated
   consumer-bump automation and each template's normal main pipeline rebuilds
   and verifies its image.

## Image Propagation Boundary

A green template publish proves that the image exists in the Gitea registry. It
does not prove that an already-running managed workspace changed image.
Control-plane runtime pins select images for fresh provisions and explicit
reprovisions; existing-fleet convergence is a separate deployment concern.

The core server does not run a background `:latest` watcher. The
`scripts/refresh-workspace-images.sh` helper and
`POST /admin/workspace-images/refresh` endpoint are explicit, single-host tools
for registry-backed self-hosts, not managed release or fleet-rollout steps.

## Core Repo Contract

`molecule-core` must not ship editable runtime code. Its responsibilities are:

- Test platform behavior against the installed runtime contract.
- Keep MCP/registry/TenantGuard behavior compatible with the runtime package.
- Fail CI if `workspace/` or legacy build-from-workspace scripts are restored.

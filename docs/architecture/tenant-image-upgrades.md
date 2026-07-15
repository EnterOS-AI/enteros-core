# Image Upgrade Boundaries

Tenant-platform images and workspace-runtime template images are separate
release surfaces. Neither uses the retired GHCR/EC2 sidecar design that this
page previously described.

## Tenant platform image

The active Gitea Actions workflows build and publish the tenant application
image. The control-plane deployment pipeline selects the tenant image used for
new or explicit tenant provisions. Consult those workflows and control-plane
configuration for the exact staging or production pin; this repository does
not define a host-side auto-updater.

## Workspace template images

The runtime package release flow opens version-bump PRs in the four standalone
workspace-template repositories. After each template's normal main pipeline is
green, it publishes a `workspace-template-<runtime>` image to
`registry.moleculesai.app/molecule-ai`.

Managed workspace selection is controlled by the control plane's runtime image
pins. A published image or an advanced pin affects fresh provisions and
explicit reprovisions; it is not evidence that an already-running workspace
changed image.

The core server has no background `:latest` watcher. The previous watcher only
started with the self-hosted Docker provisioner, conflicted with local-build
mode, and could not update managed workspaces. Existing managed-fleet
convergence remains a separate redeploy concern and must not be described as
automatic until its real end-to-end path is wired and verified.

## Explicit self-host maintenance

Registry-backed self-hosts can deliberately pull templates with either:

```bash
bash scripts/refresh-workspace-images.sh --no-recreate
```

or the admin-authenticated
`POST /admin/workspace-images/refresh?recreate=false` endpoint. Both are
single-host maintenance operations. Enabling recreation removes matching local
`ws-*` containers and interrupts in-flight work, so it must be an explicit
operator choice.

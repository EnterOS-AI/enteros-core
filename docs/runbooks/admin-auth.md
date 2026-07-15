# Admin Authentication Runbook

## Strict admin/workspace auth is fail-CLOSED — `ADMIN_TOKEN` is the bootstrap credential

Per the CTO "nothing should be fail-open" directive, the strict middleware
surfaces have no dev-mode / zero-token / DB-outage hatch that grants access.
This includes:

- `AdminAuth` and `WorkspaceAuth` (admin + per-workspace routes),
- the public A2A send/queue classifier (apart from its explicitly gated
  combined self-host/dev same-origin Canvas path).

`CanvasOrBearer` is not a security boundary: on the combined tenant proxy it
accepts a forgeable same-origin header heuristic and is therefore restricted to
the cosmetic `PUT /canvas/viewport` route.

`validateDiscoveryCaller` is different: workspaces with no live token remain
legacy/bootstrap-compatible, while token-enrolled callers must authenticate
and datastore errors fail closed with `503`. Registry heartbeat and card update
have their own documented transition behavior, including an availability-first
datastore-error branch. Do not copy either compatibility exception onto a new
route.

Consequence for **admin bootstrap**: a brand-new self-hosted / dev install has
no DB-backed tokens yet, and there is no zero-token pass through admin
middleware. The **only** way to reach admin routes (and to mint the first
workspace token via `POST /admin/workspaces/:id/tokens`) is to set `ADMIN_TOKEN`
in the platform environment and present it as the bearer. This is the "local
mimics production" principle: there is no zero-config bootstrap.

- **Local dev:** `scripts/dev-start.sh` provisions a deterministic
  `ADMIN_TOKEN` into `.env` (and exports the matching `NEXT_PUBLIC_ADMIN_TOKEN`
  so the canvas authenticates with it). See `docs/quickstart.md`.
- **Self-hosted / SaaS:** set `ADMIN_TOKEN` to a strong random secret
  (`openssl rand -base64 32`) in the platform env and bake the matching
  `NEXT_PUBLIC_ADMIN_TOKEN` into the canvas bundle.

## Required: set `MOLECULE_ENV` in all non-dev environments

```bash
# In your tenant / EC2 / Railway environment variables:
MOLECULE_ENV=production
```

This matches the production tenant default. NOTE: `MOLECULE_ENV` no longer gates
any auth decision — it only drives NON-security local-dev conveniences (loopback
bind, relaxed rate limit). Setting it to `dev`/`development` does **not** relax
authentication. Staging and production smoke tests should use the real user/API
workflow: create a workspace, then mint a one-time displayed workspace bearer
with `POST /admin/workspaces/:id/tokens`.

## Admin bearer token (`ADMIN_TOKEN`)

The platform uses `ADMIN_TOKEN` as the bearer credential for admin-gated endpoints:

| Endpoint | Auth method |
|----------|-------------|
| `GET/POST/PATCH/DELETE /workspaces` | `Authorization: Bearer <ADMIN_TOKEN>` |
| `GET /admin/liveness` | `Authorization: Bearer <ADMIN_TOKEN>` |
| `POST /org/import` | `Authorization: Bearer <ADMIN_TOKEN>` |
| `POST /admin/workspaces/:id/tokens` | `Authorization: Bearer <ADMIN_TOKEN>`; plaintext token returned once |
| `PATCH /workspaces/:id` infrastructure fields (`tier`, `parent_id`, `runtime`, `workspace_dir`, `compute`) | `Authorization: Bearer <ADMIN_TOKEN>` or a verified control-plane session; workspace and org tokens are rejected |

Missing or invalid bearer → **401 in every environment** (fail-closed; no
dev-mode fail-open). If the auth datastore is unreachable, auth-gated routes
return **503** (`platform_unavailable`) — an availability tradeoff that grants no
access — rather than allowing the request through.

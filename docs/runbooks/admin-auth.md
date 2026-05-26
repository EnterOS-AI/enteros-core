# Admin Authentication Runbook

## Required: set `MOLECULE_ENV` in all non-dev environments

```bash
# In your tenant / EC2 / Railway environment variables:
MOLECULE_ENV=production
```

This matches the production tenant default and disables development-only
shortcuts. Staging and production smoke tests should use the real user/API
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

Missing or invalid `ADMIN_TOKEN` → AdminAuth fails open in dev mode (no token set), or
returns 401 in production mode (token set but invalid).

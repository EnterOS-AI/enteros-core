# Admin authentication runbook

Authentication fails closed in every environment. There is no development,
empty-database, or datastore-outage path that grants access.

## Credential classes

The current middleware recognizes four credential classes:

| Credential | Intended use | Scope |
|---|---|---|
| Verified control-plane session cookie | SaaS Canvas | Tenant membership verified upstream; accepted by admin and workspace middleware |
| Organization API token | Human/automation access | Named, revocable, audited access across the organization |
| `ADMIN_TOKEN` | Bootstrap and break-glass | Full admin and workspace access; required configuration for local and deployed servers |
| Workspace bearer token | Agent access | Bound to one workspace under `WorkspaceAuth` |

`AdminAuth` also retains a deprecated compatibility fallback: if
`ADMIN_TOKEN` is completely unset, any live workspace token can authenticate an
admin route. Do not rely on this mode. Configuring `ADMIN_TOKEN` disables that
fallback and causes workspace tokens to be rejected by `AdminAuth`.

Some update handlers are stricter than their route middleware. Infrastructure
fields (`tier`, `parent_id`, `runtime`, `workspace_dir`, and `compute`) require
the configured admin token or a verified control-plane session; organization
and workspace tokens are rejected for those fields.

## Bootstrap

Set `ADMIN_TOKEN` before the workspace server starts. For a standalone
installation, generate a strong random value and inject it through the normal
environment secret path:

```bash
openssl rand -base64 32
```

Do not commit the value or paste it into documentation. Local development uses
`scripts/dev-start.sh`, which provisions matching server and Canvas
configuration. The normal local entry point is documented in
[`docs/quickstart.md`](../quickstart.md).

After bootstrap, create named organization API tokens through Canvas when a
revocable human or automation credential is preferable. Plaintext tokens are
shown once.

## Request behavior

Use `Authorization: Bearer <token>` for bearer credentials. A valid verified
SaaS session cookie can authenticate browser requests without a bearer.

Examples of `AdminAuth` routes include:

- workspace collection/lifecycle management;
- `/settings/secrets` and legacy `/admin/secrets` aliases;
- `/org/import` and `/org/templates`;
- bundle import/export and global event surfaces; and
- admin liveness and scheduler-health endpoints.

Workspace subroutes protected by `WorkspaceAuth` accept a token bound to the
`:id`, an organization token, the configured admin token, or a verified
control-plane session.

## Failure behavior

- Missing or invalid credentials return `401`.
- A failed auth-datastore lookup returns `503` with code
  `platform_unavailable` and does not expose the underlying database error.
- `MOLECULE_ENV=dev` or `development` does not relax authentication.
- `Origin`, `Referer`, and same-origin appearance are not authentication for
  admin or workspace-data routes.

Set `MOLECULE_ENV=production` (or `prod`) in production so security-sensitive
production guards, including strict secret-key initialization, are active.

The middleware and handler-level authorization checks are the authoritative
contract. See [ADR-001](../adr/ADR-001-admin-token-scope.md) for the deprecated
workspace-token fallback and its residual risk.

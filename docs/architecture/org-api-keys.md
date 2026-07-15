# Organization API keys

Organization API keys are revocable bearer credentials for the tenant
workspace-server. They are intended for automation that cannot use a browser
session. They do not authenticate to the control-plane admin API and cannot
cross tenant boundaries.

## Current data model

The schema is owned by the migrations under `workspace-server/migrations/`.
The current `org_api_tokens` row contains:

- a SHA-256 token hash and an eight-character display prefix; plaintext is
  returned only once at mint time;
- optional `name`, `org_id`, and `expires_at` values;
- `created_by`, `created_at`, and best-effort `last_used_at` audit metadata;
- `revoked_at` for immediate revocation.

Validation accepts only a non-revoked token whose `expires_at` is absent or in
the future. An anchored token's `org_id` is used by org-scoped authorization.
Older, unanchored rows fail closed on routes that require an org owner.

## Authentication and authorization

`AdminAuth` is fail closed. It accepts:

1. a control-plane-verified WorkOS session;
2. a live organization API key;
3. the tenant `ADMIN_TOKEN` bootstrap/break-glass bearer; or
4. only when `ADMIN_TOKEN` is unset, the deprecated live workspace-token
   fallback.

There is no bearer-less fail-open mode.

An organization key is a full tenant-admin credential. It can manage tenant
workspaces, settings, plugins, bundles, templates, approvals, and other org
keys. Infrastructure-only fields remain more restricted than ordinary tenant
lifecycle operations: session/admin callers may mutate those fields where the
route permits, while an org token is rejected. The key cannot call
control-plane admin, billing, or cross-tenant endpoints.

## Mint, list, and revoke

```text
GET    /org/tokens
POST   /org/tokens
DELETE /org/tokens/:id
```

`POST /org/tokens` accepts an optional body:

```json
{
  "name": "ci-agent",
  "expires_at": "2026-08-01T00:00:00Z"
}
```

Past expiry timestamps are rejected. `expires_at` may be omitted for an
unbounded token. The response returns `auth_token` exactly once. Live anchored
tokens are capped per org (100 by default; configurable through
`ORG_TOKEN_MINT_CEILING`).

Mint authorization also depends on the actor:

- a verified human session may mint directly;
- agent/bearer callers pass the destructive-action approval gate and require a
  valid approval anchor;
- an org-token caller records its token prefix as provenance;
- an `ADMIN_TOKEN` caller records `admin-token` provenance.

All three routes use `AdminAuth`. Mint and revoke events are written to the org
token audit log; plaintext and full bearer values must never be logged.

## Security properties and limits

- Token lookup stores and compares only SHA-256 hashes.
- Revoked, expired, missing, and malformed credentials collapse to an invalid
  credential response.
- Each integration should have its own named key and, where practical, an
  expiry.
- Every current org key is full tenant admin. Roles and per-workspace scopes
  are not implemented.
- Database compromise exposes token hashes, not plaintext tokens. Secret values
  are separately protected by the secrets-encryption rules documented in
  [Secrets key custody](./secrets-key-custody.md): production requires the
  encryption key; non-production can contain explicitly versioned legacy
  plaintext rows.

Implementation authority: `internal/middleware/wsauth_middleware.go`,
`internal/handlers/org_tokens.go`, and `internal/orgtoken/`.

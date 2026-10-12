# ADR-001: Admin credential tiers and the workspace-token fallback

**Status:** Superseded in part; residual fallback retained for compatibility
**Original date:** 2026-04-17

## Historical decision

The original server let any live workspace bearer reach `AdminAuth` routes.
That made a compromised workspace token equivalent to an organization
administrator. The original Phase-H schema and bootstrap proposal in this ADR
was not the design that shipped.

## Current implementation

`workspace-server/internal/middleware/wsauth_middleware.go` is authoritative.
`AdminAuth` is fail-closed and evaluates these credential paths:

1. a control-plane session cookie verified upstream for this tenant;
2. a named, revocable organization API token;
3. the configured `ADMIN_TOKEN`; and
4. only when `ADMIN_TOKEN` is unset, a deprecated compatibility fallback that
   accepts any live workspace token.

When `ADMIN_TOKEN` is configured, workspace tokens are rejected on admin
routes. Local setup configures it, and deployed environments must do the same.
An auth-datastore failure returns `503 platform_unavailable`; it never grants
access.

`WorkspaceAuth` separately accepts the configured admin token, an organization
token, a token bound to the requested workspace, or a verified control-plane
session. Some handlers apply stricter field-level authorization: workspace
infrastructure fields such as `tier`, `parent_id`, `runtime`, `workspace_dir`,
and `compute` require `ADMIN_TOKEN` or a verified control-plane session.

## Residual risk

A deployment that omits `ADMIN_TOKEN` still exposes the original broad
workspace-token fallback on `AdminAuth` routes. That includes global secrets,
organization import, bundle import/export, global events, templates, and other
organization-wide management surfaces. Treat running without `ADMIN_TOKEN` as
a legacy compatibility mode, not a supported security posture.

## Operating decision

- Configure a strong `ADMIN_TOKEN` for bootstrap and break-glass use.
- Prefer named organization API tokens for human and automation access because
  they are revocable and audited.
- Use per-workspace tokens only for routes scoped to that workspace.
- Do not introduce a bearer-less bootstrap or trust `Origin`/`Referer` as an
  authentication boundary.
- Remove the workspace-token fallback only through a tested breaking-change
  migration; documentation must not claim it has already gone away.

See the [admin-auth runbook](../runbooks/admin-auth.md) for the current request
and failure behavior.

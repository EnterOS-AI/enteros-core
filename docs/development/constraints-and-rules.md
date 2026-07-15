# Constraints & Rules

This page is a source-of-truth index for current Core boundaries. Historical
MVP rules such as an unauthenticated API, event-sourced workspace state, or a
destructive team-expansion lifecycle no longer describe the checked-in server.

## Authentication and Authorization

There is no blanket "open API" rule. `workspace-server/internal/router/router.go`
defines authentication per route with `WorkspaceAuth`, `AdminAuth`, verified
control-plane sessions, and the smaller set of intentionally public surfaces.
Infrastructure fields such as `parent_id`, `tier`, and `runtime` require admin
or verified control-plane authority even after workspace authentication. See
[Platform API](../api-protocol/platform-api.md) for the current route matrix.

## Authoritative State and Events

- Postgres domain tables are authoritative. `workspaces` owns current workspace
  state; `canvas_layouts`, `activity_logs`, schedules, and other domain tables
  own their respective data.
- Redis supports caches, liveness, and event fanout. It is not the durable
  source of workspace state.
- `structure_events` is append-only, but records selected lifecycle and audit
  operations only. It is not a complete replay source. See
  [Event Log](../architecture/event-log.md).

## Hierarchy and Communication

`workspaces.parent_id` defines team hierarchy and feeds peer discovery and
`CanCommunicate()` authorization. Create or reparent workspace rows to change
the hierarchy. Canvas `collapsed` state only hides or shows existing
descendants; the old destructive `/expand` and `/collapse` routes are retired.
See [Communication Rules](../api-protocol/communication-rules.md) and
[Platform API — Team hierarchy](../api-protocol/platform-api.md#team-hierarchy).

## Provisioning Boundary

Core owns workspace lifecycle and selects the configured backend through the
dispatchers in `workspace-server/internal/handlers/workspace_dispatchers.go`.
Tier controls workspace capability and isolation; it does not select Docker
versus the control-plane backend. Runtime implementation and configuration are
owned by the maintained workspace-runtime and template repositories.

## Secrets and Bundles

Bundles must not serialize API keys, passwords, or credentials. Secret handlers
use AES-GCM when `SECRETS_ENCRYPTION_KEY` is configured and production boot
fails closed without it; non-production mode still permits explicit version-0
plaintext storage for local compatibility. Secrets are injected at the runtime
boundary. See [Bundle System](../agent-runtime/bundle-system.md) and [Secrets
Key Custody](../architecture/secrets-key-custody.md).

## HTTP Security Headers

The server-wide security-header middleware lives in
`workspace-server/internal/middleware/securityheaders.go`. Route middleware and
the router remain authoritative for authentication; headers are not an auth
substitute.

## Validation

When a rule here conflicts with implementation, verify against the router,
middleware, handler, migration, and event-type sources, then update this page
in the same change.

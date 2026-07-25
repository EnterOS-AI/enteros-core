# Tenant workspace-server API

This Go service is the tenant workspace-server, not the Molecule control plane.
It does not execute agent reasoning itself; it owns the tenant's workspace
state, coordination, auth, and backend lifecycle dispatch. Control-plane admin,
billing, membership, image-pin, and provider APIs live in the separate
`molecule-controlplane` repository.

## Responsibilities

- workspace lifecycle
- registry and heartbeats
- hierarchy-aware discovery
- A2A proxying for browser-initiated calls
- approvals and activity logs
- memory APIs
- secrets and global secrets
- files, templates, bundles, terminal, and viewport state
- WebSocket fanout to canvas clients and workspaces

## Caller Identification

Caller identification is endpoint-specific. Registry discovery requires an
explicit `X-Workspace-ID` and validates the credential contract for that
workspace. On `POST /workspaces/:id/a2a`, a workspace bearer is authoritative:
the platform derives its owner, and an optional `X-Workspace-ID` claim must
match. Verified human credentials are privileged and do not become workspace
identity merely by supplying that header.

The platform uses only the server-verified caller identity to enforce
hierarchy-based access rules.


## Breaking Changes

### Infrastructure PATCH authorization (2026-07-09)

`PATCH /workspaces/:id` distinguishes cosmetic self-maintenance from changes to
the infrastructure that contains an agent:

| Fields | Accepted credential |
|---|---|
| `name`, `role`, `x`, `y`, `collapsed` | Workspace bearer, org token, `ADMIN_TOKEN`, or verified control-plane session |
| `tier`, `parent_id`, `runtime`, `workspace_dir`, `compute` | `ADMIN_TOKEN` or verified control-plane session only |

An unauthorized infrastructure PATCH is rejected as a whole before validation
or database work with HTTP `403` and code
`WORKSPACE_INFRASTRUCTURE_AUTH_REQUIRED`. Callers must not retry it with a
workspace or org token. This prevents an agent from promoting itself to a
host-privileged tier or changing its runtime, host mount, topology, or compute
backend.

### PR #701 — Input validation, route auth, UUID safety (2026-04-17)

**Affects:** `PATCH /workspaces/:id`, `GET /workspaces/:id`, `DELETE /workspaces/:id`, `GET /templates`, `GET /org/templates`

| Change | Before | After |
|---|---|---|
| `PATCH /workspaces/:id` auth | Open router — no token required for cosmetic fields | `wsAuth` group — workspace bearer token required unconditionally |
| `GET /templates` auth | No auth | AdminAuth |
| `GET /org/templates` auth | No auth | AdminAuth |
| `:id` path parameter validation | DB query with raw string; Postgres error on non-UUID | `uuid.Parse` check before DB access — 400 `"invalid workspace id"` on non-UUID |

**Field validation added to `POST /workspaces` and `PATCH /workspaces/:id`:**

| Field | Max length | Additional constraints |
|---|---|---|
| `name` | 255 chars | No `\n`, `\r`, or YAML-special chars (`{}[]|>*&!`) |
| `role` | 1,000 chars | No `\n`, `\r`, or YAML-special chars |
| `model` | 100 chars | No `\n`, `\r` |
| `runtime` | 100 chars | No `\n`, `\r` |

Violations return `400 Bad Request` with `{ "error": "<field> must be at most N characters" }` or `{ "error": "<field> must not contain newline characters" }`.

**Migration steps for callers:**
1. Add `Authorization: Bearer <workspace-token>` to all `PATCH /workspaces/:id` requests.
2. Add an admin bearer token to `GET /templates` and `GET /org/templates` requests.
3. Ensure `:id` values in E2E scripts and automation are valid UUIDs. Update any test fixtures that use non-UUID IDs (see `workspace-server/internal/handlers/*_test.go` for updated examples).

## Core Endpoints

### Health and metrics

| Method | Path | Description |
|---|---|---|
| `GET` | `/health` | Health check |
| `GET` | `/metrics` | Prometheus metrics |

### Workspaces

| Method | Path | Description |
|---|---|---|
| `POST` | `/workspaces` | Create and provision a workspace |
| `GET` | `/workspaces` | List workspaces with inline canvas layout data |
| `GET` | `/workspaces/:id` | Get one workspace |
| `PATCH` | `/workspaces/:id` | Update workspace fields. **Requires `WorkspaceAuth`.** Workspace bearers are limited to cosmetic fields; infrastructure fields require `ADMIN_TOKEN` or a verified control-plane session. Validates `name` (≤255), `role` (≤1000), `model`/`runtime` (≤100 chars); `name` and `role` reject newlines and YAML-special chars (`{}[]|>*&!`). `:id` must be a valid UUID. See [Breaking Changes](#breaking-changes). |
| `DELETE` | `/workspaces/:id` | Remove workspace |
| `POST` | `/workspaces/:id/restart` | Restart workspace through the configured backend using the runtime persisted in Postgres. |
| `POST` | `/workspaces/:id/pause` | Pause workspace |
| `POST` | `/workspaces/:id/resume` | Resume workspace |
| `POST` | `/workspaces/:id/a2a` | Authenticated A2A proxy. Enforces caller binding/hierarchy and may return queued while a long dispatch continues. |
| `POST` | `/workspaces/:id/delegate` | Async delegation — fire-and-forget, returns delegation_id |
| `GET` | `/workspaces/:id/delegations` | List delegation status (pending/completed/failed) |
| `GET` | `/workspaces/:id/audit` | Read the workspace's stored HMAC-linked audit events. **Requires `WorkspaceAuth`.** |

### Audit ledger

`GET /workspaces/:id/audit` is offset-paginated. It accepts `agent_id`,
`session_id`, RFC 3339 `from`/`to` bounds, `limit` (default 100, maximum 500),
and `offset` (default 0). The response contract is:

```json
{
  "events": [
    {
      "id": "audit-event-id",
      "timestamp": "2026-04-17T12:00:00Z",
      "agent_id": "agent-id",
      "session_id": "session-id",
      "operation": "tool_call",
      "input_hash": null,
      "output_hash": null,
      "model_used": "browser.open",
      "human_oversight_flag": false,
      "risk_flag": false,
      "prev_hmac": null,
      "hmac": "hex-encoded-hmac",
      "workspace_id": "workspace-uuid"
    }
  ],
  "total": 1,
  "chain_valid": true,
  "chain_verification": "verified"
}
```

Events use `timestamp`, `agent_id`, and `operation`; the endpoint does not emit
the structure-event fields `created_at`, `actor`, or `event_type`. The table
schema defines `task_start`, `llm_call`, `tool_call`, and `task_end` operations.
`chain_valid` is `null` when the server cannot compute a verdict, including
when `AUDIT_LEDGER_SALT` is unset or when `offset`, `session_id`, or `from`
could omit chain predecessors. Clients must distinguish that state from
`false`, which means verification detected a mismatch in a complete chain
prefix returned by the endpoint.

`chain_verification` splits the two very different reasons `chain_valid` can be
`null`, so a missing salt is never a silent no-audit:

| `chain_verification`          | `chain_valid` | meaning |
| ----------------------------- | ------------- | ------- |
| `verified`                    | `true`        | full chain re-verified, HMACs intact |
| `tampered`                    | `false`       | HMAC/link mismatch in a complete prefix (fail-closed) |
| `unavailable_partial_query`   | `null`        | salt IS set, but `offset`/`session_id`/`from` could omit predecessors (benign) |
| `disabled_no_salt`            | `null`        | `AUDIT_LEDGER_SALT` is unset: verification is OFF and the ledger is **not tamper-evident** — a misconfiguration, also logged loudly server-side |

The append-only `activity_logs`/`structure_events` stream is the immutable
source of truth; the folded `delegations` row is a materialized view whose
lifecycle transitions are forward-only (a terminal state is a sink — see
`delegation_ledger.go`), so no state transition can silently rewrite history.
Successful empty responses return `"events": []`. Canvas deliberately does not
render the hash and HMAC integrity values returned in each event.

This endpoint is a read surface; it does not create audit events. The active
runtime currently has no `audit_events` producer. Restoring runtime emission or
retiring this orphaned surface requires a separate product decision; the read
endpoint alone is not evidence that current agent actions are being recorded.

### Async Delegation

`POST /workspaces/:id/delegate` sends a task to another workspace without blocking. The platform runs the A2A request in a background goroutine and returns immediately.

```json
POST /workspaces/:id/delegate
{"target_id": "<workspace-uuid>", "task": "Review the PLAN.md"}

→ 202 {"delegation_id": "...", "status": "delegated", "target_id": "..."}
```

Poll `GET /workspaces/:id/delegations` to check results. Each entry includes `delegation_id`, `status` (pending/completed/failed), and `response_preview`. WebSocket events `DELEGATION_COMPLETE` and `DELEGATION_FAILED` are broadcast on completion.

This is the recommended way for agents to delegate work across runtime
adapters because it operates at the authenticated platform boundary.

### Registry

| Method | Path | Description | Auth |
|---|---|---|---|
| `POST` | `/registry/register` | Register URL, card, runtime state, and delivery mode. A bootstrap row with no live instance credential may receive its one-time token; an enrolled row must prove ownership. | Bootstrap credential or the workspace's live bearer, according to row state. |
| `POST` | `/registry/heartbeat` | Refresh the 180-second liveness marker and latest health/task snapshot. | Workspace bearer once an instance token exists; legacy/bootstrap rows are migrated by registration. |
| `POST` | `/registry/update-card` | Push Agent Card updates after runtime/skill changes. | Same workspace-credential contract as heartbeat. |
| `GET` | `/registry/discover/:id` | Resolve an authorized peer's current URL. | Caller identity plus admin/org/workspace bearer or verified session. Auth-datastore errors fail closed. |
| `GET` | `/registry/:id/peers` | List peers reachable by the workspace. | Same credential contract as discovery. |
| `POST` | `/registry/check-access` | Evaluate hierarchy reachability for the supplied pair. | Handler-level validation; see current implementation. |

**Why the auth callout matters:** the registration bootstrap is intentionally
narrow and state-dependent. Do not generalize it into anonymous access for an
already-enrolled workspace. If these routes change, update their handler tests
and the registration/heartbeat E2E in the same PR.

### Activity and recall

| Method | Path | Description |
|---|---|---|
| `GET` | `/workspaces/:id/activity` | List activity rows (`?type=`, `?source=canvas\|agent`, `?limit=`) |
| `POST` | `/workspaces/:id/activity` | Report activity from a workspace |
| `POST` | `/workspaces/:id/notify` | Emit user-facing notifications/activity |
| `GET` | `/workspaces/:id/session-search` | Search recent activity + memory for recall |

### Memory

There are two distinct memory surfaces:

#### Scoped agent memory

| Method | Path | Description |
|---|---|---|
| `POST` | `/workspaces/:id/memories` | Commit a `LOCAL` / `TEAM` / `GLOBAL` memory |
| `GET` | `/workspaces/:id/memories` | Search scoped memories |
| `DELETE` | `/workspaces/:id/memories/:memoryId` | Delete an owned memory |

#### Key/value workspace memory

| Method | Path | Description |
|---|---|---|
| `GET` | `/workspaces/:id/memory` | List key/value memory entries |
| `GET` | `/workspaces/:id/memory/:key` | Get one key/value entry |
| `POST` | `/workspaces/:id/memory` | Upsert a key/value entry with optional TTL |
| `DELETE` | `/workspaces/:id/memory/:key` | Delete a key/value entry |

### Secrets

#### Workspace secrets

| Method | Path | Description |
|---|---|---|
| `GET` | `/workspaces/:id/secrets` | Return merged workspace + inherited global secret metadata |
| `POST` | `/workspaces/:id/secrets` | Upsert workspace secret |
| `PUT` | `/workspaces/:id/secrets` | Upsert workspace secret |
| `DELETE` | `/workspaces/:id/secrets/:key` | Delete workspace secret |
| `GET` | `/workspaces/:id/model` | Get workspace model override |

Important detail: `GET /workspaces/:id/secrets` does **not** return values. It returns key metadata plus a `scope` field so the frontend can distinguish inherited globals from workspace overrides.

#### Global secrets

| Method | Path | Description |
|---|---|---|
| `GET` | `/settings/secrets` | List global secret metadata |
| `POST` | `/settings/secrets` | Upsert global secret |
| `PUT` | `/settings/secrets` | Upsert global secret |
| `DELETE` | `/settings/secrets/:key` | Delete global secret |

Backward-compatible admin aliases also exist under `/admin/secrets`.

### Approvals

| Method | Path | Description |
|---|---|---|
| `GET` | `/approvals/pending` | List pending approvals |
| `POST` | `/workspaces/:id/approvals` | Create approval request |
| `GET` | `/workspaces/:id/approvals` | List approvals for a workspace |
| `POST` | `/workspaces/:id/approvals/:approvalId/decide` | Approve or deny |
| `POST` | `/workspaces/:id/approvals/:approvalId/withdraw` | Requester pulls back a pending approval (issue #66). Authz is against the row's creator workspace, not the path `:id`, so it works correctly under cross-workspace approval gates (#2574 / #2593). |

### Team hierarchy

Teams are workspace rows connected by `parent_id`. Create or reparent rows
through the authenticated workspace and org-import surfaces. The destructive
`POST /workspaces/:id/expand` and `/collapse` routes are retired. Canvas visual
collapse is a presentational `PATCH /workspaces/:id` layout update; it does not
provision or delete child workspaces.

### Plugins

| Method | Path | Description |
|---|---|---|
| `GET` | `/plugins` | List available plugins; accepts `?runtime=<name>` to filter to compatible plugins |
| `GET` | `/plugins/sources` | List registered install-source schemes (currently `gitea`, `github`, and `local` in the standard server) |
| `GET` | `/workspaces/:id/plugins` | List installed plugins (each includes `supported_on_runtime: bool`) |
| `GET` | `/workspaces/:id/plugins/available` | Plugins filtered to those compatible with the workspace runtime |
| `GET` | `/workspaces/:id/plugins/compatibility?runtime=X` | Preflight runtime change — which installed plugins would become inert |
| `POST` | `/workspaces/:id/plugins` | Install plugin `{"source":"<scheme>://<spec>"}` — e.g. `local://ecc` or `gitea://owner/repo#<commit>`. Auto-restarts workspace. |
| `DELETE` | `/workspaces/:id/plugins/:name` | Uninstall plugin — removes from container, auto-restarts |

Plugins are installed per-workspace into `/configs/plugins/<name>/`. The
standard source registry ships `local`, authenticated `gitea`, and
anonymous-by-default `github` schemes;
do not advertise an unregistered scheme as planned behavior. See
[`docs/plugins/sources.md`](../plugins/sources.md) for the two-axis source/shape
model.

Install safeguards bound the cost of a single install (env-tunable via `PLUGIN_INSTALL_BODY_MAX_BYTES` / `PLUGIN_INSTALL_FETCH_TIMEOUT` / `PLUGIN_INSTALL_MAX_DIR_BYTES`).

### Files and templates

| Method | Path | Description |
|---|---|---|
| `GET` | `/templates` | List available templates. **Requires AdminAuth** (PR #701). |
| `GET` | `/org/templates` | List available org templates. **Requires AdminAuth** (PR #701). |
| `POST` | `/templates/import` | Import an agent folder as a new template |
| `GET` | `/workspaces/:id/files` | List files under an allowed root |
| `GET` | `/workspaces/:id/files/*path` | Read a file |
| `PUT` | `/workspaces/:id/files/*path` | Write a file |
| `PUT` | `/workspaces/:id/files` | Replace workspace file set |
| `DELETE` | `/workspaces/:id/files/*path` | Delete a file |

Query parameters for `GET /workspaces/:id/files`:

| Param | Default | Description |
|-------|---------|-------------|
| `root` | `/configs` | Base path — one of `/configs`, `/workspace`, `/home`, `/plugins` |
| `path` | `""` | Subdirectory relative to root (validated against path traversal) |
| `depth` | `1` | Max recursion depth (1–5). Use with `path` for lazy-loading subdirectories |

Invalid `depth` or traversal paths return 400.

### Terminal

| Protocol | Path | Description |
|---|---|---|
| `WS` | `/workspaces/:id/terminal` | Terminal session into the running container |

### Bundles

| Method | Path | Description |
|---|---|---|
| `GET` | `/bundles/export/:id` | Export workspace tree as a bundle |
| `POST` | `/bundles/import` | Import a bundle |

### Canvas viewport and events

| Method | Path | Description |
|---|---|---|
| `GET` | `/canvas/viewport` | Get saved canvas pan/zoom |
| `PUT` | `/canvas/viewport` | Save canvas pan/zoom |
| `GET` | `/events` | List structure events |
| `GET` | `/events/:workspaceId` | List workspace-scoped events |

### WebSocket

| Protocol | Path | Description |
|---|---|---|
| `WS` | `/ws` | Live events. Canvas requires a verified tenant session, org token, or `ADMIN_TOKEN`; workspaces require an ID-bound bearer. |

Authenticated Canvas clients receive the global event stream using a verified
control-plane session, org token, or admin bearer. Browser clients can offer a
credential-bearing `molecule-auth.<hex>` protocol together with the non-secret
`molecule-ws` sentinel; the server authenticates with the former and selects
only the latter, so it never reflects the credential. Workspaces connect with
`X-Workspace-ID` plus a bearer bound to that workspace and receive filtered
events based on communication rules. `Origin` is checked for browser safety but
is never accepted as authentication. Anonymous upgrades are rejected.

## A2A Proxy Behavior

`POST /workspaces/:id/a2a` is more than a naive forwarder.

It currently:

- authenticates every public HTTP request before dispatch; workspace identity
  is derived from the bearer and an optional `X-Workspace-ID` must match it
- accepts SaaS human traffic only through a verified control-plane session,
  `ADMIN_TOKEN`, or org token; authenticated external inbound traffic uses the
  target-bound inbound secret
- preserves a no-bearer same-origin Canvas fallback only for combined
  self-host/dev deployments where control-plane session verification is
  unconfigured; SaaS never trusts same-origin headers as authentication
- rejects tokenless legacy, invalid/revoked bearer, forged-self, and auth
  datastore-error requests instead of treating them as Canvas traffic
- enforces `CanCommunicate` and same-org isolation for authenticated
  workspace-to-workspace calls; verified human, self, and trusted in-process
  system callers (`webhook:*`, `system:*`, `test:*`) bypass hierarchy
- normalizes incoming JSON into JSON-RPC 2.0
- injects `messageId` when missing
- applies different timeout rules for browser-initiated vs workspace-initiated calls
- logs the resulting A2A activity
- broadcasts successful browser-initiated responses back to the canvas as `A2A_RESPONSE`
- triggers restart flow when the target container is confirmed dead

That is why the chat UX no longer depends on polling as the primary response path.

## Environment Variables

```bash
DATABASE_URL=postgres://dev:dev@postgres:5432/molecule?sslmode=prefer
REDIS_URL=redis://redis:6379
PORT=8080
SECRETS_ENCRYPTION_KEY=...
ACTIVITY_RETENTION_DAYS=7
ACTIVITY_CLEANUP_INTERVAL_HOURS=6
CORS_ORIGINS=http://localhost:3000,http://localhost:3001
RATE_LIMIT=600
```

## Related Docs

- [Registry & Heartbeat](./registry-and-heartbeat.md)
- [Communication Rules](./communication-rules.md)
- [Workspace Runtime](../agent-runtime/workspace-runtime.md)
- [Canvas UI](../frontend/canvas.md)

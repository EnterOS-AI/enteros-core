# API Reference

This document describes the REST API exposed by the Molecule AI workspace server (Go/Gin, default port `:8080`). Clients include the Canvas frontend, workspace agents communicating over A2A, and external tooling such as the MCP server and CLI.

**Base URL:** `http://localhost:8080` (development default)
**Rate limit:** 600 req/min (configurable via `RATE_LIMIT`)
**CORS origins:** `http://localhost:3000,http://localhost:3001` by default (configurable via `CORS_ORIGINS`)

---

## Authentication

Three middleware classes gate server-side routes:

- **`AdminAuth`** — fail-closed in every environment. Accepts a verified control-plane session or a valid org/admin credential (with the documented deprecated workspace-token fallback only when `ADMIN_TOKEN` is unset). Auth datastore failures return `503`; there is no zero-token bootstrap bypass.
- **`WorkspaceAuth`** — fail-closed. Accepts a verified control-plane session, an org/admin credential, or a live bearer bound to the target workspace `:id`; a per-workspace token for workspace A cannot access workspace B.
- **`CanvasOrBearer`** — accepts a valid bearer or, when the combined canvas proxy is active, a same-origin `Referer`/`Host` or exact `Origin`/`Host` match. It never uses the configured `CORS_ORIGINS` allowlist as authentication. Use it only for cosmetic routes with zero data/security impact (currently `PUT /canvas/viewport`).

Full contract: `docs/runbooks/admin-auth.md`.

---

## Routes

| Method | Path | Handler |
|--------|------|---------|
| GET | /health | inline |
| GET | /metrics | metrics.Handler() — Prometheus text format; no auth, scrape-safe |
| POST/GET/PATCH/DELETE | /workspaces[/:id] | workspace.go — `GET /workspaces`, `POST /workspaces`, and `DELETE /workspaces/:id` require `AdminAuth`. `DELETE /workspaces/:id` also requires `X-Confirm-Name: <workspace name>`; cascading deletes still require `?confirm=true`. `PATCH /workspaces/:id` always requires `WorkspaceAuth`, then applies field-level authorization: workspace bearers may update cosmetic fields (`name`, `role`, `x`, `y`, `collapsed`); infrastructure fields (`tier`, `parent_id`, `runtime`, `workspace_dir`, `compute`) require `ADMIN_TOKEN` or a verified control-plane session. Org and workspace tokens receive `403 WORKSPACE_INFRASTRUCTURE_AUTH_REQUIRED` for those fields. |
| GET/PATCH | /workspaces/:id/config | workspace.go |
| GET/POST | /workspaces/:id/memory | workspace.go |
| DELETE | /workspaces/:id/memory/:key | workspace.go |
| POST/PATCH/DELETE | /workspaces/:id/agent | agent.go |
| POST | /workspaces/:id/agent/move | agent.go |
| GET/POST/PUT | /workspaces/:id/secrets | secrets.go (POST/PUT auto-restarts workspace) |
| DELETE | /workspaces/:id/secrets/:key | secrets.go (DELETE auto-restarts workspace) |
| GET | /workspaces/:id/model | secrets.go |
| GET | /settings/secrets | secrets.go — list global secrets (keys only, values masked) |
| PUT/POST | /settings/secrets | secrets.go — set a global secret `{key, value}`; auto-restarts every non-paused/non-removed/non-external workspace that does not shadow the key with a workspace-level override |
| DELETE | /settings/secrets/:key | secrets.go — delete a global secret; same auto-restart fan-out as PUT/POST |
| POST | /admin/workspaces/:id/tokens | admin_workspace_tokens.go — mint a real workspace bearer token; requires `AdminAuth`; plaintext is returned once |
| GET | /admin/cutovers/native-channels/inventory | native_channel_cutover_inventory.go — temporary `AdminAuth`-only, count-only evidence for the native-channel-to-plugin cutover; see `docs/runbooks/native-channel-cutover-inventory.md`; removed by molecule-core#4267 after evidence acceptance |
| GET/POST/DELETE | /admin/secrets[/:key] | secrets.go — legacy aliases for /settings/secrets |
| WS | /workspaces/:id/terminal | terminal.go |
| POST/GET | /workspaces/:id/approvals | approvals.go |
| POST | /workspaces/:id/approvals/:id/decide | approvals.go |
| POST | /workspaces/:id/approvals/:id/withdraw | approvals.go — requester pulls back a pending approval (#66) |
| GET | /approvals/pending | approvals.go |
| POST/GET | /workspaces/:id/memories | memories.go |
| DELETE | /workspaces/:id/memories/:id | memories.go |
| GET | /workspaces/:id/traces | traces.go |
| GET/POST | /workspaces/:id/activity | activity.go |
| POST | /workspaces/:id/notify | activity.go (agent→user push message via WebSocket) |
| POST | /workspaces/:id/restart | workspace.go |
| POST | /workspaces/:id/pause | workspace.go (stops container, status→paused) |
| POST | /workspaces/:id/resume | workspace.go (re-provisions paused workspace) |
| POST | /workspaces/:id/a2a | a2a_proxy.go — requires a source-bound workspace bearer or verified human/inbound credential; optional `X-Workspace-ID` must match bearer ownership. Combined self-host/dev Canvas requests may use same-origin only when CP session verification is unconfigured. |
| GET | /workspaces/:id/a2a/queue/:queue_id | a2a_queue_status.go — same authentication as A2A send, followed by queue sender/target ownership checks |
| POST | /workspaces/:id/delegate | delegation.go (async fire-and-forget) |
| GET | /workspaces/:id/delegations | delegation.go (list delegation status) |
| GET/POST | /workspaces/:id/schedules | schedules.go (cron CRUD). When the workspace advertises the `scheduler` capability (it runs the `kind: trigger` scheduler plugin), these routes proxy to the runtime's volume-backed `/internal/schedules*` API (schedules_proxy.go) — the volume grid is the source of truth; otherwise they serve the legacy `workspace_schedules` table (kept until P4b). `SCHEDULE_VOLUME_PROXY_DISABLED=1` forces the legacy path. |
| PATCH/DELETE | /workspaces/:id/schedules/:scheduleId | schedules.go — same volume-proxy/legacy split as above; in the volume model `:scheduleId` is the schedule *name* |
| POST | /workspaces/:id/schedules/:scheduleId/run | schedules.go (manual trigger) — volume path pokes the trigger daemon, which fires an autonomous `self-scheduler` turn (response carries `fired_by:"daemon"`); legacy path as before |
| GET | /workspaces/:id/schedules/:scheduleId/history | schedules.go (past runs) — still reads core `activity_logs` `cron_run` rows; the volume re-point (runtime history file) is pending. Daemon fires on a plugin workspace do not write `activity_logs`, so use the runtime's history surface for those. |
| GET | /workspaces/:id/schedules/health | schedules.go — peer-readable schedule health (issue #249); still reads the legacy table (volume re-point pending) |
| POST | /admin/workspaces/:id/schedules/migrate-to-volume | schedules_proxy.go — `AdminAuth`; copies the workspace's `source='runtime'` schedule rows from the DB to its volume grid, idempotent; see `docs/guides/selfhost-schedule-migration.md` |
| POST | /admin/schedules/backfill-plugin | scheduler_plugin.go — `AdminAuth`; declares the `molecule-scheduler` trigger plugin for every workspace with `workspace_schedules` rows; **dry-run by default**, `?apply=true` to declare + arm; see `docs/runbooks/scheduler-plugin.md` |
| GET/POST | /workspaces/:id/channels | channels.go (social channel CRUD) |
| PATCH/DELETE | /workspaces/:id/channels/:channelId | channels.go |
| POST | /workspaces/:id/channels/:channelId/send | channels.go (outbound message) |
| POST | /workspaces/:id/channels/:channelId/test | channels.go (test connection) |
| GET | /channels/adapters | channels.go (list available platforms) |
| POST | /channels/discover | channels.go (auto-detect chats for a bot token) |
| POST | /webhooks/:type | channels.go (incoming social webhook) |
| GET/PUT/DELETE | /workspaces/:id/files[/*path] | templates.go |
| GET | /canvas/viewport | viewport.go — open, no auth required (cosmetic, bootstrap-friendly) |
| PUT | /canvas/viewport | viewport.go — `CanvasOrBearer`; accepts a valid bearer or, on the combined canvas proxy, a same-origin `Referer`/`Host` or exact `Origin`/`Host` match. It does not authenticate from the `CORS_ORIGINS` allowlist. Cosmetic-only route — worst case viewport corruption, recovered by page refresh. |
| GET | /templates | templates.go |
| POST | /templates/import | templates.go — `AdminAuth` required |
| POST | /registry/register | registry.go |
| POST | /registry/heartbeat | registry.go — requires `Authorization: Bearer <token>` once a workspace has any live token on file (legacy workspaces grandfathered) |
| POST | /registry/update-card | registry.go — requires `Authorization: Bearer <token>` once a workspace has any live token on file |
| GET | /registry/discover/:id | discovery.go — requires `X-Workspace-ID`; token-enrolled workspace callers present their own bearer, while admin/org/session credentials are also accepted; auth datastore errors fail closed |
| GET | /registry/:id/peers | discovery.go — path `:id` identifies the caller; same credential contract as discovery, with no `X-Workspace-ID` required |
| POST | /registry/check-access | discovery.go |
| GET | /plugins | plugins.go (list registry; supports `?runtime=` filter) |
| GET | /plugins/sources | plugins.go (list registered install-source schemes) |
| GET/POST/DELETE | /workspaces/:id/plugins[/:name] | plugins.go — list, install (`{"source":"scheme://spec"}`), uninstall per-workspace |
| GET | /workspaces/:id/plugins/available | plugins.go (filtered by workspace runtime) |
| GET | /workspaces/:id/plugins/compatibility?runtime=X | plugins.go (preflight runtime-change check) |
| GET/POST | /workspaces/:id/tokens | tokens.go — list active tokens (prefix + metadata), create new token (plaintext returned once). Max 50 per workspace. |
| DELETE | /workspaces/:id/tokens/:tokenId | tokens.go — revoke specific token by ID |
| GET | /bundles/export/:id | bundle.go — `AdminAuth` required |
| POST | /bundles/import | bundle.go — `AdminAuth` required |
| GET | /org/templates | org.go (list available org templates) |
| POST | /org/import | org.go — `AdminAuth` required; applies `resolveInsideRoot` path sanitiser on template paths |
| GET | /events | events.go — `AdminAuth` required |
| GET | /events/:workspaceId | events.go — `AdminAuth` required |
| GET | /admin/liveness | inline — `AdminAuth` required. Returns per-subsystem `supervised.Snapshot()` ages; use to check health of the background worker goroutines (health-sweep, orphan-sweeper, liveness-monitor, …). The core scheduler loop is no longer among them — it was retired in core#4399; schedule firing is per-workspace (see `docs/runbooks/scheduler-plugin.md`) |
| GET | /ws | socket.go — Canvas requires a verified tenant session, org token, or `ADMIN_TOKEN`; workspace subscribers require `X-Workspace-ID` plus a bearer bound to that workspace |

---

## Database

Migration files live in `workspace-server/migrations/` (timestamp-named; check the directory for the latest). Each migration ships as a `.up.sql`/`.down.sql` pair. The migration runner globs `*.sql`, filters out `.down.sql` files, sorts alphabetically, and executes each file on boot. All `.up.sql` files must be idempotent (`CREATE TABLE IF NOT EXISTS`, `ALTER TABLE ... IF NOT EXISTS`) because the runner re-applies every migration on every boot.

### Key Tables

| Table | Description |
|-------|-------------|
| `workspaces` | Core entity — status, runtime, `agent_card` JSONB, heartbeat columns, `current_task`, `workspace_dir` |
| `canvas_layouts` | Per-workspace x/y canvas position |
| `structure_events` | Append-only event log (workspace lifecycle, agent, approval events) |
| `activity_logs` | A2A communications, task updates, agent logs, errors. Historical `cron_run` rows (with `error_detail`) were written by the retired core scheduler loop and still back the legacy History route; the per-workspace trigger daemon writes its run log to the workspace volume instead. |
| `workspace_schedules` | **Legacy** cron store — expression, timezone, prompt, run bookkeeping, `source` (`'template'` for org/import-seeded, `'runtime'` for Canvas/API-created). For a workspace running the scheduler trigger plugin the volume grid is the source of truth and this table is bypassed by CRUD; it still serves non-plugin workspaces, template seeding, History/Health, admin health, and the webhook poke. Retired in P4b (issue #4411 item 5). |
| `workspace_channels` | Social channel integrations (Telegram, Slack, etc.) with JSONB config and allowlist |
| `agents` | Agent records |
| `workspace_secrets` | Per-workspace encrypted secrets |
| `global_secrets` | Platform-wide encrypted secrets |
| `workspace_auth_tokens` | Bearer tokens; auto-revoked on workspace delete |
| `agent_memories` | HMA scoped memory (LOCAL / TEAM / GLOBAL) |
| `approvals` | Human-in-the-loop approval requests |

# Self-host guide: migrating schedules from the DB to the workspace volume

The scheduler moved out of core: firing is owned by a per-workspace
`kind: trigger` plugin, and for a workspace running that plugin the schedule
grid lives on its **persisted volume** (`<configs>/schedules/schedules.yaml`),
not in the `workspace_schedules` Postgres table. See
`docs/design/rfc-scheduler-as-trigger-plugin.md` (Option A) and
`docs/runbooks/scheduler-plugin.md`. This guide covers the one-time
**DB → volume data move** for self-hosters with pre-existing schedule rows.

## Preconditions

- The workspace runs a runtime image that carries the trigger-plugin
  boot-install path, has the `molecule-scheduler` plugin declared/installed,
  and its heartbeat advertises the `scheduler` capability. The migration
  endpoint refuses (409) otherwise: there is no volume backend to migrate to.
- You can call core with `AdminAuth` (see `docs/runbooks/admin-auth.md`).

## What the migration does

```
POST /admin/workspaces/:id/schedules/migrate-to-volume    # AdminAuth
```

`MigrateToVolume` (`workspace-server/internal/handlers/schedules_proxy.go`):

1. Rejects with 409 if the workspace does not advertise a native scheduler.
2. Lists the workspace's current volume grid via the runtime's
   `/internal/schedules` API.
3. Copies each `source='runtime'` row from `workspace_schedules`
   (`name`, `cron_expr`→`cron`, `timezone`, `prompt`, `enabled`) into the
   volume grid through the same API.
4. Skips any entry whose name already exists on the volume — **idempotent**:
   re-running (or running before every workspace is cut over) never
   double-writes or errors.
5. Returns `{"workspace_id", "migrated", "skipped", "failed"}` counts.

It is a **copy, not a move**: the DB rows are left in place (see Rollback).

## What it skips — and why

`source='template'` rows are **not** copied. Rationale in code: template
schedules are supposed to be re-seeded on the volume by the template reconcile
channel, so copying them here would duplicate. **Be aware that channel is not
fully built yet**: core still seeds a template's `config.yaml` `schedules:`
block into the legacy DB only, and the runtime's reconcile-on-boot seeding
(runtime#303) covers only a trigger plugin's own shipped `schedules.yaml` —
the template-`config.yaml`→volume re-seed is an open design seam (scheduler
RFC P3 remainders; issue #4411). **What to do today:** if a template-source
schedule must keep firing on a volume-backed workspace now, re-create it
through Canvas or `POST /workspaces/:id/schedules` — the volume path stores it
with `source='runtime'` and the daemon fires it. Otherwise wait for the
seeding seam to close.

## Verifying after migration

- **Grid:** `GET /workspaces/:id/schedules` (Canvas List) now serves the
  volume grid for this workspace — confirm the migrated names appear. On disk:
  `<configs>/schedules/schedules.yaml`.
- **Health:** the runtime's `GET /internal/schedules/health`
  (platform-inbound auth) — or `<configs>/schedules/schedule-health.json` —
  shows `last_tick` advancing and your schedules armed.
- **History (fires):** core's Canvas-facing History and Health routes are
  **volume-proxied** — they forward to the runtime and return the same window
  the daemon writes (`scheduleHistoryLimit = 20`, mirroring the legacy query).
  The runtime surfaces (`GET /internal/schedules/history` / `.../health`) and
  the volume files `<configs>/schedules/schedule-history.json` and
  `schedule-health.json` are the same data, read directly.

## Rollback posture

> **This section changed. P4b has landed.** Earlier revisions of this guide told
> operators to set `SCHEDULE_VOLUME_PROXY_DISABLED=1` as the incident rollback.
> **That variable no longer exists anywhere in core** — setting it today does
> nothing, silently, which is the worst possible behaviour during an incident.
> There is no dual-path kill-switch to fall back to.

The legacy dual-path core-DB schedule backend was **retired in P4b**. Core no
longer stores or fires schedules, and nothing in core reads `workspace_schedules`
any more — the grid on the workspace's persisted volume is the only copy that
exists, and the workspace's `kind: trigger` scheduler plugin is the only thing
that fires it.

Practically, that means:

- **There is no "switch back to the DB" lever.** Any DB rows left over from
  before the migration are inert: nothing reads them and nothing fires them.
  Restoring them would require re-introducing the retired backend, not flipping
  a flag.
- **Recovery is forward, at the workspace.** If a workspace's grid is wrong,
  fix it through Canvas or `POST /workspaces/:id/schedules` (which writes the
  volume through the proxy), or restore the workspace volume from a snapshot.
- **Do not hand-edit the volume grid.** The store validates on load and a
  corrupt grid is refused — you would take the workspace's scheduling down
  entirely rather than partially.
- **If the daemon is not firing**, the problem is almost never the grid. Check
  that the workspace advertises the `scheduler` capability in its heartbeat and
  that its pinned runtime image carries the plugin boot-install path — see
  `docs/runbooks/scheduler-plugin.md`.

The `MOLECULE_DECLARE_SCHEDULER_PLUGIN` flag documented in that runbook is a
**roll-out** lever (it stops *new* provisions declaring the plugin); it is not a
rollback for a workspace that has already migrated.

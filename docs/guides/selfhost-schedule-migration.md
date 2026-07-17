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
- **History (fires):** the runtime's `GET /internal/schedules/history` — or
  `<configs>/schedules/schedule-history.json` — records daemon fires. Note:
  core's Canvas-facing History/Health routes still read the legacy DB (their
  re-point is pending), so daemon fires will NOT appear there; check the
  runtime surface or the volume files.

## Rollback posture

Legacy DB reads linger until P4b (the `workspace_schedules` retirement —
issue #4411 item 5), and the migration never deletes DB rows, so rollback is
cheap:

- Set `SCHEDULE_VOLUME_PROXY_DISABLED=1` on core to force the legacy DB path
  for every workspace (the staged-cutover kill-switch in
  `schedules_proxy.go`) — CRUD immediately serves the untouched DB rows again.
  Note the DB rows will not reflect edits made on the volume in the interim,
  and with the core loop retired (core#4399) nothing fires DB-only rows — the
  kill-switch restores the *data* path, not a firing engine.
- Do **not** hand-edit the volume grid to roll back; the store validates on
  load and a corrupt grid is refused.

Keep the DB rows until P4b lands and your fleet is fully volume-backed.

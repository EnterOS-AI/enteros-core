# Scheduler trigger-plugin runbook

Operating the per-workspace scheduler after the core cron loop was retired
(core#4399). Scheduling is now a `kind: trigger` daemon plugin
(`molecule-scheduler`, sourced from the SDK native-plugins registry) that runs
**inside** each workspace runtime and fires schedules as autonomous
`self-scheduler` turns. Design: `docs/design/rfc-scheduler-as-trigger-plugin.md`.
Operational rollout tracker: [issue #4411](https://git.moleculesai.app/molecule-ai/molecule-core/issues/4411).

**Read this first:** merged code ≠ live fleet. A workspace only runs the
daemon once its **pinned runtime image** carries the plugin boot-install path
and the plugin is **declared** for it. Until the fleet rollout below completes,
scheduled turns on old-pin workspaces fire nowhere.

## How a workspace gets the scheduler

All in `workspace-server/internal/handlers/scheduler_plugin.go`:

1. **Declare** — as of core#4541 the scheduler is a **base per-workspace
   ability declared on every provision** (create/restart/resume), because the
   one `molecule-scheduler` plugin now ships both the firing daemon *and* the
   self-schedule MCP tool — and you need the tool to create the first schedule,
   so it can no longer wait for a schedule to exist. `ensureSchedulerPluginDeclared`
   upserts `molecule-scheduler` into `workspace_declared_plugins` (idempotent);
   schedule-create and template seeding still call the same upsert on-demand.
   Declared plugins install at boot via `MOLECULE_DECLARED_PLUGINS` or on the
   transition-to-online reconcile. The every-provision declare is guarded by a
   default-on kill-switch — see **Kill-switch: halt the roll-out** below.
2. **Arm (hot)** — core then best-effort forwards
   `POST /internal/daemons/reload` to the running workspace
   (`armSchedulerPlugin`; bearer = the per-workspace platform-inbound secret,
   8 s timeout, failure logged and swallowed — the reconcile-on-online path is
   the durable net). A runtime pin too old to expose the endpoint 404s and arms
   on its next restart instead.
3. **Advertise** — once the daemon is up, the workspace heartbeat advertises
   the `scheduler` capability; core's `ProvidesNativeScheduler` flips true and
   schedule CRUD proxies to the volume backend (`schedules_proxy.go`).

## Kill-switch: halt the roll-out (`MOLECULE_DECLARE_SCHEDULER_PLUGIN`)

The unconditional every-provision `ensureSchedulerPluginDeclared` (step 1 above,
in `workspace_provision_shared.go`) is guarded by one env flag on the
**workspace-server (core)** deployment:

| Env var | Default | Guards | Parsing |
| --- | --- | --- | --- |
| `MOLECULE_DECLARE_SCHEDULER_PLUGIN` | **ON** | the unconditional every-provision scheduler declare | unset / `""` / `1` / `true` / `yes` ⇒ **ON**; `0` / `false` / `no` (case-insensitive, trimmed) ⇒ **OFF** |

`declareSchedulerPluginEnabled()` (`scheduler_plugin.go`) reads it: default-on
means provisioning is **byte-identical to today** unless the owner explicitly
sets a falsey value.

**To emergency-halt the scheduler roll-out without a code revert:** set
`MOLECULE_DECLARE_SCHEDULER_PLUGIN=0` (or `false`/`no`) in the workspace-server
(core) deployment env and roll it. New provisions then declare no scheduler;
already-declared workspaces are unaffected (the flag gates the declare, not
un-declare). Flipping it back to ON (or removing it) resumes the roll-out.

> **Where to set it:** this is a **code-default lever only** — it is read
> straight from the process env and is **not yet registered in the Infisical /
> CP config SSOT**. Set it in the core deployment's env (the same place other
> core process env lives), not by adding an Infisical secret expecting it to
> flow through — until it is onboarded to the config SSOT, an Infisical entry
> would not reach the process.

**Sibling flag — note the inverted default.** The idle-digest default-native
declaration (`declareDefaultNativePlugins`, `plugin_registry.go`) has its own
switch `MOLECULE_DECLARE_DEFAULT_NATIVE_PLUGINS`, which is **default OFF** and
gates declaring the *other* `install: default` native plugins (the digest
providers). The scheduler needed a **symmetric but inverted** switch: the
scheduler roll-out is on-by-default and this flag exists to *halt* it, whereas
the digest flag is off-by-default and exists to *arm* it (fleet-rollout step 3).
`""`/`0`/`false`/`no` all read OFF for the digest flag; only a truthy value arms it.

## Ownership / SSOT: the scheduler is declared exactly once

The scheduler is declared **exactly once per provision**, by
`ensureSchedulerPluginDeclared`, under the const name
`SchedulerPluginName = "molecule-scheduler"`. It is deliberately **excluded**
from `declareDefaultNativePlugins`:

- Both paths run on every provision. `declareDefaultNativePlugins` seeds the
  `install: default` native set from the SDK native-plugins registry SSOT, but
  it seeds `defaultNativePluginSourcesForDeclare()` — which is the full
  `install: default` set **MINUS `SchedulerPluginSource`** (the filter lives in
  `plugin_registry.go`).
- Why the filter matters: `declareDefaultNativePlugins` derives each plugin's
  install name from its source via `PluginNameFromSource`, which for the
  scheduler source yields a **different** name, `molecule-ai-plugin-scheduler`,
  than the const `molecule-scheduler` the dedicated path declares. Declaring via
  both paths therefore produced **two `workspace_declared_plugins` rows for one
  plugin** (a divergent-name double-declare, and a duplicate boot-install), not
  a harmless idempotent no-op.
- The filter is applied in `defaultNativePluginSourcesForDeclare()`, **not** in
  `defaultNativePluginSources()` — so the registry-derived SSOT set stays
  untouched for every other consumer (e.g. the concierge-exclusion test).

Reference: core#4541.

## The scheduler owns its own configuration

`molecule-scheduler` is a `kind: trigger` plugin that declares its own settings in
its manifest — core does not model scheduler tuning, and there is no core-side
knob for it. Its `contributes.configuration` declares two keys:

| Key | Type | Manifest default | Effect |
| --- | --- | --- | --- |
| `poll_seconds` | integer | `30` | Grid scan interval. Interpolated into the daemon env as `MOLECULE_TRIGGER_POLL_SECONDS: "${config:poll_seconds}"`. The daemon clamps to a 1s floor and falls back to 30 on a missing/unparseable value. |
| `schedules` | array | `[]` | Schedules this install **seeds** into the workspace grid, reconciled with `upsert_template` semantics: additive, keyed on name, a runtime/API edit of the same name wins, a user-deleted schedule is tombstoned and never resurrected. The default is empty on purpose — this plugin ships the daemon, not preset schedules. |

Values reach the box as a delivered settings file, **not** as core config:

```
template config.yaml  plugins[].config
        ↓  core   renderPluginSettingsFiles   (plugin_settings_delivery.go)
/configs/plugin-settings/molecule-ai-plugin-scheduler.json
        ↓  runtime  plugin_settings.resolve   (manifest defaults <- delivered)
daemon env  MOLECULE_TRIGGER_POLL_SECONDS=…
```

> ### Operational trap: the settings file is named after the REPO
>
> The filename is the plugin's **install name**, derived from its source by
> `PluginNameFromSource` — for the scheduler that is
> **`molecule-ai-plugin-scheduler`**, the repo. It is *not* the manifest's own
> `name:`, which is `molecule-scheduler`. Both identities are live at once for
> this plugin: the daemon is owned by `molecule-scheduler`, while its settings
> arrive as `molecule-ai-plugin-scheduler.json`.
>
> When debugging "my config did not take", look for
> `/configs/plugin-settings/molecule-ai-plugin-scheduler.json`. A file at
> `molecule-scheduler.json` is read by nothing and fails **silently** — the
> runtime treats a missing settings file as a clean no-op and the daemon simply
> runs on manifest defaults. (The plugin repo's own `plugin.yaml` comment
> currently names `molecule-scheduler.json` here; that comment is wrong, and the
> delivered filename is what the code actually produces.)

**Delivery is Create-path only.** Both `renderPluginSettingsFiles` call sites sit
inside `WorkspaceHandler.Create` — the local-template leg and the SaaS
fetched-bytes leg — and the rendered files ride the provision bundle. Editing a
template's `plugins[].config` therefore does not reach an existing workspace;
that workspace has to be provisioned again. A live-edit path (layer 6) is **not in
`main`** — the delivery module's own scope comment says so explicitly.

See `docs/plugins/authoring-configuration.md` for the declaration contract and
`docs/plugins/template-plugin-config.md` for setting the values.

## Health

- **Runtime surface (volume-backed, authoritative for plugin workspaces):**
  `GET /internal/schedules/health` on the workspace runtime
  (`molecule_runtime/internal_schedules.py`; same `platform_inbound`
  forward-auth as the other `/internal/*` routes). Returns the daemon's health
  file (`schedule-health.json`: `last_tick`, per-schedule errors); before the
  first tick it falls back to `{"last_tick": null, "armed": <grid count>, "errors": {}}`
  so the surface is never blank. Run history:
  `GET /internal/schedules/history` (or `/{name}/history`).
- **Core's Canvas-facing routes are volume-proxied** — `GET /workspaces/:id/schedules/health`
  and `GET /workspaces/:id/schedules/:scheduleId/history` forward to the runtime
  surface above (`healthVolume` / `historyVolume` in `schedules_proxy.go`) and
  return the same data the daemon writes. They do **not** read the legacy DB;
  an earlier revision of this runbook said the re-point was still pending, which
  is wrong — the proxy is unconditional and there is no DB arm behind it.
- **Admin cross-workspace surface:** `GET /admin/schedules/health` (`AdminAuth`,
  issue #618) reads the **volume** grid per workspace
  (`volumeAdminScheduleHealth`); its only SQL is a workspace-name lookup. A
  previous revision warned that plugin-fired workspaces would show
  `never_run`/`stale` here because the daemon does not write
  `workspace_schedules` — that caveat is obsolete, because this route no longer
  consults that table. If it reports a schedule stale, treat it as real and
  investigate rather than dismissing it as re-point drift.
- With container access you can always read the volume files directly:
  `<configs>/schedules/{schedules.yaml,schedule-health.json,schedule-history.json}`.

## Backfill — the route is GONE

> **`POST /admin/schedules/backfill-plugin` no longer exists.** The handler
> (`BackfillSchedulerPlugin`) has zero Go references and the path is absent from
> the engine's route table — a call returns **404**. The sibling one-shot lever
> `POST /admin/workspaces/:id/schedules/migrate-to-volume` (`MigrateToVolume`) is
> gone the same way. Both were P4b migration tooling for moving off the core DB,
> and they went out with the DB path itself.

There is no backfill lever to run. The scheduler is now declared **unconditionally
on every provision** (see *How a workspace gets the scheduler* above), so the
population those routes existed to remediate — workspaces with schedules but no
plugin — is refilled by provisioning rather than by an admin sweep. A workspace
that is not firing needs the fleet-rollout path (a pin that carries the plugin,
then a re-provision), not a backfill call.

## Hot-arm a single workspace

`POST /internal/daemons/reload` on the workspace runtime (bearer = that
workspace's platform-inbound secret; runtime#308). Idempotent: already-running
daemons are a warm no-op; a newly-installed trigger daemon is supervised
mid-process, no restart.

## Fleet rollout ordering (owner-gated)

Per issue #4411 — every step below is **owner-gated**; nothing here happens
automatically on merge:

1. **Runtime pins** — rebuild the runtime image carrying the plugin
   boot-install path + trigger scaffold, and promote the per-workspace pins
   (merge ≠ deploy for the runtime).
2. **Recreate** — re-provision/recreate workspaces onto the new pin
   (a soft restart reuses the old image).
3. **Declare-flag** — flip `MOLECULE_DECLARE_DEFAULT_NATIVE_PLUGINS` (core
   env, default-off, `plugin_registry.go`) so every provision declares the
   *other* `install: default` native plugins (the digest providers). Post
   core#4541 the **scheduler is no longer declared by this flag** — it is
   declared unconditionally on every provision (default-on, its own
   `MOLECULE_DECLARE_SCHEDULER_PLUGIN` kill-switch) and filtered out of this
   path to avoid a divergent-name double-declare (see **Ownership / SSOT**).
4. **Digest org-secret** — set the runtime-side digest loader flag
   `MOLECULE_DIGEST_PROVIDER_PLUGINS=1` for workspaces via the org/global
   secret env fan-out (flag-off is byte-identical; separate RFC, same rollout
   train).
5. ~~**Backfill apply**~~ — **no longer a step.** The backfill route has been
   deleted (see *Backfill — the route is GONE*); the every-provision declare
   replaced it.
6. **E2E flips** — set `E2E_SCHEDULER_CHECK=on` in
   `tests/e2e/ephemeral_cp_happy_path.sh` (activates autonomous-fire step 10d
   of `test_staging_full_saas.sh`) once the ephemeral runtime pin carries the
   plugin boot-install + trigger scaffold; likewise the digest-plugin check
   (step 10e). Default **off** so the gate cannot red on a capability the
   pinned image does not yet have.

## Related

- `docs/design/rfc-scheduler-as-trigger-plugin.md` — design + phase status
- `docs/guides/selfhost-schedule-migration.md` — DB→volume schedule migration
- `docs/runbooks/admin-auth.md` — what `AdminAuth` accepts

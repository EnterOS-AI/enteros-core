# Staged-retirement runbook — P4b (drop `workspace_schedules`) + D3 (delete the baked digest roster)

**Status:** planning artifact (reviewable deletion plan). The deletions themselves are **owner/CTO-gated** on the preconditions below and are **not** opened as PRs here — drafting them now would produce large, un-mergeable red-CI diffs against preconditions that are not yet met. The one **additive** precondition (P4b PR1, the direct-Create render leg) already landed as a sibling PR ([#4444](https://git.moleculesai.app/molecule-ai/molecule-core/pulls/4444), merged, main `76ad5eab`).

**Companions:**
- [`rfc-scheduler-as-trigger-plugin.md`](rfc-scheduler-as-trigger-plugin.md) — P4b is that RFC's §P4 store-retirement leg.
- [`rfc-digest-providers-as-native-plugins.md`](rfc-digest-providers-as-native-plugins.md) — D3 is that RFC's "retire baked" leg (G6).
- Rollout tracking: [issue #4411](https://git.moleculesai.app/molecule-ai/molecule-core/issues/4411) (scheduler operational rollout, owner-gated).

**How to read this doc:** each half is a step-by-step, precondition-gated deletion sequence. The ordered-PR tables are the executable unit — open each row's PR **only** when its "gated-on" column is fully satisfied. Every `file:line` anchor below was verified against main (`76ad5eab`) at authoring time.

---

## Part A — P4b: drop `workspace_schedules` (molecule-core)

The scheduler moved to a per-workspace trigger plugin; the volume grid is now authoritative and the fire loop (`internal/scheduler`) is already deleted (core#4399). P4b removes the last legacy leg: the core `workspace_schedules` DB table, its CRUD else-arms, its seed/webhook/health touchpoints, and finally the table itself. End state is **Option A** — volume-authoritative, zero core mirror.

### A.1 Preconditions (at-a-glance)

| # | Precondition | Met? | Evidence / gate |
|---|---|---|---|
| P1 | Fleet 100% native — 4 prod tenants restarted onto **runtime-0.4.13**, `GET /admin/schedules/health` shows **zero** legacy-DB entries | ❌ NOT MET | health handler still reports DB rows; fleet pins pending (#4411 item 1) |
| P2 | Backfill-plugin **dry-run → `?apply=true`** per tenant | ❌ NOT MET | owner/CTO-gated, #4411 step 2; route live at `POST /admin/schedules/backfill-plugin` (`router.go:574`) |
| P3 | Per-workspace **migrate-to-volume** for every `source='runtime'` workspace: `migrated + skipped == total`, `failed == 0` | ❌ NOT MET | route live at `POST /admin/workspaces/:id/schedules/migrate-to-volume` (`router.go:570`) |
| P4 | **core#4435** volume-side org-re-import inheritance path shipped + deployed | ❌ NOT MET — **HARD BLOCKER** | [issue #4435](https://git.moleculesai.app/molecule-ai/molecule-core/issues/4435) open |
| P5 | **runtime#318** `config.yaml` seed leg deployed fleet-wide | ❌ NOT MET | core leg landed (#4444); runtime seed leg + fleet deploy pending |

**No PR below may open until its row's gate is green.** The table-drop tail (PR8) additionally requires **≥1 release** of PR2–7 running in production so a rollback window exists (expand→contract).

### A.2 The single most important safety trap

> **Dropping `workspace_schedules` before core#4435 ships SILENTLY and IRREVERSIBLY loses user-created schedules on the next org re-import.**

Org re-import (`org_import.go`) re-seeds template schedules and, today, re-points runtime-created rows via `migrateRuntimeSchedulesFromRemovedPredecessor` (`org_import.go:992`, called at `org_import.go:682`). Until the **volume-side** inheritance path (#4435) exists, the volume grid has no equivalent "carry my `source='runtime'` schedules across a re-import" mechanism. If the DB table is dropped first, a re-import produces a workspace whose user schedules are simply **gone** — no error, no orphan row, nothing to reap. This is why P4 is a hard blocker gating **both** PR5 (which retires the DB migrate helper) and PR8 (the drop). Do not reorder around it.

### A.3 Ordered PR sequence

| PR | What it deletes / changes | Gated on |
|---|---|---|
| **PR1** | **(DONE — sibling #4444)** Additive: render template/org schedules directly into the delivered `config.yaml` on the direct-Create path. Nothing removed. | — (landed, main `76ad5eab`) |
| **PR2** | In `schedules.go`, remove the legacy DB-CRUD **else-arms** behind every `scheduleBackendIsVolume(...)` branch (the `FROM/INTO/UPDATE/DELETE workspace_schedules` blocks at `schedules.go:122,210,240,298,338,384,427,475,589`); make the volume proxy **unconditional**; delete `scheduleBackendIsVolume` (`schedules_proxy.go:48`) and the `SCHEDULE_VOLUME_PROXY_DISABLED` kill-switch (`schedules_proxy.go:39`, `scheduleProxyKillEnv`). | P1 |
| **PR3** | Drop the DB **seed** path: `orgImportScheduleSQL` (`org.go:195`), the `org_import.go` seeding loop (`org_import.go:714`), and `seedTemplateSchedules` (`template_schedules.go:126`). **Keep `parseTemplateSchedules`** (`template_schedules.go:79`) — the config.yaml render leg (PR1/#4444) still needs it. | after PR1; P1 + P5 |
| **PR4** | In `webhooks.go`, remove the legacy `UPDATE workspace_schedules SET next_run_at = now()` writes (`webhooks.go:444–447` and `489–492`). **Keep `pokeVolumeSchedules`** (`webhooks.go:353`) — it is the volume-native fire path. | P1 |
| **PR5** | Retire `admin_schedules_health.go` legacy DB legs — `Health` DB path (`admin_schedules_health.go:151`), `Orphans` (`:282`), `ReapOrphans` (`:322`) — and `migrateRuntimeSchedulesFromRemovedPredecessor` (`org_import.go:992` + call site `:682`). Keep the volume health path (`volumeAdminScheduleHealth`, `admin_schedules_health.go:68`). Unregister the retired admin routes (`router.go:718,719`). | **AFTER core#4435 (P4)** |
| **PR6** | Retire the ops routes `MigrateToVolume` (`schedules_proxy.go:471`) + `BackfillSchedulerPlugin` (`scheduler_plugin.go:152`) and their registrations (`router.go:570,574`). **Preserve `ensureSchedulerPluginDeclared`** (`scheduler_plugin.go:53`) — plugin-declare is still live. | after **P2 + P3 complete** (both backfills fleet-universal) |
| **PR7** | Remove the remaining `workspace_crud.go` touchpoints: the cascade-list entry (`workspace_crud.go:695`), the enabled-count read (`:747`), and the disable-on-archive UPDATE (`:822`). | after PR2–6 |
| **PR8** | The **drop migration**: `DROP INDEX IF EXISTS` (all four: `idx_schedules_workspace`, `idx_schedules_next_run`, `idx_schedules_workspace_name`) + `DROP TABLE IF EXISTS workspace_schedules`. `.down.sql` recreates the **full post-015/022/032/20260523 shape** — every column (incl. `source`, `consecutive_empty_runs`, `consecutive_sdk_errors`) + all indexes + the `workspace_schedules_source_check` constraint. Mirror the `20260520120000_drop_runtime_image_pins` precedent (`.up.sql`/`.down.sql`) **and** its content-shape test (`internal/db/migration_20260520_drop_runtime_image_pins_test.go`). | **AFTER PR2–7** AND **≥1 release** of reads-lingering for rollback (expand→contract) |
| **PR9** | Anti-regression guard in `.gitea/scripts/tests/test_no_retired_deployment_artifacts.py`: content-negative assertion that `workspace_schedules` and `orgImportScheduleSQL` appear in **zero** non-test Go files; plus a migration-shape check. **Each arm negative-controlled (RED-first)** before the deletions land. | ships alongside PR8 (guard the end state) |

### A.4 Table shape the `.down.sql` must restore (PR8)

The rollback must be byte-faithful to the accreted schema. Source of truth for the recreate:

- `015_workspace_schedules.sql` — base table (`id`, `workspace_id` FK `ON DELETE CASCADE`, `name`, `cron_expr`, `timezone`, `prompt`, `enabled`, `last_run_at`, `next_run_at`, `run_count`, `last_status`, `last_error`, `created_at`, `updated_at`) + `idx_schedules_workspace` + partial `idx_schedules_next_run WHERE enabled = true`.
- `022_workspace_schedules_source.up.sql` — `source TEXT NOT NULL DEFAULT 'runtime'`, `workspace_schedules_source_check CHECK (source IN ('template','runtime'))`, unique `idx_schedules_workspace_name (workspace_id, name)`.
- `032_schedule_consecutive_empty.up.sql` — `consecutive_empty_runs INTEGER NOT NULL DEFAULT 0`.
- `20260523000000_schedule_consecutive_sdk_errors.up.sql` — `consecutive_sdk_errors INTEGER NOT NULL DEFAULT 0`.

### A.5 Rollback posture (P4b)

Strict **expand→contract**. Reads linger in core for ≥1 release after PR2–7 so the drop (PR8) can be reverted by re-running the `.down.sql` (which reconstructs the exact table) without data-shape loss — but note this only recovers **schema**, not rows; rollback of *data* is why P4 (#4435) and the ≥1-release soak are non-negotiable before the table physically drops. PR2–7 are individually revertible (each is a code-only removal of an already-dead-in-prod path once P1 holds). PR8 is the point of no return for data; everything upstream of it is reversible.

### A.6 Governance & reserved paths

- **PR Diff Guard** hard-blocks any PR with **>5000 deletions** — PR8's `.down.sql` recreate is small, but if any single PR trips the limit, split it or request an owner **controlled BP-relax**.
- `migrations/` and `docs/design/` are **reserved paths** → each PR touching them draws a **distinct non-author reserved-path-review** (the reviewer must not be the PR author).

### A.7 Docs to update at completion

- `rfc-scheduler-as-trigger-plugin.md` — flip §P4b to **IMPLEMENTED**.
- `docs/design/testing-strategy.md` — record the migration-shape + content-negative guards.
- `TEMPLATE_ASSET_DELIVERY.md` — schedules now delivered via `config.yaml` render, not DB seed.

---

## Part B — D3: delete the baked digest roster (molecule-ai-workspace-runtime)

The idle digest moved from a hardcoded provider roster inside the runtime wheel to **native digest-provider plugins** discovered at load time. D1 (loader) and D2 (registry + 4 plugin repos) have landed. D3 deletes the now-dead baked source: `molecule_runtime/idle_digest/providers/` and the hardcoded list in `controller.build_default_providers()`, leaving discovery-only assembly.

### B.1 Preconditions (at-a-glance)

| # | Precondition | Met? | Evidence / gate |
|---|---|---|---|
| D1 | Loader + trust gate live on core main | ✅ MET | armed ephemeral sub-step `10e` green (runtime#309/#310, core#4416) |
| D2 | The 4 `molecule-ai-plugin-digest-{mail,goal,identity,task-queue}` repos cut **v0.2.x** that **OWN** the provider source (vendor the classes in-repo; STOP importing `molecule_runtime.idle_digest.providers.*`) **+** SDK `native-plugins.registry.json` source pins bumped **+** re-vendored into the runtime | ❌ NOT MET — **HARD BLOCKER** | v0.1.0 registered `install: default` (RFC §11); source still lives in the runtime |
| D3-arm | Arm fleet-wide via the org-secret channel (**controlplane#2448 merged**) + soak on the 4 tenants | ❌ NOT MET | flag flip owner-gated (#4411) |
| D4 | Re-audit the **exact** runtime provider file inventory (a prior audit stubbed) | ❌ NOT MET | must enumerate `idle_digest/providers/` on the live runtime tag before deleting |

### B.2 The single most important safety trap

> **Deleting the runtime `idle_digest/providers/` source while the plugins still `import molecule_runtime.idle_digest.providers.*` breaks the digest FLEET-WIDE at plugin load.**

The v0.1.0 plugins are thin `get_provider(context)` factories that **wrap the runtime's own provider classes** — they still import from `molecule_runtime.idle_digest.providers`. If the runtime source is deleted first, every digest-provider plugin raises `ImportError` at load, `build_default_providers()` discovery returns an empty roster, and idle digests go silent across all tenants. The **source-move** (D2 v0.2.x: vendor the classes into each plugin repo and stop importing from the runtime) MUST land and be fleet-universal **before** the runtime source is deleted. Order is: plugin source-move → SDK registry bump → runtime re-vendor → (only then) runtime deletion.

### B.3 Ordered sequence

| Step | What it does / deletes | Gated on |
|---|---|---|
| **D2 source-move PRs** (×4 plugin repos) | Cut **v0.2.x** in `molecule-ai-plugin-digest-{mail,goal,identity,task-queue}`: vendor the provider classes **in-repo**, remove all `import molecule_runtime.idle_digest.providers.*`. | D1 (met) |
| **SDK registry bump** | Bump `native-plugins.registry.json` source pins to the v0.2.x tags. | after the 4 source-move PRs |
| **Runtime re-vendor** | Re-vendor the bumped registry into `molecule_runtime/contracts/native-plugins.registry.json` (drift-gated against SDK main). | after SDK bump |
| **Arm + soak (D3-arm)** | Arm fleet-wide via the org-secret channel (controlplane#2448) + soak on the 4 tenants; confirm digests render from plugin source, not baked. | after re-vendor; **D2 fleet-universal** |
| **Runtime deletion PR** | Delete `molecule_runtime/idle_digest/providers/` **and** the hardcoded roster in `controller.build_default_providers()` (→ discovery-only). Add a **runtime-side no-baked-import anti-regression guard** (negative-controlled). Keep the per-provider **byte-identical parity goldens** (runtime#316) and the armed ephemeral `10e` as live regression gates. | **AFTER D2 fleet-universal + soak** (D3-arm green); D4 audit complete |

### B.4 Rollback posture (D3)

D1/D2 sit **behind the flag** (`MOLECULE_DIGEST_PROVIDER_PLUGINS`, default off → byte-identical baked behaviour), so the entire move is reversible up to the deletion PR by flipping the flag off. The runtime deletion PR is the contract-flip: after it lands there is no baked fallback, so it may only merge once D3-arm has soaked green on all 4 tenants. The parity goldens (runtime#316) + ephemeral `10e` remain as permanent regression gates so a future accidental re-introduction of a baked roster, or a plugin parity drift, fails CI.

### B.5 Docs to update at completion

- `rfc-digest-providers-as-native-plugins.md` — flip **D3 → done**.

---

## Appendix — verified anchors (main `76ad5eab`)

All paths under `workspace-server/internal/` unless noted.

| Symbol | File:line |
|---|---|
| `scheduleBackendIsVolume` | `handlers/schedules_proxy.go:48` |
| `scheduleProxyKillEnv` / `SCHEDULE_VOLUME_PROXY_DISABLED` | `handlers/schedules_proxy.go:39` |
| `MigrateToVolume` | `handlers/schedules_proxy.go:471` |
| legacy `workspace_schedules` CRUD arms | `handlers/schedules.go:122,210,240,298,338,384,427,475,589` |
| `orgImportScheduleSQL` | `handlers/org.go:195` |
| `parseTemplateSchedules` / `seedTemplateSchedules` | `handlers/template_schedules.go:79` / `:126` |
| `ensureSchedulerPluginDeclared` / `BackfillSchedulerPlugin` | `handlers/scheduler_plugin.go:53` / `:152` |
| `pokeVolumeSchedules` + legacy `next_run_at` UPDATE | `handlers/webhooks.go:353` + `:444,489` |
| `Health` / `Orphans` / `ReapOrphans` / `volumeAdminScheduleHealth` | `handlers/admin_schedules_health.go:151` / `:282` / `:322` / `:68` |
| `migrateRuntimeSchedulesFromRemovedPredecessor` (def / call) | `handlers/org_import.go:992` / `:682` |
| `workspace_crud.go` touchpoints | `handlers/workspace_crud.go:695,747,822` |
| schedule route registrations | `router/router.go:566,570,574,716–719` |
| table shape migrations | `migrations/015_workspace_schedules.sql`, `022_workspace_schedules_source.up.sql`, `032_schedule_consecutive_empty.up.sql`, `20260523000000_schedule_consecutive_sdk_errors.up.sql` |
| drop precedent (mirror this) | `migrations/20260520120000_drop_runtime_image_pins.{up,down}.sql` + `internal/db/migration_20260520_drop_runtime_image_pins_test.go` |
| anti-regression guard host | `.gitea/scripts/tests/test_no_retired_deployment_artifacts.py` |

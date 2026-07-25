# Self-host concierge default schedules

A **self-host** deployment's concierge (the platform agent that boots when no
control plane exists) ships with **two default schedules** so a fresh
self-hosted install is useful on day one without the operator wiring anything:

| Schedule | Cron (UTC) | What it does | Tools |
| --- | --- | --- | --- |
| `daily-activity-report` | `0 9 * * *` | Every morning, summarize what happened across the deployment in the last 24 h and deliver it to the user. | `send_message_to_user` (reads `/activity` + `/mail/summary`) |
| `plugin-auto-update` | `0 3 * * *` | Nightly, auto-apply available plugin updates and report any core/runtime updates the operator must deploy. | `check_plugin_updates`, `apply_plugin_update`, `send_message_to_user` |

Both ship `enabled: true`, `timezone: UTC`.

This is a **self-host-only** default. On SaaS/CP tenants the concierge config is
unchanged (see the gate below).

## Where the content lives — the template, not core

The schedule **content** (names, crons, prompts) lives in the **platform-agent
template**, as a top-level runtime-native `schedules:` block in
[`molecule-ai-workspace-template-platform-agent/config.yaml`][tpl]. It is the
editable / re-exportable home: change a cron or prompt there, re-export the
template, and the next concierge provision picks it up — **no molecule-core
change and no core redeploy**.

This follows the principle that a frequently-changing **plugin default** must
live in editable template/config YAML (edit → re-export), never hardcoded as a
Go literal in molecule-core (`[[feedback_plugin_defaults_live_in_template_not_core]]`).
An earlier design that baked the two schedules into a self-host-gated Go seed
was rejected for exactly this reason: static Go = redeploy-to-change; template
YAML = edit-to-change.

The entries are authored in **runtime-native** form — key `cron` (not
`cron_expr`), prompt **inlined** (no `prompt_file` indirection), kebab `name`,
`timezone`, `enabled` — because the concierge graft (below) is a pure
passthrough and does **not** run the template-schedule renderer that ordinary
workspaces use.

[tpl]: https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-template-platform-agent/src/branch/main/config.yaml

## How they reach the concierge — the graft seam

`composeConciergeRuntimeConfig`
(`workspace-server/internal/handlers/platform_agent.go`) builds the concierge's
`/configs/config.yaml`. **Important:** it rebuilds that config **wholesale from
the concierge's RUNTIME base template** (`conciergeBaseTemplateName` →
e.g. `claude-code-default`), **not** from the platform-agent template. It only
grafts three things onto that base: `runtime_config.required_env → []`,
`prompt_files`, and — as of this feature — `schedules`. So any config you want
on the concierge must be grafted here; the platform-agent template's
`config.yaml` is otherwise read only for the persona (`prompts/concierge.md`).

The schedule graft is `graftConciergeSchedules` (same file). Properties:

- **Generic passthrough.** It reads whatever `schedules:` node the
  platform-agent template carries and grafts it onto the composed concierge
  config root. The schedule content is **never hardcoded in Go** — change the
  template, the graft carries it.
- **Self-host gate.** It grafts only when `SelfHostPlatformSeedEnabled()` is
  true — i.e. `MOLECULE_ORG_ID` is unset (self-host / local; no control plane).
  On SaaS the function returns `grafted=false` and the concierge config stays
  **byte-identical** to before.
- **Boot-safe.** After grafting it re-marshals and re-parses the whole document
  (round-trip guard). If the template dir is unresolvable, its `config.yaml` is
  missing/unparseable, it carries no `schedules` node, or the merged doc fails
  the round-trip, it returns `grafted=false` and the caller ships the composed
  config **without** schedules, unchanged. An unloadable `config.yaml` would
  brick boot, so a missing schedule is always preferred over a broken config.
- **Pure passthrough — no render.** Because the template entries are already
  runtime-native, `renderTemplateSchedulesYAML` is **not** called. (Ordinary
  workspaces author schedules in a different form and *are* rendered; the
  concierge path is the exception.)

The runtime then seeds the grafted `schedules:` into the workspace's volume
schedule grid on boot/reload (`seed_schedules_from_workspace_config`), and the
per-workspace scheduler daemon fires them — see
[`scheduler-plugin.md`](scheduler-plugin.md) for how a workspace gets and arms
the scheduler daemon.

## How a self-host operator edits or disables them

Two levers, depending on whether you want the change to be durable across
re-provision:

1. **Durable — edit the template.** Edit the `schedules:` block in the
   platform-agent template `config.yaml` (change cron/prompt, flip
   `enabled: false`, or remove an entry), re-export the template, and
   re-provision the concierge. This is the source of truth; changes here survive
   recreation.
2. **Ad hoc — edit the live volume grid.** Use the concierge's schedule CRUD
   (the self-schedule tools / Canvas schedule routes) to disable, edit, or
   delete a schedule on the running workspace. The runtime seed reconcile
   (`schedule_seed.py` → `ScheduleStore.upsert_template`) is **additive and
   edit-preserving**: a re-provision refreshes template-owned entries but never
   clobbers a user's own runtime edits, and a user-deleted template schedule
   **stays deleted**. So an operator's ad hoc disable/delete on the persisted
   volume grid survives the next reconcile. (This holds as long as the persisted
   volume survives; a workspace recreated onto a **fresh** volume re-seeds the
   template default.)

## `plugin-auto-update` behavior + the self-restart caveat

The `plugin-auto-update` prompt instructs the concierge to:

1. `check_plugin_updates` — list plugins with a newer version available.
2. For each, `apply_plugin_update` — which **re-pins and restarts the affected
   workspace**.
3. Check whether a newer **core or runtime** version exists — it **cannot** apply
   those (an operator deploy is required), so it only **reports** them.
4. `send_message_to_user` an audit: which plugins were auto-updated
   (name old→new) and any core/runtime updates available to deploy.

**Self-restart caveat.** `apply_plugin_update` re-pins and **restarts** the
affected workspace. If the concierge applies an update to a plugin on **its own**
workspace, it restarts itself mid-schedule — the audit message is sent before
the restart completes, and the run does not resume after the bounce. This is
expected: the update lands, the concierge comes back on the new pin, and the
next nightly run reports a clean state.

**Graceful degradation.** If the update verbs are not installed yet (see the
deploy tail below), the prompt tells the concierge to report that update tooling
is not yet installed and do nothing else — so the schedule is harmless before
its tooling ships.

## Status (as of 2026-07-21)

The publish + pin-cascade + image-bake tail (formerly the blocker) is **DONE**:
`plugin-auto-update`'s verbs are shipped in mcp-server **1.9.6**, baked into the
**0.4.36** runtime image. The feature is code-complete. What remains OPEN is
external / owner-gated (operator action), an unbuilt guard, or follow-up
coverage — not code we still owe.

**DONE**

- Template `schedules:` block (template #20) + the tool-verified delivery-rule
  persona (template #21) — a scheduled-job prompt must show a tool result in the
  same turn, retry once, then report FAILED, never a bare confirmation line.
- Self-host graft in core (`graftConciergeSchedules`, core #4549).
- `check_plugin_updates` + `apply_plugin_update` verbs (mcp-server #115),
  **published in mcp-server 1.9.6** via the break-glass publish path.
- Pin cascade to 1.9.6: sdk #139, runtime #340, mcp-server #116, claude-code
  template #336.
- Runtime **0.4.36** fleet roll — `.runtime-version` bumped to 0.4.36 (carrying
  the 1.9.6 verbs) across the four maintained templates (claude-code #338,
  hermes #285, codex #284, openclaw #259), and the self-host installer pinned
  (`scripts/install-workspace-runtime.sh` `RUNTIME_VERSION="0.4.36"`; core
  runtime pins bumped).
- Composition coverage: core #4556 (Go **unit** test pinning both defaults
  through the graft) + runtime #341 (runtime-level seed / fire-signal / deliver
  **unit** tests with stubs). See the Coverage note under Provenance — these are
  unit tests, **not** a live e2e.

**OPEN**

- **Self-host operator image redeploy.** Publishing 0.4.36 does not touch a
  running self-host deployment; the *operator* must pull the 0.4.36 image and
  re-provision the concierge for `plugin-auto-update` to gain its verbs. Until an
  operator does, the schedule degrades gracefully (reports tooling missing).
- **Self-restart health guard.** `apply_plugin_update` restarting the
  concierge's own workspace mid-schedule (see the caveat above) could brick the
  org-root concierge on a bad ref. **Addressed by core #4565** (in review): the
  Apply path now DEFERS the restart when the target is the platform concierge
  (mirroring the reconcile path's `platformConciergeReconcileShouldSkipRestart`
  guard) — the new ref is re-pinned and picked up on the next deliberate restart,
  never as an auto-apply side effect; non-concierge workspaces still restart
  immediately. A post-restart health-probe + pin-rollback remains a possible
  future hardening.
- **Live fire→deliver e2e** — [molecule-core #4555]: a real self-host concierge
  that seeds, has the daemon fire the cron, and `send_message_to_user` actually
  deliver, is **not** covered by the unit tests above.
- **Stale gitea-MCP / release-CI token.** 1.9.6 shipped via the break-glass
  publish path because the mcp-server release-CI token is dead; a fresh token is
  needed to restore the normal tagged-release publish (no `v1.9.6` git tag was
  cut — tags stop at v1.9.5).

## `plugin-auto-update` verbs — now live (the former deploy tail)

`daily-activity-report` works as soon as the template (#20) and core graft
(#4549) are in place and the concierge re-provisions — it uses only
`send_message_to_user` and the always-present `/activity` (+ `/mail/summary`)
endpoints, so it needs no new tooling.

`plugin-auto-update` needs its management-mode verbs, which are now **shipped**
(what used to be the owner-gated deploy tail):

1. **Publish — DONE.** `molecule-mcp-server` carrying the `check_plugin_updates`
   + `apply_plugin_update` verbs (mcp-server #115) is published as **1.9.6** via
   the break-glass publish path.
2. **Pin cascade — DONE.** 1.9.6 is pinned through sdk #139, runtime #340,
   mcp-server #116, and the claude-code template #336.
3. **Bake — DONE.** 1.9.6 is baked into the **0.4.36** runtime image (four
   template `.runtime-version` bumps + the self-host installer pin).

The one remaining step is not ours: a **self-host operator** must redeploy onto
the 0.4.36 image and re-provision the concierge (see OPEN above). Until an
operator does, `plugin-auto-update` degrades gracefully (reports the tooling is
missing). `check_plugin_updates` reads `GET /admin/plugin-updates-pending`;
`apply_plugin_update` POSTs the hardened `/admin/plugin-updates/:id/apply` —
both org-key-authed (management-mode only).

## Provenance

| Piece | PR | Merge |
| --- | --- | --- |
| Template `schedules:` block + CI validation | platform-agent template #20 | `a9b362d2` |
| `graftConciergeSchedules` graft in core | molecule-core #4549 | `380e81f9` |
| `check_plugin_updates` + `apply_plugin_update` verbs | molecule-mcp-server #115 | `707c94aa` |

Design references: [`rfc-scheduler-as-trigger-plugin.md`](../design/rfc-scheduler-as-trigger-plugin.md)
(the scheduler daemon that fires these), memory
`project_selfhost_concierge_default_schedules`.

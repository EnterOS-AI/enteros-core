# RFC: Idle-digest providers as native plugins (decouple the "removable legos" from the runtime)

**Status:** Accepted 2026-07-17 — design ratified (CTO completion directive); D0–D2 merged (§11), D3 + flag flip owner-gated (tracked in issue #4411)
**Author:** CEO Assistant (delegated)
**Related:** [`rfc-scheduler-as-trigger-plugin.md`](rfc-scheduler-as-trigger-plugin.md) (the same "core capability → plugin" move for the cron engine) and [`rfc-platform-mcp-as-plugin.md`](rfc-platform-mcp-as-plugin.md) (the concierge MCP as a `install: concierge` native plugin — the sibling that establishes the entitlement-gated precheck this RFC reuses), ADR-004 (SDK-owns-contract; core/runtime carry zero baked *behaviour*), the **native-plugins registry** (`molecule-ai-sdk` `contracts/plugin/native-plugins.registry.json` — the SSOT this RFC's plugins register in), task #219 (the idle-digest provider system).

## 1. Summary & the decision requested

The idle digest is assembled from **`DigestProvider` "removable legos"** (`molecule_runtime/idle_digest/provider.py`): each provider gathers some state and `contribute()`s a section (goal-state, task-queue, identity-capabilities, sent/inbound mail). The abstraction is already clean — and the runtime **already anticipates the plugin move** in two ways: the `DigestProvider` protocol carries an `official: bool` trust marker (third-party by default) paired with a reserved-section-id gate, and `controller.build_default_providers()` already carries a `PLUGIN SEAM (D5, CTO 2026-07-14)` comment for the mail providers. But the providers are still **assembled by a hardcoded roster** — `build_default_providers()` imports the concrete classes and returns `[IdentityCapabilitiesProvider(...), TaskQueueProvider(), (mail…), GoalStateProvider()]`. That contradicts ADR-004's "runtime carries zero baked behaviour," blocks third-party digest sections, and means every new provider is a runtime release.

This RFC moves the digest providers **out of the runtime** and delivers them as **native plugins** (registered in the native-plugins registry, `install: default`), loaded by a runtime provider-loader instead of hardcoded imports — the exact decoupling the scheduler-as-trigger-plugin RFC did for cron.

**Decision requested:** approve (a) a new **`digest-provider` plugin contribution kind** (SDK plugin-manifest contract), (b) the runtime loading `DigestProvider`s from installed native plugins, and (c) the phased extraction of the current providers into plugin repos. Open items in §10.

### 1.1 What this is NOT

This is **not** "make the A2A mailbox / kernel a plugin." The mailbox kernel (`kernel.py`, `mailbox_dir.py`) and the A2A inject lane are the **transport substrate** — the nervous system that autonomous turns (and plugins themselves) are delivered *into*. Plugins run *on* it; making the transport a plugin is circular. The A2A **tools** (`a2a_tools_inbox`, `messaging`, …) are a separate, later question (they look like MCP-tool contributions). This RFC scopes to the **digest providers** only.

### 1.2 Not to be confused with the concierge `daily-activity-report` schedule

The self-host concierge ships a `daily-activity-report` **schedule** (see
[`rfc-scheduler-as-trigger-plugin.md`](rfc-scheduler-as-trigger-plugin.md) §P5.1
and `docs/runbooks/selfhost-concierge-default-schedules.md`) that composes a
morning summary from the **same** `/activity` (+ `/mail/summary`) sources these
digest providers read — the mail providers here take their comms source from the
platform mail-summary API, and the scheduled report pulls
`GET /workspaces/<id>/activity?since_secs=86400` plus `/mail/summary`. Despite
the shared data sources the two are **distinct mechanisms**: the idle digest is
an **in-process, wake-gating** assembly (it decides whether an idle agent stays
asleep, `official`/reserved-id-governed), whereas the daily report is a **cron
self-turn** the concierge composes and **delivers to the user** via
`send_message_to_user` (canvas chat + push). One is agent-facing wake logic; the
other is a user-facing digest email. They neither share code nor gate each other.

## 2. What exists today (grounding, not proposal)

- **The protocol** (`idle_digest/provider.py`): `DigestProvider` = `provider_id: str`, `official: bool`, `async contribute() -> Sequence[Contribution]`, `on_included(fired_at)`. `ProviderRunner` invokes each provider with per-provider timeout + a consecutive-failure disable, so one bad provider never wedges the idle tick.
- **The trust marker already exists** — `official: bool` on the protocol: *"True only for official platform-owned providers. Read by the runner via getattr with a False default, so a provider that omits it is treated as third-party — the fail-safe direction. An official provider MUST set `official = True` to legitimately use a reserved id."* Paired with a reserved-section-id gate (`assembler.check_reserved_id` / `contract.py`). This is the embryo of the native-vs-third-party trust tier this RFC needs — it is **extended**, not invented (§9).
- **The legos** (`idle_digest/providers/`): `GoalStateProvider` (`goal.py`, owns `goal.yaml` + renders the tier-7 goal section), `TaskQueueProvider` (`task_queue.py`), `IdentityCapabilitiesProvider` (`identity.py`), `SentMailProvider` / `InboundMailProvider` (`mail.py`, need a comms source — the platform mail-summary API). `providers/__init__.py` calls them *"Official default-installed … removable legos"* and plans phase-2 additions: sent-folder, inbound-a2a, delegation-results, **scheduler**.
- **The assembly** — `controller.build_default_providers()`: *"The official provider roster, in tier order. The boot seam calls this."* It imports the concrete classes and returns the list; `Controller.providers` is the injected seam. The function **already documents the direction**: `PLUGIN SEAM (D5, CTO 2026-07-14)` — the mail providers are to move behind the plugin boundary, handing their source in via `comms_source`, *"and this function stays the one line that changes."* So the CTO has already blessed plugin-izing the mail provider (D5); this RFC generalizes that one blessed seam to the whole roster.
- **Delivery today:** none — providers are Python modules inside the runtime wheel; `build_default_providers()` is the hardcode this RFC replaces with plugin discovery.

### 2.1 The one hard difference from the scheduler (why this isn't a copy-paste)

A trigger plugin is a **subprocess daemon** (spawned, sandboxed, talks over the inject lane). A digest provider is **in-process**: the controller `await`s `provider.contribute()` inside the runtime event loop. So the delivery mechanism is *not* a daemon spawn — it is the runtime **importing a `DigestProvider` class from an installed plugin's Python module**, analogous to how `plugins_registry` already loads per-runtime adaptor modules (`<plugin>/adapters/<runtime>.py`). That in-process trust boundary is the central design constraint (§9).

## 3. Goals / non-goals

**Goals**
- A `DigestProvider` is delivered as a **native plugin**, discovered + loaded by the runtime, not hardcoded in `controller.py`.
- The four current providers ship as native plugins (`install: default`) in the native-plugins registry — every workspace still gets them, but via the plugin path.
- Third-party / future providers (delegation-results, scheduler-status, …) plug in without a runtime release.
- The `DigestProvider` protocol becomes an **SDK contract** (SSOT), like the trigger client + schedule grid.

**Non-goals**
- Touching the mailbox kernel / A2A transport (substrate — stays).
- Making digest providers arbitrary untrusted marketplace code in-process (see §9 — native/first-party trust tier only, at least initially).
- Changing digest *content* or cadence behaviour — this is a delivery move, byte-identical output.

## 4. Target architecture

- The `DigestProvider` protocol + `Contribution` shape are an **SDK contract** (`contracts/digest-provider/`), vendored into the runtime (drift-gated) so plugin authors and the runtime agree.
- A plugin declares `kind: digest-provider` and `contributes.digestProviders: [{ module, class }]` (a Python entrypoint the runtime imports).
- `build_default_providers()` becomes a **discovery function**: the runtime scans installed digest-provider plugins (via the same `load_plugins` path the trigger daemon uses), instantiates each `DigestProvider`, and returns them **in tier order** in place of the hardcoded imports. `Controller.providers` (the existing injected seam) and `ProviderRunner` failure isolation are unchanged — a provider that fails to import or `contribute()` is dropped, never fatal.
- The four current providers become plugin repos (`molecule-ai-plugin-digest-*`), each an entry in the **native-plugins registry** with `install: default`.
- **In-process trust — extend the existing `official` marker.** Today `official = True` + the reserved-id gate distinguish platform providers from third-party at *contribution* time. This RFC extends the same axis to *load* time: only providers delivered by a native-registry (`install: default`) plugin may set `official = True` / claim a reserved id; a non-native digest-provider either loads untrusted (no reserved id, best-effort) or is refused entirely (open Q3). The native-plugins registry is the curated allow-list; a capability/entitlement gate (mirroring the concierge-MCP precheck) fences it.

## 5. The seams that must be built (the honest cost)

| # | Seam | Where |
|---|------|-------|
| G1 | `DigestProvider` + `Contribution` as an SDK contract, vendored + drift-gated into the runtime | sdk, runtime |
| G2 | `digest-provider` plugin kind + `contributes.digestProviders` in the plugin-manifest schema | sdk |
| G3 | Turn `build_default_providers()` into a **discovery function**: enumerate installed digest-provider plugins, import their module/class, return in tier order in place of the hardcoded imports; keep `Controller.providers` seam + `ProviderRunner` isolation | runtime |
| G4 | Extend the existing `official`/reserved-id trust axis to load time: only native-registry `install: default` plugins may load `official`/reserved (entitlement precheck) | runtime + core |
| G5 | Extract each current provider into a plugin repo; register in the native-plugins registry | plugin repos, sdk |
| G6 | Retire the hardcoded imports + `providers/` package from the runtime once every maintained runtime carries the loader + the plugins are delivered | runtime |

## 6. Invariants to preserve (regression contract)

- **Byte-identical digest output** at each cutover (same sections, same order, same cadence banding) — the move is delivery, not behaviour.
- **Failure isolation preserved** — a missing/broken provider plugin degrades to "that section absent," never a digest crash or boot failure (the `ProviderRunner` guarantee).
- **No untrusted in-process code** — only native/first-party providers load in-process until a sandboxed provider model exists.
- **Comms source injection** stays the one-line seam it already is (`comms_source`) — the mail providers get their platform source the same way.
- **Idle-digest stays native-loop-driven** — the *scheduling* of the digest (the kernel idle loop) is unchanged; only the *providers* move.

## 7. Phasing (each an independently reviewable increment)

- **D0 — contract (G1, G2).** Lift `DigestProvider`/`Contribution` into an SDK contract; add the `digest-provider` kind + `contributes.digestProviders`. Vendor + drift-gate into the runtime. *No behaviour change.*
- **D1 — loader behind a flag (G3, G4), proven on mail.** Turn `build_default_providers()` into a discovery function that *also* loads digest-provider plugins, behind `MOLECULE_DIGEST_PROVIDER_PLUGINS`. Prove it on the **mail provider** — the seam the CTO already blessed as D5 and the one already parameterized by `comms_source`, so it is the least-coupled first extraction. Negative-control the trust gate (a non-native provider can't load `official`/reserved). *Flagged.*
- **D2 — extract the set (G5).** mail → `molecule-ai-plugin-digest-mail`, then task-queue, identity, goal-state (goal-state last — it owns durable `goal.yaml` state, the most coupled). Each a native-registry `install: default` entry. Dual-run parity (baked vs plugin) per provider.
- **D3 — retire baked (G6).** Once every maintained runtime carries the loader and the plugins are delivered fleet-wide, delete `idle_digest/providers/` + the hardcoded roster in `build_default_providers()`; it only knows discovery.

## 8. Delivery checklist (Definition of Done)

- **Contract in sync** across sdk/runtime — `DigestProvider` vendored byte-identical, drift-gated.
- **Parity** — a golden digest (fixed workspace state) renders byte-identical baked vs plugin-loaded, per provider.
- **Failure isolation** — negative-controlled: a provider that raises on import / on `contribute()` / times out is dropped, digest still fires (assert the section is absent, boot survives).
- **Trust gate** — negative-controlled: a non-native (marketplace) digest-provider plugin is refused in-process.
- **No new scheduled CI**, contracts drift-green, memory + this RFC updated each phase.
- **Rollback** — D1/D2 behind the flag; D3 only after the loader is universal.

## 9. Risks

- **In-process untrusted code (the big one).** A digest provider runs in the runtime event loop with runtime privileges. The `official`/reserved-id marker already models *contribution*-time trust; this RFC extends it to *load* time. Mitigation: native/first-party only (native-plugins registry `install: default`, entitlement-gated) until a sandboxed provider host exists. This is why digest-providers are *native* plugins, not open marketplace plugins.
- **Import-time failure = boot risk.** A plugin whose module raises at import could break digest assembly. Mitigation: the loader imports each provider defensively (same isolation `ProviderRunner` gives `contribute()`), dropping a bad import.
- **Ordering drift.** Section/tier order matters (mail tier-2/3, goal tier-7). Mitigation: order is data (registry/tier field), asserted by the parity golden.
- **Scope creep into transport.** Keep the mailbox kernel / A2A out (§1.1) — resist "everything is a plugin" pressure onto the substrate.

## 10. Decisions

**Proposed (need CTO sign-off):**
- New **`digest-provider` plugin kind** + `contributes.digestProviders` (module/class entrypoint).
- Digest providers are **in-process, native-only** plugins (trust tier), registered `install: default` in the native-plugins registry.
- The mailbox kernel + A2A transport **stay substrate** — explicitly out of scope.

**Open:**
1. **Loader mechanism** — reuse the `plugins_registry` adaptor path (`molecule_runtime/plugins_registry/builtins.py`, the `MCPServerAdaptor` precedent from `rfc-platform-mcp-as-plugin.md`) vs a dedicated digest-provider entrypoint resolver? *Leaning: add a `DigestProviderAdaptor` alongside `MCPServerAdaptor` so both native-plugin kinds share one registry loader.*
2. **task-queue & goal-state — provider vs substrate?** The *providers* render digest sections; the underlying task queue (core `a2a_queue_status.go`) and the goal store are separate. Confirm we extract only the digest-facing provider, leaving the stores where they are (goal store already lives on the volume under the provider's mailbox dir).
3. **Trust-tier enforcement point** — runtime-side (refuse to import a non-native digest-provider) vs core-side (never declare one) vs both (defense in depth). *Leaning: both.*
4. **Sandboxed third-party providers** — out of scope now; note as the future unlock that would let marketplace digest-providers exist.

## 11. Implementation status (as built)

The phasing landed as designed; the open questions above were resolved in code as noted. Each increment merged independently, adversarially reviewed.

| Phase | What landed | Where |
|---|---|---|
| **D0** — contract | `contributes.digestProviders` + `$defs/digestProviderContribution` on the plugin-manifest schema; `digest-provider` documented as a `kind`; conformance tests. Tolerant skip-not-reject property + strict `$defs`, mirroring `contributes.daemons`. | sdk#109 (merged) |
| **D2 — registry** | The 4 digest plugins (`molecule-ai-plugin-digest-{mail,task-queue,identity,goal}`, tagged v0.1.0) registered `install: default` in the native-plugins registry. | sdk#108 (merged) + 4 new plugin repos |
| **D1 — loader (flagged)** | `molecule_runtime/idle_digest/plugin_loader.py`: discovers `contributes.digestProviders`, imports each provider in-process, appends in `build_default_providers()` behind `MOLECULE_DIGEST_PROVIDER_PLUGINS` (default off → byte-identical). Load-time trust gate (official/reserved provider loads only from a native plugin; fail-safe empty allow-list). Prove-on-mail parity test + full trust/skip matrix. | runtime#309 (merged) |
| **Core consumption** | Core reads the native-plugins registry as SSOT (`molcontracts.NativePlugins`), retiring the hardcoded `SchedulerPluginSource`/`conciergePlatformMCPSource` constants; declares the `install: default` set on every workspace at provision, gated `MOLECULE_DECLARE_DEFAULT_NATIVE_PLUGINS` (default off). | core#4415 (merged), on the molcontracts bump core#4414 (merged) |
| **D1 — trust source = registry (SSOT)** | The runtime derives the load-time trust allow-list from the **vendored native-plugins registry** (`molecule_runtime/contracts/native-plugins.registry.json`, drift-gated against SDK main), not the interim `MOLECULE_NATIVE_PLUGIN_NAMES` env var (demoted to an escape hatch). Names are derived from each entry's **`source` repo segment** — what `load_plugins` keys `LoadedPlugin.name` on — not the registry `name` field (the scheduler proves the divergence); a regression to `name` is negative-controlled. Fail-safe empty on a missing/malformed registry. | runtime#310 (merged) |
| **D1 — CI e2e (dark)** | Ephemeral-CP gate sub-step `10e`: a native digest-provider plugin loads in-process, admitted by the registry trust gate — the e2e never sets `MOLECULE_NATIVE_PLUGIN_NAMES`, so a pass proves the registry source. Gated `E2E_DIGEST_PLUGIN_CHECK` (default off until the fleet image carries the loader). | core#4416 (merged) |

**Open questions — resolved as built:**
1. **Loader mechanism** → a **dedicated** digest-provider resolver (`plugin_loader.py`, `module:Attr` via `spec_from_file_location` under the plugin dir), not the `MCPServerAdaptor` path. The trust concern (in-process) warranted a purpose-built loader with its own path-traversal + native-only guards rather than overloading the MCP adaptor.
2. **task-queue & goal-state — provider vs substrate** → extract only the **digest-facing provider**; the task-queue and goal stores stay runtime-owned. The plugin repos ship thin `get_provider(context)` factories that wrap the runtime provider (D3 moves the source into the repos).
3. **Trust-tier enforcement point** → **both**, as leaned. Runtime-side load-time gate (native-only for official/reserved) **and** core-side (the native-plugins registry is the only declared source; the concierge MCP stays entitlement-gated to `kind=platform`).
4. **Sandboxed third-party providers** → still the future unlock; unchanged.

**D0–D2 + the D3 source-move have landed** (every row above merged and adversarially reviewed). **D3's baked delete is NOT merge-ready** — beyond the owner-gated arming, real ENGINEERING and a live divergence remain (a fresh 2026-07-19 re-audit; the earlier "all landed, only owner-gated" claim understated this):

- ⚠️ **LIVE SSOT DIVERGENCE — byte-identical parity is currently BROKEN on runtime main.** After the 4 plugins froze at `v0.2.0` (2026-07-17 20:35Z), the **baked** `providers/identity.py` advanced via `2db2e8f` (`<SYSTEM IDLE PROMPT>` framing + in-process bridge-registry union) and `5e634ee` (#327 byte-budget/valve). The frozen identity plugin lacks all of it, so the identity header no longer renders byte-identical baked-vs-plugin — and the per-plugin parity CIs went self-contained (plugin-vs-static-golden) and **no longer compare baked-vs-plugin**, so the drift shipped undetected. (mail/goal/task are NOT stale: #292's resilience is in the shared substrate/`PlatformMailSummarySource` the plugins inherit; goal/task render halves are unchanged.)
- **Pre-delete ENGINEERING (not "operational only"):** (a) repoint `molecule_runtime/a2a_tools_idle.py` off `idle_digest.providers` (it still imports `GoalStateProvider`/`TaskQueueProvider`, backing the `goal_get`/`task_list` MCP tools — a naive delete breaks agent tooling fleet-wide); (b) relocate the `GoalStore`/`TaskQueueStore` writer halves (runtime-owned); (c) rewrite the parity goldens (they compare baked==plugin — deletion removes both sides) + add a runtime retired-artifacts guard (the runtime has none); (d) **cut identity plugin `>v0.2.0` to absorb the framing/bridge-union/#327** before the delete, else it regresses the header; (e) **rebase runtime#324** (head predates #292/#327/framing; `mergeable=false`).
- **D3 delete** — remove `idle_digest/providers/` + make `build_default_providers()` discovery-only. Owner-gated on Phase-B arming + flag-on AND the pre-delete engineering above.
- **Fleet rollout** — runtime image rebuild (carrying the loader + vendored registry trust source) + re-provision, then flipping `MOLECULE_DIGEST_PROVIDER_PLUGINS`, `MOLECULE_DECLARE_DEFAULT_NATIVE_PLUGINS`, and `E2E_DIGEST_PLUGIN_CHECK` on.
- **This RFC's own sign-off** — the phasing is fully built to spec; awaiting CTO ratification of the design record.

The regression contract (§6) and DoD (§8) hold: every merged increment is flag-off byte-identical, so nothing is live until the owner arms the flags.

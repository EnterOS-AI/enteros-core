# RFC: Self-Host Gated Onboarding Scene

First-run fullscreen setup for the platform agent (concierge) — canvas + workspace-server. **This document is the design SSOT for the feature.**

- **Status:** Designed 2026-07-07 (operator-ruled) — implementation tracked in [molecule-core #3496](https://git.moleculesai.app/molecule-ai/molecule-core/issues/3496)
- **Prerequisite:** the ensure-endpoint enum bugfix — [PR #3495](https://git.moleculesai.app/molecule-ai/molecule-core/pulls/3495)
- **Scope:** `canvas` + `workspace-server` self-host first-run; SaaS/CP behavior unchanged (§7)
- **Related:** [`rfc-platform-agent.md`](rfc-platform-agent.md) (amended by this design), [ADR-004](../adr/ADR-004-unconditional-concierge-and-one-ensure-flow.md), the consolidated idle-prompt design §5.1 (proactive greeting = the onboarding handoff)
- **Path convention:** canvas file references are relative to `canvas/src/`; Go references are relative to `workspace-server/`.

> **Operator ruling (2026-07-07).** "On SaaS the platform does all set up, but self-host should have a **gated onboarding fullscreen scene** for the user to select **runtime, provider, model** for the platform agent **and its key**." SaaS keeps the CP-driven install (the CP calls `POST /admin/org/platform-agent` at org-provision); self-host gets a blocking first-run scene that ends with a working concierge. This supersedes the floating `OnboardingWizard` card (workspace-era, teaches the wrong hand-create-workspaces model) on the self-host path.

> **Hard prerequisite.** `POST /admin/org/platform-agent/ensure` is 100%-broken at design time: `COALESCE(status, '')` against the `workspace_status` enum fails at parse time (`platform_agent_ensure.go:273`, introduced `8cd393187` 2026-06-26; the same pattern `registry.go:1347` warns against). Fix = select `status` bare + update the 11 sqlmock regexes in `platform_agent_ensure_test.go` + add one real-Postgres regression test (sqlmock cannot catch enum coercion). Every flow below assumes this fix has landed ([PR #3495](https://git.moleculesai.app/molecule-ai/molecule-core/pulls/3495)).

## 1 · Why the scene exists — the first-run dead end it replaces

- Fresh self-host before this feature: no CP, boot-seed off (or on with no key) ⇒ canvas shows "No platform agent yet" + a repair button that 500s, no mention of API keys, and the floating wizard teaches manual workspace creation. Nobody designed the self-host first-run.
- Even a seeded concierge lands `failed` on a keyless stack — the self-host model fallback `minimax/MiniMax-M2.7` (`platform_agent.go:576`) is a **platform-proxy arm**, which does not exist on self-host ⇒ `MISSING_PLATFORM_PROXY` abort (`workspace_provision_shared.go:237-244`).
- The scene collects exactly what provisioning fail-closes on — (runtime, model) pair + matching BYOK key — *before* the first provision, so the concierge comes up green on the first attempt.

## 2 · Gate — when the scene shows, and why it is stateless

- **G1 · self-host:** `getTenantSlug() === ""` (`tenant.ts:76-97`) **AND** `GET /org/identity` → `org_id === ""` (open route, `router.go:117`; empty on self-host by contract, `platform_agent.go:1210-1227`). The server-declared check disambiguates the SaaS apex / Vercel-preview hosts that also derive an empty slug.
- **G2 · platform agent exists but is unconfigured:** the root **always exists** — seeding is unconditional on self-host (§6). Gate: after first `/workspaces` hydration (`page.tsx:44-48`), the `kind === WORKSPACE_KIND.Platform` node (`ConciergeShell.tsx:204-216` signal) has **never been online AND no LLM key is configured** (`GET /settings/secrets` has_value scan, `deploy-preflight.ts:272-274`). A missing row is a defensive fallback only (the scene still shows; ensure's 'created' path handles it).
- **∅ · dismissal = derived state, not a flag:** no localStorage. The gate re-derives from server state every load: once a platform root is online (or a key exists and the agent has been online once), the scene never renders again. Mid-flow refresh resumes at the right step for free (key already written? agent already created?). Never render before hydration completes — no flash.

**Mount point:** root layout, sibling-inside `AuthGate` (`AuthGate.tsx` — the only existing fullscreen-gate precedent, and already a pass-through on self-host), *above* the desktop/mobile view switch so one scene covers both. Mobile has no platform-agent concept at all today (`MobileHome.tsx:199` generic empty state; `components.tsx:38-55` drops `kind`) — the root mount fixes that for free.

**Fail-closed-to-invisible:** because the root mount ships to SaaS too, the scene renders only on **positive confirmation of both gates**. If `/org/identity` errors, times out, or returns anything ambiguous — render the normal UI and never block. A gate bug must not be able to blank a SaaS tenant; the failure mode of this feature is "scene doesn't appear on self-host", never "SaaS is stuck behind a setup screen".

## 3 · Scene flow — four steps, one create

**Picker ruling (operator 2026-07-07):** runtime, provider, and model are all **dropdowns of available options only** — a strict cascade `Runtime ▾ → Provider ▾ → Model ▾` where each selection re-derives the next list and resets downstream picks. No free-text entry anywhere in the scene (`allowCustomModelEscape=false`); an invalid (runtime, model) pair must be unreachable through the UI, with the server's atomic 422 validation as the drift guard only.

1. **Welcome** *(fixed brand · no name input)* — explain what the platform agent is (org root, dispatcher). The agent name is **FIXED to the brand: "Enter OS Agent"** (operator ruling 2026-07-07) — no name field; the scene passes `name: "Enter OS Agent"` on ensure. Renaming stays possible post-setup via the standard workspace rename. Server defaults are untouched (`defaultPlatformAgentName()`: "\<MOLECULE_ORG_NAME\> Agent" else "Org Concierge", `platform_agent.go:134-139` — SaaS/CP naming unchanged). Branding reaches the persona automatically: provision feeds the row name into the `{{CONCIERGE_NAME}}` substitution (`workspace_provision_shared.go:271` → `applyConciergeProvisionConfig` → `substituteConciergeName`, `platform_agent.go:508`), so the agent introduces itself as "Enter OS Agent" with zero backend change.
2. **Runtime ▾** *(dropdown · SSOT: `GET /templates`)* — dropdown of template-backed runtimes: bucket `/templates` rows by `runtime` — the ConfigTab pattern (`ConfigTab.tsx:603-652`, a plain `<select>`, honors `displayable:false`), **NOT** CreateWorkspaceDialog's hardcoded list (the known drift trap). Only container-backed runtimes appear (claude-code / codex / hermes / openclaw) — correct for a concierge; external/kimi-cli/mock are not offerable roots. Changing it resets provider + model. The always-seeded root arrives pre-stamped with the default runtime (`MOLECULE_DEFAULT_RUNTIME` else the compiled **hermes** fallback); a different pick applies via the standard runtime-change path — `PATCH /workspaces/:id` — because the ensure upsert deliberately preserves runtime (`platform_agent.go:1461-1465`).
3. **Provider ▾ + Model ▾** *(two cascading dropdowns)* — two dropdowns fed from the chosen runtime's template row via the ProviderModelSelector **data layer** — `buildProviderCatalogFromRegistry` (`:310-352`), legacy `buildProviderCatalog` fallback when `registry_models` absent, `findProviderForModel` for adopt-mode back-fill. Provider ▾ lists the runtime's registry arms (codex→openai only; hermes/openclaw→kimi/minimax/gemini BYOK; claude-code→anthropic/kimi/minimax); picking one re-derives Model ▾ to that provider's models. If ProviderModelSelector's `grid|stack` variants don't render as selects, add a `dropdown` variant to it — do **not** fork the catalog logic. **Self-host filtering is free:** `/templates` already strips the platform provider + platform-arm models when no proxy is wired (`templates_registry.go:75-121`) ⇒ BYOK-only options. No free-text escape. Model is REQUIRED — no default by design (`workspace_provision.go:843-858`: "the platform must not provide a default"; the old silent `anthropic:claude-opus-4-7` fallback wedged every codex workspace).
4. **API key** *(global secret)* — key *name* comes from the selected provider's `required_env` / `auth_env` (`registry_models[].required_env`; SSOT = providers.yaml `auth_env`: anthropic-api→`ANTHROPIC_API_KEY`·`ANTHROPIC_AUTH_TOKEN`, openai-api→`OPENAI_API_KEY`, moonshot→`MOONSHOT_API_KEY`·`KIMI_API_KEY`, minimax→`MINIMAX_API_KEY`·…, google→`GOOGLE_API_KEY`). Optional client-side format check (`lib/validation/secret-formats.ts`) + server `POST /secrets/validate {provider, key}`. "Already configured" state keys off `has_value` from `GET /settings/secrets` (values are never returned).
5. **Create → progress → handoff** — wire sequence in §4. Progress renders provisioning states from the store's websocket updates (`socket.ts`) with `GET /workspaces` polling as fallback; failure surfaces the workspace's `last_error` humanized (§8) + Retry. On `online`: the scene dismisses into ConciergeShell home — and per the idle-prompt design §5.1 (consolidated idle-prompt design), **the concierge greets first**. The agent's proactive persona-derived greeting IS the final onboarding step; the scene ships no "send your first message" tutorial.

## 4 · Create sequence — ordering is load-bearing

```text
1. PUT /settings/secrets            {key: <provider auth_env[0]>, value: <key>}      — global scope (PUT, not POST; secrets.ts:72-95)
2. PATCH /workspaces/{root_id}      {runtime}                                        — only if the pick differs from the seeded default
3. POST /admin/org/platform-agent/ensure   {name: "Enter OS Agent", model, force:true} — extended body (see below)
     server: repair the EXISTING root ('repaired' path) → validate (runtime, model) → write MODEL workspace_secret → trigger provision
4. watch /workspaces (socket + poll) until kind='platform' status: provisioning → online | failed
5. failed → humanized last_error + Retry (re-ensure; idempotent + debounced)
```

- **Key before create:** the global-secrets auto-restart fan-out **excludes** `kind='platform'` (`secrets.go:713-761`) — a key written *after* create never reaches the concierge without an explicit restart. Written first, the ensure-triggered provision picks it up for free.
- **Why ensure must learn `model` (backend change):** the pre-feature ensure body is `{name?, runtime?, force?}` only (`platform_agent_ensure.go:220-229`) and it triggers the provision **asynchronously in the same call** (`:329-331`). A configure-then-`PUT /workspaces/:id/model` sequence **races the triggered provision**; the losing order boots the concierge on the self-host default `minimax/MiniMax-M2.7` → platform arm → `MISSING_PLATFORM_PROXY` abort. Fix: add `model` to the payload; the handler validates against the registry for the row's runtime (422 `UNREGISTERED_MODEL_FOR_RUNTIME`, same as SetModel `secrets.go:916-990`) and calls `setModelSecret` *between* repair and `triggerPlatformProvision`. `ensureConciergeModel` is seed-only and respects an existing MODEL secret (`platform_agent.go:718-735`), so the user's pick sticks permanently.
- **Runtime rides PATCH, not ensure:** the ensure/install upsert deliberately preserves an existing root's runtime, so the scene applies a changed runtime via the standard `PATCH /workspaces/:id` path *before* ensure — same validation/reset semantics ConfigTab already implements.
- **Validation gap found by the SSOT audit — close it in ②:** ensure/install previously stamped the payload runtime **verbatim** with zero validation. `POST ensure {runtime:"external"}` "succeeds" (row upserted kind='platform' runtime='external'; response claims `provisioning:true`) but the provision **silently no-ops** for external-like runtimes and external-only machinery (healthsweep's awaiting_agent flip, a2a-proxy branches) adopts the row — a permanently wedged concierge with a lying API response. The public `/registry/register` path already 403s kind='platform'; the AdminAuth endpoints didn't. The consolidated flow adds the guard: platform kind requires `isKnownRuntime(runtime) && !isExternalLikeRuntime(runtime) && runtime != "mock"` → 422 otherwise.
- **Auth (current contract):** all calls ride the existing `platformAuthHeaders()`. Local development may supply the matching `ADMIN_TOKEN` / `NEXT_PUBLIC_ADMIN_TOKEN` pair; SaaS uses the verified control-plane session. The design-time fresh-token fail-open assumption was removed: AdminAuth now fails closed, so a standalone first boot must be configured with `ADMIN_TOKEN` rather than exposing an unauthenticated admin surface.
- **No new enumeration endpoints needed:** `/templates` covers runtimes + models + required_env; avoid `GET /admin/llm/offered-models` (it does NOT filter platform arms on self-host).

## 5 · One mode — the scene configures, it never creates *(operator ruling 2026-07-07)*

> "We should always have a tenant agent, whether self-host or platform-owned. The first agent and manager agent should always be the concierge."

**Concierge existence is unconditional.** On self-host the boot seed runs on every start with no flag (§6); on SaaS the CP installs it at org-provision. So by the time any canvas loads, the org root exists — the scene is a pure **configurator** of the existing root: runtime via `PATCH` (the upsert deliberately preserves runtime, `platform_agent.go:1461-1465`), model + name + provision-trigger via ensure `force:true` → `decideEnsureAction` 'repaired' path (`platform_agent_ensure.go:154-176`) re-provisions the existing id in place. The MODEL secret written by the scene wins over any default permanently (seed-only semantics). A second platform root is impossible (partial unique index `uniq_workspaces_one_platform_root`); a missing row is a defensive edge the ensure 'created' path still covers. This also kills the old create-race class: the scene never triggers a provision it hasn't already configured.

### 5.1 · One canonical flow — SSOT consolidation *(operator ruling 2026-07-07)*

> "These are using the same function, so it should be SSOT — calling the same endpoint instead of different logic for each. Also do a clean-up at the end for the code."

Before this feature only the bottom half is shared (`installPlatformAgent`, `platform_agent.go:1417` — the transactional row upsert + root re-parent + anchor migration). The top halves diverge three ways: the ensure handler carries decide/revive/provision logic (`decideEnsureAction`), the boot seed re-implements exists-checking (`EnsureSelfHostedPlatformAgent`) *plus* a separate bespoke provision-and-identity-probe pass (`MaybeProvisionPlatformAgentOnBoot`), and the CP install endpoint is a third thin wrapper. Consolidation:

- **Extract ONE service function** — `ensurePlatformAgentFlow(ctx, db, opts{name, runtime, model, force, triggerProvision})`: decide (created / exists / repaired / revive) → install → name/model write → optional provision trigger. This is the single SSOT for the concierge lifecycle; the §4 `model` extension (work item ②) is implemented **inside this flow**, not bolted onto the old handler. 100%-covered per §10.1.
- **Every entrypoint becomes a thin adapter:** HTTP `POST /admin/org/platform-agent/ensure` (canvas scene; provision on) · the boot seed (in-process on self-host, §6; provision per the D2 posture). This **deletes** both duplicated boot top-halves — the identity-probe/restart-once behavior folds into the flow's 'repaired' branch.
- **Legacy CP endpoint = contract-frozen shim, then gone:** `POST /admin/org/platform-agent` becomes a deprecated row-only adapter over the same flow (`triggerProvision:false`, CP-supplied id) — byte-identical contract so §7 holds unconditionally. CP's own migration to `/ensure` is a **separate CP-repo ticket on the CP's release schedule** (not part of this feature); the shim is deleted in the cleanup phase (⑧) only after that lands.

### 5.2 · SSOT ledger — the rest of the blast radius *(audited 2026-07-07)*

Read-only two-agent audit of every contract surface the scene consumes. Verdicts: **consolidate** = this feature fixes or files it · **mirror-ok** = documented leaf-module mirror, import it · **separate** = deliberately not shared.

| Surface | Verdict | Action for this feature |
|---|---|---|
| Workspace **kind** | mirror-ok | Import `WORKSPACE_KIND` (`lib/workspace-kind.ts` — the documented TS mirror of Go `models.Kind*`). Cleanup ⑧ folds the one stray raw literal (`MonitorPanel.tsx:290`). |
| Workspace **status** enum | **consolidate** | Go SSOT has 10 wire values (`models/workspace_status.go:35-46`, drift-test-pinned); canvas handles only 6, via raw literals across ~25 files, plus 2 invented synthetics (`starting`, `not_configured`). Create `lib/workspace-status.ts` (the workspace-kind.ts leaf-module pattern; all 10 values + the synthetics documented); the scene imports it; migrating the ~25 literal sites = filed follow-up, not this PR. |
| **Error codes** (§8) | **consolidate** | No Go const block exists (raw strings at emit sites; `MISSING_BYOK_CREDENTIAL` emitted from two files) and ZERO TS mirrors — the scene is the first TS consumer. Create `lib/workspace-error-codes.ts` with per-code emit-site pointers and which wire channel carries it: create-boundary codes ride the 422 body `code`; provision-abort codes ride the `WORKSPACE_PROVISION_FAILED` socket-event extra. File the Go-side const block as its own issue. |
| Runtime lists — CreateWorkspaceDialog (`RUNTIME_OPTIONS` / `BASE_RUNTIME_TEMPLATE_IDS` / `DEFAULT_RUNTIME`) | **consolidate** | The exact drift-bug class ConfigTab already fixed and documented. Cleanup ⑧: derive from the `/templates` rows the dialog already fetches. |
| Runtime list — ContainerConfigTab | **consolidate** | Derive runtime choices from the same `/templates` source as ConfigTab so retired or newly-added runtimes cannot drift into a hardcoded list. Fix rides ⑧ (or a standalone small PR). |
| Offline fallback + external-like/display-name mirrors | mirror-ok | Documented, small, replaced-on-fetch. The scene imports `isExternalLikeRuntime` / `runtimeDisplayName` (`lib/externalRuntimes.ts`, `lib/runtime-names.ts`) and `WS_EVENTS` (`lib/ws-events.ts` — the documented socket-event mirror) rather than re-listing anything. |
| Provider/model catalog — registry path | already-SSOT | providers.yaml (CP-canonical) → sha-pinned synced copy in core → codegen → `/templates` `registry_*` fields, CI drift-gated at every hop. The scene consumes ONLY this path. |
| Provider/model catalog — legacy vendor heuristic (`VENDOR_LABELS`/`inferVendor`) | separate | The known pre-SSOT vocabulary, deliberately retained as the fallback for non-registry backends; its retirement is already sequenced upstream (internal#718 P3). Hard rule: the scene adds NO new dependencies on it. |
| molecule-contracts codegen | separate (future) | The org-designated eventual home for kind/status/error-code constants (RFC #3285) — no codegen for these exists today; the lean canvas-local leaf modules are the right first increment (capture-first, enforce-later). |

### 5.3 · molecule-ai-sdk — zero work needed *(audited 2026-07-07)*

- **Verdict: none — this feature lands with zero molecule-ai-sdk commits.** The SDK (external BYO workspaces, `molecule_external_workspace`) treats workspace status as an **opaque string**: it branches only on the `paused`/`deleted` booleans (poll_state / get_peers), never reads concierge or kind='platform' state, never calls the AdminAuth ensure/install endpoints, and its workspace-comms contracts deliberately leave status *values* unpinned. Verified against a fresh shallow clone of the canonical repo.
- **Scope fence:** the idle-prompt design (task #219) carries its own genuinely-new SDK item — the lease-renew ping for external idle detection (no 'lease' surface exists in the SDK today). It stays in #219; not here.
- **Stale-clone warning:** pre-rename clones of the SDK repo (`molecule-sdk-python`, package `molecule_agent`) must not be referenced for new work; the canonical repo is `molecule-ai-sdk`.

## 6 · MOLECULE_SEED_PLATFORM_AGENT — REMOVED *(operator ruling 2026-07-07)*

- **New rule:** the seed runs **unconditionally when `MOLECULE_ORG_ID` is unset** — the same self-host discriminator the boot provision already uses structurally (`prov != nil ⇔ MOLECULE_ORG_ID` unset). No flag. Idempotent: existing root ⇒ no-op. On SaaS (org id set) the tenant server still **never** self-seeds — the CP remains the sole creator; byte-identical to before, where the flag was already unset on every SaaS tenant (`main.go:153-154` contract).
- **Removal surface:** both flag gates in `main.go` (`:156` seed, `:383` boot-provision — the provision keeps its `prov != nil` gate), the compose default `docker-compose.yml:75`, the `Makefile:65` comment, and the e2e contract text in `tests/e2e/test_concierge_creates_workspace_local.sh` (its skip-loud reason changes from "flag unset" to "concierge missing ⇒ genuine bug").
- **Harness/CI unaffected:** tenant-alpha/beta set `MOLECULE_ORG_ID` (`tests/harness/compose.yml:103,188`) ⇒ still never seed; no workflow runs the root compose. Old D1 (compose default flip) and old D2 (dev-start.sh flag) both **dissolve** — remaining doc task: document `MOLECULE_DEFAULT_RUNTIME` + `MOLECULE_LLM_DEFAULT_MODEL` in .env.example/README (previously documented nowhere user-facing).
- **Headless path survives, minus the flag:** env-configured stacks (`MOLECULE_LLM_DEFAULT_MODEL` + matching key in env) converge to online on first boot with zero UI; the scene never renders (G2 sees a configured root). Interactive stacks boot the root unconfigured and the scene finishes it.

> **D2 · Unconfigured-boot posture → RESOLVED by the SSOT audit: skip + stay `offline`; `awaiting_agent` reuse REJECTED.** Skip the boot provision when the root is unconfigured (no resolvable BYOK model/key): log "concierge awaiting setup", status stays `offline`; the scene's G2 gate probes offline + missing MODEL secret/key. Reuse of `awaiting_agent` is ruled out on three concrete state-machine facts: (a) the heartbeat recovery at `registry.go:2205` is **unconditional** (no kind/runtime gate) — the first heartbeat from any provisioned concierge would silently flip it online, machinery lifting the setup gate instead of the user; (b) `cp_instance_reconciler` EXCLUDES awaiting_agent rows from reconcile scope (assumes they're external) — a SaaS concierge parked there goes invisible; (c) canvas + CLI render it as "reconnect the external agent", the wrong fix path. A NEW enum value would need a 043/046-successor migration + models constant in the same PR (drift-test enforced) — not worth it when offline+probe suffices.

> **D3 (open) · MOLECULE_TEMPLATE_REPO_TOKEN:** the platform-agent persona template + platform-MCP plugin live in PRIVATE Gitea repos (`platform_agent.go:187-203`); without the token a self-host concierge provisions but lacks `create_workspace` (documented e2e caveat). Scene v1: show a non-blocking "advanced" note when the server lacks the token. Real fix (public template mirrors / bundled assets) is out of scope here.

## 7 · Production / CP isolation — hard invariants *(operator constraint 2026-07-07)*

> "It must not affect production — how CP works for injecting credentials, metadata and creation."

- **Creation:** the CP installs the concierge via `POST /admin/org/platform-agent` (install, body `{id required, name?, runtime?}`) — **not touched by any workstream item**. The scene uses the separate `/ensure` endpoint, which the CP never calls. The `model` field added to ensure is **optional and additive**: when absent, behavior is byte-identical to before (the SaaS concierge model still resolves via `GET {cp}/cp/tenants/config` → `MOLECULE_LLM_DEFAULT_MODEL`, `platform_agent.go:626-667`).
- **Credential injection:** unchanged. The platform-managed proxy env path (`applyPlatformManagedLLMEnv`: `MOLECULE_LLM_BASE_URL`/`MOLECULE_LLM_USAGE_TOKEN` injection + bypass-key strip) and the concierge server-env wiring (`MOLECULE_API_KEY`/`MOLECULE_ORG_API_KEY`=ADMIN_TOKEN, `platform_agent.go:168-185`) are not modified. The scene writes only tenant-owned data through **existing channels**: `global_secrets` (`PUT /settings/secrets`) and the `MODEL` workspace_secret (same channel `PUT /workspaces/:id/model` uses; `ensureConciergeModel` already respects it, seed-only).
- **Metadata:** `MOLECULE_ORG_ID/SLUG/NAME` and CP-assigned identity are read-only inputs to the scene's gate — never written. Naming: the scene passes `name` explicitly; `defaultPlatformAgentName()` and the CP's own name choice are untouched.
- **Reachability:** the scene renders only when `org_id === ""` (G1) — structurally unreachable on any CP-provisioned tenant, apex, or preview host.
- **Seed-flag removal (§6):** the now-unconditional seed is gated on `MOLECULE_ORG_ID` being unset — structurally impossible on a CP-provisioned tenant (the CP always sets the org id). SaaS boot behavior is byte-identical: the flag was unset there before, the org-id check skips the seed now. The CP's install endpoint remains the sole SaaS creation path.
- **Enum bugfix:** behavior-*restoring*, not behavior-changing — `decideEnsureAction` semantics are identical; the query simply stops erroring at parse time.
- **SSOT consolidation sequencing (§5.1):** the legacy install endpoint stays a byte-identical row-only shim until the CP itself migrates to `/ensure` — a CP-repo change, separately reviewed and released on the CP's schedule. Cleanup deletes the shim only after that lands. At no point in this feature's lifetime does the CP experience a contract change.

## 8 · Error copy — replace raw JSON with states

| Wire condition | Scene copy |
|---|---|
| `422 MODEL_REQUIRED / UNREGISTERED_MODEL_FOR_RUNTIME` | "That model isn't available for \<runtime\> — pick one from the list." (should be unreachable via the picker; guards drift) |
| `MISSING_BYOK_CREDENTIAL` (last_error) | "The API key for \<provider\> is missing or didn't match — re-enter it." → back to step 4 |
| `MISSING_PLATFORM_PROXY` (last_error) | "That model needs Molecule's hosted proxy (not available self-hosted) — pick a bring-your-own-key model." (should be unreachable; `/templates` filters) |
| ensure 500 / network | "Couldn't create the platform agent — \<reason\>." + Retry (never raw `{"error":"lookup failed"}`) |
| provision timeout | Keep progress + "still provisioning — pulling the runtime image can take a few minutes on first boot" (provision-timeout sweep is 12m) |

## 9 · Work breakdown

**Backend (workspace-server):**

- ① Enum bugfix `platform_agent_ensure.go:273` + 11 sqlmock regexes + one real-PG regression test *(prereq, ships alone — PR #3495)*
- ② `model` field on ensure payload — **optional + additive** (absent ⇒ prior behavior; CP never calls ensure, §7): validate (runtime, model) → `setModelSecret` before `triggerPlatformProvision`; extend the ensure e2e. Also closes the §4 **platform-runtime guard gap**: reject external-like/mock/unknown runtimes for kind='platform' with 422 on BOTH AdminAuth endpoints
- ⑤ Remove `MOLECULE_SEED_PLATFORM_AGENT` (§6): unconditional org-id-unset seed in `main.go`, delete both flag gates + compose default + Makefile/e2e references; document `MOLECULE_DEFAULT_RUNTIME` + `MOLECULE_LLM_DEFAULT_MODEL`; D2 unconfigured-boot posture
- ⑦ SSOT consolidation (§5.1): extract `ensurePlatformAgentFlow`; ensure handler + boot seed become thin adapters (deletes `EnsureSelfHostedPlatformAgent` + `MaybeProvisionPlatformAgentOnBoot` top-halves); legacy install endpoint → deprecated row-only shim; file the CP-repo ticket for its /ensure migration

**Canvas:**

- ③ `SelfHostSetupScene` — fullscreen blocking component at root mount (§2), 5 steps (§3), stateless gate, desktop+mobile responsive. Consumes ONLY §5.2-approved SSOT surfaces: `WORKSPACE_KIND`, new `lib/workspace-status.ts` + `lib/workspace-error-codes.ts` (created here), `WS_EVENTS`, `isExternalLikeRuntime`/`runtimeDisplayName`, `/templates` registry fields — zero new hardcoded lists, zero legacy vendor-heuristic deps
- ④ Retire `OnboardingWizard`: `Canvas.tsx:29/:400`, its test file, `SearchDialog.test.tsx:43` STORAGE_KEY, 2 vi.mocks, 4 e2e "Skip guide" clicks, `docs/quickstart.md:101`
- ⑥ Error-state mapping (§8) also backported to the ConciergeShell settings "repair" panel (it stays as the post-setup repair surface)
- ⑧ End-of-feature cleanup: delete the shim once the CP migration lands; sweep replaced boot wrappers, wizard remnants, stale comments (the misleading COALESCE note at `platform_agent_ensure.go:257`), any `MOLECULE_SEED_PLATFORM_AGENT` stragglers (repo-wide grep); refresh docs/quickstart.md + .env.example. Plus the §5.2 canvas SSOT sweep: derive ContainerConfigTab and CreateWorkspaceDialog runtime lists from `/templates`, fold `MonitorPanel.tsx:290`'s raw kind literal; file the Go error-code const block + the ~25-site status-literal migration as follow-up issues

Sequencing: ① alone unblocks the existing button today. ⑦ lands together with ② (the model field is born inside the consolidated flow). ③ is the canvas feature; ④⑤⑥ ride along. ⑧ is the tail phase — shim deletion gated on the CP-side migration ticket. ⑨ (docs, below) rides each PR it describes — not a tail batch. SaaS is untouched throughout — the gate never fires when `org_id` is set, and the shim is contract-frozen.

### 9.1 · Documentation workstream (⑨) — four surfaces *(audited 2026-07-07)*

Per the org DOCUMENTATION_POLICY (internal repo, mandatory): product/feature docs are public-repo; infra-exploitation/strategy/incident material is internal. Doc updates ride the PR that changes the behavior they describe.

| Surface | What changes |
|---|---|
| **molecule-core** (in-repo, rides feature PRs) | `docs/quickstart.md:101` — first-run flow rewritten (wizard → scene) · `docs/frontend/canvas.md:28-48` — the "two onboarding surfaces" section rewritten · `docs/index.md:50` + `docs/architecture/molecule-technical-doc.md:140` — wizard mentions · `docs/design/rfc-platform-agent.md` — amended for the unconditional self-host seed + flow consolidation, and **this spec lands as `docs/design/rfc-selfhost-onboarding-scene`** (the canonical in-repo copy; session-transcript/memory footer refs stripped per doc policy) · **new ADR** — unconditional seed + one-flow consolidation, companion to ADR-002 (which already frames "zero-config OSS onboarding"; the scene completes that story) · `.env.example` + **README.md AND README.zh-CN.md** (bilingual — both) — the two envs, the removed flag, the first-run scene · `Makefile:65` comment + e2e contract text · **⚠ OpenAPI is CI-gated:** the ensure handler has swaggo annotations (`platform_agent_ensure.go:238-244`) — the payload change (model field, new 422s) requires annotation updates + `make openapi-spec` regen committed, or `openapi-spec-check` fails the PR. |
| **molecule-ai-sdk** | **Zero doc changes** — its contracts deliberately leave workspace-status values unpinned and the ensure/install endpoints are not part of its surface. One standing guard: do NOT add the ensure endpoint to workspace-comms contracts (it is AdminAuth core-internal, §5.3). |
| **internal** repo | Nothing stale to fix — grep confirms no runbook mentions the seed flag or concierge bootstrap. ADD: a short ops note (runbooks) for "self-host first-run changed: scene replaces MOLECULE_SEED_PLATFORM_AGENT; headless = the two envs" + the CP-migration ticket tracked to shim deletion (⑧). Optional: a lessons-learned one-pager on the sqlmock/enum class (fits internal, not public). |
| **Public / EnterOS** | The scene IS the EnterOS out-of-box experience: **enter-os-core-prep** README (EN + zh-CN) self-host quickstart gets the "Enter OS Agent" first-run scene (screenshots as the hero); reconcile its **COVERAGE_FLOOR.md** with the §10.1 100%-on-changed-files ruling; **enteros-landing** gets the first-run story/screenshots if it demos onboarding. ⚠ Confirm the core → enter-os-core-prep sync mechanism before writing there, so docs don't fork. |

## 10 · Test & e2e coverage

**Lesson encoded from the enum bug:** the ensure endpoint shipped 100%-broken with green CI because its tests are sqlmock — regex-matching SQL text that never executes, blind to parse-time failures. So the test plan for this feature is layered: **every SQL-shape change gets a real-Postgres leg**, and every fail-closed provision gate gets a live-stack e2e, not just unit mocks.

| Layer | Coverage to add | What it guards |
|---|---|---|
| **Go unit** (workspace-server) | ① Enum fix: re-pin the 11 sqlmock regexes to the bare `SELECT id, status FROM workspaces` shape (mirror `registry_test.go:349-353`). ② Ensure `model`: payload parse/absent-is-noop; 422 `UNREGISTERED_MODEL_FOR_RUNTIME` for a bad (runtime, model); **ordering test** — mock the provision trigger and assert `setModelSecret` committed *before* it fires; extend the existing `decideEnsureAction` table tests (pure function) for the force/repair paths. ⑤ Seed removal: `t.Setenv` matrix — seeds when `MOLECULE_ORG_ID` unset (flag env ignored/absent), never seeds when set; idempotent second boot; D2 skip-provision-when-unconfigured path. | Regex-drift reintroducing COALESCE; the model-vs-provision race; SaaS self-seed regression. |
| **Real-Postgres integration** *(new tier for this file)* | One test that executes the ensure lookup + seed + model-write against a real PG (the dockerized dev DB / harness PG, testcontainers-style) — the test sqlmock structurally cannot provide. Covers: enum scan, partial unique index `uniq_workspaces_one_platform_root` under concurrent ensure, upsert runtime-preservation. | The exact class that shipped broken: parse-time SQL errors invisible to sqlmock. |
| **Canvas unit** (vitest/RTL) | Scene: gate matrix (org_id set ⇒ never renders · unconfigured root ⇒ renders · configured/online ⇒ absent · pre-hydration ⇒ no flash); cascade (runtime change resets provider+model, provider change resets model; no free-text path); fixed name (no input rendered; ensure payload carries `"Enter OS Agent"`); **wire-order test** — mock the api module, assert key-PUT → runtime-PATCH → ensure call order; §8 error-state mapping; resume-from-derived-state at each step. Plus an a11y test (fullscreen blocking scene ⇒ keyboard/focus-trap coverage, `Canvas.a11y.test.tsx` pattern). Cleanup: delete `OnboardingWizard.test.tsx`, un-seed `molecule-onboarding-complete` in `SearchDialog.test.tsx:43`, drop the 2 `vi.mock('../OnboardingWizard')` stubs. | Gate/cascade regressions; ordering that silently reintroduces the race; wizard remnants. |
| **Playwright e2e** (canvas/e2e/) | New `selfhost-onboarding.spec.ts` against the local stack: the full §11 flow (fresh DB → scene → configure → online → "Enter OS Agent" greeting), mid-flow refresh resume, wrong-key → humanized error → retry converges. Update the 4 specs that defensively click "Skip guide" (chat-desktop:51, chat-mobile:46-50, chat-separation:29-33, filestab-smoke:46). `staging-concierge.spec.ts` stays **unmodified-and-green** = the CP-isolation regression proof. | The end-to-end DoD; wizard retirement; SaaS untouched. |
| **Shell e2e** (tests/e2e/) | `test_concierge_creates_workspace_local.sh` contract flips: with the seed unconditional, "no concierge on a local stack" changes from skip-loud (exit 0) to a **hard failure** — the script becomes a genuinely required local gate instead of a silent no-op. Harness compose e2e (template-delivery-e2e.yml) unaffected by construction (org id set) — assert stays green. | Strengthens the only functional local concierge gate; proves harness isolation. |

**Test-env prerequisites (verified against the current tree):** (a) the A/D/H e2e groups provision a *real* concierge — a real provider key is required in the local test env; the §4 platform-runtime guard deliberately excludes `mock` from kind='platform', so there is no mock-concierge shortcut (precedent: the existing concierge e2e's documented key contract). (b) H3 additionally needs `MOLECULE_TEMPLATE_REPO_TOKEN` — unset in the stock local env today (.env/compose/dev-start.sh all lack it), without which the concierge provisions but lacks its platform MCP (D3). (c) Tooling status: canvas coverage is ready (`@vitest/coverage-v8` + CI-shaped vitest.config — only the 100% per-glob thresholds need adding); Go CI already produces `coverage.out` (ci.yml:298) — add the diff-coverage assertion on top; Playwright CI workflows exist to ride (e2e-chat.yml / e2e-staging-canvas.yml pattern).

### 10.1 · Unit coverage ruling — 100% on changed code *(operator 2026-07-07)*

- **Hard gate: 100% line + branch coverage on every NEW or CHANGED file**, both sides. Go: `go test -coverprofile` + a CI diff-coverage check asserting 100% on the touched files (`platform_agent_ensure.go`, the seed/boot helpers, changed secrets/model paths). Canvas: `vitest --coverage` with per-glob `thresholds: {statements/branches/functions/lines: 100}` pinned on the scene module(s) and every file the feature edits. The gate is red below 100 — no soft targets.
- **Testability refactor this forces (good):** the seed decision moves out of `main()` into a pure helper (e.g. `shouldSeedPlatformAgent(orgID string) bool` + a thin boot wiring), because `main()` bodies can't be covered — the 100% rule makes the boot logic unit-addressable as a side effect.
- **No-exception escape hatch:** a line that genuinely cannot execute under test (e.g. process-fatal wiring) needs an explicit exclusion comment + reviewer ack in the PR — silent threshold carve-outs are not allowed.
- **100% is necessary, not sufficient:** the enum bug had passing tests over the exact broken line — text-level mocks count as coverage while proving nothing about execution. The real-PG tier and the e2e matrix below stay mandatory regardless of the unit number.

### 10.2 · Comprehensive e2e matrix

**A · Golden path** *(fresh self-host, interactive)*

| # | Scenario | Proves |
|---|---|---|
| A1 | Fresh DB, first boot ⇒ concierge row auto-seeded (kind=platform, default runtime, status offline/awaiting) — asserted via API before the canvas ever opens | §6 unconditional seed |
| A2 | Canvas loads ⇒ scene renders fullscreen and BLOCKS: nothing behind it clickable/reachable (desktop), no flash before hydration | G1/G2 gate |
| A3 | Welcome shows fixed "Enter OS Agent" — no name input exists in the DOM | brand ruling |
| A4 | Runtime ▾ lists exactly the /templates-derived set (displayable:false absent); Provider ▾ constrained per runtime (codex→openai only, hermes/openclaw→kimi/minimax/gemini BYOK, claude-code→anthropic/kimi/minimax); Model ▾ per provider; zero platform-arm entries; zero free-text inputs; every upstream change resets downstream picks | dropdown-cascade ruling |
| A5 | Configure ⇒ network-level assert of wire order (key PUT → runtime PATCH when changed → ensure{model, name, force}) ⇒ provisioning progress ⇒ `online` ⇒ scene auto-dismisses | §4 sequence |
| A6 | Concierge proactively greets as "Enter OS Agent"; delivered system-prompt.md contains the substituted name, not `{{CONCIERGE_NAME}}`; reload ⇒ scene never renders again | §5.1 handoff + derived-state dismissal |

**B · Resume**

| # | Scenario | Proves |
|---|---|---|
| B1 | Refresh at each step (2/3/4) ⇒ resumes at the correct step purely from server state (key has_value? runtime patched? agent status?) | stateless gate |
| B2 | Refresh during provisioning ⇒ re-enters progress view, not the start of the flow | derived resume |
| B3 | Browser back/forward cannot escape the gate while unconfigured | blocking contract |

**C · Failure**

| # | Scenario | Proves |
|---|---|---|
| C1 | Wrong API key ⇒ provision fails ⇒ humanized MISSING_BYOK_CREDENTIAL copy ⇒ back to key step ⇒ corrected key ⇒ retry converges `online` | §8 mapping + recovery |
| C2 | Ensure network failure ⇒ error + Retry; retry produces NO duplicate root (unique index) and no double provision | idempotency |
| C3 | Double-click configure ⇒ exactly one provision fires (debounce) | debounce |
| C4 | Slow image pull ⇒ progress copy persists past normal wait without false-failing (12m provision-timeout sweep respected) | timeout UX |
| C5 | Platform restart mid-provision ⇒ scene re-derives state on reload and converges (no wedged UI) | crash safety |
| C6 | Direct API abuse: `POST ensure/install {runtime: "external" \| "mock" \| garbage}` ⇒ 422, no row stamped, no wedged external-concierge (the §4 guard) — asserted against both AdminAuth endpoints | platform-runtime guard |

**D · Runtime variants**

| # | Scenario | Proves |
|---|---|---|
| D1 | Full golden path per runtime family — claude-code + codex in the local suite (persona grafting differs: system-prompt.md vs prompts/concierge.md; registry arms differ); hermes/openclaw as smoke | runtime-agnostic concierge |
| D2 | Runtime kept at seeded default ⇒ NO PATCH call issued; runtime changed ⇒ PATCH before ensure | conditional PATCH |

**E · Headless env path**

| # | Scenario | Proves |
|---|---|---|
| E1 | Boot with `MOLECULE_LLM_DEFAULT_MODEL` + matching key in env ⇒ online with zero UI; scene never renders | flagless headless path |
| E2 | Model env set, key missing ⇒ root unconfigured ⇒ scene renders and completes normally | G2 half-configured |
| E3 | No model env, no key ⇒ D2 posture: NO failed provision burned at boot; root waits at offline/awaiting-setup | unconfigured-boot posture |

**F · SaaS / CP isolation**

| # | Scenario | Proves |
|---|---|---|
| F1 | `staging-concierge.spec.ts` green with ZERO edits | §7 regression proof |
| F2 | Boot with `MOLECULE_ORG_ID` set ⇒ DB assert: no self-seed; CP install + credential injection + metadata byte-identical | CP sole-creator invariant |
| F3 | Harness e2e (template-delivery, tenant-alpha/beta) green unchanged | harness isolation |
| F4 | Apex / Vercel-preview host (empty slug, org_id set server-side) ⇒ scene absent | G1 disambiguation |

**G · Post-setup + surfaces**

| # | Scenario | Proves |
|---|---|---|
| G1 | Mobile 390px: scene renders, blocks, and completes (mobile previously had no platform-agent concept at all) | root-mount coverage |
| G2 | Keyboard-only completion + focus trap inside the scene (a11y) | fullscreen a11y |
| G3 | ConciergeShell settings "repair" panel still works post-setup with the §8 error mapping | repair surface intact |
| G4 | Wizard fully retired: "Skip guide" appears in no spec; `molecule-onboarding-complete` never written | retirement |

**H · Data integrity**

| # | Scenario | Proves |
|---|---|---|
| H1 | After the flow: exactly ONE kind='platform' root; MODEL secret == user pick; runtime column == pick; global key has_value; org_api_tokens/plugin-allowlist anchors intact | state correctness |
| H2 | Second boot post-setup ⇒ seed no-ops, no restart storm, concierge stays online | idempotency at boot |
| H3 | `test_concierge_creates_workspace_local.sh` as a HARD gate: the configured concierge actually creates a workspace via its platform MCP (the functional side-effect proof, not a REST 200) | concierge is genuinely functional |
| H4 | Path equivalence: a boot-seeded root, an ensure-created root, and a shim-installed root are field-for-field identical (row, template, anchors, defaults) — one flow, three adapters | §5.1 SSOT consolidation |

SOP note: local stack first with full logs (`make dev` + the shell e2e + docker logs) before any staging validation — unit-green alone has been proven insufficient twice on this exact surface (the enum bug; the runtime#181/182 precedent).

## 11 · Definition of done — e2e

- Fresh self-host, zero env config: **first boot seeds the concierge row unconditionally** (no flag, §6) ⇒ canvas boots ⇒ scene blocks everything ⇒ runtime (PATCH when changed) + provider/model + key ⇒ one configure ⇒ concierge `online` first try ⇒ scene gone ⇒ concierge greets first (idle-prompt §5.1 handoff) **introducing itself as "Enter OS Agent"** (row name → `{{CONCIERGE_NAME}}` substitution proven in the delivered system-prompt.md).
- Headless self-host (`MOLECULE_LLM_DEFAULT_MODEL` + matching key in env): converges to `online` on first boot with zero UI; the scene never renders. The scene's model pick always beats the seed default — a stack never runs `MiniMax-M2.7` unless chosen.
- **CP-isolation regression (§7):** on a CP-provisioned tenant (org id set) the tenant server never self-seeds and the scene never renders; CP install (`POST /admin/org/platform-agent`), credential injection (proxy env + bypass strip), metadata envs, and default naming are byte-identical to pre-change behavior — covered by the existing SaaS/staging e2e staying green with zero test edits on the CP path.
- Seeded-keyless stack (compose legacy): adopt mode — runtime read-only, model+key collected, force-repair ⇒ online. MODEL secret reflects the user's pick, not `MiniMax-M2.7`.
- Mid-flow refresh at every step resumes correctly from derived state (no localStorage anywhere).
- SaaS tenant + Vercel preview + apex: scene never renders (G1 server-declared check).
- All three pickers are dropdowns of available options only: runtime switch re-derives the provider list, provider switch re-derives the model list, downstream picks reset on upstream change, and no free-text path exists — no UI path can submit an invalid (runtime, model) pair.
- **Coverage gates green:** 100% line+branch on every changed file (Go diff-coverage + vitest per-glob thresholds, §10.1) and the full §10.2 e2e matrix (A–H) passing on the local stack, with F-group proving the CP path untouched.
- **SSOT consolidation (§5.1):** exactly one lifecycle flow function exists — grep proves no second decide/install/provision top-half anywhere; boot seed and HTTP ensure are demonstrably adapters over it; the legacy install shim is contract-frozen (F2) with the CP-repo migration ticket filed; cleanup phase ⑧ checklist fully executed (shim deletion pending only the CP migration).
- **SSOT consumption (§5.2):** the scene imports only the approved surfaces — grep proves it introduces no new status/kind/error-code/runtime string literals and no dependency on the legacy vendor heuristic; `lib/workspace-status.ts` and `lib/workspace-error-codes.ts` exist with all wire values + emit-site pointers; the two follow-up issues (Go const block, status-literal migration) are filed.
- Wrong API key: provision fails ⇒ scene surfaces the humanized credential error and returns to step 4; retry converges.
- Old wizard fully gone: no "Skip guide" in any e2e, no `molecule-onboarding-complete` writes.

## 12 · Prior art

Greenfield — no existing fullscreen first-run scene, no prior RFC (docs/design + docs/adr swept). Building blocks reused: `AuthGate` (only blocking-gate precedent), `MissingKeysModal` (closest functional art: provider→model→key collection on template deploy), `ProviderModelSelector` (the provider/model SSOT dropdown chain). Comparable products (Gitea installer, Grafana first-boot, Portainer init) all use the same pattern: blocking first-run scene on self-host, invisible on hosted.

---

**Status:** Designed (2026-07-07, operator-ruled) · prerequisite = ensure-endpoint enum bugfix ([PR #3495](https://git.moleculesai.app/molecule-ai/molecule-core/pulls/3495)) · tracking issue [#3496](https://git.moleculesai.app/molecule-ai/molecule-core/issues/3496).

**Grounding:** read-only multi-agent code sweep of molecule-core @ main `2d1c024e7` (canvas gate/shell · reusable UI · backend contracts · seed-flag interplay) plus a two-agent SSOT/SDK audit (contract-mirror ledger §5.2 · molecule-ai-sdk touchpoints §5.3 · found the platform-runtime guard gap §4 + the ContainerConfigTab runtime-list drift); all file:line references verified against that tree.

**Related:** the consolidated idle-prompt design §5.1 (proactive greeting = the onboarding handoff).

**Rulings 2026-07-07:** gated fullscreen scene · all pickers = cascading dropdowns · agent name FIXED "Enter OS Agent" (no input) · hard CP-isolation constraint (§7) · **MOLECULE_SEED_PLATFORM_AGENT removed — concierge existence unconditional; first/manager agent is always the concierge (§5/§6)** · unit coverage = 100% line+branch on changed files, CI-gated (§10.1) · e2e = full A–H matrix (§10.2) · ONE canonical lifecycle flow, all entrypoints thin adapters; legacy install endpoint shimmed → removed after the CP-repo migration; end-of-feature cleanup phase ⑧ (§5.1).

**Open:** D3 (template-repo token surfacing). D2 (unconfigured-boot posture) was resolved by the SSOT audit — see §6.

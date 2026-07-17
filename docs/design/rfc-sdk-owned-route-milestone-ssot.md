# RFC: SDK-owned route + milestone SSOT with a codegen derive-gate (closes the #87/#88 drift class)

**Status:** proposed (awaiting CTO sign-off to build) · **Author:** CEO-assistant · **Date:** 2026-07-16

> Scope note: this is a **proposal for sign-off**, not a landed design. The CTO
> ruled "Full SSOT (arch RFC)" for this drift class; this doc specifies it and
> asks for the specific decisions in §10. No generator, gate, or contract field
> is built yet.

## 1. Summary & the decision requested

Two guards that are supposed to keep the E2E contract honest each derive their
"truth" from the **wrong repo**, and both drifts are structural, not bugs anyone
introduced:

- **#87 — the route/header contract guard reads CORE, not the SDK.**
  `tests/e2e/lib/assert_e2e_tenant_contract.py` proves the e2e scripts speak a
  contract *core itself implements* by regex-parsing core's own router and
  walking core's own `*.go` for header reads. The SDK — the nominal producer of
  the register/heartbeat/A2A contract — is never read. Core is therefore the
  de-facto route SSOT and the SDK "blesses" nothing; the guard **cannot** detect
  SDK↔core divergence because it never opens an SDK file.

- **#88 — the happy-path required-milestone set is a hand-maintained bash
  string.** The SDK declares exactly four happy-path milestones
  (`contracts/happy-path/happy-path.contract.json`), but the runner
  (`tests/e2e/test_staging_full_saas.sh`) hardcodes those four as a literal
  `required=` string and *proves ~six more live stages that stamp no milestone
  at all*. The hand-list is the drift: widen the runner and the required set
  silently stays at four.

**The proposal (Full SSOT):** give the SDK a machine-readable **route+header
descriptor** for the endpoints it legitimately owns (registry + A2A), promote
the happy-path milestone list to a **generated Go binding** the runner derives
from, have core **vendor** both bindings the way it already vendors
`molcontracts`, and add a **codegen derive-gate** that REDs CI when core's router
or the runner's required-milestone set diverges from the SDK-declared set. This
replaces both the hand-maintained bash `required=` and the core-only route regex
as the *source* of truth, while keeping the existing core-source guard as a
complementary positive-presence check.

**Decisions requested (detail in §10):**

1. Contract shape for routes: per-endpoint `"route"` field on the existing
   `workspace-comms/*.contract.json` **vs** a single `routes.manifest.json`.
2. The SDK-owned route scope boundary (registry + A2A only) — confirm the full
   ~50-route tenant surface is an explicit **non-goal**.
3. Which #88 candidate milestones are **promoted** (load-bearing, required) vs
   kept **optional** (read-only smoke).
4. Enforcement sequencing: approve capture-first-then-enforce so the derive-gate
   lands green before it can jam the `['*']` branch-protection meta-gate.

---

## 2. What exists today (grounding, not proposal)

Every claim below is against `molecule-core` main and `molecule-ai-sdk` main as
of this RFC's date.

### 2.1 The #87 guard derives from CORE, and cannot see the SDK

`tests/e2e/lib/assert_e2e_tenant_contract.py` runs three checks — routes, raw
`$TENANT_URL` curls, and `X-*` headers — all against **core source**:

- Routes come from **one core file**:
  `ROUTER = workspace-server/internal/router/router.go`
  (`assert_e2e_tenant_contract.py:57`). It regex-parses gin `.Group(...)`
  prefixes and `.GET/.POST/...` registrations (`:61-63`, `registered_paths()`
  `:98-110`).
- Headers come from **walking all of core**: `for go in WS_SERVER.rglob("*.go")`
  (`:173`), collecting `GetHeader`/`Header.Get` reads (`:83`) plus the router's
  CORS `AllowHeaders` allow-list (`:86`, and the declaration itself at
  `router.go:60`).
- It has two self-checks that hard-exit `2` if the parse looks broken: fewer than
  **50 routes** (`:247-249`) or fewer than **10 headers** / missing
  `x-workspace-id` (`:255-259`).

It is invoked in `.gitea/workflows/ci.yml`, job `shellcheck`
(`ci.yml:458`), at `ci.yml:525`, gated to run when **scripts OR platform**
changed (`ci.yml:503`, `:505`). Job is `continue-on-error: false`
(`ci.yml:463`), and core `main` carries the intentional `['*']` branch-protection
meta-gate — so a red here blocks merge on any scripts/platform PR.

**Consequence.** The guard is a genuinely valuable *positive-presence* check
(it has already caught a dead `/activity?workspace_id=` URL and a
`X-Source-Workspace-Id` header nobody reads — see the file's own docstring
`:13-47`). But because both its route set and its header set are *core's own
source*, it can only answer "do the scripts match **core**?" It structurally
**cannot** answer "does **core** match the **SDK**?" — the question #87 is really
about. There is today **no SDK route file for it to point at**; that is the gap.

### 2.2 The #88 required-milestone set is a hand-maintained bash string

The runner stamps load-bearing lifecycle milestones via `live_milestone()`
(`test_staging_full_saas.sh:190`) and asserts them in `require_live_or_die()`
(`:197`), whose required set is a **literal string**:

```
local required="provisioned tenant_online workspace_online a2a_roundtrip"   # :200
```

Those four are stamped at `:522` (provisioned), `:615` (tenant_online), `:1107`
(workspace_online), `:1837` (a2a_roundtrip) — and they are **byte-identical** to
the four ids in the SDK contract (`happy-path.contract.json`). So far, agreement
— but by *coincidence of hand-maintenance*, not derivation.

The drift is what the runner **also proves but never stamps**. In full mode the
same script exercises, with real assertions, at least six further live stages:

| Proven stage (full mode) | Runner site | Milestone stamped? |
|---|---|---|
| HMA memory write+read round-trip | `:1975-2022` (step 9) | **none** |
| Peer discovery (`/registry/:id/peers`) | `:2024-2045` (step 9b) | **none** |
| Activity-log route (`/workspaces/:id/activity`) | `:2047-2077` | **none** |
| Workspace KV memory Edit (if-match version) | `:2079-2100` (step 9c) | **none** |
| Delegation provenance (child records parent as source) | `:2161-2302` (step 10) | **none** |
| Cascade guard (parent pause refused while child live → 409) | `:2354-2363` | **none** |
| Lifecycle pause→resume→online | `:2389-2404` (step 10b) | **none** |
| Lifecycle hibernate→auto-wake→online | `:2407-2475` (step 10b) | **none** |

These stages hard-`fail` when broken, so they are real coverage — but
`require_live_or_die` neither knows nor requires them. Add a new proven stage and
the required set does not move; delete a stamp for one of these (there is none to
delete) and nothing notices. **The hand-list IS the drift.** The SDK's own
`happy-path/README.md` already anticipates exactly this fix ("add a milestone
here, then the consumer's `require_live_or_die` picks up the new required id …
core reads the embedded milestone set instead of hand-maintaining the
`require_live_or_die` list — a codegen-drift gate keeps them in lockstep") — but
that is written as aspiration; the mechanism does not exist yet.

### 2.3 What the SDK owns today, and how core already vendors it

The SDK **does** own the workspace-comms contract bodies — but only the *request/
response payload shapes*, never the *route*:

- `contracts/workspace-comms/register.contract.json`,
  `.../heartbeat.contract.json`, `.../a2a-envelope.contract.json` each capture a
  canonical `request`/`response` **body**. None of them, nor their schemas,
  carry a `method`/`path`/`route` field (verified: zero hits for `"route"`,
  `"method"`, `"path"` in the register schema).
- `contracts/happy-path/happy-path.contract.json` declares the four milestone
  `id`s + a pointer to the runner (`runner.path`, `assertion_fn`,
  `milestone_fn`).

The codegen + vendoring machinery this RFC reuses **already exists and is
proven**:

- The SDK's `tools/gen-go.mjs` emits byte-stable Go under
  `gen/go/molcontracts/` — `contract_gen.go`, `workspace_comms_gen.go`,
  `schema_assets_gen.go`, `provision_request_gen.go` — with a `DO NOT EDIT`
  header and a codegen-drift CI gate that re-runs the generator and fails on any
  diff (per RFC molecule-core#3285 §14).
- Core consumes these as a Go module: `workspace-server/go.mod:25` pins
  `go.moleculesai.app/sdk/gen/go`, and core imports the package directly, e.g.
  `manifest_ssot.go:11,37` uses `molcontracts.PluginManifestSchemaJSON`.
- The **exact analog** already ships: the concierge management-MCP degrade gate
  derives its required verb from the SDK — `molcontracts.RequiredTool` /
  `molcontracts.MCPServerName` consumed in
  `workspace-server/internal/staginge2e/platform_agent_mgmt_mcp_gate*.go`, with a
  cross-repo drift gate (`.gitea/workflows/mcp-plugin-delivery-contract-drift.yml`)
  comparing participants against the SDK SSOT. **This RFC is that pattern applied
  to routes + milestones.**

---

## 3. Goals / non-goals

**Goals**

- G1. Make the **SDK** the declared authority for the routes it legitimately owns
  (registry + A2A), in a machine-readable form.
- G2. Make the **SDK** the derivation source for the happy-path
  required-milestone set — the runner derives, never hand-lists.
- G3. A **codegen derive-gate** that REDs CI when core's router or the runner's
  required set diverges from the SDK-declared set.
- G4. Land it **without jamming** the `['*']` branch-protection meta-gate
  (capture-first-then-enforce).

**Non-goals (explicit)**

- **N1. The SDK does NOT enumerate the full ~50-route tenant surface.** Core's
  router registers ~210 method/path lines across admin, workspace-auth, events,
  monitor, templates, bundles, org-tokens, allow-lists, etc.
  (`router.go` groups at `:182,:273,:427,:482,:495,:683,:700,:843,:900,:955,:982,:999`).
  Declaring all of them in the SDK would **invert producer-is-SSOT for the entire
  tenant HTTP layer** — those routes are core's product surface, not a
  cross-boundary contract. The SDK owns only the endpoints that are genuinely a
  contract *between* the runtime/SDK and core: the registry lane
  (`/registry/register`, `/registry/heartbeat`, `/registry/update-card`,
  `/registry/discover/:id`, `/registry/:id/peers`, `/registry/check-access` —
  `router.go:461-474`) and the A2A lane (`/workspaces/:id/a2a`,
  `/workspaces/:id/a2a/inbound`, `/workspaces/:id/a2a/queue/:queue_id` —
  `router.go:256,260,267`). A full-surface route SSOT, if ever wanted, is a
  **separate RFC** with its own producer-ownership argument.
- **N2. Do not delete or weaken the existing core-source guard.** It stays as the
  complementary positive-presence check (§5).
- **N3. No method/auth/param semantics in the route descriptor.** Same scope
  discipline the current guard keeps (`assert_e2e_tenant_contract.py:45-47`):
  method + path routability only, not header *values*, not auth, not query
  params.
- **N4. Not a runtime-behaviour change.** Per ADR-004, core carries zero runtime
  behaviour; this RFC only moves *where the contract is declared* and *what CI
  checks*, not what the server does at runtime.

---

## 4. Target architecture

### 4.1 An SDK-owned route+header descriptor (the endpoints the SDK owns)

Add, for the registry + A2A endpoints only, a machine-readable route descriptor
in the SDK. Two candidate shapes (decision D1, §10):

**Option A — per-endpoint `route` on the existing contract files.** Extend each
`workspace-comms/*.contract.json` with a `route` object and update its schema:

```jsonc
// contracts/workspace-comms/register.contract.json  (additive)
{
  "route": { "method": "POST", "path": "/registry/register" },
  "request":  { /* … unchanged … */ },
  "response": { /* … unchanged … */ }
}
```

Pros: the route lives beside the body it belongs to; one file per endpoint is the
existing organizing principle. Cons: the descriptor is spread across files; the
A2A queue/discover/peers endpoints have no body-contract file today and would
need a home.

**Option B — a single `workspace-comms/routes.manifest.json`** listing the
SDK-owned endpoints:

```jsonc
{
  "schema_version": "workspace-routes/v1",
  "owned_scope": "registry+a2a",           // documents N1 in-band
  "routes": [
    { "id": "register",       "method": "POST", "path": "/registry/register" },
    { "id": "heartbeat",      "method": "POST", "path": "/registry/heartbeat" },
    { "id": "update_card",    "method": "POST", "path": "/registry/update-card" },
    { "id": "discover",       "method": "GET",  "path": "/registry/discover/:id" },
    { "id": "peers",          "method": "GET",  "path": "/registry/:id/peers" },
    { "id": "check_access",   "method": "POST", "path": "/registry/check-access" },
    { "id": "a2a_send",       "method": "POST", "path": "/workspaces/:id/a2a" },
    { "id": "a2a_inbound",    "method": "POST", "path": "/workspaces/:id/a2a/inbound" },
    { "id": "a2a_queue",      "method": "GET",  "path": "/workspaces/:id/a2a/queue/:queue_id" }
  ],
  "headers": [ "X-Workspace-ID", "X-Molecule-Org-Id" ]   // the contract-lane headers only
}
```

Pros: one file, one `owned_scope` field making the N1 boundary explicit and
gate-checkable; a clean home for the routes without body files. Cons: a second
place the route strings live (mitigated: the derive-gate is exactly what keeps it
honest). **Recommendation: Option B** — the explicit `owned_scope` boundary and
single derive target are worth more than physical adjacency, and it avoids
inventing body files for discover/peers/queue.

Path syntax note: gin patterns use `:param`/`*wild`; the descriptor stores the
gin form verbatim so the gate can reuse the existing `matches()` gin semantics
(`assert_e2e_tenant_contract.py:117-144`).

### 4.2 A generated Go binding (`gen-go` emits it, core vendors it)

Extend the SDK `tools/gen-go.mjs` `molcontracts` block to emit **two** new
byte-stable bindings, exactly like `workspace_comms_gen.go` /
`schema_assets_gen.go` today:

- `gen/go/molcontracts/routes_gen.go` — an ordered `[]Route{Method,Path}` (+ the
  contract-lane header names) derived from §4.1.
- `gen/go/molcontracts/happy_path_gen.go` — the ordered milestone `id` slice (+
  `order`, `summary`) derived from `happy-path.contract.json`. (The SDK
  `happy-path/README.md` already names this file and this plan.)

Both carry the `Code generated … DO NOT EDIT` header and fall under the SDK's
existing codegen-drift CI (`gen/` is never hand-edited). Core picks them up
through the existing module pin (`workspace-server/go.mod:25`,
`go.moleculesai.app/sdk/gen/go`) — no new vendoring mechanism.

### 4.3 The derive-gate (what actually closes the drift)

Two new checks in core, both deriving from the vendored bindings:

- **Route derive-gate** (closes #87). A Go test in `workspace-server` asserts
  that **every** `molcontracts.SDKRoutes` entry is registered in core's router.
  Reuse the guard's existing gin-aware `registered_paths()` logic (port the
  three regexes / `matches()` to Go, or shell out to the Python parser). RED if
  the SDK declares a registry/A2A route core does not serve, or serves under a
  different method — i.e. the SDK↔core divergence the current guard is blind to.
- **Milestone derive-gate** (closes #88). Replace the hand-list at
  `test_staging_full_saas.sh:200` with a value **derived from the binding**. Two
  wiring options (decision D3-impl):
  - (i) a small Go/`go run` helper prints the required ids from
    `molcontracts.HappyPathMilestones`, and the runner reads that at start; or
  - (ii) a **static test** (`tests/e2e/*_unit.sh` wired into `ci.yml` — note the
    #112 vacuity class: e2e unit scripts need an explicit `ci.yml` line, they are
    **not** globbed) asserts the runner's `required=` string is exactly the
    binding's promoted-milestone id list. (ii) keeps the runner dependency-free at
    live time; (i) removes the string entirely. **Recommendation: (ii)** for the
    first increment (no runtime coupling added to the live path), migrating to
    (i) once the promoted set is stable.

The route derive-gate is the SSOT-inversion: **the SDK declares, core is
checked against it.** The milestone derive-gate makes "widen the happy path" a
one-place edit in the SDK contract, as its README already promises.

---

## 5. What #87 becomes (reframe)

#87 as literally worded — "repoint the guard at the SDK" — is **impossible
today**: there is no SDK route file to point at (§2.3). Reframe it to:

> **#87′: Add an SDK route authority (§4.1) + a route derive-gate (§4.3) so
> SDK↔core route divergence is caught in CI.**

Crucially, the existing core-source guard **stays**. It answers a *different,
still-needed* question — "do the e2e scripts call paths/headers core actually
serves?" — and it has live catches to its name. After this RFC there are two
complementary checks:

| Check | Question it answers | Source of truth |
|---|---|---|
| Existing guard (`assert_e2e_tenant_contract.py`) | Do the **e2e scripts** speak what **core** implements? | core router + core `*.go` (positive presence) |
| New route derive-gate (§4.3) | Does **core** implement what the **SDK** declares (registry+A2A)? | SDK route descriptor (SSOT) |

The guard's `<50`-route and `<10`-header self-checks
(`assert_e2e_tenant_contract.py:247-259`) are untouched — they guard against a
broken *parser*, orthogonal to SDK authority.

---

## 6. Milestone candidate set for #88

Candidate new milestones, each already a real proven stage (§2.2). Recommendation
on promote (required, load-bearing) vs optional (read-only smoke):

| Candidate id | Proves | Runner site | Recommendation |
|---|---|---|---|
| `memory_online` | HMA memory write+read round-trips (mutation + read-back) | `:1975-2022` | **Promote** — load-bearing: it is a write path, and its 503 on staging is what historically aborted the run before later steps (docstring `:23-27`). A real mutation that must hold. |
| `delegation_provenance` | child activity records the delegating parent as `source_id` | `:2161-2302` | **Promote** — load-bearing cross-workspace write; the `X-Workspace-ID`→`source_id` contract is exactly the class the header guard exists for. |
| `cascade_guard` | parent pause refused (409 + descendant list) while a child is live | `:2354-2363` | **Promote** — a negative-gate on a destructive op; a regression strands running children. Load-bearing safety invariant. |
| `lifecycle_pause_resume` | pause→paused→resume→online (real CP stop + re-provision) | `:2389-2404` | **Promote** — a real state-machine mutation with DB-verified settling. |
| `activity_logged` | `/workspaces/:id/activity` returns 2xx + parseable JSON | `:2047-2077` | **Optional** — read-only smoke; it deliberately does **not** assert count>0 (`:2051-2055`). Keep as optional/observed, not required. |
| `lifecycle_hibernate_wake` | hibernate→hibernated→auto-wake-on-A2A→online | `:2407-2475` | **Promote — but see caveat below.** |

**Hibernate-wake caveat (hard dependency on #92).** Today step 10b hibernates an
**idle** leaf with `POST /hibernate?force=true` (`:2407`) — it proves force-
hibernate-of-idle + auto-wake, **not** force-on-busy. The milestone, if promoted,
must ship with an **idle-only** `summary` ("force-hibernate of an idle workspace
then auto-wake on the next A2A"). It **must not** be promoted with a
"force-on-busy" summary until the hibernate/queue force-on-busy work (#92 /
design-Q #124 — a queued A2A can survive force-hibernate and re-wake the ws)
lands and the runner adds a busy-path assertion. Promoting a force-on-busy claim
the runner does not prove would re-introduce exactly the "assertion that can
never hold reads green" failure the #87 guard was built to kill.

`workspace_kv_edit` (step 9c, `:2079-2100`) is a further candidate; recommend
**optional** in increment 1 (it is a narrower sub-case of `memory_online`).

Net: promote `memory_online`, `delegation_provenance`, `cascade_guard`,
`lifecycle_pause_resume`, `lifecycle_hibernate_wake` (idle-only summary); keep
`activity_logged` (+ `workspace_kv_edit`) optional. Final promote/optional split
is **decision D3**.

---

## 7. Migration / sequencing (capture-first-then-enforce)

The failure mode to avoid: a botched repoint REDs the `shellcheck` job, which is
`continue-on-error: false` (`ci.yml:463`) under core-main's `['*']`
branch-protection meta-gate — so a broken new gate would **merge-jam every PR
touching scripts or platform**. Stage it so the gate is proven green before it
can block:

- **Phase 0 — capture (SDK, doc-only + additive contract).** Land the route
  descriptor (§4.1) and add `routes_gen.go`/`happy_path_gen.go` to `gen-go`.
  Additive to the SDK; emits new files; the SDK's own codegen-drift gate proves
  byte-stability. No core behaviour, no core gate yet.
- **Phase 1 — vendor + shadow (core, non-blocking).** Bump core's
  `go.moleculesai.app/sdk/gen/go` pin so the bindings are importable. Add the
  route derive-gate and milestone derive-gate as **`continue-on-error: true`**
  (or a standalone workflow NOT in branch protection — the exact pattern
  `mcp-plugin-delivery-contract-drift.yml` uses: "standalone workflow, NOT a job
  in ci.yml and NOT in branch protection; soak-then-promote"). Soak it green on
  main for real PRs; a red here is advisory (but still RCA'd — an advisory
  failure is still a failure).
- **Phase 2 — stamp the promoted milestones (core, runner).** Add
  `live_milestone` calls at each promoted stage (§6) and switch
  `require_live_or_die`'s required set to derive from the binding (§4.3). Because
  the milestones are stamped at stages the runner **already reaches and asserts**,
  a correctly-wired derive changes nothing at green — it only removes the
  false-green-on-skip hole. Prove the live staging run stamps the full promoted
  set before promoting the gate.
- **Phase 3 — promote to required.** Flip the derive-gate to
  `continue-on-error: false` and add it to branch protection, once it has soaked
  green. Only now can it jam — and by construction it is green.

This mirrors the RFC-#3285 codegen-drift and the mcp-plugin-delivery soak-then-
promote pattern already in the tree, so it is a known-safe rollout shape.

---

## 8. Invariants to preserve (regression contract)

- **I1.** The existing core-source guard keeps running and keeps its self-checks
  (`:247-259`). This RFC adds authority; it removes none.
- **I2.** Flag/gate-off (Phase 0–1) is **byte-identical** at green: shadow gates
  cannot change a merge outcome until Phase 3.
- **I3.** A promoted milestone must correspond to a stage the runner **actually
  proves with a hard `fail`** — never a milestone stamped by reaching a
  tautological point (the milestone-gate-is-a-noop class). Each promotion in §6
  cites the asserting site.
- **I4.** The SDK route descriptor stays within `owned_scope` (registry+A2A,
  N1); the derive-gate must reject an attempt to add a non-owned core route to
  the SDK descriptor (so the boundary can't erode by accretion).
- **I5.** No header *values*, auth, or params enter the descriptor (N3).

---

## 9. Risks

- **R1. Two homes for the registry/A2A route strings** (SDK descriptor + core
  router). Mitigation: that is precisely what the derive-gate reconciles — the
  drift becomes a CI red, not a silent divergence. This is the same "two
  deliberately-identical copies + a drift gate" model already used for
  `mcp-plugin-delivery`.
- **R2. Gin pattern fidelity.** The descriptor stores raw gin patterns; the gate
  must honour `:param`/`*wild` (reuse `matches()` semantics,
  `assert_e2e_tenant_contract.py:117-144`) or it will false-alarm on
  `/files/*path`-style routes.
- **R3. Milestone derive wiring vacuity** (#112 class). If the milestone
  derive-gate is a `tests/e2e/*_unit.sh`, it needs an **explicit `ci.yml` line**
  — these are not globbed. Negative-control it (make the runner's `required=`
  wrong on a scratch branch and confirm the gate REDs) before trusting it.
- **R4. Scope creep toward full-surface routes.** Guard with I4; a full route
  SSOT is a separate RFC.
- **R5. Hibernate-wake over-claim** (§6 caveat) — mitigated by the idle-only
  summary and the #92 dependency.

---

## 10. Decisions requested (CTO sign-off)

- **D1 — Route descriptor shape.** Option A (per-endpoint `route` on
  `workspace-comms/*.contract.json`) vs Option B (single
  `workspace-comms/routes.manifest.json` with an `owned_scope` field).
  *Author recommends B.*
- **D2 — Owned scope.** Confirm the SDK route authority is **registry + A2A
  only** (the nine endpoints in §4.1) and that the full ~50-route tenant surface
  is an explicit non-goal / separate RFC (N1). *Author recommends confirm.*
- **D3 — Milestone promote/optional split.** Confirm promote =
  {`memory_online`, `delegation_provenance`, `cascade_guard`,
  `lifecycle_pause_resume`, `lifecycle_hibernate_wake` (idle-only summary)},
  optional = {`activity_logged`, `workspace_kv_edit`}. *Author recommends as
  listed; hibernate-wake gated on §6 caveat.*
- **D4 — Enforcement sequencing.** Approve capture-first-then-enforce (§7):
  shadow/non-blocking through Phase 1–2, promote to branch-protection-required
  only after a green soak. *Author recommends approve.*

## 11. Open questions

- **Q1.** Milestone derive mechanism: static assertion on the runner's
  `required=` string (no live-path coupling) vs runner reads ids from a `go run`
  helper at start (removes the string entirely). §4.3 leans static-first; confirm.
- **Q2.** Should the route derive-gate be a Go test in `workspace-server`
  (compiled, vendored binding) or a standalone cross-repo workflow like
  `mcp-plugin-delivery-contract-drift.yml` (raw-fetch compare)? The former binds
  to the pinned module version; the latter checks live main-vs-main. Possibly
  both (compiled gate for the pin, cross-repo gate for freshness).
- **Q3.** Do `discover`/`peers`/`check-access` belong in the *route* descriptor
  only, or should they also get body contracts (they have none today)? Routes-only
  is enough to close #87; body contracts are a separate widening.
- **Q4.** Where does the SDK-owned **header** set stop? The contract lane uses
  `X-Workspace-ID` (+ `X-Molecule-Org-Id`); the router's CORS list
  (`router.go:60`) also declares `X-Molecule-Org-Slug`, `X-Confirm-Name`, etc.
  which are edge/routing metadata, not SDK-owned. Recommend the SDK descriptor
  declares only the two contract-lane headers and leaves the rest to the existing
  core-source guard.

---

## Appendix A — file:line index (grounding)

**molecule-core** (main):
- `tests/e2e/lib/assert_e2e_tenant_contract.py` — route source `:57`; regexes
  `:61-63,:80-88`; `registered_paths()` `:98-110`; `matches()` gin semantics
  `:117-144`; header walk `:173`; self-checks `:247-259`.
- `.gitea/workflows/ci.yml` — `shellcheck` job `:458`; `continue-on-error:false`
  `:463`; scripts/platform gate `:503,:505`; guard invocation `:525`.
- `tests/e2e/test_staging_full_saas.sh` — `live_milestone` `:190`;
  `require_live_or_die` `:197`; hardcoded `required=` `:200`; stamps `:522,:615,
  :1107,:1837`; unstamped proven stages `:1975,:2024,:2065,:2079,:2161,:2354,
  :2389,:2407`; idle `?force=true` hibernate `:2407`.
- `workspace-server/internal/router/router.go` — CORS AllowHeaders `:60`; group
  prefixes `:182,:273,…,:999`; A2A routes `:256,:260,:267`; registry routes
  `:461-474`.
- `workspace-server/go.mod:25` — `go.moleculesai.app/sdk/gen/go` pin.
- `workspace-server/internal/plugins/manifest_ssot.go:11,37` — `molcontracts`
  consumption example.
- `workspace-server/internal/staginge2e/platform_agent_mgmt_mcp_gate*.go` —
  `molcontracts.RequiredTool`/`MCPServerName` derive-gate precedent.
- `.gitea/workflows/mcp-plugin-delivery-contract-drift.yml` — soak-then-promote
  cross-repo drift-gate precedent.

**molecule-ai-sdk** (main):
- `contracts/happy-path/happy-path.contract.json` — 4 milestones + runner
  pointer; `happy-path/README.md` — the gen-go/derive plan (already written as
  aspiration).
- `contracts/workspace-comms/{register,heartbeat,a2a-envelope}.contract.json` —
  body-only contracts, **no** `route`/`method`/`path` field.
- `tools/gen-go.mjs` — `molcontracts` emit block (`gen/go/molcontracts/*_gen.go`,
  byte-stable, codegen-drift gated).

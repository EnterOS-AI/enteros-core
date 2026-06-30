# Fail-closed BYOK billing

**Status:** Proposal — CTO (王泓铭)-refined 2026-06-05.
Owners: hongming (CTO)
Base: molecule-core main @ `1955fdd0` (2026-06-04)

This RFC formalizes the **fail-closed BYOK billing** model: the contract that a
workspace which intends to run an LLM on the tenant's own credential
(bring-your-own-key) must be **rejected at the create API** if that credential is
missing or dead — loudly, comprehensively, and synchronously — never created and
then wedged at provision time, and never silently fell-through to a
platform-billed default.

It writes down the four hard requirements, audits the current implementation
against them (two are met today, one partial, one missing), and specifies the
two gaps to close. The derive-from-model SSOT and the platform proxy boundary are
**non-goals** here — this RFC is only about closing the credential-validation
holes around the provider derivation.

> **Update 2026-06-30:** the per-workspace `llm_billing_mode` field and its
> resolver (`ResolveLLMBillingModeDerived`) + operator-override column were
> removed. The platform-vs-BYOK decision is now a pure derivation from the
> workspace's selected `(runtime, model)` via the provider registry
> (`providers.DeriveProvider` → `IsPlatform`): a workspace routes to the metered
> proxy iff its resolved provider is the closed `platform` arm, else it is BYOK.
> The fail-closed BYOK contract below is unchanged in intent; "mode == BYOK" now
> reads "resolved provider is a specific vendor (not platform)", and there is no
> override escape hatch — the model selection alone decides.

## TL;DR

```
create API request (runtime, model)
        │
        ▼
  derive provider from providers.yaml registry SSOT        ── Req1 MET today
  (DeriveProvider; no override, no billing-mode flag)
        │
        ├─ provider IsPlatform ───────────────────► create OK (proxy bills)
        │
        └─ provider is a specific vendor (BYOK)
              │
              ├─ GAP A: credential PRESENT for the derived provider?
              │         (no → 422 MISSING_BYOK_CREDENTIAL, synchronous, loud)
              │
              ├─ GAP B: credential VALID? (cheap authed provider call;
              │         401/403 → 422 INVALID_BYOK_CREDENTIAL, loud)
              │
              ▼
        create OK → provision (re-checks presence as defense-in-depth)
```

## The model — four hard requirements

1. **Explicit selection drives the adapter.** The provider is *selected*, never
   guessed. The selection is **derived deterministically** from the chosen model
   via the `providers.yaml` registry SSOT (`DeriveProvider(runtime, model,
   availableAuthEnv)`). There is no heuristic fallback to a vendor and no
   billing-mode override — the model selection alone decides platform vs BYOK.

2. **BYOK requires the credential, validated AT CREATION, fail-closed.** A
   BYOK workspace with no usable credential for the derived provider must be
   **REJECTED at the create API** with a clear, comprehensive error (which
   credential / env var, which provider, what to do). It must NOT be created
   (201) and then wedged late at provision.

3. **Preflight-validate the credential is VALID, not just present.** Presence is
   necessary but not sufficient: a present-but-dead token (revoked, expired,
   wrong-scope) must be caught by a *cheap authenticated provider call* (a
   models-list or a 1-token completion) and the workspace rejected on 401/403
   before it goes live.

4. **Fail LOUD, never silent.** Any missing / invalid / rejected credential
   errors loudly: comprehensive server logs (provider, env var, code, workspace)
   plus a user-visible structured reason. It must NEVER silently fall through to
   `platform_managed` or to any default that bills the platform for what the
   tenant declared as BYOK.

## Current-state audit

References are `path:line` at base `1955fdd0`. Workspace-server paths are relative
to `workspace-server/`; the proxy/charge layer lives in the controlplane repo.

### Req1 — Explicit selection drives the adapter — **MET**

- `internal/handlers/workspace_provision.go` `applyPlatformManagedLLMEnv` +
  `internal/handlers/provider_derive_helpers.go` — the provider is derived from
  `(runtime, model)` via the embedded `providers.yaml` registry
  (`manifest.DeriveProvider`). A specific non-platform vendor → BYOK; the closed
  `platform` provider → route to the metered proxy. No override, no guessing.
- `internal/handlers/workspace.go:420-503` — create-time validation already
  hard-rejects (422) an unregistered `(runtime, model)` pair
  (`UNREGISTERED_MODEL_FOR_RUNTIME`) and a model whose derived provider is absent
  from the catalog (`DERIVED_PROVIDER_NOT_IN_REGISTRY`), and requires an explicit
  model (`MODEL_REQUIRED`). The selection input is validated against the SSOT at
  the boundary.

### Req4 — Fail loud, never silent — **MET**

- Default-closed on ambiguity: `internal/handlers/workspace_provision.go`
  `applyPlatformManagedLLMEnv` — an underivable provider (no model / unknown
  runtime / unregistered / ambiguous / registry-load failure) routes to the
  platform proxy *only when a proxy is wired* (`PlatformManagedProxyConfigured`),
  else BYOK on self-host; a derived specific vendor is never silently flipped to
  platform to bill the tenant by surprise.
- Proxy is platform-managed-only: controlplane `internal/handlers/llm_proxy.go:94,
  158,223,664-748` — the platform LLM proxy only serves platform-managed traffic;
  BYOK never routes through it.
- Charge layer never bills the platform for BYOK: controlplane
  `internal/credits/llm_billing.go:156-233` — BYOK usage is not charged to the
  platform ledger.

### Req2 — Credential validated at creation, fail-closed — **PARTIAL**

- The fail-closed BYOK check EXISTS but only at **provision** time:
  `internal/handlers/workspace_provision_shared.go` — if
  `!RoutedToPlatform && !HasUsableLLMCred`, the provisioner aborts with
  `MISSING_BYOK_CREDENTIAL` (molecule-core#1994).
- Gap: a credential-less BYOK **create** returns **201** and only fails later at
  provision. That violates Req2's "rejected at the create API, not
  created-then-wedged" — the user gets a workspace row and a delayed, async
  failure instead of a synchronous 4xx.

### Req3 — Credential is VALID, not just present — **MISSING**

- `HasUsableLLMCred` is **presence-only**:
  `internal/handlers/workspace_provision.go:1138-1145` —
  `hasAnyPlatformManagedLLMKey` returns true if any auth-env key is a non-empty
  string. There is **no liveness probe anywhere** — a present-but-revoked token
  passes every gate and the workspace goes live, then wedges at first real LLM
  call (the failure Req3 exists to pull forward).

## Scope of work — the two gaps

### Gap A (Req2): BYOK credential-presence check at the CREATE boundary

Add a synchronous presence check inside the create handler
(`(h *WorkspaceHandler) Create`, `internal/handlers/workspace.go:242`), after
provider derivation and the existing registry validation, **in addition to**
the provision-time check (keep that as defense-in-depth — do not remove it).

- When the derived provider is a specific vendor (BYOK), resolve its accepted
  auth env-var names from the `providers.yaml` registry (`auth_env` list, e.g.
  `[ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN]` for `anthropic-api`) and confirm at
  least one is present (non-empty) for the workspace at any in-scope secret level.
- On absence: **422** with a structured body:
  `code: MISSING_BYOK_CREDENTIAL`, plus `provider`, `missing_env` (the candidate
  env-var names), `routing: byok`, and a human `error` that names the
  provider, the missing credential, and the remediation ("set
  `ANTHROPIC_API_KEY` as a workspace or org secret, then retry create"). Reuse the
  existing `formatMissingBYOKCredentialError` wording where possible so create and
  provision speak with one voice.
- Log loudly with the same `MISSING_BYOK_CREDENTIAL` code the provisioner uses, so
  the two checkpoints are greppable as one class.

### Gap B (Req3): credential LIVENESS preflight

Add a minimal authenticated probe per provider, driven entirely by the
`providers.yaml` SSOT — no hardcoded endpoints.

- Derive the probe target from the registry entry: `protocol`/`auth_mode`,
  `base_url_template` or `base_url_anthropic`, and the `auth_env` /
  `auth_token_env` that carries the secret. Make the cheapest authenticated call
  the surface offers (models-list where available, else a 1-token completion).
- Fail-closed on **401/403**: reject the create with **422**
  `code: INVALID_BYOK_CREDENTIAL` (provider, env var, upstream status, remediation
  "the credential was found but the provider rejected it — rotate the key").
- **Recommendation: probe at create** for fast feedback, with a **provision-time
  re-check** (the credential can be revoked between create and provision; the
  provisioner is the last gate before the workspace is live). The provision
  re-check upgrades `workspace_provision_shared.go:225-232` from presence-only to
  presence-and-liveness for BYOK.
- The probe **must be cheap and time-bounded** (see Risks).
- **OAuth-provider nuance:** registry entries with `auth_mode: oauth` and
  `base_url: null` (e.g. `anthropic-oauth`, codex chatgpt-subscription) have no
  HTTP surface the platform dials — the CLI talks to the vendor directly. For
  these, the liveness probe has no cheap server-side equivalent; scope Gap B's
  *active* probe to keyed providers with a non-null base URL and fall back to the
  presence check (Gap A) for OAuth modes. Do not block on inventing an OAuth
  liveness call in this RFC.

## Non-goals

- **Not** changing the derive-from-model SSOT. Selection stays
  `providers.yaml` → `DeriveProvider`. There is no override column and no
  billing-mode flag. No new heuristics.
- **Not** routing BYOK through the platform proxy. The proxy stays
  platform-managed-only; this RFC adds validation around BYOK, it does not move
  BYOK onto a platform code path.
- **Not** re-billing or changing the charge layer. BYOK stays off the platform
  ledger.
- **Not** adding an OAuth-subscription liveness call (deferred — see Gap B
  nuance).

## Risks

- **Preflight latency on create.** An authenticated provider round-trip adds
  hundreds of ms to a few seconds to create. Mitigate with a hard, short timeout
  (target ≤ ~3s) and a clear, distinct error on timeout — a probe timeout must
  NOT be treated as "valid" (fail-closed) but must also be distinguishable from a
  real 401/403 so transient upstream blips are diagnosable. Consider whether a
  probe timeout should 422 (strict fail-closed) or surface a soft warning and
  defer to the provision-time re-check; default to fail-closed at create for the
  loud-feedback goal, with the provision re-check as the safety net.
- **Provider rate-limits.** A models-list / 1-token probe consumes the tenant's
  quota and can be rate-limited (429). A 429 is NOT an auth failure — treat it as
  inconclusive (do not reject as `INVALID_BYOK_CREDENTIAL`), log it, and defer to
  the presence check + provision-time re-check rather than blocking create on a
  429.
- **Provider-side flakiness.** 5xx from the provider is inconclusive, same
  handling as 429 — never silently pass, never hard-reject on a 5xx; log and
  defer.

## Test plan

1. **Gap A — create-time presence (unit + handler):**
   - BYOK-deriving `(runtime, model)` with NO credential in any scope → **422
     `MISSING_BYOK_CREDENTIAL`**, body names provider + missing env; no workspace
     row created.
   - Same with the credential present → create proceeds (mode `byok`).
   - `platform_managed`-deriving model with no tenant key → create proceeds
     (unchanged; proxy path).
2. **Gap B — liveness (unit with a stubbed provider HTTP surface):**
   - Present-but-401/403 key → **422 `INVALID_BYOK_CREDENTIAL`**.
   - Valid key → create proceeds.
   - 429 / 5xx / timeout → inconclusive: create NOT rejected as invalid; logged;
     provision re-check still runs.
   - `auth_mode: oauth` + `base_url: null` provider → active probe skipped,
     presence check governs.
3. **Provision defense-in-depth (existing + extended):**
   - Credential revoked between create and provision → provisioner aborts
     (presence today; liveness re-check after Gap B).
   - Existing `MISSING_BYOK_CREDENTIAL` provision-abort test stays green.
4. **Req4 regression guard:** assert no path flips a BYOK selection to
   `platform_managed` silently — an absent/dead BYOK credential always produces a
   loud 4xx with a code, never a 201 that bills the platform.

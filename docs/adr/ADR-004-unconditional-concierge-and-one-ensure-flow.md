# ADR-004: Unconditional self-host concierge seed, one ensure flow

**Status:** Accepted — operator-ruled 2026-07-07
**Date:** 2026-07-07
**Tracking:** [#3496](https://git.moleculesai.app/molecule-ai/molecule-core/issues/3496)
**Design SSOT:** [`docs/design/rfc-selfhost-onboarding-scene.md`](../design/rfc-selfhost-onboarding-scene.md)
**Related:** [`rfc-platform-agent.md`](../design/rfc-platform-agent.md) (addendum), [ADR-002](archive/ADR-002-local-build-mode-via-registry-presence.md) (zero-config OSS onboarding — this decision completes that story for the concierge)

## Context

Nobody had designed the self-host first run, and three separate gaps compounded
into a dead end:

1. **First-run dead end.** A fresh self-host stack booted with no platform
   agent (boot seed off, or on with no key), so the canvas showed "No platform
   agent yet" plus a repair button backed by a 100%-broken endpoint
   (`COALESCE(status, '')` against the `workspace_status` enum fails at parse
   time, `platform_agent_ensure.go:273`), no mention of API keys, and a
   floating `OnboardingWizard` that taught the wrong hand-create-workspaces
   model. Even a seeded concierge landed `failed` on a keyless stack: the
   self-host model fallback `minimax/MiniMax-M2.7` (`platform_agent.go:576`)
   is a platform-proxy arm that does not exist on self-host ⇒
   `MISSING_PLATFORM_PROXY` abort.
2. **Seed-flag gap.** Concierge existence hinged on the opt-in
   `MOLECULE_SEED_PLATFORM_AGENT` flag, so the default OSS experience had no
   org root at all — while the product ruling is that "the first agent and
   manager agent should always be the concierge", self-host or SaaS.
3. **Three divergent top-halves.** Only the bottom half of the concierge
   lifecycle was shared (`installPlatformAgent`). Above it, the HTTP ensure
   handler carried its own decide/revive/provision logic
   (`decideEnsureAction`), the boot seed re-implemented exists-checking
   (`EnsureSelfHostedPlatformAgent`) plus a bespoke provision-and-probe pass
   (`MaybeProvisionPlatformAgentOnBoot`), and the CP install endpoint was a
   third thin wrapper — the classic multi-copy drift shape.

## Decision

1. **Seed the concierge unconditionally when `MOLECULE_ORG_ID` is unset.**
   The self-host boot seed runs on every start with no flag; it is idempotent
   (existing root ⇒ no-op). `MOLECULE_SEED_PLATFORM_AGENT` is **removed**. On
   SaaS (org id set) the tenant server never self-seeds — the control plane
   remains the sole creator, byte-identical to prior behavior.
2. **One canonical lifecycle flow.** Extract a single service function,
   `ensurePlatformAgentFlow(ctx, db, opts{name, runtime, model, force,
   triggerProvision})` — decide (created / exists / repaired / revive) →
   install → name/model write → optional provision trigger. Every entrypoint
   becomes a thin adapter over it: the HTTP `POST /admin/org/platform-agent/ensure`
   handler, the in-process boot seed, and the legacy CP install endpoint
   `POST /admin/org/platform-agent` (a contract-frozen, row-only shim until
   the CP migrates to `/ensure` on its own release schedule, after which the
   shim is deleted).
3. **The first-run scene is a configurator, never a creator.** Because the
   root always exists by the time a canvas loads, the fullscreen self-host
   setup scene only configures it: runtime via `PATCH /workspaces/:id` (the
   upsert deliberately preserves runtime), model + name + provision trigger
   via ensure `force:true` (the 'repaired' path re-provisions the existing id
   in place). This kills the create-race class outright — the scene never
   triggers a provision it hasn't already configured.
4. **`awaiting_agent` reuse for the unconfigured root is rejected.** An
   unconfigured root stays `offline` and the boot provision is skipped; the
   scene's gate probes offline + missing MODEL secret/key. Reuse of
   `awaiting_agent` was ruled out on three state-machine facts (spec §6 D2):
   (a) the heartbeat recovery at `registry.go:2205` is unconditional — the
   first heartbeat from any provisioned concierge would silently flip it
   online, machinery lifting the setup gate instead of the user; (b)
   `cp_instance_reconciler` excludes `awaiting_agent` rows from reconcile
   scope (it assumes they are external) — a SaaS concierge parked there goes
   invisible; (c) canvas + CLI render it as "reconnect the external agent",
   the wrong fix path. A new enum value would need a migration + models
   constant in the same PR (drift-test enforced) — not worth it when
   offline+probe suffices.
5. **Platform-runtime guard.** Both AdminAuth endpoints reject
   external-like/mock/unknown runtimes for `kind='platform'` with 422
   (`isKnownRuntime && !isExternalLikeRuntime && runtime != "mock"`), closing
   the audit-found gap where `POST ensure {runtime:"external"}` produced a
   permanently wedged concierge with a lying API response.

## Consequences

- Every self-host stack has an org root from first boot; interactive stacks
  finish setup in the fullscreen scene, headless stacks converge via
  `MOLECULE_DEFAULT_RUNTIME` + `MOLECULE_LLM_DEFAULT_MODEL` + a provider key
  in env — zero UI, the scene never renders.
- The seed flag disappears from `main.go`, `docker-compose.yml`, the
  `Makefile` comment, and the e2e contract text;
  `test_concierge_creates_workspace_local.sh` flips from skip-loud to a hard
  gate ("no concierge on a local stack" is now a genuine bug).
- Exactly one decide/install/provision top-half exists; boot seed and HTTP
  ensure are demonstrably adapters (spec §11 requires grep-proof). Boot-seeded,
  ensure-created, and shim-installed roots are field-for-field identical.
- The CP experiences no contract change at any point: the install shim is
  byte-identical until the CP-repo migration lands, and the `model` field on
  ensure is optional + additive (the CP never calls ensure).
- No new workspace-status enum value, so no migration and no drift-test churn.
- The seed decision moves out of `main()` into a pure helper to satisfy the
  100%-coverage-on-changed-files gate (spec §10.1), making boot logic
  unit-addressable.

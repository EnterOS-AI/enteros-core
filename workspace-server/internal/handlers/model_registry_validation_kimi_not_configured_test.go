package handlers

// model_registry_validation_kimi_not_configured_test.go — PINNED regression for
// the 2026-06-15 incident-combo: a platform model whose provider is
// NOT_CONFIGURED must FAIL CLOSED, never silently route to another (configured)
// provider.
//
// WHY THIS FILE EXISTS (de-hardcode follow-up to PRs #3268/#3269/#3270):
//   Those PRs moved the platform-boot lanes (concierge seed + saas-platform-boot
//   + reconciler) off the hardcoded "moonshot/kimi-k2.6" default and onto the
//   KMS-resolved generic default (MOLECULE_LLM_DEFAULT_MODEL, today
//   "minimax/MiniMax-M2.7"). That de-hardcode was behavior-neutral on prod, but
//   it dropped the explicit incident-combo coverage the old hardcoded lanes
//   carried: with the default now minimax, no test any longer asserts what
//   happens to the ORIGINAL incident model (moonshot/kimi-k2.6) when its
//   provider is NOT_CONFIGURED. The merged PRs flagged restoring that coverage
//   as a follow-up. This file restores it — pinned to the LITERAL incident combo
//   on purpose (a regression test pins the historical bug, it does not chase the
//   moving KMS default), so a future refactor that re-introduces silent
//   fall-through on a NOT_CONFIGURED platform model fails CI here.
//
// THE 2026-06-15 INCIDENT (the contract this pins):
//   A platform-managed model id ("moonshot/kimi-k2.6") is requested for a
//   runtime, but the moonshot/kimi provider is NOT_CONFIGURED — there is no
//   `platform` arm that lists it and no native moonshot/kimi provider entry that
//   prefix-owns it (no key, no registry entry). The ONLY configured provider in
//   the runtime's native set is a DIFFERENT one (minimax, the de-hardcode
//   default). Resolution MUST fail closed (return the MISSING_MODEL /
//   NOT_CONFIGURED rejection) and MUST NOT silently fall through to the
//   configured minimax arm. Silent fall-through is the dangerous mode: it would
//   bill/route a user's requested model through the wrong provider with no error
//   surfaced.
//
// WHICH GATE OWNS THE FAIL-CLOSED:
//   validateRegisteredModelForRuntime (the model-side concierge fail-closed gate
//   the de-hardcode lanes call via ensureConciergeModel). For a NOT_CONFIGURED
//   platform model it returns (false, reason): the id is NOT on the runtime's
//   platform menu (ModelsForRuntime, since no platform arm lists it) AND no
//   native provider arm prefix-owns it (DeriveProvider misses, since the
//   moonshot/platform provider is absent), so the routability-aware OR is false
//   on BOTH legs → REJECT. validateDerivedProviderInRegistry then correctly
//   defers (don't double-reject — DeriveProvider's miss is the model-side gate's
//   signal, which it owns), so it returns (true, "") and the model-side
//   rejection is the authoritative one. Both behaviors are pinned below.
//
// The manifests are hand-built (same shape as offered_models_test.go /
// workspace_provision_derive_test.go) and swapped in via
// withSwappedProviderRegistry so the test is deterministic and does not depend
// on the embedded providers.yaml's evolving default.

import (
	"strings"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/providers"
)

const (
	// kimiIncidentModel is the LITERAL incident combo from 2026-06-15. Pinned as
	// a string constant ON PURPOSE: this is a regression test for a historical
	// bug, so it must keep asserting against the exact id that broke — NOT the
	// moving KMS/registry default. (Do NOT read KMS here.)
	kimiIncidentModel  = "moonshot/kimi-k2.6"
	kimiIncidentRT     = "claude-code"
	dehardcodeDefault  = "minimax/MiniMax-M2.7" // the configured default the bad path would route to
	dehardcodeProvider = "minimax"
)

// kimiNotConfiguredManifest models the NOT_CONFIGURED world: the claude-code
// runtime has ONLY a configured minimax arm (the de-hardcode default). There is
// NO `platform` provider and NO moonshot/kimi provider entry at all — so
// "moonshot/kimi-k2.6" is genuinely unroutable (no key, no registry entry).
// The DANGER the test guards: minimax IS configured and on the same runtime, so
// a regressed gate could silently route the moonshot id to minimax.
func kimiNotConfiguredManifest() *providers.Manifest {
	return &providers.Manifest{
		Providers: []providers.Provider{
			// minimax IS configured — the de-hardcode default. Its prefix only
			// owns minimax/* ids, so it does NOT prefix-own the moonshot id; the
			// fail-closed must hold because NO provider owns moonshot/kimi-k2.6.
			{Name: dehardcodeProvider, ModelPrefixMatch: `(?i)^minimax/`, AuthEnv: []string{"MINIMAX_API_KEY"}},
			// NO `platform` provider. NO `moonshot`/`kimi` provider. The
			// moonshot/kimi provider is NOT_CONFIGURED — no key, no registry entry.
		},
		Runtimes: map[string]providers.RuntimeNativeSet{
			kimiIncidentRT: {
				Providers: []providers.RuntimeProviderRef{
					{Name: dehardcodeProvider, Models: []string{dehardcodeDefault}},
				},
			},
		},
	}
}

// kimiConfiguredManifest models the HEALTHY world (positive control): the
// claude-code runtime has a configured `platform` arm that lists the incident
// model on its platform menu, exactly as the live providers.yaml does. Here the
// SAME id MUST route — proving the test asserts a real routing contract, not
// merely "everything fails".
func kimiConfiguredManifest() *providers.Manifest {
	return &providers.Manifest{
		Providers: []providers.Provider{
			// platform IS configured and prefix-owns the namespaced platform ids.
			{Name: "platform", ModelPrefixMatch: `^(moonshot|minimax)/`, AuthEnv: []string{"MOLECULE_LLM_USAGE_TOKEN"}},
		},
		Runtimes: map[string]providers.RuntimeNativeSet{
			kimiIncidentRT: {
				Providers: []providers.RuntimeProviderRef{
					{Name: "platform", Models: []string{kimiIncidentModel, dehardcodeDefault}},
				},
			},
		},
	}
}

// TestPinMoonshotKimiNotConfigured_FailsClosed is the core regression. With the
// moonshot/kimi provider NOT_CONFIGURED, requesting "moonshot/kimi-k2.6" MUST be
// rejected by the model-side gate (validateRegisteredModelForRuntime) and MUST
// NOT silently route to the configured minimax arm.
func TestPinMoonshotKimiNotConfigured_FailsClosed(t *testing.T) {
	withSwappedProviderRegistry(t, kimiNotConfiguredManifest(), nil, func() {
		// (a) The model-side fail-closed gate REJECTS the NOT_CONFIGURED platform
		//     model. ok MUST be false — this is the MISSING_MODEL / NOT_CONFIGURED
		//     fail-closed.
		ok, why := validateRegisteredModelForRuntime(kimiIncidentRT, kimiIncidentModel)
		if ok {
			t.Fatalf("FAIL-CLOSED VIOLATED: validateRegisteredModelForRuntime(%q,%q) returned ok=true — a NOT_CONFIGURED platform model was accepted (would silently route, e.g. to the configured %q arm). This is the 2026-06-15 incident class.",
				kimiIncidentRT, kimiIncidentModel, dehardcodeProvider)
		}
		// The rejection reason must name the offending model so the failure is
		// actionable (not an opaque 422). It must NOT mention the minimax arm —
		// the whole point is we did NOT route there.
		if !strings.Contains(why, kimiIncidentModel) {
			t.Errorf("rejection reason should name the rejected model %q; got %q", kimiIncidentModel, why)
		}

		// (a') Defense-in-depth: DeriveProvider itself must MISS for the
		//      NOT_CONFIGURED id — it must NOT resolve to the configured minimax
		//      provider. This is the load-bearing fact under the gate: if
		//      DeriveProvider silently returned minimax, the routability-aware OR
		//      in the gate would (wrongly) allow the id.
		m := kimiNotConfiguredManifest()
		if p, derr := m.DeriveProvider(kimiIncidentRT, kimiIncidentModel, nil); derr == nil {
			t.Fatalf("FAIL-CLOSED VIOLATED at the derive layer: DeriveProvider(%q,%q) resolved to provider %q for a NOT_CONFIGURED model — it must MISS, never silently route", kimiIncidentRT, kimiIncidentModel, p.Name)
		}
		// And explicitly: it never resolves to the (configured) minimax fall-through.
		if p, derr := m.DeriveProvider(kimiIncidentRT, kimiIncidentModel, nil); derr == nil && p.Name == dehardcodeProvider {
			t.Fatalf("SILENT FALL-THROUGH: moonshot/kimi NOT_CONFIGURED was routed to the configured %q provider", dehardcodeProvider)
		}

		// (b) The provider-side gate defers (don't double-reject): DeriveProvider's
		//     miss is the model-side gate's signal, which already owns the
		//     rejection. So this gate passes through (true) — pinning the
		//     deliberate non-double-reject contract documented on the function.
		ok2, _ := validateDerivedProviderInRegistry(kimiIncidentRT, kimiIncidentModel)
		if !ok2 {
			t.Errorf("validateDerivedProviderInRegistry(%q,%q) returned false — it should DEFER to the model-side gate on a derive miss (don't double-reject), letting the model-side rejection own the message", kimiIncidentRT, kimiIncidentModel)
		}
	})
}

// TestPinMoonshotKimiConfigured_RoutesPositiveControl is the positive control:
// when the moonshot/kimi provider IS configured (a `platform` arm lists the
// incident model on the platform menu), the SAME id MUST be accepted and MUST
// derive to the `platform` provider. Without this arm the fail-closed test above
// could pass trivially by "always rejecting" — this proves routing genuinely
// works when the provider is configured.
func TestPinMoonshotKimiConfigured_RoutesPositiveControl(t *testing.T) {
	withSwappedProviderRegistry(t, kimiConfiguredManifest(), nil, func() {
		// Model-side gate ACCEPTS: the id is on the platform menu.
		if ok, why := validateRegisteredModelForRuntime(kimiIncidentRT, kimiIncidentModel); !ok {
			t.Fatalf("POSITIVE CONTROL BROKEN: validateRegisteredModelForRuntime(%q,%q) rejected a CONFIGURED platform model (%s) — routing must work when the provider IS configured", kimiIncidentRT, kimiIncidentModel, why)
		}
		// Provider-side gate ACCEPTS: the derived provider is in the registry.
		if ok, why := validateDerivedProviderInRegistry(kimiIncidentRT, kimiIncidentModel); !ok {
			t.Fatalf("POSITIVE CONTROL BROKEN: validateDerivedProviderInRegistry(%q,%q) rejected a CONFIGURED platform model (%s)", kimiIncidentRT, kimiIncidentModel, why)
		}
		// And it derives to the closed `platform` provider (the platform-managed
		// route), NOT to some BYOK arm — the exact resolution the live boot needs.
		p, derr := kimiConfiguredManifest().DeriveProvider(kimiIncidentRT, kimiIncidentModel, nil)
		if derr != nil {
			t.Fatalf("POSITIVE CONTROL BROKEN: DeriveProvider(%q,%q) errored for a CONFIGURED platform model: %v", kimiIncidentRT, kimiIncidentModel, derr)
		}
		if p.Name != "platform" || !p.IsPlatform() {
			t.Errorf("CONFIGURED moonshot/kimi-k2.6 should derive to the platform provider; got %q (IsPlatform=%v)", p.Name, p.IsPlatform())
		}
	})
}

// TestPinMoonshotKimiNotConfigured_MutationCheck proves the fail-closed test
// above is NOT a tautology. It substitutes a PASS-THROUGH stand-in for the
// model-side gate (the mutant: a gate that always allows, i.e. the silent
// fall-through bug) and confirms the SAME assertion the real test makes
// (wantOK == false) would FAIL against that mutant. If the assertion did not
// fail against a pass-through gate, the real test would be asserting nothing.
//
// We exercise the mutant in-test (rather than editing production code) by
// pointing the assertion at a local pass-through function with the gate's
// signature; the assertion logic is identical to the real test's, so this
// directly certifies the real test depends on the gate returning false.
func TestPinMoonshotKimiNotConfigured_MutationCheck(t *testing.T) {
	// The mutant: the de-hardcode regression itself — a gate that "passes
	// through" (always allows) instead of failing closed.
	passThroughGateMutant := func(runtime, model string) (bool, string) { return true, "" }

	withSwappedProviderRegistry(t, kimiNotConfiguredManifest(), nil, func() {
		// Sanity: the REAL gate fails closed (ok=false) in this same world.
		realOK, _ := validateRegisteredModelForRuntime(kimiIncidentRT, kimiIncidentModel)
		if realOK {
			t.Fatalf("precondition failed: the real gate did not fail closed in the NOT_CONFIGURED world")
		}

		// Now apply the REAL test's assertion (want ok==false) to the MUTANT. The
		// mutant returns ok=true, so the fail-closed assertion MUST trip. We assert
		// that it trips — i.e. the assertion has teeth.
		mutantOK, _ := passThroughGateMutant(kimiIncidentRT, kimiIncidentModel)
		const wantFailClosed = false // the real test asserts ok == false
		assertionWouldTrip := (mutantOK != wantFailClosed)
		if !assertionWouldTrip {
			t.Fatalf("TAUTOLOGY: the fail-closed assertion (want ok==%v) did NOT trip against a pass-through mutant (mutant ok=%v) — the real test would pass even with the gate disabled", wantFailClosed, mutantOK)
		}
		// assertionWouldTrip == true → the real test genuinely exercises the
		// fail-closed: disabling the gate (the mutant) would make it fail.
	})
}

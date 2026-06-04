package handlers

// model_registry_validation.go — only-registered (runtime, model) validation
// at the create/config API (internal#718 P2-B item 3, CTO 2026-05-27
// "only registered providers/models selectable").
//
// The registry (internal/providers) is the SSOT for which models a runtime
// natively exposes (ModelsForRuntime). This validator rejects a (runtime, model)
// the registry does NOT recognize — but ONLY for a runtime the registry knows
// about. For a runtime absent from the first-party registry (langgraph,
// external, kimi, mock, or a future federated third-party runtime), it fails
// OPEN: the registry can't speak to that runtime's model set, so the existing
// knownRuntimes gate stays authoritative and this validator does not block.
// This is the federation-ready contract — first-party runtimes are gated against
// the registry; everything else passes through unchanged (no behavior change for
// non-registry runtimes).

import (
	"fmt"
	"sort"
	"strings"
)

// validateRegisteredModelForRuntime reports whether (runtime, model) is
// selectable per the provider registry. Returns:
//
//	(true,  "")     — allowed: model is on the runtime's platform menu
//	                  (ModelsForRuntime) OR DeriveProvider(runtime, model)
//	                  RESOLVES a native provider (the cp#529 routability-aware
//	                  BYOK path), OR the runtime is not in the registry
//	                  (fail-open), OR model=="".
//	(false, reason) — rejected: the runtime IS registered, the model is not on
//	                  its platform menu, AND no native provider prefix-owns it
//	                  (genuinely unroutable).
//
// model=="" is allowed here: the MODEL_REQUIRED gate owns the empty-model case,
// so this validator must not double-reject it.
//
// ROUTABILITY-AWARE (cp#529, CTO Option C): the final predicate is an OR —
// `model ∈ ModelsForRuntime(runtime)` OR `DeriveProvider(runtime, model, nil)`
// resolves. The platform menu carries platform-billed ids; the DeriveProvider
// path covers BYOK ids that prefix-match a name-only native arm (no platform
// billing). The drift checker in molecule-controlplane mirrors this exact OR.
func validateRegisteredModelForRuntime(runtime, model string) (bool, string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return true, "" // MODEL_REQUIRED owns this.
	}
	m, err := providerRegistry()
	if err != nil || m == nil {
		// Registry unavailable (build-time defect the gates catch). Fail open —
		// do not block create on a registry-load failure.
		return true, ""
	}
	models, err := m.ModelsForRuntime(runtime)
	if err != nil {
		// Runtime not in the registry → fail open (federation / non-first-party).
		return true, ""
	}
	for _, mid := range models {
		if mid == model {
			return true, ""
		}
	}
	// ROUTABILITY-AWARE allow path (cp#529, CTO-approved Option C). The model is
	// NOT on the runtime's platform menu (ModelsForRuntime) — but a model can be
	// legitimately SELECTABLE without being a platform-menu id: a BYOK id whose
	// prefix matches one of the runtime's NATIVE provider arms (a name-only arm
	// added in providers.yaml) resolves to a concrete provider via DeriveProvider
	// even though it carries no platform billing. Allow it iff DeriveProvider
	// resolves a provider for (runtime, model). A genuinely-unroutable id (no
	// native provider prefix-owns it) still falls through to the 422 below.
	//
	// BILLING GUARDRAIL: only CONFIRMED-NON-PLATFORM (BYOK) providers are wired as
	// name-only arms in providers.yaml (never platform/anthropic-*/openai-*/
	// moonshot/minimax/google/vertex), so a DeriveProvider-resolved id reached by
	// THIS path can never bill the platform's key for a customer's model. The
	// platform-menu ids that DO carry platform billing are already allowed by the
	// exact-membership loop above; this path only ever resolves to a BYOK arm.
	if _, derr := m.DeriveProvider(runtime, model, nil); derr == nil {
		return true, ""
	}
	return false, fmt.Sprintf(
		"model %q is not a registered model for runtime %q; pick one of the runtime's registered models (provider-registry SSOT, internal#718)",
		model, runtime)
}

// validateDerivedProviderInRegistry (issue #2172) is the provider-side companion
// to validateRegisteredModelForRuntime. The model-side check asks "is this
// (runtime, model) in the registry?"; the provider-side check asks "is the
// provider this model DERIVES to — the same one the adapter will resolve at
// boot — a known provider in providers.yaml?"
//
// Live trigger (adk-demo Assistant, 2026-06-03): workspace config
// `model=moonshot/kimi-k2.6` (claude-code) → adapter derives `provider=moonshot`
// → `ValueError: provider=moonshot not in providers registry` at BOOT. The
// save was accepted (no validation at the API boundary), and the failure only
// surfaced when the agent tried to register. CI never saw it. The drift gate
// (RFC#580) validates TEMPLATES against the registry, NOT per-workspace
// configs; the existing model-side check rejects a model the runtime doesn't
// own but says nothing about the DERIVED provider's registry membership.
//
// Returns:
//
//	(true,  "")     — pass: model is empty (MODEL_REQUIRED owns it), the
//	                  runtime is not in the registry (fail-open for
//	                  federated / non-first-party runtimes — mirror of the
//	                  model-side check's federation contract), the registry
//	                  failed to load (build-time gate owns it), OR the
//	                  derived provider name is a known provider in the
//	                  registry's `providers:` list.
//	(false, reason) — reject: a known (runtime, model) pair derives to a
//	                  provider name absent from the providers list. This is
//	                  the structural class the adk-demo boot failure belongs
//	                  to — the registry's `runtimes:` block references a
//	                  provider not declared in `providers:`, which by
//	                  construction is a registry-data bug. Catching it at
//	                  config-SAVE keeps it out of the agent-boot path.
//
// Defense-in-depth: by construction, a model in a runtime's native provider set
// has a provider that IS in the catalog (the runtime ref names a provider from
// the providers list). So the rejection path is primarily a registry-consistency
// guard. The real value is the FAIL-LOUD semantics — any future drift between
// `providers:` and `runtimes:` fails the create call with a clear pointer to
// the missing provider, instead of silently wedging the agent at boot.
func validateDerivedProviderInRegistry(runtime, model string) (bool, string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return true, "" // MODEL_REQUIRED owns this.
	}
	m, err := providerRegistry()
	if err != nil || m == nil {
		// Registry unavailable (build-time defect the gates catch). Fail open —
		// do not block create on a registry-load failure.
		return true, ""
	}
	// DeriveProvider is fail-closed for unknown runtimes. Mirror the
	// model-side check's federation contract: a runtime the registry does
	// NOT know (langgraph / external / kimi / mock / federated) is allowed
	// to pass through. DeriveProvider's `unknown runtime` error IS that
	// signal — treat it as fail-open, identical to ModelsForRuntime's
	// not-found behavior above.
	p, err := m.DeriveProvider(runtime, model, nil)
	if err != nil {
		// Either the runtime is unknown (fail-open by contract) OR the model
		// is not native to the runtime (the model-side validator already
		// rejected this — DeriveProvider's error here means
		// validateRegisteredModelForRuntime should have caught it. Don't
		// double-reject: pass through and let the model-side response own
		// the message).
		return true, ""
	}
	// Defense-in-depth: confirm the DERIVED provider is a known entry in the
	// providers list. By construction it should be (DeriveProvider only
	// returns a Provider that was looked up by name from `providers:`), but
	// a future federation merge could introduce a runtime ref pointing at a
	// contributed provider absent from the core catalog. Reject loudly here
	// rather than letting the save reach the agent-boot path and wedge with
	// "provider=X not in providers registry" (the original adk-demo class).
	for _, candidate := range m.Providers {
		if candidate.Name == p.Name {
			return true, ""
		}
	}
	// Build a sorted, comma-separated list of valid provider names so the
	// operator/caller sees the actionable list (the boot-time error message
	// the adk-demo class produced does NOT include this — the fix is to
	// surface it at the API boundary, where the caller can fix the request
	// without a stuck workspace + operator page).
	valid := make([]string, 0, len(m.Providers))
	for _, c := range m.Providers {
		valid = append(valid, c.Name)
	}
	sort.Strings(valid)
	return false, fmt.Sprintf(
		"derived provider %q (for model %q on runtime %q) is not in the providers registry; pick a model whose derived provider is one of: %s",
		p.Name, model, runtime, strings.Join(valid, ", "))
}

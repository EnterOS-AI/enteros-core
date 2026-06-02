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
	"strings"
)

// validateRegisteredModelForRuntime reports whether (runtime, model) is
// selectable per the provider registry. Returns:
//
//	(true,  "")     — allowed: model is registered for this runtime, OR the
//	                  runtime is not in the registry (fail-open), OR model=="".
//	(false, reason) — rejected: the runtime IS registered but the model is not
//	                  in its native ModelsForRuntime set.
//
// model=="" is allowed here: the MODEL_REQUIRED gate owns the empty-model case,
// so this validator must not double-reject it.
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
	return false, fmt.Sprintf(
		"model %q is not a registered model for runtime %q; pick one of the runtime's registered models (provider-registry SSOT, internal#718)",
		model, runtime)
}

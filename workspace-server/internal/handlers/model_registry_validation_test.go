package handlers

// model_registry_validation_test.go — only-registered (runtime, model)
// validation at the create/config API (internal#718 P2-B item 3). Reject a
// (runtime, model) the registry does not recognize for a runtime it DOES know;
// fail OPEN (allow) for a runtime the registry doesn't know yet (federation /
// langgraph/etc. not in the first-party registry) so the existing knownRuntimes
// gate stays authoritative there.

import "testing"

func TestValidateRegisteredModelForRuntime(t *testing.T) {
	type tc struct {
		name    string
		runtime string
		model   string
		wantOK  bool // true = allowed (registered OR runtime-not-in-registry)
	}
	cases := []tc{
		{
			name:    "registered_platform_model_allowed",
			runtime: "claude-code",
			model:   "anthropic/claude-opus-4-7",
			wantOK:  true,
		},
		{
			name:    "registered_byok_model_allowed",
			runtime: "claude-code",
			model:   "kimi-for-coding",
			wantOK:  true,
		},
		{
			name:    "registered_codex_model_allowed",
			runtime: "codex",
			model:   "gpt-5.5",
			wantOK:  true,
		},
		{
			name:    "unregistered_model_for_known_runtime_rejected",
			runtime: "claude-code",
			model:   "totally-made-up-model-xyz",
			wantOK:  false,
		},
		{
			name:    "wrong_runtime_for_model_rejected",
			runtime: "codex",
			model:   "kimi-for-coding", // claude-code's, not codex's
			wantOK:  false,
		},
		{
			// langgraph is a real core runtime but NOT in the first-party
			// registry → fail OPEN (the registry can't speak to it yet).
			name:    "runtime_not_in_registry_allowed_failopen",
			runtime: "langgraph",
			model:   "anything-goes",
			wantOK:  true,
		},
		{
			// external/kimi/mock runtimes are not in the registry → fail open.
			name:    "external_runtime_allowed_failopen",
			runtime: "external",
			model:   "whatever",
			wantOK:  true,
		},
		{
			// empty model → not this gate's job (MODEL_REQUIRED handles it);
			// allow so we don't double-reject.
			name:    "empty_model_allowed_other_gate_owns_it",
			runtime: "claude-code",
			model:   "",
			wantOK:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, _ := validateRegisteredModelForRuntime(c.runtime, c.model)
			if ok != c.wantOK {
				t.Errorf("validateRegisteredModelForRuntime(%q,%q) ok=%v want %v", c.runtime, c.model, ok, c.wantOK)
			}
		})
	}
}

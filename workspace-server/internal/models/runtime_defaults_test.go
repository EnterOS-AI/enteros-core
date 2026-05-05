package models

import "testing"

// TestDefaultModel pins the contract: known runtimes return their
// expected default; unknowns and the empty string fall through to the
// universal default. Add new runtimes here as `case` entries — pre-fix
// adding a runtime required two source edits + an audit; post-SSOT it
// requires one entry in DefaultModel + one assertion here.
func TestDefaultModel(t *testing.T) {
	cases := []struct {
		runtime string
		want    string
	}{
		// Known runtimes.
		{"claude-code", "sonnet"},

		// Universal fallback for everything else. Each runtime is named
		// explicitly so a future drift (e.g., adding a hermes-specific
		// branch) shows up as a failure on the runtime that drifted, not
		// as a generic "unknown" failure.
		{"hermes", "anthropic:claude-opus-4-7"},
		{"langgraph", "anthropic:claude-opus-4-7"},
		{"crewai", "anthropic:claude-opus-4-7"},
		{"autogen", "anthropic:claude-opus-4-7"},
		{"deepagents", "anthropic:claude-opus-4-7"},
		{"codex", "anthropic:claude-opus-4-7"},
		{"openclaw", "anthropic:claude-opus-4-7"},
		{"gemini-cli", "anthropic:claude-opus-4-7"},
		{"external", "anthropic:claude-opus-4-7"},

		// Unknown / empty — fall through to universal default rather
		// than failing closed. Pre-refactor both call sites also fell
		// through; pinning the existing behavior, not changing it.
		{"", "anthropic:claude-opus-4-7"},
		{"some-future-runtime", "anthropic:claude-opus-4-7"},
		{"CLAUDE-CODE", "anthropic:claude-opus-4-7"}, // case-sensitive — matches prior behavior
	}
	for _, tc := range cases {
		t.Run(tc.runtime, func(t *testing.T) {
			got := DefaultModel(tc.runtime)
			if got != tc.want {
				t.Errorf("DefaultModel(%q) = %q, want %q", tc.runtime, got, tc.want)
			}
		})
	}
}

// TestDefaultModel_NeverEmpty — invariant: no input produces an empty
// string. The handlers that consume this would write empty into
// config.yaml, which the runtime then can't dispatch — pinning the
// non-empty contract here protects against a future "return early on
// unknown runtime" change that would silently break workspace creation.
func TestDefaultModel_NeverEmpty(t *testing.T) {
	for _, runtime := range []string{
		"", "claude-code", "hermes", "unknown-runtime",
	} {
		if got := DefaultModel(runtime); got == "" {
			t.Errorf("DefaultModel(%q) returned empty string", runtime)
		}
	}
}

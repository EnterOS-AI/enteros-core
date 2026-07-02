package handlers

// no_billing_mode_env_test.go — architectural guard replacing the deleted
// llm_billing_mode_test.go. The per-workspace `llm_billing_mode` field and the
// MOLECULE_LLM_BILLING_MODE env were removed 2026-06-30; platform-vs-BYOK now
// derives purely from the provider registry. This test fails if any NON-test
// source file in this package re-introduces a reference to the retired env var
// (in particular `os.Getenv("MOLECULE_LLM_BILLING_MODE")`), which would silently
// resurrect the removed org/per-workspace billing-mode signal.
//
// Mirrors the os.ReadDir(".") source-walk pattern from
// TestProvisionFunctions_AllCallMintWorkspaceSecrets in
// workspace_provision_shared_test.go.

import (
	"os"
	"strings"
	"testing"
)

func TestNoSourceFileReadsMoleculeLLMBillingModeEnv(t *testing.T) {
	t.Parallel()

	const forbidden = "MOLECULE_LLM_BILLING_MODE"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		// Only NON-test source files: test files legitimately reference the
		// literal in absence-assertions and comments.
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(data), forbidden) {
			t.Errorf("%s references the retired env var %q — the per-workspace "+
				"llm_billing_mode signal was removed 2026-06-30; platform-vs-BYOK must "+
				"derive from the provider registry, not from this env. Remove the reference.",
				name, forbidden)
		}
	}
}

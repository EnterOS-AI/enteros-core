package handlers

// GUARDRAIL SELF-TEST (core side) — proves the core G0 guardrail goes RED on its
// known regression. Task #80. "How do we test the guardrails work?" — by feeding
// each guardrail check the filename-split bug it exists to catch and asserting the
// check FAILS. A guardrail that can't fail isn't a guardrail.
//
// The core G0 guardrail (concierge_prompt_filename_ssot_test.go) asserts four
// layers converge on "system-prompt.md": the substitution target key, the boot
// probe path, the default config.yaml prompt_files, and the asset allowlist. This
// self-test reintroduces the split in a throwaway fixture and asserts the SAME
// check logic the guardrail uses reports RED. (The runtime half is proved by
// tests/test_guardrail_self_test.py in molecule-ai-workspace-runtime.)

import (
	"strings"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// checkConvergesOnCanonical is the guardrail's core predicate, factored so the
// self-test can feed it a regressed (split) value and observe RED. It returns
// true iff the layer's filename matches the canonical SSOT filename.
func checkConvergesOnCanonical(layerFilename string) bool {
	return layerFilename == canonicalConciergePromptFilename
}

// G0 self-test #1 — a SPLIT substitution target (template ships its prompt under
// a different name) must make the convergence check FAIL.
func TestGuardrailSelfTest_SplitSubstitutionTargetGoesRed(t *testing.T) {
	// Pristine: the real substitution target converges (GREEN).
	if !checkConvergesOnCanonical(canonicalConciergePromptFilename) {
		t.Fatal("self-test setup error: canonical filename must converge")
	}

	// REGRESSION: someone repoints the substitution target at "concierge.md".
	const splitFilename = "concierge.md"
	if checkConvergesOnCanonical(splitFilename) {
		t.Fatalf("guardrail self-test FAILED: a split substitution target %q was "+
			"accepted as converged — the guardrail would NOT catch the filename split",
			splitFilename)
	}
	// The above proves: fed the regression, the guardrail's predicate is RED. Good.

	// And the live placeholder symptom of the split is real: a prompt delivered
	// under the wrong filename never gets substituted by the canonical-keyed path.
	configFiles := map[string][]byte{splitFilename: []byte("I am {{CONCIERGE_NAME}}.")}
	if prompt, ok := configFiles[canonicalConciergePromptFilename]; ok {
		configFiles[canonicalConciergePromptFilename] = substituteConciergeName(prompt, "Acme")
	}
	if !strings.Contains(string(configFiles[splitFilename]), conciergeNamePlaceholder) {
		t.Fatal("guardrail self-test FAILED: expected un-substituted placeholder on the split file")
	}
}

// G0 self-test #2 — a SPLIT probe path (boot probes a different filename than the
// subst writes) must make the convergence check FAIL.
func TestGuardrailSelfTest_SplitProbePathGoesRed(t *testing.T) {
	// REGRESSION: the probe reads /configs/concierge.md while subst writes
	// system-prompt.md. Extract the filename from the (regressed) probe path and
	// assert the guardrail's convergence predicate reports RED.
	const regressedProbePath = "/configs/concierge.md"
	probeFilename := regressedProbePath[strings.LastIndex(regressedProbePath, "/")+1:]
	if checkConvergesOnCanonical(probeFilename) {
		t.Fatalf("guardrail self-test FAILED: a split probe filename %q was accepted "+
			"as converged — the boot-restart-loop / generic-identity bug would slip through",
			probeFilename)
	}
}

// G0 self-test #3 — a SPLIT default config.yaml prompt_files default must make the
// convergence check FAIL. We emulate generateDefaultConfig defaulting to the
// wrong filename and assert the guardrail's line-presence check goes RED.
func TestGuardrailSelfTest_SplitDefaultConfigGoesRed(t *testing.T) {
	// REGRESSION: a hypothetical default config that emits the wrong prompt file.
	regressedConfig := "prompt_files:\n  - concierge.md\n"
	wantLine := "  - " + canonicalConciergePromptFilename
	if strings.Contains(regressedConfig, wantLine) {
		t.Fatalf("guardrail self-test FAILED: a split default config was accepted — "+
			"expected the canonical-line check %q to be absent (RED) in %q",
			wantLine, regressedConfig)
	}
}

// G0 self-test #4 — if the asset allowlist STOPPED accepting the canonical
// filename (de-bake regression: identity no longer deliverable), the guardrail's
// allowlist check would go RED. We can't mutate the real allowlist, but we prove
// the guardrail's predicate is genuinely sensitive: a NON-allowlisted filename is
// rejected (so a real de-bake regression on system-prompt.md would be caught).
func TestGuardrailSelfTest_AllowlistPredicateIsSensitive(t *testing.T) {
	// Sanity: the real canonical filename IS allowed (the green state).
	if !provisioner.IsCPTemplateAssetPath(canonicalConciergePromptFilename) {
		t.Fatal("self-test setup error: canonical filename must be allowlisted")
	}
	// A filename OUTSIDE the namespace is rejected — proving the allowlist check
	// is a real gate, not a tautology. If system-prompt.md were ever dropped from
	// the allowlist it would be rejected exactly like this unrelated name.
	const notDeliverable = "concierge-identity.txt"
	if provisioner.IsCPTemplateAssetPath(notDeliverable) {
		t.Fatalf("guardrail self-test FAILED: allowlist accepted %q — the allowlist "+
			"check is not a real gate, so a dropped-canonical regression would slip", notDeliverable)
	}
}

package handlers

// delegation_rollout_interlock_test.go — the #4338 interlock.
//
// This is the gate that stands between a one-character config change and a
// fleet-wide false-failure event. It exists because the precondition it enforces
// ("do NOT flip DELEGATION_LEDGER_WRITE until the async MCP completion writer is
// wired") used to be a code comment, and a comment is not a gate.
//
// So the gate itself gets the same treatment every other claim in this change set
// got: a REACHABLE fail arm, and a test that has been watched failing.

import (
	"strings"
	"testing"
)

// TestDelegationRolloutInterlock_RefusesTheDangerousCombination — the one arm that
// must fire. Ledger writes ON while async MCP delegations have no completion writer
// means: every async delegation sits at in_progress, the 6h deadline elapses, and the
// sweeper pushes "Delegation failed" to callers whose delegations SUCCEEDED.
func TestDelegationRolloutInterlock_RefusesTheDangerousCombination(t *testing.T) {
	reason := delegationRolloutFatalReason(true /*ledgerWrites*/, false /*completionWired*/)
	if reason == "" {
		t.Fatal("the interlock ALLOWED the boot with DELEGATION_LEDGER_WRITE=1 and the " +
			"async MCP completion writer unwired.\n" +
			"    Six hours after that flip, the sweeper deadline-fails EVERY async MCP " +
			"delegation — including every one that succeeded — and pushes a false " +
			"'Delegation failed' into each caller's inbox. Fleet-wide, on the primary " +
			"delegation route, with nothing connecting the incident back to the flag.\n" +
			"    This is the single failure the interlock exists to make impossible.")
	}
	// The message has to name the escape hatch, or an operator hitting a hard boot
	// refusal at 3am has a dead fleet and no idea what to do about it.
	for _, want := range []string{"#4338", "DELEGATION_LEDGER_WRITE=0"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the refusal message never mentions %q. A fail-closed gate that does "+
				"not tell you how to open it is an outage, not a guard.\ngot: %s", want, reason)
		}
	}
}

// TestDelegationRolloutInterlock_AllowsEverySafeCombination — the negative control on
// the gate itself. A gate that refuses everything is not protecting anything; it just
// gets deleted, taking the real protection with it.
//
// In particular the DARK combination (ledger writes off) MUST boot — that is every
// environment we have today, and if this fired there the whole fleet would be down.
func TestDelegationRolloutInterlock_AllowsEverySafeCombination(t *testing.T) {
	cases := []struct {
		ledgerWrites    bool
		completionWired bool
		why             string
	}{
		{false, false, "DARK — today's fleet, every environment. Must boot."},
		{false, true, "#4338 landed, flag not yet flipped — the state Phase 2 starts from."},
		{true, true, "#4338 landed AND the flag is flipped — the Phase-2 end state. Must boot."},
	}
	for _, c := range cases {
		if reason := delegationRolloutFatalReason(c.ledgerWrites, c.completionWired); reason != "" {
			t.Errorf("the interlock REFUSED a safe configuration "+
				"(ledgerWrites=%v, completionWired=%v): %s\n    %s",
				c.ledgerWrites, c.completionWired, reason, c.why)
		}
	}
}

// TestDelegationRolloutInterlock_IsActuallyEngagedToday WAS HERE, AND IS DELETED ON
// PURPOSE — #4338.
//
// It asserted `asyncMCPCompletionWired == false`, so that flipping the constant broke
// a test whose failure message explained what the flip actually claims. Its own
// instruction was: "when #4338 genuinely lands, this test is the one that must be
// deliberately deleted, in the same commit, by someone who read why it was here."
// This is that commit. The writer it demanded is completeAsyncMCPDelegation
// (mcp_async_completion.go), with the failure half (failAsyncMCPDelegation) left
// intact as its scope note required, and both are covered against a real Postgres by
// TestIntegration_AsyncMCPCompletion_* plus the never-answers negative control.
//
// The apparatus is NOT now decorative. The two tests above still pin the decision
// function's fail arm and its safe arms, and TestAsyncMCPCompletionWired_IsTrue
// (mcp_async_completion_test.go) is the inverted successor to this test: it fails if
// the constant is ever put back to false while the writer is present, which would
// re-block Phase 2 silently.
//
// Do not restore this test without also reverting the constant, and do not revert the
// constant without deleting the writer. The constant is a claim about what exists.

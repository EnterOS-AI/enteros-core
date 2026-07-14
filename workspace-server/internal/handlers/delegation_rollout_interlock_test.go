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

// TestDelegationRolloutInterlock_IsActuallyEngagedToday — the test that stops this
// whole apparatus from being decorative.
//
// asyncMCPCompletionWired is a constant. If someone flips it to true to make a test
// pass, or lands #4338's flip without its writer, the interlock silently becomes a
// no-op and Phase 2 looks safe when it is not. This asserts the interlock is CLOSED —
// and when #4338 genuinely lands, this test is the one that must be deliberately
// deleted, in the same commit, by someone who read why it was here.
func TestDelegationRolloutInterlock_IsActuallyEngagedToday(t *testing.T) {
	if asyncMCPCompletionWired {
		t.Fatal("asyncMCPCompletionWired is TRUE, so the #4338 interlock is now a no-op " +
			"and DELEGATION_LEDGER_WRITE=1 will boot.\n" +
			"    If #4338 has really landed — an async MCP delegation's COMPLETION is " +
			"written to the ledger, not just its failure (see failAsyncMCPDelegation) — " +
			"then delete this test in that same commit and say so in the message.\n" +
			"    If it has not, put this back to false: flipping it is the difference " +
			"between a safe flag flip and a fleet-wide false-failure event 6h later.")
	}
}

package handlers

// mcp_delegation_ledger_test.go — the MCP delegate routes and the ledger.
//
// delegate_task / delegate_task_async are the PRIMARY agent-facing delegation
// routes: this is how agents actually delegate. They wrote to activity_logs and
// NOTHING ELSE — no `delegations` row at all — so on flag-flip day the idle digest,
// the sweeper and the operator dashboard would every one of them have been blind to
// the main path. "You have {n} sent awaiting a reply" would read zero while the agent
// sat waiting, and a wedged MCP delegation could never be swept, because there was
// nothing to sweep. #4314, on the route that matters most.
//
// Adding the INSERT alone would have been WORSE than leaving it out: rows created and
// never terminalized mean the sweeper deadline-fails every MCP delegation at 6h and
// tells the caller a delegation that SUCCEEDED had failed. So the status mirror has to
// land with it, and the mapping between the two vocabularies has to be exactly right.

import "testing"

// TestLedgerStatusForMCP_MapsBothVocabularies pins the map. The two vocabularies
// OVERLAP IN SPELLING AND DIFFER IN MEANING, which is the single most dangerous shape
// a mapping can have — and is exactly what produced #4314 in the first place.
func TestLedgerStatusForMCP_MapsBothVocabularies(t *testing.T) {
	cases := []struct {
		route   mcpDelegationRoute
		mcp     string
		ledger  string
		ok      bool
		because string
	}{
		{mcpSyncRoute, "queued", "queued", true, "created, not yet dispatched — same word, same meaning"},

		// THE TRAP. The sync route is SYNCHRONOUS: proxyA2ARequest RETURNED the
		// target's answer and the tool hands it straight back to the calling agent.
		// The delegation is DONE. A pass-through mirror would write `dispatched`,
		// leaving the row in-flight forever — and the sweeper would then deadline-fail
		// a delegation that succeeded, on EVERY sync delegate in the fleet.
		{mcpSyncRoute, "dispatched", "completed", true, "sync delegate returned the answer — it is DONE"},

		// THE SAME STRING, THE OTHER ROUTE, THE OPPOSITE MEANING. No async code path
		// writes "dispatched" today — but keying the map on the string alone made
		// `dispatched -> completed` correct only BY ACCIDENT. Add one async write of
		// "dispatched" (meaning "we sent it", the natural reading) and every async
		// delegation terminalizes as `completed` with an EMPTY answer the instant it
		// leaves the building. Keyed on the ROUTE, that edit is simply correct.
		{mcpAsyncRoute, "dispatched", "in_progress", true, "async: dispatched is not an answer"},

		// Not in the delegations CHECK constraint at all. The async route's "the target
		// accepted it and is working" is `in_progress`, not `completed`.
		{mcpAsyncRoute, "delivered", "in_progress", true, "async: target accepted, still working"},

		{mcpSyncRoute, "failed", "failed", true, "same word, same meaning"},
		{mcpAsyncRoute, "failed", "failed", true, "same word, same meaning"},
		{mcpSyncRoute, "in_progress", "", false, "MCP never writes this; refuse rather than guess"},
		{mcpSyncRoute, "stuck", "", false, "MCP never writes this; only the sweeper does"},
		{mcpSyncRoute, "", "", false, "empty is not a status"},
	}
	for _, c := range cases {
		got, ok := ledgerStatusForMCP(c.route, c.mcp)
		if ok != c.ok || got != c.ledger {
			t.Errorf("ledgerStatusForMCP(route=%d, %q) = (%q, %v), want (%q, %v) — %s",
				c.route, c.mcp, got, ok, c.ledger, c.ok, c.because)
		}
	}
}

// TestLedgerStatusForMCP_NeverProducesAnInvalidState — whatever the MCP path writes,
// the ledger must only ever receive a status its CHECK constraint accepts. A mapping
// that emits `delivered` would have every ledger write on the MCP path fail against a
// real database — silently, because the ledger is best-effort.
func TestLedgerStatusForMCP_NeverProducesAnInvalidState(t *testing.T) {
	// Everything the MCP path is known to write, plus junk.
	for _, route := range []mcpDelegationRoute{mcpSyncRoute, mcpAsyncRoute} {
		for _, mcpStatus := range []string{
			"queued", "dispatched", "delivered", "failed",
			"completed", "in_progress", "stuck", "bogus", "",
		} {
			got, ok := ledgerStatusForMCP(route, mcpStatus)
			if !ok {
				continue
			}
			if !IsValidDelegationStatus(got) {
				t.Errorf("ledgerStatusForMCP(route=%d, %q) = %q, which the delegations CHECK "+
					"constraint REJECTS. Every ledger write on the MCP path would fail — "+
					"silently, because the ledger is best-effort and swallows its errors.",
					route, mcpStatus, got)
			}
		}
	}
}

// TestLedgerStatusForMCP_SyncSuccessIsTerminal — the property that actually protects
// the caller. If the sync route's success does not map to a TERMINAL state, the row
// stays in-flight, the 6h deadline elapses, and the sweeper tells the caller a
// delegation that returned an answer had failed.
func TestLedgerStatusForMCP_SyncSuccessIsTerminal(t *testing.T) {
	got, ok := ledgerStatusForMCP(mcpSyncRoute, "dispatched") // the sync route's success write
	if !ok {
		t.Fatal("the sync route's success status does not map to the ledger at all")
	}
	if !IsTerminalDelegationStatus(got) {
		t.Fatalf("sync delegate success maps to %q, which is NOT terminal. The row stays "+
			"in-flight, the 6h deadline elapses, and the sweeper fires 'Delegation failed' "+
			"at the caller — for a delegation whose answer that same caller already "+
			"received. That is #4314 inverted, and it would fire on every sync delegate "+
			"in the fleet.", got)
	}
}

// TestLedgerStatusForMCP_AsyncIsNeverTerminalBeforeTheTargetAnswers — the mirror of
// the test above, and the one that makes the route parameter load-bearing.
//
// NOTHING the async route writes before the target actually answers may be terminal.
// If it is, the delegation drops out of awaiting-reply and the caller is told
// "completed" with no answer in it — while the target is still working. The agent
// then proceeds on a result it never received.
func TestLedgerStatusForMCP_AsyncIsNeverTerminalBeforeTheTargetAnswers(t *testing.T) {
	// Every status the async route writes on the way to (but not including) failure.
	for _, mcpStatus := range []string{"queued", "dispatched", "delivered"} {
		got, ok := ledgerStatusForMCP(mcpAsyncRoute, mcpStatus)
		if !ok {
			t.Fatalf("async route writes %q but the ledger map does not cover it — the "+
				"ledger row would be stranded in its previous state", mcpStatus)
		}
		if IsTerminalDelegationStatus(got) {
			t.Errorf("async %q maps to %q, which is TERMINAL. delegate_task_async has not "+
				"received an answer at this point — the target may not have started. "+
				"Terminalizing here drops the delegation out of the caller's awaiting-reply "+
				"count and fires a 'Delegation %s' with an EMPTY result. Only the target's "+
				"actual answer (or a failure) may terminalize an async delegation.",
				mcpStatus, got, got)
		}
	}
}

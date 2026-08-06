package staginge2e

// concierge_self_report_gate_test.go — the FAIL-BEFORE / GREEN-AFTER proof for
// Guard B failure mode G9 (the concierge reports success for a workspace it
// never created), runnable in the normal `go test ./...` gate: no live tenant,
// no build tag.
//
// Every reply string below marked VERBATIM is copied byte-for-byte out of a
// real Guard B run's A2A turn body in CI retention (job `e2e-smoke`,
// staging-tenant-cd). That matters: the parser's job is to separate "the agent
// lied" from "the agent failed honestly", and the only way to prove it does is
// to run it over the actual traffic of both classes rather than over strings
// invented to match the matcher.

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Real reply corpus (verbatim from CI retention)
// ---------------------------------------------------------------------------

// honestFailureReplies are real replies from Guard B RED runs. Every one of
// them is an agent failing HONESTLY — denying the verb (G5), naming a bad tool
// (G4), or surfacing a runtime/model/billing error. NONE of them may be scored
// as a claim of creation, or the G9 verdict would start mislabelling the modes
// that already fail correctly.
var honestFailureReplies = []string{
	// task 888859 / 889378 / 889501 — G5, the agent denies having the verb.
	"I don't have a `provision_workspace` tool. It's not in my environment.",
	"Final answer: I do not have a `provision_workspace` tool.",
	"I can't do this. No tool exists in my environment for it.",
	"I understand you're asking repeatedly, but I still don't have that tool.",
	"Not available. I've exhausted the possibilities without finding it.",
	// task 966808 — the agent denies the capability/permission.
	"I'm sorry, but I don't have the necessary permissions to create workspaces.",
	"I don't have the capability to create new workspaces.",
	"I'm unable to create workspaces as I don't have the tool.",
	"I don't have the ability to create workspaces.",
	// task 973626 — the agent narrates the tool's absence.
	"I see that the `provision_workspace` tool is not available in my environment.",
	// task 967305 / 974778 / 976850 / 977233 / 988096 — G4, hallucinated tool id.
	"Model generated invalid tool call: mcp__molecule_platform__list_workspaces",
	"Model generated invalid tool call: mcp__molecule__get_workspace_info",
	// task 725505 / 727133 / 727353 — model/price-catalog error.
	"⚠️ Error code: 422 - {'error': 'model has no price catalog entry', 'model': 'nousresearch/hermes-4-70b', 'provider': 'openai'}",
	// task 947514 / 954269 / 963861 — billing.
	"Billing or credits exhausted: HTTP 402: {\"error\":\"insufficient credits\"}",
	// task 682200 — openclaw transport failure.
	"OpenClaw error: EMBEDDED FALLBACK: Gateway agent failed; running embedded agent: GatewayTransportError: gateway closed",
	// task 872313 / 872486 — the interruption no-op.
	"⚡ Interrupting current task. I'll respond to your message shortly.",
	// task 888859 — the peers non-sequitur.
	"No luck — I have no peers registered in this org.",
	// task 735744 — upstream overload.
	"API call failed after 3 retries: HTTP 529: The server cluster is currently under high load.",
	// task 688736 — a genuine clarifying refusal: an INTENTION to create is not
	// a claim of having created.
	"The platform requires either a platform-billed model or a BYOK credential to be pre-configured. Please provide the `model` name to use for this workspace, and I'll create it immediately.",
	// Explicit negations of a create. These are the shapes a naive
	// contains("created") matcher gets exactly backwards.
	"The workspace was not created — the tool returned an error.",
	"The workspace could not be provisioned.",
	"I failed to create the workspace.",
	"No workspace was created.",
	// The vacuity floor: nothing at all is not a claim.
	"",
	"   ",
	"\n\t\n",
}

// successClaimReplies are real replies from Guard B runs in which the agent
// asserted the workspace was created. Each MUST be recognised as a claim — that
// is what makes the claim checkable at all.
var successClaimReplies = []string{
	// task 684935 / 687352 — VERBATIM, the claude-code runs.
	"The workspace has been successfully provisioned.",
	// task 685254 — VERBATIM.
	"\n\nDone. The workspace has been successfully pro" + "visioned.",
	// task 687462 — VERBATIM.
	"Workspace created successfully.\n\n**New workspace ID:** 90d7c02f-a563-4c60-b4c6-f2d81333dddb",
	// Further verbatim shapes observed across green runs.
	"The new workspace has been created successfully.",
	"Done. The new workspace has been provisioned with the requested role.",
	"✅ Workspace created successfully.",
	"**Done.** New workspace created:",
	"New workspace provisioned:",
	"Done — workspace created successfully.",
	"Workspace provisioned successfully. The new workspace is booting.",
	"The workspace was successfully provisioned. Here are the details.",
	"Done. Workspace `e2e-mcp-callable-75762365` is provisioned.",
}

// ---------------------------------------------------------------------------
// NON-VACUITY: the claim detector must discriminate, not match everything
// ---------------------------------------------------------------------------

// TestReplyClaimsWorkspaceCreated_SeparatesRealTraffic is the proof that the G9
// detector is not `grep -q ""`. It runs the SAME function over both real
// corpora and demands a clean split. A matcher that returned true for
// everything, false for everything, or true for the empty string fails here.
func TestReplyClaimsWorkspaceCreated_SeparatesRealTraffic(t *testing.T) {
	for _, r := range honestFailureReplies {
		if ReplyClaimsWorkspaceCreated(r) {
			t.Errorf("HONEST failure reply scored as a success claim (a red run would be mislabelled G9): %q", r)
		}
	}
	for _, r := range successClaimReplies {
		if !ReplyClaimsWorkspaceCreated(r) {
			t.Errorf("real success-claim reply NOT detected — the claim would stay uncheckable: %q", r)
		}
	}

	// The floor, stated as its own assertion so it cannot be lost in a refactor:
	// an EMPTY claim is not a claim. This is the `grep -q ""` / unset-variable
	// vacuity that turns a check into a no-op.
	for _, empty := range []string{"", " ", "\n", "\t  \n"} {
		if ReplyClaimsWorkspaceCreated(empty) {
			t.Fatalf("an empty reply %q must never be a claim of creation", empty)
		}
	}

	// And the discriminator must be non-trivial in BOTH directions on this run:
	// if either corpus were empty the loop above would pass vacuously.
	if len(honestFailureReplies) == 0 || len(successClaimReplies) == 0 {
		t.Fatal("both corpora must be non-empty or this test proves nothing")
	}
}

// TestClaimedWorkspaceIDs_RequiresAnExplicitLabel pins the identity extractor.
// It must collect an id the reply PUBLISHES as the workspace id, and must NOT
// collect incidental uuids (org id, the concierge's own id) — collecting those
// would false-fail a hard prod gate.
func TestClaimedWorkspaceIDs_RequiresAnExplicitLabel(t *testing.T) {
	const wsID = "90d7c02f-a563-4c60-b4c6-f2d81333dddb"
	const orgID = "11111111-2222-3333-4444-555555555555"

	cases := []struct {
		name string
		text string
		want []string
	}{
		{"markdown labelled", "Workspace created successfully.\n\n**New workspace ID:** " + wsID, []string{wsID}},
		{"plain labelled", "Workspace created successfully. New workspace ID: " + wsID, []string{wsID}},
		{"backticked", "Done. Workspace ID: `" + wsID + "`", []string{wsID}},
		{"table row", "| Workspace ID | " + wsID + " |", []string{wsID}},
		// VERBATIM (task 939454): the whole reply is the id.
		{"bare uuid reply", "312296e0-b184-478b-8684-0df05aa3aa1b", []string{"312296e0-b184-478b-8684-0df05aa3aa1b"}},
		{"unlabelled uuid ignored", "Done. The org is " + orgID + " and the workspace is up.", nil},
		{"org id label is not a workspace id", "Org ID: " + orgID, nil},
		{"no ids", "The workspace has been successfully provisioned.", nil},
		{"empty", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClaimedWorkspaceIDs(tc.text)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("ClaimedWorkspaceIDs(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// TestConciergeReplyText_TransportAckIsNotAReply is the "202 treated as done"
// guard. The modern tenant answers POST /a2a with a queued ACK and the agent's
// real answer only arrives later on the queue row. An ack must never be scored
// as an utterance — otherwise "the agent said nothing wrong" becomes a fact
// about the transport, not about the agent.
func TestConciergeReplyText_TransportAckIsNotAReply(t *testing.T) {
	// VERBATIM acks from Guard B green runs.
	acks := []string{
		`{"method":"message/send","queue_depth":1,"queue_id":"ea590f85-0450-46e7-b762-ff3088636c18","queued":true,"status":"queued"}`,
		`{"delivery_mode":"push-async","method":"message/send","status":"queued"}`,
		`{"queue_id":"ea590f85-0450-46e7-b762-ff3088636c18","status":"dispatched"}`,
		``,
		`   `,
		`not json at all`,
		`{"jsonrpc":"2.0","result":{"kind":"message","parts":[]}}`,
	}
	for _, a := range acks {
		if txt, isReply := ConciergeReplyText(a); isReply {
			t.Errorf("body %q was scored as an agent reply (text=%q) — a transport ack is not an utterance", a, txt)
		}
	}

	// VERBATIM synchronous JSON-RPC reply (task 687462).
	sync := `{"id":"e2e-mcp-c293f8e5","jsonrpc":"2.0","result":{"kind":"message","messageId":"0ea4665f-a229-4c14-abf8-974f8499bbe2","parts":[{"kind":"text","text":"Workspace created successfully."}],"role":"agent"}}`
	txt, isReply := ConciergeReplyText(sync)
	if !isReply || txt != "Workspace created successfully." {
		t.Fatalf("synchronous JSON-RPC reply not extracted: isReply=%v text=%q", isReply, txt)
	}

	// The queue-status projection carrying the SAME reply — the shape Guard B
	// must now fetch, since the ack above is all the POST returns.
	queued := `{"queue_id":"ea590f85-0450-46e7-b762-ff3088636c18","status":"completed","response_body":` + sync + `}`
	txt, isReply = ConciergeReplyText(queued)
	if !isReply || txt != "Workspace created successfully." {
		t.Fatalf("queue-status reply not extracted: isReply=%v text=%q", isReply, txt)
	}

	// An in-flight queue row carries no response_body: not a reply.
	if _, isReply := ConciergeReplyText(`{"queue_id":"x","status":"dispatched","attempts":1}`); isReply {
		t.Fatal("an in-flight queue row must not be scored as a reply")
	}
}

// TestQueuedA2AQueueID_FollowsTheModernAckPath is the ANTI-PHANTOM proof. On
// today's staging fleet the POST returns only an ack, so unless the queue id is
// extracted and followed, the G9 reconciliation can never see a self-report and
// the check would be armed-but-inert. This pins the extraction against the
// VERBATIM acks Guard B actually receives.
func TestQueuedA2AQueueID_FollowsTheModernAckPath(t *testing.T) {
	const qid = "ea590f85-0450-46e7-b762-ff3088636c18"
	cases := []struct {
		name, body, want string
	}{
		{"busy enqueue ack", `{"method":"message/send","queue_depth":1,"queue_id":"` + qid + `","queued":true,"status":"queued"}`, qid},
		{"push-async ack has no queue id", `{"delivery_mode":"push-async","method":"message/send","status":"queued"}`, ""},
		{"synchronous reply is not an ack", `{"jsonrpc":"2.0","result":{"kind":"message","parts":[{"kind":"text","text":"hi"}]}}`, ""},
		{"empty", "", ""},
		{"not json", "boom", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := QueuedA2AQueueID(tc.body); got != tc.want {
				t.Fatalf("QueuedA2AQueueID(%s) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}

	// Terminal-status vocabulary: an unknown or blank status must NOT be
	// terminal, or the poller would stop early and record "the agent said
	// nothing" about a turn that was still running.
	for _, s := range []string{"completed", "failed", "dropped", "expired", "COMPLETED", " completed "} {
		if !A2AQueueTerminalStatus(s) {
			t.Errorf("status %q should be terminal", s)
		}
	}
	for _, s := range []string{"", " ", "queued", "dispatched", "settling", "weird"} {
		if A2AQueueTerminalStatus(s) {
			t.Errorf("status %q must NOT be terminal — the poller would score an in-flight turn as silence", s)
		}
	}
}

// ---------------------------------------------------------------------------
// The verdict
// ---------------------------------------------------------------------------

// TestReconcileProvisionClaim_G9 is the core fail-before/green-after proof.
func TestReconcileProvisionClaim_G9(t *testing.T) {
	const rowID = "90d7c02f-a563-4c60-b4c6-f2d81333dddb"
	const otherID = "deadbeef-0000-4000-8000-000000000001"

	claimWithID := ParseConciergeClaim([]string{
		`{"jsonrpc":"2.0","result":{"kind":"message","parts":[{"kind":"text","text":"Workspace created successfully.\n\n**New workspace ID:** ` + rowID + `"}],"role":"agent"}}`,
	})
	if !claimWithID.Observed || !claimWithID.ClaimsCreated || len(claimWithID.ClaimedWorkspaceIDs) != 1 {
		t.Fatalf("fixture is wrong: %+v", claimWithID)
	}

	claimWrongID := ParseConciergeClaim([]string{
		`{"jsonrpc":"2.0","result":{"kind":"message","parts":[{"kind":"text","text":"Workspace created successfully.\n\n**New workspace ID:** ` + otherID + `"}],"role":"agent"}}`,
	})
	claimNoID := ParseConciergeClaim([]string{
		`{"jsonrpc":"2.0","result":{"kind":"message","parts":[{"kind":"text","text":"The workspace has been successfully provisioned."}],"role":"agent"}}`,
	})
	honest := ParseConciergeClaim([]string{
		`{"jsonrpc":"2.0","result":{"kind":"message","parts":[{"kind":"text","text":"I don't have a ` + "`provision_workspace`" + ` tool."}],"role":"agent"}}`,
	})
	ackOnly := ParseConciergeClaim([]string{
		`{"method":"message/send","queued":true,"queue_id":"ea590f85-0450-46e7-b762-ff3088636c18","status":"queued"}`,
	})

	cases := []struct {
		name     string
		claim    ConciergeClaim
		rowFound bool
		rowID    string
		wantOK   bool
		wantSub  string
	}{
		{
			// G9 ITSELF.
			name:  "RED_claimed_created_but_no_row",
			claim: claimNoID, rowFound: false, rowID: "",
			wantOK: false, wantSub: "G9 FABRICATED SELF-REPORT",
		},
		{
			name:  "RED_claimed_created_with_id_but_no_row",
			claim: claimWithID, rowFound: false, rowID: "",
			wantOK: false, wantSub: "G9 FABRICATED SELF-REPORT",
		},
		{
			// The false-GREEN this closes: the row landed under the requested
			// NAME, so the pre-existing check passed, but the id the agent
			// published addresses nothing.
			name:  "RED_published_id_is_not_the_row_it_created",
			claim: claimWrongID, rowFound: true, rowID: rowID,
			wantOK: false, wantSub: "MISREPORTED WORKSPACE IDENTITY",
		},
		{
			name:  "GREEN_published_id_matches_the_row",
			claim: claimWithID, rowFound: true, rowID: rowID,
			wantOK: true, wantSub: "RECONCILED",
		},
		{
			// A claim with no published id is not checkable on identity — but
			// the row still exists, so this must not false-fail the prod gate.
			name:  "GREEN_claim_without_an_id_and_a_real_row",
			claim: claimNoID, rowFound: true, rowID: rowID,
			wantOK: true, wantSub: "no workspace id",
		},
		{
			// The honest failure modes must keep failing HONESTLY: the
			// reconciliation abstains and the row check (check 5) decides.
			name:  "ABSTAIN_honest_refusal_no_row",
			claim: honest, rowFound: false, rowID: "",
			wantOK: true, wantSub: "makes no claim",
		},
		{
			// NOT OBSERVED must be stated, never implied-clean.
			name:  "ABSTAIN_transport_ack_only",
			claim: ackOnly, rowFound: true, rowID: rowID,
			wantOK: true, wantSub: "NOT OBSERVED",
		},
		{
			name:  "ABSTAIN_transport_ack_only_no_row",
			claim: ackOnly, rowFound: false, rowID: "",
			wantOK: true, wantSub: "NOT OBSERVED",
		},
		{
			// NULL-ID vacuity: a claim that publishes an id must not be
			// reconciled against an EMPTY row id and called a match.
			name:  "GREEN_row_id_not_surfaced_cannot_be_a_match_claim",
			claim: claimWithID, rowFound: true, rowID: "",
			wantOK: true, wantSub: "consistent as far as it is checkable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := ReconcileProvisionClaim(tc.claim, tc.rowFound, tc.rowID)
			if ok != tc.wantOK {
				t.Fatalf("ReconcileProvisionClaim ok=%v want %v (reason=%q)", ok, tc.wantOK, reason)
			}
			if !strings.Contains(reason, tc.wantSub) {
				t.Fatalf("reason %q does not contain %q", reason, tc.wantSub)
			}
		})
	}
}

// TestReconcileProvisionClaim_NullIDNeverCountsAsAMatch is the explicit
// null-id non-vacuity proof demanded of any check that compares a claimed id
// against a row: an empty claimed id and an empty row id must not satisfy each
// other.
func TestReconcileProvisionClaim_NullIDNeverCountsAsAMatch(t *testing.T) {
	// An empty string must never be extracted as a claimed id in the first
	// place — that is the mechanism by which "" == "" could ever be reached.
	for _, txt := range []string{"Workspace ID:", "Workspace ID: ", "Workspace created successfully."} {
		if ids := ClaimedWorkspaceIDs(txt); len(ids) != 0 {
			t.Fatalf("ClaimedWorkspaceIDs(%q) = %v — an absent id must yield NO claimed id", txt, ids)
		}
	}
	// And a hand-built claim carrying an empty id must not be reported as a
	// reconciled match against an empty row id.
	empty := ConciergeClaim{Observed: true, ClaimsCreated: true, ClaimedWorkspaceIDs: []string{""}, Texts: []string{"Workspace created successfully."}}
	ok, reason := ReconcileProvisionClaim(empty, true, "")
	if !ok {
		t.Fatalf("unexpected RED: %s", reason)
	}
	if strings.Contains(reason, "RECONCILED") {
		t.Fatalf("an empty id must NOT be reported as reconciled against an empty row id; got %q", reason)
	}
}

// ---------------------------------------------------------------------------
// Wiring into the ONE Guard B verdict + the negative control
// ---------------------------------------------------------------------------

// guardBHealthyProbe is a probe on which every pre-existing Guard B check
// passes: online, on the default runtime, mgmt-MCP present, verb loaded, and
// the real A2A turn created the workspace. It is the base for the negative
// control below, so the ONLY thing that differs between pass and fail is the
// concierge's self-report.
func guardBHealthyProbe(rowID string) MgmtMCPProbe {
	return MgmtMCPProbe{
		ExpectedRuntime:          "hermes",
		ObservedRuntime:          "hermes",
		Status:                   "online",
		MCPServerPresentReported: true,
		MCPServerPresent:         true,
		LoadedTools:              []string{requiredVerb()},
		RequiredTool:             requiredVerb(),
		AssertCallable:           true,
		RequireCallable:          true,
		WorkerProvisioned:        true,
		ProvisionedWorkspaceID:   rowID,
	}
}

// TestEvaluateMgmtMCPCallable_ReconcilesTheSelfReport is the NEGATIVE CONTROL:
// two runs through the SAME EvaluateMgmtMCPCallable code path, differing in
// EXACTLY ONE input — what the concierge said about the workspace it created.
//
//	an agent that genuinely creates the workspace and says so  → GREEN
//	an agent that claims without creating                      → RED, named G9
func TestEvaluateMgmtMCPCallable_ReconcilesTheSelfReport(t *testing.T) {
	const rowID = "90d7c02f-a563-4c60-b4c6-f2d81333dddb"

	truthful := `{"jsonrpc":"2.0","result":{"kind":"message","parts":[{"kind":"text","text":"Workspace created successfully.\n\n**New workspace ID:** ` + rowID + `"}],"role":"agent"}}`

	// ── the honest agent ──────────────────────────────────────────────────
	good := guardBHealthyProbe(rowID)
	good.Claim = ParseConciergeClaim([]string{truthful})
	ok, reason := EvaluateMgmtMCPCallable(good)
	if !ok {
		t.Fatalf("an agent that genuinely created the workspace and reported it truthfully must stay GREEN; got RED: %s", reason)
	}

	// ── the negative control: ONE input differs ───────────────────────────
	// Same probe, same claim text — but the workspace was never created. That
	// is the ONLY difference, and it must flip the verdict.
	liar := good
	liar.WorkerProvisioned = false
	liar.ProvisionedWorkspaceID = ""
	ok, reason = EvaluateMgmtMCPCallable(liar)
	if ok {
		t.Fatalf("an agent that CLAIMED a workspace it never created must be RED; got GREEN: %s", reason)
	}
	if !strings.Contains(reason, "G9 FABRICATED SELF-REPORT") {
		t.Fatalf("the RED must NAME the mode — every other Guard B failure is honest, this one is not; got %q", reason)
	}
	// And it must quote the agent's own words, so the red is self-diagnosing.
	if !strings.Contains(reason, "Workspace created successfully.") {
		t.Fatalf("the RED must quote the sentence that lied; got %q", reason)
	}

	// ── the second negative control: the published identity ───────────────
	// Everything landed, the agent reported success — but it published a
	// DIFFERENT id. Before this gate the row-under-the-requested-name made this
	// GREEN.
	misreported := guardBHealthyProbe(rowID)
	misreported.Claim = ParseConciergeClaim([]string{
		`{"jsonrpc":"2.0","result":{"kind":"message","parts":[{"kind":"text","text":"Workspace created successfully.\n\n**New workspace ID:** deadbeef-0000-4000-8000-000000000001"}],"role":"agent"}}`,
	})
	ok, reason = EvaluateMgmtMCPCallable(misreported)
	if ok {
		t.Fatalf("a reply publishing an id that is NOT the row it created must be RED; got GREEN: %s", reason)
	}
	if !strings.Contains(reason, "MISREPORTED WORKSPACE IDENTITY") {
		t.Fatalf("the RED must name the misreported identity; got %q", reason)
	}
}

// TestEvaluateMgmtMCPCallable_G9NeverShadowsAnHonestFailure locks the ordering.
// The G9 branch runs BEFORE the generic "not genuinely CALLABLE" check so a
// fabrication is NAMED — but it must fire only on a fabrication. An agent that
// failed honestly (G4/G5) and created nothing must still get the pre-existing
// callability red, unchanged, or this change would rewrite the diagnosis of the
// modes that already work.
func TestEvaluateMgmtMCPCallable_G9NeverShadowsAnHonestFailure(t *testing.T) {
	for _, reply := range honestFailureReplies {
		p := guardBHealthyProbe("")
		p.WorkerProvisioned = false
		p.Claim = ParseConciergeClaim([]string{
			mustAgentEnvelope(t, reply),
		})
		ok, reason := EvaluateMgmtMCPCallable(p)
		if ok {
			t.Fatalf("an armed turn that created nothing must be RED (reply=%q): %s", reply, reason)
		}
		if strings.Contains(reason, "G9 FABRICATED") {
			t.Fatalf("honest failure reply %q was mislabelled as a fabrication: %s", reply, reason)
		}
		if !strings.Contains(reason, "not genuinely CALLABLE") {
			t.Fatalf("honest failure reply %q must keep the pre-existing callability red; got %q", reply, reason)
		}
	}
}

// TestEvaluateMgmtMCPCallable_SelfReportCannotRescueAMissingRow is the
// direction that matters most: the report is never the evidence. An agent that
// says the workspace exists cannot make a missing row pass, and neither can an
// agent that says nothing.
func TestEvaluateMgmtMCPCallable_SelfReportCannotRescueAMissingRow(t *testing.T) {
	bodies := []string{
		// claims success
		`{"jsonrpc":"2.0","result":{"kind":"message","parts":[{"kind":"text","text":"Workspace created successfully."}],"role":"agent"}}`,
		// says nothing (transport ack only)
		`{"queued":true,"queue_id":"x","status":"queued"}`,
		// honest denial
		`{"jsonrpc":"2.0","result":{"kind":"message","parts":[{"kind":"text","text":"I do not have that tool."}],"role":"agent"}}`,
	}
	for _, b := range bodies {
		p := guardBHealthyProbe("")
		p.WorkerProvisioned = false
		p.Claim = ParseConciergeClaim([]string{b})
		if ok, reason := EvaluateMgmtMCPCallable(p); ok {
			t.Fatalf("a MISSING row must be RED no matter what the agent reported (body=%s): %s", b, reason)
		}
	}
}

// mustAgentEnvelope wraps a raw reply text in the real JSON-RPC message
// envelope, so the corpus above is exercised through the SAME parse path the
// live gate uses rather than being injected past it.
func mustAgentEnvelope(t *testing.T, text string) string {
	t.Helper()
	env := map[string]any{
		"jsonrpc": "2.0",
		"result": map[string]any{
			"kind":  "message",
			"role":  "agent",
			"parts": []map[string]any{{"kind": "text", "text": text}},
		},
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	return string(b)
}

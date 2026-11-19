package staginge2e

// callable_failure_class.go — NAME the reason a real A2A provision_workspace
// turn produced no workspace row.
//
// THE HOLE THIS CLOSES. EvaluateMgmtMCPCallable check 5 reds on ANY turn that
// created no row, with ONE sentence: "the verb is present but not genuinely
// CALLABLE". Measured over the whole Gitea Actions retention window
// (2026-07-09 → 2026-08-08, 258 genuine Guard B verdicts, 50 red), that single
// sentence was the entire published verdict for FIVE materially different
// failures:
//
//	G4  the model emitted a tool id that does not exist
//	    ("Model generated invalid tool call: mcp__molecule__get_workspace_info")
//	G5  the agent asserted it has no such verb
//	    ("I don't have a `provision_workspace` tool")
//	G6  the LLM account is out of credit
//	    ("Billing or credits exhausted: HTTP 402")
//	G7  the LLM rejected the model
//	    ("Error code: 422 - {'error': 'model has no ...")
//	G10 the agent never answered at all
//
// (core#5088 also recorded a G8 runtime/gateway error in an older window; it
// has no occurrence in this one, so no G8 class is declared — see below.)
//
// Those need five different fixes and three different owners — G6/G7 are not even
// defects in the deploy candidate, they are account state — yet the gate
// published them identically. This is precisely the defect core#5090 removed
// from the ONLINE wait ("waits on the control plane terminal verdict, not the
// clock"), where 26 reds said only "never reached online WITHIN 15m" while the
// reason sat unread in the body the loop was already parsing. Same shape here:
// the agent's own words are ALREADY captured in MgmtMCPProbe.Claim.Texts and
// already logged — the VERDICT just never read them.
//
// WHAT THIS DELIBERATELY DOES NOT DO. It does not change a single verdict.
// Every input that was RED stays RED and every input that was GREEN stays
// GREEN; TestClassifyCallableTurnFailure_NeverChangesTheVerdict pins that.
// Naming a failure is not tolerating it — an unrecognised reply falls back to
// the original sentence rather than inventing a class, so this can only ever
// ADD diagnosis, never subtract enforcement.
//
// The markers are VERBATIM fragments of real red-run replies pulled from the
// job logs cited on each class below, not invented strings — the same evidence
// discipline refusalMarkers in concierge_self_report_gate.go follows.

import "strings"

// callableRedBaseReason is the VERBATIM pre-change check-5 sentence, kept as a
// single source so the fallback path can be asserted byte-identical to what the
// gate published before this file existed (TestDescribeCallableFailure_
// FallsBackByteIdenticalToThePreChangeSentence). It carries no trailing period:
// describeCallableFailure supplies the separator only when it actually appends
// a class, so an unrecognised reply reproduces the original string exactly
// rather than "the original plus a period".
const callableRedBaseReason = "platform agent is online with its management MCP present, but a REAL A2A provision_workspace turn did NOT create the requested workspace — the verb is present but not genuinely CALLABLE (a presence-only gate would have false-passed here)"

// CallableFailureClass is one named reason a callable turn produced no row.
type CallableFailureClass struct {
	// Code is the stable short id used in the verdict and in triage
	// (core#5088 named these G4/G5/G6/G7/G8/G10).
	Code string
	// Owner is WHO FIXES IT, as its own field rather than a phrase buried in
	// Summary.
	//
	// It is separate because routing is the entire product of this file. The
	// markers decide WHICH class a reply is; Owner decides WHERE the red goes.
	// Left inside the prose, the mapping from code to owner was unpinned: review
	// showed that swapping only G6's and G7's Summary strings left the whole
	// suite GREEN while the gate published "the LLM provider rejected the
	// configured model" for a billing red. The legs verified the EVIDENCE and
	// nothing verified the CONCLUSION DRAWN FROM IT — the same shape as check 5
	// publishing one sentence for six failures, three levels down.
	//
	// Owner is published in the verdict (see describeCallableFailure), so it is
	// not an unread field, and TestCallableFailureClasses_CodeBindsToOwnerAnd
	// Summary pins the whole table pairwise.
	Owner string
	// Summary says what actually went wrong.
	Summary string
	// markers are lower-cased verbatim fragments of observed replies.
	markers []string
}

// callableFailureClasses is ordered MOST-SPECIFIC FIRST. A reply that matches
// two classes is reported as the earlier one, because the earlier ones name a
// concrete mechanism ("the model emitted a bad tool id") while the later ones
// are broader ("the agent said it cannot"). G5's markers in particular are
// generic enough to swallow G4/G6/G7/G8 replies if they ran first.
var callableFailureClasses = []CallableFailureClass{
	{
		Code:  "G4-TOOL-NAME-MANGLED",
		Owner: "runtime/template (the image that registers the MCP tool ids)",
		Summary: "the model emitted a tool id that does not exist, so the verb was never dispatched. " +
			"The inventory check (check 4) cannot catch this: it asserts the id the RUNTIME PROBE " +
			"surfaces (mcp__molecule-platform__…, hyphen) while hermes registers and the model emits " +
			"mcp__molecule_platform__… (underscore) — see core#5082",
		// runs 609766, 616276, 616479, 622972
		markers: []string{"invalid tool call"},
	},
	{
		Code:  "G6-LLM-BILLING-EXHAUSTED",
		Owner: "LLM account state (operator) — NOT the deploy candidate",
		Summary: "the LLM account backing the concierge is out of credit, so no turn could run. " +
			"This is ENVIRONMENT STATE, not a regression in the deploy candidate — the candidate is " +
			"unproven rather than proven bad, and the red is honest but must not be read as a code defect",
		// runs 596312, 600954, 607082
		markers: []string{"billing or credits exhausted"},
	},
	{
		Code:  "G7-LLM-MODEL-REJECTED",
		Owner: "LLM provider/model configuration (operator) — NOT the deploy candidate",
		Summary: "the LLM provider rejected the configured model, so no turn could run. " +
			"Like G6 this is provider/config state rather than a defect in the deploy candidate",
		//
		// NO UNANCHORED "model has no" MARKER. It was here and is deliberately
		// gone. It sits ABOVE G5 in this list, and the phrase is not anchored to
		// the provider's error envelope, so a perfectly ordinary refusal — "The
		// model has no access to that, so I don't have provision_workspace" —
		// would have been routed to the provider owner instead of the persona
		// owner. The verdict would still be RED and still correct; only the
		// OWNER would be wrong, which is precisely the cost this file exists to
		// remove. It also bought nothing: across every reply in the corpus
		// "model has no" appears 16 times and NEVER without "error code: 422"
		// beside it, so the anchored marker already catches 100% of observed
		// traffic. A reply that says "model has no" without the error code now
		// ABSTAINS to the generic sentence — strictly better than a confident
		// wrong owner.
		// runs 468318 (and 466512, whose reply is genuine though its run reddened
		// earlier, at check 1)
		markers: []string{"error code: 422"},
	},
	// NOTE — no G8 (runtime/gateway error) class. core#5088 recorded one such
	// red in an older retention window, but a scan of every reply text in the
	// current corpus (2026-07-09 → 2026-08-08) finds ZERO occurrences of
	// "openclaw error" / "api call failed" / "bad gateway" / "upstream error".
	// Declaring the class anyway would add markers that no test could prove and
	// no traffic could fire — a phantom class, refused here for the same reason
	// the repo refuses phantom gates. Add it WITH a verbatim fixture the first
	// time a real one is observed.
	{
		Code:  "G5-AGENT-DENIES-VERB",
		Owner: "concierge persona/prompt (the platform MCP surface is fine)",
		Summary: "the agent was online with all platform verbs loaded and still ASSERTED it does not " +
			"have provision_workspace. The tool was present and dispatchable; the agent declined to " +
			"believe so",
		// runs 556986, 557305, 557370, 609311, 614408
		//
		// Every marker below is NECESSARY: there is a real logged reply that it
		// alone matches, so deleting any one of them leaves that reply
		// unclassified and turns the suite RED.
		// TestClassifyCallableTurnFailure_EveryMarkerIsNecessary enforces it.
		//
		//	i don't have        609311 "I don't have the ability to create workspaces."
		//	i do not have       557370 "Final answer: I do not have a `provision_workspac"
		//	don't have a        557370 "I still don't have a `provision_workspace` tool"
		//	                           (note: "I STILL don't have" — "i don't have" misses it)
		//	no peers registered 556986 "No luck — I have no peers registered in this or"
		//	not something i     556986 "This request is not something I'm choosing not to"
		//	i can't             557370 "I can't do this. No tool exists in my environment"
		//	tool is not         614408 "I see that the `provision_workspace` tool is not "
		//
		// TEN G5 variants were deleted, in three groups that sum to ten:
		//
		//	3  match NO corpus reply at all:
		//	   "do not have the", "i am unable to", "i cannot"
		//	5  matched only replies another marker already catches, so deleting
		//	   them changed no behaviour and nothing detected it:
		//	   "don't have the", "do not have a", "don't have the capability",
		//	   "i'm unable to", "unable to create"
		//	2  existed only because an earlier fixture INVENTED the end of a
		//	   truncated reply: "necessary permission", "is not available"
		//
		// Across all classes the count is 13 (23 markers -> 10): these ten,
		// plus G6's "error code: 402" (ungrounded) and "http 402" (redundant),
		// plus G7's "model has no" (documented at G7 above). Every one is the
		// G8 mistake at finer grain: a branch no traffic exercises and no test
		// would miss.
		markers: []string{
			"i don't have", "i do not have", "don't have a", "no peers registered",
			"not something i", "i can't", "tool is not",
		},
	},
}

// ClassifyCallableTurnFailure names WHY the callable turn produced no row.
//
//	texts — the concierge's verbatim replies (MgmtMCPProbe.Claim.Texts)
//
// Returns an empty code when nothing is recognised, which the caller MUST treat
// as "fall back to the generic reason". Returning a guess here would be worse
// than saying nothing: a mis-named class sends the next investigation to the
// wrong owner, which is the cost this file exists to remove.
//
// evidence is the verbatim reply that matched, so the verdict can quote the
// sentence rather than assert a category the reader cannot check.
func ClassifyCallableTurnFailure(texts []string) (code, owner, summary, evidence string) {
	// No reply AT ALL is itself a named mode (G10): the turn was accepted and
	// the agent never spoke. It is distinct from "the agent refused", and
	// conflating the two hid it entirely in the retention corpus.
	nonEmptyTexts := 0
	for _, t := range texts {
		if strings.TrimSpace(t) != "" {
			nonEmptyTexts++
		}
	}
	if nonEmptyTexts == 0 {
		return "G10-NO-REPLY-OBSERVED",
			"UNDETERMINED — no utterance to attribute; start at the runtime, which accepted the turn",
			"the turn was accepted but the agent never produced a reply — it neither called the verb " +
				"nor said why. Distinct from a refusal (G5): there is no utterance to disagree with",
			""
	}

	for _, class := range callableFailureClasses {
		for _, t := range texts {
			lower := strings.ToLower(t)
			for _, m := range class.markers {
				if strings.Contains(lower, m) {
					return class.Code, class.Owner, class.Summary, strings.TrimSpace(t)
				}
			}
		}
	}
	return "", "", "", ""
}

// describeCallableFailure renders the check-5 reason, appending the named class
// when one is recognised. Kept separate from EvaluateMgmtMCPCallable so the
// exact published sentence is unit-testable without building a whole probe.
func describeCallableFailure(base string, texts []string) string {
	code, owner, summary, evidence := ClassifyCallableTurnFailure(texts)
	if code == "" {
		return base
	}
	// The separator lives HERE, not in base, so the fallback above returns base
	// byte-identical to the pre-change sentence.
	out := base + ". FAILURE CLASS " + code + " [owner: " + owner + "]: " + summary + "."
	if evidence != "" {
		out += " The agent's own words: " + quoteReplies([]string{evidence})
	}
	return out
}

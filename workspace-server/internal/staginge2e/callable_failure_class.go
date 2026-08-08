package staginge2e

// callable_failure_class.go — NAME the reason a real A2A provision_workspace
// turn produced no workspace row.
//
// THE HOLE THIS CLOSES. EvaluateMgmtMCPCallable check 5 reds on ANY turn that
// created no row, with ONE sentence: "the verb is present but not genuinely
// CALLABLE". Measured over the whole Gitea Actions retention window
// (2026-07-09 → 2026-08-08, 258 genuine Guard B verdicts, 50 red), that single
// sentence was the entire published verdict for SIX materially different
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
//	G8  the runtime/gateway itself errored
//	G10 the agent never answered at all
//
// Those need six different fixes and four different owners — G6/G7 are not even
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

// CallableFailureClass is one named reason a callable turn produced no row.
type CallableFailureClass struct {
	// Code is the stable short id used in the verdict and in triage
	// (core#5088 named these G4/G5/G6/G7/G8/G10).
	Code string
	// Summary says what actually went wrong and who owns it.
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
		Code: "G4-TOOL-NAME-MANGLED",
		Summary: "the model emitted a tool id that does not exist, so the verb was never dispatched. " +
			"The inventory check (check 4) cannot catch this: it asserts the id the RUNTIME PROBE " +
			"surfaces (mcp__molecule-platform__…, hyphen) while hermes registers and the model emits " +
			"mcp__molecule_platform__… (underscore) — see core#5082. Owner: runtime/template, not the deploy candidate",
		// runs 609766, 616276, 616479, 622972
		markers: []string{"invalid tool call"},
	},
	{
		Code: "G6-LLM-BILLING-EXHAUSTED",
		Summary: "the LLM account backing the concierge is out of credit, so no turn could run. " +
			"This is ENVIRONMENT STATE, not a regression in the deploy candidate — the candidate is " +
			"unproven rather than proven bad, and the red is honest but must not be read as a code defect",
		// runs 596312, 600954, 607082
		markers: []string{"billing or credits exhausted", "http 402", "error code: 402"},
	},
	{
		Code: "G7-LLM-MODEL-REJECTED",
		Summary: "the LLM provider rejected the configured model, so no turn could run. " +
			"Like G6 this is provider/config state rather than a defect in the deploy candidate",
		// runs 466512, 468318
		markers: []string{"error code: 422", "model has no"},
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
		Code: "G5-AGENT-DENIES-VERB",
		Summary: "the agent was online with all platform verbs loaded and still ASSERTED it does not " +
			"have provision_workspace. The tool was present and dispatchable; the agent declined to " +
			"believe so. Owner: concierge persona/prompt, not the MCP surface",
		// runs 556986, 557305, 609311, 614408
		markers: []string{
			"i don't have", "i do not have", "don't have a", "don't have the",
			"do not have a", "do not have the", "no peers registered",
			"necessary permission", "not something i", "i'm unable to", "i am unable to",
			"unable to create", "don't have the capability", "is not available",
			"tool is not", "i can't", "i cannot",
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
func ClassifyCallableTurnFailure(texts []string) (code, summary, evidence string) {
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
			"the turn was accepted but the agent never produced a reply — it neither called the verb " +
				"nor said why. Distinct from a refusal (G5): there is no utterance to disagree with",
			""
	}

	for _, class := range callableFailureClasses {
		for _, t := range texts {
			lower := strings.ToLower(t)
			for _, m := range class.markers {
				if strings.Contains(lower, m) {
					return class.Code, class.Summary, strings.TrimSpace(t)
				}
			}
		}
	}
	return "", "", ""
}

// describeCallableFailure renders the check-5 reason, appending the named class
// when one is recognised. Kept separate from EvaluateMgmtMCPCallable so the
// exact published sentence is unit-testable without building a whole probe.
func describeCallableFailure(base string, texts []string) string {
	code, summary, evidence := ClassifyCallableTurnFailure(texts)
	if code == "" {
		return base
	}
	out := base + " FAILURE CLASS " + code + ": " + summary + "."
	if evidence != "" {
		out += " The agent's own words: " + quoteReplies([]string{evidence})
	}
	return out
}

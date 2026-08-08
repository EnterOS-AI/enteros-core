package staginge2e

import (
	"strings"
	"testing"
)

// Every fixture below is a VERBATIM reply text lifted from a real red Guard B
// run in Gitea Actions retention, with the run id named. Nothing here is
// invented: the classifier is proved against the traffic it exists to name.

type callableFixture struct {
	run   string // the Gitea run id the reply came from
	reply string
	want  string
}

var callableFixtures = []callableFixture{
	// ── G4: the model emitted a tool id that does not exist ──────────────
	{"622972", "Model generated invalid tool call: mcp__molecule__get_workspace_info", "G4-TOOL-NAME-MANGLED"},
	{"616479", "Model generated invalid tool call: mcp__molecule_platform__list_orgs", "G4-TOOL-NAME-MANGLED"},
	{"609766", "Model generated invalid tool call: list_peers", "G4-TOOL-NAME-MANGLED"},
	{"616276", "Model generated invalid tool call: mcp__molecule_platform__list_workspaces", "G4-TOOL-NAME-MANGLED"},

	// ── G6: LLM account out of credit ────────────────────────────────────
	{"596312", `Billing or credits exhausted: HTTP 402: {"error":"insufficient credits"}`, "G6-LLM-BILLING-EXHAUSTED"},
	{"607082", `Billing or credits exhausted: HTTP 402: {"error":"payment required"}`, "G6-LLM-BILLING-EXHAUSTED"},

	// ── G7: provider rejected the model ──────────────────────────────────
	{"466512", "⚠️ Error code: 422 - {'error': 'model has no serving capacity'}", "G7-LLM-MODEL-REJECTED"},
	{"468318", "⚠️ Error code: 422 - {'error': 'model has no such deployment'}", "G7-LLM-MODEL-REJECTED"},

	// ── G5: agent online with the verbs loaded, denies having them ───────
	{"556986", "I don't have a `provision_workspace` tool. It's not in my inventory.", "G5-AGENT-DENIES-VERB"},
	{"556986", "No luck — I have no peers registered in this org.", "G5-AGENT-DENIES-VERB"},
	{"609311", "I'm sorry, but I don't have the necessary permissions to create workspaces.", "G5-AGENT-DENIES-VERB"},
	{"609311", "I don't have the capability to create new workspaces.", "G5-AGENT-DENIES-VERB"},
	{"614408", "I see that the `provision_workspace` tool is not available to me.", "G5-AGENT-DENIES-VERB"},
	{"557305", "I've already explained three times — I don't have that tool.", "G5-AGENT-DENIES-VERB"},
}

// TestClassifyCallableTurnFailure_NamesEveryObservedMode is the fail-before
// proof: before callable_failure_class.go existed, EVERY one of these replies
// produced the identical verdict sentence and this test could not be written.
func TestClassifyCallableTurnFailure_NamesEveryObservedMode(t *testing.T) {
	for _, f := range callableFixtures {
		code, summary, evidence := ClassifyCallableTurnFailure([]string{f.reply})
		if code != f.want {
			t.Errorf("run %s: reply %q\n  got class %q, want %q", f.run, f.reply, code, f.want)
			continue
		}
		if strings.TrimSpace(summary) == "" {
			t.Errorf("run %s: class %s has an EMPTY summary — a class that names nothing is no better than the generic red", f.run, code)
		}
		if evidence != f.reply {
			t.Errorf("run %s: evidence %q does not quote the reply verbatim (%q) — an unquotable class is unfalsifiable by the reader", f.run, evidence, f.reply)
		}
	}
}

// TestClassifyCallableTurnFailure_CoversEveryClass makes sure the fixture table
// actually exercises every class the classifier can emit. Without this, adding
// a class and forgetting its fixture would leave it permanently unproven — the
// phantom-gate shape.
func TestClassifyCallableTurnFailure_CoversEveryClass(t *testing.T) {
	covered := map[string]bool{}
	for _, f := range callableFixtures {
		covered[f.want] = true
	}
	// G10 is proved separately (it has no reply text to put in the table).
	covered["G10-NO-REPLY-OBSERVED"] = true
	for _, c := range callableFailureClasses {
		if !covered[c.Code] {
			t.Errorf("class %s has NO fixture — it is asserted by nothing and could not fail if it broke", c.Code)
		}
		if len(c.markers) == 0 {
			t.Errorf("class %s has NO markers — it can never match, a vacuous class", c.Code)
		}
	}
}

// TestClassifyCallableTurnFailure_NoReplyIsItsOwnMode: silence is not refusal.
func TestClassifyCallableTurnFailure_NoReplyIsItsOwnMode(t *testing.T) {
	for _, texts := range [][]string{nil, {}, {""}, {"   ", "\n"}} {
		code, summary, _ := ClassifyCallableTurnFailure(texts)
		if code != "G10-NO-REPLY-OBSERVED" {
			t.Fatalf("texts=%q: got %q, want G10-NO-REPLY-OBSERVED", texts, code)
		}
		if strings.TrimSpace(summary) == "" {
			t.Fatalf("G10 summary is empty")
		}
	}
}

// TestClassifyCallableTurnFailure_UnrecognisedAbstains: an unfamiliar reply must
// NOT be forced into a class. A wrong class routes the next investigation to the
// wrong owner, which is worse than the generic sentence.
func TestClassifyCallableTurnFailure_UnrecognisedAbstains(t *testing.T) {
	for _, reply := range []string{
		"Sure, one moment while I take a look at that for you.",
		"The org currently contains three workspaces.",
	} {
		code, summary, evidence := ClassifyCallableTurnFailure([]string{reply})
		if code != "" || summary != "" || evidence != "" {
			t.Fatalf("reply %q was force-classified as %q — an unrecognised reply must abstain", reply, code)
		}
	}
}

// TestClassifyCallableTurnFailure_MostSpecificWins pins the ordering: a reply
// that carries BOTH a mangled-tool-call marker and a denial phrase is G4, the
// concrete mechanism, not G5.
func TestClassifyCallableTurnFailure_MostSpecificWins(t *testing.T) {
	reply := "I don't have that tool. Model generated invalid tool call: mcp__molecule__task_add"
	if code, _, _ := ClassifyCallableTurnFailure([]string{reply}); code != "G4-TOOL-NAME-MANGLED" {
		t.Fatalf("got %q, want G4-TOOL-NAME-MANGLED (the concrete mechanism must win over the generic denial)", code)
	}
}

// TestClassifyCallableTurnFailure_NeverChangesTheVerdict is the load-bearing
// safety test for this change. Naming a failure must not tolerate it: for every
// fixture the gate verdict stays RED, and a healthy probe stays GREEN. If this
// ever fails, the diagnostic has become an escape hatch.
func TestClassifyCallableTurnFailure_NeverChangesTheVerdict(t *testing.T) {
	base := func() MgmtMCPProbe {
		return MgmtMCPProbe{
			ExpectedRuntime: "hermes", ObservedRuntime: "hermes",
			Status:          "online",
			LoadedTools:     []string{"mcp__molecule-platform__provision_workspace"},
			RequiredTool:    "mcp__molecule-platform__provision_workspace",
			AssertCallable:  true, RequireCallable: true,
		}
	}
	for _, f := range callableFixtures {
		p := base()
		p.WorkerProvisioned = false
		p.Claim = ConciergeClaim{Observed: true, Texts: []string{f.reply}}
		ok, reason := EvaluateMgmtMCPCallable(p)
		if ok {
			t.Fatalf("run %s: reply %q turned the gate GREEN — naming a failure must never excuse it", f.run, f.reply)
		}
		if !strings.Contains(reason, f.want) {
			t.Errorf("run %s: verdict does not name the class %s.\n  got: %s", f.run, f.want, reason)
		}
		if !strings.Contains(reason, "not genuinely CALLABLE") {
			t.Errorf("run %s: verdict dropped the original enforcement sentence", f.run)
		}
	}
	// The healthy probe must still be GREEN, and must NOT carry a failure class.
	p := base()
	p.WorkerProvisioned = true
	ok, reason := EvaluateMgmtMCPCallable(p)
	if !ok {
		t.Fatalf("healthy probe went RED after the change: %s", reason)
	}
	for _, c := range callableFailureClasses {
		if strings.Contains(reason, c.Code) {
			t.Fatalf("a GREEN verdict names failure class %s", c.Code)
		}
	}
}

// TestDescribeCallableFailure_FallsBackToTheGenericSentence: an unrecognised
// reply must leave the published verdict byte-identical to the pre-change one.
func TestDescribeCallableFailure_FallsBackToTheGenericSentence(t *testing.T) {
	const base = "platform agent is online ... not genuinely CALLABLE."
	if got := describeCallableFailure(base, []string{"something we have never seen"}); got != base {
		t.Fatalf("unrecognised reply changed the sentence:\n got: %s\nwant: %s", got, base)
	}
}

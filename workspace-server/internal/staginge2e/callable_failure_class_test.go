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

// Each `reply` is a VERBATIM PREFIX of a reply the gate really logged, taken
// from that run's job log. Prefixes, not full replies, for two reasons: the
// older runs predate the a2aTurnLogCap widening in core#5052 so the tenant reply
// is already truncated in the log, and a prefix keeps the fixture free of the
// replacement characters a couple of the raw bytes decode to. A prefix is still
// verbatim — it is a substring of what the agent actually said.
//
// An earlier revision of this table contained INVENTED completions
// ("...tool is not available to me.", "...the necessary permissions to create
// workspaces.") which read plausibly and appear nowhere in the corpus. They
// pinned two markers that no traffic can fire. Both fixtures and both markers
// are gone; TestClassifyCallableTurnFailure_EveryMarkerIsNecessary now makes
// that class of mistake fail the build.
//
// HOW THESE ARE VERIFIED, AND BY WHAT — read this before trusting the rows.
//
// NOTHING IN THIS PACKAGE VERIFIES THE `run` COLUMN. It cannot: the job logs
// are not in the repo, `go test` has no network, and Gitea's Actions retention
// is ephemeral — run 634244's log has already expired. A committed corpus check
// would decay into a false red the day its logs age out, which is a worse
// failure than the one it would catch. So attribution is verified by PROCESS,
// not by assertion, and this comment says so rather than implying otherwise.
// (An earlier revision of this comment read as if enforced. That overclaim is
// exactly the defect this file exists to remove, so it is called out here
// rather than quietly fixed.)
//
// THE PROCESS. Each row is checked out-of-band against the job logs on two
// legs: the reply is a verbatim substring of a logged reply, AND it was logged
// by the RUN the row cites. Reconstructing a reply means taking the
// `"text":"…"` capture and cutting at the FIRST NEWLINE — the capture is greedy
// and otherwise runs on into the following log line. Older runs are further
// truncated by the pre-core#5052 a2aTurnLogCap, which is why many rows end
// mid-word. Re-deriving it needs only the job logs and that rule.
//
// WHY BOTH LEGS. The one-leg version (string only) passed a bad row: 616276
// carried "…mcp__molecule_platform__list_workspaces", which is real text —
// logged by 616479. Same string, wrong run. Only the attribution leg caught it.
//
// WHAT THE IN-REPO TESTS DO HOLD. They pin everything that does not need the
// corpus: a marker matched by no fixture (_EveryMarkerIsFixtured), a marker
// whose deletion changes nothing (_EveryMarkerIsNecessary), a fixture truncated
// so far it stops carrying its marker, and the Code->Owner->Summary binding
// (_CodeBindsToOwnerAndSummary). What they cannot see is a fixture whose text
// is real but whose `run` is wrong — that costs a reader's ability to check the
// claim, not the gate's correctness.
//
// TWO KNOWN WEAKER AXES, recorded so they are not rediscovered as surprises:
// provenance-WITHIN-run is unasserted (all 14 pass today, and it is non-binding
// because these logs contain no other `"text":"` producer); and verifiability
// decays as logs expire — a row whose run has aged out can no longer be
// re-checked by anyone, only trusted.
var callableFixtures = []callableFixture{
	// ── G4: the model emitted a tool id that does not exist ──────────────
	{"622972", "Model generated invalid tool call: mcp__molecule__get_workspace_info", "G4-TOOL-NAME-MANGLED"},
	{"616479", "Model generated invalid tool call: mcp__molecule_platform__list_orgs", "G4-TOOL-NAME-MANGLED"},
	{"609766", "Model generated invalid tool call: list_peers", "G4-TOOL-NAME-MANGLED"},
	// 616276's replies are all truncated in its log; this is the verbatim
	// prefix. An earlier revision carried a 25-char completion
	// ("…_platform__list_workspaces") that is real text but was logged by
	// 616479, not by this run — see the sweep note above the table.
	{"616276", "Model generated invalid tool call: mcp__molecule_", "G4-TOOL-NAME-MANGLED"},

	// ── G6: LLM account out of credit ────────────────────────────────────
	{"596312", `Billing or credits exhausted: HTTP 402: {"error`, "G6-LLM-BILLING-EXHAUSTED"},

	// ── G7: provider rejected the model ──────────────────────────────────
	// Prefix begins at the error code so the fixture carries no emoji bytes.
	{"468318", "Error code: 422 - {'error': 'model has no", "G7-LLM-MODEL-REJECTED"},

	// ── G5: agent online with the verbs loaded, denies having them ───────
	// Every one of these pins a DIFFERENT marker; see _EveryMarkerIsNecessary.
	{"556986", "I don't have a `provision_workspace` tool. It's n", "G5-AGENT-DENIES-VERB"},
	{"556986", "No luck — I have no peers registered in this or", "G5-AGENT-DENIES-VERB"},
	{"556986", "This request is not something I'm choosing not to", "G5-AGENT-DENIES-VERB"},
	{"557370", "Final answer: I do not have a `provision_workspac", "G5-AGENT-DENIES-VERB"},
	{"557370", "I can't do this. No tool exists in my environment", "G5-AGENT-DENIES-VERB"},
	{"557370", "I still don't have a `provision_workspace` tool", "G5-AGENT-DENIES-VERB"},
	{"609311", "I don't have the ability to create workspaces.", "G5-AGENT-DENIES-VERB"},
	{"614408", "I see that the `provision_workspace` tool is not", "G5-AGENT-DENIES-VERB"},
}

// TestClassifyCallableTurnFailure_NamesEveryObservedMode is the fail-before
// proof: before callable_failure_class.go existed, EVERY one of these replies
// produced the identical verdict sentence and this test could not be written.
func TestClassifyCallableTurnFailure_NamesEveryObservedMode(t *testing.T) {
	for _, f := range callableFixtures {
		code, owner, summary, evidence := ClassifyCallableTurnFailure([]string{f.reply})
		if code != f.want {
			t.Errorf("run %s: reply %q\n  got class %q, want %q", f.run, f.reply, code, f.want)
			continue
		}
		if strings.TrimSpace(summary) == "" {
			t.Errorf("run %s: class %s has an EMPTY summary — a class that names nothing is no better than the generic red", f.run, code)
		}
		if strings.TrimSpace(owner) == "" {
			t.Errorf("run %s: class %s has an EMPTY owner — a red that names no owner routes nowhere", f.run, code)
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

// TestClassifyCallableTurnFailure_EveryMarkerIsFixtured is the same refusal as
// the one that deleted the G8 class and the four speculative G5 variants, but at
// MARKER granularity.
//
// _CoversEveryClass only proves each CLASS has some fixture. That is too coarse:
// a class with eight markers passes it while seven of those markers are matched
// by nothing at all — delete any of the seven and the suite stays green, which
// is the definition of an unpinned branch. This test closes that gap by
// requiring every marker to appear in at least one fixture reply.
//
// It is what makes "delete a marker → the suite goes RED" true, and that
// mutation is the proof this assertion is not itself decorative.
func TestClassifyCallableTurnFailure_EveryMarkerIsFixtured(t *testing.T) {
	lowered := make([]string, 0, len(callableFixtures))
	for _, f := range callableFixtures {
		lowered = append(lowered, strings.ToLower(f.reply))
	}
	for _, c := range callableFailureClasses {
		for _, m := range c.markers {
			if m != strings.ToLower(m) {
				t.Errorf("class %s marker %q is not lower-cased — matching is done on a lower-cased reply, so it could never fire", c.Code, m)
				continue
			}
			found := false
			for _, reply := range lowered {
				if strings.Contains(reply, m) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("class %s marker %q is matched by NO fixture — it is grounded in no observed traffic. "+
					"Either add the verbatim reply that produced it (naming its run id), or delete the marker. "+
					"A marker no traffic can fire is a branch that will never be exercised and never be noticed if it breaks.",
					c.Code, m)
			}
		}
	}
}

// TestClassifyCallableTurnFailure_EveryMarkerIsNecessary is the assertion that
// actually makes per-marker deletion FAIL THE BUILD.
//
// _EveryMarkerIsFixtured is necessary but not sufficient, and mutation testing
// is what proved it. With eight G5 markers all matched by some fixture, that
// test passed — yet deleting nine of them individually left the whole suite
// GREEN, because each deleted marker was redundantly co-matched by another on
// the same reply. "Matched by a fixture" does not mean "load-bearing".
//
// The property that IS load-bearing: for every marker there must exist a reply
// that ONLY that marker matches. Then removing it strands that reply, the
// class stops being named, and _NamesEveryObservedMode reds. This is the same
// standard applied to the G8 class and to the four ungrounded variants — if
// deleting a thing changes nothing observable, it was not doing anything.
func TestClassifyCallableTurnFailure_EveryMarkerIsNecessary(t *testing.T) {
	for _, c := range callableFailureClasses {
		for _, m := range c.markers {
			sole := ""
			for _, f := range callableFixtures {
				if f.want != c.Code {
					continue
				}
				low := strings.ToLower(f.reply)
				if !strings.Contains(low, m) {
					continue
				}
				others := 0
				for _, other := range c.markers {
					if other != m && strings.Contains(low, other) {
						others++
					}
				}
				if others == 0 {
					sole = f.reply
					break
				}
			}
			if sole == "" {
				t.Errorf("class %s marker %q is REDUNDANT: every fixture it matches is also matched by another "+
					"marker of the same class, so deleting it would change nothing and no test would notice. "+
					"Add the verbatim reply that only this marker catches, or delete the marker.",
					c.Code, m)
			}
		}
	}
}

// ── Code -> Owner/Summary binding ───────────────────────────────────────────
//
// classBinding is the INDEPENDENT statement of what each code means: the phrase
// its summary must carry, and the owner the red must route to. It is written
// here, in the test, precisely so it is not the same string as the thing under
// test — a binding restated from the implementation would agree with any
// implementation, including a wrong one.
//
// distinguisher must be a phrase that is TRUE of this class and FALSE of every
// other; ownerMark likewise for the owner. The pairwise property below enforces
// that, which is what makes a swap detectable.
var classBinding = map[string]struct {
	distinguisher string
	ownerMark     string
}{
	"G4-TOOL-NAME-MANGLED":     {"tool id that does not exist", "runtime/template"},
	"G5-AGENT-DENIES-VERB":     {"ASSERTED it does not", "concierge persona"},
	"G6-LLM-BILLING-EXHAUSTED": {"out of credit", "LLM account state"},
	"G7-LLM-MODEL-REJECTED":    {"rejected the configured model", "LLM provider/model configuration"},
}

// TestCallableFailureClasses_CodeBindsToOwnerAndSummary closes the THIRD axis.
//
// The first two legs verify the EVIDENCE — that markers are grounded in real
// traffic and that fixtures are verbatim and correctly attributed. Nothing
// verified the CONCLUSION DRAWN FROM IT. Review demonstrated the gap by
// swapping only G6's and G7's Summary strings: codes unchanged, markers still
// load-bearing, fixtures still verbatim, both legs green — and the gate then
// published "the LLM provider rejected the configured model" for a BILLING red,
// naming the wrong owner. That is this PR's own defect (one sentence, wrong
// attribution) reproduced one level down in the thing meant to fix it.
//
// Written as a PROPERTY over the whole table rather than per-class assertions,
// because the failure mode is PAIRWISE: hand-written per-class checks tend to
// assert "G6 says something about credit" and stay green when G6 ALSO says
// everything G7 should say. The second loop below is the one that catches a
// swap, and it scales to every future class for free.
func TestCallableFailureClasses_CodeBindsToOwnerAndSummary(t *testing.T) {
	if len(classBinding) != len(callableFailureClasses) {
		t.Fatalf("classBinding has %d entries but there are %d classes — a class with no binding is unpinned, "+
			"and a binding with no class is dead", len(classBinding), len(callableFailureClasses))
	}
	for _, c := range callableFailureClasses {
		want, ok := classBinding[c.Code]
		if !ok {
			t.Errorf("class %s has NO binding entry — its summary and owner are asserted by nothing", c.Code)
			continue
		}
		// 1. POSITIVE: this class says its own thing, and routes to its own owner.
		if !strings.Contains(c.Summary, want.distinguisher) {
			t.Errorf("class %s summary does not contain its distinguishing phrase %q.\n  summary: %s",
				c.Code, want.distinguisher, c.Summary)
		}
		if !strings.Contains(c.Owner, want.ownerMark) {
			t.Errorf("class %s owner %q does not name %q — the red would route to the wrong team",
				c.Code, c.Owner, want.ownerMark)
		}
		// 2. PAIRWISE: and says NOBODY ELSE'S. This is the swap detector.
		for otherCode, other := range classBinding {
			if otherCode == c.Code {
				continue
			}
			if strings.Contains(c.Summary, other.distinguisher) {
				t.Errorf("class %s summary ALSO contains %s's distinguishing phrase %q — the two are not "+
					"distinguishable, so swapping them would go unnoticed.\n  summary: %s",
					c.Code, otherCode, other.distinguisher, c.Summary)
			}
			if strings.Contains(c.Owner, other.ownerMark) {
				t.Errorf("class %s owner ALSO names %s's owner %q — ownership is ambiguous",
					c.Code, otherCode, other.ownerMark)
			}
		}
	}
}

// TestDescribeCallableFailure_PublishesTheOwner keeps Owner from becoming a
// field that exists and is never read — which would be its own vacuous surface,
// and the reason it is a struct field rather than a comment.
func TestDescribeCallableFailure_PublishesTheOwner(t *testing.T) {
	for _, f := range callableFixtures {
		got := describeCallableFailure(callableRedBaseReason, []string{f.reply})
		want := classBinding[f.want].ownerMark
		if !strings.Contains(got, want) {
			t.Errorf("run %s: published verdict does not name the owner %q.\n  got: %s", f.run, want, got)
		}
	}
}

// TestClassifyCallableTurnFailure_UnanchoredModelHasNoStaysDeleted pins the
// review finding that G7's unanchored "model has no" misroutes ownership.
//
// G7 sits ABOVE G5, so with that marker present a plain refusal that happens to
// mention the model reads as a provider fault. The verdict stays RED either way
// — only the OWNER changes — but sending the next investigation to the wrong
// team is exactly the cost this file exists to remove. Reinstating the marker
// must fail here.
func TestClassifyCallableTurnFailure_UnanchoredModelHasNoStaysDeleted(t *testing.T) {
	reply := "The model has no access to that, so I don't have provision_workspace."
	code, _, _, _ := ClassifyCallableTurnFailure([]string{reply})
	if code != "G5-AGENT-DENIES-VERB" {
		t.Fatalf("got %q, want G5-AGENT-DENIES-VERB: an unanchored provider-sounding phrase inside an ordinary "+
			"refusal must not route to the provider owner", code)
	}
	for _, c := range callableFailureClasses {
		for _, m := range c.markers {
			if m == "model has no" {
				t.Fatal("the unanchored \"model has no\" marker was reinstated; it never appears in the corpus " +
					"without \"error code: 422\" beside it, so it catches nothing new and can only misroute")
			}
		}
	}
}

// TestClassifyCallableTurnFailure_NoReplyIsItsOwnMode: silence is not refusal.
func TestClassifyCallableTurnFailure_NoReplyIsItsOwnMode(t *testing.T) {
	for _, texts := range [][]string{nil, {}, {""}, {"   ", "\n"}} {
		code, owner, summary, _ := ClassifyCallableTurnFailure(texts)
		if code != "G10-NO-REPLY-OBSERVED" {
			t.Fatalf("texts=%q: got %q, want G10-NO-REPLY-OBSERVED", texts, code)
		}
		if strings.TrimSpace(summary) == "" {
			t.Fatalf("G10 summary is empty")
		}
		if strings.TrimSpace(owner) == "" {
			t.Fatalf("G10 owner is empty")
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
		code, owner, summary, evidence := ClassifyCallableTurnFailure([]string{reply})
		if code != "" || owner != "" || summary != "" || evidence != "" {
			t.Fatalf("reply %q was force-classified as %q — an unrecognised reply must abstain", reply, code)
		}
	}
}

// TestClassifyCallableTurnFailure_MostSpecificWins pins the ordering: a reply
// that carries BOTH a mangled-tool-call marker and a denial phrase is G4, the
// concrete mechanism, not G5.
func TestClassifyCallableTurnFailure_MostSpecificWins(t *testing.T) {
	reply := "I don't have that tool. Model generated invalid tool call: mcp__molecule__task_add"
	if code, _, _, _ := ClassifyCallableTurnFailure([]string{reply}); code != "G4-TOOL-NAME-MANGLED" {
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
	const base = "platform agent is online ... not genuinely CALLABLE"
	if got := describeCallableFailure(base, []string{"something we have never seen"}); got != base {
		t.Fatalf("unrecognised reply changed the sentence:\n got: %s\nwant: %s", got, base)
	}
}

// TestDescribeCallableFailure_FallsBackByteIdenticalToThePreChangeSentence
// pins the ACTUAL published string, not a stand-in.
//
// The PR body claimed the fallback was "byte-identical to the pre-change
// sentence". Review found that was false: the base had gained a trailing
// period, so 6,552 probe/reply pairs differed by exactly that one byte.
// Harmless downstream — every consumer matches on `strings.Contains(...,
// "not genuinely CALLABLE")` — but a precision claim that is not precise is
// the one kind of defect this PR is least entitled to ship. The separator now
// lives in describeCallableFailure, and this test holds the literal so the
// claim is enforced rather than asserted.
func TestDescribeCallableFailure_FallsBackByteIdenticalToThePreChangeSentence(t *testing.T) {
	const preChange = "platform agent is online with its management MCP present, but a REAL A2A provision_workspace turn did NOT create the requested workspace — the verb is present but not genuinely CALLABLE (a presence-only gate would have false-passed here)"
	if callableRedBaseReason != preChange {
		t.Fatalf("the base sentence drifted from the pre-change wording:\n got: %q\nwant: %q", callableRedBaseReason, preChange)
	}
	p := MgmtMCPProbe{
		ExpectedRuntime: "hermes", ObservedRuntime: "hermes",
		Status:          "online",
		LoadedTools:     []string{"mcp__molecule-platform__provision_workspace"},
		RequiredTool:    "mcp__molecule-platform__provision_workspace",
		AssertCallable:  true, RequireCallable: true,
		WorkerProvisioned: false,
		Claim:             ConciergeClaim{Observed: true, Texts: []string{"an utterance no marker matches"}},
	}
	ok, reason := EvaluateMgmtMCPCallable(p)
	if ok {
		t.Fatal("an unclassifiable reply must still be RED")
	}
	if reason != preChange {
		t.Fatalf("unclassified check-5 verdict is NOT byte-identical to the pre-change sentence:\n got: %q\nwant: %q", reason, preChange)
	}
}

package staginge2e

// readiness_late_verdict_test.go — the LATE-VERDICT arm.
//
// BLOCKER 2 (review 20596). Observe evaluated the settle window BEFORE the
// budget, so a terminal verdict published inside the final `Settle` of the wait
// could never finish settling and fell through to budgetMessage — which states
//
//	"the control plane never published a terminal verdict about it either"
//
// while the SAME failure output printed `failed` as the last status and a trace
// containing `failed@13m0s`, and dropped the control plane's reason entirely.
//
// The window is not academic:
//
//	concierge  budget 15m, settle 3m -> a verdict after t+12m  (last 20% )
//	org        budget  7m, settle 3m -> a verdict after t+4m   (last 43% )
//
// and green orgs reach running in up to 249s = 4m09s, i.e. the org boundary sits
// right inside the normal spread. G2's nine reds never recorded WHEN `failed`
// was published, so the tail cannot be assumed empty — it is unmeasured, which
// is exactly why the message must not assert something it did not check.
//
// A message that says "never published" while displaying the published status is
// worse than the stopwatch it replaced: the stopwatch was uninformative, this
// would be actively false. These tests pin that it cannot come back.

import (
	"strings"
	"testing"
	"time"
)

// seqLateVerdict: the control plane publishes `failed` at t+13m of a 15m budget,
// so only 2m of the 3m settle can elapse before the budget runs out.
var seqLateVerdict = []statusAt{
	{at: 0, status: "provisioning"},
	{at: 13 * time.Minute, status: "failed",
		detail: "platform agent heartbeat denied: no seeded MODEL workspace_secret; refusing to mark online (RCA #2970 FAIL-CLOSED)"},
}

// seqLateVerdictOrg: the org-shaped case. instance_status=failed at t+5m of a 7m
// budget — inside the last 43% of the wait, where nearly half the window lives.
var seqLateVerdictOrg = []statusAt{
	{at: 0, status: "<instance_status omitted>"},
	{at: 30 * time.Second, status: "provisioning"},
	{at: 5 * time.Minute, status: "failed",
		detail: "organizations_provider_check violation: migration 071 rejects literal 'local'"},
}

func TestReadinessWatch_ALateTerminalVerdictIsReportedAsATerminalVerdict(t *testing.T) {
	w := NewConciergeOnlineWatch(conciergeBudget, TerminalSettleDefault)
	got, _ := runWatchWait(w, seqLateVerdict, conciergePoll)

	if got.ok {
		t.Fatalf("a late published failure must still fail the wait")
	}

	// The message must NOT claim nothing was published — the watch saw it.
	if strings.Contains(got.message, "never published a terminal verdict") {
		t.Fatalf("BLOCKER 2: the failure claims the control plane published nothing, "+
			"while the same watch observed status=%q. A message that contradicts its own data is worse "+
			"than the timeout it replaced.\nGot: %s", w.LastStatus(), got.message)
	}
	if strings.Contains(got.message, "STUCK, not FAILED") {
		t.Fatalf("BLOCKER 2: a published failure was reported as STUCK.\nGot: %s", got.message)
	}

	// It must name the verdict AND carry the control plane's own reason — the
	// single field this whole change exists to surface.
	for _, want := range []string{
		`TERMINAL status="failed"`,
		"RCA #2970 FAIL-CLOSED",
	} {
		if !strings.Contains(got.message, want) {
			t.Fatalf("a late verdict must still name %q.\nGot: %s", want, got.message)
		}
	}

	// And it must be HONEST that the settle did not complete, rather than
	// implying the verdict was confirmed across the full self-heal window.
	if !strings.Contains(got.message, "budget expired before the") {
		t.Fatalf("a late verdict must say the budget cut the settle short, not imply a completed settle.\nGot: %s", got.message)
	}
}

func TestOrgInstanceWatch_ALateProvisionFailureIsNotReportedAsStuck(t *testing.T) {
	const orgBudget = 7 * time.Minute
	w := NewOrgInstanceRunningWatch("e2e-mcp-616947", orgBudget, TerminalSettleDefault)
	got, _ := runWatchWait(w, seqLateVerdictOrg, conciergePoll)

	if got.ok {
		t.Fatalf("a late org provision failure must fail the wait")
	}
	if strings.Contains(got.message, "never published a terminal verdict") ||
		strings.Contains(got.message, "STUCK, not FAILED") {
		t.Fatalf("BLOCKER 2 (org arm): a published org failure was reported as STUCK.\nGot: %s", got.message)
	}
	if !strings.Contains(got.message, "organizations_provider_check") {
		t.Fatalf("a late org failure must still quote org_instances.last_error.\nGot: %s", got.message)
	}
}

// The genuinely-stuck arm must KEEP its wording. Blocker 2's fix must not be a
// blanket "always say terminal" — a subject about which nothing was ever
// published has to stay distinguishable, which is the property the whole PR is
// about.
func TestReadinessWatch_StuckWithNothingPublishedKeepsItsOwnWording(t *testing.T) {
	w := NewConciergeOnlineWatch(conciergeBudget, TerminalSettleDefault)
	got, _ := runWatchWait(w, seqStuckNoVerdict, conciergePoll)
	if got.ok {
		t.Fatalf("a stuck subject must fail")
	}
	if !strings.Contains(got.message, "STUCK, not FAILED") ||
		!strings.Contains(got.message, "never published a terminal verdict") {
		t.Fatalf("the genuinely-stuck arm must keep its distinct wording.\nGot: %s", got.message)
	}
	if strings.Contains(got.message, "PUBLISHED FAILURE VERDICT") {
		t.Fatalf("a stuck subject must not be reported as a published failure.\nGot: %s", got.message)
	}
}

// A verdict that WAS published and then REVISED (the control plane's self-heal
// working), after which the subject wedges in a non-terminal state, is a third
// shape. It is stuck — but the message must not claim nothing was ever
// published, because something was.
func TestReadinessWatch_APublishedThenRevisedVerdictIsNotErasedFromTheReport(t *testing.T) {
	seq := []statusAt{
		{at: 0, status: "provisioning"},
		{at: 90 * time.Second, status: "failed", detail: "management MCP server absent"},
		{at: 200 * time.Second, status: "awaiting_agent"}, // revised, then wedges here forever
	}
	w := NewConciergeOnlineWatch(conciergeBudget, TerminalSettleDefault)
	got, _ := runWatchWait(w, seq, conciergePoll)

	if got.ok {
		t.Fatalf("wedged in awaiting_agent must fail")
	}
	if got.elapsed < conciergeBudget {
		t.Fatalf("a revised verdict must NOT abort early — the subject left the terminal set, so the wait continues (got %s)", got.elapsed)
	}
	if strings.Contains(got.message, "never published a terminal verdict") {
		t.Fatalf("the report erases a verdict the watch actually observed.\nGot: %s", got.message)
	}
	if !strings.Contains(got.message, "awaiting_agent") {
		t.Fatalf("the report must name where the subject actually wedged.\nGot: %s", got.message)
	}
}

// ── The surviving mutant (review 20596, non-blocking) ───────────────────────
//
// Deleting the terminalStatus/terminalSince reset in Observe's `default:` arm
// survived the original mutant set: a FLAPPING subject would then be judged
// from its FIRST sighting of a terminal status rather than its current run, so
// a workspace that failed, recovered, failed again and then genuinely came
// online would be called dead. Shipped behaviour was already correct; the
// coverage was missing.
func TestReadinessWatch_LeavingTheTerminalSetRestartsTheSettle(t *testing.T) {
	// failed@60 -> provisioning@120 -> failed@210 -> online@330.
	// WITH the reset: the second failed run starts at 210, so the settle would
	// not expire until ~390 and online at 330 wins  -> READY.
	// WITHOUT the reset: the settle is still anchored at 60 and the wait aborts
	// at ~240, before online  -> RED.
	seq := []statusAt{
		{at: 0, status: "provisioning"},
		{at: 60 * time.Second, status: "failed", detail: "transient boot failure"},
		{at: 120 * time.Second, status: "provisioning"},
		{at: 210 * time.Second, status: "failed", detail: "transient boot failure (second)"},
		{at: 330 * time.Second, status: "online"},
	}
	w := NewConciergeOnlineWatch(conciergeBudget, TerminalSettleDefault)
	got, _ := runWatchWait(w, seq, conciergePoll)

	if !got.ok {
		t.Fatalf("a flapping subject that reaches online must be READY — the settle must be anchored on the "+
			"CURRENT terminal run, not the first one ever seen (aborted at %s: %s)", got.elapsed, got.message)
	}
	if got.elapsed < 330*time.Second {
		t.Fatalf("READY reported at %s, before the workspace actually came online at 330s", got.elapsed)
	}
	// The trace must show the flap, so an operator can see the subject moved.
	if strings.Count(w.Trace(), "failed@") != 2 {
		t.Fatalf("the trace must record BOTH terminal runs so a flap is visible: %s", w.Trace())
	}
}

// The same property stated on the state machine directly: once the subject
// leaves the terminal set, the pending verdict is forgotten, including its
// detail (a stale reason attached to a later, different failure would be a
// misdiagnosis).
func TestReadinessWatch_LeavingTheTerminalSetClearsThePendingReason(t *testing.T) {
	w := NewConciergeOnlineWatch(conciergeBudget, TerminalSettleDefault)
	base := time.Now()
	w.Observe(base, Obs{ReadOK: true, Status: "failed", Detail: "FIRST-REASON"})
	w.Observe(base.Add(10*time.Second), Obs{ReadOK: true, Status: "provisioning"})
	// A second, different failure with no reason attached must NOT inherit the
	// first one.
	var msg string
	for i := 1; i <= 40; i++ {
		st := w.Observe(base.Add(time.Duration(20+15*i)*time.Second), Obs{ReadOK: true, Status: "failed"})
		if st.Decision == WaitFailTerminal {
			msg = st.Message
			break
		}
	}
	if msg == "" {
		t.Fatalf("the second terminal run never settled")
	}
	if strings.Contains(msg, "FIRST-REASON") {
		t.Fatalf("a stale reason from an earlier, revised failure leaked into a later one: %s", msg)
	}
}

// The late-verdict message must timestamp the verdict it is actually reporting.
//
// Review 20599: the `settled=false` branch read "at t+X" from everTerminalAt
// (the FIRST-ever sighting of any terminal status) while "held it Y later" read
// the CURRENT run. On a flap those are different events, so the message could
// say "published at t+1m0s … and had not revised it" while its own trace, two
// clauses later, showed the revision at t+2m0s.
//
// Strictly weaker than the blocker it resembles — the verdict is still named,
// the reason still quoted, the trace still correct in the same output — and it
// needs a flap, which retention has never produced. But it is the same species:
// a sentence contradicted by the data printed beside it. Fixed rather than
// documented.
func TestReadinessWatch_LateVerdictTimestampsTheRunItIsReporting(t *testing.T) {
	// failed@60s -> provisioning@120s -> failed@14m, on a 15m budget: the second
	// terminal run cannot complete the 3m settle, so this takes the late branch.
	seq := []statusAt{
		{at: 0, status: "provisioning"},
		{at: 60 * time.Second, status: "failed", detail: "first, transient"},
		{at: 120 * time.Second, status: "provisioning"},
		{at: 14 * time.Minute, status: "failed", detail: "second, the one being reported"},
	}
	w := NewConciergeOnlineWatch(conciergeBudget, TerminalSettleDefault)
	got, _ := runWatchWait(w, seq, conciergePoll)

	if got.ok {
		t.Fatalf("must fail")
	}
	// It must timestamp the run it is reporting (t+14m), NOT the first sighting.
	if strings.Contains(got.message, "at t+1m0s") {
		t.Fatalf("the message timestamps the FIRST terminal sighting (t+1m0s) while reporting the CURRENT run, "+
			"and its own trace shows the intervening recovery. A sentence contradicted by the data beside it.\nGot: %s",
			got.message)
	}
	if !strings.Contains(got.message, "at t+14m0s") {
		t.Fatalf("the message must timestamp the terminal run it is reporting (t+14m0s).\nGot: %s", got.message)
	}
	// The reason must be the CURRENT run's, not the stale first one.
	if strings.Contains(got.message, "first, transient") {
		t.Fatalf("stale reason from the earlier, revised failure leaked into the report.\nGot: %s", got.message)
	}
	if !strings.Contains(got.message, "second, the one being reported") {
		t.Fatalf("the report must quote the CURRENT run's reason.\nGot: %s", got.message)
	}
	// And the trace must still show the flap, so the recovery is visible.
	if strings.Count(w.Trace(), "failed@") != 2 {
		t.Fatalf("the trace must record both terminal runs: %s", w.Trace())
	}
}

package staginge2e

// readiness_terminal_signal_test.go — the FAIL-BEFORE / GREEN-AFTER proof for
// the terminal-signal readiness wait, runnable in the normal `go test ./...`
// gate (no live tenant, no build tag, no wall clock).
//
// Every sequence replayed here is a REAL sequence read out of Gitea retention
// for the `e2e-smoke (staging real provision + A2A HARD GATE)` job of
// staging-tenant-cd.yml, not an invented one:
//
//	GREEN (n=209)   provisioning@0s -> online@10s          (median)
//	GREEN slowest   provisioning@0s -> online@137s         (max over 209 runs)
//	RED   (n=9)     provisioning@0s -> failed@93s -> ...   (job 817356,
//	                tenant e2e-life-550377-0ed05f, 2026-07-20T23:25:57Z →
//	                23:27:30Z; the row was then observed continuously until
//	                23:41:02Z and NEVER left `failed`)
//
// The PRE-FIX algorithm is transcribed verbatim as legacyPositiveEdgeWait so
// the defect is executable rather than described: on the RED sequence it burns
// all 900s and reports a TIMEOUT, which is exactly why 35 of the 58 genuine
// Guard B reds were filed as "boot/provision timeouts" when 24 of them were
// sitting on a published `failed` verdict the whole time.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// ── Replay harness ───────────────────────────────────────────────────────────

// statusAt is one step of a replayed observation sequence: from `at` onwards
// (until the next step) the API returns this status/detail.
type statusAt struct {
	at     time.Duration
	status string
	detail string
	// readOK=false models a failed read (non-200) at this point.
	readFail bool
}

// replaySeq answers "what would the API have returned at t+elapsed?".
func replaySeq(seq []statusAt, elapsed time.Duration) statusAt {
	cur := statusAt{at: 0, status: "", readFail: true}
	for _, s := range seq {
		if elapsed >= s.at {
			cur = s
		}
	}
	return cur
}

// waitOutcome is the algorithm-independent result the requirement is stated
// against, so the SAME assertions can be pointed at the pre-fix and post-fix
// algorithms.
type waitOutcome struct {
	ok      bool
	elapsed time.Duration
	message string
}

// runWatchWait drives the PRODUCTION wait loop (ReadinessWatch.Observe) over a
// replayed sequence on a fake clock. This is the same call shape the tagged
// live waits use, so the unit proof and the deploy gate share one algorithm.
func runWatchWait(w *ReadinessWatch, seq []statusAt, poll time.Duration) (waitOutcome, []string) {
	base := time.Date(2026, 7, 20, 23, 25, 57, 0, time.UTC)
	var logs []string
	for elapsed := time.Duration(0); ; elapsed += poll {
		s := replaySeq(seq, elapsed)
		step := w.Observe(base.Add(elapsed), Obs{
			ReadOK: !s.readFail,
			Status: s.status,
			Detail: s.detail,
			Extra:  true,
		})
		if step.Transitioned && step.Message != "" {
			logs = append(logs, step.Message)
		}
		switch step.Decision {
		case WaitReady:
			return waitOutcome{ok: true, elapsed: elapsed, message: step.Message}, logs
		case WaitFailTerminal, WaitFailBudget, WaitFailMisconfigured:
			return waitOutcome{ok: false, elapsed: elapsed, message: step.Message}, logs
		}
		// Safety net so a bug in the watch cannot hang the unit gate.
		if elapsed > 4*time.Hour {
			return waitOutcome{ok: false, elapsed: elapsed, message: "REPLAY RAN AWAY: the watch never returned a decision"}, logs
		}
	}
}

// legacyPositiveEdgeWait is main's PRE-FIX algorithm, transcribed verbatim from
// TestPlatformAgentMgmtMCP_Staging's concierge loop and from
// waitForWorkspaceOnlineRoutable: poll for the one success value until a fixed
// wall-clock deadline, never look at anything else, then report a timeout.
// Kept here permanently so the regression this file fixes can never be
// reintroduced without a test noticing.
func legacyPositiveEdgeWait(ready string, budget, poll time.Duration, seq []statusAt) waitOutcome {
	var last string
	for elapsed := time.Duration(0); elapsed < budget; elapsed += poll {
		s := replaySeq(seq, elapsed)
		if s.readFail {
			continue
		}
		last = s.status
		if s.status == ready {
			return waitOutcome{ok: true, elapsed: elapsed}
		}
	}
	return waitOutcome{
		ok:      false,
		elapsed: budget,
		message: fmt.Sprintf("never reached %s WITHIN %s (last status=%q)", ready, budget, last),
	}
}

// ── The real retention sequences ─────────────────────────────────────────────

// seqRedPublishedFailure is job 817356 / tenant e2e-life-550377-0ed05f: the
// control plane published `failed` at t+93s and never revised it for the
// remaining 13m32s the gate kept polling.
var seqRedPublishedFailure = []statusAt{
	{at: 0, status: "provisioning"},
	{at: 93 * time.Second, status: "failed",
		detail: "platform agent heartbeat denied: management MCP server absent (mcp_server_present=false); refusing to mark online (RCA #2970 FAIL-CLOSED)"},
}

// seqGreenMedian is the median GREEN boot over 209 runs.
var seqGreenMedian = []statusAt{
	{at: 0, status: "provisioning"},
	{at: 10 * time.Second, status: "online"},
}

// seqGreenSlowest is the SLOWEST GREEN boot over 209 runs (137s).
var seqGreenSlowest = []statusAt{
	{at: 0, status: "provisioning"},
	{at: 137 * time.Second, status: "online"},
}

// seqStuckNoVerdict is the OTHER red shape (2 of the 26): the subject wedges in
// a non-terminal state and the control plane publishes nothing at all. Job
// 915241 (2026-08-05) ended with last status="provisioning" after 15m.
var seqStuckNoVerdict = []statusAt{
	{at: 0, status: "provisioning"},
}

// seqSelfHealed is the case the settle window exists to protect: the control
// plane marks the concierge failed, fires its declared-plugin reconcile (#33
// deadlock-break) and the NEXT heartbeat promotes it back to online. A gate
// that aborted on the FIRST sighting of `failed` would manufacture a red here.
var seqSelfHealed = []statusAt{
	{at: 0, status: "provisioning"},
	{at: 93 * time.Second, status: "failed", detail: "management MCP server absent"},
	{at: 150 * time.Second, status: "online"},
}

const (
	conciergeBudget = 15 * time.Minute
	conciergePoll   = 15 * time.Second
)

// ── 1. THE FAIL-BEFORE: the pre-fix algorithm on the real red sequence ───────

func TestLegacyPositiveEdgeWait_BurnsFullBudgetAndMisreportsAPublishedFailure(t *testing.T) {
	got := legacyPositiveEdgeWait("online", conciergeBudget, conciergePoll, seqRedPublishedFailure)

	if got.ok {
		t.Fatalf("pre-fix algorithm should not have reported ready on the red sequence")
	}
	// THE DEFECT, executable: the whole 15m budget spent on a verdict that was
	// published at t+93s.
	if got.elapsed != conciergeBudget {
		t.Fatalf("pre-fix algorithm elapsed=%s, expected the FULL budget %s — this test documents the defect; if it no longer burns the budget the transcription has drifted from the code it models",
			got.elapsed, conciergeBudget)
	}
	// ...and it names a TIMEOUT, not the failure.
	if !strings.Contains(got.message, "never reached") {
		t.Fatalf("pre-fix message %q should be timeout-shaped", got.message)
	}
	if strings.Contains(strings.ToLower(got.message), "terminal") ||
		strings.Contains(got.message, "RCA #2970") {
		t.Fatalf("pre-fix message %q must NOT carry the control plane's reason — that is the gap being closed", got.message)
	}
	t.Logf("PRE-FIX (main): %s after %s", got.message, got.elapsed)
}

// ── 2. THE GREEN-AFTER: the fixed wait on the same real red sequence ─────────

func TestReadinessWatch_AbortsPromptlyOnAPublishedTerminalVerdict(t *testing.T) {
	w := NewConciergeOnlineWatch(conciergeBudget, TerminalSettleDefault)
	got, logs := runWatchWait(w, seqRedPublishedFailure, conciergePoll)

	if got.ok {
		t.Fatalf("the fixed wait must FAIL on a published terminal verdict, got ready")
	}

	// PROMPT: the verdict was published at t+93s and the settle is 180s, so the
	// gate must speak by ~t+273s — not at the 900s budget. Assert a hard ceiling
	// well under the budget so "prompt" is a property, not a hope.
	maxPrompt := 93*time.Second + TerminalSettleDefault + conciergePoll
	if got.elapsed > maxPrompt {
		t.Fatalf("fixed wait took %s; must abort within %s (published-at + settle + one poll), far short of the %s budget",
			got.elapsed, maxPrompt, conciergeBudget)
	}
	if got.elapsed >= conciergeBudget {
		t.Fatalf("fixed wait still burned the full budget (%s) — the terminal arm did not fire", got.elapsed)
	}

	// DISTINGUISHABLE: the message must name the terminal status, say it is a
	// published verdict rather than a timeout, and quote the CP's own reason.
	for _, want := range []string{
		`TERMINAL status="failed"`,
		"PUBLISHED FAILURE VERDICT",
		"RCA #2970 FAIL-CLOSED",
		"Observed status trace: provisioning@0s -> failed@",
	} {
		if !strings.Contains(got.message, want) {
			t.Fatalf("failure message must contain %q so the red is self-diagnosing.\nGot: %s", want, got.message)
		}
	}
	// It must NOT read as a timeout — that wording is reserved for the other,
	// genuinely different failure mode.
	if strings.Contains(got.message, "never reached \"online\" within") {
		t.Fatalf("a published failure must not be worded as a timeout: %s", got.message)
	}

	// The status trace must have been LOGGED as it happened. Guard B's reds were
	// undiagnosable precisely because its loop logged nothing between "concierge
	// id: X" and the final timeout.
	if len(logs) < 2 {
		t.Fatalf("expected a logged line per status transition, got %d: %v", len(logs), logs)
	}
	t.Logf("POST-FIX: aborted after %s (budget %s)", got.elapsed, conciergeBudget)
	for _, l := range logs {
		t.Logf("  progress: %s", l)
	}
}

// ── 3. NEGATIVE CONTROL — vary exactly ONE input ─────────────────────────────
//
// Same watch, same poll, same budget, same starting status, same elapsed times.
// The ONLY thing that changes is what the workspace does at t+93s. A workspace
// that never becomes ready must still FAIL in every arm; what must differ is
// HOW FAST it fails and WHAT IT SAYS.

func TestReadinessWatch_NegativeControl_OneInputVaried(t *testing.T) {
	cases := []struct {
		name string
		seq  []statusAt

		wantOK bool
		// wantByAtMost bounds how long the wait may take.
		wantByAtMost time.Duration
		wantContains []string
		wantAbsent   []string
	}{
		{
			// CONTROL: healthy. Ready, fast.
			name:         "green_median_boot",
			seq:          seqGreenMedian,
			wantOK:       true,
			wantByAtMost: 30 * time.Second,
		},
		{
			// CONTROL: the slowest boot that ever passed. Must still pass — the
			// fix must not clip the real distribution.
			name:         "green_slowest_ever_observed_137s",
			seq:          seqGreenSlowest,
			wantOK:       true,
			wantByAtMost: 150 * time.Second,
		},
		{
			// VARIED INPUT: at t+93s the CP publishes `failed` instead of
			// `online`. Must fail PROMPTLY and name the verdict.
			name:         "dead_published_failed",
			seq:          seqRedPublishedFailure,
			wantOK:       false,
			wantByAtMost: 93*time.Second + TerminalSettleDefault + conciergePoll,
			wantContains: []string{"PUBLISHED FAILURE VERDICT", `TERMINAL status="failed"`},
			wantAbsent:   []string{"STUCK, not FAILED"},
		},
		{
			// VARIED INPUT: at t+93s nothing happens at all — no verdict is ever
			// published. This one MUST burn the budget (there is nothing to react
			// to), and it must say so in words that cannot be mistaken for the
			// case above.
			name:         "stuck_no_verdict_ever_published",
			seq:          seqStuckNoVerdict,
			wantOK:       false,
			wantByAtMost: conciergeBudget + conciergePoll,
			wantContains: []string{"STUCK, not FAILED", `never reached "online"`},
			wantAbsent:   []string{"PUBLISHED FAILURE VERDICT"},
		},
		{
			// VARIED INPUT: the CP publishes `failed` and then SELF-HEALS inside
			// its own grace window. The gate must NOT have jumped the gun.
			name:         "failed_then_self_healed_within_grace",
			seq:          seqSelfHealed,
			wantOK:       true,
			wantByAtMost: 165 * time.Second,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := NewConciergeOnlineWatch(conciergeBudget, TerminalSettleDefault)
			got, _ := runWatchWait(w, tc.seq, conciergePoll)
			if got.ok != tc.wantOK {
				t.Fatalf("ok=%v want %v (message: %s)", got.ok, tc.wantOK, got.message)
			}
			if got.elapsed > tc.wantByAtMost {
				t.Fatalf("took %s, must resolve within %s", got.elapsed, tc.wantByAtMost)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got.message, want) {
					t.Fatalf("message missing %q: %s", want, got.message)
				}
			}
			for _, no := range tc.wantAbsent {
				if strings.Contains(got.message, no) {
					t.Fatalf("message must NOT contain %q (the two failure modes must stay distinguishable): %s", no, got.message)
				}
			}
			t.Logf("%s -> ok=%v after %s", tc.name, got.ok, got.elapsed)
		})
	}
}

// The two failure modes must never be reported with the same words. If they
// ever converge, a stuck subject and a dead one become indistinguishable in a
// log again — which is how this cluster was mis-triaged for a month.
func TestReadinessWatch_TheTwoFailureMessagesAreDistinguishable(t *testing.T) {
	wDead := NewConciergeOnlineWatch(conciergeBudget, TerminalSettleDefault)
	dead, _ := runWatchWait(wDead, seqRedPublishedFailure, conciergePoll)
	wStuck := NewConciergeOnlineWatch(conciergeBudget, TerminalSettleDefault)
	stuck, _ := runWatchWait(wStuck, seqStuckNoVerdict, conciergePoll)

	if dead.message == stuck.message {
		t.Fatalf("a published failure and a stuck subject produced the SAME message: %s", dead.message)
	}
	if dead.elapsed >= stuck.elapsed {
		t.Fatalf("a published failure (%s) must be reported strictly sooner than a stuck subject (%s)", dead.elapsed, stuck.elapsed)
	}
}

// ── 4. NON-VACUITY ───────────────────────────────────────────────────────────

// A watch whose terminal arm can never fire is a readiness check that asserts
// nothing. Each mutation below is refused BEFORE any polling happens.
func TestReadinessWatch_RefusesAConfigurationThatCouldNeverFail(t *testing.T) {
	mutations := []struct {
		name    string
		mutate  func(*ReadinessWatch)
		wantSub string
	}{
		{
			name:    "empty_terminal_set",
			mutate:  func(w *ReadinessWatch) { w.Terminal = nil },
			wantSub: "EMPTY terminal-status set",
		},
		{
			name:    "settle_outlives_budget",
			mutate:  func(w *ReadinessWatch) { w.Settle = w.Budget },
			wantSub: "can NEVER fire",
		},
		{
			name:    "zero_settle",
			mutate:  func(w *ReadinessWatch) { w.Settle = 0 },
			wantSub: "non-positive Settle",
		},
		{
			name:    "no_ready_status",
			mutate:  func(w *ReadinessWatch) { w.Ready = "" },
			wantSub: "no Ready status",
		},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			w := NewConciergeOnlineWatch(conciergeBudget, TerminalSettleDefault)
			m.mutate(w)
			step := w.Observe(time.Now(), Obs{ReadOK: true, Status: "provisioning", Extra: true})
			if step.Decision != WaitFailMisconfigured {
				t.Fatalf("mutation %q produced decision %v; a watch that cannot fail must be REFUSED, not tolerated", m.name, step.Decision)
			}
			if !strings.Contains(step.Message, m.wantSub) {
				t.Fatalf("refusal message must name the defect %q: %s", m.wantSub, step.Message)
			}
		})
	}
}

// The shipped watches must actually be sound — the guard above is worthless if
// the real configurations trip it.
func TestReadinessWatch_ShippedConfigurationsAreSound(t *testing.T) {
	// The EXACT configurations the deploy gate constructs — budgets and
	// env-resolved settle included — not look-alikes built from test literals.
	// If these were only reachable under the staging_e2e tag this proof could
	// not see them, and a shipped watch that can never fail would go unnoticed.
	for _, w := range []*ReadinessWatch{
		DeployConciergeOnlineWatch(),
		DeployOrgInstanceRunningWatch("e2e-mcp-x"),
		DeployWorkspaceOnlineRoutableWatch("initial boot", 15*time.Minute),
		NewConciergeOnlineWatch(conciergeBudget, TerminalSettleDefault),
		NewWorkspaceOnlineRoutableWatch("initial boot", conciergeBudget, TerminalSettleDefault),
		NewOrgInstanceRunningWatch("e2e-mcp-x", 7*time.Minute, TerminalSettleDefault),
	} {
		if msg := w.validate(); msg != "" {
			t.Fatalf("shipped watch %q is misconfigured: %s", w.Subject, msg)
		}
		if len(w.Terminal) == 0 {
			t.Fatalf("shipped watch %q has an empty terminal set", w.Subject)
		}
		// Each named terminal status must genuinely classify as terminal — a
		// list nobody reads is the same vacuum in a different shape.
		for _, s := range w.Terminal {
			if !w.isTerminal(s) {
				t.Fatalf("watch %q lists %q as terminal but does not classify it as such", w.Subject, s)
			}
		}
		if w.isTerminal(w.Ready) {
			t.Fatalf("watch %q treats its own READY status %q as terminal", w.Subject, w.Ready)
		}
		if w.isTerminal("provisioning") {
			t.Fatalf("watch %q treats the normal in-flight status as terminal — that would red every healthy boot", w.Subject)
		}
	}
}

// The settle window must not be switchable off from the environment. Every
// "absent / malformed / zero / negative" input — the exact shapes that turn a
// shell check into a no-op — must resolve to the SAFE default.
func TestResolveTerminalSettle_CannotBeDisabledFromTheEnvironment(t *testing.T) {
	for _, raw := range []string{"", " ", "\t\n", "0", "-1", "-999", "abc", "1e3", "12.5", "0x10", "  0  "} {
		if got := ResolveTerminalSettle(raw); got != TerminalSettleDefault {
			t.Fatalf("ResolveTerminalSettle(%q) = %s; every non-positive/malformed input must fall back to the default %s so the terminal arm can never be silenced",
				raw, got, TerminalSettleDefault)
		}
	}
	if got := ResolveTerminalSettle("30"); got != 30*time.Second {
		t.Fatalf("a valid shortening override must be honoured, got %s", got)
	}
	if got := ResolveTerminalSettle("99999"); got != TerminalSettleMax {
		t.Fatalf("an oversized override must clamp to %s, got %s", TerminalSettleMax, got)
	}
	if TerminalSettleDefault <= 0 || TerminalSettleDefault >= conciergeBudget {
		t.Fatalf("TerminalSettleDefault=%s is not a usable window against the %s budget", TerminalSettleDefault, conciergeBudget)
	}
}

// The settle default is the control plane's OWN self-heal window for this
// signal (handlers.managementMCPUnloadedGrace / db.LivenessTTL, both 180s).
// Pinned so a silent drift here cannot make the gate out-run the remediation it
// is watching.
func TestTerminalSettleDefault_MatchesTheControlPlaneSelfHealWindow(t *testing.T) {
	const cpSelfHealWindow = 180 * time.Second // registry.go managementMCPUnloadedGrace, db.LivenessTTL
	if TerminalSettleDefault != cpSelfHealWindow {
		t.Fatalf("TerminalSettleDefault=%s but the control plane's self-heal window is %s — the gate must never declare a terminal verdict faster than the remediation it observes",
			TerminalSettleDefault, cpSelfHealWindow)
	}
	// ...and it must have real headroom over the measured distribution: the
	// slowest GREEN concierge boot in 209 retention runs was 137s and the
	// slowest observed provisioning->failed transition was 126s.
	const slowestGreenBoot = 137 * time.Second
	if TerminalSettleDefault <= slowestGreenBoot {
		t.Fatalf("settle %s must exceed the slowest observed healthy boot %s, or a slow-but-healthy boot could be called dead",
			TerminalSettleDefault, slowestGreenBoot)
	}
}

// A failed READ is not evidence. It must neither satisfy readiness nor start a
// terminal settle — otherwise a flaky tenant proxy could red the gate.
func TestReadinessWatch_AFailedReadIsNotEvidence(t *testing.T) {
	w := NewConciergeOnlineWatch(conciergeBudget, TerminalSettleDefault)
	base := time.Now()
	for i := 0; i < 10; i++ {
		step := w.Observe(base.Add(time.Duration(i)*conciergePoll), Obs{ReadOK: false, Extra: true})
		if step.Decision != WaitContinue {
			t.Fatalf("a failed read produced %v; it must only advance the budget", step.Decision)
		}
	}
	step := w.Observe(base.Add(11*conciergePoll), Obs{ReadOK: true, Status: "online", Extra: true})
	if step.Decision != WaitReady {
		t.Fatalf("after unreadable polls a genuine online must still be READY, got %v", step.Decision)
	}
}

// RequireExtra must be load-bearing: an online-but-not-routable workspace is
// NOT ready. Without this, waitForWorkspaceOnlineRoutable's second condition
// would be silently dropped by the refactor.
func TestReadinessWatch_RequireExtraIsLoadBearing(t *testing.T) {
	w := NewWorkspaceOnlineRoutableWatch("initial boot", conciergeBudget, TerminalSettleDefault)
	step := w.Observe(time.Now(), Obs{ReadOK: true, Status: "online", Extra: false, ExtraLabel: "routable=false"})
	if step.Decision == WaitReady {
		t.Fatalf("online but NOT routable must not be READY")
	}
	w2 := NewWorkspaceOnlineRoutableWatch("initial boot", conciergeBudget, TerminalSettleDefault)
	step2 := w2.Observe(time.Now(), Obs{ReadOK: true, Status: "online", Extra: true, ExtraLabel: "routable=true"})
	if step2.Decision != WaitReady {
		t.Fatalf("online AND routable must be READY, got %v", step2.Decision)
	}
}

// The org-instance watch must react to the org-level verdict the admin list
// already returns (instance_status + last_error) rather than polling for
// `running` until the 7m budget expires.
func TestOrgInstanceWatch_AbortsOnAPublishedProvisionFailure(t *testing.T) {
	const orgBudget = 7 * time.Minute
	seq := []statusAt{
		{at: 0, status: ""}, // row present, instance_status omitted (not yet provisioned)
		{at: 20 * time.Second, status: "provisioning"},
		{at: 60 * time.Second, status: "failed",
			detail: "organizations_provider_check violation: migration 071 rejects literal 'local'"},
	}
	w := NewOrgInstanceRunningWatch("e2e-mcp-610403", orgBudget, TerminalSettleDefault)
	got, _ := runWatchWait(w, seq, conciergePoll)
	if got.ok {
		t.Fatalf("a published org provision failure must fail the wait")
	}
	if got.elapsed >= orgBudget {
		t.Fatalf("org wait burned the full %s budget on a verdict published at t+60s", orgBudget)
	}
	if !strings.Contains(got.message, "organizations_provider_check") {
		t.Fatalf("the org failure must quote the control plane's own last_error: %s", got.message)
	}
	if !strings.Contains(got.message, `TERMINAL status="failed"`) {
		t.Fatalf("the org failure must name the terminal status: %s", got.message)
	}
}

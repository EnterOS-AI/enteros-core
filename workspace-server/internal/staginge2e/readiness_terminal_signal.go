package staginge2e

// readiness_terminal_signal.go — the PURE, unit-provable decision logic behind
// every "wait until it is ready" in the staging e2e gates.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE DEFECT THIS FILE FIXES (one defect, three call sites)
// ─────────────────────────────────────────────────────────────────────────────
//
// Every readiness wait in this package was POSITIVE-EDGE-ONLY: it polled for a
// single success value against a fixed wall-clock budget and NEVER read the
// failure signal that the very same API response already carried.
//
//	adminCreateOrg                     polled instance_status=="running"  for 7m
//	                                   while the same row carried last_error.
//	TestPlatformAgentMgmtMCP_Staging   polled status=="online"           for 15m
//	                                   while the same body carried last_sample_error.
//	waitForWorkspaceOnlineRoutable     polled status=="online"           for 15m
//	                                   likewise.
//
// Consequence, measured over the 278 e2e-smoke verdicts in Gitea retention
// (2026-07-06 → 08-06, staging-tenant-cd.yml):
//
//   - 35 of the 58 genuine Guard B reds (60%) were filed as "boot/provision
//     timeouts". They are NOT timeouts. In 24 of the 26 "concierge never online"
//     reds the LAST OBSERVED STATUS WAS "failed" — a verdict the control plane
//     had already published, with a reason, minutes earlier. The gate sat on it
//     for the rest of the 15 minutes and then reported a timeout.
//   - The budgets are not marginal, so raising or lowering them is irrelevant:
//     on the 209 GREEN runs the concierge reached online in min 31s / p50 47s /
//     p99 136s / MAX 137s against a 900s budget (6.6x the worst success; not one
//     green ever exceeded 300s), and the org reached running in p50 31s / p99
//     92s / max 249s against a 420s budget.
//   - Across 999 workspace status transitions logged by the lifecycle test,
//     provisioning→online occurred 969 times (median 10s, max 112s) and
//     provisioning→failed 9 times (median 92s, max 126s).
//     failed→online occurred ZERO times — in each of the 9 cases the row was
//     then observed continuously for the remaining ~13.5 minutes of the budget
//     and never moved.
//
// So nothing here is SLOW; things are DEAD, and the gate refuses to look at the
// death certificate. That is why the cluster was mis-triaged as a timing
// problem, and why four separate multi-hour staging incidents (2026-07-09/10,
// 07-20/21, 07-23, 08-05) each produced ~10 reds whose logs say only "never
// reached online WITHIN 15m" — the gate deletes its own evidence (t.Cleanup
// purges the org seconds later) and never quotes the reason the CP handed it.
// adminCreateOrg already learned this lesson once and grew a self-diagnosing
// timeout; the concierge wait — where 26 of the 35 reds live — never did.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE FIX: wait on the REAL SIGNAL, in BOTH directions
// ─────────────────────────────────────────────────────────────────────────────
//
// ReadinessWatch turns each poll into a three-way verdict instead of a boolean:
//
//	READY     — the real readiness signal arrived.                → return
//	TERMINAL  — the CP published a failure verdict and HELD it.   → fail NOW,
//	            quoting the CP's own reason.
//	PENDING   — nothing conclusive published yet.                 → poll again.
//
// It deliberately does NOT abort on the FIRST sighting of a terminal status.
// The control plane's platform-agent heartbeat gate (RCA #2970 / core#3082,
// workspace-server/internal/handlers/registry.go) lists `failed` among the
// statuses a later heartbeat MAY legitimately promote back to online, and the
// #33 deadlock-break deliberately marks a concierge failed, fires the declared-
// plugin reconcile, and expects the NEXT heartbeat to recover it. Aborting
// instantly would race that remediation and manufacture a new class of red.
//
// So a terminal status must PERSIST for a settle window before it is believed.
// The window is not invented: it is the control plane's OWN self-heal /
// flap-absorption window for exactly this signal — managementMCPUnloadedGrace
// (registry.go) and db.LivenessTTL are both 180s. Holding the same 180s here
// means the gate can never out-run the remediation it is observing, and the
// measured distribution says it costs nothing: no green run has ever spent more
// than 137s getting online in the first place. Worst case a genuinely dead
// concierge is now called at ~126s (slowest observed time-to-failed) + 180s +
// one poll ≈ 5m20s instead of 15m00s, and the message names the terminal status
// and the CP's reason instead of a stopwatch.
//
// ─────────────────────────────────────────────────────────────────────────────
// NON-VACUITY (the property this file must never lose)
// ─────────────────────────────────────────────────────────────────────────────
//
// A readiness check that can only ever say "keep waiting" is a check that
// asserts nothing — the `grep -q ""` of gates. Three structural guards, all
// unit-proven in readiness_terminal_signal_test.go:
//
//  1. A watch with an EMPTY terminal set, a non-positive settle/budget, or a
//     settle window that outlives the budget (so the terminal arm could never
//     fire) is REFUSED as misconfigured on the first observation — it never
//     silently degrades into "wait, then time out".
//  2. ResolveTerminalSettle CANNOT be turned into a no-op from the environment:
//     unset, empty, garbage, zero and negative all fall back to the default, and
//     an absurdly large override is clamped.
//  3. The terminal and ready verdicts are proven to DISCRIMINATE: the same
//     replayed observation sequence returns READY when the status reaches
//     online and TERMINAL when it does not, and the two failure messages are
//     textually distinguishable so a stuck-with-no-verdict run can never be
//     read as a published failure.
//
// Untagged on purpose (mirrors orginstancestatus.go and
// platform_agent_mgmt_mcp_gate.go): the decision runs in the normal
// `go test ./...` gate with no live tenant, and the tagged live waits FEED this
// same code rather than keeping a second, drifting copy.

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
)

// WaitDecision is the verdict for ONE polled observation of a readiness wait.
type WaitDecision int

const (
	// WaitContinue — nothing conclusive has been published; poll again.
	WaitContinue WaitDecision = iota
	// WaitReady — the REAL readiness signal arrived.
	WaitReady
	// WaitFailTerminal — the control plane published a terminal failure verdict
	// and held it past its own self-heal window. Stop now and quote the reason.
	WaitFailTerminal
	// WaitFailBudget — the budget expired with NO verdict in either direction:
	// the subject is genuinely STUCK and the control plane said nothing about
	// it. Deliberately a DIFFERENT decision (and a differently-worded message)
	// from WaitFailTerminal so the two failure modes can never be conflated
	// again, which is precisely how this whole cluster got mis-triaged.
	WaitFailBudget
	// WaitFailMisconfigured — the watch itself cannot decide anything. Refused
	// loudly rather than degrading into a wait whose only possible failure is a
	// timeout (a vacuous gate).
	WaitFailMisconfigured
)

func (d WaitDecision) String() string {
	switch d {
	case WaitContinue:
		return "continue"
	case WaitReady:
		return "ready"
	case WaitFailTerminal:
		return "fail-terminal"
	case WaitFailBudget:
		return "fail-budget"
	case WaitFailMisconfigured:
		return "fail-misconfigured"
	}
	return "unknown"
}

// Terminal reports whether the decision ends the wait unsuccessfully.
func (d WaitDecision) Terminal() bool {
	return d == WaitFailTerminal || d == WaitFailBudget || d == WaitFailMisconfigured
}

const (
	// TerminalSettleDefault is how long a terminal status must PERSIST before
	// the gate believes it.
	//
	// It is the control plane's own remediation window for this exact signal,
	// not a number chosen here: handlers.managementMCPUnloadedGrace (the
	// platform-agent management-MCP flap absorber) and db.LivenessTTL are both
	// 180s. Those live in packages this test-support package must not import
	// (internal/handlers carries non-test package init side effects and would
	// drag a production wiring graph into the e2e binary), so the value is
	// restated here WITH its provenance and pinned by a unit test.
	//
	// Headroom against the measured distribution: the slowest observed
	// provisioning→failed transition is 126s and the slowest GREEN concierge
	// boot in 209 runs is 137s, so 180s of persistence cannot clip a healthy
	// boot; and failed→online was observed ZERO times in retention, so the
	// window costs 180s on a red and nothing on a green.
	TerminalSettleDefault = 180 * time.Second

	// TerminalSettleMax bounds an operator override. An override larger than
	// the wait's own budget would silently disable the terminal arm and return
	// the gate to the positive-edge-only behaviour this file exists to remove,
	// so overrides are clamped here AND re-checked against the budget in
	// ReadinessWatch.validate.
	TerminalSettleMax = 10 * time.Minute

	// TerminalSettleEnv lets an operator shorten the settle for a hand-run
	// reproduction. It can never lengthen it past TerminalSettleMax and can
	// never disable the terminal arm.
	TerminalSettleEnv = "E2E_TERMINAL_SETTLE_SECS"

	// conciergeOnlineBudget / orgProvisionBudget are the wall-clock budgets of
	// the two waits, UNCHANGED from before this fix and deliberately so: the
	// measured distributions say they were never the problem (a fresh org
	// reaches running in p50 31s / p99 92s / max 249s of 420s; a concierge
	// reaches online in p50 47s / p99 136s / max 137s of 900s, across 209 green
	// runs). Raising a budget would have fixed nothing and lowering one would
	// clip the tail. What changed is that neither wait has to REACH its budget
	// to speak any more.
	conciergeOnlineBudget = 15 * time.Minute
	orgProvisionBudget    = 7 * time.Minute
)

// ResolveTerminalSettle turns a raw env value into the settle window.
//
// FAIL-SAFE BY CONSTRUCTION: every input that does not name a positive number
// of seconds — unset, empty, whitespace, garbage, "0", a negative — resolves to
// TerminalSettleDefault. There is no input that yields zero or an unbounded
// window, so the terminal arm cannot be switched off from the environment.
// (An unset variable making a check a no-op is the exact failure shape this
// repo keeps re-learning; here the no-op input is the SAFE input.)
func ResolveTerminalSettle(raw string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return TerminalSettleDefault
	}
	d := time.Duration(n) * time.Second
	if d > TerminalSettleMax {
		return TerminalSettleMax
	}
	return d
}

// Obs is ONE polled observation, exactly as the caller read it.
type Obs struct {
	// ReadOK is true when the read itself succeeded (HTTP 200 / row present).
	// A failed read is NOT evidence of anything: it neither satisfies nor
	// refutes readiness, so it only advances the budget.
	ReadOK bool
	// Status is the status field the API returned.
	Status string
	// Detail is the control plane's OWN reason field for this subject
	// (workspaces.last_sample_error / org_instances.last_error). The whole
	// point of this file is that this value already existed on every red run
	// and no gate ever read it.
	Detail string
	// Extra is a SECOND readiness condition the caller also requires (the
	// lifecycle wait needs online AND routable). Only consulted when the watch
	// sets RequireExtra; ExtraLabel is rendered into the trace so a workspace
	// that is online-but-not-routable is visibly distinct from a healthy one.
	Extra      bool
	ExtraLabel string
}

// WaitStep is the outcome of one observation.
type WaitStep struct {
	// Decision is the verdict.
	Decision WaitDecision
	// Transitioned is true when this observation differs from the previous one.
	// The live waits log ONLY on a transition — the whole reason the lifecycle
	// test's reds were diagnosable from retention and Guard B's were not.
	Transitioned bool
	// Message is the transition line (on WaitContinue/WaitReady) or the full
	// failure reason (on a terminal decision).
	Message string
}

// ReadinessWatch is the pure state machine driving one readiness wait. It holds
// no clock and no I/O: the caller supplies `now` and the observation, which is
// what makes the whole wait — including the settle window and the budget —
// replayable in a unit test against real logged sequences.
type ReadinessWatch struct {
	// Subject names the thing being waited on, for messages.
	Subject string
	// Ready is the single status that means the real signal arrived.
	Ready string
	// Terminal are the statuses that are the control plane's PUBLISHED verdict
	// that this boot did not survive.
	Terminal []string
	// Settle is how long a Terminal status must persist before it is believed.
	Settle time.Duration
	// Budget is the overall wall-clock budget for the wait.
	Budget time.Duration
	// RequireExtra makes Obs.Extra a second necessary condition for READY.
	RequireExtra bool

	start          time.Time
	started        bool
	lastKey        string
	lastStatus     string
	terminalStatus string
	terminalDetail string
	terminalSince  time.Time
	firstSeenAt    time.Time
	trace          []string
}

// validate refuses a watch whose terminal arm could never fire. Returns "" when
// the configuration is sound.
func (w *ReadinessWatch) validate() string {
	var bad []string
	if strings.TrimSpace(w.Ready) == "" {
		bad = append(bad, "no Ready status (the wait could never succeed)")
	}
	if len(w.Terminal) == 0 {
		bad = append(bad, "EMPTY terminal-status set (the wait could only ever end in a timeout — a vacuous readiness check)")
	}
	if w.Budget <= 0 {
		bad = append(bad, "non-positive Budget")
	}
	if w.Settle <= 0 {
		bad = append(bad, "non-positive Settle (a terminal status would be believed instantly, racing the control plane's own self-heal)")
	}
	if w.Settle > 0 && w.Budget > 0 && w.Settle >= w.Budget {
		bad = append(bad, fmt.Sprintf("Settle (%s) >= Budget (%s), so the terminal arm can NEVER fire and the wait silently degrades to positive-edge-only", w.Settle, w.Budget))
	}
	if len(bad) == 0 {
		return ""
	}
	return "readiness watch for " + nonEmpty(w.Subject, "<unnamed subject>") +
		" is MISCONFIGURED: " + strings.Join(bad, "; ")
}

// isTerminal reports whether status is in the published-failure set.
func (w *ReadinessWatch) isTerminal(status string) bool {
	return containsStr(w.Terminal, status)
}

// Trace renders the observed status timeline, e.g.
// `provisioning@0s -> failed@93s`. It is the diagnostic that was missing from
// every Guard B red in retention.
func (w *ReadinessWatch) Trace() string {
	if len(w.trace) == 0 {
		return "<no successful read>"
	}
	return strings.Join(w.trace, " -> ")
}

// LastStatus is the most recent successfully-read status ("" if none).
func (w *ReadinessWatch) LastStatus() string { return w.lastStatus }

// Observe feeds ONE poll into the watch. `now` is injected rather than read
// from the clock so an entire wait — settle window and budget included — can be
// replayed deterministically in a unit test against a real logged sequence.
func (w *ReadinessWatch) Observe(now time.Time, o Obs) WaitStep {
	if !w.started {
		if msg := w.validate(); msg != "" {
			return WaitStep{Decision: WaitFailMisconfigured, Transitioned: true, Message: msg}
		}
		w.start = now
		w.started = true
	}

	elapsed := now.Sub(w.start)

	key := "<unreadable>"
	if o.ReadOK {
		key = nonEmpty(o.Status, "<no status field>")
		if w.RequireExtra {
			key += " " + nonEmpty(o.ExtraLabel, fmt.Sprintf("extra=%v", o.Extra))
		}
	}
	step := WaitStep{}
	if key != w.lastKey {
		step.Transitioned = true
		w.lastKey = key
		w.trace = append(w.trace, fmt.Sprintf("%s@%s", key, elapsed.Truncate(time.Second)))
		step.Message = fmt.Sprintf("[%s] %s (t+%s)", w.Subject, key, elapsed.Truncate(time.Second))
	}

	if o.ReadOK {
		w.lastStatus = o.Status
		if w.firstSeenAt.IsZero() {
			w.firstSeenAt = now
		}

		if o.Status == w.Ready && (!w.RequireExtra || o.Extra) {
			step.Decision = WaitReady
			if step.Message == "" {
				step.Message = fmt.Sprintf("[%s] %s (t+%s)", w.Subject, key, elapsed.Truncate(time.Second))
			}
			return step
		}

		switch {
		case w.isTerminal(o.Status):
			// A DIFFERENT terminal status restarts the settle: the subject is
			// still moving, so the control plane has not settled on a verdict.
			if w.terminalStatus != o.Status {
				w.terminalStatus = o.Status
				w.terminalSince = now
			}
			// Keep the most informative reason we have ever seen for it.
			if strings.TrimSpace(o.Detail) != "" {
				w.terminalDetail = o.Detail
			}
			if held := now.Sub(w.terminalSince); held >= w.Settle {
				step.Decision = WaitFailTerminal
				step.Message = w.terminalMessage(o.Status, held, elapsed)
				return step
			}
		default:
			// Left the terminal set — the control plane's self-heal is working,
			// so forget the pending verdict and keep waiting on the real signal.
			w.terminalStatus = ""
			w.terminalDetail = ""
			w.terminalSince = time.Time{}
		}
	}

	if elapsed >= w.Budget {
		step.Decision = WaitFailBudget
		step.Message = w.budgetMessage(elapsed)
		return step
	}
	step.Decision = WaitContinue
	return step
}

// terminalMessage names the PUBLISHED failure verdict. Deliberately worded so
// it can never be mistaken for a timeout.
func (w *ReadinessWatch) terminalMessage(status string, held, elapsed time.Duration) string {
	return fmt.Sprintf(
		"%s reached TERMINAL status=%q and HELD it for %s (>= the control plane's own %s self-heal window) after %s of waiting. "+
			"This is a PUBLISHED FAILURE VERDICT, not a slow boot — the control plane decided this boot would not survive and never revised it. "+
			"Control-plane reason: %s. Observed status trace: %s. "+
			"(Waiting out the remaining budget would have added nothing: across the e2e retention window a workspace that reached %q never once returned to %q.)",
		nonEmpty(w.Subject, "subject"), status, held.Truncate(time.Second), w.Settle, elapsed.Truncate(time.Second),
		nonEmpty(strings.TrimSpace(w.terminalDetail), "<the control plane surfaced no reason field>"),
		w.Trace(), status, w.Ready)
}

// budgetMessage names the OTHER failure mode: nothing was ever published.
func (w *ReadinessWatch) budgetMessage(elapsed time.Duration) string {
	return fmt.Sprintf(
		"%s never reached %q within %s AND the control plane never published a terminal verdict about it either "+
			"(last status=%q; terminal statuses watched: [%s]). "+
			"This is STUCK, not FAILED — the subject is wedged in a non-terminal state with nothing said about it, which is a different bug from a published failure. "+
			"Observed status trace: %s.",
		nonEmpty(w.Subject, "subject"), w.Ready, w.Budget, nonEmpty(w.lastStatus, "<never read>"),
		strings.Join(w.Terminal, ","), w.Trace())
}

// ── The two configured watches ───────────────────────────────────────────────

// TerminalOrgInstanceStatuses are the org_instances statuses that are the
// control plane's published verdict that a fresh org's provision did not
// survive: 'failed' is written by the provisioner (and carries org_instances
// .last_error, which GET /cp/admin/orgs already returns as `last_error`), and
// 'abandoned' is the terminal state the failed-provision reaper escalates to.
// Intentionally EXCLUDES 'stopped'/'suspended' — those are operator/billing
// states, not provision verdicts.
var TerminalOrgInstanceStatuses = []string{"failed", "abandoned"}

// NewConciergeOnlineWatch waits for a fresh org's platform agent to reach
// online. Its terminal set reuses terminalBadWorkspaceStatuses — the SAME SSOT
// list Guard B already uses to refuse a created-but-dead workspace row
// (platform_agent_mgmt_mcp_gate.go, core#5052) — so "this workspace did not
// survive" means one thing in this package, not two.
//
// 'degraded' is deliberately NOT terminal: the control plane lists it among the
// statuses a later heartbeat may promote back to online, and it was never once
// observed on a green run, so a run that wedges there correctly falls through to
// the STUCK message rather than being guessed at.
func NewConciergeOnlineWatch(budget, settle time.Duration) *ReadinessWatch {
	return &ReadinessWatch{
		Subject:  "platform agent (concierge)",
		Ready:    string(models.StatusOnline),
		Terminal: append([]string(nil), terminalBadWorkspaceStatuses...),
		Settle:   settle,
		Budget:   budget,
	}
}

// NewWorkspaceOnlineRoutableWatch is the same watch for an ordinary workspace
// boot, where READY additionally requires the row to be routable (url set).
func NewWorkspaceOnlineRoutableWatch(subject string, budget, settle time.Duration) *ReadinessWatch {
	return &ReadinessWatch{
		Subject:      nonEmpty(subject, "workspace"),
		Ready:        string(models.StatusOnline),
		Terminal:     append([]string(nil), terminalBadWorkspaceStatuses...),
		Settle:       settle,
		Budget:       budget,
		RequireExtra: true,
	}
}

// NewOrgInstanceRunningWatch waits for a freshly-created org's instance to
// reach running.
func NewOrgInstanceRunningWatch(slug string, budget, settle time.Duration) *ReadinessWatch {
	return &ReadinessWatch{
		Subject:  "org " + nonEmpty(slug, "<unnamed>") + " instance",
		Ready:    "running",
		Terminal: append([]string(nil), TerminalOrgInstanceStatuses...),
		Settle:   settle,
		Budget:   budget,
	}
}

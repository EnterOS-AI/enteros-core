package handlers

// workspace_restart_prewarm_resilience_test.go — core#5025 findings 4 and 7:
// what the pre-flight does when the control plane is momentarily unavailable,
// and what it holds while it waits.
//
// Finding 4. The pre-flight fails CLOSED on any transport error or 5xx, one shot,
// no retry. The stop leg it precedes (cpStopWithRetryErr) deliberately DOES retry,
// for the stated reason that refusing to reprovision strands the user. The
// pre-flight inherited none of that, so a ~60s control-plane redeploy refuses
// every restart on the fleet — and this box redeploys the control plane routinely.
//
// Finding 7. The pre-flight runs while holding acquireRestartProvisionGate, on a
// context.Background() with a 20-minute client timeout. A hung pull therefore
// freezes every restart, heal and provision path for that workspace for twenty
// minutes. A slow pull is supposed to be slow; it is not supposed to look like a
// total lifecycle freeze.
//
// The two are one design: retries only make sense inside a bounded budget, and a
// bounded budget is what keeps the gate from being a hostage to the registry.

import (
	"context"
	"errors"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// flakyCPProv fails the first failUntil calls with err, then confirms. It also
// records the deadline it saw on each call so the budget can be asserted.
type flakyCPProv struct {
	prewarmCPProv
	failUntil int
	err       error
	attempts  int
	deadlines []time.Duration
	block     chan struct{} // when non-nil, EnsureImage blocks on it
}

func (p *flakyCPProv) EnsureImage(ctx context.Context, req provisioner.EnsureImageRequest) (provisioner.EnsureImageResult, error) {
	p.attempts++
	p.calls = append(p.calls, "EnsureImage")
	p.lastRequest = req
	if dl, ok := ctx.Deadline(); ok {
		p.deadlines = append(p.deadlines, time.Until(dl))
	} else {
		p.deadlines = append(p.deadlines, 0)
	}
	if p.block != nil {
		select {
		case <-p.block:
		case <-ctx.Done():
			return provisioner.EnsureImageResult{}, ctx.Err()
		}
	}
	if p.attempts <= p.failUntil {
		return provisioner.EnsureImageResult{}, p.err
	}
	return provisioner.EnsureImageResult{Status: "ready"}, nil
}

func shrinkPrewarmRetryForTest(t *testing.T) {
	t.Helper()
	prevDelay := cpStopRetryBaseDelay
	cpStopRetryBaseDelay = time.Millisecond
	t.Cleanup(func() { cpStopRetryBaseDelay = prevDelay })
}

func hermesPayload() models.CreateWorkspacePayload {
	return models.CreateWorkspacePayload{Name: "n", Tier: 4, Runtime: "hermes", Template: "hermes"}
}

// TestEnsurePinnedImageBeforeStop_RetriesATransientControlPlane is finding 4.
//
// A control plane that is redeploying answers with a transport error for the ~60s
// it takes to come back. That says NOTHING about the image, and refusing on it
// strands every restart on the fleet for the duration — including the restarts
// that were trying to recover a wedged workspace.
func TestEnsurePinnedImageBeforeStop_RetriesATransientControlPlane(t *testing.T) {
	shrinkPrewarmRetryForTest(t)
	stub := &flakyCPProv{failUntil: cpStopRetryAttempts - 1, err: errors.New("dial tcp 10.0.0.4:443: connect: connection refused")}
	h := &WorkspaceHandler{cpProv: stub}

	if !h.ensurePinnedImageBeforeStop(context.Background(), "ws-redeploy", hermesPayload()) {
		t.Fatalf("a control plane that came back on attempt %d must not refuse the restart; attempts=%d",
			cpStopRetryAttempts, stub.attempts)
	}
	if stub.attempts != cpStopRetryAttempts {
		t.Fatalf("expected the pre-flight to retry up to the stop leg's own budget (%d attempts); got %d",
			cpStopRetryAttempts, stub.attempts)
	}
}

// TestEnsurePinnedImageBeforeStop_RetryBudgetMatchesTheStopLeg pins the two
// policies to each other rather than to two hand-typed numbers. The stop leg's
// budget is the stated precedent for retrying here at all; a pre-flight that
// quietly retried fewer times would strand exactly the users the stop leg's
// retry exists to protect.
func TestEnsurePinnedImageBeforeStop_RetryBudgetMatchesTheStopLeg(t *testing.T) {
	shrinkPrewarmRetryForTest(t)
	// One more failure than the budget allows: the pre-flight must give up and
	// decline, so the boundary is observed rather than assumed.
	stub := &flakyCPProv{failUntil: cpStopRetryAttempts, err: errors.New("502 bad gateway")}
	h := &WorkspaceHandler{cpProv: stub}

	if h.ensurePinnedImageBeforeStop(context.Background(), "ws-down", hermesPayload()) {
		t.Fatal("a control plane that never came back must still fail CLOSED — the running container is all the user has")
	}
	if stub.attempts != cpStopRetryAttempts {
		t.Fatalf("pre-flight made %d attempts, the stop leg is allowed %d — the two budgets must be the SAME value, not two copies of it",
			stub.attempts, cpStopRetryAttempts)
	}
}

// TestEnsurePinnedImageBeforeStop_UnobtainableImageIsNotRetried keeps the retry
// from swallowing the failure this whole guard exists for.
//
// A pin whose digest is not in the registry can never be pulled. Retrying it
// three times with backoff turns a fast, correct refusal into a slow one while
// the per-workspace gate is held — the core#5019 case paying finding 7's cost.
func TestEnsurePinnedImageBeforeStop_UnobtainableImageIsNotRetried(t *testing.T) {
	shrinkPrewarmRetryForTest(t)
	stub := &flakyCPProv{failUntil: 99, err: provisioner.ErrEnsureImagePermanent}
	h := &WorkspaceHandler{cpProv: stub}

	if h.ensurePinnedImageBeforeStop(context.Background(), "ws-bad-digest", hermesPayload()) {
		t.Fatal("an unobtainable digest must decline")
	}
	if stub.attempts != 1 {
		t.Fatalf("a permanently unobtainable image was retried %d times. Retrying what cannot succeed "+
			"only lengthens the window in which the workspace's gate is held.", stub.attempts)
	}
}

// TestEnsurePinnedImageBeforeStop_CompatSkipIsNotRetried — a 404 means this
// control plane has no such endpoint. That is a fact about the deployment, not a
// blip; three round trips will report it three times.
func TestEnsurePinnedImageBeforeStop_CompatSkipIsNotRetried(t *testing.T) {
	shrinkPrewarmRetryForTest(t)
	stub := &flakyCPProv{failUntil: 99, err: provisioner.ErrEnsureImageUnsupported}
	h := &WorkspaceHandler{cpProv: stub}

	if !h.ensurePinnedImageBeforeStop(context.Background(), "ws-old-cp", hermesPayload()) {
		t.Fatal("an older control plane must not block restarts")
	}
	if stub.attempts != 1 {
		t.Fatalf("the compat skip was retried %d times", stub.attempts)
	}
}

// TestEnsurePinnedImageBeforeStop_IsBounded is finding 7's first half.
//
// The pre-flight must carry its OWN deadline. Inheriting context.Background() and
// leaning on a 20-minute HTTP client timeout is what let a hung pull hold the
// per-workspace gate for twenty minutes.
func TestEnsurePinnedImageBeforeStop_IsBounded(t *testing.T) {
	stub := &flakyCPProv{failUntil: 0}
	h := &WorkspaceHandler{cpProv: stub}

	h.ensurePinnedImageBeforeStop(context.Background(), "ws-bounded", hermesPayload())

	if len(stub.deadlines) != 1 {
		t.Fatalf("expected one call, got %d", len(stub.deadlines))
	}
	if stub.deadlines[0] <= 0 {
		t.Fatal("the pre-flight ran with NO deadline. A hung pull then holds the per-workspace " +
			"restart/provision gate for the full client timeout, freezing every restart, heal and " +
			"provision path for that workspace (core#5025 finding 7).")
	}
	if stub.deadlines[0] > restartPrewarmBudget {
		t.Fatalf("pre-flight deadline %s exceeds the declared budget %s", stub.deadlines[0], restartPrewarmBudget)
	}
}

// TestRestartPrewarmBudget_IsWellUnderTheClientTimeout tests the DEFAULT, not the
// parameter. Every test above injects its own stub; a budget constant that had
// drifted back up to the 20-minute client timeout would leave them all green
// while production kept the gate for twenty minutes.
func TestRestartPrewarmBudget_IsWellUnderTheClientTimeout(t *testing.T) {
	if restartPrewarmBudget <= 0 {
		t.Fatal("restartPrewarmBudget must be positive — zero or negative means an already-expired context")
	}
	if restartPrewarmBudget >= provisioner.EnsureImageClientTimeout() {
		t.Fatalf("restartPrewarmBudget (%s) does not bound anything: the HTTP client already gives up at %s, "+
			"so the gate is still held for the full client timeout",
			restartPrewarmBudget, provisioner.EnsureImageClientTimeout())
	}
	// The lower bound, and the one that is easy to get wrong in the direction
	// that LOOKS safe. A budget too small to contain a real cold pull declines
	// every restart while the pull is still legitimately in progress, so a
	// workspace can never adopt a newly promoted pin — the guard would have
	// converted "slow adoption" into "no adoption", which is worse than the
	// outage it exists to prevent. 10 minutes is the measured 7.05GB cold-pull
	// design point from core#5019.
	if restartPrewarmBudget < 10*time.Minute {
		t.Fatalf("restartPrewarmBudget (%s) is too short for the cold multi-GB pull this guard exists to "+
			"WAIT for; a newly promoted pin would be refused on every restart forever", restartPrewarmBudget)
	}
}

// TestRestartWorkspaceAutoOpts_PrewarmDoesNotHoldTheGate is finding 7's second
// half, and the one that matters operationally.
//
// While the pre-flight is in flight, every OTHER lifecycle path for that
// workspace must still be able to run. A pull is allowed to be slow. It is not
// allowed to be indistinguishable from a workspace whose lifecycle has stopped.
func TestRestartWorkspaceAutoOpts_PrewarmDoesNotHoldTheGate(t *testing.T) {
	block := make(chan struct{})
	stub := &flakyCPProv{failUntil: 99, err: errors.New("still pulling"), block: block}
	h := &WorkspaceHandler{cpProv: stub}

	done := make(chan bool, 1)
	go func() {
		done <- h.RestartWorkspaceAutoOpts(context.Background(), "ws-slow-pull", "", nil, hermesPayload(), false)
	}()

	// Wait until the pre-flight is actually in flight, then prove the gate is
	// still acquirable by another lifecycle path.
	gate := acquireRestartProvisionGate("ws-slow-pull")
	acquired := make(chan struct{})
	go func() {
		gate.Lock()
		close(acquired)
		gate.Unlock()
	}()

	select {
	case <-acquired:
	case <-time.After(3 * time.Second):
		close(block)
		<-done
		t.Fatal("the per-workspace restart/provision gate was held for the whole pre-flight. A slow or " +
			"hung pull then freezes every restart, heal and provision path for that workspace " +
			"(core#5025 finding 7).")
	}

	close(block)
	if <-done {
		t.Error("the pre-flight never succeeded, so the restart must report that nothing was dispatched")
	}
	h.waitAsyncForTest()
}

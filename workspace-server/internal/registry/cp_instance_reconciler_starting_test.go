package registry

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeStartingChecker is fakeRunningChecker plus InstanceStarting: whether the
// control plane reports the instance as still coming up.
type fakeStartingChecker struct {
	fakeRunningChecker
	starting  map[string]bool
	startErrs map[string]error
	asked     []string
}

func (f *fakeStartingChecker) InstanceStarting(_ context.Context, wsID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, wsID)
	if err, ok := f.startErrs[wsID]; ok {
		return false, err
	}
	return f.starting[wsID], nil
}

// startingClock pins the reconciler's clock and its starting records for one
// test; advance moves the clock on.
func startingClock(t *testing.T) (advance func(time.Duration)) {
	t.Helper()
	now := time.Date(2026, 11, 18, 17, 38, 26, 0, time.UTC)
	prev := reconcilerNow
	reconcilerNow = func() time.Time { return now }
	startingMu.Lock()
	startingSince = map[string]time.Time{}
	startingMu.Unlock()
	t.Cleanup(func() {
		reconcilerNow = prev
		startingMu.Lock()
		startingSince = map[string]time.Time{}
		startingMu.Unlock()
	})
	return func(d time.Duration) { now = now.Add(d) }
}

// A Kubernetes pod registers (the row reads online) before it is Ready, and
// the control plane reports "provisioning" until then: IsRunning's (false,
// nil) in that window is a pod booting. The reconciler leaves it (2026-11-18:
// it restarted a fresh agent 7 s after its registration).
func TestReconcileOnce_InstanceStillStarting_IsLeftAlone(t *testing.T) {
	startingClock(t)
	mock := setupTestDB(t)
	checker := &fakeStartingChecker{
		fakeRunningChecker: fakeRunningChecker{running: map[string]bool{"ws-booting": false}},
		starting:           map[string]bool{"ws-booting": true},
	}
	off := &recordingOffline{}
	expectReconcileQuery(mock, reconcileRows([2]string{"ws-booting", "enteros-ws-booting"}))

	reconcileOnce(context.Background(), checker, off.handler())

	if got := off.got(); len(got) != 0 {
		t.Fatalf("a starting instance was flipped offline: %v", got)
	}
	if len(checker.asked) != 1 {
		t.Fatalf("InstanceStarting asked %v times, want once", checker.asked)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations (no re-confirm read for a starting instance): %v", err)
	}
}

// A pod that never becomes Ready is still reprovisioned: once it has been
// starting for cpInstanceStartingGrace, the reconciler flips it as before.
func TestReconcileOnce_InstanceStartingPastTheGrace_FlipsOffline(t *testing.T) {
	advance := startingClock(t)
	mock := setupTestDB(t)
	checker := &fakeStartingChecker{
		fakeRunningChecker: fakeRunningChecker{running: map[string]bool{"ws-stuck": false}},
		starting:           map[string]bool{"ws-stuck": true},
	}
	off := &recordingOffline{}

	expectReconcileQuery(mock, reconcileRows([2]string{"ws-stuck", "enteros-ws-stuck"}))
	reconcileOnce(context.Background(), checker, off.handler())
	advance(cpInstanceStartingGrace - time.Second)
	expectReconcileQuery(mock, reconcileRows([2]string{"ws-stuck", "enteros-ws-stuck"}))
	reconcileOnce(context.Background(), checker, off.handler())
	if got := off.got(); len(got) != 0 {
		t.Fatalf("flipped inside the grace: %v", got)
	}

	advance(2 * time.Second)
	expectReconcileQuery(mock, reconcileRows([2]string{"ws-stuck", "enteros-ws-stuck"}))
	expectReconfirm(mock, "ws-stuck", "online", "enteros-ws-stuck")
	reconcileOnce(context.Background(), checker, off.handler())
	if got := off.got(); len(got) != 1 || got[0] != "ws-stuck" {
		t.Fatalf("after the grace: onOffline calls %v, want ws-stuck", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// The grace counts one continuous start: an instance seen running again (or
// gone from the candidates) starts a new one.
func TestReconcileOnce_StartingRecordIsDroppedOnceRunning(t *testing.T) {
	advance := startingClock(t)
	mock := setupTestDB(t)
	checker := &fakeStartingChecker{
		fakeRunningChecker: fakeRunningChecker{running: map[string]bool{"ws-a": false}},
		starting:           map[string]bool{"ws-a": true},
	}
	off := &recordingOffline{}

	expectReconcileQuery(mock, reconcileRows([2]string{"ws-a", "enteros-ws-a"}))
	reconcileOnce(context.Background(), checker, off.handler())
	advance(4 * time.Minute)
	checker.running["ws-a"] = true
	expectReconcileQuery(mock, reconcileRows([2]string{"ws-a", "enteros-ws-a"}))
	reconcileOnce(context.Background(), checker, off.handler())
	advance(4 * time.Minute)
	checker.running["ws-a"] = false // a restart: starting again, a new grace
	expectReconcileQuery(mock, reconcileRows([2]string{"ws-a", "enteros-ws-a"}))
	reconcileOnce(context.Background(), checker, off.handler())

	if got := off.got(); len(got) != 0 {
		t.Fatalf("a new start was flipped on the old start's clock: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// Asking fails: fail-safe, the row stays online (like IsRunning's errors).
// An instance that is not starting (stopped, terminated) is flipped as before.
func TestReconcileOnce_StartingErrorIsFailSafe_NotStartingStillFlips(t *testing.T) {
	startingClock(t)
	mock := setupTestDB(t)
	checker := &fakeStartingChecker{
		fakeRunningChecker: fakeRunningChecker{running: map[string]bool{"ws-err": false, "ws-dead": false}},
		starting:           map[string]bool{"ws-dead": false},
		startErrs:          map[string]error{"ws-err": errors.New("cp provisioner: status: unexpected 502")},
	}
	off := &recordingOffline{}
	expectReconcileQuery(mock, reconcileRows([2]string{"ws-err", "i-err"}, [2]string{"ws-dead", "i-dead"}))
	expectReconfirm(mock, "ws-dead", "online", "i-dead")

	reconcileOnce(context.Background(), checker, off.handler())

	if got := off.got(); len(got) != 1 || got[0] != "ws-dead" {
		t.Fatalf("onOffline calls %v, want only ws-dead", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

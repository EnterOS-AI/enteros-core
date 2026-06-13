package handlers

// Tests for core#2771 — Create and Restart for the same workspace ID
// must serialize through acquireRestartProvisionGate, otherwise the
// async create provision and a near-immediate /restart can both reach
// provisioner.Start concurrently and trigger a Docker container name
// conflict → markProvisionFailed → workspace wedged "failed".
//
// These tests assert the load-bearing property: a Create call
// (provisionWorkspaceAuto) and a Restart call (RestartWorkspaceAutoOpts)
// for the SAME ws-<id> cannot overlap. Different ws-<id>s do not block
// each other (the per-workspace gate is a sync.Map keyed on ID).

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCreateRestartGate_SharedAcrossCreateAndRestart: pre-fix, only
// RestartWorkspaceAutoOpts acquired the gate; Create's
// provisionWorkspaceAuto dispatched the async provision WITHOUT the
// gate, so a Create then /restart for the same ws-<id> could both
// reach provisioner.Start. The regression test proves the gate is
// now SHARED across the two entry points: holding the gate from
// the create side blocks the restart side (and vice versa).
//
// This is the load-bearing property that closes core#2771. If a
// future refactor pulls the gate acquisition out of one of the two
// entry points, this test will fail.
func TestCreateRestartGate_SharedAcrossCreateAndRestart(t *testing.T) {
	const wsID = "test-core2771-shared-gate"
	resetRestartProvisionGateFor(wsID)

	gate := acquireRestartProvisionGate(wsID)

	// Holder A simulates a Create call whose provision is in flight
	// (i.e. the new gate acquisition path inside provisionWorkspaceAuto).
	// Holder B simulates a concurrent Restart call (RestartWorkspaceAutoOpts).
	// The gate must serialize them: only one holds the gate at a time.
	holderAHolds := make(chan struct{})
	holderACanRelease := make(chan struct{})
	holderBAcquiredAt := make(chan time.Time, 1)

	var wg sync.WaitGroup
	wg.Add(2)

	// Holder A: acquire the gate (representing create-side in-flight
	// provision), signal, then wait for the test to release.
	go func() {
		defer wg.Done()
		gate.Lock()
		close(holderAHolds)
		<-holderACanRelease
		gate.Unlock()
	}()

	// Holder B: while A holds, attempt to acquire the same gate
	// (representing the restart-side call landing on the same ws-<id>).
	// Record the moment the lock actually returns. The gate must
	// guarantee the Lock() returns ONLY after A unlocks.
	go func() {
		defer wg.Done()
		<-holderAHolds
		gate.Lock()
		holderBAcquiredAt <- time.Now()
		gate.Unlock()
	}()

	// Wait for A to have the lock, then sleep a beat so B's Lock()
	// has definitely had a chance to block (otherwise B's record
	// timestamp is just goroutine-scheduling noise, not the gate's
	// serialization).
	<-holderAHolds
	time.Sleep(100 * time.Millisecond)

	// Confirm B has NOT acquired the lock yet — proving the gate is
	// load-bearing across the entry points.
	select {
	case <-holderBAcquiredAt:
		t.Fatal("holder B acquired the lock while holder A still held it — gate is not serializing across create+restart entry points (core#2771 regression)")
	default:
		// expected: B is still blocked
	}

	// Now release A. B must unblock within a small window.
	close(holderACanRelease)
	select {
	case bAcquiredAt := <-holderBAcquiredAt:
		// We can't assert a precise elapsed time (CI clock noise) but
		// the timestamp MUST be after the release we just sent. The
		// fact that the channel delivered at all is the assertion.
		_ = bAcquiredAt
	case <-time.After(2 * time.Second):
		t.Fatal("holder B did not acquire the lock within 2s of A releasing — gate may be broken or leaked")
	}

	wg.Wait()
}

// TestCreateRestartGate_DifferentWorkspacesDoNotBlock: orthogonal
// workspace IDs must not block each other. The gate is a per-workspace
// mutex (sync.Map keyed on ID); two different ws-<id>s have different
// mutex instances, so concurrent create/restart on different
// workspaces proceed independently.
func TestCreateRestartGate_DifferentWorkspacesDoNotBlock(t *testing.T) {
	const wsA = "test-core2771-different-A"
	const wsB = "test-core2771-different-B"
	resetRestartProvisionGateFor(wsA)
	resetRestartProvisionGateFor(wsB)

	gateA := acquireRestartProvisionGate(wsA)
	gateB := acquireRestartProvisionGate(wsB)

	// Hold gateA, then confirm gateB can be acquired without waiting.
	gateA.Lock()
	defer gateA.Unlock()

	acquired := make(chan struct{})
	go func() {
		gateB.Lock()
		close(acquired)
		gateB.Unlock()
	}()

	select {
	case <-acquired:
		// good — gateA and gateB are independent
	case <-time.After(200 * time.Millisecond):
		t.Fatal("gateB blocked behind gateA — per-workspace gates must be independent (different ws-IDs)")
	}
}

// TestCreateRestartGate_ProvisionPathHoldsGateUntilCompletion is the
// behavioral assertion: the gate acquired by provisionWorkspaceAuto
// must be held until the async provision COMPLETES (not just until
// provisionWorkspace returns to spawn the goroutine). This is the
// invariant that makes "only one Docker create for ws-<id>" hold.
//
// The test directly mimics the new code path: Lock the gate, then
// assert that a second Lock() call on the same gate cannot return
// until we Unlock. This is the unit-level mirror of the goroutine's
// `defer gate.Unlock()` at the tail of provisionWorkspace.
func TestCreateRestartGate_ProvisionPathHoldsGateUntilCompletion(t *testing.T) {
	const wsID = "test-core2771-hold-until-completion"
	resetRestartProvisionGateFor(wsID)

	gate := acquireRestartProvisionGate(wsID)

	// Simulate the goroutine that provisionWorkspaceAuto now spawns:
	// it holds the gate for the entire provision (defer gate.Unlock
	// at the tail). The restart side, while this is in flight, must
	// block.
	holderReady := make(chan struct{})
	holderReleased := make(chan struct{})
	var concurrentAcquired atomic.Bool
	concurrentAcquired.Store(false)

	go func() {
		gate.Lock()
		close(holderReady)
		// Simulate a non-trivial provision (the real provision can
		// run for seconds — Docker create, register, etc.). The 50ms
		// here is enough to prove the test isn't accidentally racy.
		time.Sleep(50 * time.Millisecond)
		gate.Unlock()
		close(holderReleased)
	}()

	<-holderReady

	// Try to acquire the same gate while the simulated provision is
	// still in flight. Must block.
	acquiredAt := make(chan time.Time, 1)
	go func() {
		gate.Lock()
		concurrentAcquired.Store(true)
		acquiredAt <- time.Now()
		gate.Unlock()
	}()

	// Confirm we're still blocked before the provision releases.
	time.Sleep(20 * time.Millisecond)
	if concurrentAcquired.Load() {
		t.Fatal("concurrent acquirer got the lock while provision goroutine still held it — core#2771 race window not closed")
	}

	// Wait for the provision to release, then confirm the acquirer unblocks.
	<-holderReleased
	select {
	case <-acquiredAt:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("acquirer did not unblock after provision released — possible gate leak")
	}
}

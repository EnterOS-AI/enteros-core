package handlers

// provision_failure_delivery_test.go — markProvisionFailed must DELIVER.
//
// Computing the right reason is worth nothing if the write that carries it is
// dropped. Review of #5093 found the first version of the recovery closed only
// the case where the caller's context was already dead ON ENTRY, leaving three
// ways for the verdict to be lost anyway:
//
//	1. check-then-USE — live at the entry check, dead by the time of the write
//	2. one shared budget — a slow broadcast starves the durable write entirely
//	3. the broadcast's own error was discarded, so the leg the docstring leaned
//	   on was the one leg whose failure nobody could see
//
// (1) and (2) together meant the fix NARROWED the window rather than closing it.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// slowBroadcaster blocks inside RecordAndBroadcast — the one leg of
// markProvisionFailed that can legitimately take real time (it records an event
// row and fans out to subscribers). It also records whether the context IT was
// handed was usable, so a "fix" that rescues the write by starving the
// notification cannot be mistaken for a fix.
type slowBroadcaster struct {
	mu sync.Mutex

	// waitFor, when non-nil, is waited on (bounded by maxBlock) before
	// returning — this is what lets the CALLER's context expire while this leg
	// is still in flight.
	waitFor  context.Context
	maxBlock time.Duration
	err      error

	called       bool
	ctxErrAtCall error
	ctxHadBudget bool
}

func (s *slowBroadcaster) BroadcastOnly(_ string, _ string, _ interface{}) {}

func (s *slowBroadcaster) RecordAndBroadcast(ctx context.Context, _, _ string, _ interface{}) error {
	if s.waitFor != nil {
		timer := time.NewTimer(s.maxBlock)
		select {
		case <-s.waitFor.Done():
		case <-timer.C:
		}
		timer.Stop()
	}
	s.mu.Lock()
	s.called = true
	s.ctxErrAtCall = ctx.Err()
	if dl, ok := ctx.Deadline(); ok {
		s.ctxHadBudget = time.Until(dl) > 0
	}
	s.mu.Unlock()
	return s.err
}

func (s *slowBroadcaster) observed() (called bool, ctxErr error, hadBudget bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.called, s.ctxErrAtCall, s.ctxHadBudget
}

// TestMarkProvisionFailed_BothLegsSurviveACallerThatDiesMidCall is the residual
// half of the ceiling gap: the ORIGINAL defect in a narrower window.
//
// Deciding ONCE, up front, whether the caller's context is usable is a
// check-then-USE race. The context is live at the check, is therefore passed
// straight through, and then dies while the call is still in flight. By the time
// the work runs it is exactly as dead as the one the entry check was written to
// catch, and database/sql drops the statement just the same — the reason computed
// correctly and lost anyway.
//
// The caller here is ALIVE on entry. That is the whole point: a context already
// expired at entry does not exercise this at all, which is why the first version
// of these tests could not see it.
//
// IT MUST DISCRIMINATE BOTH LEGS, and doing so takes deliberate construction.
// Once the durable write was reordered ahead of the broadcast, a version of this
// test that only blocked inside the broadcast stopped discriminating leg 1
// entirely: the write now runs first, so nothing has had time to elapse and it
// completes even on a caller context. The test kept its name and quietly stopped
// pinning the race it is named for. So each leg is made to outlive the caller on
// its own:
//
//	leg 1 (the UPDATE)    — WillDelayFor outlasts the caller's deadline, and
//	                        sqlmock's ExecContext honours ctx DURING that delay
//	leg 2 (the broadcast) — blocks until the caller's context is actually Done
//
// Sabotaging EITHER leg to use the caller's context turns this red — verified by
// doing exactly that, one leg at a time.
//
// Leg 1 is asserted through the LOG, not through ExpectationsWereMet, and the
// difference is not cosmetic. sqlmock matches and FULFILS an expectation inside
// c.exec, before it races the delay against ctx.Done — so a cancelled exec still
// leaves every expectation met and still runs the argument matchers. Asserting
// on expectations alone therefore proves the statement reached the driver, NOT
// that it succeeded, and a sabotaged leg 1 sails straight through it. The
// absence of the "db update FAILED" line is the outcome signal.
//
// It still does not replace TestMarkProvisionFailed_DeliversThroughAnExpiredContext
// or TestCeilingReasonReachesTheColumn: those cover a caller already dead ON
// ENTRY, which is a different input, and no amount of mid-call coverage implies
// it. Do not delete them on the strength of this one.
func TestMarkProvisionFailed_BothLegsSurviveACallerThatDiesMidCall(t *testing.T) {
	// The caller's remaining budget. Every other duration here is a multiple of
	// it, so the test's outcome never depends on machine speed in the dangerous
	// direction — a slower machine only makes the caller MORE certainly dead.
	const callerBudget = 60 * time.Millisecond
	// 5x the caller budget: under sabotage the caller is long dead before the
	// write's delay elapses; with the fix, leg 1's own reserve covers it easily.
	const writeDelay = 5 * callerBudget

	mock := setupTestDB(t)
	var got string
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(sqlmock.AnyArg(), captureLastSampleError{got: &got}, sqlmock.AnyArg()).
		WillDelayFor(writeDelay).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ctx, cancel := context.WithTimeout(context.Background(), callerBudget)
	defer cancel()
	if ctx.Err() != nil {
		t.Fatal("precondition: the caller context must be ALIVE at entry")
	}

	readLog := captureProvLog(t)
	bc := &slowBroadcaster{waitFor: ctx, maxBlock: 2 * time.Second}
	h := NewWorkspaceHandler(bc, nil, "http://localhost:8080", t.TempDir())
	h.markProvisionFailed(ctx, "ws-midcall-1", provisionFailedBudgetExhausted, nil)

	// Leg 1 — the durable write outlived the caller. The LOG is the outcome
	// signal; see the note above on why ExpectationsWereMet cannot be.
	if logged := readLog(); strings.Contains(logged, "db update FAILED") {
		t.Errorf("the WRITE was lost to a caller that died mid-call; log was:\n%s", logged)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the write statement never reached the driver: %v", err)
	}
	if got != provisionFailedBudgetExhausted {
		t.Errorf("last_sample_error = %q, want %q", got, provisionFailedBudgetExhausted)
	}
	// Leg 2 — so did the notification.
	called, ctxErr, hadBudget := bc.observed()
	if !called {
		t.Error("the BROADCAST leg never ran")
	}
	if ctxErr != nil {
		t.Errorf("the BROADCAST leg ran on a context already dead (%v) — it could not have reached the canvas", ctxErr)
	}
	if !hadBudget {
		t.Error("the BROADCAST leg had no budget left")
	}
}

// TestMarkProvisionFailed_NeitherLegStarvesTheOther is the paired negative
// control for the reserve, and it exists because the obvious fix for the test
// above — do the write first and let the notification take its chances — trades
// one silent loss for another.
//
// A write that lands while the canvas is never told is better than the reverse;
// both legs failing quietly is worse than either. So the split has to be shown
// to protect BOTH: the durable write runs, AND the broadcast still receives a
// live context with budget left on it.
func TestMarkProvisionFailed_NeitherLegStarvesTheOther(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	bc := &slowBroadcaster{waitFor: ctx, maxBlock: 2 * time.Second}
	h := NewWorkspaceHandler(bc, nil, "http://localhost:8080", t.TempDir())
	h.markProvisionFailed(ctx, "ws-midcall-2", "both legs must run", nil)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the durable write did not run: %v", err)
	}
	called, ctxErr, hadBudget := bc.observed()
	if !called {
		t.Error("the broadcast leg never ran — the write was rescued by starving the notification")
	}
	if ctxErr != nil {
		t.Errorf("the broadcast leg ran on an already-dead context (%v) — it could not have reached the canvas", ctxErr)
	}
	if !hadBudget {
		t.Error("the broadcast leg had no budget left — it is bounded, but it must also be USABLE")
	}
}

// TestMarkProvisionFailed_LogsADiscardedBroadcastFailure.
//
// RecordAndBroadcast returns an error and the call site threw it away — so the
// ONE leg whose failure was invisible was the leg the docstring leaned on
// ("the broadcast already fired, the operator sees the failure event in the
// canvas"). That is precisely the shape this PR exists to remove: a claim
// resting on an outcome nobody observes. Logging it is the difference between
// "the canvas was told" and "we tried to tell the canvas".
func TestMarkProvisionFailed_LogsADiscardedBroadcastFailure(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	readLog := captureProvLog(t)
	bc := &slowBroadcaster{err: errors.New("subscriber fan-out refused: EVENTBUS-DOWN")}
	h := NewWorkspaceHandler(bc, nil, "http://localhost:8080", t.TempDir())
	h.markProvisionFailed(context.Background(), "ws-bcfail-1", "a reason worth announcing", nil)

	out := readLog()
	if !strings.Contains(out, "EVENTBUS-DOWN") {
		t.Errorf("a failed provision-failure broadcast was silent; log was:\n%s", out)
	}
	if !strings.Contains(out, "ws-bcfail-1") {
		t.Errorf("the broadcast-failure log does not name the workspace; log was:\n%s", out)
	}
}

// TestProvisionFailureLegContext_IsAlwaysDetachedAndBounded replaces the earlier
// "a live caller passes through unchanged" control, which encoded the WRONG
// invariant: pass-through IS the check-then-use race above, because a context
// that is live at the check can die before the write.
//
// The invariant that actually holds: every leg runs on a context the caller
// cannot cancel, and every leg is bounded. Immunity without a bound would be an
// unbounded write on a shutting-down process; a bound without immunity is the
// original bug. Both are pinned, for a live caller AND a dead one, so neither
// half can be dropped silently.
func TestProvisionFailureLegContext_IsAlwaysDetachedAndBounded(t *testing.T) {
	type key struct{}
	budget := 250 * time.Millisecond

	for _, tc := range []struct {
		name string
		mk   func() (context.Context, context.CancelFunc)
	}{
		{"live caller", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.WithValue(context.Background(), key{}, "carried"), time.Hour)
		}},
		{"already-expired caller", func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.WithValue(context.Background(), key{}, "carried"), time.Now().Add(-time.Second))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller, cancelCaller := tc.mk()
			leg, cancelLeg := provisionFailureLegContext(caller, budget)
			defer cancelLeg()

			if leg.Err() != nil {
				t.Fatalf("leg context is dead on arrival (%v)", leg.Err())
			}
			// The caller dying must NOT reach the leg. This is the race.
			cancelCaller()
			if leg.Err() != nil {
				t.Errorf("cancelling the caller killed the leg context (%v) — the verdict can still be lost mid-call", leg.Err())
			}
			dl, ok := leg.Deadline()
			if !ok {
				t.Error("leg context has NO deadline — immunity must not mean running unbounded")
			} else if remaining := time.Until(dl); remaining <= 0 || remaining > budget {
				t.Errorf("leg budget = %s, want (0, %s]", remaining, budget)
			}
			if leg.Value(key{}) != "carried" {
				t.Error("request-scoped values were dropped — context.WithoutCancel exists to keep them")
			}
		})
	}
}

// TestProvisionFailureReserveLeavesBothLegsUsable pins the split itself.
//
// This PR's own cpCreatePrewarmBudget guards exactly this hazard one file over —
// two operations in series on one budget, where the first can consume all of it
// — and the same reasoning applies to the two legs here.
func TestProvisionFailureReserveLeavesBothLegsUsable(t *testing.T) {
	if provisionFailureRecordReserve <= 0 {
		t.Fatalf("record reserve = %s — the durable write is guaranteed nothing", provisionFailureRecordReserve)
	}
	if provisionFailureRecordReserve >= provisionFailureWriteBudget {
		t.Errorf("record reserve %s consumes the whole %s budget — the broadcast leg is starved by construction",
			provisionFailureRecordReserve, provisionFailureWriteBudget)
	}
}

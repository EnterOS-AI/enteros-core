package handlers

// delegation_sweeper_reply_test.go — the sweeper OWES the caller a reply.
//
// Before this, the sweeper was the component that detects a target agent has
// died, and it told nobody: it flipped the row to failed/stuck and returned.
// The caller's agent never learned its delegation was dead, and because the
// mail digest counted only non-terminal statuses, the delegation ALSO vanished
// from its "awaiting reply" count. The single case an operator most needs to
// see was the one the platform made invisible (#4314).
//
// These tests pin the reply. They are deliberately written to FAIL if the reply
// write is dropped — the pre-existing sweeper tests could not have caught it,
// because sqlmock's ExpectationsWereMet() only asserts that EXPECTED calls
// fired, never that no EXTRA ones did, and the reply write is best-effort (it
// logs and continues on error). A green suite that cannot see the write is
// exactly the green-wash this whole change set exists to undo.

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// sweeperReplyMock wires an ordered sqlmock through the full terminalize path:
// the in-flight SELECT, the ledger's guard+UPDATE, then the reply rows.
func sweeperReplyMock(t *testing.T, delegationID string, lastBeat, deadline time.Time, fromStatus string) (sqlmock.Sqlmock, *DelegationSweeper) {
	t.Helper()
	mock := setupTestDB(t)
	// The reply rides the ledger flag (see emitTerminalDelegationReply): while the
	// ledger is dark there must be NO caller notification at all.
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	mock.ExpectQuery(`SELECT d\.delegation_id, d\.caller_id, d\.callee_id`).
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "caller_id", "callee_id", "status", "beat", "deadline"}).
			AddRow(delegationID, "caller-ws", "callee-ws", "in_progress", lastBeat, deadline))
	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs(delegationID).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(fromStatus))
	mock.ExpectExec(`UPDATE delegations`).WillReturnResult(sqlmock.NewResult(0, 1))
	// newTestSweeper bypasses the boot grace: production suppresses the stuck arm
	// for one threshold after start (our own downtime makes every callee look stale
	// at once), which TestSweeper_BootGrace pins directly.
	return mock, newTestSweeper(NewDelegationLedger(nil))
}

// NOTE ON `fromStatus`: these tests drive from `queued`, the status every
// delegation is actually BORN in (Insert hard-codes it). An earlier revision
// pinned them to `in_progress` — a status with ZERO writers in the entire
// codebase. Testing a state production never reaches is the same fabrication as
// the sqlmock-invented heartbeat this PR is undoing; it just fails to fail.
//
// expectLedgerReply pins the caller-side ledger reply row: it must be
// correlatable (response_body.delegation_id) and addressed to the callee.
//
// rowStatus is the ACTIVITY_LOGS status, which is a different vocabulary from
// the delegation state machine's — 'stuck' has no activity_logs spelling and is
// NOT written at all: `stuck` is a recoverable WARNING and emits NO reply (see the
// sweeper's stuck arm). An earlier revision mapped stuck->failed for this row, which
// was a symptom of treating a wedged target as a dead one. Passing the delegation
// status straight through here is the mistake this signature exists to prevent.
func expectLedgerReply(mock sqlmock.Sqlmock, rowStatus string) {
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO activity_logs`)).
		WithArgs("caller-ws", "caller-ws", "callee-ws", sqlmock.AnyArg(), rowStatus, sqlmock.AnyArg(),
			`{"delegation_id":"deleg-x"}`).
		WillReturnResult(sqlmock.NewResult(1, 1))
}

func TestSweeperReply_DeadlineFailure_NotifiesCaller(t *testing.T) {
	past := time.Now().Add(-1 * time.Minute)
	mock, sw := sweeperReplyMock(t, "deleg-x", time.Now(), past, "queued")
	expectLedgerReply(mock, "failed")

	res := sw.Sweep(context.Background())

	if res.DeadlineFailures != 1 {
		t.Fatalf("DeadlineFailures = %d, want 1", res.DeadlineFailures)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("deadline terminalize did not emit the caller's reply: %v", err)
	}
}

func TestSweeperReply_Stuck_MarksButNEVERNotifies(t *testing.T) {
	// A wedged target is NOT a dead delegation, and this is the correction to the
	// first cut of this PR, which treated it as one.
	//
	// a2a_proxy enqueues to a2a_queue precisely when the target is
	// settling/restarting — i.e. NOT heartbeating — and queue rows are infinite-TTL
	// by default. The platform's own design is to HOLD the message across an
	// arbitrarily long target outage and deliver it on the target's next heartbeat.
	// So a 10-minute heartbeat gap means "the target may have a problem", NOT "this
	// delegation is dead".
	//
	// Sending "Delegation failed" here would be a lie that the real answer then
	// contradicts — and worse, the terminal row would make the drain's
	// recordLedgerStatus("completed") bounce off ErrInvalidTransition, so the ledger
	// would permanently read `stuck` for a delegation that SUCCEEDED.
	//
	// stuck therefore: marks the row (the digest renders the ⚠ warning from it),
	// emits NO reply, and stays recoverable. Only the 6h deadline notifies.
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1") // even with the inbox wide open
	stale := time.Now().Add(-30 * time.Minute)
	future := time.Now().Add(2 * time.Hour)
	mock, sw := sweeperReplyMock(t, "deleg-x", stale, future, "queued")
	// deliberately NO reply expectations — neither ledger row nor inbox row.

	res := sw.Sweep(context.Background())

	if res.StuckMarked != 1 {
		t.Fatalf("StuckMarked = %d, want 1 — the row must still be marked", res.StuckMarked)
	}
	// If a reply is ever re-introduced here it hits an unexpected sqlmock call,
	// which surfaces as a write error and is COUNTED — sqlmock itself will not
	// complain about the extra INSERT, so the counter is the only witness.
	if res.ReplyErrors != 0 {
		t.Fatalf("stuck must attempt NO reply at all, got ReplyErrors=%d — a wedged "+
			"target may still deliver via a2a_queue; a death notice here is a lie", res.ReplyErrors)
	}
	if res.Errors != 0 {
		t.Fatalf("Errors = %d, want 0 — unexpected SQL fired on the stuck path", res.Errors)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("stuck must NOT notify the caller — a wedged target may still deliver: %v", err)
	}
}

func TestSweeperReply_InboxPushRidesTheFlag(t *testing.T) {
	// The INBOX row (a2a_receive) is still gated while the reply channel is
	// dark. The LEDGER row is not — it must fire either way, so the caller-side
	// record exists from day one and the Phase-3 flag flip only adds the inbox
	// delivery on top.
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")
	past := time.Now().Add(-1 * time.Minute)
	mock, sw := sweeperReplyMock(t, "deleg-x", time.Now(), past, "queued")
	expectLedgerReply(mock, "failed")
	// ...and now, additionally, the inbox row.
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO activity_logs`)).
		WithArgs("caller-ws", "caller-ws", "callee-ws", "Delegation failed",
			`{"delegation_id":"deleg-x"}`, sqlmock.AnyArg(), "error", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	sw.Sweep(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("flag on: a deadline failure must ALSO deliver an inbox reply: %v", err)
	}
}

// TestSweeperReply_StuckIsReachableFromEveryPreTerminalState pins that a wedged
// TARGET brings its delegation down from ANY pre-terminal state.
//
// The heartbeat here is the TARGET WORKSPACE's (workspaces.last_heartbeat_at,
// written on every registry heartbeat) — NOT delegations.last_heartbeat, whose
// only writer (DelegationLedger.Heartbeat) has zero production call sites and is
// therefore always NULL. An earlier revision of this test fabricated that dead
// column via sqlmock and called itself a regression pin: a green test for a
// scenario that cannot occur. It is now driven by the signal that actually
// exists.
//
// A target can go silent while its delegation is still `queued` or `dispatched`,
// so restricting `stuck` to in_progress hands the sweeper an
// ErrInvalidTransition and leaves the row un-terminalized, retried every sweep.
func TestSweeperReply_StuckIsReachableFromEveryPreTerminalState(t *testing.T) {
	for _, from := range []string{"queued", "dispatched", "in_progress"} {
		t.Run(from, func(t *testing.T) {
			stale := time.Now().Add(-30 * time.Minute)
			future := time.Now().Add(2 * time.Hour)
			mock, sw := sweeperReplyMock(t, "deleg-x", stale, future, from)
			// no reply: stuck is a warning, not a death.

			res := sw.Sweep(context.Background())

			if res.Errors != 0 {
				t.Fatalf("from %s: sweeper errored (ErrInvalidTransition error-loop): %+v", from, res)
			}
			if res.StuckMarked != 1 {
				t.Fatalf("from %s: StuckMarked = %d, want 1 — a stale-heartbeat row must terminalize from ANY pre-terminal state", from, res.StuckMarked)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("from %s: %v", from, err)
			}
		})
	}
}

// TestSweeperReply_FlagOff_NotifiesNobody is the claim the whole PR rests on.
//
// The sweeper is started UNCONDITIONALLY — it is NOT gated on
// DELEGATION_LEDGER_WRITE. "It's a no-op because the table is empty" is an
// assumption about every environment's history, not a property of the code: a
// row left behind by any period when the flag WAS on is now long past its 6h
// deadline, and the first sweep after deploy would fire a months-late
// "Delegation failed" message into a live agent's inbox.
//
// So the notification rides the same flag as the ledger it reports on, and this
// test pins it. Note sqlmock cannot fail on an UNEXPECTED call — so the pin is
// that NO reply expectation is registered and the sweep still terminalizes
// cleanly with zero ReplyErrors; a reply write would hit an unexpected-call
// error and be counted.
func TestSweeperReply_FlagOff_NotifiesNobody(t *testing.T) {
	t.Setenv("DELEGATION_LEDGER_WRITE", "")
	past := time.Now().Add(-1 * time.Minute)

	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT d\.delegation_id, d\.caller_id, d\.callee_id`).
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "caller_id", "callee_id", "status", "beat", "deadline"}).
			AddRow("deleg-x", "caller-ws", "callee-ws", "in_progress", time.Now(), past))
	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-x").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("queued"))
	mock.ExpectExec(`UPDATE delegations`).WillReturnResult(sqlmock.NewResult(0, 1))
	// deliberately NO reply expectation.

	sw := NewDelegationSweeper(nil, NewDelegationLedger(nil))
	res := sw.Sweep(context.Background())

	if res.DeadlineFailures != 1 {
		t.Fatalf("must still terminalize: %+v", res)
	}
	if res.ReplyErrors != 0 {
		t.Fatalf("flag off must emit NO reply, but a reply write was attempted: %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected SQL with the ledger dark: %v", err)
	}
}

// TestSweeperReply_LedgerInsertFailure_StillPushesInboxAndCounts pins the
// degraded path. The reply is best-effort — it logs and continues — which is
// precisely the shape that let the original silent-death bug hide. Two claims:
// a failed ledger write is COUNTED (ReplyErrors, so it is visible in the sweep
// log rather than swallowed), and it does NOT deny the agent its inbox
// notification, because a missing dashboard row is not a reason to also drop the
// message telling the caller its delegation died.
func TestSweeperReply_LedgerInsertFailure_StillPushesInboxAndCounts(t *testing.T) {
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")
	past := time.Now().Add(-1 * time.Minute)
	mock, sw := sweeperReplyMock(t, "deleg-x", time.Now(), past, "queued")

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO activity_logs`)).
		WillReturnError(errors.New("deadlock detected"))
	// the inbox push must STILL be attempted
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO activity_logs`)).
		WithArgs("caller-ws", "caller-ws", "callee-ws", "Delegation failed",
			`{"delegation_id":"deleg-x"}`, sqlmock.AnyArg(), "error", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	res := sw.Sweep(context.Background())

	if res.DeadlineFailures != 1 {
		t.Fatalf("a failed reply must not un-terminalize the delegation: %+v", res)
	}
	if res.ReplyErrors != 1 {
		t.Fatalf("ReplyErrors = %d, want 1 — a dropped reply must be counted, not swallowed", res.ReplyErrors)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a failed ledger row must not deny the agent its inbox notification: %v", err)
	}
}

// TestSweeper_BootGrace_DoesNotMassMarkStuckAfterOurOwnOutage.
//
// The staleness signal is workspaces.last_heartbeat_at — written BY the
// workspaces TO THIS SERVER. So if THIS server was down longer than the
// threshold, every callee in the fleet looks stale the instant we come back,
// before any of them has had a chance to beat again. Start() sweeps immediately
// on boot (deliberately), so without a grace period the first sweep after a
// workspace-server outage would mark the ENTIRE in-flight set stuck — a fleet-wide
// event manufactured entirely out of our own downtime.
//
// The deadline arm is NOT suppressed: it reads a per-row timestamp that our
// downtime cannot forge.
func TestSweeper_BootGrace_DoesNotMassMarkStuckAfterOurOwnOutage(t *testing.T) {
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	stale := time.Now().Add(-30 * time.Minute) // target looks long-dead...
	future := time.Now().Add(2 * time.Hour)

	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT d\.delegation_id, d\.caller_id, d\.callee_id`).
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "caller_id", "callee_id", "status", "beat", "deadline"}).
			AddRow("deleg-x", "caller-ws", "callee-ws", "in_progress", stale, future))
	// deliberately NO status SELECT / UPDATE: the row must not be touched at all.

	sw := NewDelegationSweeper(nil, NewDelegationLedger(nil))
	// ...but WE only just started. (NewDelegationSweeper stamps startedAt=now.)
	res := sw.Sweep(context.Background())

	if res.StuckMarked != 0 {
		t.Fatalf("StuckMarked = %d, want 0 — a sweep within one threshold of boot must "+
			"not mark anything stuck; the staleness is ours, not the target's", res.StuckMarked)
	}
	// THE REAL PIN. sqlmock cannot fail on an UNEXPECTED call; it hands the caller
	// an error instead — which the sweeper counts. So "the row was never touched"
	// is observable as res.Errors == 0, NOT as ExpectationsWereMet (which only
	// proves the calls we DID expect fired). Asserting only the latter is how the
	// first version of this test passed with the boot grace deleted.
	if res.Errors != 0 {
		t.Fatalf("Errors = %d, want 0 — the sweeper issued SQL against a row it must "+
			"not have touched during the boot grace", res.Errors)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("boot grace must not touch the row: %v", err)
	}
}

// TestSweeperReply_LostTheRace_StaysSilent pins the double-notify fix.
//
// SetStatus returns nil on TWO NON-transitions (missing row; same-status replay),
// and the sweeper used to read err==nil as "I performed the transition". Reachable
// without exotic timing: the sweeper picks up a past-deadline row while the agent
// concurrently POSTs its own terminal status. The agent terminalizes AND notifies;
// the sweeper's SetStatus then sees current == status, returns nil, and notifies
// AGAIN. The caller's agent is told twice that one delegation ended.
//
// SetStatus is now a compare-and-swap that reports whether IT was the transition.
func TestSweeperReply_LostTheRace_StaysSilent(t *testing.T) {
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	t.Setenv("DELEGATION_RESULT_INBOX_PUSH", "1")
	past := time.Now().Add(-1 * time.Minute)

	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT d\.delegation_id, d\.caller_id, d\.callee_id`).
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "caller_id", "callee_id", "status", "beat", "deadline"}).
			AddRow("deleg-x", "caller-ws", "callee-ws", "in_progress", time.Now(), past))
	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-x").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("queued"))
	// The CAS matches ZERO rows: someone else terminalized it in between.
	mock.ExpectExec(`UPDATE delegations`).WillReturnResult(sqlmock.NewResult(0, 0))
	// deliberately NO reply expectations — the winner already notified.

	sw := NewDelegationSweeper(nil, NewDelegationLedger(nil))
	res := sw.Sweep(context.Background())

	if res.DeadlineFailures != 0 {
		t.Fatalf("DeadlineFailures = %d, want 0 — we lost the race, we did not terminalize it", res.DeadlineFailures)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("losing the CAS must emit NO reply (the winner already did): %v", err)
	}
}

// TestSweeper_AlreadyStuckRowIsNotRewrittenEverySweep.
//
// `stuck` is non-terminal, so a stuck row correctly STAYS in the sweeper's
// candidate set — only the deadline may kill it. But re-calling SetStatus on it
// every sweep takes the same-status branch with a non-empty detail, which fires
// the COALESCE UPDATE: a new MVCC tuple and a WAL record every 5 minutes, for up
// to 6 hours, changing nothing — on every wedged delegation in the fleet.
//
// The row must be re-examined (for its deadline) and left alone (for its status).
func TestSweeper_AlreadyStuckRowIsNotRewrittenEverySweep(t *testing.T) {
	t.Setenv("DELEGATION_LEDGER_WRITE", "1")
	stale := time.Now().Add(-30 * time.Minute)
	future := time.Now().Add(2 * time.Hour) // deadline NOT reached

	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT d\.delegation_id, d\.caller_id, d\.callee_id, d\.status`).
		WillReturnRows(sqlmock.NewRows([]string{
			"delegation_id", "caller_id", "callee_id", "status", "beat", "deadline",
		}).AddRow("deleg-x", "caller-ws", "callee-ws", "stuck", stale, future))
	// deliberately NO status SELECT and NO UPDATE: the row is already stuck.

	sw := newTestSweeper(NewDelegationLedger(nil))
	res := sw.Sweep(context.Background())

	if res.StuckMarked != 0 {
		t.Fatalf("StuckMarked = %d, want 0 — the row is ALREADY stuck", res.StuckMarked)
	}
	if res.Errors != 0 {
		t.Fatalf("Errors = %d, want 0 — the sweeper re-wrote an already-stuck row "+
			"(unexpected SQL). That is ~72 no-op UPDATEs per wedged delegation.", res.Errors)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("an already-stuck row must be re-examined but NOT re-written: %v", err)
	}
}

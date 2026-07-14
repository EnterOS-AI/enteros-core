package handlers

// delegation_ledger_cas_test.go — SetStatus is a COMPARE-AND-SWAP, and what it
// returns is a LICENCE TO NOTIFY. These tests pin the ways it used to hand that
// licence out wrongly, and the ways it used to withhold it wrongly.
//
// Every terminal delegation transition owes the caller exactly one delegate_result
// (single-reply authority). So:
//
//   - a reply granted that should have been withheld  = a DOUBLE notification
//   - a reply withheld that should have been granted  = a delegation that dies in
//     silence, which IS #4314
//
// Both failure modes are live bugs, and the second one is the one a bool could not
// express. `ReplyNotMine` means "somebody else will tell the caller"; the ledger
// having NO OPINION (dark, no row, DB error) is `ReplyUnarbitrated` and means
// "nobody else will" — so we must. See the ReplyAuthority doc.

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestSetStatus_RowsAffectedErrorIsSurfaced closes the hole the verifier found:
// reverting this fix left the whole suite GREEN.
//
// The UPDATE is a CAS — `WHERE delegation_id = $1 AND status = $5` — so
// RowsAffected() is the ONLY thing that distinguishes "I moved this row" from
// "somebody beat me to it". The original code read:
//
//	rows, err := res.RowsAffected()
//	if err != nil {
//	    return false, nil        // <- the bug
//	}
//
// A driver that cannot report RowsAffected therefore produced (false, nil): not a
// transition, and no error. Indistinguishable from a legitimate lost race — so the
// delegation terminalized in the database and NOTHING was ever sent to the caller.
func TestSetStatus_RowsAffectedErrorIsSurfaced(t *testing.T) {
	mock := setupTestDB(t)

	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-x").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("queued"))
	// A result whose RowsAffected() fails — precisely what a driver that does not
	// support the call returns.
	mock.ExpectExec(`UPDATE delegations`).
		WillReturnResult(sqlmock.NewErrorResult(errors.New("driver does not support RowsAffected")))

	authority, err := NewDelegationLedger(nil).SetStatus(
		context.Background(), "deleg-x", "completed", "", "done")

	if err == nil {
		t.Fatal("SetStatus swallowed a RowsAffected() error. It must be surfaced: this " +
			"is the deadline arm's LAST look at the row, and a swallowed error there " +
			"loses the caller's notification permanently, with nothing in the logs.")
	}
	// We could not determine whether we won the CAS. That is the ledger failing to
	// arbitrate — NOT a finding that someone else owns the reply. If the UPDATE did
	// commit, the row is terminal, drops out of the sweeper's in-flight SELECT and is
	// never revisited, so no other writer will EVER speak for this delegation.
	// Withholding the reply here is a permanent silent death; sending it risks at
	// worst a duplicate. Prefer the duplicate.
	if authority != ReplyUnarbitrated {
		t.Errorf("a CAS whose outcome could not be read returned %v; want ReplyUnarbitrated "+
			"so the caller is still told. Returning ReplyNotMine here is indistinguishable "+
			"from a lost race and silently drops the only notification the caller will "+
			"ever get.", authority)
	}
}

// TestSetStatus_LostRaceIsNotATransition — the UPDATE matched zero rows because a
// competing writer already moved the row. That writer owns the reply and is sending
// it, so we must stay silent. Before the CAS, SetStatus returned nil here and every
// caller read "success", so BOTH writers notified.
func TestSetStatus_LostRaceIsNotATransition(t *testing.T) {
	mock := setupTestDB(t)

	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-x").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("queued"))
	mock.ExpectExec(`UPDATE delegations`).
		WillReturnResult(sqlmock.NewResult(0, 0)) // zero rows: somebody beat us

	authority, err := NewDelegationLedger(nil).SetStatus(
		context.Background(), "deleg-x", "completed", "", "done")

	if err != nil {
		t.Fatalf("a lost race is not an error, it is a no-op: %v", err)
	}
	if authority != ReplyNotMine {
		t.Fatalf("SetStatus returned %v on an UPDATE that matched ZERO rows; want "+
			"ReplyNotMine. The writer that DID move the row is notifying the caller — if "+
			"we notify too, the caller's agent receives two delegate_results for one "+
			"delegation. This is the case the single-reply rule exists for.", authority)
	}
	if mayReply(authority) {
		t.Fatal("mayReply() authorized a reply on a LOST CAS RACE — double notification")
	}
}

// TestSetStatus_MissingRowMustStillNotifyTheCaller — THE REGRESSION THE REVIEW CAUGHT,
// and the reason ReplyAuthority is not a bool.
//
// A delegation with no ledger row is not hypothetical: EVERY delegation in flight at
// the moment DELEGATION_LEDGER_WRITE is flipped was created while the ledger was
// dark, so it has no row. `Insert` is also best-effort and swallows its errors, so a
// dropped insert produces the same shape.
//
// The first cut gated every inbox push on a `didTransition bool`, and a missing row
// returned false. So: the agent genuinely reports `failed`, the ledger has nothing to
// arbitrate with, NOBODY tells the caller, and the caller waits forever for a
// delegation that already ended. That is #4314 — reintroduced by the fix for #4314,
// at the exact moment of the flip.
//
// A missing row is not "somebody else owns the reply". It is "there is no arbiter",
// and with no arbiter we fall back to the pre-ledger behaviour: reply.
func TestSetStatus_MissingRowMustStillNotifyTheCaller(t *testing.T) {
	mock := setupTestDB(t)

	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("ghost").
		WillReturnRows(sqlmock.NewRows([]string{"status"})) // no rows

	authority, err := NewDelegationLedger(nil).SetStatus(
		context.Background(), "ghost", "completed", "", "done")

	if err != nil {
		t.Fatalf("a missing ledger row is not an error — the ledger is best-effort and "+
			"must not fail the delegation: %v", err)
	}
	if authority != ReplyUnarbitrated {
		t.Fatalf("SetStatus returned %v for a delegation with NO LEDGER ROW; want "+
			"ReplyUnarbitrated.\n"+
			"    ReplyNotMine here means 'someone else is telling the caller' — but there "+
			"is no row, so there is no other writer, and NOBODY tells it. The agent "+
			"reported a terminal status and the caller waits forever.\n"+
			"    Every delegation in flight at the DELEGATION_LEDGER_WRITE flip has no "+
			"row, so this is the NORMAL state during the flip, not an edge case.",
			authority)
	}
	if !mayReply(authority) {
		t.Fatal("mayReply() SUPPRESSED the caller's reply for a delegation the ledger " +
			"has no row for. Silent death (#4314), fleet-wide, at the moment of the flip.")
	}
}

// TestSetStatus_DuplicateTerminalPostIsNotMine — the row is ALREADY in this status,
// so whoever put it there has already replied. This is the one non-transition that
// genuinely must stay silent, and it must be distinguishable from the missing-row
// case above. A bool made them identical.
func TestSetStatus_DuplicateTerminalPostIsNotMine(t *testing.T) {
	mock := setupTestDB(t)

	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-x").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("completed"))

	authority, err := NewDelegationLedger(nil).SetStatus(
		context.Background(), "deleg-x", "completed", "", "")

	if err != nil {
		t.Fatalf("a duplicate terminal post is idempotent, not an error: %v", err)
	}
	if authority != ReplyNotMine {
		t.Fatalf("a replay of a status the row ALREADY holds returned %v; want "+
			"ReplyNotMine. The writer that first set it notified the caller; replying "+
			"again is the double-notify bug.", authority)
	}
}

// TestSetStatus_SelectErrorIsDeferredNotUnarbitrated — N5, case (a).
//
// The SELECT that reads current status failed, so NO UPDATE RAN and the row is
// DEFINITIVELY unchanged — still in-flight. The caller of this (the sweeper) is its
// own retrier and will be back in 5 minutes. So the authority must be ReplyDeferred
// (stay silent, someone WILL speak — us, next tick), NOT ReplyUnarbitrated (reply
// now, nobody ever will). Returning Unarbitrated here double-replies on the retry.
func TestSetStatus_SelectErrorIsDeferredNotUnarbitrated(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-x").
		WillReturnError(errors.New("connection reset by peer"))

	authority, err := NewDelegationLedger(nil).SetStatus(
		context.Background(), "deleg-x", "failed", "boom", "")

	if err == nil {
		t.Fatal("a failed SELECT must surface its error")
	}
	if authority != ReplyDeferred {
		t.Fatalf("SELECT error returned %v, want ReplyDeferred. No UPDATE ran, so the "+
			"row is unchanged and still in-flight; the sweeper retries it next tick. "+
			"ReplyUnarbitrated here makes THIS sweep reply AND the retry reply — two "+
			"'Delegation failed' for one delegation.", authority)
	}
	if mayReply(authority) {
		t.Fatal("mayReply authorized a reply on a transition that provably did not happen")
	}
}

// TestSetStatus_UpdateErrorIsDeferredNotUnarbitrated — N5, case (b). Same as (a): the
// UPDATE itself errored (lock timeout, statement timeout, a reset pooled connection),
// so it did not land. Row unchanged, still in-flight, sweeper will retry.
func TestSetStatus_UpdateErrorIsDeferredNotUnarbitrated(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-x").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("queued"))
	mock.ExpectExec(`UPDATE delegations`).
		WillReturnError(errors.New("canceling statement due to lock timeout"))

	authority, err := NewDelegationLedger(nil).SetStatus(
		context.Background(), "deleg-x", "failed", "boom", "")

	if err == nil {
		t.Fatal("a failed UPDATE must surface its error")
	}
	if authority != ReplyDeferred {
		t.Fatalf("UPDATE error returned %v, want ReplyDeferred. The UPDATE did not land, "+
			"so the row is unchanged and still in-flight; replying now double-replies "+
			"when the sweeper retries.", authority)
	}
}

// TestSetStatus_RowsAffectedErrorStaysUnarbitrated — the boundary. This one MUST remain
// ReplyUnarbitrated (reply), because unlike (a)/(b) the UPDATE may have COMMITTED — and
// if it did, the row is terminal, leaves the in-flight SELECT, and no sweep ever
// revisits it. Silence here is the permanent silent death. This pins that the N5 split
// did not over-reach and reclassify (c) too.
func TestSetStatus_RowsAffectedErrorStaysUnarbitrated(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT status FROM delegations WHERE delegation_id = \$1`).
		WithArgs("deleg-x").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("queued"))
	mock.ExpectExec(`UPDATE delegations`).
		WillReturnResult(sqlmock.NewErrorResult(errors.New("driver does not support RowsAffected")))

	authority, err := NewDelegationLedger(nil).SetStatus(
		context.Background(), "deleg-x", "failed", "boom", "")

	if err == nil {
		t.Fatal("a RowsAffected error must be surfaced")
	}
	if authority != ReplyUnarbitrated {
		t.Fatalf("RowsAffected error returned %v, want ReplyUnarbitrated. The UPDATE MAY "+
			"have committed; if it did the row is terminal and no sweep revisits it, so "+
			"this is the last chance to notify. ReplyDeferred here is a permanent silent "+
			"death. (a)/(b) defer; (c) does not.", authority)
	}
}

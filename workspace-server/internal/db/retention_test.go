package db

// Unit (sqlmock) coverage for PruneActivityLogs — the MUST-FIX 3
// age-AND-acked prune with a hard ceiling. These pin the SQL shape and the
// hard>=soft clamp that makes the predicate provably no less conservative
// than the old age-only prune. The behavioural proof that it actually RETAINS
// old-but-unacked rows lives in the handlers integration test.

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func withMockDB(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	prev := DB
	DB = mockDB
	t.Cleanup(func() {
		DB = prev
		mockDB.Close()
	})
	return mock
}

// TestPruneActivityLogs_AckedAndAgePredicate pins the predicate shape: the
// soft branch is gated on both age (make_interval) AND the acked cursor
// (last_acked_seq via a correlated subquery), OR'd with the hard-ceiling age
// branch. A regression to the old age-only `DELETE ... WHERE created_at < ...`
// would not contain last_acked_seq and would fail this expectation.
func TestPruneActivityLogs_AckedAndAgePredicate(t *testing.T) {
	mock := withMockDB(t)
	mock.ExpectExec(`(?s)DELETE FROM activity_logs.+last_acked_seq.+make_interval`).
		WithArgs(7, 30).
		WillReturnResult(sqlmock.NewResult(0, 3))

	n, err := PruneActivityLogs(context.Background(), 7, 30)
	if err != nil {
		t.Fatalf("PruneActivityLogs: %v", err)
	}
	if n != 3 {
		t.Fatalf("rows affected = %d, want 3", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestPruneActivityLogs_ClampsHardBelowSoft — a misconfigured hard < soft is
// clamped UP to soft so the hard branch can never delete a row younger than
// the acked soft floor would (which would violate "never prunes earlier").
func TestPruneActivityLogs_ClampsHardBelowSoft(t *testing.T) {
	mock := withMockDB(t)
	// soft=30, hard=7 (bad) → both args must land as 30.
	mock.ExpectExec(`(?s)DELETE FROM activity_logs`).
		WithArgs(30, 30).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if _, err := PruneActivityLogs(context.Background(), 30, 7); err != nil {
		t.Fatalf("PruneActivityLogs: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("hard<soft was not clamped to soft: %v", err)
	}
}

// TestPruneActivityLogs_DefaultsNonPositiveSoft — a zero/negative soft falls
// back to 7 (defensive; the caller already defaults, but the DB helper must
// not issue a nonsensical 0-day interval).
func TestPruneActivityLogs_DefaultsNonPositiveSoft(t *testing.T) {
	mock := withMockDB(t)
	mock.ExpectExec(`(?s)DELETE FROM activity_logs`).
		WithArgs(7, 30).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if _, err := PruneActivityLogs(context.Background(), 0, 30); err != nil {
		t.Fatalf("PruneActivityLogs: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("non-positive soft not defaulted: %v", err)
	}
}

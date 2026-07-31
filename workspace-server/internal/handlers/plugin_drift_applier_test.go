package handlers

// plugin_drift_applier_test.go — coverage for the DETERMINISTIC drift-queue
// drainer (core#4977).
//
// What these pin, and why each matters:
//
//   - the drainer selects PENDING entries OLDEST-FIRST and bounded. Without a
//     bound, a large fan-out would restart the whole fleet in one tick.
//   - a per-entry failure is ISOLATED. Convergence for workspace B must not
//     depend on workspace A's upstream being reachable — that coupling is how
//     "one bad plugin freezes the fleet" bugs happen.
//   - sentinel outcomes (gone / dismissed / already applied) are NOT failures.
//     Counting them as failures would make a healthy queue look broken.
//
// The apply seam is stubbed on purpose: applying for real needs a git fetch
// and a Docker daemon. What is under test here is the DRAIN CONTRACT. The
// apply implementation itself is covered by admin_plugin_drift_test.go (unit)
// and admin_plugin_drift_integration_test.go (real Postgres).

import (
	"context"
	"errors"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/plugins"
	"github.com/DATA-DOG/go-sqlmock"
)

// expectPendingSelect wires the drainer's SELECT and returns the given ids.
func expectPendingSelect(mock sqlmock.Sqlmock, ids ...string) {
	rows := sqlmock.NewRows([]string{"id"})
	for _, id := range ids {
		rows = rows.AddRow(id)
	}
	mock.ExpectQuery(`SELECT id\s+FROM plugin_update_queue\s+WHERE status = 'pending'\s+ORDER BY created_at ASC\s+LIMIT`).
		WillReturnRows(rows)
}

// TestDrainPendingDrift_AppliesEveryPendingEntry is the core regression gate:
// before core#4977 nothing consumed this queue at all, so a detected drift sat
// pending forever and the workspace never converged.
func TestDrainPendingDrift_AppliesEveryPendingEntry(t *testing.T) {
	mock := setupTestDB(t)
	expectPendingSelect(mock, "q1", "q2", "q3")

	var seen []string
	applied, failed := DrainPendingDrift(context.Background(),
		func(_ context.Context, id string) (*driftApplyOutcome, error) {
			seen = append(seen, id)
			return &driftApplyOutcome{WorkspaceID: "ws", PluginName: "p", Materialized: true}, nil
		})

	if applied != 3 || failed != 0 {
		t.Errorf("applied=%d failed=%d, want 3/0", applied, failed)
	}
	if len(seen) != 3 || seen[0] != "q1" || seen[1] != "q2" || seen[2] != "q3" {
		t.Errorf("drain order = %v, want [q1 q2 q3] (oldest-first)", seen)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestDrainPendingDrift_OneFailureDoesNotStopTheRest: entry 2 fails; 1 and 3
// must still be attempted. A drainer that aborted on first error would let a
// single unreachable upstream freeze convergence for every other workspace.
func TestDrainPendingDrift_OneFailureDoesNotStopTheRest(t *testing.T) {
	mock := setupTestDB(t)
	expectPendingSelect(mock, "q1", "q2", "q3")

	var seen []string
	applied, failed := DrainPendingDrift(context.Background(),
		func(_ context.Context, id string) (*driftApplyOutcome, error) {
			seen = append(seen, id)
			if id == "q2" {
				return nil, errors.New("upstream unreachable")
			}
			return &driftApplyOutcome{Materialized: true}, nil
		})

	if applied != 2 || failed != 1 {
		t.Errorf("applied=%d failed=%d, want 2/1", applied, failed)
	}
	if len(seen) != 3 {
		t.Errorf("attempted %d entries (%v), want all 3 — a failure must not abort the drain", len(seen), seen)
	}
}

// TestDrainPendingDrift_SentinelsAreNotFailures: a row that vanished, was
// dismissed, or was already applied is "nothing to do", not an error. If these
// counted as failures a perfectly healthy queue would report a failing drain
// on every tick and mask a real one.
func TestDrainPendingDrift_SentinelsAreNotFailures(t *testing.T) {
	mock := setupTestDB(t)
	expectPendingSelect(mock, "gone", "dismissed", "done")

	applied, failed := DrainPendingDrift(context.Background(),
		func(_ context.Context, id string) (*driftApplyOutcome, error) {
			switch id {
			case "gone":
				return &driftApplyOutcome{}, errDriftEntryNotFound
			case "dismissed":
				return &driftApplyOutcome{}, errDriftEntryDismissed
			default:
				return &driftApplyOutcome{}, errDriftEntryAlreadyApplied
			}
		})

	if applied != 0 || failed != 0 {
		t.Errorf("applied=%d failed=%d, want 0/0 — sentinels are not failures", applied, failed)
	}
}

// TestDrainPendingDrift_EmptyQueueIsANoOp — the common case must be silent and
// cheap, not an error path.
func TestDrainPendingDrift_EmptyQueueIsANoOp(t *testing.T) {
	mock := setupTestDB(t)
	expectPendingSelect(mock)

	called := false
	applied, failed := DrainPendingDrift(context.Background(),
		func(_ context.Context, _ string) (*driftApplyOutcome, error) {
			called = true
			return &driftApplyOutcome{}, nil
		})

	if applied != 0 || failed != 0 || called {
		t.Errorf("applied=%d failed=%d applyCalled=%v, want 0/0/false", applied, failed, called)
	}
}

// TestDrainPendingDrift_SelectErrorIsSurvivable: a transient DB error must not
// panic or wedge the loop — the next tick retries.
func TestDrainPendingDrift_SelectErrorIsSurvivable(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id\s+FROM plugin_update_queue`).
		WillReturnError(errors.New("connection reset"))

	applied, failed := DrainPendingDrift(context.Background(),
		func(_ context.Context, _ string) (*driftApplyOutcome, error) {
			t.Fatal("apply must not be called when the SELECT failed")
			return nil, nil
		})
	if applied != 0 || failed != 0 {
		t.Errorf("applied=%d failed=%d, want 0/0", applied, failed)
	}
}

// TestDriftApplyBudget_IsBounded pins the per-tick cap as a real constraint.
// Each apply triggers a git fetch and usually a workspace RESTART, so an
// unbounded drain after a fan-out event would bounce the fleet at once.
func TestDriftApplyBudget_IsBounded(t *testing.T) {
	if driftApplyMaxPerTick <= 0 {
		t.Fatalf("driftApplyMaxPerTick = %d, must be positive", driftApplyMaxPerTick)
	}
	if driftApplyMaxPerTick > 100 {
		t.Errorf("driftApplyMaxPerTick = %d — too large to be a restart budget", driftApplyMaxPerTick)
	}
	// The applier must run more often than the sweeper that feeds it,
	// otherwise queued drift waits a full detection cycle to even be seen.
	if DriftApplyInterval >= plugins.DriftSweepInterval {
		t.Errorf("DriftApplyInterval (%s) must be shorter than the sweep interval (%s)",
			DriftApplyInterval, plugins.DriftSweepInterval)
	}
}

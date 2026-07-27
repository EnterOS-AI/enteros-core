package handlers

// wake_redrive_sweeper_test.go — coverage for the fleet-wide re-drive sweeper
// (wake_redrive_sweeper.go). The sweeper's only job is: find the distinct stuck
// workspaces (one fleet query) and call the re-drive owner once per workspace.
// The owner's own selection/bounding is proven in wake_redrive_test.go +
// wake_redrive_integration_test.go, so here the owner is a recorder and we pin
// the sweep's fan-out + the nil-safe gates.

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

const wakeRedriveFleetQuery = `SELECT DISTINCT workspace_id\s+FROM wake_intents`

// recordedRedrive captures the per-workspace ReDriveStuckWakes calls the sweeper
// makes, and returns a canned result so the sweep's aggregation is observable.
type recordedRedrive struct {
	calls []string
	perWS map[string]ReDriveResult
}

func (r *recordedRedrive) redrive(_ context.Context, workspaceID string) (ReDriveResult, error) {
	r.calls = append(r.calls, workspaceID)
	if res, ok := r.perWS[workspaceID]; ok {
		return res, nil
	}
	return ReDriveResult{}, nil
}

// TestWakeRedriveSweeper_DrivesEachStuckWorkspace proves a tick with stuck
// workspaces runs ONE fleet query and calls ReDriveStuckWakes for each distinct
// workspace it returns, aggregating their results.
func TestWakeRedriveSweeper_DrivesEachStuckWorkspace(t *testing.T) {
	mock := setupTestDB(t)
	rec := &recordedRedrive{perWS: map[string]ReDriveResult{
		"ws-a": {Redriven: 2, Dropped: 0},
		"ws-b": {Redriven: 0, Dropped: 1},
	}}
	sw := NewWakeRedriveSweeper(nil, rec.redrive, func() bool { return true })

	mock.ExpectQuery(wakeRedriveFleetQuery).
		WithArgs(int(redriveStuckAfter.Seconds()), int(redriveMinInterval.Seconds()), wakeRedriveFleetLimit).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id"}).AddRow("ws-a").AddRow("ws-b"))

	res := sw.Sweep(context.Background())
	if res.Workspaces != 2 || res.Redriven != 2 || res.Dropped != 1 || res.Errors != 0 {
		t.Fatalf("sweep result = %+v, want Workspaces=2 Redriven=2 Dropped=1 Errors=0", res)
	}
	if len(rec.calls) != 2 || rec.calls[0] != "ws-a" || rec.calls[1] != "ws-b" {
		t.Errorf("re-drive calls = %v, want [ws-a ws-b]", rec.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestWakeRedriveSweeper_NoStuckWorkspaces_IsNoOp proves a tick that finds no
// stuck workspaces runs the fleet query and then does nothing — the owner is
// never called.
func TestWakeRedriveSweeper_NoStuckWorkspaces_IsNoOp(t *testing.T) {
	mock := setupTestDB(t)
	rec := &recordedRedrive{}
	sw := NewWakeRedriveSweeper(nil, rec.redrive, func() bool { return true })

	mock.ExpectQuery(wakeRedriveFleetQuery).
		WithArgs(int(redriveStuckAfter.Seconds()), int(redriveMinInterval.Seconds()), wakeRedriveFleetLimit).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id"}))

	res := sw.Sweep(context.Background())
	if res.Workspaces != 0 || res.Redriven != 0 || res.Dropped != 0 || res.Errors != 0 {
		t.Errorf("empty-fleet sweep result = %+v, want all zero", res)
	}
	if len(rec.calls) != 0 {
		t.Errorf("owner called %d times on an empty fleet; want 0", len(rec.calls))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestWakeRedriveSweeper_UnwiredReEmit_DoesNothing proves the nil-safe gate:
// when the re-emit hook is unwired the sweep does NOT even run the fleet query
// (the mock expects no queries), and the owner is never called.
func TestWakeRedriveSweeper_UnwiredReEmit_DoesNothing(t *testing.T) {
	mock := setupTestDB(t)
	rec := &recordedRedrive{}
	sw := NewWakeRedriveSweeper(nil, rec.redrive, func() bool { return false }) // re-emit unwired

	res := sw.Sweep(context.Background())
	if res != (RedriveSweepResult{}) {
		t.Errorf("unwired sweep result = %+v, want zero", res)
	}
	if len(rec.calls) != 0 {
		t.Errorf("owner called %d times while unwired; want 0", len(rec.calls))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unwired sweep touched the DB; want no queries: %v", err)
	}
}

// TestWakeRedriveSweeper_NilOwner_IsNoOp proves a sweeper with no owner injected
// is a safe no-op (defensive: production always injects one).
func TestWakeRedriveSweeper_NilOwner_IsNoOp(t *testing.T) {
	mock := setupTestDB(t)
	sw := NewWakeRedriveSweeper(nil, nil, func() bool { return true })

	res := sw.Sweep(context.Background())
	if res != (RedriveSweepResult{}) {
		t.Errorf("nil-owner sweep result = %+v, want zero", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("nil-owner sweep touched the DB; want no queries: %v", err)
	}
}

// TestWakeRedriveSweeper_IntervalDefault pins the default cadence + the env
// override plumbing (mirrors the other sweepers' interval test).
func TestWakeRedriveSweeper_IntervalDefault(t *testing.T) {
	sw := NewWakeRedriveSweeper(nil, func(context.Context, string) (ReDriveResult, error) {
		return ReDriveResult{}, nil
	}, func() bool { return true })
	if sw.Interval() != defaultWakeRedriveInterval {
		t.Errorf("default interval = %s, want %s", sw.Interval(), defaultWakeRedriveInterval)
	}
}

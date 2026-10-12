package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/metrics"
)

// phantom_busy_sweeper_test.go — coverage for the standalone phantom-busy
// counter-drift sweeper extracted from the retired core scheduler (P4).
//
// The drift predicate itself (active_tasks>0 AND status!='removed' AND no
// activity_logs within the stale window) lives entirely in the UPDATE ...
// RETURNING SQL, which sqlmock cannot evaluate. So at this level each case is
// expressed as "the query RETURNs N rows" vs "returns none", and the test pins
// that N returned rows ⇒ Sweep reports N reset + the metric ticks N times, and
// an empty result ⇒ zero reset, zero metric. The predicate wiring is covered by
// the real-DB integration harness, not here.

func newTestPhantomSweeper(t *testing.T) *PhantomBusySweeper {
	t.Helper()
	return NewPhantomBusySweeper(nil) // binds to the sqlmock db.DB set by setupTestDB
}

func TestPhantomBusySweeper_EmptyResultIsCleanNoOp(t *testing.T) {
	mock := setupTestDB(t)
	sw := newTestPhantomSweeper(t)

	before := metrics.PhantomBusyResets()

	mock.ExpectQuery(`UPDATE workspaces\s+SET active_tasks = 0`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))

	if n := sw.Sweep(context.Background()); n != 0 {
		t.Errorf("empty set must reset zero workspaces; got %d", n)
	}
	if delta := metrics.PhantomBusyResets() - before; delta != 0 {
		t.Errorf("no metric increment expected on empty set; got +%d", delta)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestPhantomBusySweeper_ResetsReturnedRowsAndTicksMetric(t *testing.T) {
	mock := setupTestDB(t)
	sw := newTestPhantomSweeper(t)

	before := metrics.PhantomBusyResets()

	// Two drifted workspaces come back from the UPDATE ... RETURNING.
	mock.ExpectQuery(`UPDATE workspaces\s+SET active_tasks = 0`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}).
			AddRow("11111111-1111-1111-1111-111111111111", "ws-a").
			AddRow("22222222-2222-2222-2222-222222222222", "ws-b"))

	if n := sw.Sweep(context.Background()); n != 2 {
		t.Errorf("expected 2 workspaces reset; got %d", n)
	}
	if delta := metrics.PhantomBusyResets() - before; delta != 2 {
		t.Errorf("metric must tick once per reset row; got +%d, want +2", delta)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestPhantomBusySweeper_QueryErrorReturnsZeroNoMetric(t *testing.T) {
	mock := setupTestDB(t)
	sw := newTestPhantomSweeper(t)

	before := metrics.PhantomBusyResets()

	mock.ExpectQuery(`UPDATE workspaces\s+SET active_tasks = 0`).
		WillReturnError(context.DeadlineExceeded)

	if n := sw.Sweep(context.Background()); n != 0 {
		t.Errorf("query error must yield zero reset; got %d", n)
	}
	if delta := metrics.PhantomBusyResets() - before; delta != 0 {
		t.Errorf("query error must not tick the metric; got +%d", delta)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestPhantomBusySweeper_IntervalEnvOverride(t *testing.T) {
	// Unset → default cadence.
	if got := NewPhantomBusySweeper(nil).Interval(); got != defaultPhantomBusyInterval {
		t.Errorf("default interval: got %s want %s", got, defaultPhantomBusyInterval)
	}
	// Override honored at construction.
	t.Setenv("PHANTOM_BUSY_SWEEPER_INTERVAL_S", "120")
	if got := NewPhantomBusySweeper(nil).Interval(); got != 120*time.Second {
		t.Errorf("env override: got %s want 120s", got)
	}
}

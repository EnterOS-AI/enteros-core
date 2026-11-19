package registry

// cp_orphan_sweeper_grace_test.go — the sweeper leaves a workspace that was
// removed moments ago to the delete that removed it.
//
// A delete marks its rows 'removed' FIRST and stops (and, when asked, erases)
// them afterwards. The sweeper selects exactly those rows. A sweep that lands
// in between stops the workspace keep-state and clears instance_id, and the
// delete's own StopAndPrune then finds no instance id and does nothing: the API
// answers "purged" while the identity claim, the seed Secret and the Service
// stay, with no row left to find them by (review of 2026-11-19, internal#1316).
// The gap is under a second for most deletes and up to about 15 s for one that
// waits on an in-flight provision, against a 60 s sweep interval.

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// The sweep asks only for rows removed longer ago than the grace.
func TestCPSweepOnce_LeavesWorkspacesRemovedWithinTheGrace(t *testing.T) {
	mock := setupTestDB(t)
	reaper := &fakeCPReaper{}

	mock.ExpectQuery(`(?s)WHERE status = 'removed'\s+AND instance_id IS NOT NULL\s+AND instance_id != ''\s+AND updated_at < now\(\) - make_interval\(secs => \$2\)\s+ORDER BY updated_at DESC\s+LIMIT \$1`).
		WithArgs(cpSweepLimit, cpSweepRemovedGrace.Seconds()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	cpSweepOnce(context.Background(), reaper)

	if len(reaper.stopCalls) != 0 {
		t.Fatalf("Stop calls = %v, want none", reaper.stopCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the sweep did not ask for rows older than the grace: %v", err)
	}
}

// The grace has to outlast the slowest delete: the 15 s wait for an in-flight
// provision, the 10 s wait for a Light operation, and the 30 s teardown budget.
// It must also stay short enough that a workspace whose inline stop failed is
// not left running for long.
func TestCPSweepRemovedGrace_OutlastsADeleteAndStaysShort(t *testing.T) {
	if cpSweepRemovedGrace < 2*time.Minute {
		t.Errorf("grace = %s: a slow delete (15 s + 10 s + 30 s) could still be pre-empted", cpSweepRemovedGrace)
	}
	if cpSweepRemovedGrace > 15*time.Minute {
		t.Errorf("grace = %s: a leaked instance would keep running that long before the sweeper stops it", cpSweepRemovedGrace)
	}
}

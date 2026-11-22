package registry

// cp_kept_state_reaper_test.go — the state a plain delete keeps is erased once
// the retention window has passed (molecule-core#5193).

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/rescue"
)

type eraseCall struct{ workspaceID, instanceID string }

type fakeKeptStateEraser struct {
	mu    sync.Mutex
	err   error
	calls []eraseCall
}

func (f *fakeKeptStateEraser) EraseKeptState(_ context.Context, workspaceID, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, eraseCall{workspaceID, instanceID})
	return f.err
}

var (
	keptCandidatesRe = `(?s)SELECT id::text, kept_state_instance_id\s+FROM workspaces\s+WHERE status = 'removed'\s+AND kept_state_instance_id IS NOT NULL\s+AND kept_state_instance_id != ''\s+AND kept_state_since < now\(\) - make_interval\(secs => \$2\)\s+ORDER BY kept_state_since\s+LIMIT \$1`
	keptLockRe       = `(?s)SELECT kept_state_instance_id\s+FROM workspaces\s+WHERE id = \$1\s+AND status = 'removed'\s+AND kept_state_instance_id = \$2\s+FOR UPDATE SKIP LOCKED`
	keptClearRe      = regexp.QuoteMeta(`UPDATE workspaces SET kept_state_instance_id = NULL, kept_state_since = NULL WHERE id = $1`)
	keptBackoffRe    = regexp.QuoteMeta(`UPDATE workspaces SET kept_state_since = kept_state_since + make_interval(secs => $2) WHERE id = $1`)
)

// The workspace has been removed past the retention: its kept state is erased
// through the control plane with the instance id the delete stopped, and the
// row forgets it.
func TestKeptStateReapOnce_ErasesPastTheRetention(t *testing.T) {
	mock := setupTestDB(t)
	eraser := &fakeKeptStateEraser{}

	mock.ExpectQuery(keptCandidatesRe).
		WithArgs(keptStateReapLimit, KeptStateRetention.Seconds()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "kept_state_instance_id"}).AddRow("ws-1", "enteros-ws-ws1"))
	mock.ExpectBegin()
	mock.ExpectQuery(keptLockRe).WithArgs("ws-1", "enteros-ws-ws1").
		WillReturnRows(sqlmock.NewRows([]string{"kept_state_instance_id"}).AddRow("enteros-ws-ws1"))
	mock.ExpectExec(keptClearRe).WithArgs("ws-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	cpKeptStateReapOnce(context.Background(), eraser)

	if len(eraser.calls) != 1 || eraser.calls[0] != (eraseCall{"ws-1", "enteros-ws-ws1"}) {
		t.Fatalf("erase calls = %v, want one for ws-1 / enteros-ws-ws1", eraser.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// A failed erase keeps the instance id (the next try needs it) and pushes the
// row back by the backoff, so a control plane that keeps refusing is asked
// again later rather than every cycle.
func TestKeptStateReapOnce_FailedEraseKeepsTheIDAndBacksOff(t *testing.T) {
	mock := setupTestDB(t)
	eraser := &fakeKeptStateEraser{err: errors.New("cp provisioner: stop ws-1: unexpected 500")}

	mock.ExpectQuery(keptCandidatesRe).
		WithArgs(keptStateReapLimit, KeptStateRetention.Seconds()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "kept_state_instance_id"}).AddRow("ws-1", "enteros-ws-ws1"))
	mock.ExpectBegin()
	mock.ExpectQuery(keptLockRe).WithArgs("ws-1", "enteros-ws-ws1").
		WillReturnRows(sqlmock.NewRows([]string{"kept_state_instance_id"}).AddRow("enteros-ws-ws1"))
	mock.ExpectExec(keptBackoffRe).WithArgs("ws-1", keptStateEraseBackoff.Seconds()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	cpKeptStateReapOnce(context.Background(), eraser)

	if len(eraser.calls) != 1 {
		t.Fatalf("erase calls = %v, want one", eraser.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations (the id must NOT be cleared): %v", err)
	}
}

// A row that was revived (a removed concierge brought back in place) or that
// another replica is erasing right now is not locked by this pass: nothing is
// erased. Erasing a revived workspace would delete the identity of a LIVE
// agent.
func TestKeptStateReapOnce_SkipsARowThatIsNoLongerRemoved(t *testing.T) {
	mock := setupTestDB(t)
	eraser := &fakeKeptStateEraser{}

	mock.ExpectQuery(keptCandidatesRe).
		WithArgs(keptStateReapLimit, KeptStateRetention.Seconds()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "kept_state_instance_id"}).AddRow("ws-1", "enteros-ws-ws1"))
	mock.ExpectBegin()
	mock.ExpectQuery(keptLockRe).WithArgs("ws-1", "enteros-ws-ws1").
		WillReturnRows(sqlmock.NewRows([]string{"kept_state_instance_id"}))
	mock.ExpectRollback()

	cpKeptStateReapOnce(context.Background(), eraser)

	if len(eraser.calls) != 0 {
		t.Fatalf("erase calls = %v, want none", eraser.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestKeptStateReapOnce_NothingToDo(t *testing.T) {
	mock := setupTestDB(t)
	eraser := &fakeKeptStateEraser{}
	mock.ExpectQuery(keptCandidatesRe).
		WithArgs(keptStateReapLimit, KeptStateRetention.Seconds()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "kept_state_instance_id"}))

	cpKeptStateReapOnce(context.Background(), eraser)

	if len(eraser.calls) != 0 {
		t.Fatalf("erase calls = %v, want none", eraser.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestKeptStateReapOnce_NilDBAndNilEraserAreSafe(t *testing.T) {
	prev := db.DB
	db.DB = nil
	t.Cleanup(func() { db.DB = prev })
	cpKeptStateReapOnce(context.Background(), &fakeKeptStateEraser{})
	StartCPKeptStateReaper(context.Background(), nil) // returns at once
}

// The retention is the platform's: on AWS a plain delete's data volume is
// reaped by the control plane's orphan sweep after the same 24 h
// (rescue.RescueVolumeGrace is that number's SSOT in this repo). It must also
// outlast the CP orphan sweeper's grace, or a row could be erased while the
// delete that removed it is still running.
func TestKeptStateRetention_IsThePlatformsVolumeGrace(t *testing.T) {
	if KeptStateRetention != rescue.RescueVolumeGrace {
		t.Errorf("KeptStateRetention = %s, want %s (the volume grace on every other substrate)", KeptStateRetention, rescue.RescueVolumeGrace)
	}
	if KeptStateRetention <= cpSweepRemovedGrace {
		t.Errorf("KeptStateRetention = %s must outlast the removed-row grace %s", KeptStateRetention, cpSweepRemovedGrace)
	}
	if keptStateEraseBackoff < 10*time.Minute || keptStateEraseBackoff > KeptStateRetention {
		t.Errorf("keptStateEraseBackoff = %s: too eager (a refusing CP is asked every few minutes) or longer than the retention", keptStateEraseBackoff)
	}
}

// The CP orphan sweeper's own Stop is keep-state too, so when it clears
// instance_id it must keep the id for the reaper exactly as the delete does.
func TestCPSweepOnce_KeepsTheStoppedInstanceForTheReaper(t *testing.T) {
	mock := setupTestDB(t)
	reaper := &fakeCPReaper{}

	mock.ExpectQuery(`(?s)SELECT id::text\s+FROM workspaces`).
		WithArgs(cpSweepLimit, cpSweepRemovedGrace.Seconds()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-uuid-1"))
	mock.ExpectExec(regexp.QuoteMeta(db.ClearInstanceIDKeepingStateSQL)).
		WithArgs("ws-uuid-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	cpSweepOnce(context.Background(), reaper)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the sweeper must keep the stopped instance id for the reaper: %v", err)
	}
}

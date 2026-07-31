package handlers

// plugin_drift_materialize_test.go — the "don't report converged when nothing
// converged" rule (core#4977).
//
// THE BUG THIS PINS
//
// On a docker-less tenant the apply path copies NO bytes: DeliverForApply
// returns errNoPushTarget ("deliver by pull") and the RESTART is what makes
// the boot installer fetch the new commit. The self-brick guard deliberately
// DEFERS that restart for a kind=platform concierge in its fragile window.
//
// The pre-fix code re-pinned installed_sha regardless. That combination is
// silently fatal: the box still has the OLD tree on disk, but workspace_plugins
// now claims the NEW sha, so the drift sweeper compares new-vs-new, finds no
// drift, and never reports it again. The concierge is permanently stale and
// every signal says converged.
//
// Both directions are asserted, because a fix that simply stopped re-pinning
// would break ordinary auto-apply just as badly (a workspace that DID converge
// would be re-queued forever, restarting on every sweep).

import (
	"context"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/plugins"
	"github.com/DATA-DOG/go-sqlmock"
)

// stubDriftStaging points the stage/deliver seams at in-memory fakes.
// deliveredByPush=false reproduces the docker-less tenant (errNoPushTarget).
func stubDriftStaging(t *testing.T, newSHA string, deliveredByPush bool) {
	t.Helper()
	origStage, origDeliver := driftStageFn, driftDeliverFn
	driftStageFn = func(_ *PluginsHandler, _ context.Context, req installRequest) (*stageResult, error) {
		src, err := plugins.ParseSource(req.Source)
		if err != nil {
			t.Fatalf("stub stage: parse %q: %v", req.Source, err)
		}
		return &stageResult{
			StagedDir:    t.TempDir(),
			PluginName:   "reno-stars-coordinator",
			Source:       src,
			InstalledSHA: newSHA,
		}, nil
	}
	driftDeliverFn = func(_ *PluginsHandler, _ context.Context, _ string, _ *stageResult) error {
		if deliveredByPush {
			return nil
		}
		return errNoPushTarget
	}
	t.Cleanup(func() { driftStageFn, driftDeliverFn = origStage, origDeliver })
}

// recordSpy observes whether installed_sha was re-pinned. sqlmock cannot serve
// this role: an unexpected INSERT only returns an error, and the apply path
// treats a record failure as non-fatal, so the write would happen silently.
type recordSpy struct {
	calls []string // installed SHAs passed to the recorder
	err   error    // returned to the caller when non-nil
}

func (r *recordSpy) install(t *testing.T) {
	t.Helper()
	orig := driftRecordFn
	driftRecordFn = func(_ context.Context, _, _, _, _, installedSHA string) error {
		r.calls = append(r.calls, installedSHA)
		return r.err
	}
	t.Cleanup(func() { driftRecordFn = orig })
}

// expectDriftApplyPreamble wires the two reads every apply performs.
func expectDriftApplyPreamble(mock sqlmock.Sqlmock, queueID, wsID, pluginName, sourceRaw string) {
	mock.ExpectQuery(`SELECT workspace_id, plugin_name, tracked_ref, status\s+FROM plugin_update_queue`).
		WithArgs(queueID).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "plugin_name", "tracked_ref", "status"}).
			AddRow(wsID, pluginName, "ref:main", "pending"))
	mock.ExpectQuery(`SELECT source_raw FROM workspace_plugins`).
		WithArgs(wsID, pluginName).
		WillReturnRows(sqlmock.NewRows([]string{"source_raw"}).AddRow(sourceRaw))
}

// TestApplyQueuedDrift_DeferredRestart_DoesNotRePinSHA — the concierge case.
//
// sqlmock is STRICT: any statement the code issues that was not expected fails
// the test. So the absence of an `INSERT INTO workspace_plugins ... ON CONFLICT`
// expectation is itself the assertion that installed_sha was NOT advanced.
func TestApplyQueuedDrift_DeferredRestart_DoesNotRePinSHA(t *testing.T) {
	mock := setupTestDB(t)
	stubDriftStaging(t, "newsha0000000000000000000000000000000000", false /* deliver by pull */)

	const queueID, wsID = "q-concierge", "concierge-ws-uuid"
	expectDriftApplyPreamble(mock, queueID, wsID, "reno-stars-coordinator",
		"gitea://RenoStarsAI-production-client/reno-stars-coordinator#main")

	// Self-brick guard: platform + online => defer the restart.
	mock.ExpectQuery(`SELECT kind, status FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "status"}).AddRow("platform", "online"))

	// The queue entry is still marked applied — we did all we could for it.
	mock.ExpectExec(`UPDATE plugin_update_queue SET status = 'applied'`).
		WithArgs(queueID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	spy := &restartSpy{}
	rec := &recordSpy{}
	rec.install(t)
	h := NewAdminPluginDriftHandler(NewPluginsHandler(t.TempDir(), nil, spy.fn))

	outcome, err := h.applyQueuedDrift(context.Background(), queueID)
	if err != nil {
		t.Fatalf("applyQueuedDrift: %v", err)
	}
	waitGlobalAsyncForTest()

	if outcome.Restarting {
		t.Error("restarting=true for a platform concierge — the self-brick guard should have deferred it")
	}
	if outcome.Materialized {
		t.Error("materialized=true, but no bytes were pushed AND no restart was dispatched — " +
			"nothing reached the box")
	}
	// THE load-bearing assertion: the SHA must not have been re-pinned.
	// Verified by mutation — restoring the pre-fix unconditional re-pin must
	// fail exactly here.
	if len(rec.calls) != 0 {
		t.Errorf("installed_sha was re-pinned to %v after a DEFERRED restart — "+
			"the box still has the old tree, so the sweeper would now see no drift "+
			"and the concierge would be permanently stale while reporting converged", rec.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/unexpected statements: %v", err)
	}
}

// TestApplyQueuedDrift_RestartDispatched_RePinsSHA — the ordinary workspace
// case, and the negative control for the test above. A fix that stopped
// re-pinning unconditionally would fail here, and the workspace would be
// re-queued and restarted on every single sweep.
func TestApplyQueuedDrift_RestartDispatched_RePinsSHA(t *testing.T) {
	mock := setupTestDB(t)
	const newSHA = "newsha0000000000000000000000000000000000"
	stubDriftStaging(t, newSHA, false /* deliver by pull; restart carries it */)

	const queueID, wsID = "q-tenant", "tenant-ws-uuid"
	expectDriftApplyPreamble(mock, queueID, wsID, "reno-stars-coordinator",
		"gitea://RenoStarsAI-production-client/reno-stars-coordinator#main")

	// kind=workspace => restart IS dispatched.
	mock.ExpectQuery(`SELECT kind, status FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "status"}).AddRow("workspace", "online"))

	// The persist itself is observed through the record seam, not sqlmock.
	mock.ExpectExec(`UPDATE plugin_update_queue SET status = 'applied'`).
		WithArgs(queueID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	spy := &restartSpy{}
	rec := &recordSpy{}
	rec.install(t)
	h := NewAdminPluginDriftHandler(NewPluginsHandler(t.TempDir(), nil, spy.fn))

	outcome, err := h.applyQueuedDrift(context.Background(), queueID)
	if err != nil {
		t.Fatalf("applyQueuedDrift: %v", err)
	}
	waitGlobalAsyncForTest()

	if len(rec.calls) != 1 || rec.calls[0] != newSHA {
		t.Errorf("installed_sha re-pin calls = %v, want exactly [%s] — "+
			"without the re-pin the sweeper re-queues this workspace forever", rec.calls, newSHA)
	}
	if !outcome.Restarting {
		t.Error("restarting=false for an ordinary workspace — auto-apply would be broken")
	}
	if !outcome.Materialized {
		t.Error("materialized=false even though a restart was dispatched")
	}
	if outcome.InstalledSHA != newSHA {
		t.Errorf("InstalledSHA = %q, want %q", outcome.InstalledSHA, newSHA)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

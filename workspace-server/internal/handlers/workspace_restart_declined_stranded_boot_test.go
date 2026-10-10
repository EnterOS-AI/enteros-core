package handlers

// A declined restart must publish a verdict when there was NO container to keep
// (core#5136 — Guard B job 915241, 2026-08-05).
//
// WHAT HAPPENED, from the tenant's own logs. A fresh staging org's concierge was
// booted the way every concierge is booted: the row is inserted by
// platform_agent.go (status='provisioning', last_heartbeat_at NULL) and then
// POST /workspaces/:id/restart is dispatched against it. The control plane could
// not make the pinned hermes image available — `ensure-image` answered 502 three
// times ("pull stalled (no progress for 2m0s)") — so core#5019's pre-warm guard
// declined at 08:55:58, 7m20s in, logging "the workspace was NOT stopped; it
// keeps its running container".
//
// There was no running container. This was a first boot.
//
// markRestartDeclined then wrote last_sample_error and LEFT THE STATUS ALONE, on
// the assumption that a decline always protects something live. The row stayed
// 'provisioning' with nothing serving it and no verdict published:
//
//   - no container ever started, so no heartbeat ever reached evaluateStatus and
//     its 300s warm-fail net (registry.go conciergeWarmupFailGrace) never armed;
//   - the provisioning-timeout sweep clocks on updated_at — which
//     markRestartDeclined itself bumps — and gives hermes 30 minutes anyway.
//
// Guard B waited 15 minutes, saw status="provisioning" with an empty tool
// inventory, and reported the WRONG cause (a missing management-MCP plugin).
//
// These cases pin the contract in BOTH directions. The positive case is the
// stranded first boot; the two negative controls are the states where the
// original assumption is TRUE and the status must stay untouched — including
// the manual-restart path, which writes status='provisioning' BEFORE dispatching
// and would be mis-failed by a status-only test. `last_heartbeat_at IS NULL` is
// the discriminator: a row that has never recorded a heartbeat has never had a
// running agent, so there is nothing for the decline to have preserved.

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// declinedStrandedSQL is the conditional terminal UPDATE. Both guards are part
// of the contract, not decoration: without `status = 'provisioning'` a paused or
// hibernated workspace would be failed by a decline, and without
// `last_heartbeat_at IS NULL` the manual-restart path (which marks the row
// provisioning before dispatching) would fail a workspace whose container is
// still up and still serving.
const declinedStrandedSQL = `UPDATE workspaces\s+SET status\s+= \$3,\s+last_sample_error = \$2,\s+updated_at\s+= now\(\)\s+WHERE id = \$1\s+AND status = 'provisioning'\s+AND last_heartbeat_at IS NULL`

// TestMarkRestartDeclined_FirstBootWithNoContainer_PublishesFailedVerdict is the
// job-915241 shape: the conditional UPDATE matches (1 row), so the decline is
// terminal. The row must be failed, the reason must name the decline rather than
// the generic "still serving its previous version", and a
// WORKSPACE_PROVISION_FAILED must be emitted so the canvas flips the node out of
// its spinner — the same event the provisioning-timeout sweep uses for exactly
// this reason.
//
// The legacy last_sample_error-only UPDATE must NOT also run: the conditional
// statement already wrote the accurate message, and a second write would
// overwrite it with the inaccurate one. sqlmock is ordered, so an extra
// statement fails the test rather than passing silently.
func TestMarkRestartDeclined_FirstBootWithNoContainer_PublishesFailedVerdict(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := &WorkspaceHandler{broadcaster: newTestBroadcaster()}

	mock.ExpectExec(declinedStrandedSQL).
		WithArgs("ws-firstboot", sqlmock.AnyArg(), "failed").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// The decline event, carrying the ACCURATE reason for this shape.
	mock.ExpectExec("INSERT INTO structure_events").
		WithArgs("WORKSPACE_RESTART_DECLINED", "ws-firstboot", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	// The canvas fail-state event.
	mock.ExpectExec("INSERT INTO structure_events").
		WithArgs("WORKSPACE_PROVISION_FAILED", "ws-firstboot", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	h.markRestartDeclined(context.Background(), "ws-firstboot", "concierge", "hermes", "platform-agent")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a first-boot decline with no container to keep must fail the row and publish "+
			"WORKSPACE_PROVISION_FAILED, and must NOT then re-write last_sample_error with the "+
			"\"still serving its previous version\" message: %v", err)
	}
}

// TestMarkRestartDeclined_LiveContainer_LeavesStatusUntouched is the negative
// control for the manual-restart path, and the reason the discriminator is
// last_heartbeat_at rather than status alone.
//
// RestartWorkspaceAutoOpts is reached from POST /workspaces/:id/restart, which
// has ALREADY written status='provisioning' before dispatching. The container is
// still up (the decline is what kept it up) and it has heartbeated, so the
// conditional UPDATE matches 0 rows. The historical behaviour must survive
// exactly: last_sample_error only, status untouched, one event.
func TestMarkRestartDeclined_LiveContainer_LeavesStatusUntouched(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := &WorkspaceHandler{broadcaster: newTestBroadcaster()}

	mock.ExpectExec(declinedStrandedSQL).
		WithArgs("ws-live", sqlmock.AnyArg(), "failed").
		WillReturnResult(sqlmock.NewResult(0, 0))

	mock.ExpectExec("INSERT INTO structure_events").
		WithArgs("WORKSPACE_RESTART_DECLINED", "ws-live", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	// The historical write: last_sample_error ONLY, no status column.
	mock.ExpectExec(`UPDATE workspaces SET last_sample_error = \$2, updated_at = now\(\) WHERE id = \$1`).
		WithArgs("ws-live", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h.markRestartDeclined(context.Background(), "ws-live", "box", "hermes", "hermes")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a decline that DID keep a running container must not touch status — it must "+
			"record last_sample_error and nothing else (writing 'failed' over a live workspace is "+
			"the confident lie core#5025 finding 3 refused): %v", err)
	}
}

// TestMarkRestartDeclined_StrandedCheckErrors_FallsBackToLegacyBehaviour is the
// fail-open control. An unreadable row state is NOT evidence that a boot failed,
// so a DB error on the conditional UPDATE must leave the historical path intact
// rather than guessing 'failed'. This is the same rule #5135 applies to an
// unreadable poll: no information is not a verdict.
func TestMarkRestartDeclined_StrandedCheckErrors_FallsBackToLegacyBehaviour(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	h := &WorkspaceHandler{broadcaster: newTestBroadcaster()}

	mock.ExpectExec(declinedStrandedSQL).
		WithArgs("ws-dberr", sqlmock.AnyArg(), "failed").
		WillReturnError(context.DeadlineExceeded)

	mock.ExpectExec("INSERT INTO structure_events").
		WithArgs("WORKSPACE_RESTART_DECLINED", "ws-dberr", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mock.ExpectExec(`UPDATE workspaces SET last_sample_error = \$2, updated_at = now\(\) WHERE id = \$1`).
		WithArgs("ws-dberr", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h.markRestartDeclined(context.Background(), "ws-dberr", "box", "hermes", "hermes")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("an error on the stranded-boot check must fail OPEN to the historical decline "+
			"behaviour, never report a boot failure on a guess: %v", err)
	}
}

//go:build integration

package handlers

// workspace_restart_decline_integration_test.go — what a DECLINED restart leaves
// behind, asserted against a real Postgres.
//
// core#5025 added a pre-flight that can refuse to stop a workspace. Both restart
// entry points honour the refusal, but neither had a story for the row:
//
//   - Manual restart (findings 2): the HTTP handler has ALREADY written
//     status='provisioning', url='' and answered 200 by the time the dispatcher
//     runs. On a decline nothing rewrites url, and the heartbeat path writes
//     status only — so the workspace flips back to 'online' with an EMPTY url.
//     Healthy-looking and unroutable: strictly worse than the restart it
//     refused, because the container it was protecting can no longer be reached.
//
//   - Auto restart (finding 3): the cycle returns before any write at all, so an
//     unrecoverable restart is indistinguishable from "nothing was tried".
//
// These are integration tests on purpose. Both defects are about WHICH COLUMNS a
// statement touches, and a mock that is told which statements to expect cannot
// witness a column being cleared by one it was not told about.

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

func restartDeclineDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("INTEGRATION_DB_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_DB_URL unset")
	}
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Skipf("no database: %v", err)
	}
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() {
		db.DB = prev
		_ = conn.Close()
	})
	return conn
}

// seedRoutableWorkspace inserts an ONLINE workspace with a real url — the state
// a user's healthy box is in when a restart is requested.
func seedRoutableWorkspace(t *testing.T, conn *sql.DB, name string) (id, url string) {
	t.Helper()
	id = uuid.New().String()
	url = "http://127.0.0.1:18080"
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO workspaces (id, name, kind, tier, runtime, status, url)
		VALUES ($1, $2, 'workspace', 0, 'hermes', 'online', $3)
	`, id, name, url); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() { _, _ = conn.Exec(`DELETE FROM workspaces WHERE id = $1`, id) })
	return id, url
}

func declineWorkspaceRow(t *testing.T, conn *sql.DB, id string) (status, url string) {
	t.Helper()
	if err := conn.QueryRow(`SELECT status, COALESCE(url,'') FROM workspaces WHERE id = $1`, id).
		Scan(&status, &url); err != nil {
		t.Fatalf("read workspace row: %v", err)
	}
	return status, url
}

// TestIntegration_ManualRestart_DeclinedPrewarmLeavesTheWorkspaceRoutable is
// finding 2.
//
// The manual restart handler marks the row provisioning BEFORE it knows whether
// a stop will happen. That is survivable for status — the heartbeat rewrites it
// — but NOT for url, which nothing on the heartbeat path ever restores. The rule
// this pins: a workspace whose container was never stopped must still be
// reachable at the url it was reachable at before.
func TestIntegration_ManualRestart_DeclinedPrewarmLeavesTheWorkspaceRoutable(t *testing.T) {
	conn := restartDeclineDB(t)
	setupTestRedis(t)
	id, url := seedRoutableWorkspace(t, conn, "decline-routable")

	// Exactly what the HTTP handler writes before dispatching.
	markProvisioningForRestart(context.Background(), id)

	stub := &prewarmCPProv{ensureErr: errors.New("manifest unknown: sha256:93dfaf12")}
	h := &WorkspaceHandler{cpProv: stub, broadcaster: newTestBroadcaster()}

	if h.RestartWorkspaceAutoOpts(context.Background(), id, "", nil,
		models.CreateWorkspacePayload{Name: "decline-routable", Runtime: "hermes"}, false) {
		t.Fatal("precondition: an unobtainable image must decline this restart")
	}
	h.waitAsyncForTest()

	for _, c := range stub.calls {
		if c == "Stop" || c == "StopAndPrune" {
			t.Fatalf("precondition: the container must NOT have been stopped; calls=%v", stub.calls)
		}
	}

	_, gotURL := declineWorkspaceRow(t, conn, id)
	if gotURL != url {
		t.Fatalf("the declined restart left url=%q (was %q). Nothing stopped the container, so it is "+
			"still serving on that address — but the heartbeat writes STATUS only, so the workspace "+
			"flips back to 'online' with no url at all: healthy-looking and unroutable.", gotURL, url)
	}
}

// TestIntegration_AutoRestart_DeclineIsTerminalAndVisible is finding 3.
//
// A declined auto-restart is not "nothing happened": the platform tried to
// recover a workspace and could not. Without a durable, operator-visible
// outcome the only trace is a log line on a box nobody is tailing, and the
// state is indistinguishable from a restart that was never attempted.
//
// The signal is deliberately NOT status='failed': the container is still up and
// still serving, and marking a live box failed is the same class of lie as
// finding 2's empty url. It is a durable event plus the operator-visible error
// column the canvas already renders.
func TestIntegration_AutoRestart_DeclineIsTerminalAndVisible(t *testing.T) {
	conn := restartDeclineDB(t)
	setupTestRedis(t) // RecordAndBroadcast publishes to Redis before it reaches the hub
	id, url := seedRoutableWorkspace(t, conn, "decline-visible")

	h := &WorkspaceHandler{
		cpProv:      &prewarmCPProv{ensureErr: errors.New("no space left on device")},
		broadcaster: newTestBroadcaster(),
	}
	h.markRestartDeclined(context.Background(), id, "decline-visible", "hermes", "hermes")

	var lastErr sql.NullString
	if err := conn.QueryRow(`SELECT last_sample_error FROM workspaces WHERE id = $1`, id).Scan(&lastErr); err != nil {
		t.Fatalf("read last_sample_error: %v", err)
	}
	if !lastErr.Valid || lastErr.String == "" {
		t.Fatal("a declined restart must leave an operator-visible reason on the row — otherwise an " +
			"unrecoverable restart looks exactly like one that was never attempted")
	}

	var n int
	if err := conn.QueryRow(
		`SELECT count(*) FROM structure_events WHERE workspace_id = $1 AND event_type = $2`,
		id, string(events.EventWorkspaceRestartDeclined)).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly one durable %s event, got %d — the decision has to survive the "+
			"process that made it", events.EventWorkspaceRestartDeclined, n)
	}

	// The workspace is STILL UP. A terminal signal must not cost it its routing
	// or claim a health state it is not in.
	status, gotURL := declineWorkspaceRow(t, conn, id)
	if gotURL != url {
		t.Errorf("recording the decline cleared the url (%q, was %q) — the container is still serving there", gotURL, url)
	}
	if status == string(models.StatusFailed) {
		t.Errorf("status=failed misreports a workspace whose container is still running and heartbeating")
	}
}

// TestIntegration_ManualRestart_AllowedPrewarmDoesClearTheURL is the RED CONTROL
// for the test above.
//
// Same handler, same helper, only the pre-flight outcome differs. Without it,
// "url survived a declined restart" would also pass if the url were simply never
// cleared at all — which would leave callers routing at a container that no
// longer exists, the failure mode core#3220 fixed. The url must survive a
// refusal AND still disappear the moment a stop is really issued.
func TestIntegration_ManualRestart_AllowedPrewarmDoesClearTheURL(t *testing.T) {
	conn := restartDeclineDB(t)
	setupTestRedis(t)
	id, url := seedRoutableWorkspace(t, conn, "allow-clears-url")

	markProvisioningForRestart(context.Background(), id)
	if _, gotURL := declineWorkspaceRow(t, conn, id); gotURL != url {
		t.Fatalf("precondition: marking a restart provisioning must not touch url; got %q", gotURL)
	}

	stub := &prewarmCPProv{ensureRes: provisioner.EnsureImageResult{Status: "ready"}}
	h := &WorkspaceHandler{cpProv: stub, broadcaster: newTestBroadcaster()}

	if !h.RestartWorkspaceAutoOpts(context.Background(), id, "", nil,
		models.CreateWorkspacePayload{Name: "allow-clears-url", Runtime: "hermes"}, false) {
		t.Fatal("precondition: a confirmed image must allow this restart")
	}
	h.waitAsyncForTest()

	if _, gotURL := declineWorkspaceRow(t, conn, id); gotURL != "" {
		t.Fatalf("the container WAS stopped but url is still %q — callers keep routing at a "+
			"destroyed container (core#3220)", gotURL)
	}
}

// declineRecord reads the operator-visible evidence a declined restart is
// required to leave: the reason on the row and the durable event.
func declineRecord(t *testing.T, conn *sql.DB, id string) (reason string, events_ int) {
	t.Helper()
	var lastErr sql.NullString
	if err := conn.QueryRow(`SELECT last_sample_error FROM workspaces WHERE id = $1`, id).Scan(&lastErr); err != nil {
		t.Fatalf("read last_sample_error: %v", err)
	}
	if err := conn.QueryRow(
		`SELECT count(*) FROM structure_events WHERE workspace_id = $1 AND event_type = $2`,
		id, string(events.EventWorkspaceRestartDeclined)).Scan(&events_); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return lastErr.String, events_
}

// TestIntegration_AutoRestartCycle_DeclineIsRecordedByTheCycleItself drives the
// PROGRAMMATIC path end to end.
//
// Asserting markRestartDeclined works is not the same claim as asserting the
// restart cycle calls it — and the second is the one that matters, because the
// pre-fix bug was precisely that the cycle returned without recording anything.
// A mutation that deletes the call from runRestartCycle must fail here.
func TestIntegration_AutoRestartCycle_DeclineIsRecordedByTheCycleItself(t *testing.T) {
	conn := restartDeclineDB(t)
	setupTestRedis(t)
	id, url := seedRoutableWorkspace(t, conn, "cycle-decline")

	stub := &prewarmCPProv{ensureErr: errors.New("manifest unknown: sha256:93dfaf12")}
	h := &WorkspaceHandler{cpProv: stub, broadcaster: newTestBroadcaster()}

	h.runRestartCycle(id)
	h.waitAsyncForTest()

	for _, c := range stub.calls {
		if c == "Stop" || c == "StopAndPrune" || c == "Start" {
			t.Fatalf("precondition: an unobtainable image must stop and reprovision NOTHING; calls=%v", stub.calls)
		}
	}

	reason, n := declineRecord(t, conn, id)
	if reason == "" || n != 1 {
		t.Fatalf("the auto-restart cycle declined and left last_sample_error=%q, %d durable event(s). "+
			"An unrecoverable restart with no record is indistinguishable from a restart nobody ran — "+
			"which is exactly how this went unnoticed.", reason, n)
	}

	status, gotURL := declineWorkspaceRow(t, conn, id)
	if gotURL != url {
		t.Errorf("the declined cycle cleared url (%q, was %q) — its container was never stopped", gotURL, url)
	}
	if status == string(models.StatusProvisioning) {
		t.Errorf("status=provisioning after a restart that never started: the row now claims a "+
			"transition that was refused (status=%q)", status)
	}
}

// TestIntegration_ManualRestart_DeclineIsRecordedByTheDispatcher is the same
// claim for the manual entry point, which has its own decline branch.
func TestIntegration_ManualRestart_DeclineIsRecordedByTheDispatcher(t *testing.T) {
	conn := restartDeclineDB(t)
	setupTestRedis(t)
	id, _ := seedRoutableWorkspace(t, conn, "dispatch-decline")

	h := &WorkspaceHandler{
		cpProv:      &prewarmCPProv{ensureErr: errors.New("no space left on device")},
		broadcaster: newTestBroadcaster(),
	}
	if h.RestartWorkspaceAutoOpts(context.Background(), id, "", nil,
		models.CreateWorkspacePayload{Name: "dispatch-decline", Runtime: "hermes"}, false) {
		t.Fatal("precondition: the declined restart must report that nothing was dispatched")
	}
	h.waitAsyncForTest()

	reason, n := declineRecord(t, conn, id)
	if reason == "" || n != 1 {
		t.Fatalf("the manual restart path declined and left last_sample_error=%q, %d durable event(s) — "+
			"the operator who clicked Restart gets a 200 and no explanation anywhere", reason, n)
	}
}

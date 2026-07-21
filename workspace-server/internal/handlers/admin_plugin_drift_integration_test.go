//go:build integration
// +build integration

// admin_plugin_drift_integration_test.go — REAL Postgres gate for the
// plugin-auto-update (Schedule B) Apply path and its SELF-BRICK guard.
//
// Run with:
//
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run TestIntegration_AdminPluginDrift -v
//
// CI: piggybacks on the handlers-postgres-integration workflow — its path
// filter includes workspace-server/internal/handlers/** and migrations/**,
// and its runner selects tests with -run ^TestIntegration_.
//
// Why this is NOT (only) a sqlmock test
// -------------------------------------
// The self-brick guard (applyRestartAfterDrift ->
// platformConciergeReconcileShouldSkipRestart) branches on the LIVE
// workspaces.kind/status row: platform+online => DEFER restart (return false),
// anything else => DISPATCH restart (return true). The sqlmock sibling
// (admin_plugin_drift_test.go) can only prove "a SELECT with the right shape
// fired against a canned row" — it cannot prove the branch reads the ACTUAL
// row state written to the real schema, nor that the Apply handler transitions
// the queue row to 'applied' against the real plugin_update_queue table with
// its CHECK constraint + partial unique index. Schedule B (the concierge's
// nightly plugin-auto-update cron) can enqueue drift for its OWN workspace and
// drive Apply against itself, so the guard is what stops a bad ref from
// rebooting the org-root concierge into a brick mid-batch. This test moves that
// proof onto the real schema.
//
// NON-VACUITY / negative control (guard leg)
// ------------------------------------------
// If the guard branch were removed from applyRestartAfterDrift (i.e. it
// restarted UNCONDITIONALLY, the pre-fix behavior), the platform/online
// sub-case below would observe restarting==true and spy.calls==[wsID], failing
// both assertions. The non-concierge sub-case is the opposite negative control:
// if the guard OVER-fired and suppressed ordinary restarts, restarting would be
// false and spy empty, failing there. The two sub-cases together pin the branch
// to exactly one side each — neither passes vacuously.
//
// NOT SAFE FOR t.Parallel() — these tests clear + seed shared tables and rebind
// the process-global db.DB.

package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// integrationDB_AdminPluginDrift opens the integration Postgres, rebinds the
// process-global db.DB (the connection every drift/tracking query reads through)
// to it, and clears the slate. Restores the previous db.DB and closes on
// cleanup.
//
// The slate-clear mirrors platform_agent_ensure_integration_test.go: the
// partial unique index uniq_workspaces_one_platform_root forbids a SECOND
// parentless platform row, so a leaked platform root from a predecessor suite
// would make our concierge INSERT collide. Because the handler under test reads
// the global db.DB (not a tx), we cannot isolate inside a rolled-back
// transaction the way the enum-safety test does — so we delete the relevant
// rows directly. Safe because the handlers integration suite runs
// sequentially (single DB, no t.Parallel), and children (workspace_plugins,
// plugin_update_queue) cascade-delete via ON DELETE CASCADE.
func integrationDB_AdminPluginDrift(t *testing.T) *sql.DB {
	t.Helper()
	url := requireIntegrationDBURL(t)
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}

	clear := func() {
		// Remove our own test rows (by name prefix) and any platform root that
		// would collide with the concierge INSERT. plugin_update_queue and
		// workspace_plugins cascade off workspaces.
		if _, err := conn.ExecContext(context.Background(), `
			DELETE FROM workspaces WHERE name LIKE 'itest-drift-%';
			DELETE FROM workspaces WHERE kind = 'platform';
		`); err != nil {
			t.Fatalf("clear slate: %v", err)
		}
	}
	clear()

	prev := db.DB
	db.DB = conn
	t.Cleanup(func() {
		clear()
		db.DB = prev
		conn.Close()
	})
	return conn
}

// seedWorkspaceForDrift inserts a workspace with an explicit kind/status and
// returns its id. parent_id is NULL (required for kind='platform' by the
// workspaces_platform_root_check constraint; harmless for kind='workspace').
func seedWorkspaceForDrift(t *testing.T, conn *sql.DB, name, kind, status string) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO workspaces (id, name, kind, tier, runtime, status, parent_id)
		VALUES ($1, $2, $3, 0, 'claude-code', $4, NULL)
	`, id, name, kind, status); err != nil {
		t.Fatalf("seed workspace (%s/%s): %v", kind, status, err)
	}
	return id
}

// TestIntegration_AdminPluginDrift_SelfBrickGuard is the GUARD LEG: it drives
// applyRestartAfterDrift against REAL workspaces rows and proves the branch
// reads the live kind/status.
//
//   - platform + online  => restarting=false, restart NOT dispatched (defer:
//     the self-brick guard — the concierge must not auto-reboot itself).
//   - workspace + online => restarting=true, restart dispatched exactly once
//     (auto-apply preserved for ordinary tenants).
//
// See the file header for the non-vacuity / negative-control argument: dropping
// the platformConciergeReconcileShouldSkipRestart branch flips the first
// sub-case to (true, [wsID]) and fails it.
func TestIntegration_AdminPluginDrift_SelfBrickGuard(t *testing.T) {
	conn := integrationDB_AdminPluginDrift(t)
	ctx := context.Background()

	conciergeID := seedWorkspaceForDrift(t, conn, "itest-drift-concierge", "platform", "online")
	tenantID := seedWorkspaceForDrift(t, conn, "itest-drift-tenant", "workspace", "online")

	// --- concierge (platform/online): restart DEFERRED ---
	conciergeSpy := &restartSpy{}
	hConcierge := NewAdminPluginDriftHandler(NewPluginsHandler(t.TempDir(), nil, conciergeSpy.fn))

	restarting := hConcierge.applyRestartAfterDrift(ctx, conciergeID)
	waitGlobalAsyncForTest() // drain any detached restart (there should be none)

	if restarting {
		t.Errorf("platform concierge: expected restarting=false (self-brick guard), got true")
	}
	if calls := conciergeSpy.snapshot(); len(calls) != 0 {
		t.Errorf("platform concierge: expected NO restart dispatched, got %v", calls)
	}

	// --- non-concierge (workspace/online): restart DISPATCHED once ---
	tenantSpy := &restartSpy{}
	hTenant := NewAdminPluginDriftHandler(NewPluginsHandler(t.TempDir(), nil, tenantSpy.fn))

	restarting = hTenant.applyRestartAfterDrift(ctx, tenantID)
	waitGlobalAsyncForTest() // the restart runs on a detached globalGoAsync goroutine

	if !restarting {
		t.Errorf("tenant workspace: expected restarting=true (auto-apply preserved), got false")
	}
	if calls := tenantSpy.snapshot(); len(calls) != 1 || calls[0] != tenantID {
		t.Errorf("tenant workspace: expected exactly one restart of %q, got %v", tenantID, calls)
	}
}

// writeLocalPluginFixture writes a minimal local:// plugin tree under base and
// returns the source string ("local://<name>"). The LocalResolver copies this
// tree with NO network — the whole reason the full Apply handler is drivable in
// this network-free lane.
func writeLocalPluginFixture(t *testing.T, base, name string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	// version + description are required by the plugin-manifest SSOT schema
	// (core#3383) — the Apply pipeline fail-closes with a 422 without them.
	manifest := "name: " + name + "\nversion: 1.0.0\ndescription: apply-path integration fixture\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write plugin.yaml: %v", err)
	}
	return "local://" + name
}

// seedPluginDriftRows seeds the workspace_plugins install record + a pending
// plugin_update_queue row for a workspace and returns the queue id. installedSHA
// is the "old" SHA the Apply re-pin is expected to overwrite.
func seedPluginDriftRows(t *testing.T, conn *sql.DB, wsID, pluginName, sourceRaw, trackedRef, installedSHA string) string {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO workspace_plugins (workspace_id, plugin_name, source_raw, tracked_ref, installed_sha)
		VALUES ($1, $2, $3, $4, $5)
	`, wsID, pluginName, sourceRaw, trackedRef, installedSHA); err != nil {
		t.Fatalf("seed workspace_plugins: %v", err)
	}
	queueID := uuid.New().String()
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO plugin_update_queue (id, workspace_id, plugin_name, tracked_ref, current_sha, latest_sha, status)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending')
	`, queueID, wsID, pluginName, trackedRef, installedSHA, "newsha-upstream"); err != nil {
		t.Fatalf("seed plugin_update_queue: %v", err)
	}
	return queueID
}

// applyDrift invokes the Apply handler against a queue id and returns the
// recorder + decoded JSON body.
func applyDrift(t *testing.T, h *AdminPluginDriftHandler, queueID string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/plugin-updates/"+queueID+"/apply", nil)
	c.Params = gin.Params{{Key: "id", Value: queueID}}
	h.Apply(c)
	waitGlobalAsyncForTest() // drain the detached restart goroutine, if any

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode Apply body (code=%d): %v; raw=%s", w.Code, err, w.Body.String())
	}
	return w, body
}

// TestIntegration_AdminPluginDrift_ApplyEndToEnd is the MUTATION LEG: it drives
// the FULL Apply handler (resolve -> stage -> deliver-by-pull -> re-pin -> mark
// applied -> restart-decision) against a seeded pending queue row, using a
// network-free local:// fixture, and asserts the real-schema mutations.
//
// Two sub-cases, distinguished ONLY by the target workspace kind, prove the
// self-brick guard end-to-end through the handler (not just the isolated
// applyRestartAfterDrift):
//
//   - tenant (workspace/online): queue row -> 'applied', installed_sha re-pinned
//     (overwritten from the seeded old SHA), response restarting=true, restart
//     dispatched once. Auto-apply intact.
//   - concierge (platform/online): queue row STILL -> 'applied' (the update IS
//     recorded), but response restarting=false and NO restart dispatched — the
//     Schedule-B self-brick scenario: the concierge re-pins its own plugin
//     without rebooting itself into a possible brick.
//
// deliver-by-pull note: with a nil docker client and no instance-id lookup,
// DeliverForApply returns errNoPushTarget, which Apply treats as the docker-less
// "re-materialize on restart" path and proceeds — no container/network needed.
func TestIntegration_AdminPluginDrift_ApplyEndToEnd(t *testing.T) {
	conn := integrationDB_AdminPluginDrift(t)
	ctx := context.Background()
	base := t.TempDir()
	source := writeLocalPluginFixture(t, base, "demo")

	t.Run("tenant restarts and applies", func(t *testing.T) {
		wsID := seedWorkspaceForDrift(t, conn, "itest-drift-apply-tenant", "workspace", "online")
		queueID := seedPluginDriftRows(t, conn, wsID, "demo", source, "tag:v1.0.0", "oldsha-installed")

		spy := &restartSpy{}
		h := NewAdminPluginDriftHandler(NewPluginsHandler(base, nil, spy.fn))
		w, body := applyDrift(t, h, queueID)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if body["status"] != "applied" {
			t.Errorf("response status = %v, want \"applied\"", body["status"])
		}
		if body["restarting"] != true {
			t.Errorf("response restarting = %v, want true (auto-apply preserved)", body["restarting"])
		}
		// Real-schema mutation 1: queue row transitioned to 'applied'.
		assertQueueStatus(t, conn, queueID, "applied")
		// Real-schema mutation 2: installed_sha was re-pinned — overwritten from
		// the seeded old SHA. local:// sources carry no upstream SHA, so the new
		// value is the empty string; the point is that the re-pin WRITE fired
		// (oldsha-installed -> "").
		if got := readInstalledSHA(t, conn, wsID, "demo"); got == "oldsha-installed" {
			t.Errorf("installed_sha still %q — re-pin write did not fire", got)
		}
		// Restart dispatched exactly once for the tenant.
		if calls := spy.snapshot(); len(calls) != 1 || calls[0] != wsID {
			t.Errorf("expected one restart of %q, got %v", wsID, calls)
		}
	})

	t.Run("concierge applies but defers restart", func(t *testing.T) {
		// Clear any platform root the tenant sub-case did not create but a prior
		// run might have; the helper's cleanup runs only at the end of the outer
		// test, so guard the single-platform-root index here too.
		if _, err := conn.ExecContext(ctx, `DELETE FROM workspaces WHERE kind = 'platform'`); err != nil {
			t.Fatalf("clear platform rows: %v", err)
		}
		wsID := seedWorkspaceForDrift(t, conn, "itest-drift-apply-concierge", "platform", "online")
		queueID := seedPluginDriftRows(t, conn, wsID, "demo", source, "tag:v1.0.0", "oldsha-installed")

		spy := &restartSpy{}
		h := NewAdminPluginDriftHandler(NewPluginsHandler(base, nil, spy.fn))
		w, body := applyDrift(t, h, queueID)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if body["status"] != "applied" {
			t.Errorf("response status = %v, want \"applied\"", body["status"])
		}
		// The self-brick guard: the update IS applied, but the concierge is NOT
		// restarted.
		if body["restarting"] != false {
			t.Errorf("response restarting = %v, want false (self-brick guard)", body["restarting"])
		}
		assertQueueStatus(t, conn, queueID, "applied")
		if calls := spy.snapshot(); len(calls) != 0 {
			t.Errorf("expected NO restart of the concierge, got %v", calls)
		}
	})
}

func assertQueueStatus(t *testing.T, conn *sql.DB, queueID, want string) {
	t.Helper()
	var got string
	if err := conn.QueryRowContext(context.Background(),
		`SELECT status FROM plugin_update_queue WHERE id = $1`, queueID).Scan(&got); err != nil {
		t.Fatalf("read queue status: %v", err)
	}
	if got != want {
		t.Errorf("plugin_update_queue.status = %q, want %q", got, want)
	}
}

func readInstalledSHA(t *testing.T, conn *sql.DB, wsID, pluginName string) string {
	t.Helper()
	var sha sql.NullString
	if err := conn.QueryRowContext(context.Background(),
		`SELECT installed_sha FROM workspace_plugins WHERE workspace_id = $1 AND plugin_name = $2`,
		wsID, pluginName).Scan(&sha); err != nil {
		t.Fatalf("read installed_sha: %v", err)
	}
	return sha.String
}

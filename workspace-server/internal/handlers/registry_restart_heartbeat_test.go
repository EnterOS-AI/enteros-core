package handlers

// registry_restart_heartbeat_test.go — a heartbeat from the pod a restart is
// stopping must not promote the workspace out of 'provisioning'
// (2026-11-18). The restart clears workspaces.url and sets 'provisioning';
// the control plane's stop returns while the old pod still runs (Kubernetes:
// its termination grace period), so that pod can beat once more. Before the
// fix its beat flipped the row to 'online' with no URL: every caller that
// waits for online (a Light park, a schedule create) then acted on an agent
// that was not there. Only a row with a URL (written by the new pod's
// registration) is promoted, both by the inline CASE and by evaluateStatus's
// fallback.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// heartbeatPromoteGuardRe is the inline CASE of the heartbeat UPDATE: only a
// provisioning row with a URL becomes online.
const heartbeatPromoteGuardRe = `UPDATE workspaces SET[\s\S]*status\s+= \(CASE WHEN status = 'provisioning' AND kind <> 'platform' AND COALESCE\(url, ''\) <> '' THEN 'online' ELSE status END\)::workspace_status`

// heartbeatFallbackPromoteRe is evaluateStatus's provisioning fallback.
const heartbeatFallbackPromoteRe = `UPDATE workspaces SET status = \$1::workspace_status, updated_at = now\(\) WHERE id = \$2 AND status = 'provisioning' AND COALESCE\(url, ''\) <> ''`

func postHeartbeat(t *testing.T, h *RegistryHandler, workspaceID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"` + workspaceID + `","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":412}`
	c.Request = httptest.NewRequest(http.MethodPost, "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Heartbeat(c)
	return w
}

func TestHeartbeat_PromotesAProvisioningRowOnlyWithARegisteredURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		// promoted is what the fallback UPDATE (guarded on the URL) changed:
		// 0 for a row whose restart cleared the URL, 1 for a registered one.
		promoted  int64
		broadcast bool
	}{
		{"the pod a restart is stopping (no URL): stays provisioning, no WORKSPACE_ONLINE", 0, false},
		{"a registered pod (URL): online, WORKSPACE_ONLINE", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			h := NewRegistryHandler(newTestBroadcaster())
			const ws = "ws-restarting"

			mock.ExpectQuery(`SELECT COALESCE\(current_task, ''\), COALESCE\(monthly_spend, 0\), status, desired_generation FROM workspaces`).
				WithArgs(ws).
				WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status", "desired_generation"}).AddRow("", 0, "provisioning", int64(0)))
			mock.ExpectExec(heartbeatPromoteGuardRe).
				WithArgs(ws, 0.0, "", 0, 412, "", nil).
				WillReturnResult(sqlmock.NewResult(0, 1))
			// The row as evaluateStatus reads it: the guarded CASE left it
			// provisioning (sqlmock cannot run the CASE; the fallback below is
			// where its outcome is scripted).
			mock.ExpectQuery(`SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =`).
				WithArgs(ws).
				WillReturnRows(sqlmock.NewRows([]string{"status", "kind", "last_register_failure_at", "mcp_unloaded_since"}).AddRow("provisioning", "workspace", nil, nil))
			mock.ExpectExec(heartbeatFallbackPromoteRe).
				WithArgs("online", ws).
				WillReturnResult(sqlmock.NewResult(0, tc.promoted))
			if tc.broadcast {
				mock.ExpectExec(`INSERT INTO structure_events`).WillReturnResult(sqlmock.NewResult(0, 1))
			}
			rec := recordSQL(t)

			if w := postHeartbeat(t, h, ws); w.Code != http.StatusOK {
				t.Fatalf("heartbeat = %d %s", w.Code, w.Body.String())
			}
			online := false
			for _, q := range rec() {
				if strings.Contains(q, "INSERT INTO structure_events") {
					online = true
				}
			}
			if online != tc.broadcast {
				t.Errorf("WORKSPACE_ONLINE recorded = %v, want %v", online, tc.broadcast)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet (the promotion must be guarded on the URL): %v", err)
			}
		})
	}
}

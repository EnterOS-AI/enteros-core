//go:build e2e_busy_inject

package handlers

import (
	"net/http"
	"sync"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

// ── TEST-ONLY busy-inject (build tag e2e_busy_inject) ────────────────────────
//
// Compiled ONLY into the ephemeral gate's throwaway tenant image (built with
// `--build-arg BUILD_TAGS=e2e_busy_inject`). NEVER into a production/staging
// image. See testbusy.go for the full safety rationale.
//
// It exposes POST /workspaces/:id/test-busy (under WorkspaceAuth) which pins a
// workspace's active_tasks to a caller-chosen floor so the E2E can make a
// workspace deterministically report BUSY without a real LLM turn — and holds
// that floor across the ~30s agent heartbeat (which would otherwise clobber
// active_tasks back to the runtime-reported 0) until the caller explicitly
// releases it or a safety TTL expires.

// testBusyTTL bounds how long an injected floor survives without a refresh, so a
// crashed/aborted test can never strand a workspace as permanently-busy. The E2E
// releases explicitly (active_tasks:0) after its force-on-busy assertions; the
// TTL is pure defense-in-depth.
const testBusyTTL = 5 * time.Minute

type testBusyHold struct {
	value   int
	expires time.Time
}

var (
	testBusyMu    sync.Mutex
	testBusyFloor = map[string]testBusyHold{}
)

func init() {
	// Arm the heartbeat seam declared in testbusy.go. With this assigned, the
	// heartbeat handler raises the persisted active_tasks to the injected floor
	// (if any, unexpired) instead of the runtime-reported value.
	testBusyActiveTasksHook = func(workspaceID string, reported int) int {
		testBusyMu.Lock()
		defer testBusyMu.Unlock()
		h, ok := testBusyFloor[workspaceID]
		if !ok {
			return reported
		}
		if time.Now().After(h.expires) {
			delete(testBusyFloor, workspaceID)
			return reported
		}
		if h.value > reported {
			return h.value
		}
		return reported
	}
}

func setTestBusyFloor(workspaceID string, value int) {
	testBusyMu.Lock()
	defer testBusyMu.Unlock()
	if value <= 0 {
		delete(testBusyFloor, workspaceID)
		return
	}
	testBusyFloor[workspaceID] = testBusyHold{value: value, expires: time.Now().Add(testBusyTTL)}
}

// RegisterTestBusyRoutes wires the test-busy endpoint onto the (already
// WorkspaceAuth-gated) group it is handed. WorkspaceAuth is the request-time
// authorization: the caller must present a valid bearer for THIS workspace, so
// the route cannot be driven cross-workspace even inside the tag-built image.
func RegisterTestBusyRoutes(rg gin.IRoutes) {
	rg.POST("/test-busy", handleTestBusy)
}

type testBusyRequest struct {
	// ActiveTasks is the floor to pin (>=1 to make the workspace busy; 0 or
	// negative RELEASES a previously-injected hold and zeroes the DB column).
	ActiveTasks int `json:"active_tasks"`
	// IsBusy optionally overrides the is_busy column; when omitted it derives
	// from active_tasks>0, matching the heartbeat's COALESCE fallback.
	IsBusy *bool `json:"is_busy"`
}

func handleTestBusy(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()

	var req testBusyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// Tolerate an empty body → default to a single active task (busy).
		req = testBusyRequest{ActiveTasks: 1}
	}

	value := req.ActiveTasks
	if value < 0 {
		value = 0
	}
	busy := value > 0
	if req.IsBusy != nil {
		busy = *req.IsBusy
	}

	// Persist immediately so a GET right after this call already reflects the
	// state (the DB is the same column the hibernate handler and GET read).
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET active_tasks = $2, is_busy = $3, updated_at = now() WHERE id = $1 AND status != 'removed'`,
		id, value, busy); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "test-busy: db update failed"})
		return
	}

	// Install/refresh (or release) the sticky floor so subsequent heartbeats do
	// not clobber the injected value back to the runtime-reported 0.
	setTestBusyFloor(id, value)

	c.JSON(http.StatusOK, gin.H{
		"workspace_id": id,
		"active_tasks": value,
		"is_busy":      busy,
		"held":         value > 0,
	})
}

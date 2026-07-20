//go:build !e2e_busy_inject

package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestTestBusy_ProductionInert is the NEGATIVE CONTROL for the test-only
// busy-inject (task #92). It runs on the DEFAULT build (no e2e_busy_inject tag)
// — i.e. exactly what ships — and proves the lever is completely inert there:
//
//  1. testBusyActiveTasksHook is nil, so the heartbeat handler persists the
//     runtime-reported active_tasks verbatim (behaviour-identical to before).
//  2. RegisterTestBusyRoutes registers NOTHING, so POST .../test-busy 404s like
//     any unknown path — there is no way to inject busy state in production.
func TestTestBusy_ProductionInert(t *testing.T) {
	if testBusyActiveTasksHook != nil {
		t.Fatalf("testBusyActiveTasksHook must be nil in a production (untagged) build; the heartbeat active_tasks value must be the runtime-reported one, not an injected floor")
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/workspaces/:id")
	RegisterTestBusyRoutes(grp)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-abc/test-busy", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("test-busy route must NOT exist in a production build: expected 404, got %d", rec.Code)
	}
}

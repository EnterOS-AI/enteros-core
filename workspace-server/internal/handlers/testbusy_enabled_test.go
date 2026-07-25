//go:build e2e_busy_inject

package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestTestBusy_HookArmed proves that UNDER the e2e_busy_inject tag the heartbeat
// seam is armed and the sticky floor behaves: it raises a reported count up to
// the injected floor, never lowers a higher reported count, honours an explicit
// release, and expires. This is the "positive control" paired with the negative
// control in testbusy_disabled_test.go.
func TestTestBusy_HookArmed(t *testing.T) {
	if testBusyActiveTasksHook == nil {
		t.Fatal("testBusyActiveTasksHook must be armed under the e2e_busy_inject build tag")
	}
	const ws = "ws-busy-1"
	// No floor yet → reported passes through unchanged.
	if got := testBusyActiveTasksHook(ws, 0); got != 0 {
		t.Fatalf("no floor: expected passthrough 0, got %d", got)
	}
	// Inject floor=2 → a reported 0 is raised to 2.
	setTestBusyFloor(ws, 2)
	if got := testBusyActiveTasksHook(ws, 0); got != 2 {
		t.Fatalf("floor=2, reported=0: expected 2, got %d", got)
	}
	// A genuinely higher reported count wins (we floor, never cap).
	if got := testBusyActiveTasksHook(ws, 5); got != 5 {
		t.Fatalf("floor=2, reported=5: expected 5, got %d", got)
	}
	// Release → passthrough again.
	setTestBusyFloor(ws, 0)
	if got := testBusyActiveTasksHook(ws, 0); got != 0 {
		t.Fatalf("after release: expected passthrough 0, got %d", got)
	}
	// Expiry: install an already-expired hold and confirm it is ignored + purged.
	testBusyMu.Lock()
	testBusyFloor[ws] = testBusyHold{value: 9, expires: time.Now().Add(-time.Second)}
	testBusyMu.Unlock()
	if got := testBusyActiveTasksHook(ws, 1); got != 1 {
		t.Fatalf("expired floor must be ignored: expected 1, got %d", got)
	}
	testBusyMu.Lock()
	_, still := testBusyFloor[ws]
	testBusyMu.Unlock()
	if still {
		t.Fatal("expired floor should have been purged on read")
	}
}

// TestTestBusy_RouteRegistered proves the route is wired under the tag (contrast
// with the 404 the untagged build asserts). We only check registration, not the
// handler body, to avoid a DB dependency.
func TestTestBusy_RouteRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/workspaces/:id")
	RegisterTestBusyRoutes(grp)

	found := false
	for _, ri := range r.Routes() {
		if ri.Method == http.MethodPost && ri.Path == "/workspaces/:id/test-busy" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("POST /workspaces/:id/test-busy must be registered under the e2e_busy_inject tag")
	}
	// Sanity: the path resolves to a handler (not 404). It will 500 without a DB,
	// but crucially NOT 404 — proving the route exists.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-x/test-busy", nil)
	func() {
		defer func() { _ = recover() }() // handler touches db.DB (nil in unit test)
		r.ServeHTTP(rec, req)
	}()
	if rec.Code == http.StatusNotFound {
		t.Fatalf("route resolved to 404 under the tag; expected it to reach the handler")
	}
}

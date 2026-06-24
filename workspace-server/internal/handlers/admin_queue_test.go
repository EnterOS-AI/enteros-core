package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestNewAdminQueueHandler_Constructor verifies the constructor returns a
// non-nil handler with the expected zero-state fields.
func TestNewAdminQueueHandler_Constructor(t *testing.T) {
	h := NewAdminQueueHandler()
	if h == nil {
		t.Fatal("NewAdminQueueHandler returned nil")
	}
	// Zero-value struct fields must not panic on use.
	_ = h.DropStale
}

// TestDropStale_InvalidMaxAge verifies the handler rejects non-positive
// max_age_minutes values.
func TestDropStale_InvalidMaxAge(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	h := NewAdminQueueHandler()

	for _, tc := range []struct {
		name     string
		maxAge   string
		wantCode int
	}{
		{"zero", "0", http.StatusBadRequest},
		{"negative", "-5", http.StatusBadRequest},
		{"non-integer", "abc", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/admin/a2a-queue/drop-stale?max_age_minutes="+tc.maxAge, nil)
			h.DropStale(c)
			if w.Code != tc.wantCode {
				t.Errorf("max_age_minutes=%s: got %d, want %d", tc.maxAge, w.Code, tc.wantCode)
			}
		})
	}
}

// TestDropStale_Success verifies the handler calls dropStaleItems with the
// correct parsed TTL and workspace_id, and returns the dropped count.
func TestDropStale_Success(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	h := NewAdminQueueHandler()

	var capturedWorkspaceID string
	var capturedMaxAge int
	prev := dropStaleItems
	dropStaleItems = func(ctx context.Context, workspaceID string, maxAgeMinutes int) (int, error) {
		capturedWorkspaceID = workspaceID
		capturedMaxAge = maxAgeMinutes
		return 3, nil
	}
	defer func() { dropStaleItems = prev }()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/a2a-queue/drop-stale?max_age_minutes=30&workspace_id=ws-abc", nil)
	h.DropStale(c)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	if capturedWorkspaceID != "ws-abc" {
		t.Errorf("workspace_id = %q; want ws-abc", capturedWorkspaceID)
	}
	if capturedMaxAge != 30 {
		t.Errorf("max_age = %d; want 30", capturedMaxAge)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if resp["dropped"] != float64(3) {
		t.Errorf("dropped = %v; want 3", resp["dropped"])
	}
}

// TestDropStale_DBError propagates 500 to the client.
func TestDropStale_DBError(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	h := NewAdminQueueHandler()

	prev := dropStaleItems
	dropStaleItems = func(ctx context.Context, wsID string, maxAge int) (int, error) {
		return 0, sql.ErrConnDone
	}
	defer func() { dropStaleItems = prev }()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/a2a-queue/drop-stale?max_age_minutes=60", nil)
	h.DropStale(c)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("got %d, want 500", w.Code)
	}
}

// TestDropStale_AllWorkspaces verifies an absent workspace_id param results
// in an empty string passed to dropStaleItems (signals "all workspaces").
func TestDropStale_AllWorkspaces(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	h := NewAdminQueueHandler()

	var capturedWorkspaceID string
	prev := dropStaleItems
	dropStaleItems = func(ctx context.Context, workspaceID string, maxAgeMinutes int) (int, error) {
		capturedWorkspaceID = workspaceID
		return 0, nil
	}
	defer func() { dropStaleItems = prev }()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/a2a-queue/drop-stale?max_age_minutes=120", nil)
	h.DropStale(c)

	if capturedWorkspaceID != "" {
		t.Errorf("workspace_id = %q; want empty (all workspaces)", capturedWorkspaceID)
	}
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
}

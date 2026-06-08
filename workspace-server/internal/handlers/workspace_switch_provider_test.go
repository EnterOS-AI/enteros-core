package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestSwitchProvider_StopBeforeProviderWrite is the load-bearing ordering pin
// (RFC #622 Hazard 1). The stop (cpStopWithRetry) MUST appear before the UPDATE
// that writes the new provider — otherwise the stop resolves the new provider
// and deprovisions the old box against the wrong backend, leaking it. A
// source-level position check guards against a refactor reordering the two.
func TestSwitchProvider_StopBeforeProviderWrite(t *testing.T) {
	wd, _ := os.Getwd()
	src, err := os.ReadFile(filepath.Join(wd, "workspace_switch_provider.go"))
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	stripped := stripGoComments(src)
	stopIdx := bytes.Index(stripped, []byte("cpStopWithRetry(ctx, id, \"SwitchProvider\")"))
	if stopIdx < 0 {
		t.Fatal("SwitchProvider must stop the old box via cpStopWithRetry before reprovisioning")
	}
	// the provider write is the jsonb_set on compute -> {provider}
	writeIdx := bytes.Index(stripped, []byte("'{provider}'"))
	if writeIdx < 0 {
		t.Fatal("SwitchProvider must write the new provider via jsonb_set on compute->{provider}")
	}
	if stopIdx >= writeIdx {
		t.Fatalf("ORDERING HAZARD: cpStopWithRetry (idx %d) must come BEFORE the provider write (idx %d) — else the old box is deprovisioned with the new backend and leaks", stopIdx, writeIdx)
	}
	// and the instance_id must be cleared in the same UPDATE (retry-safety)
	if !bytes.Contains(stripped, []byte("instance_id = NULL")) {
		t.Fatal("SwitchProvider must clear instance_id when writing the new provider (retry-safety)")
	}
}

// TestSwitchProvider_RejectsBadProvider: the allowlist check fires before any DB
// access, so a bad/missing provider is a clean 400 without touching the backend.
func TestSwitchProvider_RejectsBadProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &WorkspaceHandler{}
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"provider":"azure"}`, http.StatusBadRequest},
		{`{"provider":""}`, http.StatusBadRequest},
		{`{"provider":"AWS-typo"}`, http.StatusBadRequest},
		{`{}`, http.StatusBadRequest},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
		c.Request = httptest.NewRequest("POST", "/workspaces/ws-1/switch-provider", strings.NewReader(tc.body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.SwitchProvider(c)
		if w.Code != tc.want {
			t.Errorf("body %s: got %d want %d (%s)", tc.body, w.Code, tc.want, w.Body.String())
		}
	}
}

// TestSwitchProvider_RouteRegistered pins the route wiring.
func TestSwitchProvider_RouteRegistered(t *testing.T) {
	wd, _ := os.Getwd()
	src, err := os.ReadFile(filepath.Join(wd, "..", "router", "router.go"))
	if err != nil {
		t.Fatalf("read router: %v", err)
	}
	if !bytes.Contains(src, []byte(`POST("/switch-provider", wh.SwitchProvider)`)) {
		t.Fatal("router must register POST /switch-provider → wh.SwitchProvider")
	}
}

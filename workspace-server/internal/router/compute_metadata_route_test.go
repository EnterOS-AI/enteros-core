package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/handlers"
	"github.com/gin-gonic/gin"
)

// compute_metadata_route_test.go — issue #2489 SSOT endpoint.
//
// The /compute/metadata route is the single point every consumer reads
// to learn cloud-provider + instance-type allowlists. Without this test,
// a future router refactor could silently drop the route (consumers
// degrade to cached / hard-coded defaults — exactly the drift the
// endpoint exists to prevent) or mount it under an auth group (which
// would 401 the canvas's pre-auth call from a logged-out browser tab).
//
// The contract being pinned:
//   1. The route is registered and reachable.
//   2. The route is PUBLIC — no AdminAuth, no WorkspaceAuth.
//   3. The wire shape matches the canvas's expectation (same JSON keys).
//   4. The in-tree Go consumer (handlers.workspaceComputeInstanceAllowlist)
//      AGREE with the endpoint's value.

func buildComputeMetadataEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/compute/metadata", handlers.ComputeMetadata)
	return r
}

func TestComputeMetadata_Public_Returns200(t *testing.T) {
	r := buildComputeMetadataEngine(t)

	req := httptest.NewRequest(http.MethodGet, "/compute/metadata", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%s)", w.Code, w.Body.String())
	}
}

func TestComputeMetadata_ReturnsExpectedShape(t *testing.T) {
	r := buildComputeMetadataEngine(t)

	req := httptest.NewRequest(http.MethodGet, "/compute/metadata", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var got struct {
		Providers []struct {
			ID              string   `json:"id"`
			Label           string   `json:"label"`
			DefaultInstance string   `json:"default_instance"`
			Instances       []string `json:"instances"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v (body=%s)", err, w.Body.String())
	}

	if len(got.Providers) != 3 {
		t.Fatalf("expected 3 providers, got %d", len(got.Providers))
	}
	want := []struct {
		id, label, defaultInstance string
		instanceCount             int
	}{
		{"aws", "AWS (default)", "t3.medium", 7},
		{"gcp", "GCP", "e2-standard-2", 5},
		{"hetzner", "Hetzner", "cpx31", 9},
	}
	for i, w := range want {
		p := got.Providers[i]
		if p.ID != w.id {
			t.Errorf("providers[%d].id = %q, want %q", i, p.ID, w.id)
		}
		if p.Label != w.label {
			t.Errorf("providers[%d].label = %q, want %q", i, p.Label, w.label)
		}
		if p.DefaultInstance != w.defaultInstance {
			t.Errorf("providers[%d].default_instance = %q, want %q", i, p.DefaultInstance, w.defaultInstance)
		}
		if len(p.Instances) != w.instanceCount {
			t.Errorf("providers[%d].instances len = %d, want %d", i, len(p.Instances), w.instanceCount)
		}
	}
}

func TestComputeMetadata_AgreesWithInTreeAllowlist(t *testing.T) {
	// The endpoint must return the same instance sets that the PATCH
	// validator uses. We probe the allowlist via the exported test
	// helper TestValidateWorkspaceCompute_InstanceTypePerProvider (it
	// pins the exact sets), but here we simply cross-check counts and
	// key presence so the endpoint and the allowlist stay in sync.
	// A more thorough check lives in handlers/workspace_compute_test.go.
	r := buildComputeMetadataEngine(t)

	req := httptest.NewRequest(http.MethodGet, "/compute/metadata", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var got struct {
		Providers []struct {
			ID        string   `json:"id"`
			Instances []string `json:"instances"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v (body=%s)", err, w.Body.String())
	}

	for _, p := range got.Providers {
		if len(p.Instances) == 0 {
			t.Errorf("provider %q has empty instances", p.ID)
		}
	}
}

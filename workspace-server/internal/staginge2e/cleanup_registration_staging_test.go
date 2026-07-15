//go:build staging_e2e

package staginge2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkspaceLifecycleRegisterTenantCleanupDeletesExactSlug(t *testing.T) {
	const (
		slug  = "e2e-cleanup-contract"
		token = "test-admin-token"
	)

	type capturedRequest struct {
		method      string
		path        string
		authorize   string
		contentType string
		body        string
	}
	requests := make(chan capturedRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- capturedRequest{
			method:      r.Method,
			path:        r.URL.Path,
			authorize:   r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"),
			body:        string(body),
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := stagingCfg{cpBase: server.URL, adminToken: token}
	if ok := t.Run("registered cleanup", func(t *testing.T) {
		registerTenantCleanup(t, cfg, slug)
	}); !ok {
		t.Fatal("registered cleanup subtest failed")
	}

	var got capturedRequest
	select {
	case got = <-requests:
	default:
		t.Fatal("registered cleanup did not issue a DELETE request")
	}
	if got.method != http.MethodDelete {
		t.Fatalf("cleanup method = %q, want DELETE", got.method)
	}
	if got.path != "/cp/admin/tenants/"+slug {
		t.Fatalf("cleanup path = %q, want exact slug path", got.path)
	}
	if got.authorize != "Bearer "+token {
		t.Fatalf("cleanup authorization header = %q, want test bearer", got.authorize)
	}
	if got.contentType != "application/json" {
		t.Fatalf("cleanup content type = %q, want application/json", got.contentType)
	}
	var confirmation struct {
		Confirm string `json:"confirm"`
	}
	if err := json.Unmarshal([]byte(got.body), &confirmation); err != nil {
		t.Fatalf("decode cleanup confirmation: %v", err)
	}
	if confirmation.Confirm != slug {
		t.Fatalf("cleanup confirmation = %q, want exact slug", confirmation.Confirm)
	}
	select {
	case extra := <-requests:
		t.Fatalf("cleanup issued an unexpected second request: %+v", extra)
	default:
	}
}

package provisioner

// cp_provisioner_kept_state_test.go — EraseKeptState erases what a plain
// delete kept, through the teardown for the instance that delete stopped
// (molecule-core#5193). The row's instance_id is already NULL by then, so the
// call takes the kept id as an argument and must not read it from the row.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEraseKeptState_PrunesTheNamedInstanceWithTheRowsProvider(t *testing.T) {
	// The row's instance_id is gone; a lookup would find nothing.
	primeInstanceIDLookup(t, map[string]string{})
	prev := resolveProvider
	resolveProvider = func(context.Context, string) (string, error) { return "enteros", nil }
	t.Cleanup(func() { resolveProvider = prev })

	var gotMethod, gotPath string
	var gotQuery map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.Query()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"deprovisioned","already":true}`)
	}))
	defer srv.Close()
	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}

	if err := p.EraseKeptState(context.Background(), "ws-kept", "enteros-ws-kept"); err != nil {
		t.Fatalf("EraseKeptState: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/cp/workspaces/ws-kept" {
		t.Errorf("request = %s %s, want DELETE /cp/workspaces/ws-kept", gotMethod, gotPath)
	}
	if got := gotQuery["instance_id"]; len(got) != 1 || got[0] != "enteros-ws-kept" {
		t.Errorf("instance_id = %v, want the kept instance", got)
	}
	if got := gotQuery["prune"]; len(got) != 1 || got[0] != "true" {
		t.Errorf("prune = %v, want true", got)
	}
	if got := gotQuery["provider"]; len(got) != 1 || got[0] != "enteros" {
		t.Errorf("provider = %v, want the row's provider", got)
	}
}

func TestEraseKeptState_EmptyInstanceIsANoOp(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}
	if err := p.EraseKeptState(context.Background(), "ws-kept", " "); err != nil {
		t.Fatalf("EraseKeptState: %v", err)
	}
	if called {
		t.Error("a request was sent for an empty instance id")
	}
}

func TestEraseKeptState_SurfacesARefusal(t *testing.T) {
	prev := resolveProvider
	resolveProvider = func(context.Context, string) (string, error) { return "", nil }
	t.Cleanup(func() { resolveProvider = prev })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"failed to deprovision workspace"}`)
	}))
	defer srv.Close()
	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}
	err := p.EraseKeptState(context.Background(), "ws-kept", "enteros-ws-kept")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want the control plane's 500 so the reaper backs off and retries", err)
	}
}

func TestEraseKeptState_WithoutABackendSaysSo(t *testing.T) {
	var nilProv *CPProvisioner
	if err := nilProv.EraseKeptState(context.Background(), "ws", "i-1"); err != ErrNoBackend {
		t.Errorf("nil provisioner: err = %v, want ErrNoBackend", err)
	}
	if err := (&CPProvisioner{}).EraseKeptState(context.Background(), "ws", "i-1"); err != ErrNoBackend {
		t.Errorf("no http client: err = %v, want ErrNoBackend", err)
	}
}

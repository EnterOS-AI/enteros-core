package provisioner

// cp_provisioner_purged_test.go — StopPurgedInstance tears down an instance
// whose workspace row is gone.
//
// Stop and StopAndPrune read the instance id from the workspace row. A
// workspace purged while its provision was still in flight has no row, so the
// instance that provision created could not be stopped by either: they answer
// "no instance_id on file, nothing to do". The caller holds the instance id
// (Start just returned it), so this call takes it as an argument.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStopPurgedInstance_ErasesTheNamedInstanceWithoutReadingTheRow(t *testing.T) {
	// No row: a lookup would find nothing, and the call must not depend on one.
	primeInstanceIDLookup(t, map[string]string{})

	var gotMethod, gotPath string
	var gotQuery map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.Query()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"deprovisioned"}`)
	}))
	defer srv.Close()
	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}

	if err := p.StopPurgedInstance(context.Background(), "ws-gone", "enteros-ws-gone"); err != nil {
		t.Fatalf("StopPurgedInstance: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/cp/workspaces/ws-gone" {
		t.Errorf("request = %s %s, want DELETE /cp/workspaces/ws-gone", gotMethod, gotPath)
	}
	if got := gotQuery["instance_id"]; len(got) != 1 || got[0] != "enteros-ws-gone" {
		t.Errorf("instance_id = %v, want the instance the caller named", got)
	}
	if got := gotQuery["prune"]; len(got) != 1 || got[0] != "true" {
		t.Errorf("prune = %v, want true: a purged workspace's data can never be attached again", got)
	}
	if _, ok := gotQuery["provider"]; ok {
		t.Errorf("a provider was sent (%v); the row that names it is gone, so the control plane's default decides", gotQuery["provider"])
	}
}

func TestStopPurgedInstance_WithoutAnInstanceIsANoOp(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}

	if err := p.StopPurgedInstance(context.Background(), "ws-gone", "  "); err != nil {
		t.Fatalf("StopPurgedInstance: %v", err)
	}
	if called {
		t.Error("a request was sent for an empty instance id")
	}
}

func TestStopPurgedInstance_SurfacesARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"failed to deprovision workspace"}`)
	}))
	defer srv.Close()
	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", httpClient: srv.Client()}

	err := p.StopPurgedInstance(context.Background(), "ws-gone", "enteros-ws-gone")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want the control plane's 500 so the caller can retry", err)
	}
}

func TestStopPurgedInstance_WithoutABackendSaysSo(t *testing.T) {
	var nilProv *CPProvisioner
	if err := nilProv.StopPurgedInstance(context.Background(), "ws-gone", "i-1"); err != ErrNoBackend {
		t.Errorf("nil provisioner: err = %v, want ErrNoBackend", err)
	}
	if err := (&CPProvisioner{}).StopPurgedInstance(context.Background(), "ws-gone", "i-1"); err != ErrNoBackend {
		t.Errorf("no http client: err = %v, want ErrNoBackend", err)
	}
}

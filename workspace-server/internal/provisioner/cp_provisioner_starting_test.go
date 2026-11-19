package provisioner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestInstanceStarting_ReadsTheControlPlaneState — "provisioning" (a
// Kubernetes Deployment whose pod is not Ready yet) and "pending" (EC2
// launching) are an instance coming up; IsRunning reports both as not
// running, which is why the instance reconciler asks this before it
// restarts an online workspace.
func TestInstanceStarting_ReadsTheControlPlaneState(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{"ws-1": "enteros-ws-0000aaaa0001"})
	cases := map[string]struct{ starting, running bool }{
		"provisioning": {true, false},
		"pending":      {true, false},
		"running":      {false, true},
		"stopped":      {false, false},
		"terminated":   {false, false},
	}
	for state, want := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/cp/workspaces/ws-1/status" {
				t.Errorf("path = %q", r.URL.Path)
			}
			_, _ = io.WriteString(w, `{"state":"`+state+`"}`)
		}))
		p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", sharedSecret: "s", adminToken: "t", httpClient: srv.Client()}
		starting, err := p.InstanceStarting(context.Background(), "ws-1")
		running, rerr := p.IsRunning(context.Background(), "ws-1")
		srv.Close()
		if err != nil || rerr != nil {
			t.Errorf("state=%s: errors %v / %v", state, err, rerr)
			continue
		}
		if starting != want.starting || running != want.running {
			t.Errorf("state=%s: starting=%v running=%v, want %v %v", state, starting, running, want.starting, want.running)
		}
	}
}

// TestInstanceStarting_ErrorsAreNotStarting — a transport or status error is
// (false, err): the reconciler treats it as "leave it alone", like IsRunning's
// (true, err).
func TestInstanceStarting_ErrorsAreNotStarting(t *testing.T) {
	primeInstanceIDLookup(t, map[string]string{"ws-1": "enteros-ws-0000aaaa0001"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	p := &CPProvisioner{baseURL: srv.URL, orgID: "org-1", sharedSecret: "s", adminToken: "t", httpClient: srv.Client()}
	starting, err := p.InstanceStarting(context.Background(), "ws-1")
	if err == nil || starting {
		t.Fatalf("InstanceStarting on a 502 = (%v, %v), want (false, error)", starting, err)
	}
	if running, rerr := p.IsRunning(context.Background(), "ws-1"); rerr == nil || !running {
		t.Fatalf("IsRunning on a 502 = (%v, %v), want the fail-safe (true, error)", running, rerr)
	}
	var nilP *CPProvisioner
	if _, err := nilP.InstanceStarting(context.Background(), "ws-1"); err != ErrNoBackend {
		t.Fatalf("nil provisioner: err = %v, want ErrNoBackend", err)
	}
}

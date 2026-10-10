package provisioner

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// core#5019: a workspace adopting a newly promoted template pin is STOPPED
// before the CP is asked to provision it, and the CP cannot answer until it
// has the pinned image. Those images are ~7GB. Every CP call shared one
// `&http.Client{Timeout: 120 * time.Second}`, so a cold pull blew the deadline
// and the workspace was left with no container at all:
//
//	12:35:30  evt: provision.ec2_stopped          <- container destroyed
//	12:37:30  cp provisioner: send: Post ".../cp/workspaces/provision":
//	          context deadline exceeded (Client.Timeout exceeded while awaiting headers)
//
// Reproduced on an internal staging workspace, then confirmed by fixing only
// that variable: `docker pull` of the pinned digest, identical restart call,
// back in 13 seconds. 120s is a sane ceiling for the small JSON calls
// (status/stop/console) and is simply the wrong budget for an image pull.
//
// The window is widest exactly when the operation is most wanted -- right
// after a promote, when by definition no host has the new image yet.

// TestProvisionUsesItsOwnLongerTimeout pins the DEFAULT, not an injected
// value: constructing through NewCPProvisioner (the production path) must
// yield a provision client whose budget is sized for an image pull, while the
// general client stays short.
func TestProvisionUsesItsOwnLongerTimeout(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "org-1")
	t.Setenv("CP_PROVISION_SHARED_SECRET", "s3cret")

	p, err := NewCPProvisioner()
	if err != nil {
		t.Fatalf("NewCPProvisioner: %v", err)
	}

	if p.httpClient == nil || p.provisionHTTPClient == nil {
		t.Fatal("both the general and provision clients must be constructed")
	}
	if p.provisionHTTPClient.Timeout <= p.httpClient.Timeout {
		t.Errorf("provision timeout (%s) must exceed the general CP timeout (%s); "+
			"sharing one budget is what left a workspace down in core#5019",
			p.provisionHTTPClient.Timeout, p.httpClient.Timeout)
	}
	// A cold ~7GB pull over the tunnel is minutes, not seconds. Pin a floor so
	// the budget cannot be quietly trimmed back toward the general timeout.
	if p.provisionHTTPClient.Timeout < 10*time.Minute {
		t.Errorf("provision timeout = %s, want >= 10m to cover a cold multi-GB image pull",
			p.provisionHTTPClient.Timeout)
	}
	// The short budget still applies to the small JSON calls.
	if p.httpClient.Timeout != cpAPITimeout {
		t.Errorf("general CP timeout = %s, want %s", p.httpClient.Timeout, cpAPITimeout)
	}
}

// TestStartSendsOnTheProvisionClient proves the provision POST actually goes
// through the long-budget client rather than merely having one defined.
// Without this, the constant above could be perfect and the send path could
// still use the 120s client -- the same "mechanism exists, caller bypasses it"
// gap that makes a green suite decorative.
func TestStartSendsOnTheProvisionClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"instance_id":"i-abc123","state":"pending"}`)
	}))
	defer srv.Close()

	sentinel := errors.New("general client must not carry the provision POST")
	p := &CPProvisioner{
		baseURL:      srv.URL,
		orgID:        "org-1",
		sharedSecret: "s3cret",
		// Any use of the general client for provisioning fails loudly.
		httpClient: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, sentinel
			}),
		},
		provisionHTTPClient: srv.Client(),
	}

	id, err := p.Start(context.Background(), WorkspaceConfig{
		WorkspaceID: "ws-1",
		Runtime:     "python",
	})
	if err != nil {
		if errors.Is(err, sentinel) {
			t.Fatal("the provision POST went through the general 120s client")
		}
		t.Fatalf("Start: %v", err)
	}
	if id != "i-abc123" {
		t.Errorf("instance id = %q, want i-abc123", id)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

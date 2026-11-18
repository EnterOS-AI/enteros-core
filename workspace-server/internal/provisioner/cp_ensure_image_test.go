package provisioner

// cp_ensure_image_test.go — the wire contract of the core#5019 pull-before-stop
// pre-flight.
//
// The whole guard reduces to one question the tenant asks the control plane and
// one rule for reading the answer. If the rule is wrong, the guard is worse than
// useless: reading a refusal as permission puts the #5019 outage back, and
// reading permission as a refusal wedges every restart on the fleet. So the
// status mapping is pinned exhaustively rather than by example.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newEnsureImageTestProvisioner(baseURL string) *CPProvisioner {
	return &CPProvisioner{
		baseURL:               baseURL,
		orgID:                 "org-5019",
		sharedSecret:          "shared-secret-value",
		adminToken:            "admin-token-value",
		httpClient:            &http.Client{Timeout: 5 * time.Second},
		ensureImageHTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
}

func TestEnsureImage_PostsTheWorkspaceIdentityAndAuth(t *testing.T) {
	var gotPath, gotAuth, gotAdmin string
	var gotBody EnsureImageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAdmin = r.Header.Get("X-Molecule-Admin-Token")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready","image_ref":"reg/workspace-template-hermes@sha256:93dfaf12"}`))
	}))
	defer srv.Close()

	p := newEnsureImageTestProvisioner(srv.URL)
	res, err := p.EnsureImage(context.Background(), EnsureImageRequest{
		WorkspaceID: "ws-1", Runtime: "hermes", Template: "hermes",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/cp/workspaces/ensure-image" {
		t.Errorf("path = %q, want /cp/workspaces/ensure-image", gotPath)
	}
	// Same gate pair as the provision POST — the CP must be able to tell WHICH
	// tenant is asking, or one tenant could warm (and enumerate) another's pins.
	if gotAuth != "Bearer shared-secret-value" {
		t.Errorf("platform gate header = %q", gotAuth)
	}
	if gotAdmin != "admin-token-value" {
		t.Errorf("per-tenant identity header = %q", gotAdmin)
	}
	// org_id must be filled from the provisioner even though the caller omitted
	// it: CP resolves the pin per tenant, and an empty org would resolve someone
	// else's (or nothing).
	if gotBody.OrgID != "org-5019" {
		t.Errorf("org_id = %q, want the provisioner's own org", gotBody.OrgID)
	}
	if gotBody.WorkspaceID != "ws-1" || gotBody.Runtime != "hermes" || gotBody.Template != "hermes" {
		t.Errorf("identity fields not forwarded: %+v", gotBody)
	}
	if res.Status != "ready" || !strings.Contains(res.ImageRef, "sha256:93dfaf12") {
		t.Errorf("result not decoded: %+v", res)
	}
}

// TestEnsureImage_StatusMapping is the load-bearing table. Each row states the
// CP's answer and whether the tenant is then allowed to destroy the container.
func TestEnsureImage_StatusMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantErr    bool
		wantCompat bool // wantErr AND errors.Is(err, ErrEnsureImageUnsupported)
	}{
		{name: "200 ready — image is on the daemon", status: 200, body: `{"status":"ready"}`},
		{name: "201 ready — CP may answer created", status: 201, body: `{"status":"ready"}`},
		{
			name:   "200 not_applicable — backend pulls at box boot, nothing for CP to pre-warm",
			status: 200, body: `{"status":"not_applicable"}`,
		},
		{
			// The one deliberate fail-OPEN. A tenant that rolls ahead of its CP
			// must keep restarting, or a version skew becomes a fleet-wide
			// restart outage — strictly worse than the bug being fixed.
			name:   "404 — control plane predates the endpoint",
			status: 404, body: `404 page not found`,
			wantErr: true, wantCompat: true,
		},
		{
			name:   "501 — control plane knows the route and declines to implement it",
			status: 501, body: `{"error":"not implemented"}`,
			wantErr: true, wantCompat: true,
		},
		{
			// THE #5019 CASE. A pin whose digest is not in the registry can never
			// be pulled; a longer timeout does nothing for it. Must fail closed.
			name:   "422 unobtainable digest — the failure #5020's budget cannot help",
			status: 422, body: `{"error":"manifest unknown for sha256:93dfaf12"}`,
			wantErr: true,
		},
		{name: "500 — CP-side failure", status: 500, body: `{"error":"pull failed: no space left on device"}`, wantErr: true},
		{name: "502 — registry outage behind CP", status: 502, body: `{"error":"registry unreachable"}`, wantErr: true},
		{name: "401 — misconfigured credentials must NOT read as permission", status: 401, body: `{"error":"unauthorized"}`, wantErr: true},
		{name: "403 — forbidden must NOT read as permission", status: 403, body: `{"error":"forbidden"}`, wantErr: true},
		{name: "200 with an undecodable body is not a confirmation", status: 200, body: `not json at all`, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// A control plane that predates ensure-image still serves the
				// rest of /cp/workspaces/*. That is the POSITIVE signal the 404
				// compat branch requires (core#5025 finding 5): without it, a
				// misrouted base URL would 404 identically and fail OPEN. The
				// discrimination itself is pinned in
				// cp_ensure_image_compat_test.go.
				if strings.HasSuffix(r.URL.Path, "/status") {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"state":"running"}`))
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := newEnsureImageTestProvisioner(srv.URL).
				EnsureImage(context.Background(), EnsureImageRequest{WorkspaceID: "ws", Runtime: "hermes"})

			if tc.wantErr && err == nil {
				t.Fatalf("HTTP %d must NOT read as permission to destroy the container", tc.status)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("HTTP %d must allow the restart; got %v", tc.status, err)
			}
			if got := errors.Is(err, ErrEnsureImageUnsupported); got != tc.wantCompat {
				t.Fatalf("errors.Is(err, ErrEnsureImageUnsupported) = %v, want %v (err=%v)", got, tc.wantCompat, err)
			}
		})
	}
}

func TestEnsureImage_TransportFailureIsNotPermission(t *testing.T) {
	// A CP we cannot reach tells us NOTHING about the image. Pre-#5019 the
	// tenant stopped anyway and then discovered the provision would also fail;
	// the workspace paid for that with its container.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	_, err := newEnsureImageTestProvisioner(url).
		EnsureImage(context.Background(), EnsureImageRequest{WorkspaceID: "ws", Runtime: "hermes"})
	if err == nil {
		t.Fatal("an unreachable control plane must not read as permission to destroy the container")
	}
	if errors.Is(err, ErrEnsureImageUnsupported) {
		t.Fatal("a transport failure is NOT the same signal as 'this CP has no such endpoint' — conflating them fails open on every outage")
	}
}

func TestEnsureImage_ErrorNeverEchoesTheRawBody(t *testing.T) {
	// Same hygiene rule as Start: an upstream that reflected the request would
	// otherwise put the provision bearer into our logs. Only the structured
	// {"error": …} field is quoted.
	secret := "shared-secret-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`your Authorization was Bearer ` + secret))
	}))
	defer srv.Close()

	_, err := newEnsureImageTestProvisioner(srv.URL).
		EnsureImage(context.Background(), EnsureImageRequest{WorkspaceID: "ws", Runtime: "hermes"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the shared secret leaked into the error text: %v", err)
	}
}

// TestEnsureImageTimeout_IsItsOwnBudget pins the #5019/#5020 lesson: the
// pre-flight may spend minutes pulling ~7GB and must never inherit the 120s
// small-JSON budget that made a cold pull fatal in the first place.
func TestEnsureImageTimeout_IsItsOwnBudget(t *testing.T) {
	if cpEnsureImageTimeout <= cpAPITimeout {
		t.Fatalf("cpEnsureImageTimeout (%s) must exceed the small-JSON cpAPITimeout (%s): the ensure-image call may have to pull a multi-GB image (core#5019)",
			cpEnsureImageTimeout, cpAPITimeout)
	}
	if cpEnsureImageTimeout < 10*time.Minute {
		t.Fatalf("cpEnsureImageTimeout (%s) is too short for a cold multi-GB pull over throttled registry egress; the measured 7.05GB case is the design point", cpEnsureImageTimeout)
	}
}

// TestNewCPProvisioner_WiresTheEnsureImageClient tests the DEFAULT, not just the
// parameter: every test above injects a client, so a constructor that forgot to
// wire one would leave every one of them green while production silently fell
// back. Asserts the value the constructor actually installs.
func TestNewCPProvisioner_WiresTheEnsureImageClient(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "org-ctor")
	p, err := NewCPProvisioner()
	if err != nil {
		t.Fatalf("NewCPProvisioner: %v", err)
	}
	if p.ensureImageHTTPClient == nil {
		t.Fatal("NewCPProvisioner must wire ensureImageHTTPClient — production would otherwise fall back to an ad-hoc client")
	}
	if p.ensureImageHTTPClient.Timeout != cpEnsureImageTimeout {
		t.Fatalf("ensureImageHTTPClient.Timeout = %s, want cpEnsureImageTimeout (%s)", p.ensureImageHTTPClient.Timeout, cpEnsureImageTimeout)
	}
	if p.ensureImageHTTPClient == p.httpClient {
		t.Fatal("the ensure-image client must not BE the 120s small-JSON client — that is the #5019 shape")
	}
}

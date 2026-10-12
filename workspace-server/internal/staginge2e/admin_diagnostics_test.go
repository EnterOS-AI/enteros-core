package staginge2e

// admin_diagnostics_test.go — untagged proof that the Guard B provisioning-
// timeout annotation actually carries the control plane's own explanation, and
// that it can never replace the verdict it is annotating.
//
// The regression being locked is the one that cost two investigations: a red
// staging-tenant-cd printing "org <slug> did not reach instance_status=running
// within timeout" and NOTHING about why, while the CP's own diagnostics endpoint
// held the check-constraint rejection the whole time.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBestEffortAdminGet_CarriesTheControlPlanesExplanation(t *testing.T) {
	// The shape the CP actually returns for a tenant stuck mid-provision, with
	// the provisioner error verbatim — this is the string whose absence made the
	// original failures unreadable.
	const stuckBody = `{"slug":"e2e-mcp-617016-d26411","db":{"org_present":true,` +
		`"org_status":"provisioning","instance_present":false,"instance_status":""},` +
		`"last_error":"provision: set org provider local: ERROR: new row for relation ` +
		`\"organizations\" violates check constraint \"organizations_provider_check\" (SQLSTATE 23514)"}`

	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.RequestURI()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(stuckBody))
	}))
	defer srv.Close()

	got := bestEffortAdminGet(srv.URL, "tok-abc", "/cp/admin/tenants/acme/diagnostics")

	if gotAuth != "Bearer tok-abc" {
		t.Errorf("admin bearer not sent: got %q", gotAuth)
	}
	if gotPath != "/cp/admin/tenants/acme/diagnostics" {
		t.Errorf("wrong path: got %q", gotPath)
	}
	if !strings.Contains(got, "HTTP 200") {
		t.Errorf("annotation must report the status code; got %q", got)
	}
	// The whole point: the constraint text survives into the failure message.
	for _, want := range []string{"organizations_provider_check", "23514", "instance_status"} {
		if !strings.Contains(got, want) {
			t.Errorf("the CP's own explanation must survive into the annotation; %q missing from %q", want, got)
		}
	}
}

// TestBestEffortAdminGet_NeverReplacesTheVerdict pins the "best-effort" half.
// Every one of these inputs would, with a naive implementation, either panic or
// abort the test — and would therefore REPLACE the real provisioning-timeout
// diagnosis with a secondary transport error. All must degrade to a readable
// string instead.
func TestBestEffortAdminGet_NeverReplacesTheVerdict(t *testing.T) {
	// A server that is up but refuses the call.
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer deny.Close()

	// A server that is DOWN — the common case when the CP is the thing that broke.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	cases := []struct {
		name            string
		base, tok, path string
		wantSub         string
	}{
		{"non_200_is_reported_not_fatal", deny.URL, "t", "/cp/admin/tenants/x/diagnostics", "HTTP 403"},
		{"dead_control_plane", deadURL, "t", "/cp/admin/tenants/x/diagnostics", "<unavailable:"},
		{"empty_base_url", "", "t", "/cp/admin/tenants/x/diagnostics", "no CP base URL"},
		{"whitespace_base_url", "   ", "t", "/cp/admin/tenants/x/diagnostics", "no CP base URL"},
		{"unparseable_base_url", "://nope", "t", "/x", "<unavailable:"},
		{"trailing_slash_base_is_normalised", deny.URL + "/", "t", "/cp/admin/tenants/x/diagnostics", "HTTP 403"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bestEffortAdminGet(tc.base, tc.tok, tc.path)
			if got == "" {
				t.Fatal("must always return a human-readable string, never empty")
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Fatalf("got %q, want it to contain %q", got, tc.wantSub)
			}
		})
	}
}

// TestBestEffortAdminGet_CapIsWideEnoughForARealDiagnosticsBody guards the
// core#5052 lesson in its new home: a cap that slices the body apart puts us
// straight back to an unreadable red run.
func TestBestEffortAdminGet_CapIsWideEnoughForARealDiagnosticsBody(t *testing.T) {
	// A real staging diagnostics body, captured 2026-08-05.
	const real = `{"slug":"staging-canary","domain":"staging.moleculesai.app","db":` +
		`{"org_present":true,"org_status":"running","instance_present":true,` +
		`"instance_status":"running","instance_id":"mol-tenant-staging-canary-ffc8ccfade97"},` +
		`"dns":{"present":false,"type":"CNAME","fqdn":"staging-canary.staging.moleculesai.app"},` +
		`"tunnel":{"present":false,"name":"staging-tenant-staging-canary-ffc8ccfa","connections":0},` +
		`"checked_at":"2026-08-05T07:17:03.231013579Z"}`

	if truncate(real, a2aTurnLogCap) != real {
		t.Fatalf("a2aTurnLogCap=%d truncates a REAL diagnostics body (len=%d) — the annotation would be lossy",
			a2aTurnLogCap, len(real))
	}
}

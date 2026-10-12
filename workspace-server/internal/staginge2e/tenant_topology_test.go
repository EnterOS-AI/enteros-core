//go:build staging_e2e

package staginge2e

// Unit test for deriveTenantTopology (tenant_topology.go). Pure logic, no
// network and no STAGING_E2E gate — it is the Go twin of
// tests/e2e/lib/test_tenant_topology_unit.sh and, crucially, PROVES the
// default-unset invariance: with all MOLECULE_TENANT_* unset the derived
// baseURL / origin equal the historical hardcoded "https://"+slug+"."+suffix
// values byte-for-byte, and NO route headers are added.

import (
	"net/http"
	"strings"
	"testing"
)

// clearTopoEnv unsets every MOLECULE_TENANT_* + the loopback opt-in so each case
// starts from the default (staging) shape. t.Setenv restores originals on cleanup.
func clearTopoEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"MOLECULE_TENANT_URL", "MOLECULE_TENANT_DOMAIN", "MOLECULE_TENANT_ROUTE_HOST",
		"MOLECULE_TENANT_ROUTE_DOMAIN", "MOLECULE_TENANT_ROUTE_PORT",
		"MOLECULE_TENANT_ORIGIN_TEMPLATE", "E2E_CP_ALLOW_EPHEMERAL_LOOPBACK",
	} {
		t.Setenv(k, "")
	}
}

// TestDeriveTenantTopology_DefaultUnsetIsByteIdenticalToStaging is the
// load-bearing invariance proof: default-unset ⇒ exact historical values.
func TestDeriveTenantTopology_DefaultUnsetIsByteIdenticalToStaging(t *testing.T) {
	clearTopoEnv(t)

	slug := "e2e-req-12345-ab12cd"
	suffix := "staging.moleculesai.app"
	cpBase := "https://staging-api.moleculesai.app"

	top, err := deriveTenantTopology(slug, suffix, cpBase)
	if err != nil {
		t.Fatalf("default derive returned error: %v", err)
	}

	// The historical hardcoded values every request used before this refactor.
	wantHost := slug + "." + suffix
	wantBase := "https://" + wantHost
	wantOrigin := "https://" + wantHost

	if top.baseURL != wantBase {
		t.Errorf("default baseURL = %q, want historical %q", top.baseURL, wantBase)
	}
	if top.origin != wantOrigin {
		t.Errorf("default origin = %q, want historical %q", top.origin, wantOrigin)
	}
	if top.routeHost != "" {
		t.Errorf("default routeHost = %q, want empty (no ephemeral routing on staging)", top.routeHost)
	}

	// And the request the harness builds must be byte-identical: same URL, same
	// Origin, and NO Host override / X-Molecule-Org-Slug added.
	rewritten, rtop, err := tenantTopoFromURL(wantBase + "/workspaces/abc?confirm=true")
	if err != nil {
		t.Fatalf("tenantTopoFromURL error: %v", err)
	}
	if want := wantBase + "/workspaces/abc?confirm=true"; rewritten != want {
		t.Errorf("rewritten URL = %q, want byte-identical %q", rewritten, want)
	}
	req, _ := http.NewRequest("GET", rewritten, nil)
	applyTenantRouting(req, rtop)
	if got := req.Header.Get("Origin"); got != wantOrigin {
		t.Errorf("applied Origin = %q, want %q", got, wantOrigin)
	}
	// http.NewRequest populates req.Host from the URL host (this was already true
	// of the pre-refactor code). The invariance requirement is that
	// applyTenantRouting does NOT OVERRIDE it on the default path — it must remain
	// the tenant subdomain, byte-identical to the old request on the wire.
	if req.Host != wantHost {
		t.Errorf("default applied req.Host = %q, want unchanged URL host %q (no override)", req.Host, wantHost)
	}
	if got := req.Header.Get("X-Molecule-Org-Slug"); got != "" {
		t.Errorf("default applied X-Molecule-Org-Slug = %q, want empty", got)
	}
}

// TestDeriveTenantTopology_Ephemeral covers the loopback ephemeral CP shape:
// baseURL swaps to the CP base, route headers appear, Origin is derived.
func TestDeriveTenantTopology_Ephemeral(t *testing.T) {
	clearTopoEnv(t)
	t.Setenv("MOLECULE_TENANT_URL", "http://controlplane:8080")
	t.Setenv("MOLECULE_TENANT_ROUTE_DOMAIN", "lvh.me")

	slug := "eph1"
	top, err := deriveTenantTopology(slug, "staging.moleculesai.app", "http://controlplane:8080")
	if err != nil {
		t.Fatalf("ephemeral derive error: %v", err)
	}
	if top.baseURL != "http://controlplane:8080" {
		t.Errorf("ephemeral baseURL = %q, want CP base", top.baseURL)
	}
	if top.routeHost != "eph1.lvh.me" {
		t.Errorf("ephemeral routeHost = %q, want eph1.lvh.me", top.routeHost)
	}
	if top.slug != "eph1" {
		t.Errorf("ephemeral slug = %q, want eph1", top.slug)
	}
	// Origin is DERIVED (not the CP base, which cors would 403): scheme + port
	// from baseURL, host from the route host.
	if top.origin != "http://eph1.lvh.me:8080" {
		t.Errorf("ephemeral origin = %q, want http://eph1.lvh.me:8080", top.origin)
	}

	// The applied request carries the routing headers.
	rewritten, rtop, err := tenantTopoFromURL("https://eph1.staging.moleculesai.app/workspaces")
	if err != nil {
		t.Fatalf("tenantTopoFromURL error: %v", err)
	}
	if rewritten != "http://controlplane:8080/workspaces" {
		t.Errorf("ephemeral rewritten URL = %q, want CP-based", rewritten)
	}
	req, _ := http.NewRequest("POST", rewritten, nil)
	applyTenantRouting(req, rtop)
	if req.Host != "eph1.lvh.me" {
		t.Errorf("ephemeral req.Host = %q, want eph1.lvh.me", req.Host)
	}
	if got := req.Header.Get("X-Molecule-Org-Slug"); got != "eph1" {
		t.Errorf("ephemeral X-Molecule-Org-Slug = %q, want eph1", got)
	}
	if got := req.Header.Get("Origin"); got != "http://eph1.lvh.me:8080" {
		t.Errorf("ephemeral Origin = %q, want http://eph1.lvh.me:8080", got)
	}
}

// TestDeriveTenantTopology_OriginTemplateWins mirrors the shell precedence.
func TestDeriveTenantTopology_OriginTemplateWins(t *testing.T) {
	clearTopoEnv(t)
	t.Setenv("MOLECULE_TENANT_URL", "http://controlplane:8080")
	t.Setenv("MOLECULE_TENANT_ROUTE_DOMAIN", "lvh.me")
	t.Setenv("MOLECULE_TENANT_ORIGIN_TEMPLATE", "http://{slug}.lvh.me:8080")

	top, err := deriveTenantTopology("eph2", "staging.moleculesai.app", "http://controlplane:8080")
	if err != nil {
		t.Fatalf("origin-template derive error: %v", err)
	}
	if top.origin != "http://eph2.lvh.me:8080" {
		t.Errorf("origin-template = %q, want http://eph2.lvh.me:8080", top.origin)
	}
}

// TestDeriveTenantTopology_UnderivableOriginFailsClosed is the NEGATIVE CONTROL:
// routing active, baseURL has no usable scheme, and no template ⇒ hard error
// (the fail-closed twin of the shell's rc=1).
func TestDeriveTenantTopology_UnderivableOriginFailsClosed(t *testing.T) {
	clearTopoEnv(t)
	t.Setenv("MOLECULE_TENANT_URL", "noscheme-garbage")
	t.Setenv("MOLECULE_TENANT_ROUTE_DOMAIN", "lvh.me")

	_, err := deriveTenantTopology("eph4", "staging.moleculesai.app", "http://controlplane:8080")
	if err == nil {
		t.Fatal("expected an error for an un-derivable ephemeral Origin, got nil")
	}
	if !strings.Contains(err.Error(), "cannot derive a tenant CORS Origin") {
		t.Errorf("error = %q, want the actionable 'cannot derive a tenant CORS Origin' message", err.Error())
	}
}

// TestValidateStagingCPBase_LoopbackGatedByOptIn proves the loopback relaxation
// is inert unless E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1 (default-unset invariance for
// the validation), and that the historical staging/loopback arms are unchanged.
func TestValidateStagingCPBase_LoopbackGatedByOptIn(t *testing.T) {
	clearTopoEnv(t)

	// Historical accepts — unchanged regardless of the opt-in.
	for _, ok := range []string{
		"https://staging-api.moleculesai.app",
		"http://127.0.0.1",
		"http://localhost",
	} {
		if err := validateStagingCPBase(ok); err != nil {
			t.Errorf("validateStagingCPBase(%q) = %v, want nil", ok, err)
		}
	}

	// lvh.me / container-name loopback: REJECTED with the opt-in unset.
	for _, ephem := range []string{
		"http://controlplane:8080",
		"http://eph1.lvh.me:8080",
		"http://lvh.me",
	} {
		if err := validateStagingCPBase(ephem); err == nil {
			t.Errorf("validateStagingCPBase(%q) = nil with opt-in unset, want rejection", ephem)
		}
	}

	// With the opt-in set, the same ephemeral bases are accepted.
	t.Setenv("E2E_CP_ALLOW_EPHEMERAL_LOOPBACK", "1")
	for _, ephem := range []string{
		"http://controlplane:8080",
		"http://eph1.lvh.me:8080",
		"http://lvh.me",
	} {
		if err := validateStagingCPBase(ephem); err != nil {
			t.Errorf("validateStagingCPBase(%q) with opt-in = %v, want nil", ephem, err)
		}
	}
	// A public https host is still rejected even under the opt-in (opt-in only
	// loosens http loopback).
	if err := validateStagingCPBase("https://evil.example.com"); err == nil {
		t.Error("validateStagingCPBase(https://evil.example.com) = nil under opt-in, want rejection")
	}
}

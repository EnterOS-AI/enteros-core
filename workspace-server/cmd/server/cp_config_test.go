package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRefreshEnvFromCP_NoopWhenNotSaaS: without MOLECULE_ORG_ID or
// ADMIN_TOKEN, the function short-circuits silently — self-hosted dev
// must not fail or log spam here.
func TestRefreshEnvFromCP_NoopWhenNotSaaS(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "")
	t.Setenv("ADMIN_TOKEN", "")
	if err := refreshEnvFromCP(); err != nil {
		t.Errorf("expected nil on non-SaaS, got %v", err)
	}
}

// TestRefreshEnvFromCP_AppliesCPResponse: wire a stub CP, run refresh,
// confirm the returned env vars ended up in os.Environ().
func TestRefreshEnvFromCP_AppliesCPResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tenant-admin-token" {
			t.Errorf("bearer: got %q", got)
		}
		if got := r.Header.Get("X-Molecule-Org-Id"); got != "org-abc" {
			t.Errorf("org id header: got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"MOLECULE_CP_SHARED_SECRET":"new-secret","MOLECULE_CP_URL":"https://api.moleculesai.app","DISPLAY_SESSION_SIGNING_SECRET":"display-secret","MOLECULE_LLM_BASE_URL":"https://api.moleculesai.app/api/v1/internal/llm/openai/v1","MOLECULE_LLM_USAGE_TOKEN":"tenant-admin-token","MOLECULE_LLM_DEFAULT_MODEL":"google/gemini-4-hydrogen"}`)
	}))
	defer srv.Close()

	t.Setenv("MOLECULE_ORG_ID", "org-abc")
	t.Setenv("ADMIN_TOKEN", "tenant-admin-token")
	t.Setenv("MOLECULE_CP_URL", srv.URL)
	t.Setenv("MOLECULE_CP_SHARED_SECRET", "") // clear before refresh

	if err := refreshEnvFromCP(); err != nil {
		t.Fatalf("refreshEnvFromCP: %v", err)
	}
	if got := os.Getenv("MOLECULE_CP_SHARED_SECRET"); got != "new-secret" {
		t.Errorf("SHARED_SECRET: want new-secret, got %q", got)
	}
	if got := os.Getenv("DISPLAY_SESSION_SIGNING_SECRET"); got != "display-secret" {
		t.Errorf("DISPLAY_SESSION_SIGNING_SECRET: want display-secret, got %q", got)
	}
	if got := os.Getenv("MOLECULE_LLM_BASE_URL"); got != "https://api.moleculesai.app/api/v1/internal/llm/openai/v1" {
		t.Errorf("MOLECULE_LLM_BASE_URL: got %q", got)
	}
	if got := os.Getenv("MOLECULE_LLM_USAGE_TOKEN"); got != "tenant-admin-token" {
		t.Errorf("MOLECULE_LLM_USAGE_TOKEN: got %q", got)
	}
	if got := os.Getenv("MOLECULE_LLM_DEFAULT_MODEL"); got != "google/gemini-4-hydrogen" {
		t.Errorf("MOLECULE_LLM_DEFAULT_MODEL: got %q", got)
	}
}

// TestRefreshEnvFromCP_ManagedTenantRequiresLLMKeys: watch-fail-first
// per Researcher Task #46. When running as a managed tenant
// (MOLECULE_ORG_ID + ADMIN_TOKEN set), missing LLM proxy env vars
// after refreshEnvFromCP MUST surface as MISSING_CP_LLM_ENV, not be
// silently accepted. Without this guard, a CP that loses its LLM
// creds (e.g. during an incident) would let a tenant boot and then
// fail later at first LLM call — worse than a loud refusal here.
func TestRefreshEnvFromCP_ManagedTenantRequiresLLMKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stub CP returns a CP response WITHOUT any of the required
		// LLM keys — simulates the failure mode where the CP side
		// dropped or never had the LLM creds for this org.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"MOLECULE_CP_SHARED_SECRET":"x","MOLECULE_CP_URL":"https://api.moleculesai.app"}`)
	}))
	defer srv.Close()

	t.Setenv("MOLECULE_ORG_ID", "org-managed-1")
	t.Setenv("ADMIN_TOKEN", "admin-tok")
	t.Setenv("MOLECULE_CP_URL", srv.URL)
	// Clear all LLM keys to simulate the boot-without-LLM-env failure mode.
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "")
	t.Setenv("MOLECULE_LLM_USAGE_URL", "")
	t.Setenv("MOLECULE_LLM_BASE_URL", "")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "")

	// refreshEnvFromCP itself should succeed — CP is reachable, returned 200.
	if err := refreshEnvFromCP(); err != nil {
		t.Fatalf("refreshEnvFromCP: %v", err)
	}
	// The boot assertion must catch the missing LLM keys.
	err := assertManagedTenantHasLLMEnv()
	if err == nil {
		t.Fatal("expected MISSING_CP_LLM_ENV error for managed tenant without LLM keys, got nil")
	}
	if !strings.Contains(err.Error(), "MISSING_CP_LLM_ENV") {
		t.Errorf("expected error to contain MISSING_CP_LLM_ENV, got: %v", err)
	}
}

// TestRefreshEnvFromCP_ManagedTenantHappyPath: when the CP returns
// all 4 LLM-proxy keys, the gate must PASS — no MISSING_CP_LLM_ENV
// for a properly-configured managed tenant. Watch-fail counterpart
// to TestRefreshEnvFromCP_ManagedTenantRequiresLLMKeys: if THIS test
// ever fires MISSING_CP_LLM_ENV on the byte-correct key set, the
// requiredLLMEnvVars list has drifted from the CP emission again.
// Per Researcher REQUEST_CHANGES TEST ADEQUACY note.
func TestRefreshEnvFromCP_ManagedTenantHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Return ALL 4 LLM-proxy keys — names byte-matched to
		// tenant_config.go:140-144 CP emission.
		fmt.Fprint(w, `{"MOLECULE_LLM_USAGE_TOKEN":"tok-1","MOLECULE_LLM_USAGE_URL":"https://llm.example.com/usage","MOLECULE_LLM_BASE_URL":"https://llm.example.com","MOLECULE_LLM_ANTHROPIC_BASE_URL":"https://llm.example.com/anthropic"}`)
	}))
	defer srv.Close()

	t.Setenv("MOLECULE_ORG_ID", "org-managed-happy")
	t.Setenv("ADMIN_TOKEN", "admin-tok")
	t.Setenv("MOLECULE_CP_URL", srv.URL)
	// Pre-clear so we can verify the refresh actually populated them.
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "")
	t.Setenv("MOLECULE_LLM_USAGE_URL", "")
	t.Setenv("MOLECULE_LLM_BASE_URL", "")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "")

	if err := refreshEnvFromCP(); err != nil {
		t.Fatalf("refreshEnvFromCP: %v", err)
	}
	// Sanity: refresh actually applied the keys.
	if got := os.Getenv("MOLECULE_LLM_USAGE_TOKEN"); got != "tok-1" {
		t.Errorf("refresh did not apply USAGE_TOKEN: got %q", got)
	}
	// The boot assertion must pass — no MISSING_CP_LLM_ENV.
	if err := assertManagedTenantHasLLMEnv(); err != nil {
		t.Errorf("managed happy path must not MISSING_CP_LLM_ENV, got: %v", err)
	}
}

// TestRefreshEnvFromCP_ManagedTenantPartialEnv: when the CP returns
// 3 of 4 LLM-proxy keys (one missing), the gate must STILL catch it
// and the error must name the missing key. Per Researcher
// REQUEST_CHANGES TEST ADEQUACY note — partial-env coverage is
// critical because the production failure mode is usually "one
// key dropped" not "all keys dropped".
func TestRefreshEnvFromCP_ManagedTenantPartialEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 3 of 4 — MOLECULE_LLM_ANTHROPIC_BASE_URL is missing.
		fmt.Fprint(w, `{"MOLECULE_LLM_USAGE_TOKEN":"tok-1","MOLECULE_LLM_USAGE_URL":"https://llm.example.com/usage","MOLECULE_LLM_BASE_URL":"https://llm.example.com"}`)
	}))
	defer srv.Close()

	t.Setenv("MOLECULE_ORG_ID", "org-managed-partial")
	t.Setenv("ADMIN_TOKEN", "admin-tok")
	t.Setenv("MOLECULE_CP_URL", srv.URL)
	// Pre-clear all 4 so the 3 that come back from CP are the only
	// ones set; the 4th (MOLECULE_LLM_ANTHROPIC_BASE_URL) stays empty.
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "")
	t.Setenv("MOLECULE_LLM_USAGE_URL", "")
	t.Setenv("MOLECULE_LLM_BASE_URL", "")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "")

	if err := refreshEnvFromCP(); err != nil {
		t.Fatalf("refreshEnvFromCP: %v", err)
	}
	err := assertManagedTenantHasLLMEnv()
	if err == nil {
		t.Fatal("expected MISSING_CP_LLM_ENV for partial env (3 of 4 keys), got nil")
	}
	if !strings.Contains(err.Error(), "MISSING_CP_LLM_ENV") {
		t.Errorf("expected error to contain MISSING_CP_LLM_ENV, got: %v", err)
	}
	if !strings.Contains(err.Error(), "MOLECULE_LLM_ANTHROPIC_BASE_URL") {
		t.Errorf("expected error to name the missing key MOLECULE_LLM_ANTHROPIC_BASE_URL, got: %v", err)
	}
}

// TestAssertManagedTenantHasLLMEnv_NotManagedIsNoop: self-hosted
// (no orgID/adminToken) must NOT block on missing LLM keys — dev
// ergonomics matter and the assertion's contract is "managed only".
func TestAssertManagedTenantHasLLMEnv_NotManagedIsNoop(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "")
	t.Setenv("ADMIN_TOKEN", "")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "")
	t.Setenv("MOLECULE_LLM_USAGE_URL", "")
	t.Setenv("MOLECULE_LLM_BASE_URL", "")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "")
	if err := assertManagedTenantHasLLMEnv(); err != nil {
		t.Errorf("self-hosted (not managed) must not block, got: %v", err)
	}
}

// TestRefreshEnvFromCP_CPUnreachableDoesNotFailBoot: network errors must
// return non-nil BUT main.go treats that as warn-and-continue. We assert
// the function returns an error (not a panic) so the caller can log.
func TestRefreshEnvFromCP_CPUnreachableDoesNotFailBoot(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "org-abc")
	t.Setenv("ADMIN_TOKEN", "t")
	t.Setenv("MOLECULE_CP_URL", "http://127.0.0.1:1") // closed port
	err := refreshEnvFromCP()
	if err == nil {
		t.Error("expected an error when CP is unreachable")
	}
}

// TestRefreshEnvFromCP_NonOKPropagates: CP returns 500 → error.
func TestRefreshEnvFromCP_NonOKPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("MOLECULE_ORG_ID", "org-abc")
	t.Setenv("ADMIN_TOKEN", "t")
	t.Setenv("MOLECULE_CP_URL", srv.URL)
	if err := refreshEnvFromCP(); err == nil {
		t.Error("expected error on 500, got nil")
	}
}

// TestRefreshEnvFromCP_RejectsOversizedValue: a single-value-over-4KiB
// payload must NOT poison the environment.
func TestRefreshEnvFromCP_RejectsOversizedValue(t *testing.T) {
	giant := make([]byte, 5<<10)
	for i := range giant {
		giant[i] = 'x'
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"MOLECULE_CP_SHARED_SECRET":%q}`, string(giant))
	}))
	defer srv.Close()
	t.Setenv("MOLECULE_ORG_ID", "org-abc")
	t.Setenv("ADMIN_TOKEN", "t")
	t.Setenv("MOLECULE_CP_URL", srv.URL)
	t.Setenv("MOLECULE_CP_SHARED_SECRET", "original")
	if err := refreshEnvFromCP(); err != nil {
		t.Fatalf("refreshEnvFromCP: %v", err)
	}
	if got := os.Getenv("MOLECULE_CP_SHARED_SECRET"); got != "original" {
		t.Errorf("oversized value was applied — want %q, got %d bytes",
			"original", len(got))
	}
}

// TestRefreshEnvFromCP_ClientTimeoutFiresOnSlowUpstream: the
// core#2125 fix replaced http.DefaultClient with
// `&http.Client{Timeout: 10 * time.Second}`. This regression test
// proves that a hung / slow upstream does NOT block the boot — the
// client times out at 10s and refreshEnvFromCP returns an error
// within a small bound. Without the timeout, this test would block
// for 12s+ (the slow server's delay) AND the test would still pass
// on the wrong invariant — so we ALSO assert the elapsed wall time
// is well under the server delay (proving the timeout fired, not
// the server response).
//
// Runtime cost: ~10s wall clock. Acceptable for a regression test
// that runs once per CI build; the alternative (mock the http
// transport) would test the mock, not the real http.Client.Timeout
// contract — exactly the trade-off core#2125 is about.
func TestRefreshEnvFromCP_ClientTimeoutFiresOnSlowUpstream(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 10s slow-upstream test in -short mode")
	}
	// Server that delays 12s — LONGER than the 10s client timeout, so
	// the timeout MUST fire first. If the timeout were absent (or set
	// higher), this handler would run to completion and refreshEnvFromCP
	// would return success after 12s — both wrong.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(12 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	t.Setenv("MOLECULE_ORG_ID", "org-abc")
	t.Setenv("ADMIN_TOKEN", "t")
	t.Setenv("MOLECULE_CP_URL", srv.URL)

	start := time.Now()
	err := refreshEnvFromCP()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected timeout error on slow upstream, got nil (would mean the 10s timeout is missing)")
	}
	// Bound the elapsed time at 11s — the 10s client timeout + up to
	// 1s of slack for goroutine scheduling. If this is > 11s, either
	// the timeout was raised or the test server isn't actually slow.
	if elapsed > 11*time.Second {
		t.Errorf("refreshEnvFromCP took %v on a 12s-delay server; expected the 10s client timeout to fire first (elapsed < 11s)", elapsed)
	}
	// The error should mention timeout / deadline — proves the failure
	// mode is the client.Timeout, not a misrouted request.
	errStr := err.Error()
	if !strings.Contains(errStr, "timeout") && !strings.Contains(errStr, "deadline") {
		t.Errorf("error should mention timeout/deadline (the client.Timeout path), got: %v", err)
	}
}

// withFastRetry shrinks the CP-config retry cadence for a test and restores it.
func withFastRetry(t *testing.T, window, interval time.Duration) {
	t.Helper()
	savedW, savedI := cpConfigRetryWindow, cpConfigRetryInterval
	cpConfigRetryWindow, cpConfigRetryInterval = window, interval
	t.Cleanup(func() { cpConfigRetryWindow, cpConfigRetryInterval = savedW, savedI })
}

// TestEnsureManagedTenantLLMEnv_RetriesThroughRowCommitRace: a freshly-
// provisioned tenant can hit GET /cp/tenants/config BEFORE the CP commits its
// org_instances row (the admin_token lookup 401s). ensureManagedTenantLLMEnv
// must RETRY until the row lands (200 + LLM keys) rather than fatal on the first
// 401 — the exact failure the ephemeral-CP happy-path proof surfaced (fast
// local-docker provisioning exposes the race that slow EC2 boot masks).
func TestEnsureManagedTenantLLMEnv_RetriesThroughRowCommitRace(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// First two calls race the CP row-commit → 401 (no org_instances row
		// yet); the third lands after the row commits → 200 with the LLM env.
		if atomic.AddInt32(&calls, 1) < 3 {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"MOLECULE_LLM_USAGE_TOKEN":"tok","MOLECULE_LLM_USAGE_URL":"https://llm/u","MOLECULE_LLM_BASE_URL":"https://llm","MOLECULE_LLM_ANTHROPIC_BASE_URL":"https://llm/a"}`)
	}))
	defer srv.Close()

	t.Setenv("MOLECULE_ORG_ID", "org-race")
	t.Setenv("ADMIN_TOKEN", "admin-tok")
	t.Setenv("MOLECULE_CP_URL", srv.URL)
	t.Setenv("MOLECULE_CP_CONFIG_RETRY_WINDOW", "") // use the package-var window below
	for _, k := range requiredLLMEnvVars {
		t.Setenv(k, "")
	}
	withFastRetry(t, 5*time.Second, 20*time.Millisecond)

	if err := ensureManagedTenantLLMEnv(); err != nil {
		t.Fatalf("expected success after retrying through the 401 row-commit race, got: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got < 3 {
		t.Errorf("expected >=3 CP calls (2x401 then 200), got %d — retry did not engage", got)
	}
	if got := os.Getenv("MOLECULE_LLM_USAGE_TOKEN"); got != "tok" {
		t.Errorf("LLM env not applied after retry: MOLECULE_LLM_USAGE_TOKEN=%q", got)
	}
}

// TestEnsureManagedTenantLLMEnv_FatalsOnPersistentFetchFailure: if the fetch
// keeps FAILING (401/network) for the whole window (CP never commits the row, or
// is down), ensureManagedTenantLLMEnv returns non-nil so the caller fatals — and
// the error carries the REAL fetch cause (401), not a generic MISSING_CP_LLM_ENV
// (code-review #4: the fatal must name the mechanism).
func TestEnsureManagedTenantLLMEnv_FatalsOnPersistentFetchFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized) // ALWAYS 401
	}))
	defer srv.Close()

	t.Setenv("MOLECULE_ORG_ID", "org-bad")
	t.Setenv("ADMIN_TOKEN", "admin-tok")
	t.Setenv("MOLECULE_CP_URL", srv.URL)
	t.Setenv("MOLECULE_CP_CONFIG_RETRY_WINDOW", "")
	for _, k := range requiredLLMEnvVars {
		t.Setenv(k, "")
	}
	withFastRetry(t, 100*time.Millisecond, 20*time.Millisecond)

	err := ensureManagedTenantLLMEnv()
	if err == nil {
		t.Fatal("expected non-nil verdict on a persistent fetch failure (must still fatal upstream), got nil")
	}
	// The verdict must name the real cause (the 401), not the generic assertion.
	if !strings.Contains(err.Error(), "did not succeed") || !strings.Contains(err.Error(), "401") {
		t.Errorf("expected the error to carry the real fetch cause (…did not succeed…: cp returned 401), got: %v", err)
	}
}

// TestEnsureManagedTenantLLMEnv_FailsFastOnDeterministic200Incomplete: a fetch
// that SUCCEEDS (200) but still lacks a required LLM key is CP-side drift, NOT
// the row-commit race — retrying cannot fix it. ensureManagedTenantLLMEnv must
// fatal IMMEDIATELY with MISSING_CP_LLM_ENV, NOT burn the whole retry window
// (code-review #3). We prove "fast" by giving a LONG window and asserting the
// call returns well under it.
func TestEnsureManagedTenantLLMEnv_FailsFastOnDeterministic200Incomplete(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		// 200 with only 3 of the 4 required keys — deterministic drift, every time.
		fmt.Fprint(w, `{"MOLECULE_LLM_USAGE_TOKEN":"tok","MOLECULE_LLM_USAGE_URL":"https://llm/u","MOLECULE_LLM_BASE_URL":"https://llm"}`)
	}))
	defer srv.Close()

	t.Setenv("MOLECULE_ORG_ID", "org-drift")
	t.Setenv("ADMIN_TOKEN", "admin-tok")
	t.Setenv("MOLECULE_CP_URL", srv.URL)
	t.Setenv("MOLECULE_CP_CONFIG_RETRY_WINDOW", "")
	for _, k := range requiredLLMEnvVars {
		t.Setenv(k, "")
	}
	// A LONG window: if the code wrongly retried a deterministic 200, this test
	// would take ~30s. The fast-fail must return in well under a second.
	withFastRetry(t, 30*time.Second, time.Second)

	start := time.Now()
	err := ensureManagedTenantLLMEnv()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected MISSING_CP_LLM_ENV on a deterministic partial-200, got nil")
	}
	if !strings.Contains(err.Error(), "MISSING_CP_LLM_ENV") {
		t.Errorf("expected MISSING_CP_LLM_ENV (the missing key named), got: %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("deterministic partial-200 must fail FAST (no window burn); took %v", elapsed)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("deterministic partial-200 must NOT retry; want 1 CP call, got %d", got)
	}
}

// TestResolveCPConfigRetryWindow: the MOLECULE_CP_CONFIG_RETRY_WINDOW override is
// honored only for a VALID positive duration; invalid / zero / negative values
// are REJECTED (fall back to the default), never silently applied — a "0" must
// NOT disable the retry (code-review #1/#2).
func TestResolveCPConfigRetryWindow(t *testing.T) {
	withFastRetry(t, 90*time.Second, time.Second) // default window under test
	cases := []struct {
		name, env string
		want      time.Duration
	}{
		{"unset → default", "", 90 * time.Second},
		{"valid → honored", "45s", 45 * time.Second},
		{"zero → rejected (must NOT disable retry)", "0", 90 * time.Second},
		{"zero-dur → rejected", "0s", 90 * time.Second},
		{"negative → rejected", "-5s", 90 * time.Second},
		{"unitless typo → rejected", "45", 90 * time.Second},
		{"garbage → rejected", "soon", 90 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("MOLECULE_CP_CONFIG_RETRY_WINDOW", c.env)
			if got := resolveCPConfigRetryWindow(); got != c.want {
				t.Errorf("MOLECULE_CP_CONFIG_RETRY_WINDOW=%q → %v, want %v", c.env, got, c.want)
			}
		})
	}
}

// TestEnsureManagedTenantLLMEnv_NotManagedNoRetry: self-hosted (no orgID/token)
// returns nil immediately and never enters the retry loop — a long window must
// NOT delay a self-host boot.
func TestEnsureManagedTenantLLMEnv_NotManagedNoRetry(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "")
	t.Setenv("ADMIN_TOKEN", "")
	t.Setenv("MOLECULE_CP_CONFIG_RETRY_WINDOW", "")
	withFastRetry(t, 30*time.Second, time.Second) // would hang if the loop ran

	start := time.Now()
	if err := ensureManagedTenantLLMEnv(); err != nil {
		t.Fatalf("self-hosted must return nil, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("self-hosted must NOT enter the retry loop; took %v", elapsed)
	}
}

// TestEnsureManagedTenantLLMEnv_HappyPathNoRetry: when the first fetch already
// delivers all LLM keys, ensureManagedTenantLLMEnv returns nil after exactly one
// CP call — no retry latency on the common path.
func TestEnsureManagedTenantLLMEnv_HappyPathNoRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"MOLECULE_LLM_USAGE_TOKEN":"tok","MOLECULE_LLM_USAGE_URL":"https://llm/u","MOLECULE_LLM_BASE_URL":"https://llm","MOLECULE_LLM_ANTHROPIC_BASE_URL":"https://llm/a"}`)
	}))
	defer srv.Close()

	t.Setenv("MOLECULE_ORG_ID", "org-happy")
	t.Setenv("ADMIN_TOKEN", "admin-tok")
	t.Setenv("MOLECULE_CP_URL", srv.URL)
	t.Setenv("MOLECULE_CP_CONFIG_RETRY_WINDOW", "")
	for _, k := range requiredLLMEnvVars {
		t.Setenv(k, "")
	}
	withFastRetry(t, 5*time.Second, 20*time.Millisecond)

	if err := ensureManagedTenantLLMEnv(); err != nil {
		t.Fatalf("happy path must succeed, got: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("happy path must make exactly 1 CP call (no retry), got %d", got)
	}
}

// TestAssertSaaSTenantHasAdminToken (core#4485): a Molecule-managed tenant
// (MOLECULE_ORG_ID set) with no ADMIN_TOKEN must REFUSE boot — otherwise
// AdminAuth's Tier-3 fallback accepts any workspace token as org-admin. Self-
// hosted / local-dev (no MOLECULE_ORG_ID) must still boot, keeping the deprecated
// Tier-3 path. Gated on MOLECULE_ORG_ID (the CP-provisioner signal), NOT
// MOLECULE_DEPLOY_MODE. Negative-controlled: both refuse arms and pass arms;
// a whitespace-only ADMIN_TOKEN counts as unset.
func TestAssertSaaSTenantHasAdminToken(t *testing.T) {
	cases := []struct {
		name       string
		deployMode string
		orgID      string
		adminToken string
		wantErr    bool
	}{
		{"self-host: no org, no admin token -> ok (Tier-3 exempt)", "", "", "", false},
		{"self-host: DEPLOY_MODE=self-hosted, no org, no admin -> ok", "self-hosted", "", "", false},
		{"managed: org set + DEPLOY_MODE=self-hosted, no admin -> refuse (CP-provisioned on ORG_ID alone)", "self-hosted", "org-abc", "", true},
		{"managed: org set, admin token set -> ok", "", "org-abc", "secret", false},
		{"managed: org set, NO admin token -> refuse", "", "org-abc", "", true},
		{"managed: org set, whitespace admin token -> refuse", "", "org-abc", "   ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MOLECULE_DEPLOY_MODE", tc.deployMode)
			t.Setenv("MOLECULE_ORG_ID", tc.orgID)
			t.Setenv("ADMIN_TOKEN", tc.adminToken)
			err := assertSaaSTenantHasAdminToken()
			if tc.wantErr && err == nil {
				t.Fatalf("expected boot refusal, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil (must not break this deploy), got %v", err)
			}
		})
	}
}

// withFastBackgroundFill shrinks fillEnvFromCPInBackground's window and longest
// pause for a test and restores them.
func withFastBackgroundFill(t *testing.T, window, maxInterval time.Duration) {
	t.Helper()
	savedW, savedI := cpEnvBackgroundFillWindow, cpEnvBackgroundFillMaxInterval
	cpEnvBackgroundFillWindow, cpEnvBackgroundFillMaxInterval = window, maxInterval
	t.Cleanup(func() { cpEnvBackgroundFillWindow, cpEnvBackgroundFillMaxInterval = savedW, savedI })
}

// joinBackgroundEnvFill makes ensureManagedTenantLLMEnv start its background
// fill where the test can wait for it. The returned channel closes when the fill
// returns; started counts how often it was started.
func joinBackgroundEnvFill(t *testing.T) (done <-chan struct{}, started *int32) {
	t.Helper()
	ch := make(chan struct{})
	var n int32
	saved := startBackgroundEnvFill
	startBackgroundEnvFill = func(firstErr error) {
		atomic.AddInt32(&n, 1)
		go func() {
			defer close(ch)
			fillEnvFromCPInBackground(firstErr)
		}()
	}
	t.Cleanup(func() { startBackgroundEnvFill = saved })
	return ch, &n
}

// TestEnsureManagedTenantLLMEnv_FetchFailsWithLLMEnvPresent_FillsTheRest: the
// 2026-11-19 staging defect. A fresh tenant on the Kubernetes substrate already
// carries the four required LLM names in its Deployment env, so when its first
// config fetch lost the org_instances row-commit race (401) it booted at once
// and never asked again: MOLECULE_LLM_DEFAULT_MODEL stayed unset, the tenant
// reported the code fallback as its platform default, and the concierge handed
// that to the workspaces it created. Boot must still not wait, but the fetch
// must be retried, and it must only FILL: a name that has a value keeps it.
func TestEnsureManagedTenantLLMEnv_FetchFailsWithLLMEnvPresent_FillsTheRest(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The row commits after the second miss.
		if atomic.AddInt32(&calls, 1) < 3 {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"MOLECULE_LLM_USAGE_TOKEN":"from-cp","MOLECULE_LLM_DEFAULT_MODEL":"minimax/MiniMax-M3"}`)
	}))
	defer srv.Close()

	t.Setenv("MOLECULE_ORG_ID", "org-fresh")
	t.Setenv("ADMIN_TOKEN", "admin-tok")
	t.Setenv("MOLECULE_CP_URL", srv.URL)
	t.Setenv("MOLECULE_CP_CONFIG_RETRY_WINDOW", "")
	for _, k := range requiredLLMEnvVars {
		t.Setenv(k, "baked-"+k) // what the provisioner wrote into the Deployment
	}
	t.Setenv("MOLECULE_LLM_DEFAULT_MODEL", "") // not in the Deployment env
	withFastRetry(t, 30*time.Second, 10*time.Millisecond)
	withFastBackgroundFill(t, 10*time.Second, 20*time.Millisecond)
	done, started := joinBackgroundEnvFill(t)

	start := time.Now()
	if err := ensureManagedTenantLLMEnv(); err != nil {
		t.Fatalf("a tenant with its LLM env present must boot when the fetch fails, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("boot must not wait for the control plane when the LLM env is present; took %v", elapsed)
	}
	if got := atomic.LoadInt32(started); got != 1 {
		t.Fatalf("the failed fetch must be retried in the background; fills started = %d, want 1", got)
	}

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("the background fill did not finish after the control plane started answering")
	}
	if got := os.Getenv("MOLECULE_LLM_DEFAULT_MODEL"); got != "minimax/MiniMax-M3" {
		t.Errorf("MOLECULE_LLM_DEFAULT_MODEL = %q after the background fill, want the control plane's minimax/MiniMax-M3", got)
	}
	if got := os.Getenv("MOLECULE_LLM_USAGE_TOKEN"); got != "baked-MOLECULE_LLM_USAGE_TOKEN" {
		t.Errorf("a fill after boot must not change a value already in use: MOLECULE_LLM_USAGE_TOKEN = %q", got)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("want 3 config fetches (boot 401, retry 401, retry 200) and none after the answer, got %d", got)
	}
}

// TestEnsureManagedTenantLLMEnv_FetchSucceeds_NoBackgroundFill: a fetch that
// answered is applied in full at boot, so nothing is left to retry.
func TestEnsureManagedTenantLLMEnv_FetchSucceeds_NoBackgroundFill(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"MOLECULE_LLM_DEFAULT_MODEL":"minimax/MiniMax-M3"}`)
	}))
	defer srv.Close()

	t.Setenv("MOLECULE_ORG_ID", "org-ok")
	t.Setenv("ADMIN_TOKEN", "admin-tok")
	t.Setenv("MOLECULE_CP_URL", srv.URL)
	for _, k := range requiredLLMEnvVars {
		t.Setenv(k, "baked-"+k)
	}
	t.Setenv("MOLECULE_LLM_DEFAULT_MODEL", "google/gemini-4-hydrogen") // an older value in the Deployment
	_, started := joinBackgroundEnvFill(t)

	if err := ensureManagedTenantLLMEnv(); err != nil {
		t.Fatalf("expected nil, got: %v", err)
	}
	if got := atomic.LoadInt32(started); got != 0 {
		t.Errorf("a fetch that answered needs no background fill; started = %d", got)
	}
	// At boot the control plane's value replaces the older one, as before.
	if got := os.Getenv("MOLECULE_LLM_DEFAULT_MODEL"); got != "minimax/MiniMax-M3" {
		t.Errorf("MOLECULE_LLM_DEFAULT_MODEL = %q, want the control plane's value applied at boot", got)
	}
}

// TestFillEnvFromCPInBackground_GivesUpAfterWindow: a control plane that never
// answers must not be asked forever, and nothing is set.
func TestFillEnvFromCPInBackground_GivesUpAfterWindow(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
	}))
	defer srv.Close()

	t.Setenv("MOLECULE_ORG_ID", "org-never")
	t.Setenv("ADMIN_TOKEN", "admin-tok")
	t.Setenv("MOLECULE_CP_URL", srv.URL)
	t.Setenv("MOLECULE_LLM_DEFAULT_MODEL", "")
	withFastRetry(t, 30*time.Second, 10*time.Millisecond)
	withFastBackgroundFill(t, 150*time.Millisecond, 40*time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		fillEnvFromCPInBackground(fmt.Errorf("cp returned 401"))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the background fill did not give up after its window")
	}
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Errorf("want more than one retry inside the window, got %d", got)
	}
	if got := os.Getenv("MOLECULE_LLM_DEFAULT_MODEL"); got != "" {
		t.Errorf("nothing must be set when the control plane never answered, got %q", got)
	}
}

// TestApplyCPTenantConfig_OnlyUnset pins the fill rule: unset names are set, and
// a name that has a value, an empty CP value, an empty key and an oversized
// value are all left alone.
func TestApplyCPTenantConfig_OnlyUnset(t *testing.T) {
	t.Setenv("CPFILL_HAS_VALUE", "kept")
	t.Setenv("CPFILL_UNSET", "")
	t.Setenv("CPFILL_EMPTY_FROM_CP", "")
	t.Setenv("CPFILL_OVERSIZED", "")
	cfg := map[string]string{
		"CPFILL_HAS_VALUE":     "from-cp",
		"CPFILL_UNSET":         "from-cp",
		"CPFILL_EMPTY_FROM_CP": "",
		"CPFILL_OVERSIZED":     strings.Repeat("x", 4<<10+1),
		"":                     "no-name",
	}
	if n := applyCPTenantConfig(cfg, true); n != 1 {
		t.Errorf("filled %d value(s), want exactly 1 (CPFILL_UNSET)", n)
	}
	for name, want := range map[string]string{
		"CPFILL_HAS_VALUE":     "kept",
		"CPFILL_UNSET":         "from-cp",
		"CPFILL_EMPTY_FROM_CP": "",
		"CPFILL_OVERSIZED":     "",
	} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// The boot-time apply still replaces a value: that is what a rotated secret needs.
	if n := applyCPTenantConfig(map[string]string{"CPFILL_HAS_VALUE": "rotated"}, false); n != 1 || os.Getenv("CPFILL_HAS_VALUE") != "rotated" {
		t.Errorf("boot-time apply must replace a value: n=%d CPFILL_HAS_VALUE=%q", n, os.Getenv("CPFILL_HAS_VALUE"))
	}
}

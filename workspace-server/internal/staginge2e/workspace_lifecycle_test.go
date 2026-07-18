//go:build staging_e2e

package staginge2e

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestWorkspaceLifecycle_Staging is the live, against-real-staging end-to-end
// test for core#2332 P1.10 — workspace lifecycle (soft-restart / pause / resume
// / hibernate) coverage.
//
// What it proves that the handler unit tests (httptest in
// internal/handlers/*_test.go) cannot: that against a real staging tenant on
// the configured compute provider, the lifecycle endpoints actually transition
// the container state and recover — not just flip a DB flag or return HTTP 200.
//
// Pipeline:
//
//  1. Provision a throwaway org + tenant via the CP admin API.
//
//  2. Acquire the tenant admin token (accepted by ws-server WorkspaceAuth as
//     ADMIN_TOKEN — see middleware/wsauth_middleware.go).
//
//  3. Create a workspace via the tenant ws-server; wait for status=online with
//     a routable url (the real boot→register signal).
//
//  4. Drive each lifecycle endpoint and assert OBSERVABLE state:
//
//     soft restart (POST /restart):
//     online → provisioning → online, and a post-restart serve probe (A2A
//     round-trip) succeeds — proves the container came back serveable, not
//     just that the row flipped.
//
//     pause (POST /pause):
//     → paused, AND the container is genuinely stopped — observed via the
//     tenant API as: url cleared + the workspace no longer serves A2A
//     (a stopped container is unreachable; a mere flag would still serve).
//     resume (POST /resume):
//     paused → provisioning → online + serveable again.
//
//     hibernate (POST /hibernate?force=true):
//     online → hibernated, container stopped (url cleared, unserveable).
//     wake (next A2A message):
//     hibernated → online (auto-wake-on-message; Resume only handles paused).
//
// Status is read from the live DB-backed GET /workspaces/:id (canvas) endpoint
// — the response body of the lifecycle POST could lie; the GET proves the row.
//
// Guarded by the staging_e2e build tag and STAGING_E2E=1 env gate. Teardown is
// t.Cleanup-driven (admin DELETE /cp/admin/tenants).
func TestWorkspaceLifecycle_Staging(t *testing.T) {
	cfg := requireStagingEnv(t)

	slug := e2eSlug("life")
	t.Logf("workspace-lifecycle: slug=%s", slug)

	// --- Step 1: provision org via admin API ---
	orgID := adminCreateOrg(t, cfg, slug)
	t.Logf("org created: org_id=%s", orgID)

	// --- Step 1b: acquire tenant admin token + wait for tenant TLS ready ---
	token := tenantAdminToken(t, cfg, slug)
	tenantHost := slug + "." + cfg.subdomainSuffix
	waitForHTTP(t, tenantHost, http.StatusOK, 10*time.Minute, "tenant /health ready")
	t.Logf("tenant TLS ready: %s", tenantHost)

	// --- Step 2: create workspace + wait online (routable) ---
	wsID := tenantCreateWorkspace(t, cfg, tenantHost, token, orgID)
	waitForWorkspaceOnlineRoutable(t, tenantHost, token, orgID, wsID, 15*time.Minute, "initial boot")
	t.Logf("workspace %s online + routable", wsID)

	// Baseline: the freshly-online workspace must actually serve A2A.
	assertServes(t, tenantHost, token, orgID, wsID, "baseline (post-boot)")

	// ── soft restart ────────────────────────────────────────────────────────
	// online → provisioning → online; container must come back serveable.
	t.Run("restart", func(t *testing.T) {
		status, body := postLifecycle(t, tenantHost, token, orgID, wsID, "/restart")
		if status != http.StatusOK {
			t.Fatalf("restart: HTTP %d: %s", status, body)
		}
		if st := jsonField(body, "status"); st != "provisioning" {
			t.Fatalf("restart: body status=%q (expected provisioning): %s", st, body)
		}
		// The endpoint flips status→provisioning synchronously (before the HTTP
		// response) then re-provisions in a goroutine. We don't hard-assert
		// observing the intermediate 'provisioning' via GET: on a fast box the
		// row can race back to online before our first poll, so requiring to
		// CATCH provisioning would be a false-negative flake. The body already
		// proved the synchronous flip; the load-bearing observable is the
		// eventual online+routable + a successful serve probe below.
		waitForWorkspaceOnlineRoutable(t, tenantHost, token, orgID, wsID, 15*time.Minute, "restart→online")
		// Post-restart liveness/serve probe — proves the container is actually
		// back, not just that the status row says online.
		assertServes(t, tenantHost, token, orgID, wsID, "post-restart")
		t.Logf("restart VERIFIED: online → provisioning → online + serveable")
	})

	// ── pause → resume ──────────────────────────────────────────────────────
	t.Run("pause_resume", func(t *testing.T) {
		// Per-subtest online BASELINE (core#3456 finding #6, shared-mutable-
		// state isolation): the three subtests share ONE workspace sequentially.
		// Re-establishing a known online+routable baseline at the top of each
		// converts an upstream subtest's bad exit state from a silent cascade
		// (e.g. a failed resume leaving the ws not-online → hibernate 404s "not
		// in a hibernatable state") into an isolated, independently-meaningful
		// subtest. It is a real-signal wait (breaks instantly when online), not
		// a tolerate — a genuinely stuck ws still fails loud at the deadline.
		waitForWorkspaceOnlineRoutable(t, tenantHost, token, orgID, wsID, 15*time.Minute, "pause_resume baseline")

		// pause → paused, container genuinely stopped.
		status, body := postLifecycle(t, tenantHost, token, orgID, wsID, "/pause")
		if status != http.StatusOK {
			t.Fatalf("pause: HTTP %d: %s", status, body)
		}
		if st := jsonField(body, "status"); st != "paused" {
			t.Fatalf("pause: body status=%q (expected paused): %s", st, body)
		}
		waitForWorkspaceStatus(t, tenantHost, token, orgID, wsID, "paused", 3*time.Minute, "pause→paused")
		// Genuinely-stopped assertion: the canvas GET clears url on pause
		// (Pause SETs url=''), and a stopped container no longer serves A2A.
		// A handler that only flipped a flag without stopping the container
		// would still be reachable here — so this is the real-stop signal.
		assertURLCleared(t, tenantHost, token, orgID, wsID, 3*time.Minute, "pause")
		assertNotServing(t, tenantHost, token, orgID, wsID, "pause")
		t.Logf("pause VERIFIED: paused + url cleared + container unserveable (genuinely stopped)")

		// resume → provisioning → online + serveable again.
		status, body = postLifecycle(t, tenantHost, token, orgID, wsID, "/resume")
		if status != http.StatusOK {
			t.Fatalf("resume: HTTP %d: %s", status, body)
		}
		if st := jsonField(body, "status"); st != "provisioning" {
			t.Fatalf("resume: body status=%q (expected provisioning): %s", st, body)
		}
		waitForWorkspaceOnlineRoutable(t, tenantHost, token, orgID, wsID, 15*time.Minute, "resume→online")
		assertServes(t, tenantHost, token, orgID, wsID, "post-resume")
		t.Logf("resume VERIFIED: paused → provisioning → online + serveable")
	})

	// ── hibernate → wake ────────────────────────────────────────────────────
	t.Run("hibernate_wake", func(t *testing.T) {
		// Per-subtest online BASELINE (see pause_resume). Guarantees hibernate
		// starts from online+routable regardless of how the previous subtest
		// exited, so /hibernate's online|degraded precondition is met and the
		// subtest is independently meaningful rather than cascading.
		waitForWorkspaceOnlineRoutable(t, tenantHost, token, orgID, wsID, 15*time.Minute, "hibernate_wake baseline")

		// hibernate (force, since a fresh online ws may carry no active tasks
		// but we don't want a transient active_tasks>0 to 409 the test).
		status, body := postLifecycle(t, tenantHost, token, orgID, wsID, "/hibernate?force=true")
		if status != http.StatusOK {
			t.Fatalf("hibernate: HTTP %d: %s", status, body)
		}
		if st := jsonField(body, "status"); st != "hibernated" {
			t.Fatalf("hibernate: body status=%q (expected hibernated): %s", st, body)
		}
		// Confirm it settled at 'hibernated' (not stuck mid-'hibernating') and
		// the container is genuinely stopped (url cleared + unserveable).
		waitForWorkspaceStatus(t, tenantHost, token, orgID, wsID, "hibernated", 3*time.Minute, "hibernate→hibernated")
		assertURLCleared(t, tenantHost, token, orgID, wsID, 3*time.Minute, "hibernate")
		assertNotServing(t, tenantHost, token, orgID, wsID, "hibernate")
		t.Logf("hibernate VERIFIED: hibernated + url cleared + container unserveable")

		// wake: a hibernated workspace auto-wakes on the next incoming A2A
		// message (NOT /resume — Resume only handles status=paused). The wake
		// A2A itself may return transient 5xx while the container re-provisions;
		// the load-bearing contract is the STATUS transition back to online.
		sendWakeA2A(t, tenantHost, token, orgID, wsID)
		waitForWorkspaceOnlineRoutable(t, tenantHost, token, orgID, wsID, 15*time.Minute, "hibernate→wake→online")
		assertServes(t, tenantHost, token, orgID, wsID, "post-wake")
		t.Logf("wake VERIFIED: hibernated → online via auto-wake A2A + serveable")
	})
}

// ---------------------------------------------------------------------------
// lifecycle drivers + observable-state assertions
// ---------------------------------------------------------------------------

// postLifecycle POSTs a lifecycle endpoint (path includes any ?query) on the
// tenant ws-server using the tenant admin token (accepted by WorkspaceAuth).
func postLifecycle(t *testing.T, host, token, orgID, wsID, pathAndQuery string) (int, string) {
	t.Helper()
	url := "https://" + host + "/workspaces/" + wsID + pathAndQuery
	return doTenantJSON(t, "POST", url, token, orgID, "")
}

// workspaceStatusAndURL reads the canvas GET /workspaces/:id and returns
// (status, url). url is "" when the workspace is not routable (paused/hibernated
// clear it). httpStatus is surfaced so callers can distinguish 404/Gone.
//
// CRITICAL — read the TOP-LEVEL url, not the nested agent_card.url. The GET
// response embeds the agent's self-reported endpoint at agent_card.url, and gin
// marshals the response map with keys sorted alphabetically, so "agent_card"
// serializes BEFORE the top-level "url". A naive flat jsonField(body,"url") would
// therefore match agent_card.url FIRST — and the lifecycle handlers only clear the
// TOP-LEVEL url on pause/hibernate (agent_card is display identity, retained across
// pause). That made assertURLCleared read the never-cleared agent_card.url and fail
// "url never cleared" even though the container WAS stopped and the top-level url
// WAS cleared (verified live on staging: container GONE, DB workspaces.url=”). Parse
// the top level with encoding/json so we read the load-bearing routability signal.
func workspaceStatusAndURL(t *testing.T, host, token, orgID, wsID string) (httpStatus int, status, url string) {
	t.Helper()
	u := "https://" + host + "/workspaces/" + wsID
	hs, body := doTenantJSON(t, "GET", u, token, orgID, "")
	return hs, topLevelString(body, "status"), topLevelString(body, "url")
}

// topLevelString decodes the JSON object in body and returns the TOP-LEVEL string
// value for key (or "" on any decode error / missing / non-string). Unlike the
// flat jsonField scanner it never descends into nested objects, so a nested field
// that happens to share the key name (e.g. agent_card.url) can't shadow the
// top-level value.
func topLevelString(body, key string) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return ""
	}
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// waitForWorkspaceStatus polls the canvas GET until .status == want.
func waitForWorkspaceStatus(t *testing.T, host, token, orgID, wsID, want string, timeout time.Duration, why string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		_, st, _ := workspaceStatusAndURL(t, host, token, orgID, wsID)
		if st != last {
			t.Logf("    [%s] status → %q", why, st)
			last = st
		}
		if st == want {
			return
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("%s: workspace %s never reached status=%q within %s (last=%q)", why, wsID, want, timeout, last)
}

// waitForWorkspaceOnlineRoutable polls until status=online AND url is non-empty.
// A routable url is the real "the agent is reachable" signal the SDK uses — an
// online row without a url is not yet serveable.
func waitForWorkspaceOnlineRoutable(t *testing.T, host, token, orgID, wsID string, timeout time.Duration, why string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastStatus, lastURL string
	for time.Now().Before(deadline) {
		_, st, url := workspaceStatusAndURL(t, host, token, orgID, wsID)
		if st != lastStatus || (url != "") != (lastURL != "") {
			t.Logf("    [%s] status=%q routable=%v", why, st, url != "")
			lastStatus, lastURL = st, url
		}
		if st == "online" && url != "" {
			return
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("%s: workspace %s never reached online+routable within %s (last status=%q, url-set=%v)",
		why, wsID, timeout, lastStatus, lastURL != "")
}

// assertURLCleared asserts the canvas GET reports an empty url within timeout.
// Pause/Hibernate SET url=” as part of stopping the container; a non-empty url
// means the workspace is still routable (container not stopped).
func assertURLCleared(t *testing.T, host, token, orgID, wsID string, timeout time.Duration, why string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastURL string
	for time.Now().Before(deadline) {
		_, _, url := workspaceStatusAndURL(t, host, token, orgID, wsID)
		lastURL = url
		if url == "" {
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("%s: workspace %s url never cleared within %s (last url-set=%v) — container may not have actually stopped",
		why, wsID, timeout, lastURL != "")
}

// serveProbe sends one A2A message/send to the workspace and reports whether the
// agent served it (2xx). A 2xx means a live container handled the request; a
// connection error / 5xx / 4xx means it did not serve.
func serveProbe(t *testing.T, host, token, orgID, wsID string) (served bool, code int) {
	t.Helper()
	url := "https://" + host + "/workspaces/" + wsID + "/a2a"
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"message/send","id":"e2e-probe","params":{"message":{"role":"user","messageId":%q,"parts":[{"kind":"text","text":"platform lifecycle e2e serve probe — reply with the single token: PONG"}]}}}`,
		fmt.Sprintf("e2e-probe-%d", time.Now().UnixNano()))
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build serve probe: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Molecule-Org-Id", orgID)
	req.Header.Set("Origin", "https://"+host)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, 0
	}
	defer resp.Body.Close()
	drain(resp)
	return resp.StatusCode >= 200 && resp.StatusCode < 300, resp.StatusCode
}

// assertServes requires the workspace to serve an A2A round-trip within a short
// readiness window (it may have just transitioned to online; allow brief warmup
// + tolerate transient cold 5xx, same edge class the shell harness tolerates).
func assertServes(t *testing.T, host, token, orgID, wsID, why string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	var lastCode int
	for time.Now().Before(deadline) {
		served, code := serveProbe(t, host, token, orgID, wsID)
		lastCode = code
		if served {
			return
		}
		time.Sleep(15 * time.Second)
	}
	t.Fatalf("%s: workspace %s never served an A2A round-trip within 5m (last http=%d) — online but not serveable",
		why, wsID, lastCode)
}

// assertNotServing requires the workspace to STOP serving A2A within timeout —
// the observable proxy available through the tenant API that the container is
// genuinely stopped, not merely flagged paused/hibernated.
//
// NOTE: a hibernated workspace auto-wakes on the NEXT A2A message — so a single
// probe could itself trigger a wake. We therefore look for the workspace to be
// unreachable on the FIRST probe taken after the status/url already settled to
// stopped; we do not retry-poll the probe (that would wake it). A live-and-
// serving container returns 2xx immediately, which is the regression we catch.
//
// TODO(core#2332): the strongest "container stopped" signal is the provider's
// actual container/process state, which is owned by the CP/provisioner and is
// not exposed to this tenant ws-server test. This asserts the strongest signal
// available here (url cleared + immediate non-serve). If a CP-side admin
// endpoint surfaces provider state to the tenant API, tighten this assertion.
func assertNotServing(t *testing.T, host, token, orgID, wsID string, why string) {
	t.Helper()
	// The status/url already settled to stopped before this is called. One
	// probe — not a retry loop — to avoid auto-waking a hibernated workspace.
	served, code := serveProbe(t, host, token, orgID, wsID)
	if served {
		t.Fatalf("%s: workspace %s STILL serves A2A (http=%d) after status settled to stopped — "+
			"container was not actually stopped (handler flipped the flag only)", why, wsID, code)
	}
	t.Logf("    [%s] workspace unserveable after stop (probe http=%d) — container genuinely stopped", why, code)
}

// sendWakeA2A sends a wake message to a hibernated workspace. The wake A2A may
// itself return transient 5xx while the container re-provisions — we send it
// best-effort with bounded retries on the cold-restart 5xx class and let the
// caller assert the real contract (status → online).
func sendWakeA2A(t *testing.T, host, token, orgID, wsID string) {
	t.Helper()
	for attempt := 1; attempt <= 12; attempt++ {
		served, code := serveProbe(t, host, token, orgID, wsID)
		if served {
			t.Logf("    wake A2A served (http=%d) on attempt %d", code, attempt)
			return
		}
		// 5xx / 0 (conn refused while container is down) are expected during
		// cold wake — retry. The wake has still been dispatched (it reaches the
		// ProxyA2A handler, which triggers re-provision); we just couldn't get a
		// 2xx synchronously. Keep nudging until the status assertion takes over.
		t.Logf("    wake A2A attempt %d/12: http=%d (cold restart) — retrying", attempt, code)
		time.Sleep(15 * time.Second)
	}
	t.Logf("    wake A2A did not return 2xx within retries — relying on status→online assertion to confirm wake")
}

// drain reads and discards a response body (cap 1 MiB) so the connection can be
// reused / closed cleanly.
func drain(resp *http.Response) {
	buf := make([]byte, 4096)
	total := 0
	for {
		n, e := resp.Body.Read(buf)
		total += n
		if e != nil || total > 1<<20 {
			break
		}
	}
}

// ---------------------------------------------------------------------------
// harness (self-contained — this package is excluded from the default build).
// Mirrors the idioms of cp's internal/staginge2e (cp#386): STAGING_E2E=1 gate,
// CP_ADMIN_API_TOKEN admin surface, provision→wait-online→assert, t.Cleanup
// teardown. Core has no CP client packages, so these are HTTP-only.
// ---------------------------------------------------------------------------

type stagingCfg struct {
	cpBase          string
	adminToken      string
	subdomainSuffix string
}

// requireStagingEnv gates the suite. STAGING_E2E != 1 SKIPs (the suite's
// contract — advisory-by-infra, not fail-open within a run). With STAGING_E2E=1
// but creds absent it also skips LOUD (so a misconfigured CI run can't false-
// green by silently passing zero assertions).
func requireStagingEnv(t *testing.T) stagingCfg {
	t.Helper()
	if os.Getenv("STAGING_E2E") != "1" {
		t.Skip("STAGING_E2E != 1 — skipping live staging e2e (set STAGING_E2E=1 + CP_BASE_URL + CP_ADMIN_API_TOKEN to run)")
	}
	get := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	cfg := stagingCfg{
		cpBase:          strings.TrimRight(get("CP_BASE_URL"), "/"),
		adminToken:      get("CP_ADMIN_API_TOKEN"),
		subdomainSuffix: envOr("STAGING_TENANT_SUBDOMAIN_SUFFIX", "staging.moleculesai.app"),
	}
	var missing []string
	for k, v := range map[string]string{
		"CP_BASE_URL":        cfg.cpBase,
		"CP_ADMIN_API_TOKEN": cfg.adminToken,
	} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Skipf("STAGING_E2E=1 but missing required env: %s — skipping LOUD (not a silent pass)", strings.Join(missing, ", "))
	}
	if err := validateStagingCPBase(cfg.cpBase); err != nil {
		t.Fatalf("unsafe CP_BASE_URL for staging E2E: %v", err)
	}
	return cfg
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// e2eSlug builds the slug for an ephemeral e2e org.
//
// It keeps the `e2e-<tag>` prefix so CP's IsEphemeral classifier still tags the
// org throwaway, and — crucially — embeds the CI run-id (GITHUB_RUN_ID) so the
// workflow-level `if: always()` teardown net can sweep a leaked org by run-id
// even when Go's t.Cleanup never fires. `go test -timeout` firing and a
// SIGKILL'd runner both SKIP t.Cleanup, so the old run-id-less slug form
// (`e2e-<tag>-<unixtime>`) could not be matched by the always()-net and leaked
// until the age-guarded reaper eventually caught it. Embedding the run-id closes
// that gap: belt (t.Cleanup on the happy/failure path) and braces (the
// run-id-scoped always()-net on timeout/kill).
//
// On local runs GITHUB_RUN_ID is empty, so it falls back to the legacy
// unix-timestamp form. A short 24-bit random suffix keeps parallel/rerun slugs
// collision-resistant while staying well under the DNS-label / CP slug limit.
func e2eSlug(tag string) string {
	if runID := strings.TrimSpace(os.Getenv("GITHUB_RUN_ID")); runID != "" {
		var entropy [3]byte
		if _, err := cryptorand.Read(entropy[:]); err == nil {
			return fmt.Sprintf("e2e-%s-%s-%s", tag, runID, hex.EncodeToString(entropy[:]))
		}
		// A crypto/rand outage should not turn slug construction into a silent
		// empty value. Preserve the exact six-hex contract with a clock fallback;
		// the create endpoint still rejects any real collision.
		fallback := uint64(time.Now().UnixNano()) & 0xffffff
		return fmt.Sprintf("e2e-%s-%s-%06x", tag, runID, fallback)
	}
	return fmt.Sprintf("e2e-%s-%d", tag, time.Now().Unix()%100000000)
}

// adminCreateOrg provisions a throwaway org via the CP admin API, immediately
// registers exact-slug cleanup, and then waits for its instance to reach
// running (provisioning is async). Cleanup must be registered here rather than
// by callers: a provisioning timeout calls t.Fatalf before this function
// returns, so caller-owned cleanup would never be installed.
func adminCreateOrg(t *testing.T, cfg stagingCfg, slug string) (orgID string) {
	t.Helper()
	// Compute backend for the throwaway tenant. Defaults to molecules-server (the local-docker
	// backend) now that the AWS EC2 path is closed. The canonical cloudprovider SSOT wire id
	// the CP org-create validates (IsValidRequest) and persists as organizations.provider
	// "local" (PersistKey); an empty provider would fall back to the CP DefaultProvider (the
	// closed AWS path). The e2e workflows export E2E_PROVIDER; envOr pins the local default.
	provider := envOr("E2E_PROVIDER", "molecules-server")
	body := fmt.Sprintf(`{"slug":%q,"name":%q,"owner_user_id":%q,"provider":%q}`, slug, "E2E Workspace Lifecycle", "e2e-runner:"+slug, provider)
	status, resp := doJSON(t, "POST", cfg.cpBase+"/cp/admin/orgs", cfg.adminToken, body)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("AdminCreate org: HTTP %d: %s", status, resp)
	}
	// A 200/201 means the server accepted the exact slug. Schedule targeted
	// cleanup before parsing the response so a malformed success body still
	// triggers a teardown attempt.
	registerTenantCleanup(t, cfg, slug)
	id := jsonField(resp, "id")
	if id == "" {
		t.Fatalf("AdminCreate org: no id in response: %s", resp)
	}
	deadline := time.Now().Add(7 * time.Minute)
	for time.Now().Before(deadline) {
		st, list := doJSON(t, "GET", cfg.cpBase+"/cp/admin/orgs", cfg.adminToken, "")
		if st == http.StatusOK && strings.Contains(list, `"slug":"`+slug+`"`) &&
			orgInstanceStatus(list, slug) == "running" {
			return id
		}
		time.Sleep(15 * time.Second)
	}
	t.Fatalf("org %s did not reach instance_status=running within timeout", slug)
	return ""
}

func registerTenantCleanup(t *testing.T, cfg stagingCfg, slug string) {
	t.Helper()
	t.Cleanup(func() { adminDeleteTenant(t, cfg, slug) })
}

func adminDeleteTenant(t *testing.T, cfg stagingCfg, slug string) {
	t.Helper()
	if err := deleteTenantAndVerify(cfg, slug, 3*time.Minute, 5*time.Second); err != nil {
		// Cleanup is part of the live contract. A green functional assertion
		// with a leaked tenant is not a passing E2E, so fail the test closed.
		t.Errorf("teardown failed for exact tenant %s: %v", slug, err)
		return
	}
	t.Logf("teardown: deleted exact tenant %s and verified it absent", slug)
}

// deleteTenantAndVerify retries the one transient response the CP emits while
// an asynchronous lifecycle operation is still settling (HTTP 409), then
// proves the exact slug is absent through the CP's identity-bound tenant
// endpoint (404). It is deliberately bounded and exact-slug-only: no fleet
// sweep and no false-green warning path.
func deleteTenantAndVerify(cfg stagingCfg, slug string, timeout, pollInterval time.Duration) error {
	if cfg.cpBase == "" || cfg.adminToken == "" {
		return fmt.Errorf("control-plane base URL and admin token are required")
	}
	if err := validateStagingCPBase(cfg.cpBase); err != nil {
		return err
	}
	if !validE2ESlug(slug) {
		return fmt.Errorf("refusing cleanup for non-E2E slug %q", slug)
	}
	if timeout <= 0 || pollInterval <= 0 {
		return fmt.Errorf("cleanup timeout and poll interval must be positive")
	}

	requestTimeout := 30 * time.Second
	if timeout < requestTimeout {
		requestTimeout = timeout
	}
	client := &http.Client{
		Timeout: requestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	deadline := time.Now().Add(timeout)
	body := fmt.Sprintf(`{"confirm":%q}`, slug)
	var lastStatus int
	var lastDetail string

	for {
		status, respBody, err := cleanupJSONRequest(
			client,
			http.MethodDelete,
			strings.TrimRight(cfg.cpBase, "/")+"/cp/admin/tenants/"+slug,
			cfg.adminToken,
			body,
		)
		lastStatus = status
		if err != nil {
			lastDetail = err.Error()
		} else {
			lastDetail = cleanupResponseDetail(respBody)
			switch {
			case status == http.StatusOK || status == http.StatusAccepted || status == http.StatusNotFound:
				absent, verifyErr := exactTenantAbsent(client, cfg, slug)
				if verifyErr == nil && absent {
					return nil
				}
				if verifyErr != nil {
					lastDetail = "absence verification: " + verifyErr.Error()
				} else {
					lastDetail = "DELETE accepted but exact tenant identity still exists"
				}
			case status == http.StatusConflict || status == http.StatusRequestTimeout ||
				status == http.StatusTooManyRequests || status >= http.StatusInternalServerError:
				// Retry below. A 409 is expected while a just-completed test still
				// has an active lifecycle operation; transport/5xx/429 failures are
				// also bounded rather than converted into a false green.
			default:
				return fmt.Errorf("DELETE returned non-retryable HTTP %d: %s", status, lastDetail)
			}
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("cleanup deadline exhausted after HTTP %d: %s", lastStatus, lastDetail)
		}
		sleepFor := pollInterval
		if remaining < sleepFor {
			sleepFor = remaining
		}
		time.Sleep(sleepFor)
	}
}

func exactTenantAbsent(client *http.Client, cfg stagingCfg, slug string) (bool, error) {
	status, body, err := cleanupJSONRequest(
		client,
		http.MethodGet,
		fmt.Sprintf("%s/cp/admin/tenants/%s/boot-events?limit=1", strings.TrimRight(cfg.cpBase, "/"), slug),
		cfg.adminToken,
		"",
	)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		return true, nil
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("exact tenant identity returned HTTP %d: %s", status, cleanupResponseDetail(body))
	}
	var identity struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal([]byte(body), &identity); err != nil {
		return false, fmt.Errorf("decode exact tenant identity: %w", err)
	}
	if identity.Slug != slug {
		return false, fmt.Errorf("exact tenant identity mismatch: got %q", identity.Slug)
	}
	return false, nil
}

func validateStagingCPBase(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("invalid control-plane URL")
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Scheme == "https" && host == "staging-api.moleculesai.app" {
		return nil
	}
	if parsed.Scheme == "http" && (host == "127.0.0.1" || host == "::1" || host == "localhost") {
		return nil
	}
	return fmt.Errorf("refusing to send staging admin bearer to %s://%s", parsed.Scheme, host)
}

func validE2ESlug(slug string) bool {
	if len(slug) == 0 || len(slug) > 63 ||
		(!strings.HasPrefix(slug, "e2e-") && !strings.HasPrefix(slug, "rt-e2e-")) {
		return false
	}
	for _, r := range slug {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func cleanupJSONRequest(client *http.Client, method, url, token, body string) (int, string, error) {
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("build %s request: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("%s request: %w", method, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readBody(resp), nil
}

func cleanupResponseDetail(body string) string {
	detail := strings.Join(strings.Fields(body), " ")
	if len(detail) > 300 {
		return detail[:300] + "..."
	}
	return detail
}

// tenantAdminToken fetches the per-tenant admin token from the CP admin surface.
// Only available once the tenant platform has finished provisioning.
func tenantAdminToken(t *testing.T, cfg stagingCfg, slug string) string {
	t.Helper()
	url := cfg.cpBase + "/cp/admin/orgs/" + slug + "/admin-token"
	deadline := time.Now().Add(7 * time.Minute)
	for time.Now().Before(deadline) {
		status, body := doJSON(t, "GET", url, cfg.adminToken, "")
		if status == http.StatusOK {
			if tok := jsonField(body, "admin_token"); tok != "" {
				return tok
			}
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("tenant admin token not available for %s within timeout", slug)
	return ""
}

// tenantCreateWorkspace creates a default-runtime workspace via the tenant
// ws-server, exercising the full tenant → CP provisioner → configured-provider
// path (currently molecules-server/local Docker in staging).
//
// De-hardcode: the runtime + model FOLLOW the same KMS-injection pattern the
// de-hardcode lanes use — E2E_RUNTIME / E2E_MODEL are exported by the e2e
// workflows from the platform default SSOT (MOLECULE_DEFAULT_RUNTIME /
// MOLECULE_LLM_DEFAULT_MODEL @ /shared/controlplane/llm). When unset (local runs),
// envOr falls back to the CURRENT SSOT value (minimax/MiniMax-M2.7), NOT a
// moonshot vendor const, so the local fallback tracks the SSOT default.
func tenantCreateWorkspace(t *testing.T, cfg stagingCfg, host, token, orgID string) string {
	t.Helper()
	return tenantCreateWorkspaceWithRuntime(
		t, cfg, host, token, orgID, "core2332-life-e2e",
		envOr("E2E_RUNTIME", "claude-code"),
		envOr("E2E_MODEL", "minimax/MiniMax-M2.7"),
	)
}

// tenantCreateWorkspaceWithRuntime creates a workspace on a specific runtime +
// model. Both must be a PLATFORM-namespaced pairing so billing resolves
// platform_managed (no BYOK credential needed at create) — used by the
// external-runtime request e2e (codex + the cheap openai/gpt-5.4-mini).
func tenantCreateWorkspaceWithRuntime(t *testing.T, cfg stagingCfg, host, token, orgID, name, runtime, model string) string {
	t.Helper()
	url := "https://" + host + "/workspaces"
	body := fmt.Sprintf(
		`{"name":%q,"runtime":%q,"tier":%d,"model":%q,"provider":%q}`,
		name, runtime, 1, model, "platform",
	)
	// Cold-origin 503 retry (#91, RCA run 527280): the create POST intermittently
	// returns an EMPTY-body 503 in the ~1-2s Cloudflare cold-origin window right
	// after /health went green — the request never reached the ws-server handler.
	// Retry ONLY that "never reached a handler" signature (empty-body 503 or a
	// transport error surfaced as status 0), honoring the origin's Retry-After.
	// A non-empty body, or a 502/504 (a non-idempotent POST that may already have
	// been processed), is surfaced on the first try — never re-POSTed. This
	// mirrors the shell (tests/e2e/lib/workspace_create_retry.sh) and TS
	// (canvas workspaceCreateRetry.ts) classifiers so all three create seams
	// share ONE non-masking rule.
	const maxAttempts = 4
	var status int
	var resp string
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var retryAfter string
		status, resp, retryAfter = doTenantCreateOnce(t, url, token, orgID, body)
		if status == http.StatusCreated || status == http.StatusOK {
			id := jsonField(resp, "id")
			if id == "" {
				t.Fatalf("tenant workspace create: no id in response: %s", resp)
			}
			return id
		}
		if coldCreateShouldRetry(status, resp) && attempt < maxAttempts {
			delay := parseColdRetryAfter(retryAfter)
			t.Logf("tenant workspace create (runtime=%s model=%s): cold-origin retryable HTTP %d (empty body), attempt %d/%d — sleeping %ds (Retry-After=%q) [#91]",
				runtime, model, status, attempt, maxAttempts, delay, retryAfter)
			time.Sleep(time.Duration(delay) * time.Second)
			continue
		}
		break
	}
	t.Fatalf("tenant workspace create (runtime=%s model=%s): HTTP %d: %s", runtime, model, status, resp)
	return ""
}

// doTenantCreateOnce issues the SAME tenant create request as doTenantJSON (the
// three SaaS-auth headers + Content-Type, the same 90s client) but RETURNS the
// Retry-After header and, on a transport error, status=0 with empty body so the
// caller's retry loop — not this helper — decides. It deliberately does NOT
// t.Fatalf on a transport error: a connection reset / never-established socket
// in the cold-origin window is exactly the retryable "never reached a handler"
// signal, and t.Fatalf here would kill the test before the loop could retry.
func doTenantCreateOnce(t *testing.T, url, token, orgID, body string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build POST %s: %v", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Molecule-Org-Id", orgID)
	req.Header.Set("Origin", "https://"+strings.SplitN(strings.TrimPrefix(url, "https://"), "/", 2)[0])
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		if coldCreateTransportRetryable(err) {
			// Connection reset / refused / never-established in the cold window:
			// no handler was reached, so model as status 0 (no status line) and
			// let coldCreateShouldRetry retry — the transport twin of curl `000`.
			return 0, "", ""
		}
		// Client-side timeout (http.Client.Timeout / context deadline) or abort:
		// the origin may already have PROCESSED this non-idempotent POST before
		// the client stopped waiting, so a re-POST would DOUBLE-CREATE. Surface a
		// non-retryable sentinel — status -1 AND a non-empty body each
		// independently force coldCreateShouldRetry to false — so the loop stops.
		return -1, "transport error (maybe-processed, not retried): " + err.Error(), ""
	}
	defer resp.Body.Close()
	return resp.StatusCode, readBody(resp), resp.Header.Get("Retry-After")
}

// coldCreateTransportRetryable classifies a client.Do transport error (an error
// return with NO HTTP response). It is the Go twin of the shell's curl-exit
// gate and the TS classifier's TypeError-vs-TimeoutError split:
//
//   - A CLIENT-side timeout — http.Client.Timeout, a request-context deadline,
//     or any net.Error whose Timeout() is true — is maybe-processed: the origin
//     may already have handled the non-idempotent POST /workspaces before the
//     client gave up, so re-POSTing risks a DOUBLE-CREATE. NOT retryable.
//     (Mirrors the TS classifier refusing TimeoutError/AbortError and the shell
//     refusing curl exit 28.)
//   - Everything else at the transport layer — connection refused / reset by
//     peer / never-established socket / EOF — never reached a handler, so it is
//     the safe cold-origin transient to retry (the twin of curl `000`).
func coldCreateTransportRetryable(err error) bool {
	if err == nil {
		return false
	}
	// http.Client.Timeout cancels the request context, surfacing (possibly
	// wrapped in *url.Error) as context.DeadlineExceeded.
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Dial/read/write deadlines and other timeouts surface as a net.Error with
	// Timeout()==true; *url.Error forwards Timeout() from the error it wraps.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return false
	}
	// Non-timeout transport failure (reset/refused/never-established) → retry.
	return true
}

// coldCreateShouldRetry is the pure, non-masking classifier for the create POST.
// It mirrors create_should_retry_cold (shell) / shouldRetryColdCreate (TS)
// byte-for-byte: retry ONLY a "never reached a handler" signature.
//
//   - A NON-EMPTY body is a real response from the ws-server handler (which
//     always emits a body — e.g. a 422/400 JSON error). Emptiness IS the
//     "not the handler" signal, so a non-empty body is NEVER retried; it is
//     surfaced so a genuine regression cannot be masked by a retry.
//   - status 503 with an empty body → the edge/ingress "Service Unavailable"
//     synthesised before a handler is up (the RCA'd cold-origin signature).
//   - status 0 (transport error / connection reset) → the same cold window at
//     the transport layer.
//   - 502 / 504 (and everything else) → NOT retried: a bad-gateway /
//     gateway-timeout on a non-idempotent POST may already have been processed,
//     so re-POSTing risks a double-create.
func coldCreateShouldRetry(status int, body string) bool {
	if strings.TrimSpace(body) != "" {
		return false
	}
	switch status {
	case 503, 0:
		return true
	default:
		return false
	}
}

// parseColdRetryAfter honors ONLY a bare integer delta-seconds Retry-After
// (RFC 7231 also allows an HTTP-date; we do not follow a date). Anything that is
// not all-digits after stripping whitespace — an HTTP-date, empty, or junk —
// falls back to the default rather than being mangled into a huge value.
// Default 2, capped at 10 so a hostile/large value can't stall the whole gate.
func parseColdRetryAfter(raw string) int {
	s := strings.Join(strings.Fields(raw), "") // strip all whitespace (mirror shell tr -d)
	if s == "" {
		return 2
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 2
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 2
	}
	if n > 10 {
		n = 10
	}
	return n
}

// --- reachability ----------------------------------------------------------

func waitForHTTP(t *testing.T, host string, want int, timeout time.Duration, why string) {
	t.Helper()
	url := "https://" + host + "/health"
	client := &http.Client{Timeout: 15 * time.Second}
	deadline := time.Now().Add(timeout)
	var last int
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", url, nil)
		resp, err := client.Do(req)
		if err == nil {
			last = resp.StatusCode
			resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("%s: %s never returned HTTP %d within %s (last=%d)", why, url, want, timeout, last)
}

// waitForTenantRoute gates on a REAL proxied tenant route (not the allowlisted
// /health) serving a STABLE consecutive-200 streak before the caller asserts
// against it. The CP publishes instance_status=running on a /health-only canary
// (controlplane#1012 — internal/provisioner/canary.go probes only /health), so
// the first proxied API call (e.g. GET /plugins, GET /workspaces) can transiently
// 502/503 while the app finishes wiring under load. This is a readiness gate, not
// a retry-until-green mask: a genuinely half-wired tenant never reaches the streak
// and this fails loudly; the caller's content assertions still run afterwards.
func waitForTenantRoute(t *testing.T, host, path, token, orgID string, wantStreak int, timeout time.Duration, why string) {
	t.Helper()
	url := "https://" + host + path
	client := &http.Client{Timeout: 15 * time.Second}
	deadline := time.Now().Add(timeout)
	streak, last := 0, 0
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Molecule-Org-Id", orgID)
		req.Header.Set("Origin", "https://"+host)
		resp, err := client.Do(req)
		if err == nil {
			last = resp.StatusCode
			// 401/403 are definitive auth failures, not a boot-window condition —
			// fail fast instead of burning the whole deadline and mis-blaming the
			// provisioner.
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				resp.Body.Close()
				t.Fatalf("%s: %s returned HTTP %d — an AUTH failure (bad tenant token/org-id), not a readiness/half-wired issue", why, url, resp.StatusCode)
			}
			if resp.StatusCode == http.StatusOK {
				// Reject a 200 whose body is the canvas SPA HTML fallback (route
				// not registered yet): the assertion this gates needs JSON.
				body := readBody(resp)
				resp.Body.Close()
				trimmed := strings.TrimSpace(body)
				if len(trimmed) > 0 && trimmed[0] != '<' {
					streak++
					if streak >= wantStreak {
						return
					}
					time.Sleep(3 * time.Second)
					continue
				}
				streak = 0
				time.Sleep(3 * time.Second)
				continue
			}
			resp.Body.Close()
		}
		streak = 0
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("%s: %s never served a stable %dx200 JSON within %s (last=%d) — persistent half-wired tenant (controlplane#1012), not a transient boot window", why, url, wantStreak, timeout, last)
}

// --- HTTP helpers ----------------------------------------------------------

// doJSON hits the CP admin surface (bearer admin token, no tenant headers).
func doJSON(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 150 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readBody(resp)
}

// doTenantJSON hits the tenant ws-server. It adds the three headers the SaaS
// auth chain requires: Authorization (tenant admin token), X-Molecule-Org-Id
// (tenant guard 404s anything without it), and Origin (Cloudflare WAF rejects a
// mismatched/absent Origin with 404).
func doTenantJSON(t *testing.T, method, url, token, orgID, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Molecule-Org-Id", orgID)
	req.Header.Set("Origin", "https://"+strings.SplitN(strings.TrimPrefix(url, "https://"), "/", 2)[0])
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readBody(resp)
}

func readBody(resp *http.Response) string {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, e := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if e != nil || len(buf) > 1<<20 {
			break
		}
	}
	return string(buf)
}

// jsonField does a flat, dependency-free extraction of a top-level string field
// value ("key":"value") — sufficient for the id/status/url fields we read.
func jsonField(body, key string) string {
	needle := `"` + key + `":"`
	i := strings.Index(body, needle)
	if i < 0 {
		return ""
	}
	rest := body[i+len(needle):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// orgInstanceStatus lives in orginstancestatus.go (untagged) so its regression
// test runs in the normal `go test ./...` gate, not only under -tags staging_e2e.

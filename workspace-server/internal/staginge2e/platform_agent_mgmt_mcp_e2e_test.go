//go:build staging_e2e

package staginge2e

// platform_agent_mgmt_mcp_e2e_test.go — HARD-GATE assertion that a FRESH org's
// PLATFORM AGENT (the concierge auto-installed at org-create — the exact path the
// UI/user takes) actually comes up with its MANAGEMENT MCP present and its
// lifecycle verb (provision_workspace) LOADED/callable, not merely /health-200.
//
// WHY THIS EXISTS (operator "test14" — a fresh staging org whose platform agent
// instantly failed):
//
//	The pre-existing staging gates provision a PLAIN default-runtime workspace
//	workspace with NO declared plugins (TestWorkspaceLifecycle_Staging) or drive
//	the concierge WITHOUT waiting for it to boot online (TestConciergePlatformAgent
//	_Staging asserts only DB/handler state — kind/parent_id/config-tab auth). A
//	plain workspace reaches online WITHOUT any management MCP, and the concierge
//	test never waits for online, so BOTH stay green even when a fresh platform
//	agent can NEVER mark online because its mgmt-MCP plugin was not installed. That
//	is exactly the "checks presence/serve-text, not mgmt-MCP callability" flaw the
//	prod-deploy hard-gate rule warns about.
//
//	test14's concrete failure: the control-plane rewrote the concierge's mgmt-MCP
//	declared plugin source from gitea://…  to  presign://<name> (server-side R2
//	relay delivery) and dropped the plugin tree into <config>/.relay-plugins/<name>/,
//	but the DEPLOYED workspace-runtime image had only the `gitea` plugin provider
//	(no `presign` provider — workspace-runtime #229 adds it), so the boot-install
//	skipped the source ("skip unsupported source: presign://…"), the plugin never
//	landed in /configs/plugins, the mgmt MCP was absent, and the heartbeat
//	fail-closed (RCA #2970) refused to mark the concierge online — permanently.
//
// This test makes that failure RED AT THE GATE: on the broken fleet the platform
// agent never reaches online, so the assertion fails; once the presign-consumer
// runtime is deployed it passes. The assertion is DETERMINISTIC (no LLM
// tool-call nondeterminism): a kind='platform' agent is, BY CONSTRUCTION of the
// RCA #2970 gate, unable to reach status=online unless mcp_server_present=true,
// so waiting for the concierge to go online IS the mgmt-MCP-present contract. We
// additionally assert, when the tenant surfaces them, that the row reports
// mcp_server_present=true and that loaded_mcp_tools carries the contract's
// provision_workspace verb — tightening "present" to "the lifecycle verb the org
// actually needs is loaded".

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	molcontracts "go.moleculesai.app/sdk/gen/go/molcontracts"
)

func TestPlatformAgentMgmtMCP_Staging(t *testing.T) {
	cfg := requireStagingEnv(t)

	slug := e2eSlug("mcp")
	t.Logf("platform-agent-mgmt-mcp: slug=%s", slug)

	// --- Fresh org via the admin API (the same org-create path the UI drives) ---
	orgID := adminCreateOrg(t, cfg, slug)
	t.Logf("org created: org_id=%s", orgID)

	token := tenantAdminToken(t, cfg, slug)
	host := slug + "." + cfg.subdomainSuffix
	waitForHTTP(t, host, http.StatusOK, 10*time.Minute, "tenant /health ready")
	t.Logf("tenant TLS ready: %s", host)

	// ── Guard B ORDERING FIX: assert we are gating the DEPLOY-CANDIDATE image ──
	// The deploy audit found the e2e ran against the PRE-ADVANCE pin, not the
	// candidate — so a broken candidate could roll to the fleet while the gate
	// exercised the (good) OLD image. When the deploy path exports
	// E2E_EXPECT_TENANT_BUILD_SHA (the :staging-<sha> the pipeline is rolling to),
	// assert the tenant's own /buildinfo git_sha matches it. A mismatch means the
	// gate is NOT exercising the candidate → HARD FAIL (turn the silent ordering
	// bug into a red gate), rather than a false green on a stale image.
	assertTenantBuildIsCandidate(t, host, token, orgID)

	// The platform agent (concierge) is auto-installed at org-create. A fresh org
	// with no platform-agent root is itself a regression (nothing can manage the
	// org), so allow a short backfill window then fail loud.
	platformID := findPlatformRoot(t, host, token, orgID)
	if platformID == "" {
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) && platformID == "" {
			time.Sleep(10 * time.Second)
			platformID = findPlatformRoot(t, host, token, orgID)
		}
	}
	if platformID == "" {
		t.Fatalf("fresh org %s has NO platform-agent root — the concierge was never installed "+
			"(cannot manage the org)", slug)
	}
	t.Logf("platform agent (concierge) id: %s", platformID)

	// The management-MCP lifecycle verb the concierge MUST load. SSOT:
	// molecule-ai-sdk — the SAME literal the server-side heartbeat gate matches
	// (handlers.conciergePlatformMCPProvisionWorkspaceTool). Never hardcoded here.
	requiredTool := "mcp__" + molcontracts.MCPServerName + "__" + molcontracts.RequiredTool

	// The DEFAULT runtime the fresh concierge MUST be on. De-hardcoded via
	// E2E_DEFAULT_RUNTIME, which the deploy path exports from the SSOT
	// (MOLECULE_DEFAULT_RUNTIME @ /shared/controlplane). The fallback mirrors the
	// compiled-in product default for local/manual runs. The gate stays
	// default-specific so a real default-flip skew onto the WRONG runtime is still
	// caught, not silently passed.
	expectedRuntime := envOr("E2E_DEFAULT_RUNTIME", "hermes")

	// Whether to require the REAL A2A callable turn (the deep proof) vs the
	// online/presence-only assertion. The deploy gate sets
	// E2E_ASSERT_MGMT_MCP_CALLABLE=1 so a fresh-org provision_workspace call is
	// genuinely exercised; local runs default OFF (presence-only) to stay cheap.
	assertCallable := isTruthy(envOr("E2E_ASSERT_MGMT_MCP_CALLABLE", ""))

	// Wait for the concierge to reach status=online — which RCA #2970 makes
	// UNREACHABLE unless its management MCP is present — collecting the row-reported
	// signals (mcp_server_present, loaded_mcp_tools, runtime) into a probe.
	deadline := time.Now().Add(15 * time.Minute)
	var probe MgmtMCPProbe
	probe.ExpectedRuntime = expectedRuntime
	probe.RequiredTool = requiredTool
	probe.AssertCallable = assertCallable
	online := false
	var lastStatus, lastPresent, lastTools string
	for time.Now().Before(deadline) {
		hs, body := doTenantJSON(t, "GET", "https://"+host+"/workspaces/"+platformID, token, orgID, "")
		if hs == http.StatusOK {
			status := jsonField(body, "status")
			present, presentReported := jsonBool(body, "mcp_server_present")
			tools := loadedMCPTools(body)
			probe.Status = status
			probe.MCPServerPresent = present
			probe.MCPServerPresentReported = presentReported
			probe.LoadedTools = tools
			probe.ObservedRuntime = observedRuntime(body)
			lastStatus = status
			lastPresent = fmt.Sprintf("%v(reported=%v)", present, presentReported)
			lastTools = strings.Join(tools, ",")

			if status == "online" {
				online = true
				break
			}
		}
		time.Sleep(15 * time.Second)
	}
	if !online {
		t.Fatalf("platform agent %s never reached online WITHIN 15m "+
			"(last status=%q mcp_server_present=%s loaded_mcp_tools=[%s]; required verb=%q).\n"+
			"This is the test14 / broken-openclaw-pin failure mode: the concierge's mgmt-MCP plugin was "+
			"not installed (e.g. a presign:// declared source the deployed runtime cannot resolve) → "+
			"RCA #2970 fail-closed → the platform agent can never manage the org.",
			platformID, lastStatus, lastPresent, lastTools, requiredTool)
	}

	// The CALLABLE proof (Guard B core): drive a REAL A2A tool-use turn asking the
	// concierge to actually RUN provision_workspace, and assert the deterministic
	// side effect — a genuine kind='workspace' row with the requested name appears.
	// Presence-only checks (status/inventory) cannot catch a present-but-not-runnable
	// verb; this can. Only when the deploy gate opts in (E2E_ASSERT_MGMT_MCP_CALLABLE).
	if assertCallable {
		probe.WorkerProvisioned = driveProvisionWorkspaceCallable(t, host, token, orgID, platformID)
	}

	// One verdict, computed by the SAME pure logic the fail-before unit test proves
	// (platform_agent_mgmt_mcp_gate.go / _gate_test.go). RED names the regression class.
	ok, reason := EvaluateMgmtMCPCallable(probe)
	if !ok {
		t.Fatalf("Guard B gate RED for fresh org %s (runtime=%q status=%q present=%s tools=[%s] callable_required=%v): %s",
			slug, probe.ObservedRuntime, probe.Status, lastPresent, lastTools, assertCallable, reason)
	}
	t.Logf("Guard B gate GREEN for fresh org %s (runtime=%q expected=%q present=%s tools=[%s] callable_required=%v): %s",
		slug, probe.ObservedRuntime, expectedRuntime, lastPresent, lastTools, assertCallable, reason)
}

// assertTenantBuildIsCandidate enforces the ORDERING FIX: when the deploy path
// exports E2E_EXPECT_TENANT_BUILD_SHA, the tenant serving this fresh org MUST be
// running that exact candidate build (its /buildinfo git_sha matches). A mismatch
// means the gate is exercising a stale pre-advance pin — HARD FAIL. Unset (local
// runs) → skipped with a log (nothing to compare against).
func assertTenantBuildIsCandidate(t *testing.T, host, token, orgID string) {
	t.Helper()
	want := strings.TrimSpace(envReadFirst("E2E_EXPECT_TENANT_BUILD_SHA", "E2E_CANDIDATE_BUILD_SHA"))
	if want == "" {
		t.Logf("candidate-build guard: E2E_EXPECT_TENANT_BUILD_SHA unset — skipping (local run; nothing to gate the ordering against)")
		return
	}
	// /buildinfo is on the tenant guard allowlist and returns {"git_sha": "..."}.
	deadline := time.Now().Add(3 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		hs, body := doTenantJSON(t, "GET", "https://"+host+"/buildinfo", token, orgID, "")
		if hs == http.StatusOK {
			got := jsonField(body, "git_sha")
			last = got
			if got != "" && buildSHAMatches(got, want) {
				t.Logf("candidate-build guard OK: tenant /buildinfo git_sha=%s matches deploy candidate %s", got, want)
				return
			}
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("ORDERING BUG: tenant %s /buildinfo git_sha=%q does NOT match the deploy candidate %q — "+
		"the gate is exercising a stale pre-advance pin, not the candidate image. The candidate must be in "+
		"front of the gate (advance the pin / roll the canary onto the candidate) BEFORE this e2e runs.",
		host, last, want)
}

// buildSHAMatches compares a tenant-reported git_sha to the expected candidate
// sha tolerant of short/long forms (7-char :staging-<sha> vs full 40-char SHA):
// a prefix match in either direction counts.
func buildSHAMatches(got, want string) bool {
	got = strings.ToLower(strings.TrimSpace(got))
	want = strings.ToLower(strings.TrimSpace(want))
	if got == "" || want == "" {
		return false
	}
	return strings.HasPrefix(got, want) || strings.HasPrefix(want, got)
}

// driveProvisionWorkspaceCallable sends the concierge a real A2A message/send
// turn instructing it to call provision_workspace, then polls GET /workspaces for
// the DETERMINISTIC side effect — a genuine kind='workspace' row with the exact
// name we asked for. Returns true iff that row appears (the verb genuinely ran).
// Cold-start tolerant: retries the A2A POST on 5xx and re-nudges while polling.
func driveProvisionWorkspaceCallable(t *testing.T, host, token, orgID, platformID string) bool {
	t.Helper()
	worker := fmt.Sprintf("e2e-mcp-callable-%s", newUUIDv4(t)[:8])
	prompt := fmt.Sprintf("Please create a new team member workspace in this org right now using your platform "+
		"tools. Use the provision_workspace tool with name exactly %q and role \"engineer\". Do not ask any "+
		"clarifying questions — the name and role are final. After the tool succeeds, reply with the new "+
		"workspace id.", worker)
	payload := a2aMessageSend(t, prompt)

	actBudget := durationOr("E2E_AGENT_ACT_SECS", 420*time.Second)
	url := "https://" + host + "/workspaces/" + platformID + "/a2a"

	sendA2A := func() {
		// Wide per-call window: a cold concierge's first turn opens the LLM
		// connection + loads the platform MCP subprocess before running the tool.
		st, resp := doTenantJSONTimeout(t, "POST", url, token, orgID, payload, actBudget)
		t.Logf("A2A provision_workspace turn → HTTP %d (worker=%s): %s", st, worker, truncate(resp, 200))
	}
	sendA2A()

	deadline := time.Now().Add(actBudget)
	nextNudge := time.Now().Add(75 * time.Second)
	for time.Now().Before(deadline) {
		if id, kind := findWorkspaceByName(t, host, token, orgID, worker); id != "" {
			if kind != "" && kind != "workspace" {
				t.Logf("callable turn produced %q with kind=%q (want workspace) — treating as not-a-real-create", worker, kind)
				return false
			}
			// Best-effort targeted cleanup of the worker the concierge created.
			t.Cleanup(func() {
				_, _ = doTenantJSON(t, "DELETE",
					"https://"+host+"/workspaces/"+id+"?confirm=true", token, orgID, "")
			})
			t.Logf("CALLABLE CONFIRMED: concierge %s ran provision_workspace → workspace %q (id=%s) exists",
				platformID, worker, id)
			return true
		}
		if time.Now().After(nextNudge) {
			t.Logf("worker %q not yet created — re-nudging the concierge (cold-start tolerance)", worker)
			sendA2A()
			nextNudge = time.Now().Add(75 * time.Second)
		}
		time.Sleep(8 * time.Second)
	}
	t.Logf("callable turn: workspace %q never appeared within %s — provision_workspace not genuinely callable", worker, actBudget)
	return false
}

// loadedMCPTools extracts the loaded_mcp_tools string array from a GET
// /workspaces/:id body. Absent / null / non-array → empty (the caller treats an
// empty list as "not surfaced" and leans on the status=online RCA #2970 gate).
func loadedMCPTools(body string) []string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return nil
	}
	raw, ok := m["loaded_mcp_tools"]
	if !ok {
		return nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	return arr
}

// (containsStr moved to platform_agent_mgmt_mcp_gate.go — the untagged gate file —
// so the pure verdict logic and this live test share one definition.)

// isTruthy parses a permissive boolean env value (1/true/yes/on).
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// envReadFirst returns the first non-empty env value among keys (alias support).
func envReadFirst(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// durationOr reads an integer-seconds env var, falling back to def.
func durationOr(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return def
}

// a2aMessageSend builds the JSON-RPC message/send envelope (the canvas user→agent
// chat path, handlers/a2a_proxy.go) carrying a single text part.
func a2aMessageSend(t *testing.T, text string) string {
	t.Helper()
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  "message/send",
		"id":      "e2e-mcp-" + newUUIDv4(t)[:8],
		"params": map[string]any{
			"message": map[string]any{
				"role":      "user",
				"messageId": "e2e-" + newUUIDv4(t)[:8],
				"parts":     []map[string]any{{"kind": "text", "text": text}},
			},
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("a2aMessageSend marshal: %v", err)
	}
	return string(b)
}

// findWorkspaceByName returns (id, kind) of the workspace whose name == want in
// GET /workspaces, or ("","") if absent. The list rows omit name from the shared
// parseWorkspaceList row, so this does its own permissive decode.
func findWorkspaceByName(t *testing.T, host, token, orgID, want string) (id, kind string) {
	t.Helper()
	hs, body := doTenantJSON(t, "GET", "https://"+host+"/workspaces", token, orgID, "")
	if hs != http.StatusOK {
		return "", ""
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return "", ""
	}
	for _, m := range raw {
		if rawString(m["name"]) == want {
			return rawString(m["id"]), rawString(m["kind"])
		}
	}
	return "", ""
}

// doTenantJSONTimeout is doTenantJSON with a caller-set client timeout — an A2A
// tool-use turn on a cold concierge can exceed doTenantJSON's default 90s.
func doTenantJSONTimeout(t *testing.T, method, url, token, orgID, body string, timeout time.Duration) (int, string) {
	t.Helper()
	rewritten, top, err := tenantTopoFromURL(url)
	if err != nil {
		t.Fatalf("tenant topology for %s: %v", url, err)
	}
	req, err := http.NewRequest(method, rewritten, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, rewritten, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Molecule-Org-Id", orgID)
	applyTenantRouting(req, top)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		// A transport timeout on a long LLM turn is not fatal to the test — the
		// deterministic side-effect poll is the real assertion; surface it softly.
		t.Logf("A2A %s %s transport error (non-fatal, will poll side effect): %v", method, url, err)
		return 0, ""
	}
	defer resp.Body.Close()
	return resp.StatusCode, readBody(resp)
}

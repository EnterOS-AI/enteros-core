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
//	_Staging asserts kind/parent_id/config-tab auth and accepts only the schedules
//	proxy's exact pre-registration offline response). A plain workspace reaches
//	online WITHOUT any management MCP, and the concierge test never waits for
//	online, so BOTH stay green even when a fresh platform
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
//
// ---------------------------------------------------------------------------
// GATE 1.5 (k8s design doc) — how an operator discharges it with this test
// ---------------------------------------------------------------------------
//
// The doc's acceptance criterion is: "a fresh org provisions onto k8s AND its
// concierge answers a real provision_workspace call — NOT a PONG", discharged by
// running Guard B against the controlplane-test CP once its two Secrets exist and
// the Deployment is scaled to 1.
//
// That is a HAND-RUN path, and two of this test's defaults are wrong for it:
//
//	1. E2E_ASSERT_MGMT_MCP_CALLABLE defaults OFF (only staging-tenant-cd sets it),
//	   so the verdict would return the presence-only GREEN — a pass that never
//	   made the tool call the criterion is about.
//	2. requireStagingEnv SKIPs on unset STAGING_E2E / absent creds, and `go test`
//	   reports a skipped-only package as `ok` — so a typo reads as a Gate-1.5 pass.
//
// GUARD_B_REQUIRE_CALLABLE=1 closes both: it implies the callable turn and turns
// a missing precondition into a loud failure. The Gate-1.5 invocation is:
//
//	kubectl -n controlplane-test port-forward svc/controlplane 8080:8080 &
//	cd workspace-server && \
//	  STAGING_E2E=1 \
//	  GUARD_B_REQUIRE_CALLABLE=1 \
//	  CP_BASE_URL=http://127.0.0.1:8080 \
//	  CP_ADMIN_API_TOKEN=<controlplane-test admin token> \
//	  STAGING_TENANT_SUBDOMAIN_SUFFIX=<the cluster's tenant domain> \
//	  E2E_PROVIDER=molecules-server \
//	  go test -tags staging_e2e ./internal/staginge2e/ \
//	    -run TestPlatformAgentMgmtMCP_Staging -count=1 -v -timeout 50m
//
// The port-forward is not incidental: cp-on-k8s.yaml exposes only a ClusterIP
// Service (no IngressRoute), and validateStagingCPBase accepts loopback but
// REFUSES an in-cluster DNS name like controlplane.controlplane-test.svc — it
// will not send a CP admin bearer to an unvetted host. Loopback is the supported
// route for a hand-run Gate 1.5.

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
	// Guard B posture for THIS run, resolved by the same pure helper the unit
	// proof exercises. GUARD_B_REQUIRE_CALLABLE is the require-live flag an
	// operator sets when discharging the k8s design doc's Gate 1.5 ("a fresh org
	// provisions onto k8s AND its concierge answers a real provision_workspace
	// call — NOT a PONG"). It implies E2E_ASSERT_MGMT_MCP_CALLABLE.
	assertCallable, requireCallable := GuardBMode(
		os.Getenv("E2E_ASSERT_MGMT_MCP_CALLABLE"),
		os.Getenv("GUARD_B_REQUIRE_CALLABLE"),
	)

	// REQUIRE-LIVE ENV PRE-CHECK (mirrors CP serving-e2e SERVING_E2E_REQUIRE_LIVE).
	// requireStagingEnv SKIPs on an unset STAGING_E2E or absent creds — and `go
	// test` reports a package whose only test skipped as `ok`, so on the Gate-1.5
	// hand path a mistyped env var reads as a PASS at the gate level. When the
	// caller has declared require-live, a missing precondition is a
	// MISCONFIGURATION of the run, not an optional arm: fail loud and name it.
	// This runs BEFORE requireStagingEnv so the skip can never happen first.
	if requireCallable {
		var missing []string
		if strings.TrimSpace(os.Getenv("STAGING_E2E")) != "1" {
			missing = append(missing, "STAGING_E2E=1")
		}
		for _, k := range []string{"CP_BASE_URL", "CP_ADMIN_API_TOKEN"} {
			if strings.TrimSpace(os.Getenv(k)) == "" {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			t.Fatalf("GUARD_B_REQUIRE_CALLABLE is set but this run cannot reach a control plane: missing %s. "+
				"Under require-live a SKIP is a misconfiguration, not an optional arm — `go test` reports a "+
				"skipped-only package as `ok`, so allowing the skip here would report Gate 1.5 green while "+
				"asserting nothing.", strings.Join(missing, ", "))
		}
	}

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

	// (assertCallable / requireCallable were resolved at the top of the test by
	// GuardBMode, before requireStagingEnv, so the require-live pre-check runs
	// ahead of any skip.)

	// Wait for the concierge to reach status=online — which RCA #2970 makes
	// UNREACHABLE unless its management MCP is present — collecting the row-reported
	// signals (mcp_server_present, loaded_mcp_tools, runtime) into a probe.
	//
	// TERMINAL-SIGNAL WAIT (readiness_terminal_signal.go). This loop used to be
	// positive-edge-only: it polled for status=="online" for a fixed 15 minutes,
	// logged NOTHING in between, and then reported a timeout. Over the 278
	// e2e-smoke verdicts in retention that produced 26 reds — the single largest
	// Guard B failure cluster — and in 24 of them the last status was "failed":
	// a verdict the control plane had published minutes earlier, with a reason,
	// in the very body this loop was already parsing. The gate then purged the
	// org, destroying its own evidence. Four multi-hour staging incidents were
	// each investigated from logs that said only "never reached online WITHIN
	// 15m".
	//
	// Now every poll yields one of three answers, a status TRANSITION is logged
	// as it happens (the lifecycle wait always did this; that is why its reds
	// were diagnosable and these were not), and a published terminal verdict
	// ends the wait — after, and only after, it has outlived the control plane's
	// own self-heal window, so a concierge the CP is still remediating is never
	// called dead early.
	watch := NewConciergeOnlineWatch(conciergeOnlineBudget,
		ResolveTerminalSettle(os.Getenv(TerminalSettleEnv)))
	var probe MgmtMCPProbe
	probe.ExpectedRuntime = expectedRuntime
	probe.RequiredTool = requiredTool
	probe.AssertCallable = assertCallable
	probe.RequireCallable = requireCallable
	online := false
	var lastPresent, lastTools, waitFailure string
	for !online {
		hs, body := doTenantJSON(t, "GET", "https://"+host+"/workspaces/"+platformID, token, orgID, "")
		obs := Obs{ReadOK: hs == http.StatusOK}
		if hs == http.StatusOK {
			status := jsonField(body, "status")
			present, presentReported := jsonBool(body, "mcp_server_present")
			tools := loadedMCPTools(body)
			probe.Status = status
			probe.MCPServerPresent = present
			probe.MCPServerPresentReported = presentReported
			probe.LoadedTools = tools
			probe.ObservedRuntime = observedRuntime(body)
			lastPresent = fmt.Sprintf("%v(reported=%v)", present, presentReported)
			lastTools = strings.Join(tools, ",")
			// last_sample_error is the control plane's OWN reason for a failed
			// platform agent (markWorkspaceFailed writes it alongside the status
			// flip; core#5035 kept it un-stripped on this endpoint precisely so a
			// reader like this one can quote it).
			obs.Status = status
			obs.Detail = topLevelString(body, "last_sample_error")
		}
		step := watch.Observe(time.Now(), obs)
		if step.Transitioned && step.Message != "" {
			t.Logf("    %s mcp_server_present=%s loaded_mcp_tools=[%s]", step.Message, lastPresent, lastTools)
		}
		switch step.Decision {
		case WaitReady:
			online = true
		case WaitFailTerminal, WaitFailBudget, WaitFailMisconfigured:
			waitFailure = step.Message
		default:
			time.Sleep(15 * time.Second)
			continue
		}
		if waitFailure != "" {
			break
		}
	}
	if !online {
		t.Fatalf("platform agent %s never came online.\n"+
			"  %s\n"+
			"  mcp_server_present=%s loaded_mcp_tools=[%s]; required verb=%q.\n"+
			"  This is the test14 / broken-openclaw-pin failure mode: the concierge's mgmt-MCP plugin was "+
			"not installed (e.g. a presign:// declared source the deployed runtime cannot resolve) → "+
			"RCA #2970 fail-closed → the platform agent can never manage the org.\n"+
			"  CP tenant diagnostics: %s\n"+
			"  CP boot-events:        %s",
			platformID, waitFailure, lastPresent, lastTools, requiredTool,
			bestEffortAdminGet(cfg.cpBase, cfg.adminToken, "/cp/admin/tenants/"+slug+"/diagnostics"),
			bestEffortAdminGet(cfg.cpBase, cfg.adminToken, "/cp/admin/tenants/"+slug+"/boot-events?limit=20"))
	}

	// core#5026 — the concierge reached online, so its boot-install report MUST be
	// in the control plane by now. The runtime sends it before heartbeat.start()
	// and online is unreachable without a heartbeat, so an absent report is not a
	// race: it is a send that FAILED. On runtime 0.4.72 it failed on every boot,
	// because the report went out as boot step 1 — before /registry/register minted
	// the workspace bearer this route requires — and 401'd. Nothing noticed,
	// because no gate had ever asked whether a report came out the other end, which
	// is why the 0.4.72 promote was green. This is a FRESH org, so presence alone
	// is exact here (no earlier boot could have left a row behind); the FRESHNESS
	// half is asserted across the restart in TestWorkspaceLifecycle_Staging.
	//
	// Deliberately NOT behind an opt-in env flag. The fleet read
	// (/admin/plugin-install-reports) and the molecule_plugin_install_degraded_
	// workspaces gauge are both computed from this row, so a candidate that cannot
	// write it ships an observability surface that reports a fleet nobody measured
	// — exactly what a hard gate is for. A flag would make this a phantom gate.
	assertBootInstallReportLanded(t, host, token, orgID, platformID, "platform agent (concierge)")

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
		t.Fatalf("Guard B gate RED for fresh org %s (runtime=%q status=%q present=%s tools=[%s] callable_armed=%v require_live=%v): %s",
			slug, probe.ObservedRuntime, probe.Status, lastPresent, lastTools, assertCallable, requireCallable, reason)
	}
	t.Logf("Guard B gate GREEN for fresh org %s (runtime=%q expected=%q present=%s tools=[%s] callable_armed=%v require_live=%v): %s",
		slug, probe.ObservedRuntime, expectedRuntime, lastPresent, lastTools, assertCallable, requireCallable, reason)
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
		// core#5052: this used to truncate at 200 chars. The runtime's own
		// failure text is `Model generated invalid tool call: <name>` wrapped in
		// a JSON-RPC envelope, and the envelope alone is ~145 chars — so the one
		// field that names WHY the turn failed (the rejected tool id) was cut off
		// mid-word on every red run. Two separate investigations of this gate
		// stalled on `mcp__molecule_…` with the rest destroyed by this call.
		// The cap exists for log hygiene, not for redaction: make it wide enough
		// that the diagnostic survives.
		t.Logf("A2A provision_workspace turn → HTTP %d (worker=%s): %s", st, worker, truncate(resp, a2aTurnLogCap))
	}
	sendA2A()

	deadline := time.Now().Add(actBudget)
	nextNudge := time.Now().Add(75 * time.Second)
	for time.Now().Before(deadline) {
		if id, kind, status := findWorkspaceByName(t, host, token, orgID, worker); id != "" {
			// Best-effort targeted cleanup of the worker the concierge created.
			// Registered BEFORE the verdict so a row we then REFUSE (kind skew /
			// terminal-failed) is still torn down rather than leaked into the org.
			t.Cleanup(func() {
				_, _ = doTenantJSON(t, "DELETE",
					"https://"+host+"/workspaces/"+id+"?confirm=true", token, orgID, "")
			})
			// core#5052: the row verdict is PURE and unit-tested
			// (ClassifyProvisionedWorkspace / platform_agent_mgmt_mcp_gate_test.go).
			// It reads `status` as well as `kind` — the pre-fix gate matched on
			// name+kind only, so a workspace that was created and then FAILED
			// still reported "CALLABLE CONFIRMED".
			ok, reason := ClassifyProvisionedWorkspace(kind, status)
			if !ok {
				t.Logf("callable turn produced %q (id=%s kind=%q status=%q) but it is NOT callable proof: %s",
					worker, id, kind, status, reason)
				return false
			}
			t.Logf("CALLABLE CONFIRMED: concierge %s ran provision_workspace → workspace %q (id=%s kind=%q status=%q): %s",
				platformID, worker, id, kind, status, reason)
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

// isTruthy parses a permissive boolean env value (1/true/yes/on). Delegates to
// the untagged isTruthyValue so the tagged live path and the unit-proven pure
// path can never drift on what "on" means.
func isTruthy(v string) bool { return isTruthyValue(v) }

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

// findWorkspaceByName returns (id, kind, status) of the workspace whose
// name == want in GET /workspaces, or ("","","") if absent. The list rows omit
// name from the shared parseWorkspaceList row, so this does its own permissive
// decode.
//
// core#5052: `status` is returned as well as `kind`. It used to return only
// (id, kind), which made Guard B's hardest assertion VACUOUS — a workspace the
// concierge created that then FAILED to provision still satisfied "CALLABLE
// CONFIRMED", because nothing ever read its status. The verdict itself lives in
// the pure, unit-tested ClassifyProvisionedWorkspace.
func findWorkspaceByName(t *testing.T, host, token, orgID, want string) (id, kind, status string) {
	t.Helper()
	hs, body := doTenantJSON(t, "GET", "https://"+host+"/workspaces", token, orgID, "")
	if hs != http.StatusOK {
		return "", "", ""
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return "", "", ""
	}
	for _, m := range raw {
		if rawString(m["name"]) == want {
			return rawString(m["id"]), rawString(m["kind"]), rawString(m["status"])
		}
	}
	return "", "", ""
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

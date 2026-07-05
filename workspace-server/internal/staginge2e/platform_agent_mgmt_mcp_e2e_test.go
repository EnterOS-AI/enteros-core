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
//	The pre-existing staging gates provision a PLAIN default-runtime (claude-code)
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
	"strings"
	"testing"
	"time"

	molcontracts "go.moleculesai.app/molecule-contracts/gen/go"
)

func TestPlatformAgentMgmtMCP_Staging(t *testing.T) {
	cfg := requireStagingEnv(t)

	slug := fmt.Sprintf("e2e-mcp-%d", time.Now().Unix()%100000000)
	t.Logf("platform-agent-mgmt-mcp: slug=%s", slug)

	// --- Fresh org via the admin API (the same org-create path the UI drives) ---
	orgID := adminCreateOrg(t, cfg, slug)
	t.Cleanup(func() { adminDeleteTenant(t, cfg, slug) })
	t.Logf("org created: org_id=%s", orgID)

	token := tenantAdminToken(t, cfg, slug)
	host := slug + "." + cfg.subdomainSuffix
	waitForHTTP(t, host, http.StatusOK, 10*time.Minute, "tenant /health ready")
	t.Logf("tenant TLS ready: %s", host)

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
	// molecule-contracts — the SAME literal the server-side heartbeat gate matches
	// (handlers.conciergePlatformMCPProvisionWorkspaceTool). Never hardcoded here.
	requiredTool := "mcp__" + molcontracts.MCPServerName + "__" + molcontracts.RequiredTool

	// Wait for the concierge to reach status=online — which RCA #2970 makes
	// UNREACHABLE unless its management MCP is present — and, when the tenant
	// surfaces them, that mcp_server_present=true AND the provision_workspace verb
	// is in loaded_mcp_tools. This is the fresh-org mgmt-MCP present+callable
	// contract the plain-workspace lifecycle / DB-only concierge gates never
	// exercise.
	deadline := time.Now().Add(15 * time.Minute)
	var lastStatus, lastPresent, lastTools string
	for time.Now().Before(deadline) {
		hs, body := doTenantJSON(t, "GET", "https://"+host+"/workspaces/"+platformID, token, orgID, "")
		if hs == http.StatusOK {
			status := jsonField(body, "status")
			present, presentReported := jsonBool(body, "mcp_server_present")
			tools := loadedMCPTools(body)
			lastStatus = status
			lastPresent = fmt.Sprintf("%v(reported=%v)", present, presentReported)
			lastTools = strings.Join(tools, ",")

			if status == "online" {
				// online ⟹ mcp_server_present=true by the RCA #2970 gate. Tighten
				// with the row-reported signals when the tenant surfaces them: a
				// concierge that flipped online but omits the verb would be a
				// contract regression we want RED, not a silent pass.
				if presentReported && !present {
					t.Fatalf("platform agent %s is online but mcp_server_present=false — "+
						"RCA #2970 online contract violated: %s", platformID, truncate(body, 400))
				}
				if len(tools) > 0 && !containsStr(tools, requiredTool) {
					t.Fatalf("platform agent %s is online and reports loaded_mcp_tools=[%s] but NOT the "+
						"required lifecycle verb %q — mgmt MCP present but provision_workspace not callable",
						platformID, lastTools, requiredTool)
				}
				t.Logf("platform agent %s ONLINE with management MCP present (present=%s, tools=[%s]) — "+
					"required verb %q callable; gate green", platformID, lastPresent, lastTools, requiredTool)
				return
			}
		}
		time.Sleep(15 * time.Second)
	}
	t.Fatalf("platform agent %s never reached online WITH its management MCP callable within 15m "+
		"(last status=%q mcp_server_present=%s loaded_mcp_tools=[%s]; required verb=%q).\n"+
		"This is the test14 failure mode: the concierge's mgmt-MCP plugin was not installed (e.g. a "+
		"presign:// declared source the deployed runtime cannot resolve) → RCA #2970 fail-closed → the "+
		"platform agent can never manage the org.", platformID, lastStatus, lastPresent, lastTools, requiredTool)
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

// containsStr reports whether want is in xs (exact match).
func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

//go:build staging_e2e

package staginge2e

// workspace_requests_e2e_test.go — core#2606: a NORMAL (non-platform)
// workspace can raise a task AND an approval into the user's unified inbox
// (the Tasks/Approvals tabs), using its OWN workspace token against the
// wsAuth-gated POST /workspaces/:id/requests — the exact server path the
// runtime's create_request / create_approval bridge tools call.
//
// This complements the runtime unit tests (which pin the tool's payload +
// auth) with a live, against-real-staging proof that the request actually
// lands and surfaces in the org pending view. Reuses the lifecycle harness
// (requireStagingEnv / adminCreateOrg / tenantAdminToken /
// tenantCreateWorkspace / doTenantJSON / jsonField) — no new plumbing.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceCanRaiseTaskAndApprovalToUser(t *testing.T) {
	cfg := requireStagingEnv(t)
	slug := fmt.Sprintf("e2e-req-%d", time.Now().Unix()%100000000)
	t.Logf("workspace-requests: slug=%s", slug)

	orgID := adminCreateOrg(t, cfg, slug)
	t.Cleanup(func() { adminDeleteTenant(t, cfg, slug) })
	adminToken := tenantAdminToken(t, cfg, slug)
	tenantHost := slug + "." + cfg.subdomainSuffix
	waitForHTTP(t, tenantHost, http.StatusOK, 10*time.Minute, "tenant /health ready")

	// A normal workspace (kind defaults to "workspace").
	wsID := tenantCreateWorkspace(t, cfg, tenantHost, adminToken, orgID)
	t.Logf("workspace created: %s", wsID)

	// Mint the workspace's OWN bearer — the request tools authenticate with
	// the workspace token, not the org admin token, so the e2e must too.
	wsToken := mintWorkspaceToken(t, cfg, tenantHost, adminToken, orgID, wsID)

	// Raise a task and an approval to the user via the wsAuth endpoint.
	taskTitle := "e2e-2606 task " + slug
	apprTitle := "e2e-2606 approval " + slug
	raiseRequest(t, tenantHost, wsToken, orgID, wsID, "task", taskTitle)
	raiseRequest(t, tenantHost, wsToken, orgID, wsID, "approval", apprTitle)

	// Both must surface in the org's unified pending view (what the canvas
	// Tasks/Approvals tabs read). Poll briefly — creation broadcasts are
	// async but the row is committed synchronously, so this is fast.
	requirePending(t, tenantHost, adminToken, orgID, "task", taskTitle)
	requirePending(t, tenantHost, adminToken, orgID, "approval", apprTitle)
	t.Logf("both task + approval from a normal workspace surfaced in the org pending view ✓")
}

// TestExternalRuntimeWorkspaceCanRaiseTaskAndApproval is the EXTERNAL-runtime
// half of the core#2606 coverage: the request/approval surface is SSOT-shared,
// so a workspace on a NON-claude-code runtime (here codex, on the cheap
// platform model openai/gpt-5.4-mini) must be able to raise a task AND an
// approval into the user's unified inbox via the SAME wsAuth-gated
// POST /workspaces/:id/requests endpoint. This closes the gap left by
// TestWorkspaceCanRaiseTaskAndApprovalToUser (which only covered a normal
// claude-code workspace) and proves the endpoint is runtime-agnostic — exactly
// the "external + platform workspaces just inherit it SSOT-ly" design.
func TestExternalRuntimeWorkspaceCanRaiseTaskAndApproval(t *testing.T) {
	cfg := requireStagingEnv(t)
	slug := fmt.Sprintf("e2e-extreq-%d", time.Now().Unix()%100000000)
	t.Logf("external-runtime requests: slug=%s", slug)

	orgID := adminCreateOrg(t, cfg, slug)
	t.Cleanup(func() { adminDeleteTenant(t, cfg, slug) })
	adminToken := tenantAdminToken(t, cfg, slug)
	tenantHost := slug + "." + cfg.subdomainSuffix
	waitForHTTP(t, tenantHost, http.StatusOK, 10*time.Minute, "tenant /health ready")

	// An EXTERNAL runtime workspace (codex) on the cheapest platform model.
	wsID := tenantCreateWorkspaceWithRuntime(t, cfg, tenantHost, adminToken, orgID, "ext-codex", "codex", "openai/gpt-5.4-mini")
	t.Logf("external (codex) workspace created: %s", wsID)

	// The request is raised against the wsAuth endpoint with the workspace's own
	// token — no runtime boot required; the endpoint is what the SSOT a2a-bridge
	// create_request/create_approval tools call regardless of runtime.
	wsToken := mintWorkspaceToken(t, cfg, tenantHost, adminToken, orgID, wsID)

	taskTitle := "e2e-2606 ext task " + slug
	apprTitle := "e2e-2606 ext approval " + slug
	raiseRequest(t, tenantHost, wsToken, orgID, wsID, "task", taskTitle)
	raiseRequest(t, tenantHost, wsToken, orgID, wsID, "approval", apprTitle)

	requirePending(t, tenantHost, adminToken, orgID, "task", taskTitle)
	requirePending(t, tenantHost, adminToken, orgID, "approval", apprTitle)
	t.Logf("both task + approval from an EXTERNAL (codex) workspace surfaced in the org pending view ✓")
}

// mintWorkspaceToken mints a workspace-scoped bearer via the admin surface.
func mintWorkspaceToken(t *testing.T, cfg stagingCfg, host, adminToken, orgID, wsID string) string {
	t.Helper()
	url := "https://" + host + "/admin/workspaces/" + wsID + "/tokens"
	status, resp := doTenantJSON(t, "POST", url, adminToken, orgID, "{}")
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("mint workspace token: HTTP %d: %s", status, resp)
	}
	tok := jsonField(resp, "auth_token")
	if tok == "" {
		t.Fatalf("mint workspace token: no auth_token in response: %s", resp)
	}
	return tok
}

// raiseRequest POSTs a task/approval to recipient=user as the workspace.
func raiseRequest(t *testing.T, host, wsToken, orgID, wsID, kind, title string) {
	t.Helper()
	url := "https://" + host + "/workspaces/" + wsID + "/requests"
	body := fmt.Sprintf(
		`{"kind":%q,"recipient_type":"user","recipient_id":"","title":%q,"detail":"raised by a normal workspace (core#2606 e2e)"}`,
		kind, title,
	)
	status, resp := doTenantJSON(t, "POST", url, wsToken, orgID, body)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("raise %s request (workspace token, wsAuth): HTTP %d: %s", kind, status, resp)
	}
	if id := jsonField(resp, "request_id"); id == "" {
		t.Fatalf("raise %s request: no request_id: %s", kind, resp)
	}
}

// requirePending polls GET /requests/pending?kind= until `title` appears.
func requirePending(t *testing.T, host, adminToken, orgID, kind, title string) {
	t.Helper()
	url := "https://" + host + "/requests/pending?kind=" + kind
	deadline := time.Now().Add(60 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		status, resp := doTenantJSON(t, "GET", url, adminToken, orgID, "")
		last = resp
		if status == http.StatusOK {
			var rows []map[string]any
			if json.Unmarshal([]byte(resp), &rows) == nil {
				for _, r := range rows {
					if tt, _ := r["title"].(string); strings.Contains(tt, title) {
						return
					}
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("%s request %q never appeared in /requests/pending; last body: %s", kind, title, last)
}

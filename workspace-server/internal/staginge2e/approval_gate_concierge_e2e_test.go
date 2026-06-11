//go:build staging_e2e

package staginge2e

// approval_gate_concierge_e2e_test.go — live staging end-to-end coverage for
// core#2574: the Phase-4 approval gate on the admin-token path.
//
// What it proves that unit + integration tests cannot: that against a REAL
// SaaS tenant, the concierge's ADMIN_TOKEN (Tier 2b) is gated when minting
// org API tokens and writing workspace secrets — not just that the Go code
// returns 202, but that the REAL Postgres row is created and the retry-after-
// approval flow works through the actual HTTP surface.
//
// Run with:
//
//	STAGING_E2E=1 CP_BASE_URL=https://cp.staging.moleculesai.app \
//	  CP_ADMIN_API_TOKEN=... go test -tags=staging_e2e ./internal/staginge2e/ \
//	  -run TestConciergeApprovalGate_Staging -v
//
// Guarded by the staging_e2e build tag + STAGING_E2E=1 env gate.
// Teardown is t.Cleanup-driven (admin DELETE /cp/admin/tenants).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestConciergeApprovalGate_Staging(t *testing.T) {
	cfg := requireStagingEnv(t)

	slug := fmt.Sprintf("e2e-gate-%d", time.Now().Unix()%100000000)
	t.Logf("approval-gate: slug=%s", slug)

	// --- Step 1: provision throwaway org + tenant ---
	orgID := adminCreateOrg(t, cfg, slug)
	t.Cleanup(func() { adminDeleteTenant(t, cfg, slug) })
	t.Logf("org created: org_id=%s", orgID)

	token := tenantAdminToken(t, cfg, slug)
	host := slug + "." + cfg.subdomainSuffix
	waitForHTTP(t, host, http.StatusOK, 10*time.Minute, "tenant /health ready")
	t.Logf("tenant TLS ready: %s", host)

	// --- Step 2: create an ordinary workspace (secret-write needs a :id) ---
	ordinaryWS := tenantCreateWorkspace(t, cfg, host, token, orgID)
	t.Logf("ordinary workspace created: %s", ordinaryWS)

	// Install platform agent so there is a concierge workspace (kind=platform).
	platformID := findPlatformRoot(t, host, token, orgID)
	if platformID == "" {
		platformID = newUUIDv4(t)
		body := fmt.Sprintf(`{"id":%q,"name":%q}`, platformID, "E2E Gate Concierge")
		status, resp := doTenantJSON(t, "POST",
			"https://"+host+"/admin/org/platform-agent", token, orgID, body)
		if status != http.StatusOK {
			t.Fatalf("install platform agent: HTTP %d: %s", status, resp)
		}
		t.Logf("platform agent installed: %s", platformID)
	} else {
		t.Logf("platform agent already present: %s", platformID)
	}

	// ── Regression (c): concierge admin-token mint WITHOUT approval → 202 ─────
	t.Run("org_token_mint_without_approval_rejected", func(t *testing.T) {
		status, body := doTenantJSON(t, "POST",
			"https://"+host+"/org/tokens", token, orgID, `{"name":"exploit-test-token"}`)
		if status == http.StatusOK {
			var resp map[string]interface{}
			_ = json.Unmarshal([]byte(body), &resp)
			t.Fatalf("REGRESSION core#2574: admin-token mint returned 200 (token created) with ZERO approvals — %v", resp)
		}
		if status != http.StatusAccepted {
			t.Fatalf("admin-token mint: want 202, got %d: %s", status, body)
		}
		if !strings.Contains(body, `"status":"pending_approval"`) {
			t.Fatalf("expected pending_approval in 202 body, got: %s", body)
		}
		if !strings.Contains(body, `"action":"org_token_mint"`) {
			t.Fatalf("expected action=org_token_mint in 202 body, got: %s", body)
		}
		approvalID := jsonField(body, "approval_id")
		if approvalID == "" {
			t.Fatalf("approval_id missing from 202 body: %s", body)
		}
		t.Logf("mint correctly gated (202, approval_id=%s)", approvalID)

		// Verify NO token was actually minted.
		listStatus, listBody := doTenantJSON(t, "GET",
			"https://"+host+"/org/tokens", token, orgID, "")
		if listStatus != http.StatusOK {
			t.Fatalf("list tokens: HTTP %d: %s", listStatus, listBody)
		}
		if strings.Contains(listBody, "exploit-test-token") {
			t.Fatalf("REGRESSION: exploit-test-token appears in live token list despite zero approvals")
		}
		t.Logf("no token minted — list does not contain exploit-test-token")
	})

	// ── (a): concierge admin-token secret write WITHOUT approval → 202 ────────
	var secretApprovalID string
	t.Run("secret_write_without_approval_rejected", func(t *testing.T) {
		url := "https://" + host + "/workspaces/" + ordinaryWS + "/secrets"
		status, body := doTenantJSON(t, "POST", url, token, orgID,
			`{"key":"E2E_GATED_SECRET","value":"gated-value-123"}`)
		if status == http.StatusOK {
			var resp map[string]interface{}
			_ = json.Unmarshal([]byte(body), &resp)
			t.Fatalf("REGRESSION core#2574: admin-token secret write returned 200 with ZERO approvals — %v", resp)
		}
		if status != http.StatusAccepted {
			t.Fatalf("admin-token secret write: want 202, got %d: %s", status, body)
		}
		if !strings.Contains(body, `"status":"pending_approval"`) {
			t.Fatalf("expected pending_approval in 202 body, got: %s", body)
		}
		if !strings.Contains(body, `"action":"secret_write"`) {
			t.Fatalf("expected action=secret_write in 202 body, got: %s", body)
		}
		secretApprovalID = jsonField(body, "approval_id")
		if secretApprovalID == "" {
			t.Fatalf("approval_id missing from 202 body: %s", body)
		}
		t.Logf("secret write correctly gated (202, approval_id=%s)", secretApprovalID)

		// Verify the secret was NOT actually written.
		listStatus, listBody := doTenantJSON(t, "GET", url, token, orgID, "")
		if listStatus != http.StatusOK {
			t.Fatalf("list secrets: HTTP %d: %s", listStatus, listBody)
		}
		if strings.Contains(listBody, "E2E_GATED_SECRET") {
			t.Fatalf("REGRESSION: E2E_GATED_SECRET appears in list despite zero approvals")
		}
		t.Logf("secret not written — list does not contain E2E_GATED_SECRET")
	})

	// ── (b): WITH a granted approval, secret write succeeds ───────────────────
	// The decide endpoint is /workspaces/:id/approvals/:approvalId/decide.
	// For secret write, the workspace_id is the ordinary workspace ID.
	// For org token mint, workspace_id is empty (org-scoped), so there is no
	// HTTP decide path today — the integration tests cover the mint retry.
	t.Run("secret_write_with_approval_succeeds", func(t *testing.T) {
		if secretApprovalID == "" {
			t.Skip("secret_write_without_approval_rejected did not capture an approval_id")
		}

		// 1. Grant the approval via the decide endpoint.
		decideURL := fmt.Sprintf("https://%s/workspaces/%s/approvals/%s/decide",
			host, ordinaryWS, secretApprovalID)
		status, body := doTenantJSON(t, "POST", decideURL, token, orgID, `{"decision":"approved"}`)
		if status != http.StatusOK {
			t.Fatalf("approve decision: HTTP %d: %s", status, body)
		}
		if !strings.Contains(body, `"status":"approved"`) {
			t.Fatalf("expected approved status in decide body, got: %s", body)
		}
		t.Logf("approval %s granted", secretApprovalID)

		// 2. Retry the secret write with the SAME key.
		writeURL := "https://" + host + "/workspaces/" + ordinaryWS + "/secrets"
		status, body = doTenantJSON(t, "POST", writeURL, token, orgID,
			`{"key":"E2E_GATED_SECRET","value":"gated-value-123"}`)
		if status != http.StatusOK {
			t.Fatalf("retry after approval: want 200, got %d: %s", status, body)
		}
		if !strings.Contains(body, `"status":"saved"`) {
			t.Fatalf("expected saved status in 200 body, got: %s", body)
		}
		t.Logf("secret write succeeded after approval (200)")

		// 3. Verify the secret IS now present.
		listStatus, listBody := doTenantJSON(t, "GET", writeURL, token, orgID, "")
		if listStatus != http.StatusOK {
			t.Fatalf("list secrets after write: HTTP %d: %s", listStatus, listBody)
		}
		if !strings.Contains(listBody, "E2E_GATED_SECRET") {
			t.Fatalf("E2E_GATED_SECRET missing from list after approved write: %s", listBody)
		}
		t.Logf("secret present in list after approved write")

		// 4. Single-use: a SECOND write with the SAME key must create a NEW
		//    pending approval (the first one was consumed).
		status, body = doTenantJSON(t, "POST", writeURL, token, orgID,
			`{"key":"E2E_GATED_SECRET","value":"gated-value-456"}`)
		if status != http.StatusAccepted {
			t.Fatalf("second write after consumption: want 202 (fresh pending), got %d: %s", status, body)
		}
		if !strings.Contains(body, `"status":"pending_approval"`) {
			t.Fatalf("expected fresh pending_approval for second write, got: %s", body)
		}
		freshApprovalID := jsonField(body, "approval_id")
		if freshApprovalID == secretApprovalID {
			t.Fatalf("approval_id reused — the consumed approval was not single-use: %s", freshApprovalID)
		}
		t.Logf("single-use verified: second write got fresh approval_id=%s", freshApprovalID)
	})

	// ── List pending approvals via admin surface ──────────────────────────────
	// The /approvals/pending endpoint is AdminAuth-gated and returns ALL pending
	// approvals across the tenant. After the mint rejection above, there should be
	// at least one pending org_token_mint approval (workspace_id may be "").
	t.Run("pending_approvals_listable_by_admin", func(t *testing.T) {
		status, body := doTenantJSON(t, "GET",
			"https://"+host+"/approvals/pending", token, orgID, "")
		if status != http.StatusOK {
			t.Fatalf("list pending approvals: HTTP %d: %s", status, body)
		}
		if !strings.Contains(body, `"action":"org_token_mint"`) {
			t.Fatalf("expected at least one org_token_mint pending approval in list: %s", body)
		}
		t.Logf("pending approvals list contains org_token_mint (admin can see the gate-fired request)")
	})
}

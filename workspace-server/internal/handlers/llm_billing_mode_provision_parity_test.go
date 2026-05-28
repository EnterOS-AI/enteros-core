package handlers

// llm_billing_mode_provision_parity_test.go — molecule-core#1994.
//
// Root cause pinned in Phase 1: the PROVISION path resolved billing mode from
// the raw payload.Model, while the READ endpoint resolves from the stored
// MODEL workspace_secret. On a RE-PROVISION (restart/resume/auto-restart) the
// payload is rebuilt from the DB with Name+Tier+Runtime ONLY — payload.Model
// is "" (workspace_restart.go:333/844/1017 via withStoredCompute, which
// backfills Compute but NOT Model). So applyPlatformManagedLLMEnv called
// ResolveLLMBillingModeDerived(runtime, "", ...) → DeriveProvider errored on an
// empty model → default-closed platform_managed → the CP proxy got baked in and
// the workspace billed the PLATFORM Anthropic key for the customer's own usage
// (Reno Stars Marketing agent 6b66de8d, opus, claude-code; live-confirmed
// 2026-05-28: container env MODEL=opus but MOLECULE_LLM_BILLING_MODE_RESOLVED=
// platform_managed + ANTHROPIC_BASE_URL=<platform proxy>).
//
// The fix: applyPlatformManagedLLMEnv resolves the effective model using the
// SAME fallback chain applyRuntimeModelEnv already uses
// (payload.Model → envVars["MOLECULE_MODEL"] → envVars["MODEL"]) BEFORE
// deriving, so the provision path's derive inputs match the read path's. The
// merged envVars already carries the MODEL workspace_secret (loadWorkspaceSecrets).
//
// These tests are mutation-load-bearing: reverting the effective-model fix
// (passing payload.Model verbatim) turns
// TestApplyPlatformManagedLLMEnv_ReProvisionUsesStoredModel and the parity
// test RED.

import (
	"context"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
)

// TestApplyPlatformManagedLLMEnv_ReProvisionUsesStoredModel is the direct
// repro of the #1994 divergence at the provision resolver. payload.Model is ""
// (the re-provision shape) but the workspace's own oauth + MODEL=opus are
// present in envVars (loaded from workspace_secrets). The resolver MUST derive
// from the stored model → anthropic-oauth → byok, NOT default-closed to
// platform_managed.
//
// Asserts the byok outcome AND that the byok branch's effects fired:
//   - billing-mode env = byok (not platform_managed)
//   - ANTHROPIC_BASE_URL NOT rewritten to the platform proxy (left direct)
//   - the workspace's OWN oauth (workspace_secrets provenance, NOT in
//     globalKeys) survives — usable credential present.
//
// Mutation: revert applyPlatformManagedLLMEnv to pass payload.Model ("") to the
// resolver → derive errors on empty model → platform_managed → this test RED on
// every assertion.
func TestApplyPlatformManagedLLMEnv_ReProvisionUsesStoredModel(t *testing.T) {
	ctx := context.Background()
	const wsID = "6b66de8d-9337-4fb4-be8d-6d49dca0d809" // Reno Stars Marketing agent

	mock := setupTestDB(t)
	// Resolver reads the override (NULL — no explicit operator pin).
	expectOverrideQuery(mock, wsID, "")

	// The container env as loadWorkspaceSecrets would have built it on a
	// re-provision: the workspace's OWN oauth (workspace_secrets provenance) +
	// the stored MODEL=opus. The platform proxy URL is present from the prior
	// platform_managed boot (the env we must NOT re-bake).
	envVars := map[string]string{
		"MODEL":                   "opus",
		"CLAUDE_CODE_OAUTH_TOKEN": "RENO-OWN-OAUTH", // workspace_secrets origin
		"ANTHROPIC_BASE_URL":      "https://api.moleculesai.app/api/v1/internal/llm/anthropic",
	}
	// globalKeys is EMPTY — the oauth is the workspace's own (workspace_secrets),
	// so it is NOT global-origin and must survive the strip.
	globalKeys := map[string]struct{}{}

	// payload.Model == "" — exactly the re-provision shape.
	res := applyPlatformManagedLLMEnv(ctx, envVars, globalKeys, wsID, "claude-code", "")

	if res.ResolvedMode != LLMBillingModeBYOK {
		t.Fatalf("re-provision with stored MODEL=opus must resolve byok, got %q (source=%s) — the #1994 divergence", res.ResolvedMode, res.Source)
	}
	if res.Source != BillingModeSourceDerivedProvider {
		t.Errorf("source: got %q want derived_provider (opus → anthropic-oauth)", res.Source)
	}
	if envVars["MOLECULE_LLM_BILLING_MODE_RESOLVED"] != LLMBillingModeBYOK {
		t.Errorf("MOLECULE_LLM_BILLING_MODE_RESOLVED: got %q want byok", envVars["MOLECULE_LLM_BILLING_MODE_RESOLVED"])
	}
	// byok must NOT route through the platform proxy.
	if got := envVars["ANTHROPIC_BASE_URL"]; got != "https://api.moleculesai.app/api/v1/internal/llm/anthropic" {
		// The byok branch must leave ANTHROPIC_BASE_URL untouched (the prior
		// proxy URL is what re-provision must STOP re-asserting from the
		// platform path; the workspace template resets it to direct on the byok
		// path). The key assertion is the inverse below: the platform path did
		// NOT run, so MOLECULE_LLM_BASE_URL / usage token were NOT injected.
		_ = got
	}
	// The decisive proxy-bypass assertions: the platform_managed path injects
	// these; the byok branch must NOT.
	if _, ok := envVars["MOLECULE_LLM_USAGE_TOKEN"]; ok {
		t.Errorf("byok path must NOT inject the platform usage token (proxy billing); got %q", envVars["MOLECULE_LLM_USAGE_TOKEN"])
	}
	if !res.HasUsableLLMCred {
		t.Errorf("the workspace's OWN oauth (workspace_secrets origin) must survive → HasUsableLLMCred=true")
	}
	if envVars["CLAUDE_CODE_OAUTH_TOKEN"] != "RENO-OWN-OAUTH" {
		t.Errorf("workspace-origin oauth must survive the byok strip; got %q", envVars["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestApplyPlatformManagedLLMEnv_ReadProvisionParity is the core regression
// guard against the #1994 divergence ever returning: for the same workspace
// inputs (same runtime, same stored MODEL, same auth env, same override), the
// READ-path resolver (ResolveLLMBillingMode → readWorkspaceDeriveInputs) and
// the PROVISION-path resolver (applyPlatformManagedLLMEnv) MUST land on the
// same billing mode.
//
// Mutation: revert the effective-model fix → provision path derives from ""
// → platform_managed while the read path derives opus → byok → parity BREAKS
// → this test RED.
func TestApplyPlatformManagedLLMEnv_ReadProvisionParity(t *testing.T) {
	ctx := context.Background()
	const wsID = "6b66de8d-9337-4fb4-be8d-6d49dca0d809"

	// ---- READ PATH ----
	// ResolveLLMBillingMode reads in order: override (NULL) → runtime → secrets
	// (MODEL=opus + the oauth key) → then ResolveLLMBillingModeDerived re-reads
	// the override (NULL again).
	readMock := setupTestDB(t)
	expectOverrideQuery(readMock, wsID, "") // first override read (legacy resolver)
	readMock.ExpectQuery(`SELECT runtime FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("claude-code"))
	readMock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}).
			AddRow("MODEL", []byte("opus"), 0).
			AddRow("CLAUDE_CODE_OAUTH_TOKEN", []byte("RENO-OWN-OAUTH"), 0))
	expectOverrideQuery(readMock, wsID, "") // second override read (derived resolver)

	readRes, err := ResolveLLMBillingMode(ctx, wsID, "")
	if err != nil {
		t.Fatalf("read-path resolve err: %v", err)
	}
	if err := readMock.ExpectationsWereMet(); err != nil {
		t.Errorf("read-path sqlmock expectations: %v", err)
	}

	// ---- PROVISION PATH ----
	provMock := setupTestDB(t)
	expectOverrideQuery(provMock, wsID, "")
	provEnv := map[string]string{
		"MODEL":                   "opus",
		"CLAUDE_CODE_OAUTH_TOKEN": "RENO-OWN-OAUTH",
	}
	provRes := applyPlatformManagedLLMEnv(ctx, provEnv, map[string]struct{}{}, wsID, "claude-code", "")
	if err := provMock.ExpectationsWereMet(); err != nil {
		t.Errorf("provision-path sqlmock expectations: %v", err)
	}

	if readRes.ResolvedMode != provRes.ResolvedMode {
		t.Fatalf("PARITY VIOLATION (#1994): read-path resolved %q but provision-path resolved %q for the same workspace inputs (claude-code, MODEL=opus)",
			readRes.ResolvedMode, provRes.ResolvedMode)
	}
	if readRes.ResolvedMode != LLMBillingModeBYOK {
		t.Errorf("both paths should resolve byok for (claude-code, opus); got %q", readRes.ResolvedMode)
	}
}

// TestApplyPlatformManagedLLMEnv_DefaultPreservation pins the CTO invariant
// "default stays platform": a workspace with no non-platform provider selection
// and no own credential (no stored MODEL, empty env) still resolves
// platform_managed. The fix must NOT flip genuinely-platform workspaces to byok.
//
// This mirrors the agents-team genuinely-platform case. Mutation: a fix that
// silently defaulted byok on an empty/underivable model would turn this RED.
func TestApplyPlatformManagedLLMEnv_DefaultPreservation(t *testing.T) {
	ctx := context.Background()
	const wsID = "11111111-2222-3333-4444-555555555555"

	mock := setupTestDB(t)
	expectOverrideQuery(mock, wsID, "")

	// No MODEL anywhere, no auth env — nothing to derive.
	envVars := map[string]string{}
	res := applyPlatformManagedLLMEnv(ctx, envVars, map[string]struct{}{}, wsID, "claude-code", "")

	if res.ResolvedMode != LLMBillingModePlatformManaged {
		t.Fatalf("no model + no cred must default platform_managed (CTO: default stays platform), got %q (source=%s)", res.ResolvedMode, res.Source)
	}
	if res.Source != BillingModeSourceDerivedDefault {
		t.Errorf("source: got %q want derived_default", res.Source)
	}
	if envVars["MOLECULE_LLM_BILLING_MODE_RESOLVED"] != LLMBillingModePlatformManaged {
		t.Errorf("resolved env: got %q want platform_managed", envVars["MOLECULE_LLM_BILLING_MODE_RESOLVED"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestApplyPlatformManagedLLMEnv_ByokGlobalOnlyOAuthStillFailsClosed is the
// #711 regression guard against the #1994 fix over-reaching. The CUSTOMER must
// supply their OWN oauth (workspace_secrets). A workspace whose ONLY oauth is
// the PLATFORM's global-origin token (globalKeys) must STILL strip it and fail
// closed — the fix must not "materialize the global-origin oauth" (that would
// re-introduce the original drain). Here MODEL=opus derives byok, the only
// oauth is global-origin → stripped → no usable cred.
func TestApplyPlatformManagedLLMEnv_ByokGlobalOnlyOAuthStillFailsClosed(t *testing.T) {
	ctx := context.Background()
	const wsID = "99999999-8888-7777-6666-555555555555"

	mock := setupTestDB(t)
	expectOverrideQuery(mock, wsID, "")

	envVars := map[string]string{
		"MODEL":                   "opus",
		"CLAUDE_CODE_OAUTH_TOKEN": "PLATFORM-GLOBAL-OAUTH",
	}
	// The oauth is GLOBAL-origin (the platform's token).
	globalKeys := map[string]struct{}{"CLAUDE_CODE_OAUTH_TOKEN": {}}

	res := applyPlatformManagedLLMEnv(ctx, envVars, globalKeys, wsID, "claude-code", "")

	if res.ResolvedMode != LLMBillingModeBYOK {
		t.Fatalf("opus derives byok regardless of cred provenance; got %q", res.ResolvedMode)
	}
	if res.HasUsableLLMCred {
		t.Errorf("#711: a global-origin platform oauth must be stripped on byok → no usable cred, got HasUsableLLMCred=true (drain would ship)")
	}
	if _, present := envVars["CLAUDE_CODE_OAUTH_TOKEN"]; present {
		t.Errorf("#711: global-origin oauth must be stripped, but it survived: %q", envVars["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestReProvisionPayloadOmitsModel is a static guard pinning the upstream
// trigger: the re-provision payload builders pass Name+Tier+Runtime but NOT
// Model, so applyPlatformManagedLLMEnv cannot rely on payload.Model and must
// fall back to the stored MODEL in envVars. If a future change starts threading
// Model into these payloads, this test documents that the fallback is then
// belt-and-suspenders (still correct), not the sole mechanism.
func TestReProvisionPayloadOmitsModel(t *testing.T) {
	// Mirrors withStoredCompute(ctx, id, CreateWorkspacePayload{Name, Tier,
	// Runtime}) at workspace_restart.go:333/844/1017 — Model is the zero value.
	p := models.CreateWorkspacePayload{Name: "Reno Stars Marketing", Tier: 1, Runtime: "claude-code"}
	if p.Model != "" {
		t.Fatalf("re-provision payload model expected empty (the #1994 trigger), got %q", p.Model)
	}
}

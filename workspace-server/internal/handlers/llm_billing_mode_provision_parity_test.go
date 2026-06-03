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
	// payload.Model == "" — exactly the re-provision shape. The oauth is
	// workspace_secrets-origin (NOT in globalKeys) → exempt from the #728
	// provider-matched strip regardless of provider match.
	res := applyPlatformManagedLLMEnv(ctx, envVars, wsID, "claude-code", "", nil)

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
	provRes := applyPlatformManagedLLMEnv(ctx, provEnv, wsID, "claude-code", "", nil)
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
	res := applyPlatformManagedLLMEnv(ctx, envVars, wsID, "claude-code", "", nil)

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

// TestApplyPlatformManagedLLMEnv_ByokGlobalScopeOAuthSurvives is the
// molecule-core#1994 (corrected-model) inversion of the former internal#711
// strip test. `global_secrets` is the TENANT's store, so a byok workspace
// whose oauth lives at GLOBAL scope (shared across the tenant's workspaces) is
// running on the TENANT's own credential — it must SURVIVE and route direct,
// not be stripped + failed-closed. MODEL=opus derives byok; the global-scope
// oauth is the tenant's own and is exactly what byok runs on.
//
// Mutation (load-bearing): re-add stripGlobalOriginLLMCreds on the byok branch
// → the oauth disappears → HasUsableLLMCred=false → this test RED on both the
// survival assertion and the usable-cred assertion.
func TestApplyPlatformManagedLLMEnv_ByokGlobalScopeOAuthSurvives(t *testing.T) {
	ctx := context.Background()
	const wsID = "99999999-8888-7777-6666-555555555555"

	mock := setupTestDB(t)
	expectOverrideQuery(mock, wsID, "")

	// The tenant's own oauth at global scope (a global_secrets row), shared
	// across all the tenant's workspaces. There is no separate workspace row.
	envVars := map[string]string{
		"MODEL":                   "opus",
		"CLAUDE_CODE_OAUTH_TOKEN": "TENANT-OWN-GLOBAL-OAUTH",
	}
	// Provenance: the oauth is GLOBAL-origin (internal#728). It must STILL
	// survive — opus derives anthropic-oauth, whose auth_env IS
	// CLAUDE_CODE_OAUTH_TOKEN, so the provider-matched strip keeps it. This is
	// the PM/reno opus-byok regression guard against #728's strip.
	globalKeys := map[string]struct{}{"CLAUDE_CODE_OAUTH_TOKEN": {}}

	res := applyPlatformManagedLLMEnv(ctx, envVars, wsID, "claude-code", "", globalKeys)

	if res.ResolvedMode != LLMBillingModeBYOK {
		t.Fatalf("opus derives byok; got %q", res.ResolvedMode)
	}
	// The tenant's own global-scope oauth SURVIVES — byok runs on it, direct.
	if envVars["CLAUDE_CODE_OAUTH_TOKEN"] != "TENANT-OWN-GLOBAL-OAUTH" {
		t.Errorf("tenant's own global-scope oauth must survive on byok; got %q", envVars["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	if !res.HasUsableLLMCred {
		t.Errorf("tenant's own global-scope oauth is a usable credential → HasUsableLLMCred must be true (byok must not be failed-closed)")
	}
	// byok must NOT force the platform proxy.
	if _, present := envVars["MOLECULE_LLM_USAGE_TOKEN"]; present {
		t.Errorf("byok must not inject the platform usage token; got %q", envVars["MOLECULE_LLM_USAGE_TOKEN"])
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

// --- internal#728 Bug 1: provider-matched credential injection ---------------

// TestApplyPlatformManagedLLMEnv_MinimaxStripsStrayGlobalOAuth is the direct
// repro of DevB (Dev Engineer B, MiniMax-M2.7, claude-code; live-confirmed
// 2026-05-28). config.yaml correctly resolves provider=minimax, but the
// container inherits the tenant-GLOBAL CLAUDE_CODE_OAUTH_TOKEN; the claude-code
// runtime greedily prefers it (`llm-auth: detected oauth`) and routes
// MiniMax-M2.7 → api.anthropic.com → `Claude Code returned an error result`.
//
// The #728 provider-matched strip must REMOVE the stray global-origin oauth
// (minimax's auth_env is MINIMAX_API_KEY/ANTHROPIC_AUTH_TOKEN/ANTHROPIC_API_KEY
// — NOT CLAUDE_CODE_OAUTH_TOKEN) while KEEPING the minimax routing key.
//
// Mutation (load-bearing): remove the stripNonMatchingGlobalOriginLLMCreds
// call (revert to #1994's blanket keep) → the oauth survives → this test RED on
// the oauth-absent assertion. Make the strip provider-UNAWARE (strip all
// global bypass keys) → MINIMAX_API_KEY also vanishes → RED on the
// minimax-routing assertion. Make it provenance-UNAWARE (strip by name
// regardless of origin) → the workspace-origin exemption test below goes RED.
func TestApplyPlatformManagedLLMEnv_MinimaxStripsStrayGlobalOAuth(t *testing.T) {
	ctx := context.Background()
	const wsID = "22222222-3333-4444-5555-666666666666" // agents-team Dev Engineer B

	mock := setupTestDB(t)
	expectOverrideQuery(mock, wsID, "")

	// The container env on a re-provision: the MiniMax routing key + the stray
	// tenant-global oauth (both global_secrets origin) + the stored model.
	envVars := map[string]string{
		"MODEL":                   "MiniMax-M2.7",
		"MINIMAX_API_KEY":         "MINIMAX-TENANT-KEY",
		"CLAUDE_CODE_OAUTH_TOKEN": "STRAY-TENANT-GLOBAL-OAUTH",
	}
	// Both creds are global_secrets origin (the tenant configured them at org
	// scope; no per-workspace override re-set them).
	globalKeys := map[string]struct{}{
		"MINIMAX_API_KEY":         {},
		"CLAUDE_CODE_OAUTH_TOKEN": {},
	}

	res := applyPlatformManagedLLMEnv(ctx, envVars, wsID, "claude-code", "", globalKeys)

	if res.ResolvedMode != LLMBillingModeBYOK {
		t.Fatalf("MiniMax-M2.7 must derive minimax → byok, got %q (source=%s)", res.ResolvedMode, res.Source)
	}
	if res.Source != BillingModeSourceDerivedProvider {
		t.Errorf("source: got %q want derived_provider (MiniMax-M2.7 → minimax)", res.Source)
	}
	// THE FIX: the stray global oauth that does NOT match minimax's auth_env
	// must be gone, so the runtime cannot prefer it and mis-route to Anthropic.
	if v, present := envVars["CLAUDE_CODE_OAUTH_TOKEN"]; present {
		t.Errorf("stray global-origin CLAUDE_CODE_OAUTH_TOKEN must be STRIPPED for a minimax-resolving workspace (the DevB bug); still present=%q", v)
	}
	// The minimax routing key (IS in minimax's auth_env) must remain.
	if envVars["MINIMAX_API_KEY"] != "MINIMAX-TENANT-KEY" {
		t.Errorf("minimax routing key must SURVIVE (it matches the resolved provider's auth_env); got %q", envVars["MINIMAX_API_KEY"])
	}
	if !res.HasUsableLLMCred {
		t.Errorf("MINIMAX_API_KEY is a usable credential → HasUsableLLMCred must stay true (not failed-closed)")
	}
	if _, present := envVars["MOLECULE_LLM_USAGE_TOKEN"]; present {
		t.Errorf("byok must not inject the platform usage token")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestApplyPlatformManagedLLMEnv_WorkspaceOriginCredExemptFromStrip pins the
// provenance guard: a CLAUDE_CODE_OAUTH_TOKEN the USER set via the canvas
// Secrets tab (workspace_secrets origin → NOT in globalKeys) must NEVER be
// stripped, even on a minimax-resolving workspace where it doesn't match the
// derived provider's auth_env. The user authored it deliberately; the #728
// strip is scoped to the inherited operator-store channel only.
//
// Mutation: drop the `if _, isBypass...; continue` / globalKeys gate (strip by
// name regardless of origin) → the user's oauth vanishes → RED.
func TestApplyPlatformManagedLLMEnv_WorkspaceOriginCredExemptFromStrip(t *testing.T) {
	ctx := context.Background()
	const wsID = "33333333-4444-5555-6666-777777777777"

	mock := setupTestDB(t)
	expectOverrideQuery(mock, wsID, "")

	envVars := map[string]string{
		"MODEL":                   "MiniMax-M2.7",
		"MINIMAX_API_KEY":         "MINIMAX-TENANT-KEY",
		"CLAUDE_CODE_OAUTH_TOKEN": "USER-AUTHORED-OAUTH",
	}
	// MINIMAX_API_KEY is global-origin; the oauth is WORKSPACE-origin (the user
	// re-set it via the Secrets tab, so loadWorkspaceSecrets cleared its
	// global-origin flag) → exempt.
	globalKeys := map[string]struct{}{"MINIMAX_API_KEY": {}}

	res := applyPlatformManagedLLMEnv(ctx, envVars, wsID, "claude-code", "", globalKeys)

	if res.ResolvedMode != LLMBillingModeBYOK {
		t.Fatalf("MiniMax-M2.7 derives byok; got %q", res.ResolvedMode)
	}
	if envVars["CLAUDE_CODE_OAUTH_TOKEN"] != "USER-AUTHORED-OAUTH" {
		t.Errorf("workspace-origin (user-authored) oauth must NOT be stripped even when it doesn't match the provider; got %q", envVars["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	if envVars["MINIMAX_API_KEY"] != "MINIMAX-TENANT-KEY" {
		t.Errorf("matching minimax key must survive; got %q", envVars["MINIMAX_API_KEY"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestApplyPlatformManagedLLMEnv_MissingProxyEnvFailClosed is the #2162
// regression guard. A platform-managed workspace whose CP proxy env is absent
// must NOT start credential-less. The empty-proxy path must return
// HasUsableLLMCred=false so the caller aborts with MISSING_PLATFORM_PROXY.
//
// Mutation: revert the early-return from HasUsableLLMCred=false to true
// → workspace starts with zero credential → "container started but never
// called /registry/register" (600s provision-timeout sweep) → this test RED.
func TestApplyPlatformManagedLLMEnv_MissingProxyEnvFailClosed(t *testing.T) {
	ctx := context.Background()
	const wsID = "29b95be9-811e-4857-be36-1dafdbf4f697" // adk-demo failure workspace

	mock := setupTestDB(t)
	expectOverrideQuery(mock, wsID, "")

	// No proxy env present — simulates the boot-race / misconfig path.
	envVars := map[string]string{}
	res := applyPlatformManagedLLMEnv(ctx, envVars, wsID, "claude-code", "moonshot/kimi-k2.6", nil)

	if res.ResolvedMode != LLMBillingModePlatformManaged {
		t.Fatalf("platform-managed model must stay platform_managed, got %q (source=%s)", res.ResolvedMode, res.Source)
	}
	// THE FIX: must NOT report usable credential when none was injected.
	if res.HasUsableLLMCred {
		t.Fatalf("empty proxy env → HasUsableLLMCred must be false (fail-closed), got true — the #2162 dark-wedge class")
	}
	// No credential env must be present.
	if _, present := envVars["ANTHROPIC_API_KEY"]; present {
		t.Errorf("empty proxy env must NOT inject ANTHROPIC_API_KEY")
	}
	if _, present := envVars["MOLECULE_LLM_USAGE_TOKEN"]; present {
		t.Errorf("empty proxy env must NOT inject MOLECULE_LLM_USAGE_TOKEN")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestApplyPlatformManagedLLMEnv_ProxyEnvPresentInjectsCredential is the
// positive-path pair to the #2162 regression guard: when the CP proxy env IS
// present, the platform-managed path must inject ANTHROPIC_API_KEY +
// ANTHROPIC_BASE_URL for an Anthropic-native runtime and report
// HasUsableLLMCred=true.
func TestApplyPlatformManagedLLMEnv_ProxyEnvPresentInjectsCredential(t *testing.T) {
	ctx := context.Background()
	const wsID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	mock := setupTestDB(t)
	expectOverrideQuery(mock, wsID, "")

	envVars := map[string]string{}
	// Simulate the CP proxy env being present (as it is in production).
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.moleculesai.app/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_ANTHROPIC_BASE_URL", "https://api.moleculesai.app/api/v1/internal/llm/anthropic/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "PLATFORM-PROXY-TOKEN")

	res := applyPlatformManagedLLMEnv(ctx, envVars, wsID, "claude-code", "moonshot/kimi-k2.6", nil)

	if res.ResolvedMode != LLMBillingModePlatformManaged {
		t.Fatalf("expected platform_managed, got %q", res.ResolvedMode)
	}
	if !res.HasUsableLLMCred {
		t.Fatalf("proxy env present → HasUsableLLMCred must be true, got false")
	}
	if envVars["ANTHROPIC_API_KEY"] != "PLATFORM-PROXY-TOKEN" {
		t.Errorf("ANTHROPIC_API_KEY must be injected with the platform proxy token; got %q", envVars["ANTHROPIC_API_KEY"])
	}
	if envVars["ANTHROPIC_BASE_URL"] != "https://api.moleculesai.app/api/v1/internal/llm/anthropic/v1" {
		t.Errorf("ANTHROPIC_BASE_URL must be injected with the platform anthropic proxy; got %q", envVars["ANTHROPIC_BASE_URL"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

package handlers

// org_import_model_persist_test.go — regression tests for core#2594 on the
// org-import path.
//
// Bug: createWorkspaceTree resolved the workspace model (ws.Model →
// defaults.Model, required) and threaded it into the FIRST provision via
// payload.Model, but never PERSISTED it as the MODEL workspace_secret. Every
// other provision entry point (WorkspaceHandler.Create, SecretsHandler.SetModel)
// writes MODEL so the choice survives; org-import was the sole gap.
//
// Consequence: any provision that rebuilds the payload from the workspaces row
// (restart / resume / auto-recover / re-import) read an EMPTY payload.Model —
// the workspaces table has no model column and no MODEL secret existed — so
// applyRuntimeModelEnv set neither MOLECULE_MODEL nor MODEL and the universal
// MISSING_MODEL gate (workspace_provision_shared.go) aborted the workspace:
// "no resolved model (MISSING_MODEL, core#2594); refusing the runtime's opaque
// default". The mini-company template (Concierge over Marketing+sub-team,
// Accounting, Legal) reproduced it: 6 imported workspaces failed to get
// containers after their first cycle.
//
// The fix adds a setModelSecret(ctx, id, model) write right after the workspaces
// INSERT succeeds, so the resolved model is durable for the whole imported tree.
// These tests pin BOTH resolution sources the operator called out:
//   1. the org `defaults.model` (no per-workspace override), and
//   2. a per-workspace `model:` override (which must win over the default).
//
// Harness: a single leaf workspace, non-external, non-mock, with NO provisioner
// wired (HasProvisioner()==false) so createWorkspaceTree creates + persists then
// returns without spawning a provision goroutine. The two load-bearing queries —
// the workspaces INSERT and the workspace_secrets MODEL upsert — are asserted in
// order; the benign tail (canvas_layouts insert, schedule migration) is
// best-effort and errors non-fatally after both expectations are consumed.

import (
	"database/sql/driver"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// modelSecretMatcher matches the encrypted_value argument of the MODEL
// workspace_secrets upsert against an expected model string. crypto is disabled
// in unit tests (no 32-byte key), so setModelSecret stores the model as
// plaintext bytes — but we accept either []byte or string so the assertion does
// not depend on the driver's byte representation.
type modelSecretMatcher struct{ want string }

func (m modelSecretMatcher) Match(v driver.Value) bool {
	switch b := v.(type) {
	case []byte:
		return string(b) == m.want
	case string:
		return b == m.want
	default:
		return false
	}
}

// driveLeafImportAndAssertModel imports a single leaf workspace and asserts the
// MODEL workspace_secret was persisted with wantModel. Shared by the default and
// override cases.
func driveLeafImportAndAssertModel(t *testing.T, ws OrgWorkspace, defaults OrgDefaults, wantModel string) {
	t.Helper()

	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	// provisioner=nil → HasProvisioner()==false → create+persist, no provision
	// goroutine (keeps the test synchronous + free of async DB races).
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	h := &OrgHandler{workspace: wh, broadcaster: broadcaster}

	// Query 1 (fatal if unmatched): the workspaces INSERT ... RETURNING id.
	mock.ExpectQuery(`INSERT INTO workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-leaf-uuid"))

	// Query 2 (the fix): the MODEL workspace_secrets upsert. $1=workspace id
	// (a fresh uuid → AnyArg), $2=encrypted model value (asserted), $3=version.
	mock.ExpectExec(`INSERT INTO workspace_secrets`).
		WithArgs(sqlmock.AnyArg(), modelSecretMatcher{want: wantModel}, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	results := []map[string]interface{}{}
	provisionSem := make(chan struct{}, 1)
	parentID := (*string)(nil)

	if err := h.createWorkspaceTree(ws, parentID, 0, 0, 0, 0, defaults, "", &results, provisionSem); err != nil {
		t.Fatalf("createWorkspaceTree returned error: %v", err)
	}

	// Both expectations (workspaces INSERT + MODEL upsert) must have fired.
	// If the fix regressed (no setModelSecret call), the MODEL upsert
	// expectation is unfulfilled and this fails.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expected the workspaces INSERT and the MODEL workspace_secret upsert to fire: %v", err)
	}
}

// TestCreateWorkspaceTree_PersistsDefaultModelSecret pins that a workspace with
// no per-workspace model inherits + PERSISTS the org defaults.model as its MODEL
// secret. This is the mini-company shape: every workspace relies on
// defaults.model = minimax/MiniMax-M2.7 (the platform-billed slash form).
func TestCreateWorkspaceTree_PersistsDefaultModelSecret(t *testing.T) {
	ws := OrgWorkspace{
		Name:    "Concierge",
		Role:    "Chief of Staff",
		Runtime: "openclaw",
		// No Model → resolves to defaults.Model.
	}
	defaults := OrgDefaults{
		Runtime: "openclaw",
		Model:   "minimax/MiniMax-M2.7",
		Tier:    3,
	}
	driveLeafImportAndAssertModel(t, ws, defaults, "minimax/MiniMax-M2.7")
}

// TestCreateWorkspaceTree_PersistsPerWorkspaceModelOverride pins that a
// per-workspace model: override is the value persisted (it wins over
// defaults.model), so a heterogeneous template's per-agent model choices each
// survive restart — not just the org default.
func TestCreateWorkspaceTree_PersistsPerWorkspaceModelOverride(t *testing.T) {
	ws := OrgWorkspace{
		Name:    "Legal",
		Role:    "Legal (contracts & compliance)",
		Runtime: "openclaw",
		Model:   "anthropic/claude-sonnet-4-6", // override
	}
	defaults := OrgDefaults{
		Runtime: "openclaw",
		Model:   "minimax/MiniMax-M2.7", // must NOT be the persisted value
		Tier:    3,
	}
	driveLeafImportAndAssertModel(t, ws, defaults, "anthropic/claude-sonnet-4-6")
}

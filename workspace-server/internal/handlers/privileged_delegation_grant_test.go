package handlers

// Scoped single-use grant for PRIVILEGED / boundary-crossing delegations.
// Negative-controls BOTH directions:
//   - a privileged delegation WITHOUT a consumable grant is REJECTED (403);
//   - a privileged delegation WITH one succeeds AND consumes the grant;
//   - a routine intra-org sibling A2A caller (no admin/org token) is UNAFFECTED
//     and needs no grant — even with the gate armed;
//   - the whole gate is dormant (byte-identical to prior behaviour) until the
//     MOLECULE_PRIVILEGED_DELEGATION_GATE flag is set.
//
// These exercise gatePrivilegedDelegation directly (b=nil, so no broadcast
// query) so the DB expectations are exactly requireApproval's consume/create
// sequence — mirroring approval_gate_scope_test.go.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func newDelegCtx(cred string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-source/delegate", nil)
	switch cred {
	case "org-token":
		c.Set("org_token_id", "tok")
	case "admin-token":
		c.Set("caller_is_admin_token", true)
	}
	return c
}

// newDelegCtxRec is like newDelegCtx but returns the recorder so callers can
// inspect the written status/body.
func newDelegCtxRec(cred string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-source/delegate", nil)
	switch cred {
	case "org-token":
		c.Set("org_token_id", "tok")
	case "admin-token":
		c.Set("caller_is_admin_token", true)
	}
	return c, w
}

func TestPrivilegedDelegationGateEnabled_DefaultOff(t *testing.T) {
	os.Unsetenv("MOLECULE_PRIVILEGED_DELEGATION_GATE")
	if privilegedDelegationGateEnabled() {
		t.Fatal("gate must be OFF by default (no env)")
	}
	for _, v := range []string{"1", "true"} {
		t.Setenv("MOLECULE_PRIVILEGED_DELEGATION_GATE", v)
		if !privilegedDelegationGateEnabled() {
			t.Errorf("%q must enable the gate", v)
		}
	}
	t.Setenv("MOLECULE_PRIVILEGED_DELEGATION_GATE", "0")
	if privilegedDelegationGateEnabled() {
		t.Error(`"0" must keep the gate off`)
	}
}

// The privileged classifier: only admin/org-token callers are privileged, and
// only while the flag is armed. Workspace/canvas/system callers (routine A2A)
// are never privileged.
func TestDelegationRequiresGrant_Classifier(t *testing.T) {
	// Flag OFF → nobody requires a grant, regardless of credential.
	os.Unsetenv("MOLECULE_PRIVILEGED_DELEGATION_GATE")
	for _, cred := range []string{"none", "org-token", "admin-token"} {
		if delegationRequiresGrant(newDelegCtx(cred)) {
			t.Errorf("flag off: %q must NOT require a grant", cred)
		}
	}
	// Flag ON → admin/org-token privileged; routine caller still fluid.
	t.Setenv("MOLECULE_PRIVILEGED_DELEGATION_GATE", "1")
	if delegationRequiresGrant(newDelegCtx("none")) {
		t.Error("flag on + no privileged token → routine A2A must NOT require a grant")
	}
	if !delegationRequiresGrant(newDelegCtx("admin-token")) {
		t.Error("flag on + admin-token → must require a grant")
	}
	if !delegationRequiresGrant(newDelegCtx("org-token")) {
		t.Error("flag on + org-token → must require a grant")
	}
}

// NEG-CONTROL (routine unaffected): a routine intra-org sibling A2A caller
// proceeds with NO grant and NO DB touch — even with the gate armed. If any DB
// query fired, requireApproval would error and the gate would return false.
func TestGatePrivilegedDelegation_RoutineBypass(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_ = setupTestDB(t) // db.DB is the mock; NO queries are expected on this path

	t.Setenv("MOLECULE_PRIVILEGED_DELEGATION_GATE", "1")
	if proceed, grantID := gatePrivilegedDelegation(newDelegCtx("none"), nil, "ws-source", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "do the thing"); !proceed {
		t.Error("routine (no privileged token) delegation must proceed with no grant")
	} else if grantID != "" {
		t.Errorf("routine delegation must consume NO grant, got grantID=%q", grantID)
	}

	// Gate dormant (flag off) + admin-token → still proceeds, no DB touch, no grant.
	os.Unsetenv("MOLECULE_PRIVILEGED_DELEGATION_GATE")
	if proceed, grantID := gatePrivilegedDelegation(newDelegCtx("admin-token"), nil, "ws-source", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "do the thing"); !proceed {
		t.Error("gate dormant: admin-token delegation must proceed unchanged")
	} else if grantID != "" {
		t.Errorf("gate dormant: must consume NO grant, got grantID=%q", grantID)
	}
}

// NEG-CONTROL (hole closed): a privileged delegation with NO consumable grant
// is REJECTED with 403 and dispatches nothing.
func TestGatePrivilegedDelegation_RejectedWithoutGrant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mock := setupTestDB(t)
	t.Setenv("MOLECULE_PRIVILEGED_DELEGATION_GATE", "1")

	// requireApproval sequence, no pre-existing approval: UPDATE consumes
	// nothing (ErrNoRows), INSERT returns a fresh pending grant id, parent
	// lookup returns nil. (b=nil → no broadcast query.)
	mock.ExpectQuery(`UPDATE approval_requests SET consumed_at`).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`WITH existing AS`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("grant-pending-1"))
	mock.ExpectQuery(`SELECT parent_id FROM workspaces WHERE id`).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))

	c, w := newDelegCtxRec("admin-token")
	proceed, grantID := gatePrivilegedDelegation(c, nil, "ws-source", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "delete prod")
	if proceed {
		t.Fatal("privileged delegation WITHOUT a grant MUST be rejected (want proceed=false)")
	}
	if grantID != "" {
		t.Errorf("a rejected delegation consumes NO grant, so grantID must be empty, got %q", grantID)
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response json: %v", err)
	}
	if resp["grant_request_id"] != "grant-pending-1" {
		t.Errorf("want grant_request_id=grant-pending-1, got %v", resp["grant_request_id"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/extra DB expectations: %v", err)
	}
}

// NEG-CONTROL (legit succeeds): a privileged delegation WITH a matching
// approved grant proceeds AND consumes it (single-use).
func TestGatePrivilegedDelegation_ConsumesGrantAndProceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mock := setupTestDB(t)
	t.Setenv("MOLECULE_PRIVILEGED_DELEGATION_GATE", "1")

	// The atomic consume UPDATE ... RETURNING id finds an approved+unconsumed
	// grant and consumes it — exactly requireApproval's single-use half. No
	// INSERT/parent-lookup follows on the consumed path.
	mock.ExpectQuery(`UPDATE approval_requests SET consumed_at`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("grant-approved-42"))

	c := newDelegCtx("admin-token")
	proceed, grantID := gatePrivilegedDelegation(c, nil, "ws-source", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "delete prod")
	if !proceed {
		t.Fatal("privileged delegation WITH a valid grant MUST proceed (want proceed=true)")
	}
	// The CONSUMED grant id must be surfaced so the dispatch path can restore it
	// if the hand-off later fails (FINDING[3]).
	if grantID != "grant-approved-42" {
		t.Errorf("want consumed grantID=grant-approved-42, got %q", grantID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("grant was not consumed as expected: %v", err)
	}
}

// Anti-replay: the grant is bound to (target, task) so an approved grant for one
// operation cannot be replayed to a different target or task.
func TestPrivilegedDelegationContext_BindsTargetAndTask(t *testing.T) {
	a := privilegedDelegationContext("target-A", "task one")
	b := privilegedDelegationContext("target-B", "task one")
	if a["target_id"] == b["target_id"] {
		t.Error("different targets must yield different context target_id")
	}
	c := privilegedDelegationContext("target-A", "task two")
	if a["task_hash"] == c["task_hash"] {
		t.Error("different tasks must yield different task_hash")
	}
	if a["task_hash"] == "" {
		t.Error("task_hash must be populated")
	}
}

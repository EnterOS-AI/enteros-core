package handlers

// Rework proofs for the privileged-delegation single-use grant gate (PR #4539
// review: FINDING[2] idempotent-replay ordering, FINDING[3] recoverable
// consume, FINDING[7] IsGated SSOT wiring).
//
// Non-vacuous by construction: every id is a REALISTIC, DISTINCT UUID (the
// original masking bug hid because source/target/grant collapsed to the same
// literal on both sides of the assertion), and each hole is negative-controlled
// in BOTH directions — the attack is rejected AND the legit flow still passes.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/approvals"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// FINDING[2]: the grant gate runs AFTER the idempotency lookup, so an idempotent
// REPLAY of an already-accepted delegation replays the original delegation_id
// WITHOUT requiring or consuming a fresh grant.
//
// This is the regression gate for the mis-sequenced gate: with the old ordering
// (consume BEFORE lookupIdempotentDelegation) an armed privileged retry re-enters
// the gate, finds the one-shot grant already consumed, and 403s instead of
// replaying. Here the ONLY DB expectation is the idempotency SELECT — no
// approval_requests UPDATE. On the old ordering the gate's consume UPDATE would
// fire first, mismatch the expectation, and the handler would 500 (or 403) — so
// a green 200-idempotent_hit is only reachable with the fix.
func TestDelegate_PrivilegedIdempotentReplay_ReplaysWithoutConsumingGrant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	// Armed gate + admin-token caller = a PRIVILEGED delegation that would need a
	// grant on a fresh dispatch.
	t.Setenv("MOLECULE_PRIVILEGED_DELEGATION_GATE", "1")

	// Distinct realistic UUIDs on every axis.
	const (
		sourceID   = "11111111-1111-4111-8111-111111111111"
		targetID   = "22222222-2222-4222-8222-222222222222"
		existingID = "33333333-3333-4333-8333-333333333333"
		idemKey    = "idem-replay-key-abc"
	)

	// The idempotency lookup finds an in-flight (non-failed) prior delegation.
	// (loadWorkspaceCanDelegate's SELECT can_delegate fires first and is
	// intentionally unmatched → errors → treated as can_delegate unknown →
	// proceeds, exactly like the existing idempotency tests.)
	mock.ExpectQuery("SELECT request_body->>'delegation_id', status, target_id").
		WithArgs(sourceID, idemKey).
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "status", "target_id"}).
			AddRow(existingID, "dispatched", targetID))
	// DELIBERATELY NO `UPDATE approval_requests SET consumed_at` expectation:
	// the replay must short-circuit BEFORE the grant gate. If the gate ran, its
	// consume UPDATE would be an unexpected query.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: sourceID}}
	c.Set("caller_is_admin_token", true) // privileged caller
	body := fmt.Sprintf(`{"target_id":"%s","task":"delete prod","idempotency_key":"%s"}`, targetID, idemKey)
	c.Request = httptest.NewRequest("POST", "/workspaces/"+sourceID+"/delegate", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.Delegate(c)

	if w.Code != http.StatusOK {
		t.Fatalf("armed privileged idempotent REPLAY must replay (200), got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response json: %v", err)
	}
	if resp["delegation_id"] != existingID {
		t.Errorf("replay must return the ORIGINAL delegation_id %s, got %v", existingID, resp["delegation_id"])
	}
	if resp["idempotent_hit"] != true {
		t.Errorf("want idempotent_hit=true, got %v", resp["idempotent_hit"])
	}
	// Proves no grant consume fired: an unexpected approval UPDATE would have
	// been recorded here as an unmet/extra expectation (and would have 500'd).
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("replay must consume NO grant — unexpected DB activity: %v", err)
	}
}

// FINDING[7]: the gate decision is wired through the approvals SSOT (IsGated),
// not a divergent inline classifier. Dropping ActionPrivilegedDelegate from the
// gated map must make the delegation gate inert even while armed.
//
// Negative control on the wiring itself: baseline (armed + privileged) requires
// a grant; un-gating in the SSOT flips it off; both privileged credential
// classes track the SSOT.
func TestDelegationRequiresGrant_WiredThroughIsGatedSSOT(t *testing.T) {
	t.Setenv("MOLECULE_PRIVILEGED_DELEGATION_GATE", "1")

	// Baseline: armed + privileged → requires a grant.
	if !delegationRequiresGrant(newDelegCtx("admin-token")) {
		t.Fatal("armed + admin-token must require a grant (baseline)")
	}
	if !delegationRequiresGrant(newDelegCtx("org-token")) {
		t.Fatal("armed + org-token must require a grant (baseline)")
	}

	// Remove the action from the single source of truth.
	restore := approvals.SetGatedForTest(approvals.ActionPrivilegedDelegate, false)
	defer restore()

	if delegationRequiresGrant(newDelegCtx("admin-token")) {
		t.Error("un-gating ActionPrivilegedDelegate in the SSOT must make the gate inert for admin-token")
	}
	if delegationRequiresGrant(newDelegCtx("org-token")) {
		t.Error("un-gating ActionPrivilegedDelegate in the SSOT must make the gate inert for org-token")
	}
}

// FINDING[3] (unit half): the consume is recoverable — restore returns a grant
// to the unconsumed pool with a workspace-scoped UPDATE that clears consumed_at.
func TestRestorePrivilegedDelegationGrant_ClearsConsumedAt(t *testing.T) {
	mock := setupTestDB(t)

	const (
		grantID = "44444444-4444-4444-8444-444444444444"
		wsID    = "55555555-5555-4555-8555-555555555555"
	)
	mock.ExpectExec(`UPDATE approval_requests\s+SET consumed_at = NULL`).
		WithArgs(grantID, wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	restorePrivilegedDelegationGrant(context.Background(), wsID, grantID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("restore must fire the workspace-scoped clear-consumed UPDATE: %v", err)
	}
}

// FINDING[3] negative control: an empty grant id (routine / gate-off / a reject
// that consumed nothing) must NEVER touch the DB — otherwise every ordinary
// delegation's failure path would issue a stray UPDATE.
func TestRestorePrivilegedDelegationGrant_EmptyGrantIsNoDBTouch(t *testing.T) {
	mock := setupTestDB(t)
	// No expectation registered: any query here is an unexpected-call failure.

	restorePrivilegedDelegationGrant(context.Background(), "any-ws", "")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("empty grantID must not touch the DB: %v", err)
	}
}

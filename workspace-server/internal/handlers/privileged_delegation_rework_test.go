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
	"database/sql"
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

// FINDING[4]: concurrent idempotent-retry race. Two same-idempotency-key
// requests can both slip past lookupIdempotentDelegation (each reads before the
// other writes). The gate now runs AFTER the unique-winning insert, so the
// LOSING request resolves at the insert's unique-constraint conflict and REPLAYS
// the winner's delegation_id WITHOUT ever reaching — or consuming — a grant. A
// legit idempotent retry therefore never 403s and never burns a grant, even
// while armed.
//
// Regression proof: the only approval_requests activity that could fire is a
// consume UPDATE, and NO such expectation is registered. Under the OLD ordering
// (gate BEFORE insert) an armed privileged retry consumes the sole single-use
// grant first — a consume UPDATE that mismatches the INSERT expectation and
// 500s, OR (with a grant already spent by the winner) a hard 403. A green
// 200-idempotent_hit with zero approval DB touches is only reachable with the
// gate moved below the insert. Distinct realistic UUIDs on every axis so a
// collapsed-literal can't mask the winner-id assertion.
func TestDelegate_PrivilegedConcurrentRetryRace_ReplaysWithoutGrantOr403(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	// Armed gate + admin-token caller = a PRIVILEGED delegation that would need a
	// grant on a genuinely-fresh dispatch.
	t.Setenv("MOLECULE_PRIVILEGED_DELEGATION_GATE", "1")

	const (
		sourceID = "aaaaaaaa-1111-4a11-8a11-aaaaaaaaaaaa"
		targetID = "bbbbbbbb-2222-4b22-8b22-bbbbbbbbbbbb"
		winnerID = "cccccccc-3333-4c33-8c33-cccccccccccc"
		idemKey  = "idem-race-key-xyz"
	)

	// (1) idempotency lookup MISSES — the concurrent winner has not committed its
	//     row yet, so this retry believes it is the first.
	mock.ExpectQuery("SELECT request_body->>'delegation_id', status, target_id").
		WithArgs(sourceID, idemKey).
		WillReturnError(fmt.Errorf("sql: no rows in result set"))
	// (2) THIS request LOSES the unique-constraint race on insert.
	mock.ExpectExec("INSERT INTO activity_logs").
		WithArgs(sourceID, sourceID, targetID, "Delegating to "+targetID, sqlmock.AnyArg(), sqlmock.AnyArg(), idemKey).
		WillReturnError(fmt.Errorf("pq: duplicate key value violates unique constraint \"activity_logs_idempotency_uniq\""))
	// (3) re-query resolves to the committed WINNER — replay path.
	mock.ExpectQuery("SELECT request_body->>'delegation_id', status").
		WithArgs(sourceID, idemKey).
		WillReturnRows(sqlmock.NewRows([]string{"delegation_id", "status"}).
			AddRow(winnerID, "dispatched"))
	// DELIBERATELY NO `UPDATE approval_requests SET consumed_at`: the loser must
	// replay BEFORE the grant gate. A consume here would be an unexpected query.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: sourceID}}
	c.Set("caller_is_admin_token", true) // privileged caller
	body := fmt.Sprintf(`{"target_id":"%s","task":"delete prod","idempotency_key":"%s"}`, targetID, idemKey)
	c.Request = httptest.NewRequest("POST", "/workspaces/"+sourceID+"/delegate", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.Delegate(c)

	if w.Code != http.StatusOK {
		t.Fatalf("armed privileged CONCURRENT idempotent retry must REPLAY (200), never 403/500; got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response json: %v", err)
	}
	if resp["delegation_id"] != winnerID {
		t.Errorf("retry must replay the WINNER's delegation_id %s, got %v", winnerID, resp["delegation_id"])
	}
	if resp["idempotent_hit"] != true {
		t.Errorf("want idempotent_hit=true, got %v", resp["idempotent_hit"])
	}
	// Proves the loser touched NO grant: an approval consume/create UPDATE would
	// be an unexpected/extra expectation here (and would have 500'd or 403'd).
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("concurrent retry must consume NO grant and 403 nothing — unexpected DB activity: %v", err)
	}
}

// FINDING[4] (negative control / reject direction): a GENUINELY-new privileged
// delegation with no consumable grant must still be REJECTED (403) — and because
// the pending row is now inserted BEFORE the gate, the reject path must ROLL IT
// BACK (bounded DELETE) so the never-dispatched delegation neither holds the
// idempotency slot nor lingers as a phantom in-flight row. This guards against
// the reorder accidentally (a) making the gate permissive, or (b) leaking the
// speculative row on a 403.
func TestDelegate_PrivilegedNewDelegationNoGrant_Rejected403AndRollsBackRow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	t.Setenv("MOLECULE_PRIVILEGED_DELEGATION_GATE", "1")

	const (
		sourceID = "dddddddd-4444-4d44-8d44-dddddddddddd"
		targetID = "eeeeeeee-5555-4e55-8e55-eeeeeeeeeeee"
	)

	// No idempotency_key → lookup issues no query; loadWorkspaceCanDelegate's
	// SELECT is unmatched (errors → proceeds), then the pending INSERT wins.
	mock.ExpectExec("INSERT INTO activity_logs").
		WithArgs(sourceID, sourceID, targetID, "Delegating to "+targetID, sqlmock.AnyArg(), sqlmock.AnyArg(), nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Gate: requireApproval finds no approved grant (consume UPDATE → ErrNoRows),
	// creates a pending grant request, broadcasts EventApprovalRequested (one
	// structure_events INSERT), parent lookup returns NULL → 403.
	mock.ExpectQuery(`UPDATE approval_requests SET consumed_at`).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`WITH existing AS`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("grant-pending-req-1"))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT parent_id FROM workspaces WHERE id`).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))
	// The reject MUST roll back the speculative pending row — bounded to exactly
	// this (workspace_id, delegation_id) pending delegate row.
	mock.ExpectExec("DELETE FROM activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: sourceID}}
	c.Set("caller_is_admin_token", true) // privileged caller
	body := fmt.Sprintf(`{"target_id":"%s","task":"delete prod"}`, targetID)
	c.Request = httptest.NewRequest("POST", "/workspaces/"+sourceID+"/delegate", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	dh.Delegate(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("armed privileged NEW delegation with no grant must be REJECTED (403), got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response json: %v", err)
	}
	if resp["grant_request_id"] != "grant-pending-req-1" {
		t.Errorf("want grant_request_id=grant-pending-req-1, got %v", resp["grant_request_id"])
	}
	// ExpectationsWereMet proves the rollback DELETE actually fired — an
	// un-rolled-back row would leave the DELETE expectation unmet.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("reject must 403 AND roll back the speculative pending row: %v", err)
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

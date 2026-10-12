package handlers

// Tests for the audit_events PRODUCER.
//
// Every test here is written to go RED against the pre-fix tree:
//   - the wiring tests assert an INSERT INTO audit_events happens on a
//     lifecycle route (pre-fix: no INSERT existed anywhere in the repo);
//   - the chain tests assert prev_hmac actually links successive rows and that
//     the shipped verifier accepts what the shipped writer produces (pre-fix:
//     unverifiable — nothing produced rows);
//   - the canonical-form test pins the backward-compatibility rule that makes
//     adding the details column safe.

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ============================= helpers =====================================

// auditWriterMock returns a sqlmock DB with ordering disabled. The audited
// handlers interleave their own queries with the ledger append, and this file
// asserts on the ledger append specifically, not on statement order.
func auditWriterMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	mock.MatchExpectationsInOrder(false)
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

// expectAuditAppendCapturing declares the full statement sequence of one
// ledger append (begin -> advisory lock -> chain tail -> insert -> commit) and
// captures the INSERT arguments so a test can assert on the row that was
// actually written.
//
// prevHMAC is the chain tail to hand back; "" means "no predecessor".
func expectAuditAppendCapturing(t *testing.T, mock sqlmock.Sqlmock, prevHMAC string, rec *recordedAuditArgs) {
	t.Helper()
	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))

	tail := sqlmock.NewRows([]string{"hmac", "timestamp"})
	if prevHMAC != "" {
		tail = tail.AddRow(prevHMAC, time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC))
	}
	mock.ExpectQuery(`SELECT hmac, timestamp`).WillReturnRows(tail)

	mock.ExpectExec(`INSERT INTO audit_events`).
		WithArgs(
			auditArg(func(v driver.Value) { rec.id = auditArgString(v) }),
			auditArg(func(v driver.Value) {
				if ts, ok := v.(time.Time); ok {
					rec.timestamp = ts
				}
			}),
			auditArg(func(v driver.Value) { rec.agentID = auditArgString(v) }),
			auditArg(func(v driver.Value) { rec.sessionID = auditArgString(v) }),
			auditArg(func(v driver.Value) { rec.operation = auditArgString(v) }),
			auditArg(nil), // input_hash
			auditArg(nil), // output_hash
			auditArg(nil), // model_used
			auditArg(func(v driver.Value) { rec.humanFlag = auditArgBool(v) }),
			auditArg(func(v driver.Value) { rec.riskFlag = auditArgBool(v) }),
			auditArg(func(v driver.Value) {
				if v != nil {
					s := auditArgString(v)
					rec.prevHMAC = &s
				}
			}),
			auditArg(func(v driver.Value) { rec.hmac = auditArgString(v); rec.seen = true }),
			auditArg(func(v driver.Value) { rec.workspaceID = auditArgString(v) }),
			auditArg(func(v driver.Value) {
				if v != nil {
					s := auditArgString(v)
					rec.details = &s
				}
			}),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
}

// recordedAuditArgs holds the INSERT arguments observed by the matchers above.
type recordedAuditArgs struct {
	id, agentID, sessionID, operation, hmac, workspaceID string
	prevHMAC                                             *string
	details                                              *string
	riskFlag, humanFlag                                  bool
	timestamp                                            time.Time
	seen                                                 bool
}

// auditAnyArg matches any driver value and optionally records it. sqlmock has
// no built-in "match anything but tell me what it was" argument.
type auditAnyArg struct{ fn func(driver.Value) }

func (a auditAnyArg) Match(v driver.Value) bool {
	if a.fn != nil {
		a.fn(v)
	}
	return true
}

func auditArg(fn func(driver.Value)) auditAnyArg { return auditAnyArg{fn: fn} }

func auditArgString(v driver.Value) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	}
	return ""
}

func auditArgBool(v driver.Value) bool {
	b, _ := v.(bool)
	return b
}

func newAuditTestCtx(w *httptest.ResponseRecorder, method, path string) *gin.Context {
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, nil)
	return c
}

// ============================= writer: chain =================================

// TestRecordAuditEvent_WritesFirstRowWithNullPrev is the base case: an empty
// chain produces a row whose prev_hmac is NULL and whose hmac is the canonical
// signature the shipped verifier recomputes.
//
// MUTATION CHECK: pre-fix there is no RecordAuditEvent at all; with the
// function present but its INSERT removed, ExpectationsWereMet fails.
func TestRecordAuditEvent_WritesFirstRowWithNullPrev(t *testing.T) {
	t.Setenv("AUDIT_LEDGER_SALT", "chain-test-salt")
	resetAuditKeyCache()
	t.Cleanup(resetAuditKeyCache)

	database, mock := auditWriterMock(t)
	rec := &recordedAuditArgs{}
	expectAuditAppendCapturing(t, mock, "", rec)

	RecordAuditEvent(context.Background(), database, AuditEntry{
		WorkspaceID: "ws-1",
		Actor:       "admin-token",
		SessionID:   "req-7",
		Operation:   AuditOpWorkspaceCreate,
		Details:     map[string]any{"workspace_name": "client-site"},
	})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ledger append did not happen as specified: %v", err)
	}
	if !rec.seen {
		t.Fatal("no INSERT INTO audit_events was issued")
	}
	if rec.prevHMAC != nil {
		t.Errorf("first row in a chain must have NULL prev_hmac, got %q", *rec.prevHMAC)
	}
	if rec.agentID != "admin-token" {
		t.Errorf("agent_id must carry the actor, got %q", rec.agentID)
	}
	if rec.operation != AuditOpWorkspaceCreate {
		t.Errorf("operation = %q, want %q", rec.operation, AuditOpWorkspaceCreate)
	}
	if rec.details == nil || !strings.Contains(*rec.details, "client-site") {
		t.Errorf("details must name the subject, got %v", rec.details)
	}

	// The stored hmac must be exactly what the shipped verifier recomputes.
	row := &auditEventRow{
		ID: rec.id, Timestamp: rec.timestamp, AgentID: rec.agentID,
		SessionID: rec.sessionID, Operation: rec.operation,
		HumanOversightFlag: rec.humanFlag, RiskFlag: rec.riskFlag,
		PrevHMAC: rec.prevHMAC, WorkspaceID: rec.workspaceID, Details: rec.details,
	}
	want := computeAuditHMAC(getAuditHMACKey(), row)
	if rec.hmac != want {
		t.Errorf("stored hmac %q is not the canonical signature %q — the verifier would call this tampered", rec.hmac, want)
	}
}

// TestRecordAuditEvent_LinksToChainTail proves prev_hmac actually chains: the
// second append must carry the first row's hmac.
func TestRecordAuditEvent_LinksToChainTail(t *testing.T) {
	t.Setenv("AUDIT_LEDGER_SALT", "chain-test-salt")
	resetAuditKeyCache()
	t.Cleanup(resetAuditKeyCache)

	const tail = "aaaabbbbccccdddd0000111122223333444455556666777788889999aaaabbbb"

	database, mock := auditWriterMock(t)
	rec := &recordedAuditArgs{}
	expectAuditAppendCapturing(t, mock, tail, rec)

	RecordAuditEvent(context.Background(), database, AuditEntry{
		WorkspaceID: "ws-1",
		Actor:       "admin-token",
		Operation:   AuditOpWorkspaceDelete,
		RiskFlag:    true,
	})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
	if rec.prevHMAC == nil {
		t.Fatal("prev_hmac is NULL — the chain is not linked, so the ledger is not tamper-evident")
	}
	if *rec.prevHMAC != tail {
		t.Errorf("prev_hmac = %q, want the chain tail %q", *rec.prevHMAC, tail)
	}
	if !rec.riskFlag {
		t.Error("a delete must be recorded with risk_flag set")
	}
}

// TestRecordAuditEvent_ChainRoundTripsThroughVerifier is the end-to-end proof
// that the writer and the shipped verifier agree: two rows produced by the
// writer's own signing path must verify as a valid chain.
func TestRecordAuditEvent_ChainRoundTripsThroughVerifier(t *testing.T) {
	t.Setenv("AUDIT_LEDGER_SALT", "roundtrip-salt")
	resetAuditKeyCache()
	t.Cleanup(resetAuditKeyCache)

	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	d1 := `{"workspace_name":"a"}`
	d2 := `{"workspace_name":"b"}`

	first := &auditEventRow{
		ID: "evt-1", Timestamp: base, AgentID: "admin-token", SessionID: "r1",
		Operation: AuditOpWorkspaceCreate, WorkspaceID: "ws-1", Details: &d1,
	}
	first.HMAC = signAuditRow(first)

	second := &auditEventRow{
		ID: "evt-2", Timestamp: base.Add(time.Second), AgentID: "admin-token", SessionID: "r2",
		Operation: AuditOpWorkspaceDelete, RiskFlag: true, WorkspaceID: "ws-1",
		PrevHMAC: &first.HMAC, Details: &d2,
	}
	second.HMAC = signAuditRow(second)

	valid := verifyAuditChain([]auditEventRow{*first, *second})
	if valid == nil || !*valid {
		t.Fatalf("writer-produced chain failed the shipped verifier: %v", valid)
	}

	// Negative control: mutate a signed field and the verifier must reject it.
	tampered := []auditEventRow{*first, *second}
	tampered[0].Operation = "workspace.create.but.not.really"
	if v := verifyAuditChain(tampered); v == nil || *v {
		t.Errorf("verifier accepted a tampered row: %v", v)
	}

	// Negative control 2: break the link and the verifier must reject it.
	broken := []auditEventRow{*first, *second}
	other := "deadbeef"
	broken[1].PrevHMAC = &other
	broken[1].HMAC = signAuditRow(&broken[1])
	if v := verifyAuditChain(broken); v == nil || *v {
		t.Errorf("verifier accepted a broken chain link: %v", v)
	}
}

// TestRecordAuditEvent_UnsignedWhenSaltMissing pins the degraded-but-recorded
// behaviour: with no AUDIT_LEDGER_SALT the event is STILL written (losing the
// record is worse than losing tamper-evidence) and the row self-declares that
// it is unsigned.
func TestRecordAuditEvent_UnsignedWhenSaltMissing(t *testing.T) {
	t.Setenv("AUDIT_LEDGER_SALT", "")
	resetAuditKeyCache()
	t.Cleanup(resetAuditKeyCache)

	database, mock := auditWriterMock(t)
	rec := &recordedAuditArgs{}
	expectAuditAppendCapturing(t, mock, "", rec)

	RecordAuditEvent(context.Background(), database, AuditEntry{
		WorkspaceID: "ws-1", Actor: "admin-token", Operation: AuditOpWorkspaceCreate,
	})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("event must still be recorded without a salt: %v", err)
	}
	if !auditIsUnsigned(rec.hmac) {
		t.Errorf("row written without a key must carry the unsigned marker, got %q", rec.hmac)
	}
}

// TestRecordAuditEvent_NeverPanicsOnDBFailure — the ledger append runs after
// the audited mutation committed, so it must degrade to a log line, never take
// down the request.
func TestRecordAuditEvent_NeverPanicsOnDBFailure(t *testing.T) {
	t.Setenv("AUDIT_LEDGER_SALT", "salt")
	resetAuditKeyCache()
	t.Cleanup(resetAuditKeyCache)

	database, mock := auditWriterMock(t)
	mock.ExpectBegin().WillReturnError(sql.ErrConnDone)

	RecordAuditEvent(context.Background(), database, AuditEntry{
		WorkspaceID: "ws-1", Actor: "a", Operation: AuditOpWorkspaceCreate,
	})
	// Reaching here without a panic is the assertion.
	RecordAuditEvent(context.Background(), nil, AuditEntry{
		WorkspaceID: "ws-1", Actor: "a", Operation: AuditOpWorkspaceCreate,
	})
}

// TestRecordAuditEvent_RequiresAnchor — a row with no anchor workspace cannot
// satisfy the NOT NULL FK, so it must be dropped with a loud log rather than
// issuing a doomed INSERT.
func TestRecordAuditEvent_RequiresAnchor(t *testing.T) {
	database, mock := auditWriterMock(t)
	RecordAuditEvent(context.Background(), database, AuditEntry{
		WorkspaceID: "", Actor: "a", Operation: AuditOpWorkspaceCreate,
	})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no statements should have been issued: %v", err)
	}
}

// ============================= canonical form ===============================

// TestAuditCanonicalPayload_OmitsDetailsWhenNil is the backward-compatibility
// gate for the details column. A row written before the column existed has
// details NULL and MUST hash exactly as it did before, or adding the column
// would retroactively invalidate every historical chain.
func TestAuditCanonicalPayload_OmitsDetailsWhenNil(t *testing.T) {
	ev := &auditEventRow{
		ID: "e", Timestamp: time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
		AgentID: "a", SessionID: "s", Operation: "o",
	}
	payload := auditCanonicalPayload(ev)
	if strings.Contains(payload, "details") {
		t.Fatalf("NULL details must be OMITTED from the canonical form, got %s", payload)
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("canonical payload is not valid JSON: %v", err)
	}
	if _, present := decoded["details"]; present {
		t.Error("details key present for a NULL column")
	}

	// And with details set it MUST be covered by the signature, otherwise the
	// subject of an event could be rewritten without detection.
	d := `{"workspace_id":"ws-1"}`
	ev.Details = &d
	withDetails := auditCanonicalPayload(ev)
	if withDetails == payload {
		t.Error("details is not covered by the canonical form — an attacker could rewrite the subject of an event")
	}
}

// TestAuditQuery_UnsignedEventsReportedDistinctly — unsigned rows must not be
// reported as "verified" (a silent pass) nor as "tampered" (a false alarm
// sending operators after an attacker who does not exist).
func TestAuditQuery_UnsignedEventsReportedDistinctly(t *testing.T) {
	t.Setenv("AUDIT_LEDGER_SALT", "present-now")
	resetAuditKeyCache()
	t.Cleanup(resetAuditKeyCache)

	mock := setupTestDB(t)
	cols := []string{"id", "timestamp", "agent_id", "session_id", "operation",
		"input_hash", "output_hash", "model_used", "human_oversight_flag",
		"risk_flag", "prev_hmac", "hmac", "workspace_id", "details"}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM audit_events")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT id, timestamp, agent_id`).
		WillReturnRows(sqlmock.NewRows(cols).AddRow(
			"e1", time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC), "admin-token", "",
			AuditOpWorkspaceCreate, nil, nil, nil, false, false, nil,
			unsignedHMACPrefix+"abc", "ws-1", nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/audit", nil)

	NewAuditHandler().Query(c)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		ChainValid        *bool  `json:"chain_valid"`
		ChainVerification string `json:"chain_verification"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ChainVerification != "unsigned_events_present" {
		t.Errorf("chain_verification = %q, want %q", body.ChainVerification, "unsigned_events_present")
	}
	if body.ChainValid == nil || *body.ChainValid {
		t.Errorf("chain_valid must be false (fail-closed) for unsigned rows, got %v", body.ChainValid)
	}
}

// ============================= actor resolution =============================

// TestAuditActor_DiscriminatesCredentialClass is the test that speaks directly
// to the incident: a workspace was created by SOME holder of the tenant admin
// credential and nothing recorded which. Every class the middleware can
// authenticate must resolve to a distinct, non-empty actor string.
func TestAuditActor_DiscriminatesCredentialClass(t *testing.T) {
	cases := []struct {
		name string
		set  map[string]any
		want string
	}{
		{"org token", map[string]any{"org_token_prefix": "mol_ab12"}, "org-token:mol_ab12"},
		{"human session", map[string]any{"cp_session_actor": "session:hash", "cp_session_user_id": "user_01H"}, "session:user_01H"},
		{"session without user id", map[string]any{"cp_session_actor": "session:hash"}, "session:hash"},
		{"admin token", map[string]any{"caller_credential_class": "admin-token"}, "admin-token"},
		{"tier3 fallback", map[string]any{"caller_credential_class": "admin-token-tier3-fallback"}, "admin-token-tier3-fallback"},
		{"workspace token", map[string]any{"caller_credential_class": "workspace-token"}, "workspace-token"},
		{"nothing", map[string]any{}, auditActorUnknown},
	}

	seen := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c := newAuditTestCtx(w, "POST", "/workspaces")
			for k, v := range tc.set {
				c.Set(k, v)
			}
			got := auditActor(c)
			if got != tc.want {
				t.Fatalf("auditActor = %q, want %q", got, tc.want)
			}
			if got == "" {
				t.Fatal("actor must never be empty — an unattributable audit row is the bug this fixes")
			}
			if prev, dup := seen[got]; dup && prev != tc.name {
				t.Errorf("actor %q is ambiguous between %q and %q", got, prev, tc.name)
			}
			seen[got] = tc.name
		})
	}
}

// TestAuditActor_HumanOversightFlagOnlyForVerifiedSession — the flag must not
// be settable by a machine credential, or "a human approved this" becomes
// meaningless.
func TestAuditActor_HumanOversightFlagOnlyForVerifiedSession(t *testing.T) {
	w := httptest.NewRecorder()
	machine := newAuditTestCtx(w, "POST", "/workspaces")
	machine.Set("caller_credential_class", "admin-token")
	if auditIsHuman(machine) {
		t.Error("admin-token caller must not be flagged as human oversight")
	}

	human := newAuditTestCtx(httptest.NewRecorder(), "POST", "/workspaces")
	human.Set("cp_session_actor", "session:abc")
	if !auditIsHuman(human) {
		t.Error("CP-verified session must be flagged as human oversight")
	}
}

// TestAuditTokenPrefix_NeverLeaksPlaintext — the ledger is readable by anyone
// with audit access, a strictly wider set than the caller who received the
// token, so the plaintext must never land in a row.
func TestAuditTokenPrefix_NeverLeaksPlaintext(t *testing.T) {
	const secret = "mol_ws_supersecrettokenvalue"
	got := auditTokenPrefix(secret)
	if got == secret {
		t.Fatal("full token would be written to the ledger")
	}
	if len(got) >= len(secret) {
		t.Fatalf("prefix %q is not shorter than the token", got)
	}
	if !strings.HasPrefix(secret, got) {
		t.Fatalf("prefix %q is not a prefix of the token", got)
	}
	if auditTokenPrefix("short") != "" {
		t.Error("a token shorter than the prefix window must yield no prefix at all")
	}
}

// TestAuditDeleteAnchor_PrefersParent — a delete filed under the workspace
// being deleted is erased by the purge that the row records. The anchor must
// prefer the parent, which is never in the purge set.
func TestAuditDeleteAnchor_PrefersParent(t *testing.T) {
	if got := auditDeleteAnchor("parent-1", "target-1"); got != "parent-1" {
		t.Errorf("anchor = %q, want the parent — a self-anchored delete event is destroyed by its own purge", got)
	}
	if got := auditDeleteAnchor("", "target-1"); got != "target-1" {
		t.Errorf("rootless workspace must fall back to itself, got %q", got)
	}
}

// ============================= route wiring =================================
//
// These are the tests that would have caught the original bug: they assert the
// lifecycle ROUTES append to the ledger. Against the pre-fix tree they fail
// immediately — no handler issued an INSERT INTO audit_events.

// TestRevokeAuthTokens_AppendsAuditEvent — POST
// /admin/workspaces/:id/revoke-auth-tokens invalidates every live bearer for a
// workspace. That is the move an attacker makes before re-registering a
// workspace they control; it must be attributable.
func TestRevokeAuthTokens_AppendsAuditEvent(t *testing.T) {
	t.Setenv("AUDIT_LEDGER_SALT", "wiring-salt")
	resetAuditKeyCache()
	t.Cleanup(resetAuditKeyCache)

	h, mock := setupBootstrapHandler(t)
	mock.MatchExpectationsInOrder(false)

	mock.ExpectExec(`UPDATE workspace_auth_tokens`).
		WithArgs("ws-revoke").
		WillReturnResult(sqlmock.NewResult(0, 1))
	rec := &recordedAuditArgs{}
	expectAuditAppendCapturing(t, mock, "", rec)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-revoke"}}
	c.Request = httptest.NewRequest("POST", "/admin/workspaces/ws-revoke/revoke-auth-tokens", nil)
	c.Set("caller_credential_class", "admin-token")

	h.RevokeAuthTokens(c)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("revoke-all did not append an audit event: %v", err)
	}
	if rec.operation != AuditOpTokenRevoke {
		t.Errorf("operation = %q, want %q", rec.operation, AuditOpTokenRevoke)
	}
	if rec.agentID != "admin-token" {
		t.Errorf("agent_id = %q, want the authenticating credential class", rec.agentID)
	}
	if rec.workspaceID != "ws-revoke" {
		t.Errorf("workspace_id = %q, want ws-revoke", rec.workspaceID)
	}
	if !rec.riskFlag {
		t.Error("revoke-all must be recorded with risk_flag set")
	}
}

// TestTokenRevoke_AppendsAuditEvent — DELETE /workspaces/:id/tokens/:tokenId.
func TestTokenRevoke_AppendsAuditEvent(t *testing.T) {
	t.Setenv("AUDIT_LEDGER_SALT", "wiring-salt")
	resetAuditKeyCache()
	t.Cleanup(resetAuditKeyCache)

	mock := setupTestDB(t)
	mock.MatchExpectationsInOrder(false)

	mock.ExpectExec(`UPDATE workspace_auth_tokens`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	rec := &recordedAuditArgs{}
	expectAuditAppendCapturing(t, mock, "", rec)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "11111111-1111-4111-8111-111111111111"},
		{Key: "tokenId", Value: "tok-9"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/11111111-1111-4111-8111-111111111111/tokens/tok-9", nil)
	c.Set("caller_credential_class", "workspace-token")

	NewTokenHandler().Revoke(c)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("token revoke did not append an audit event: %v", err)
	}
	if rec.operation != AuditOpTokenRevoke {
		t.Errorf("operation = %q, want %q", rec.operation, AuditOpTokenRevoke)
	}
	if rec.details == nil || !strings.Contains(*rec.details, "tok-9") {
		t.Errorf("details must name the revoked token id, got %v", rec.details)
	}
}

// TestTokenRevoke_NotFoundWritesNoAuditEvent — the ledger must never claim a
// revocation that did not happen.
func TestTokenRevoke_NotFoundWritesNoAuditEvent(t *testing.T) {
	mock := setupTestDB(t)
	mock.MatchExpectationsInOrder(false)
	mock.ExpectExec(`UPDATE workspace_auth_tokens`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "11111111-1111-4111-8111-111111111111"},
		{Key: "tokenId", Value: "gone"},
	}
	c.Request = httptest.NewRequest("DELETE", "/workspaces/11111111-1111-4111-8111-111111111111/tokens/gone", nil)

	NewTokenHandler().Revoke(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestWorkspaceDelete_AppendsAuditEventAnchoredAtParent drives the real DELETE
// handler and asserts the ledger append happens — the direct regression test
// for "a workspace was deleted and nothing recorded who".
//
// The anchor assertion is the load-bearing part: filed under the target, the
// row would be destroyed by the same purge it records.
func TestWorkspaceDelete_AppendsAuditEventAnchoredAtParent(t *testing.T) {
	t.Setenv("AUDIT_LEDGER_SALT", "wiring-salt")
	resetAuditKeyCache()
	t.Cleanup(resetAuditKeyCache)

	const wsID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const parentID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

	mock, r := setupWorkspaceCrudTest(t)
	setupTestRedis(t)
	mock.MatchExpectationsInOrder(false)
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	r.DELETE("/workspaces/:id", h.Delete)

	expectWorkspaceDeleteLookupWithParent(mock, wsID, "Client Site", 0, "running", parentID)
	// No children → the confirmation gate passes and CascadeDelete runs.
	mock.ExpectQuery(`SELECT id, name FROM workspaces WHERE parent_id = \$1 AND status != 'removed'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))
	mock.ExpectQuery(`WITH RECURSIVE descendants`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	rec := &recordedAuditArgs{}
	expectAuditAppendCapturing(t, mock, "", rec)

	req, _ := http.NewRequest("DELETE", "/workspaces/"+wsID, nil)
	req.Header.Set("X-Confirm-Name", "Client Site")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if !rec.seen {
		t.Fatalf("DELETE /workspaces/:id wrote NO audit_events row (response %d: %s)", w.Code, w.Body.String())
	}
	if rec.operation != AuditOpWorkspaceDelete {
		t.Errorf("operation = %q, want %q", rec.operation, AuditOpWorkspaceDelete)
	}
	if rec.workspaceID != parentID {
		t.Errorf("audit anchor = %q, want the parent %q — a self-anchored delete row is erased by its own purge", rec.workspaceID, parentID)
	}
	if rec.agentID != "admin-token" {
		t.Errorf("agent_id = %q, want the authenticating credential class", rec.agentID)
	}
	if !rec.riskFlag {
		t.Error("a delete must carry risk_flag")
	}
	if rec.details == nil || !strings.Contains(*rec.details, wsID) {
		t.Errorf("details must name the DELETED workspace (the anchor is the parent), got %v", rec.details)
	}
}

// TestWorkspaceCreate_AppendsAuditEvent is the direct regression test for the
// incident: POST /workspaces is AdminAuth-gated, so the caller always held a
// privileged credential — but nothing wrote down WHICH one, leaving a
// client-tenant workspace creation permanently unattributable.
//
// Against the pre-fix tree this fails immediately: no create path issued an
// INSERT INTO audit_events.
func TestWorkspaceCreate_AppendsAuditEvent(t *testing.T) {
	t.Setenv("AUDIT_LEDGER_SALT", "wiring-salt")
	resetAuditKeyCache()
	t.Cleanup(resetAuditKeyCache)

	mock := setupTestDB(t)
	setupTestRedis(t)
	mock.MatchExpectationsInOrder(false)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	payload := models.CreateWorkspacePayload{
		Name:    "client-site",
		Role:    "engineer",
		Runtime: "claude-code",
		Model:   "anthropic:claude-opus-4-7",
		Tier:    3,
	}

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO canvas_layouts").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO workspace_auth_tokens").WillReturnResult(sqlmock.NewResult(0, 1))

	rec := &recordedAuditArgs{}
	expectAuditAppendCapturing(t, mock, "", rec)

	body, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBuffer(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("X-Request-Id", "req-abc123")
	// Model the AdminAuth outcome the middleware would have set: a caller
	// holding the tenant ADMIN_TOKEN — exactly the 2026-07-23 situation.
	c.Set("caller_credential_class", "admin-token")

	handler.Create(c)
	// Create dispatches provisioning work on globalAsync. Drain it before the
	// test returns so no straggler goroutine outlives the db.DB swap-back and
	// lands its query in a LATER test's sqlmock (the documented #2490 race).
	waitGlobalAsyncForTest()

	if !rec.seen {
		t.Fatalf("POST /workspaces wrote NO audit_events row (response %d: %s)", w.Code, w.Body.String())
	}
	if rec.operation != AuditOpWorkspaceCreate {
		t.Errorf("operation = %q, want %q", rec.operation, AuditOpWorkspaceCreate)
	}
	if rec.agentID != "admin-token" {
		t.Errorf("agent_id = %q — the audit row must name WHICH credential authenticated the create", rec.agentID)
	}
	if rec.sessionID != "req-abc123" {
		t.Errorf("session_id = %q, want the upstream correlation id", rec.sessionID)
	}
	if rec.details == nil || !strings.Contains(*rec.details, "client-site") {
		t.Errorf("details must name the created workspace, got %v", rec.details)
	}
	if rec.workspaceID == "" {
		t.Error("audit row must be anchored on the created workspace")
	}
}

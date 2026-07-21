package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/pbkdf2"
)

// ============================= helpers =====================================

// testAuditKey derives the same PBKDF2 key as getAuditHMACKey() using a fixed
// test salt, so we can generate expected HMACs in tests without relying on the
// module-level cached key (which may have been set by a previous test run).
// NOTE: iterations must stay in sync with auditPBKDF2Iterations in audit.go.
func testAuditKey(t *testing.T, salt string) []byte {
	t.Helper()
	return pbkdf2.Key(
		[]byte(salt),
		[]byte("molecule-audit-ledger-v1"),
		210_000,
		32,
		sha256.New,
	)
}

// makeAuditHMAC computes the canonical HMAC for an auditEventRow using key.
func makeAuditHMAC(t *testing.T, key []byte, ev *auditEventRow) string {
	t.Helper()
	canonical := map[string]interface{}{
		"agent_id":             ev.AgentID,
		"human_oversight_flag": ev.HumanOversightFlag,
		"id":                   ev.ID,
		"input_hash":           nilOrString(ev.InputHash),
		"model_used":           nilOrString(ev.ModelUsed),
		"operation":            ev.Operation,
		"output_hash":          nilOrString(ev.OutputHash),
		"prev_hmac":            nilOrString(ev.PrevHMAC),
		"risk_flag":            ev.RiskFlag,
		"session_id":           ev.SessionID,
		"timestamp":            ev.Timestamp.UTC().Format("2006-01-02T15:04:05Z"),
	}
	payload, _ := json.Marshal(canonical)
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// strPtr is a test helper to get a *string from a literal.
func strPtr(s string) *string { return &s }

// resetAuditKeyCache clears the cached HMAC key so tests can control it via env.
func resetAuditKeyCache() {
	auditKeyOnce = *new(sync.Once)
	auditHMACKey = nil
}

// ============================= computeAuditHMAC ============================

// TestComputeAuditHMAC_Deterministic verifies that two calls with identical
// fields return the same digest.
func TestComputeAuditHMAC_Deterministic(t *testing.T) {
	key := testAuditKey(t, "test-salt")
	ts := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	ev := &auditEventRow{
		ID:                 "evt-1",
		Timestamp:          ts,
		AgentID:            "agent-a",
		SessionID:          "sess-1",
		Operation:          "task_start",
		HumanOversightFlag: false,
		RiskFlag:           false,
	}
	h1 := computeAuditHMAC(key, ev)
	h2 := computeAuditHMAC(key, ev)
	if h1 != h2 {
		t.Fatalf("HMAC not deterministic: %s vs %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("expected 64-char hex, got len=%d", len(h1))
	}
}

// TestComputeAuditHMAC_FieldSensitivity verifies that changing any field changes
// the digest.
func TestComputeAuditHMAC_FieldSensitivity(t *testing.T) {
	key := testAuditKey(t, "test-salt")
	ts := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	base := &auditEventRow{
		ID: "evt-1", Timestamp: ts,
		AgentID: "a", SessionID: "s", Operation: "task_start",
	}
	baseH := computeAuditHMAC(key, base)

	cases := []struct {
		name string
		ev   auditEventRow
	}{
		{"agent_id", auditEventRow{ID: "evt-1", Timestamp: ts, AgentID: "b", SessionID: "s", Operation: "task_start"}},
		{"operation", auditEventRow{ID: "evt-1", Timestamp: ts, AgentID: "a", SessionID: "s", Operation: "task_end"}},
		{"risk_flag", auditEventRow{ID: "evt-1", Timestamp: ts, AgentID: "a", SessionID: "s", Operation: "task_start", RiskFlag: true}},
		{"prev_hmac", auditEventRow{ID: "evt-1", Timestamp: ts, AgentID: "a", SessionID: "s", Operation: "task_start", PrevHMAC: strPtr("abc")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := computeAuditHMAC(key, &tc.ev)
			if h == baseH {
				t.Errorf("expected different HMAC when %s changes", tc.name)
			}
		})
	}
}

// TestComputeAuditHMAC_TimestampStripsSubseconds verifies that microsecond-precision
// timestamps produce the same HMAC as their second-truncated versions.
func TestComputeAuditHMAC_TimestampStripsSubseconds(t *testing.T) {
	key := testAuditKey(t, "test-salt")
	ts1 := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	ts2 := time.Date(2026, 4, 17, 12, 0, 0, 999999000, time.UTC)
	ev1 := &auditEventRow{ID: "e", Timestamp: ts1, AgentID: "a", SessionID: "s", Operation: "o"}
	ev2 := &auditEventRow{ID: "e", Timestamp: ts2, AgentID: "a", SessionID: "s", Operation: "o"}
	if computeAuditHMAC(key, ev1) != computeAuditHMAC(key, ev2) {
		t.Error("subsecond precision should not affect HMAC")
	}
}

// ============================= missing-salt is LOUD, not silent ============

// TestGetAuditHMACKey_MissingSaltLogsLoudly is the security neg-control for the
// silent-no-audit hole: an unset AUDIT_LEDGER_SALT must NOT be a silent skip —
// it must emit a loud SECURITY/AUDIT config error. Proves the loud log fires
// (and, in the sibling sub-test, that a configured salt is silent).
func TestGetAuditHMACKey_MissingSaltLogsLoudly(t *testing.T) {
	t.Run("unset salt logs loudly and derives no key", func(t *testing.T) {
		resetAuditKeyCache()
		os.Unsetenv("AUDIT_LEDGER_SALT")
		defer resetAuditKeyCache()

		buf := captureLog(t)
		key := getAuditHMACKey()
		out := buf.String()

		if key != nil {
			t.Fatalf("expected nil key when salt unset, got %d bytes", len(key))
		}
		if !strings.Contains(out, "SECURITY/AUDIT") || !strings.Contains(out, "AUDIT_LEDGER_SALT is not set") {
			t.Fatalf("missing salt must be a LOUD config error, not a silent skip; log was: %q", out)
		}
		if !strings.Contains(out, "NOT tamper-evident") {
			t.Errorf("loud log should spell out the consequence (not tamper-evident); got: %q", out)
		}
	})

	t.Run("configured salt is silent and derives a key", func(t *testing.T) {
		resetAuditKeyCache()
		t.Setenv("AUDIT_LEDGER_SALT", "loud-log-negctl")
		defer resetAuditKeyCache()

		buf := captureLog(t)
		key := getAuditHMACKey()
		out := buf.String()

		if key == nil {
			t.Fatal("expected a derived key when salt is set")
		}
		if strings.Contains(out, "SECURITY/AUDIT") {
			t.Errorf("a configured salt must NOT emit the missing-salt error; got: %q", out)
		}
	})
}

// ============================= verifyAuditChain ============================

// TestVerifyAuditChain_NilKeyReturnsNil verifies that unset SALT → nil result
// (chain_valid reported as null).
func TestVerifyAuditChain_NilKeyReturnsNil(t *testing.T) {
	resetAuditKeyCache()
	t.Setenv("AUDIT_LEDGER_SALT", "") // empty string → salt absent
	defer resetAuditKeyCache()

	result := verifyAuditChain([]auditEventRow{})
	if result != nil {
		t.Errorf("expected nil when SALT unset, got %v", *result)
	}
}

// TestVerifyAuditChain_EmptySliceReturnsTrue verifies vacuous truth.
func TestVerifyAuditChain_EmptySliceReturnsTrue(t *testing.T) {
	// We need the key to be set for verifyAuditChain to proceed.
	// Reset and set env var so getAuditHMACKey() returns a key.
	resetAuditKeyCache()
	t.Setenv("AUDIT_LEDGER_SALT", "test-salt-empty")
	defer resetAuditKeyCache()

	result := verifyAuditChain([]auditEventRow{})
	if result == nil || !*result {
		t.Error("expected true for empty event slice")
	}
}

// TestVerifyAuditChain_ValidChain verifies a well-formed two-event chain.
func TestVerifyAuditChain_ValidChain(t *testing.T) {
	const testSalt = "test-salt-valid"
	resetAuditKeyCache()
	t.Setenv("AUDIT_LEDGER_SALT", testSalt)
	defer resetAuditKeyCache()

	key := testAuditKey(t, testSalt)
	ts := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)

	ev1 := auditEventRow{
		ID: "e1", Timestamp: ts, AgentID: "a", SessionID: "s",
		Operation: "task_start",
	}
	ev1.HMAC = makeAuditHMAC(t, key, &ev1)

	ev2 := auditEventRow{
		ID: "e2", Timestamp: ts.Add(time.Second), AgentID: "a", SessionID: "s",
		Operation: "task_end",
		PrevHMAC:  strPtr(ev1.HMAC),
	}
	ev2.HMAC = makeAuditHMAC(t, key, &ev2)

	result := verifyAuditChain([]auditEventRow{ev1, ev2})
	if result == nil || !*result {
		t.Error("expected valid chain")
	}
}

// TestVerifyAuditChain_TamperedHMACDetected verifies that a corrupted HMAC
// causes the chain to fail.
func TestVerifyAuditChain_TamperedHMACDetected(t *testing.T) {
	const testSalt = "test-salt-tamper"
	resetAuditKeyCache()
	t.Setenv("AUDIT_LEDGER_SALT", testSalt)
	defer resetAuditKeyCache()

	key := testAuditKey(t, testSalt)
	ts := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)

	ev := auditEventRow{
		ID: "e1", Timestamp: ts, AgentID: "a", SessionID: "s", Operation: "task_start",
	}
	ev.HMAC = makeAuditHMAC(t, key, &ev)
	// Corrupt the stored HMAC
	ev.HMAC = "deadbeef" + ev.HMAC[8:]

	result := verifyAuditChain([]auditEventRow{ev})
	if result == nil || *result {
		t.Error("expected invalid chain")
	}
}

// TestVerifyAuditChain_BrokenPrevHMACDetected verifies that a wrong prev_hmac
// link causes the chain to fail.
func TestVerifyAuditChain_BrokenPrevHMACDetected(t *testing.T) {
	const testSalt = "test-salt-broken"
	resetAuditKeyCache()
	t.Setenv("AUDIT_LEDGER_SALT", testSalt)
	defer resetAuditKeyCache()

	key := testAuditKey(t, testSalt)
	ts := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)

	ev1 := auditEventRow{
		ID: "e1", Timestamp: ts, AgentID: "a", SessionID: "s", Operation: "task_start",
	}
	ev1.HMAC = makeAuditHMAC(t, key, &ev1)

	wrong := "wrongprev" + strings.Repeat("0", 55)
	ev2 := auditEventRow{
		ID: "e2", Timestamp: ts.Add(time.Second), AgentID: "a", SessionID: "s",
		Operation: "task_end",
		PrevHMAC:  strPtr(wrong), // should be ev1.HMAC
	}
	ev2.HMAC = makeAuditHMAC(t, key, &ev2)

	result := verifyAuditChain([]auditEventRow{ev1, ev2})
	if result == nil || *result {
		t.Error("expected broken chain when prev_hmac is wrong")
	}
}

// ============================= AuditHandler.Query ==========================

// TestAuditQuery_Success verifies the happy path: rows returned + chain_valid.
func TestAuditQuery_Success(t *testing.T) {
	const testSalt = "test-salt-query"
	resetAuditKeyCache()
	t.Setenv("AUDIT_LEDGER_SALT", testSalt)
	defer resetAuditKeyCache()

	mock := setupTestDB(t)
	setupTestRedis(t)

	key := testAuditKey(t, testSalt)
	ts := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)

	ev := auditEventRow{
		ID: "e1", Timestamp: ts, AgentID: "agent-1", SessionID: "sess-1",
		Operation: "task_start", WorkspaceID: "ws-1",
	}
	ev.HMAC = makeAuditHMAC(t, key, &ev)

	// COUNT query
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_events`).
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	// SELECT query
	mock.ExpectQuery(`SELECT id, timestamp, agent_id`).
		WithArgs("ws-1", 100, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "timestamp", "agent_id", "session_id", "operation",
			"input_hash", "output_hash", "model_used",
			"human_oversight_flag", "risk_flag", "prev_hmac", "hmac", "workspace_id",
		}).AddRow(
			ev.ID, ev.Timestamp, ev.AgentID, ev.SessionID, ev.Operation,
			nil, nil, nil,
			ev.HumanOversightFlag, ev.RiskFlag, nil, ev.HMAC, ev.WorkspaceID,
		))

	h := NewAuditHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-1/audit", nil)

	h.Query(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp["total"] != float64(1) {
		t.Errorf("total = %v, want 1", resp["total"])
	}
	events, ok := resp["events"].([]interface{})
	if !ok || len(events) != 1 {
		t.Fatalf("expected 1 event, got %v", resp["events"])
	}
	event, ok := events[0].(map[string]interface{})
	if !ok {
		t.Fatalf("event should be an object, got %T", events[0])
	}
	if event["agent_id"] != "agent-1" || event["operation"] != "task_start" {
		t.Fatalf("event contract drifted: %v", event)
	}
	if _, exists := resp["entries"]; exists {
		t.Fatal("response must use events, not the unsupported entries envelope")
	}
	if _, exists := resp["cursor"]; exists {
		t.Fatal("response must use total/offset pagination, not cursor")
	}
	// chain_valid should be a bool (true — chain is intact)
	chainValid, ok := resp["chain_valid"].(bool)
	if !ok {
		t.Fatalf("chain_valid should be bool, got %T (%v)", resp["chain_valid"], resp["chain_valid"])
	}
	if !chainValid {
		t.Error("expected chain_valid=true for valid chain")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestAuditQuery_NoSaltReturnsNullChainValid verifies chain_valid is null when
// AUDIT_LEDGER_SALT is absent.
func TestAuditQuery_NoSaltReturnsNullChainValid(t *testing.T) {
	resetAuditKeyCache()
	os.Unsetenv("AUDIT_LEDGER_SALT")
	defer resetAuditKeyCache()

	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_events`).
		WithArgs("ws-2").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	mock.ExpectQuery(`SELECT id, timestamp, agent_id`).
		WithArgs("ws-2", 100, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "timestamp", "agent_id", "session_id", "operation",
			"input_hash", "output_hash", "model_used",
			"human_oversight_flag", "risk_flag", "prev_hmac", "hmac", "workspace_id",
		}))

	h := NewAuditHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-2"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-2/audit", nil)

	h.Query(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// chain_valid must be null (not false, not true) — JSON null decodes to nil in Go
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)

	if v, present := resp["chain_valid"]; present && v != nil {
		t.Errorf("chain_valid should be null when AUDIT_LEDGER_SALT unset, got %v", v)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestAuditQuery_EmptyEventsSerializesAsArray locks the JSON contract used by
// Canvas. Successful empty list responses must be [] rather than null.
func TestAuditQuery_EmptyEventsSerializesAsArray(t *testing.T) {
	resetAuditKeyCache()
	t.Setenv("AUDIT_LEDGER_SALT", "")
	defer resetAuditKeyCache()

	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_events`).
		WithArgs("ws-empty").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	mock.ExpectQuery(`SELECT id, timestamp, agent_id`).
		WithArgs("ws-empty", 100, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "timestamp", "agent_id", "session_id", "operation",
			"input_hash", "output_hash", "model_used",
			"human_oversight_flag", "risk_flag", "prev_hmac", "hmac", "workspace_id",
		}))

	h := NewAuditHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-empty"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-empty/audit", nil)

	h.Query(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Events json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if string(resp.Events) != "[]" {
		t.Fatalf("events = %s, want []", resp.Events)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestAuditQuery_FiltersByAgentID verifies the agent_id query param adds a WHERE clause.
func TestAuditQuery_FiltersByAgentID(t *testing.T) {
	resetAuditKeyCache()
	os.Unsetenv("AUDIT_LEDGER_SALT")
	defer resetAuditKeyCache()

	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_events`).
		WithArgs("ws-3", "agent-x").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	mock.ExpectQuery(`SELECT id, timestamp, agent_id`).
		WithArgs("ws-3", "agent-x", 100, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "timestamp", "agent_id", "session_id", "operation",
			"input_hash", "output_hash", "model_used",
			"human_oversight_flag", "risk_flag", "prev_hmac", "hmac", "workspace_id",
		}))

	h := NewAuditHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-3"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-3/audit?agent_id=agent-x", nil)

	h.Query(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestAuditQuery_InvalidFromParam verifies 400 for bad RFC3339 from param.
func TestAuditQuery_InvalidFromParam(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	h := NewAuditHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-4"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-4/audit?from=not-a-date", nil)

	h.Query(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad from param, got %d", w.Code)
	}
}

// TestAuditQuery_InvalidToParam verifies 400 for bad RFC3339 to param.
func TestAuditQuery_InvalidToParam(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	h := NewAuditHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-5"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-5/audit?to=bad", nil)

	h.Query(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad to param, got %d", w.Code)
	}
}

// TestAuditQuery_LimitCap verifies that limit > 500 is capped to 500.
func TestAuditQuery_LimitCap(t *testing.T) {
	resetAuditKeyCache()
	os.Unsetenv("AUDIT_LEDGER_SALT")
	defer resetAuditKeyCache()

	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_events`).
		WithArgs("ws-6").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	// Limit should be capped to 500
	mock.ExpectQuery(`SELECT id, timestamp, agent_id`).
		WithArgs("ws-6", 500, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "timestamp", "agent_id", "session_id", "operation",
			"input_hash", "output_hash", "model_used",
			"human_oversight_flag", "risk_flag", "prev_hmac", "hmac", "workspace_id",
		}))

	h := NewAuditHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-6"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-6/audit?limit=9999", nil)

	h.Query(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestAuditQuery_PaginatedOffsetReturnsNullChainValid verifies that when
// offset > 0 the handler cannot verify a partial chain and returns null.
func TestAuditQuery_PaginatedOffsetReturnsNullChainValid(t *testing.T) {
	const testSalt = "test-salt-paginated"
	resetAuditKeyCache()
	t.Setenv("AUDIT_LEDGER_SALT", testSalt)
	defer resetAuditKeyCache()

	mock := setupTestDB(t)
	setupTestRedis(t)

	key := testAuditKey(t, testSalt)
	ts := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)

	ev := auditEventRow{
		ID: "e1", Timestamp: ts, AgentID: "agent-1", SessionID: "sess-1",
		Operation: "task_start", WorkspaceID: "ws-7",
	}
	ev.HMAC = makeAuditHMAC(t, key, &ev)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_events`).
		WithArgs("ws-7").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(10))

	mock.ExpectQuery(`SELECT id, timestamp, agent_id`).
		WithArgs("ws-7", 100, 50).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "timestamp", "agent_id", "session_id", "operation",
			"input_hash", "output_hash", "model_used",
			"human_oversight_flag", "risk_flag", "prev_hmac", "hmac", "workspace_id",
		}).AddRow(
			ev.ID, ev.Timestamp, ev.AgentID, ev.SessionID, ev.Operation,
			nil, nil, nil,
			ev.HumanOversightFlag, ev.RiskFlag, nil, ev.HMAC, ev.WorkspaceID,
		))

	h := NewAuditHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-7"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-7/audit?offset=50", nil)

	h.Query(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)

	// chain_valid must be null when offset > 0 — partial view cannot verify chain
	if v, present := resp["chain_valid"]; present && v != nil {
		t.Errorf("chain_valid should be null for paginated response (offset>0), got %v", v)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestAuditQuery_SessionFilterReturnsNullChainValid verifies that an arbitrary
// session subset is not reported as tampered merely because earlier events for
// the same agent are outside the result set.
func TestAuditQuery_SessionFilterReturnsNullChainValid(t *testing.T) {
	const testSalt = "test-salt-session-filter"
	resetAuditKeyCache()
	t.Setenv("AUDIT_LEDGER_SALT", testSalt)
	defer resetAuditKeyCache()

	mock := setupTestDB(t)
	setupTestRedis(t)

	key := testAuditKey(t, testSalt)
	ts := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	ev := auditEventRow{
		ID: "e2", Timestamp: ts, AgentID: "agent-1", SessionID: "sess-filtered",
		Operation: "tool_call", PrevHMAC: strPtr("prior-event-hmac"), WorkspaceID: "ws-8",
	}
	ev.HMAC = makeAuditHMAC(t, key, &ev)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_events`).
		WithArgs("ws-8", "sess-filtered").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	mock.ExpectQuery(`SELECT id, timestamp, agent_id`).
		WithArgs("ws-8", "sess-filtered", 100, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "timestamp", "agent_id", "session_id", "operation",
			"input_hash", "output_hash", "model_used",
			"human_oversight_flag", "risk_flag", "prev_hmac", "hmac", "workspace_id",
		}).AddRow(
			ev.ID, ev.Timestamp, ev.AgentID, ev.SessionID, ev.Operation,
			nil, nil, nil,
			ev.HumanOversightFlag, ev.RiskFlag, *ev.PrevHMAC, ev.HMAC, ev.WorkspaceID,
		))

	h := NewAuditHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-8"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-8/audit?session_id=sess-filtered", nil)

	h.Query(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if v, present := resp["chain_valid"]; present && v != nil {
		t.Errorf("chain_valid should be null for a session-filtered subset, got %v", v)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestAuditQuery_FromFilterReturnsNullChainValid verifies that a time window
// which omits the beginning of a chain is reported as unverified, not valid.
func TestAuditQuery_FromFilterReturnsNullChainValid(t *testing.T) {
	resetAuditKeyCache()
	t.Setenv("AUDIT_LEDGER_SALT", "test-salt-from-filter")
	defer resetAuditKeyCache()

	mock := setupTestDB(t)
	setupTestRedis(t)
	from := time.Date(2026, 4, 17, 11, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_events`).
		WithArgs("ws-9", from).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	mock.ExpectQuery(`SELECT id, timestamp, agent_id`).
		WithArgs("ws-9", from, 100, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "timestamp", "agent_id", "session_id", "operation",
			"input_hash", "output_hash", "model_used",
			"human_oversight_flag", "risk_flag", "prev_hmac", "hmac", "workspace_id",
		}))

	h := NewAuditHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-9"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-9/audit?from=2026-04-17T11:00:00Z", nil)

	h.Query(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if v, present := resp["chain_valid"]; present && v != nil {
		t.Errorf("chain_valid should be null for a from-filtered subset, got %v", v)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// ===================== chain_verification field (split null) ===============

// TestAuditQuery_ChainVerification_Verified proves the field reads "verified"
// on an intact chain (positive control for the split-null field).
func TestAuditQuery_ChainVerification_Verified(t *testing.T) {
	const testSalt = "test-salt-cv-verified"
	resetAuditKeyCache()
	t.Setenv("AUDIT_LEDGER_SALT", testSalt)
	defer resetAuditKeyCache()

	mock := setupTestDB(t)
	setupTestRedis(t)

	key := testAuditKey(t, testSalt)
	ts := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	ev := auditEventRow{
		ID: "e1", Timestamp: ts, AgentID: "agent-1", SessionID: "sess-1",
		Operation: "task_start", WorkspaceID: "ws-cv1",
	}
	ev.HMAC = makeAuditHMAC(t, key, &ev)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_events`).
		WithArgs("ws-cv1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT id, timestamp, agent_id`).
		WithArgs("ws-cv1", 100, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "timestamp", "agent_id", "session_id", "operation",
			"input_hash", "output_hash", "model_used",
			"human_oversight_flag", "risk_flag", "prev_hmac", "hmac", "workspace_id",
		}).AddRow(
			ev.ID, ev.Timestamp, ev.AgentID, ev.SessionID, ev.Operation,
			nil, nil, nil,
			ev.HumanOversightFlag, ev.RiskFlag, nil, ev.HMAC, ev.WorkspaceID,
		))

	h := NewAuditHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-cv1"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-cv1/audit", nil)
	h.Query(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp["chain_verification"] != "verified" {
		t.Errorf("chain_verification = %v, want verified", resp["chain_verification"])
	}
	if cv, _ := resp["chain_valid"].(bool); !cv {
		t.Errorf("chain_valid = %v, want true", resp["chain_valid"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestAuditQuery_ChainVerification_Tampered is the tamper neg-control at the
// handler layer: a corrupted stored HMAC must fail closed to
// chain_valid=false / chain_verification="tampered".
func TestAuditQuery_ChainVerification_Tampered(t *testing.T) {
	const testSalt = "test-salt-cv-tamper"
	resetAuditKeyCache()
	t.Setenv("AUDIT_LEDGER_SALT", testSalt)
	defer resetAuditKeyCache()

	mock := setupTestDB(t)
	setupTestRedis(t)

	key := testAuditKey(t, testSalt)
	ts := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	ev := auditEventRow{
		ID: "e1", Timestamp: ts, AgentID: "agent-1", SessionID: "sess-1",
		Operation: "task_start", WorkspaceID: "ws-cv2",
	}
	ev.HMAC = makeAuditHMAC(t, key, &ev)
	ev.HMAC = "deadbeef" + ev.HMAC[8:] // tamper the stored HMAC

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_events`).
		WithArgs("ws-cv2").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT id, timestamp, agent_id`).
		WithArgs("ws-cv2", 100, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "timestamp", "agent_id", "session_id", "operation",
			"input_hash", "output_hash", "model_used",
			"human_oversight_flag", "risk_flag", "prev_hmac", "hmac", "workspace_id",
		}).AddRow(
			ev.ID, ev.Timestamp, ev.AgentID, ev.SessionID, ev.Operation,
			nil, nil, nil,
			ev.HumanOversightFlag, ev.RiskFlag, nil, ev.HMAC, ev.WorkspaceID,
		))

	h := NewAuditHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-cv2"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-cv2/audit", nil)
	h.Query(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp["chain_verification"] != "tampered" {
		t.Errorf("chain_verification = %v, want tampered", resp["chain_verification"])
	}
	cv, ok := resp["chain_valid"].(bool)
	if !ok || cv {
		t.Errorf("chain_valid = %v, want false (fail-closed on tamper)", resp["chain_valid"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestAuditQuery_ChainVerification_DisabledNoSalt proves the split-null: an
// unset salt is reported as "disabled_no_salt" (a misconfiguration), NOT the
// benign "unavailable_partial_query" — so a missing salt is not a silent
// no-audit even at the API layer. chain_valid stays null (contract-preserving).
func TestAuditQuery_ChainVerification_DisabledNoSalt(t *testing.T) {
	resetAuditKeyCache()
	os.Unsetenv("AUDIT_LEDGER_SALT")
	defer resetAuditKeyCache()

	mock := setupTestDB(t)
	setupTestRedis(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_events`).
		WithArgs("ws-cv3").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT id, timestamp, agent_id`).
		WithArgs("ws-cv3", 100, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "timestamp", "agent_id", "session_id", "operation",
			"input_hash", "output_hash", "model_used",
			"human_oversight_flag", "risk_flag", "prev_hmac", "hmac", "workspace_id",
		}))

	h := NewAuditHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-cv3"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-cv3/audit", nil)
	h.Query(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp["chain_verification"] != "disabled_no_salt" {
		t.Errorf("chain_verification = %v, want disabled_no_salt", resp["chain_verification"])
	}
	if v, present := resp["chain_valid"]; present && v != nil {
		t.Errorf("chain_valid should be null when salt unset, got %v", v)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestAuditQuery_ChainVerification_PartialQuery proves the OTHER null case is
// still distinguished as benign: salt IS set, but offset>0 could omit chain
// predecessors, so the field is "unavailable_partial_query" (NOT
// disabled_no_salt, NOT tampered).
func TestAuditQuery_ChainVerification_PartialQuery(t *testing.T) {
	const testSalt = "test-salt-cv-partial"
	resetAuditKeyCache()
	t.Setenv("AUDIT_LEDGER_SALT", testSalt)
	defer resetAuditKeyCache()

	mock := setupTestDB(t)
	setupTestRedis(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM audit_events`).
		WithArgs("ws-cv4").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(10))
	mock.ExpectQuery(`SELECT id, timestamp, agent_id`).
		WithArgs("ws-cv4", 100, 50).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "timestamp", "agent_id", "session_id", "operation",
			"input_hash", "output_hash", "model_used",
			"human_oversight_flag", "risk_flag", "prev_hmac", "hmac", "workspace_id",
		}))

	h := NewAuditHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-cv4"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-cv4/audit?offset=50", nil)
	h.Query(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp["chain_verification"] != "unavailable_partial_query" {
		t.Errorf("chain_verification = %v, want unavailable_partial_query", resp["chain_verification"])
	}
	if v, present := resp["chain_valid"]; present && v != nil {
		t.Errorf("chain_valid should be null for paginated response, got %v", v)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestRestartContextArbitration_FirstBootSkips pins RFC concierge rule 2: a
// genuine first boot (has_greeted marker UNSET) must NEVER take the
// restart-context path. The arbitration decision returns false AND
// fireRestartContextIfBooted must not claim the per-restart concurrency gate
// (no sender goroutine spawned) — the first-boot greeting owns that edge.
func TestRestartContextArbitration_FirstBootSkips(t *testing.T) {
	mock := setupTestDB(t)
	const wsID = "ws-firstboot-skip"

	mock.ExpectQuery(`SELECT has_greeted FROM workspaces`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"has_greeted"}).AddRow(false))

	if shouldFireRestartContext(context.Background(), wsID) {
		t.Fatalf("genuine first boot must NOT fire restart-context")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations: %v", err)
	}

	// The full arbitration wrapper must also leave the concurrency gate clear.
	mock.ExpectQuery(`SELECT has_greeted FROM workspaces`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"has_greeted"}).AddRow(false))
	h := &WorkspaceHandler{}
	h.fireRestartContextIfBooted(context.Background(), wsID, restartContextData{RestartAt: time.Now()})
	if restartContextInFlight(wsID) {
		t.Fatalf("first boot must not set the restart-context pending gate")
	}
	h.asyncWG.Wait() // no goroutine should have been spawned
}

// TestRestartContextArbitration_MarkerReadErrorFailsClosed pins the fail-closed
// invariant: on an unreadable has_greeted marker the decision is SKIP — a first
// boot must never slip onto the restart-context path on a transient DB error.
func TestRestartContextArbitration_MarkerReadErrorFailsClosed(t *testing.T) {
	mock := setupTestDB(t)
	const wsID = "ws-marker-err"
	mock.ExpectQuery(`SELECT has_greeted FROM workspaces`).
		WithArgs(wsID).
		WillReturnError(errDBDown)

	if shouldFireRestartContext(context.Background(), wsID) {
		t.Fatalf("marker-read error must fail CLOSED (skip restart-context)")
	}
}

// TestRestartContextArbitration_RealRestartFires pins the other side: a REAL
// restart (has_greeted marker SET) fires restart-context.
func TestRestartContextArbitration_RealRestartFires(t *testing.T) {
	mock := setupTestDB(t)
	const wsID = "ws-real-restart"
	mock.ExpectQuery(`SELECT has_greeted FROM workspaces`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"has_greeted"}).AddRow(true))

	if !shouldFireRestartContext(context.Background(), wsID) {
		t.Fatalf("a real restart (marker set) must fire restart-context")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations: %v", err)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{-5 * time.Second, "0s"},
		{45 * time.Second, "45s"},
		{3 * time.Minute, "3m0s"},
		{2*time.Hour + 14*time.Minute, "2h14m"},
		{25 * time.Hour, "25h0m"},
	}
	for _, c := range cases {
		got := humanDuration(c.in)
		if got != c.want {
			t.Errorf("humanDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildRestartContextMessage_MissingPriorHeartbeat_DefensiveRender(t *testing.T) {
	// RFC concierge rule 2: a genuine first boot NO LONGER reaches this builder
	// — restart-context is arbitrated (fireRestartContextIfBooted) to fire only
	// for an already-greeted workspace, and a greeted box was online so it
	// carries a last_heartbeat_at. The zero-PrevSessionAt placeholder is now
	// purely a DEFENSIVE fallback for the pathological marker-set-but-heartbeat-
	// unreadable case; this test pins that it still renders a consistent shape
	// (it is no longer the "fresh box / first boot" scenario it once asserted).
	d := restartContextData{
		RestartAt:     time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC),
		PrevSessionAt: time.Time{},
		EnvKeys:       nil,
	}
	msg := buildRestartContextMessage(d)

	mustContain(t, msg, "=== WORKSPACE RESTART CONTEXT ===")
	mustContain(t, msg, "Restart at: 2026-04-13T12:00:00Z")
	mustContain(t, msg, "Previous session ended: (no prior session on record)")
	mustContain(t, msg, "Env vars now available: (none)")
	mustContain(t, msg, "=== END RESTART CONTEXT ===")
}

func TestBuildRestartContextMessage_WithDataRendersKeysAndDelta(t *testing.T) {
	prev := time.Date(2026, 4, 13, 10, 0, 0, 0, time.UTC)
	d := restartContextData{
		RestartAt:     prev.Add(2*time.Hour + 14*time.Minute),
		PrevSessionAt: prev,
		EnvKeys:       []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"},
	}
	msg := buildRestartContextMessage(d)

	mustContain(t, msg, "Previous session ended: 2026-04-13T10:00:00Z (2h14m ago)")
	mustContain(t, msg, "Env vars now available: ANTHROPIC_API_KEY, OPENAI_API_KEY")
	if strings.Contains(msg, "sk-") || strings.Contains(msg, "secret-value") {
		t.Errorf("rendered message leaked secret-shaped content: %q", msg)
	}
}

func TestBuildRestartA2APayload_ShapeIsJSONRPCMessageSend(t *testing.T) {
	body, err := buildRestartA2APayload("ws-test", "hello world")
	if err != nil {
		t.Fatalf("buildRestartA2APayload: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}

	if parsed["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", parsed["jsonrpc"])
	}
	if parsed["method"] != "message/send" {
		t.Errorf("method = %v, want message/send", parsed["method"])
	}

	params, ok := parsed["params"].(map[string]any)
	if !ok {
		t.Fatalf("params missing or wrong type: %v", parsed["params"])
	}
	message, ok := params["message"].(map[string]any)
	if !ok {
		t.Fatalf("params.message missing: %v", params)
	}
	if message["role"] != "user" {
		t.Errorf("role = %v, want user", message["role"])
	}
	if message["messageId"] == nil || message["messageId"] == "" {
		t.Errorf("messageId missing")
	}

	meta, ok := message["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata missing: %v", message)
	}
	if meta["kind"] != "restart_context" {
		t.Errorf("metadata.kind = %v, want restart_context", meta["kind"])
	}
	if meta["source"] != "platform" {
		t.Errorf("metadata.source = %v, want platform", meta["source"])
	}

	parts, ok := message["parts"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("parts wrong shape: %v", message["parts"])
	}
	firstPart, _ := parts[0].(map[string]any)
	if firstPart["text"] != "hello world" {
		t.Errorf("parts[0].text = %v, want hello world", firstPart["text"])
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected output to contain %q, got:\n%s", needle, haystack)
	}
}

// ==================== at most one pending restart-context snapshot (core#5241) ====================

// The two statements that keep one snapshot pending per workspace, pinned with
// the JSON path they search: buildRestartA2APayload must write source_type
// exactly there (TestBuildRestartA2APayload_SourceTypeIsWhereTheQueueSearches).
const (
	dropQueuedRestartContextSQL = `UPDATE a2a_queue\s+SET status = 'dropped',\s+last_error = 'superseded: a newer restart-context snapshot replaced it'\s+` +
		`WHERE workspace_id = \$1\s+AND status = 'queued'\s+AND body #>> '\{params,message,metadata,source_type\}' = \$2`
	restartContextDeliveringSQL = `SELECT EXISTS \(\s+SELECT 1 FROM a2a_queue\s+WHERE workspace_id = \$1\s+AND status = 'dispatched'\s+` +
		`AND body #>> '\{params,message,metadata,source_type\}' = \$2`
)

func TestBuildRestartA2APayload_SourceTypeIsWhereTheQueueSearches(t *testing.T) {
	body, err := buildRestartA2APayload("ws-test", "hello", "restart-context:ws-test")
	if err != nil {
		t.Fatalf("buildRestartA2APayload: %v", err)
	}
	var parsed struct {
		Params struct {
			Message struct {
				Metadata map[string]any `json:"metadata"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if got := parsed.Params.Message.Metadata["source_type"]; got != restartContextSourceType {
		t.Fatalf("params.message.metadata.source_type = %v, want %q — the queue finds pending snapshots by it", got, restartContextSourceType)
	}
}

// A snapshot an earlier restart left queued is replaced, not joined: the stale
// one is dropped and the new one queued in its place.
func TestEnqueueRestartContext_ReplacesTheQueuedSnapshot(t *testing.T) {
	mock := setupTestDB(t)
	const wsID = "ws-rc-replace"
	wantKey := "restart-context-" + wsID

	mock.ExpectExec(dropQueuedRestartContextSQL).
		WithArgs(wsID, restartContextSourceType).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(restartContextDeliveringSQL).
		WithArgs(wsID, restartContextSourceType).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec("UPDATE a2a_queue").
		WithArgs(wsID, wantKey).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("INSERT INTO a2a_queue").
		WithArgs(wsID, nil, 90, sqlmock.AnyArg(), "message/send", wantKey, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("q-new"))
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	h := &WorkspaceHandler{}
	h.enqueueRestartContext(context.Background(), wsID, restartContextData{RestartAt: time.Now()}, "unit test")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the queued snapshot was not replaced by the new one: %v", err)
	}
}

// While the drain is handing a snapshot over, a new one is not queued next to
// it — that would be the second pending snapshot.
func TestEnqueueRestartContext_SkipsWhileASnapshotIsBeingDelivered(t *testing.T) {
	mock := setupTestDB(t)
	logs := captureLog(t)
	const wsID = "ws-rc-in-flight"

	mock.ExpectExec(dropQueuedRestartContextSQL).
		WithArgs(wsID, restartContextSourceType).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(restartContextDeliveringSQL).
		WithArgs(wsID, restartContextSourceType).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	delivered := 0
	h := &WorkspaceHandler{wakeMarkDelivered: func(context.Context, string, string) error {
		delivered++
		return nil
	}}
	h.enqueueRestartContext(context.Background(), wsID, restartContextData{RestartAt: time.Now()}, "unit test", "restart-context:"+wsID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
	if out := logs.String(); !strings.Contains(out, "not queueing a second") || strings.Contains(out, "queued for heartbeat drain") {
		t.Fatalf("a second snapshot was queued next to the one being delivered; log:\n%s", out)
	}
	if delivered != 0 {
		t.Fatalf("the wake was marked delivered %d time(s) for a snapshot that was never queued", delivered)
	}
}

// A snapshot pushed straight to the agent supersedes one an earlier restart
// left queued; otherwise the drain would deliver the stale one afterwards.
func TestSendRestartContext_DropsTheQueuedSnapshotBeforePushing(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	const wsID = "ws-rc-push"
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	waitForHandlerAsyncBeforeDBCleanup(t, h)

	mock.ExpectQuery(`SELECT status FROM workspaces WHERE id =`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("online"))
	mock.ExpectQuery(`SELECT url, last_heartbeat_at FROM workspaces WHERE id =`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"url", "last_heartbeat_at"}).AddRow("http://ws-rc-push.example:8000", time.Now()))
	mock.ExpectExec(dropQueuedRestartContextSQL).
		WithArgs(wsID, restartContextSourceType).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The push itself is left unmocked: it fails fast, and the drop above is
	// what this test is about.

	h.sendRestartContext(wsID, restartContextData{RestartAt: time.Now().Add(-time.Minute)})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the stale queued snapshot was not dropped before the push: %v", err)
	}
}

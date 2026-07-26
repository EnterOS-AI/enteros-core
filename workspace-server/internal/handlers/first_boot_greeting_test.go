package handlers

// first_boot_greeting_test.go — pins the first-boot greeting
// (first_boot_greeting.go): a REAL agent turn greets in persona, the static
// fallback covers a failed turn, and the greet-once gate stops everything
// when chat history exists.

import (
	"context"
	"strings"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"github.com/DATA-DOG/go-sqlmock"
)

// stubTurn returns an a2aTurnFn that records its invocation and returns the
// given JSON-RPC response body.
func stubTurn(t *testing.T, calls *[]string, status int, respBody string, retErr error) a2aTurnFn {
	t.Helper()
	return func(_ context.Context, workspaceID string, body []byte, callerID string, logActivity bool) (int, []byte, error) {
		*calls = append(*calls, workspaceID)
		if logActivity {
			t.Errorf("greet turn must use logActivity=false (writer is the single chat entry point)")
		}
		if callerID != "system:first-boot-greeting" {
			t.Errorf("greet turn callerID = %q", callerID)
		}
		if !strings.Contains(string(body), "first_boot_greeting") {
			t.Errorf("greet payload missing first_boot_greeting metadata: %s", body)
		}
		return status, []byte(respBody), retErr
	}
}

// expectNotGreeted pins the greet-once gate reading the has_greeted boot marker
// (RFC concierge rule 2 SSOT) and finding it UNSET — a fresh box that must
// greet. Replaces the old derived activity_logs user-chat EXISTS query.
func expectNotGreeted(mock sqlmock.Sqlmock, wsID string) {
	mock.ExpectQuery(`SELECT has_greeted FROM workspaces`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"has_greeted"}).AddRow(false))
}

func expectWriterSend(mock sqlmock.Sqlmock, wsID, name string) {
	mock.ExpectQuery("SELECT name, talk_to_user_enabled FROM workspaces").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "talk_to_user_enabled"}).AddRow(name, true))
	mock.ExpectExec(`INSERT INTO activity_logs.*'a2a_receive'.*'notify'`).
		WithArgs(wsID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
}

// expectMarkGreeted pins the commit-on-delivery marker write that follows a
// successful AgentMessageWriter.Send — has_greeted flips true ONLY after the
// user has seen the greeting.
func expectMarkGreeted(mock sqlmock.Sqlmock, wsID string) {
	mock.ExpectExec(`UPDATE workspaces SET has_greeted = true`).
		WithArgs(wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func sentMessage(t *testing.T, emitter *capturingEmitter) string {
	t.Helper()
	if len(emitter.events) != 1 {
		t.Fatalf("expected 1 broadcast, got %d: %#v", len(emitter.events), emitter.events)
	}
	ev := emitter.events[0]
	if ev.eventType != string(events.EventAgentMessage) {
		t.Fatalf("event type = %q, want AGENT_MESSAGE", ev.eventType)
	}
	payload, _ := ev.payload.(map[string]interface{})
	msg, _ := payload["message"].(string)
	return msg
}

func TestFirstBootGreeting_UsesInCharacterAgentReply(t *testing.T) {
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	var calls []string
	greet := FirstBootGreeter(
		NewAgentMessageWriter(db.DB, emitter),
		stubTurn(t, &calls, 200,
			`{"jsonrpc":"2.0","result":{"message":{"parts":[{"kind":"text","text":"Hey — I'm Scout, your research agent. Ask me to track a topic!"}]}}}`,
			nil),
	)

	expectNotGreeted(mock, "ws-first")
	expectWriterSend(mock, "ws-first", "Scout")
	expectMarkGreeted(mock, "ws-first")

	greet("ws-first", 0)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected exactly one agent turn, got %d", len(calls))
	}
	msg := sentMessage(t, emitter)
	if !strings.Contains(msg, "I'm Scout") {
		t.Errorf("greeting should be the agent's own reply, got %q", msg)
	}
	if strings.Contains(msg, "What are we building?") {
		t.Errorf("in-character reply must not be replaced by the fallback: %q", msg)
	}
}

func TestFirstBootGreeting_FallsBackWhenTurnFails(t *testing.T) {
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	var calls []string
	greet := FirstBootGreeter(
		NewAgentMessageWriter(db.DB, emitter),
		stubTurn(t, &calls, 502, `bad gateway`, nil),
	)

	expectNotGreeted(mock, "ws-fb")
	expectWriterSend(mock, "ws-fb", "Enter OS Agent")
	expectMarkGreeted(mock, "ws-fb")

	greet("ws-fb", 45)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations: %v", err)
	}
	msg := sentMessage(t, emitter)
	// The concierge fallback: friendly, mentions the real tool count, and
	// guides with concrete example asks.
	for _, want := range []string{"Org Concierge", "45 management tools", "What are we building?", "Create a research agent"} {
		if !strings.Contains(msg, want) {
			t.Errorf("fallback greeting missing %q: %q", want, msg)
		}
	}
}

func TestFirstBootGreeting_FallsBackOnErrorReply(t *testing.T) {
	// An A2A-level error reply ("[error] …" from extractA2AText) is not a
	// greeting — fall back rather than show the user an error string.
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	var calls []string
	greet := FirstBootGreeter(
		NewAgentMessageWriter(db.DB, emitter),
		stubTurn(t, &calls, 200, `{"jsonrpc":"2.0","error":{"message":"boom"}}`, nil),
	)

	expectNotGreeted(mock, "ws-err-reply")
	expectWriterSend(mock, "ws-err-reply", "Agent")
	expectMarkGreeted(mock, "ws-err-reply")

	greet("ws-err-reply", 0)

	msg := sentMessage(t, emitter)
	if strings.Contains(msg, "boom") {
		t.Errorf("error reply leaked into the greeting: %q", msg)
	}
	if !strings.Contains(msg, "online and ready") {
		t.Errorf("expected role-agnostic fallback, got %q", msg)
	}
}

func TestFirstBootGreeting_QueuedGreetSendsNothingSynchronously(t *testing.T) {
	// Both queued acks — the poll-mode {"status":"queued"} short-circuit AND
	// the busy-target {"queued":true} enqueue — mean the proxy ACCEPTED the
	// greet turn but has NOT answered it. The synchronous greeter must send
	// NOTHING: not the raw envelope, and (RFC rule 1) not the premature static
	// fallback either. The agent's real reply arrives later — via the queue
	// drain (attachQueuedTurnCompletion) for a busy target, or the agent's own
	// /notify for a poll-mode target — so relaying anything now would
	// double-greet. Pins that the unified QueuedA2AResponse matcher recognizes
	// both shapes.
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"poll-mode short-circuit", 200, `{"status":"queued","delivery_mode":"poll","method":"message/send"}`},
		{"busy-target enqueue", 202, `{"queued":true,"queue_id":"q-1","queue_depth":1,"message":"workspace agent busy — request queued"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			emitter := &capturingEmitter{}
			var calls []string
			greet := FirstBootGreeter(
				NewAgentMessageWriter(db.DB, emitter),
				stubTurn(t, &calls, tc.status, tc.body, nil),
			)

			expectNotGreeted(mock, "ws-queued")

			greet("ws-queued", 0)

			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("DB expectations: %v", err)
			}
			if len(emitter.events) != 0 {
				t.Fatalf("queued greet must send nothing synchronously, got %#v", emitter.events)
			}
		})
	}
}

func TestFirstBootGreeting_BusyQueuedGreet_DrainDeliversRealReply(t *testing.T) {
	// RFC concierge rule 1, end to end: a BUSY-QUEUED greet turn must NOT emit
	// the static fallback synchronously, and the agent's REAL reply — produced
	// later and drained from a2a_queue — must be delivered via the
	// AgentMessageWriter SSOT. The two halves collapse to exactly ONE greeting
	// (dedup-safe).
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	const wsID = "ws-busy-greet"

	// Half 1 — greet turn is busy-queued. Only the history check hits the DB;
	// no writer Send, because the fallback must not fire on a queued ack.
	var calls []string
	greet := FirstBootGreeter(
		NewAgentMessageWriter(db.DB, emitter),
		stubTurn(t, &calls, 202,
			`{"queued":true,"queue_id":"q-1","queue_depth":1,"message":"workspace agent busy — request queued"}`,
			nil),
	)
	expectNotGreeted(mock, wsID)
	greet(wsID, 0)
	if len(emitter.events) != 0 {
		t.Fatalf("busy-queued greet must send nothing synchronously, got %#v", emitter.events)
	}

	// Half 2 — the queue drain dispatched the greet turn, got the agent's real
	// in-character reply, and hands it to attachQueuedTurnCompletion. The
	// self-first-boot-greet exception must DELIVER it (not skip it as an
	// internal self-message) AND commit the has_greeted marker on delivery.
	reqBody, err := buildFirstBootGreetPayload(wsID, "wake-busy-greet")
	if err != nil {
		t.Fatalf("buildFirstBootGreetPayload: %v", err)
	}
	realReply := `{"jsonrpc":"2.0","result":{"message":{"parts":[{"kind":"text","text":"Hey — I'm Scout, your research agent. Ask me to track a topic!"}]}}}`
	h := &WorkspaceHandler{broadcaster: emitter}
	expectWriterSend(mock, wsID, "Scout")
	expectMarkGreeted(mock, wsID)
	h.attachQueuedTurnCompletion(context.Background(), wsID, false, reqBody, []byte(realReply))
	h.asyncWG.Wait()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations: %v", err)
	}
	msg := sentMessage(t, emitter) // exactly ONE broadcast total
	if !strings.Contains(msg, "I'm Scout") {
		t.Errorf("drained greeting should be the agent's real reply, got %q", msg)
	}
	if strings.Contains(msg, "online and ready") || strings.Contains(msg, "What are we building?") {
		t.Errorf("real drained reply must not be replaced by the fallback: %q", msg)
	}
}

func TestFirstBootGreeting_DrainedGreet_EmptyReplyFallsBack(t *testing.T) {
	// The static fallback IS still the last resort on the drain-deliver path:
	// if the drained greet reply carries no usable prose (LLM error / unknown
	// shape), deliver the role-agnostic static greeting so the fresh chat is
	// never silent.
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	const wsID = "ws-drain-empty"
	reqBody, err := buildFirstBootGreetPayload(wsID, "wake-drain-empty")
	if err != nil {
		t.Fatalf("buildFirstBootGreetPayload: %v", err)
	}
	h := &WorkspaceHandler{broadcaster: emitter}
	expectWriterSend(mock, wsID, "Agent")
	expectMarkGreeted(mock, wsID)
	// An A2A-level error reply is not usable greeting prose.
	h.attachQueuedTurnCompletion(context.Background(), wsID, false,
		reqBody, []byte(`{"jsonrpc":"2.0","error":{"message":"boom"}}`))
	h.asyncWG.Wait()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations: %v", err)
	}
	msg := sentMessage(t, emitter)
	if strings.Contains(msg, "boom") {
		t.Errorf("error reply leaked into the greeting: %q", msg)
	}
	if !strings.Contains(msg, "online and ready") {
		t.Errorf("expected role-agnostic fallback, got %q", msg)
	}
}

func TestAttachQueuedTurnCompletion_NonGreetSelfSourceStillSkipped(t *testing.T) {
	// The first-boot exception must stay NARROW: every OTHER self-source turn
	// (restart-context wake, harvester nudge) is an internal message the user
	// must never see, so it stays skipped — no broadcast, no DB write.
	setupTestDB(t) // no expectations => any DB call fails the test
	emitter := &capturingEmitter{}
	h := &WorkspaceHandler{broadcaster: emitter}
	reqBody := []byte(`{"jsonrpc":"2.0","method":"message/send","params":{"metadata":{"source_type":"self-restart-context"},"message":{"messageId":"m1","parts":[{"kind":"text","text":"=== WORKSPACE RESTART CONTEXT ==="}]}}}`)
	realReply := `{"jsonrpc":"2.0","result":{"message":{"parts":[{"kind":"text","text":"context noted"}]}}}`
	h.attachQueuedTurnCompletion(context.Background(), "ws-rc", false, reqBody, []byte(realReply))
	h.asyncWG.Wait()
	if len(emitter.events) != 0 {
		t.Fatalf("non-greet self-source turn must not deliver, got %#v", emitter.events)
	}
}

func TestFirstBootGreeting_JSONShapedReplyFallsBack(t *testing.T) {
	// extractA2AText echoes the raw body for unknown shapes — a JSON
	// envelope must never become the user's first chat bubble.
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	var calls []string
	greet := FirstBootGreeter(
		NewAgentMessageWriter(db.DB, emitter),
		stubTurn(t, &calls, 200, `{"unexpected":"shape"}`, nil),
	)

	expectNotGreeted(mock, "ws-json")
	expectWriterSend(mock, "ws-json", "Agent")
	expectMarkGreeted(mock, "ws-json")

	greet("ws-json", 0)

	msg := sentMessage(t, emitter)
	if strings.Contains(msg, "unexpected") {
		t.Errorf("raw JSON leaked into the greeting: %q", msg)
	}
	if !strings.Contains(msg, "online and ready") {
		t.Errorf("expected fallback, got %q", msg)
	}
}

func TestFirstBootGreeting_ConcurrentInvocationsGreetOnce(t *testing.T) {
	// The greet-once history gate is check-then-act with a window as wide as
	// the agent turn — the pending guard must make overlapping invocations
	// (register retry racing the verified flip) collapse to ONE greeting.
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	turnStarted := make(chan struct{})
	turnRelease := make(chan struct{})
	var turns int
	greet := FirstBootGreeter(
		NewAgentMessageWriter(db.DB, emitter),
		func(_ context.Context, _ string, _ []byte, _ string, _ bool) (int, []byte, error) {
			turns++
			close(turnStarted)
			<-turnRelease
			return 200, []byte(`{"jsonrpc":"2.0","result":{"message":{"parts":[{"kind":"text","text":"Hello from me"}]}}}`), nil
		},
	)

	// Exactly ONE marker check + ONE send + ONE marker write may hit the DB.
	expectNotGreeted(mock, "ws-race")
	expectWriterSend(mock, "ws-race", "Agent")
	expectMarkGreeted(mock, "ws-race")

	done := make(chan struct{})
	go func() {
		greet("ws-race", 0)
		close(done)
	}()
	<-turnStarted
	// Second invocation while the first is mid-turn: must no-op instantly.
	greet("ws-race", 0)
	close(turnRelease)
	<-done

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations (exactly one greet): %v", err)
	}
	if turns != 1 {
		t.Errorf("agent turn ran %d times, want 1", turns)
	}
	if len(emitter.events) != 1 {
		t.Errorf("expected exactly one greeting broadcast, got %d", len(emitter.events))
	}
}

func TestFirstBootGreeting_SkipsWhenAlreadyGreeted(t *testing.T) {
	// RFC concierge rule 2: the greet-once gate now reads the has_greeted boot
	// marker (SSOT), NOT a derived user-chat query. Marker SET (a greeted box
	// being reconnected/restarted) stops everything: no agent turn, no lookup,
	// no INSERT, no broadcast — the double-greet hole (greeted-but-user-silent)
	// is closed because the gate no longer keys on whether the USER chatted.
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	var calls []string
	greet := FirstBootGreeter(
		NewAgentMessageWriter(db.DB, emitter),
		stubTurn(t, &calls, 200, `{}`, nil),
	)

	mock.ExpectQuery(`SELECT has_greeted FROM workspaces`).
		WithArgs("ws-restart").
		WillReturnRows(sqlmock.NewRows([]string{"has_greeted"}).AddRow(true))

	greet("ws-restart", 45)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("expected no agent turn on already-greeted box, got %d", len(calls))
	}
	if len(emitter.events) != 0 {
		t.Fatalf("expected no broadcast on already-greeted box, got %#v", emitter.events)
	}
}

func TestFirstBootGreeting_SkipsOnMarkerCheckError(t *testing.T) {
	// Fail CLOSED: an unreadable has_greeted marker must not risk a duplicate
	// greeting.
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	var calls []string
	greet := FirstBootGreeter(
		NewAgentMessageWriter(db.DB, emitter),
		stubTurn(t, &calls, 200, `{}`, nil),
	)

	mock.ExpectQuery(`SELECT has_greeted FROM workspaces`).
		WithArgs("ws-err").
		WillReturnError(errDBDown)

	greet("ws-err", 3)

	if len(calls) != 0 || len(emitter.events) != 0 {
		t.Fatalf("expected nothing on marker-check error, got calls=%d events=%#v", len(calls), emitter.events)
	}
}

func TestFirstBootGreeting_DeliveryCommitsMarker_NoReGreet(t *testing.T) {
	// RFC concierge rule 2, the double-greet close: commit-on-delivery sets the
	// has_greeted marker AFTER Send, so a later reconcile/restart greet reads the
	// marker SET and skips. This is the exact hole the old derived user-chat
	// query left open (greeted-but-user-silent re-greeted). Sequenced on ONE mock:
	// boot 1 greets (gate unset → deliver → mark), boot 2 skips (gate set).
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	const wsID = "ws-commit-marker"
	var calls []string
	greet := FirstBootGreeter(
		NewAgentMessageWriter(db.DB, emitter),
		stubTurn(t, &calls, 200,
			`{"jsonrpc":"2.0","result":{"message":{"parts":[{"kind":"text","text":"Hi, I'm Ada."}]}}}`,
			nil),
	)

	// Boot 1 — fresh box: gate unset → agent turn → deliver → COMMIT marker.
	expectNotGreeted(mock, wsID)
	expectWriterSend(mock, wsID, "Ada")
	expectMarkGreeted(mock, wsID)
	greet(wsID, 0)

	// Boot 2 — reconcile restart: gate now reads SET → skip entirely (no turn,
	// no send, no second broadcast). Proves delivery-committed marker prevents
	// the reconcile double-greet.
	mock.ExpectQuery(`SELECT has_greeted FROM workspaces`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"has_greeted"}).AddRow(true))
	greet(wsID, 0)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations: %v", err)
	}
	if len(calls) != 1 {
		t.Errorf("agent turn ran %d times, want 1 (second boot must skip)", len(calls))
	}
	if len(emitter.events) != 1 {
		t.Errorf("expected exactly ONE greeting broadcast across two boots, got %d", len(emitter.events))
	}
}

func TestFirstBootGreeting_WakeKeyDedupsRetriedDelivery(t *testing.T) {
	// RFC concierge rule 2, idempotency: the wake key
	// (params.metadata.wake_idempotency_key, minted at decision time) dedups a
	// RETRIED delivery of the SAME wake — the sync path racing its own drain, a
	// double drain, or a replayed queue row. Two deliverFirstBootGreeting calls
	// with the same key collapse to exactly ONE Send + ONE marker commit.
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	const wsID = "ws-wake-dedup"
	const wakeKey = "wake-dedup-test-1"
	writer := NewAgentMessageWriter(db.DB, emitter)

	// Only the FIRST delivery may hit the DB (lookup + insert + marker write).
	expectWriterSend(mock, wsID, "Agent")
	expectMarkGreeted(mock, wsID)

	if err := deliverFirstBootGreeting(context.Background(), writer, wsID, "Hello from the agent.", wakeKey); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	// Retry with the SAME wake key: must dedup — no Send, no marker write.
	if err := deliverFirstBootGreeting(context.Background(), writer, wsID, "Hello AGAIN (retry).", wakeKey); err != nil {
		t.Fatalf("retried delivery should no-op, got: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations (exactly one delivery): %v", err)
	}
	if len(emitter.events) != 1 {
		t.Fatalf("wake key must dedup the retry to ONE broadcast, got %d", len(emitter.events))
	}
	if msg := sentMessage(t, emitter); !strings.Contains(msg, "Hello from the agent") {
		t.Errorf("delivered message = %q, want the first delivery's text", msg)
	}
}

func TestFirstBootFallbackText(t *testing.T) {
	// A workspace with no reported tools must not claim a tool count.
	if got := firstBootFallbackText(0); strings.Contains(got, "0 ") {
		t.Errorf("zero-tool fallback claims a count: %q", got)
	}
	if got := firstBootFallbackText(45); !strings.Contains(got, "45 management tools") {
		t.Errorf("concierge fallback missing count: %q", got)
	}
}

// errDBDown is a sentinel for the fail-closed test.
var errDBDown = sentinelDBErr("db down")

type sentinelDBErr string

func (e sentinelDBErr) Error() string { return string(e) }

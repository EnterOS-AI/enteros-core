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

func expectNoHistory(mock sqlmock.Sqlmock, wsID string) {
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
}

func expectWriterSend(mock sqlmock.Sqlmock, wsID, name string) {
	mock.ExpectQuery("SELECT name, talk_to_user_enabled FROM workspaces").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "talk_to_user_enabled"}).AddRow(name, true))
	mock.ExpectExec(`INSERT INTO activity_logs.*'a2a_receive'.*'notify'`).
		WithArgs(wsID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
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

	expectNoHistory(mock, "ws-first")
	expectWriterSend(mock, "ws-first", "Scout")

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

	expectNoHistory(mock, "ws-fb")
	expectWriterSend(mock, "ws-fb", "Enter OS Agent")

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

	expectNoHistory(mock, "ws-err-reply")
	expectWriterSend(mock, "ws-err-reply", "Agent")

	greet("ws-err-reply", 0)

	msg := sentMessage(t, emitter)
	if strings.Contains(msg, "boom") {
		t.Errorf("error reply leaked into the greeting: %q", msg)
	}
	if !strings.Contains(msg, "online and ready") {
		t.Errorf("expected role-agnostic fallback, got %q", msg)
	}
}

func TestFirstBootGreeting_QueuedPollModeSendsNothing(t *testing.T) {
	// Poll-mode short-circuit: the proxy queued the greet prompt and the
	// agent will answer via /notify when it polls. Relaying anything now
	// would post the raw queued envelope as the first chat bubble AND later
	// duplicate the agent's real greeting.
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	var calls []string
	greet := FirstBootGreeter(
		NewAgentMessageWriter(db.DB, emitter),
		stubTurn(t, &calls, 200, `{"status":"queued","delivery_mode":"poll","method":"message/send"}`, nil),
	)

	expectNoHistory(mock, "ws-poll")

	greet("ws-poll", 0)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations: %v", err)
	}
	if len(emitter.events) != 0 {
		t.Fatalf("queued response must send nothing, got %#v", emitter.events)
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

	expectNoHistory(mock, "ws-json")
	expectWriterSend(mock, "ws-json", "Agent")

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

	// Exactly ONE history check + ONE send may hit the DB.
	expectNoHistory(mock, "ws-race")
	expectWriterSend(mock, "ws-race", "Agent")

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

func TestFirstBootGreeting_SkipsWhenHistoryExists(t *testing.T) {
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	var calls []string
	greet := FirstBootGreeter(
		NewAgentMessageWriter(db.DB, emitter),
		stubTurn(t, &calls, 200, `{}`, nil),
	)

	// History present (a restart / reconnect, not a first boot) — the gate
	// stops everything: no agent turn, no lookup, no INSERT, no broadcast.
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("ws-restart").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	greet("ws-restart", 45)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("expected no agent turn on non-first boot, got %d", len(calls))
	}
	if len(emitter.events) != 0 {
		t.Fatalf("expected no broadcast on non-first boot, got %#v", emitter.events)
	}
}

func TestFirstBootGreeting_SkipsOnHistoryCheckError(t *testing.T) {
	// Fail CLOSED: an unreadable history must not risk a duplicate greeting.
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	var calls []string
	greet := FirstBootGreeter(
		NewAgentMessageWriter(db.DB, emitter),
		stubTurn(t, &calls, 200, `{}`, nil),
	)

	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("ws-err").
		WillReturnError(errDBDown)

	greet("ws-err", 3)

	if len(calls) != 0 || len(emitter.events) != 0 {
		t.Fatalf("expected nothing on history-check error, got calls=%d events=%#v", len(calls), emitter.events)
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

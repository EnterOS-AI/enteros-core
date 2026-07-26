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

// expectClaimWon pins the atomic claim-on-delivery marker flip that PRECEDES a
// successful AgentMessageWriter.Send: has_greeted is set true via a
// compare-and-set (WHERE has_greeted = false) and exactly one row flips, so this
// caller won the claim and proceeds to Send. Replaces the old post-Send
// markGreeted write — the claim is now the authoritative cross-wake dedup.
func expectClaimWon(mock sqlmock.Sqlmock, wsID string) {
	mock.ExpectExec(`UPDATE workspaces SET has_greeted = true WHERE id = \$1 AND has_greeted = false`).
		WithArgs(wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectClaimLost pins the claim losing the compare-and-set (zero rows flipped)
// — another wake already greeted this box, so the caller SKIPS Send silently.
func expectClaimLost(mock sqlmock.Sqlmock, wsID string) {
	mock.ExpectExec(`UPDATE workspaces SET has_greeted = true WHERE id = \$1 AND has_greeted = false`).
		WithArgs(wsID).
		WillReturnResult(sqlmock.NewResult(0, 0))
}

// expectClaimRollback pins the rollback of a WON claim after Send failed — the
// greeting never reached the user, so has_greeted is reset to false to re-arm a
// future wake's retry.
func expectClaimRollback(mock sqlmock.Sqlmock, wsID string) {
	mock.ExpectExec(`UPDATE workspaces SET has_greeted = false WHERE id = \$1`).
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
	expectClaimWon(mock, "ws-first")
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

	expectNotGreeted(mock, "ws-fb")
	expectClaimWon(mock, "ws-fb")
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

	expectNotGreeted(mock, "ws-err-reply")
	expectClaimWon(mock, "ws-err-reply")
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
	reqBody, err := buildFirstBootGreetPayload(wsID, 0)
	if err != nil {
		t.Fatalf("buildFirstBootGreetPayload: %v", err)
	}
	realReply := `{"jsonrpc":"2.0","result":{"message":{"parts":[{"kind":"text","text":"Hey — I'm Scout, your research agent. Ask me to track a topic!"}]}}}`
	h := &WorkspaceHandler{broadcaster: emitter}
	expectClaimWon(mock, wsID)
	expectWriterSend(mock, wsID, "Scout")
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
	reqBody, err := buildFirstBootGreetPayload(wsID, 0)
	if err != nil {
		t.Fatalf("buildFirstBootGreetPayload: %v", err)
	}
	h := &WorkspaceHandler{broadcaster: emitter}
	expectClaimWon(mock, wsID)
	expectWriterSend(mock, wsID, "Agent")
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
	expectClaimWon(mock, "ws-json")
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

	// Exactly ONE marker check + ONE claim + ONE send may hit the DB.
	expectNotGreeted(mock, "ws-race")
	expectClaimWon(mock, "ws-race")
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

	// Boot 1 — fresh box: gate unset → agent turn → claim → deliver.
	expectNotGreeted(mock, wsID)
	expectClaimWon(mock, wsID)
	expectWriterSend(mock, wsID, "Ada")
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

func TestFirstBootGreeting_ClaimDedupsRetriedDelivery(t *testing.T) {
	// RFC concierge rule 2, idempotency: the has_greeted claim (atomic
	// compare-and-set) is the AUTHORITATIVE dedup for a RETRIED delivery — the
	// sync path racing its own drain, a double drain, or a replayed queue row.
	// The first deliverFirstBootGreeting WINS the claim (one row flips) and
	// Sends; the second LOSES the claim (zero rows flip) and skips. Exactly ONE
	// Send. Unlike the old in-memory sync.Map, the claim survives across two
	// DISTINCT wake goroutines and does not grow unboundedly.
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	const wsID = "ws-claim-dedup"
	writer := NewAgentMessageWriter(db.DB, emitter)

	// First delivery: WIN the claim, then Send.
	expectClaimWon(mock, wsID)
	expectWriterSend(mock, wsID, "Agent")
	// Second delivery: LOSE the claim (already true) → no Send.
	expectClaimLost(mock, wsID)

	if err := deliverFirstBootGreeting(context.Background(), writer, wsID, "Hello from the agent."); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	// Retry (a second distinct wake): must dedup on the claim — no Send.
	if err := deliverFirstBootGreeting(context.Background(), writer, wsID, "Hello AGAIN (retry)."); err != nil {
		t.Fatalf("retried delivery should no-op, got: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations (exactly one delivery): %v", err)
	}
	if len(emitter.events) != 1 {
		t.Fatalf("claim must dedup the retry to ONE broadcast, got %d", len(emitter.events))
	}
	if msg := sentMessage(t, emitter); !strings.Contains(msg, "Hello from the agent") {
		t.Errorf("delivered message = %q, want the first delivery's text", msg)
	}
}

func TestFirstBootGreeting_ClaimRollbackReArmsRetryOnSendFailure(t *testing.T) {
	// Rollback-on-failure (review fix): a WON claim whose Send FAILS must reset
	// has_greeted to false so a future wake re-claims and retries — never leave
	// the marker durably true for an undelivered greeting (commit-on-delivery).
	// A subsequent delivery then WINS a fresh claim and Sends.
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	const wsID = "ws-claim-rollback"
	writer := NewAgentMessageWriter(db.DB, emitter)

	// Attempt 1: win the claim, but the writer lookup errors → Send fails →
	// the claim is rolled back to false.
	expectClaimWon(mock, wsID)
	mock.ExpectQuery("SELECT name, talk_to_user_enabled FROM workspaces").
		WithArgs(wsID).
		WillReturnError(errDBDown)
	expectClaimRollback(mock, wsID)

	if err := deliverFirstBootGreeting(context.Background(), writer, wsID, "Greeting text."); err == nil {
		t.Fatal("expected Send failure to propagate as an error")
	}
	if len(emitter.events) != 0 {
		t.Fatalf("failed Send must not broadcast, got %#v", emitter.events)
	}

	// Attempt 2 (a later wake): the rolled-back marker lets it re-claim and Send.
	expectClaimWon(mock, wsID)
	expectWriterSend(mock, wsID, "Agent")
	if err := deliverFirstBootGreeting(context.Background(), writer, wsID, "Greeting text."); err != nil {
		t.Fatalf("retry after rollback should deliver, got: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations: %v", err)
	}
	if len(emitter.events) != 1 {
		t.Fatalf("retry after rollback must deliver exactly one greeting, got %d", len(emitter.events))
	}
}

func TestFirstBootGreeting_ReleaseGreetClaim_DetachedFromExpiredContext(t *testing.T) {
	// Edge-#1 (re-review MEDIUM): the has_greeted rollback must run on a context
	// DETACHED from the caller's Send context. The rollback fires precisely when
	// writer.Send FAILED, and the dominant cause is the greetSendTimeout/sendCtx
	// deadline expiring — so reusing that same done ctx for the rollback UPDATE
	// would fail DETERMINISTICALLY (database/sql rejects an exec on a cancelled ctx
	// before it reaches the pool), leaving has_greeted stuck true: nothing
	// delivered AND no future re-greet. Simulate the worst case with an
	// ALREADY-CANCELLED ctx: with the WithoutCancel detach the rollback UPDATE
	// still reaches the DB; WITHOUT it, the exec never fires and this expectation
	// goes unmet (the exact regression guard).
	mock := setupTestDB(t)
	const wsID = "ws-detached-rollback"

	expired, cancel := context.WithCancel(context.Background())
	cancel() // the Send-context budget is gone — as it is when Send failed on timeout

	expectClaimRollback(mock, wsID)
	releaseGreetClaim(expired, wsID)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("rollback UPDATE did not fire on the detached context (Edge-#1 regressed): %v", err)
	}

	// Prove the marker is genuinely re-armed: a SUBSEQUENT wake (fresh ctx) WINS a
	// new claim and delivers — the greeting the expired-ctx Send dropped is now
	// retried and the fresh chat is no longer permanently silent.
	emitter := &capturingEmitter{}
	writer := NewAgentMessageWriter(db.DB, emitter)
	expectClaimWon(mock, wsID)
	expectWriterSend(mock, wsID, "Agent")
	if err := deliverFirstBootGreeting(context.Background(), writer, wsID, "Re-greet after rollback."); err != nil {
		t.Fatalf("subsequent wake should re-greet after the detached rollback, got: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations: %v", err)
	}
	if len(emitter.events) != 1 {
		t.Fatalf("subsequent wake must deliver exactly one greeting, got %d", len(emitter.events))
	}
	if msg := sentMessage(t, emitter); !strings.Contains(msg, "Re-greet after rollback") {
		t.Errorf("re-greet delivered wrong text: %q", msg)
	}
}

func TestFirstBootGreeting_TwoDistinctWakesGreetExactlyOnce(t *testing.T) {
	// Rule-1 finding #2 / rule-2 nit 1: two DISTINCT wakes racing to greet the
	// same fresh box must collapse to EXACTLY ONE greeting via the atomic claim
	// — the exact case the old in-memory sync.Map could not cover (its keys did
	// not survive across two independent wake goroutines). K2 (sync path) wins
	// the claim and greets; K1 (its busy-queued greet now draining) tries to
	// deliver its own real reply, LOSES the claim, and delivers nothing.
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	const wsID = "ws-two-wakes"

	// Wake K2 — synchronous greet path: gate unset → claim WON → send.
	var calls []string
	greet := FirstBootGreeter(
		NewAgentMessageWriter(db.DB, emitter),
		stubTurn(t, &calls, 200,
			`{"jsonrpc":"2.0","result":{"message":{"parts":[{"kind":"text","text":"Hi, I'm Ada."}]}}}`,
			nil),
	)
	expectNotGreeted(mock, wsID)
	expectClaimWon(mock, wsID)
	expectWriterSend(mock, wsID, "Ada")
	greet(wsID, 0)

	// Wake K1 — a separate busy-queued greet now draining. Its real reply tries
	// to deliver, but the claim is already taken → LOSE → deliver nothing.
	reqBody, err := buildFirstBootGreetPayload(wsID, 0)
	if err != nil {
		t.Fatalf("buildFirstBootGreetPayload: %v", err)
	}
	h := &WorkspaceHandler{broadcaster: emitter}
	expectClaimLost(mock, wsID)
	h.attachQueuedTurnCompletion(context.Background(), wsID, false, reqBody,
		[]byte(`{"jsonrpc":"2.0","result":{"message":{"parts":[{"kind":"text","text":"Hi again from the drain."}]}}}`))
	h.asyncWG.Wait()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations (exactly one greeting across two wakes): %v", err)
	}
	if len(emitter.events) != 1 {
		t.Fatalf("two distinct wakes must greet exactly once, got %d", len(emitter.events))
	}
	if msg := sentMessage(t, emitter); !strings.Contains(msg, "I'm Ada") {
		t.Errorf("first wake's greeting should stand, got %q", msg)
	}
}

func TestFirstBootGreeting_TerminalDropDeliversFallback_PreservesToolCount(t *testing.T) {
	// Rule-1 finding #1 (never-silent) + finding #3 (toolCount): a busy-queued
	// first-boot greet whose drain TERMINALLY fails must still deliver the static
	// fallback — and a concierge (toolCount>0) must keep its tool-count text,
	// recovered from the greet payload metadata, not degrade to the zero-tool
	// greeting. Exercises the seam the drain's terminal branches call.
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	const wsID = "ws-terminal-drop"
	reqBody, err := buildFirstBootGreetPayload(wsID, 45)
	if err != nil {
		t.Fatalf("buildFirstBootGreetPayload: %v", err)
	}
	h := &WorkspaceHandler{broadcaster: emitter}
	expectClaimWon(mock, wsID)
	expectWriterSend(mock, wsID, "Concierge")
	h.deliverFirstBootFallbackOnTerminalDrop(context.Background(), wsID, reqBody)
	h.asyncWG.Wait()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations: %v", err)
	}
	msg := sentMessage(t, emitter)
	if !strings.Contains(msg, "45 management tools") {
		t.Errorf("terminal-drop fallback lost the concierge tool count: %q", msg)
	}
}

func TestFirstBootGreeting_TerminalDropNoOpForNonGreet(t *testing.T) {
	// The never-silent fallback is NARROW: a terminally-dropped item that is NOT
	// a self-first-boot-greet (an ordinary user turn, a restart-context wake)
	// must deliver nothing — no claim, no Send, no broadcast.
	setupTestDB(t) // no expectations => any DB call fails the test
	emitter := &capturingEmitter{}
	h := &WorkspaceHandler{broadcaster: emitter}
	reqBody := []byte(`{"jsonrpc":"2.0","method":"message/send","params":{"metadata":{"source_type":"self-restart-context"},"message":{"messageId":"m1"}}}`)
	h.deliverFirstBootFallbackOnTerminalDrop(context.Background(), "ws-nongreet", reqBody)
	h.asyncWG.Wait()
	if len(emitter.events) != 0 {
		t.Fatalf("non-greet terminal drop must deliver nothing, got %#v", emitter.events)
	}
}

func TestFirstBootGreeting_DrainedGreet_EmptyReplyPreservesToolCount(t *testing.T) {
	// Rule-1 finding #3 on the drain-deliver path: when the drained greet reply
	// is unusable and the fallback fires, a concierge (toolCount>0) keeps its
	// tool-count text — the count is threaded through the payload metadata rather
	// than hardcoded to firstBootFallbackText(0).
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	const wsID = "ws-drain-toolcount"
	reqBody, err := buildFirstBootGreetPayload(wsID, 45)
	if err != nil {
		t.Fatalf("buildFirstBootGreetPayload: %v", err)
	}
	h := &WorkspaceHandler{broadcaster: emitter}
	expectClaimWon(mock, wsID)
	expectWriterSend(mock, wsID, "Concierge")
	h.attachQueuedTurnCompletion(context.Background(), wsID, false,
		reqBody, []byte(`{"jsonrpc":"2.0","error":{"message":"boom"}}`))
	h.asyncWG.Wait()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB expectations: %v", err)
	}
	msg := sentMessage(t, emitter)
	if !strings.Contains(msg, "45 management tools") {
		t.Errorf("drain fallback lost the concierge tool count: %q", msg)
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

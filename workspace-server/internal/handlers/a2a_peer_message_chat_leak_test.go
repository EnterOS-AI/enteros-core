package handlers

// Regression coverage: an A2A message from a PEER AGENT must never render
// in "My Chat", the human-facing tab.
//
// The incident (2026-07-12, enter-os / CEO Assistant): a peer workspace
// replied over A2A and its message appeared as a blue user bubble in the
// operator's own chat, as if the human had typed it. Agent-to-agent
// traffic was being injected into the human conversation.
//
// The mechanism was a DISAGREEMENT between the two paths that feed My Chat:
//
//   - The READER (messagestore.PostgresMessageStore.queryActivityRows)
//     serves `activity_type='a2a_receive' AND source_id IS NULL` — a canvas
//     send has no caller workspace, so source_id is NULL; a peer agent
//     authenticates as itself, so its row carries source_id=<peer>. By that
//     rule a peer's row is correctly EXCLUDED from chat-history.
//
//   - The LIVE path (persistUserMessageAtIngest) broadcast USER_MESSAGE
//     UNCONDITIONALLY. The canvas appends that frame straight into My Chat.
//
// So a peer message rendered live in the human's chat and then vanished on
// reload (chat-history never had it) — the two paths disagreeing about what
// "a message in My Chat" means. This is the SECOND time they diverged:
// core#3082 fixed the identical leak for SYSTEM callers (the platform warmup
// turn "Platform readiness check — no action needed." rendered as a user
// bubble) but only special-cased isSystemCaller at the call site; peer agents
// were never covered.
//
// The fix makes the live path ask the reader's own question —
// isChatHistoryVisible == (callerIDToSourceID(callerID) == nil) — so the two
// cannot drift apart again. These tests pin the resulting contract:
//
//  1. peer caller  → activity row persisted (Agent Comms + audit), NO
//     USER_MESSAGE frame.
//  2. canvas caller → USER_MESSAGE frame still fires (cross-device sync,
//     core#2697, must not regress).
//  3. the predicate agrees with the reader's source_id rule for every
//     caller class — the anti-drift assertion.

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
)

// userMessageFrames returns just the USER_MESSAGE broadcasts — the frames
// the canvas appends into My Chat (ChatTab's onUserMessageBroadcast).
func userMessageFrames(c *capturingEmitter) []capturedEvent {
	var out []capturedEvent
	for _, e := range c.events {
		if e.eventType == string(events.EventUserMessage) {
			out = append(out, e)
		}
	}
	return out
}

// expectIngestRow sets up the DB calls persistUserMessageAtIngest makes:
// the workspace-name lookup, then the activity_logs INSERT.
func expectIngestRow(mock sqlmock.Sqlmock, wsID string) {
	mock.ExpectQuery("SELECT name FROM workspaces").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("CEO Assistant"))
	mock.ExpectExec("INSERT INTO activity_logs").
		WillReturnResult(sqlmock.NewResult(0, 1))
}

const peerA2ABody = `{"jsonrpc":"2.0","id":"peer-1","method":"message/send",` +
	`"params":{"message":{"role":"user","messageId":"msg-peer-1",` +
	`"parts":[{"kind":"text","text":"All four base images are mirrored."}]}}}`

// A peer agent's A2A message must NOT be broadcast as a user message.
// Pre-fix this FAILS: persistUserMessageAtIngest broadcast unconditionally,
// so the peer's text rendered as a blue bubble in the operator's My Chat.
func TestPeerAgentMessage_IsNotBroadcastIntoMyChat(t *testing.T) {
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	handler := NewWorkspaceHandler(emitter, nil, "http://localhost:8080", t.TempDir())

	const wsID = "11111111-1111-1111-1111-111111111111"  // the CEO Assistant
	const peerID = "22222222-2222-2222-2222-222222222222" // the peer agent

	expectIngestRow(mock, wsID)

	handler.persistUserMessageAtIngest(context.Background(), wsID, peerID, []byte(peerA2ABody), "message/send")

	// The row IS written — Agent Comms reads it, and it is the audit trail.
	// Only the human-chat bubble is suppressed.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the peer's activity_logs row must still be persisted (Agent Comms + audit): %v", err)
	}

	if frames := userMessageFrames(emitter); len(frames) != 0 {
		t.Fatalf("a peer agent's A2A message was broadcast as a USER_MESSAGE (%d frame(s)): "+
			"agent-to-agent traffic renders in the human's My Chat as if they typed it, and then "+
			"disappears on reload because chat-history excludes source_id=<peer>. It belongs in "+
			"Agent Comms only. payload=%v", len(frames), frames[0].payload)
	}
}

// The human's own message must still reach every other device. The canvas
// authenticates with the org/admin token and sets no X-Workspace-ID, so its
// callerID is empty — the same condition that makes its row's source_id NULL
// and puts it in chat-history. Guards against "fixing" the leak by killing
// cross-device sync (core#2697) outright.
func TestCanvasUserMessage_IsStillBroadcastForCrossDeviceSync(t *testing.T) {
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	handler := NewWorkspaceHandler(emitter, nil, "http://localhost:8080", t.TempDir())

	const wsID = "11111111-1111-1111-1111-111111111111"
	body := `{"jsonrpc":"2.0","id":"canvas-1","method":"message/send",` +
		`"params":{"message":{"role":"user","messageId":"msg-canvas-1",` +
		`"parts":[{"kind":"text","text":"can you link to my lark?"}]}}}`

	expectIngestRow(mock, wsID)

	handler.persistUserMessageAtIngest(context.Background(), wsID, "" /* canvas: no caller workspace */, []byte(body), "message/send")

	frames := userMessageFrames(emitter)
	if len(frames) != 1 {
		t.Fatalf("the human's own message must still broadcast exactly one USER_MESSAGE for "+
			"cross-device sync (core#2697); got %d", len(frames))
	}
	payload, ok := frames[0].payload.(map[string]interface{})
	if !ok {
		t.Fatalf("USER_MESSAGE payload shape changed: %T", frames[0].payload)
	}
	if payload["content"] != "can you link to my lark?" {
		t.Fatalf("USER_MESSAGE carried the wrong content: %v", payload["content"])
	}
}

// ANTI-DRIFT. The live broadcast gate and the chat-history reader must answer
// the SAME question for every caller class. The reader's rule is source_id IS
// NULL, which is exactly callerIDToSourceID(callerID) == nil — so if this ever
// stops holding, the two paths have forked again and one of the two leaks
// (peer traffic into My Chat) or drops (human message missing live) returns.
func TestChatHistoryVisibility_MatchesTheReadersSourceIDRule(t *testing.T) {
	cases := []struct {
		name     string
		callerID string
		inMyChat bool
		why      string
	}{
		{"canvas (org token, no caller workspace)", "", true,
			"source_id NULL → chat-history serves it → it IS the human's turn"},
		{"peer agent", "22222222-2222-2222-2222-222222222222", false,
			"source_id=<peer> → chat-history excludes it → Agent Comms only"},
		{"the workspace itself (self-send)", "11111111-1111-1111-1111-111111111111", false,
			"source_id=<self> → chat-history excludes it → not a human turn"},
		{"system caller (platform warmup, core#3082)", "system:concierge-warmup", true,
			"normalized to source_id NULL; the platform's own turns are classified " +
				"system/notice by source_type at render, NOT hidden here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isChatHistoryVisible(tc.callerID)
			if got != tc.inMyChat {
				t.Fatalf("isChatHistoryVisible(%q) = %v, want %v — %s.\n"+
					"The live USER_MESSAGE broadcast and the chat-history reader "+
					"(messagestore: activity_type='a2a_receive' AND source_id IS NULL) have "+
					"drifted apart; whatever they now disagree on will render live and vanish "+
					"on reload, or vice versa.", tc.callerID, got, tc.inMyChat, tc.why)
			}
			// Pin the equivalence itself, not just the truth table.
			if want := callerIDToSourceID(tc.callerID) == nil; got != want {
				t.Fatalf("isChatHistoryVisible(%q)=%v but callerIDToSourceID-is-nil=%v — the "+
					"predicate no longer mirrors the source_id rule it exists to mirror",
					tc.callerID, got, want)
			}
		})
	}
}

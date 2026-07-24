package handlers

// a2a_proxy_session_continuity_test.go — the SYSTEM-outcome gate for
// "everything continues in ONE session across restarts, unless the user
// clicks New Session" (core#4587 follow-up).
//
// The canvas half (the browser keeps one stable conversation id across
// reloads and rotates only on New Session) is gated by canvas
// useChatSend.contextId.test.tsx. This is the PLATFORM half, and it is
// cross-runtime by construction: every runtime keys (or resumes) its session
// on the a2a message.contextId, so if the platform stamps the SAME stable
// contextId on
//
//   * a normal canvas turn (via the ensureCanvasSessionContextID belt), AND
//   * a restart-context wake (buildRestartA2APayload), AND
//   * a first-boot greeting (buildFirstBootGreetPayload)
//
// then a restart / reprovision / first boot lands in the user's existing
// conversation instead of a fresh, runtime-minted session — the Langfuse
// 3-session fragmentation this convergence fixed. The belt test proves the
// canvas turn gets canvas-<wsid>; NOTHING before this asserted that the two
// platform self-turns converge on the same id, which is the actual
// continuity guarantee.
//
// The three properties this pins:
//   1. CONVERGENCE — all three surfaces produce identical contextId.
//   2. RESTART-INVARIANCE — the id is a pure function of the workspace id, so
//      repeated builds (successive restarts) never drift it.
//   3. NEW-SESSION CARVE-OUT — a canvas turn carrying an explicit rotated
//      contextId is preserved untouched, so the ONLY way to leave the shared
//      thread is the user's explicit New Session (client-minted sess-*).

import (
	"encoding/json"
	"testing"
)

// ctxIDFromPayload extracts params.message.contextId from a self-turn payload
// (buildRestartA2APayload / buildFirstBootGreetPayload shape).
func ctxIDFromPayload(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Params struct {
			Message struct {
				ContextID string `json:"contextId"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return env.Params.Message.ContextID
}

func TestSessionContinuity_AllSurfacesConvergeOnStableContextID(t *testing.T) {
	const ws = "11111111-2222-3333-4444-555555555555"
	want := canvasSessionContextID(ws) // "canvas-<ws>"

	// Property 1 — CONVERGENCE.
	restartBody, err := buildRestartA2APayload(ws, "=== WORKSPACE RESTART CONTEXT ===")
	if err != nil {
		t.Fatalf("buildRestartA2APayload: %v", err)
	}
	if got := ctxIDFromPayload(t, restartBody); got != want {
		t.Errorf("restart-context contextId = %q, want %q — a restart would land in a DIFFERENT session than the user's chat (continuity break)", got, want)
	}

	greetBody, err := buildFirstBootGreetPayload(ws)
	if err != nil {
		t.Fatalf("buildFirstBootGreetPayload: %v", err)
	}
	if got := ctxIDFromPayload(t, greetBody); got != want {
		t.Errorf("first-boot contextId = %q, want %q — first boot would fragment from the user's session", got, want)
	}

	// The canvas turn: a contextless message/send gets the SAME id injected.
	canvasBody := []byte(`{"params":{"message":{"role":"user","messageId":"m-1","parts":[{"kind":"text","text":"hi"}]}}}`)
	injected, changed := ensureCanvasSessionContextID(canvasBody, ws)
	if !changed {
		t.Fatal("belt did not inject a contextId on a contextless canvas turn")
	}
	if got, _ := parseMsg(t, injected)["contextId"].(string); got != want {
		t.Errorf("canvas turn contextId = %q, want %q", got, want)
	}
	// The whole point: restart + first-boot + canvas all equal -> ONE session.

	// Property 2 — RESTART-INVARIANCE: pure function of ws id, so successive
	// restarts (repeated builds) never drift the id.
	restartBody2, _ := buildRestartA2APayload(ws, "=== WORKSPACE RESTART CONTEXT === (second restart)")
	if got := ctxIDFromPayload(t, restartBody2); got != want {
		t.Errorf("contextId drifted across restarts: %q != %q", got, want)
	}
}

func TestSessionContinuity_NewSessionRotationIsPreserved(t *testing.T) {
	const ws = "11111111-2222-3333-4444-555555555555"
	// A canvas turn carrying an EXPLICIT rotated contextId (the client-local
	// sess-* the user minted by clicking "New Session") must be preserved —
	// the belt must NOT drag it back onto the shared canvas-<ws> thread.
	rotated := "sess-" + ws + "-abc123"
	body := []byte(`{"params":{"message":{"role":"user","messageId":"m-2","contextId":"` + rotated + `","parts":[{"kind":"text","text":"fresh"}]}}}`)
	out, changed := ensureCanvasSessionContextID(body, ws)
	if changed {
		t.Fatal("belt overwrote a user-rotated contextId — New Session would not actually start a fresh session")
	}
	if got, _ := parseMsg(t, out)["contextId"].(string); got != rotated {
		t.Errorf("rotated contextId altered: got %q, want %q", got, rotated)
	}
	if rotated == canvasSessionContextID(ws) {
		t.Fatal("rotated id collided with the default — a New Session would not diverge")
	}
}

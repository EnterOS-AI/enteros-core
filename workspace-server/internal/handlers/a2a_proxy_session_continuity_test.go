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
// The two properties this pins:
//   1. CONVERGENCE — all three surfaces produce identical contextId.
//   2. RESTART-INVARIANCE — the id is a pure function of the workspace id, so
//      repeated builds (successive restarts) never drift it.
//
// The NEW-SESSION carve-out — a canvas turn carrying an explicit rotated
// contextId (the client-minted sess-*) is preserved untouched, so the only way
// to leave the shared thread is the user's explicit New Session — is the belt's
// job and is gated by TestEnsureCanvasSessionContextID_PreservesCallerSupplied
// (a2a_proxy_contextid_belt_test.go). It is deliberately NOT re-asserted here:
// on the Go side "preserve a caller-supplied contextId" is one behavior
// regardless of the id's prefix (the belt already pins it), and the sess-* vs
// canvas-<ws> namespace *divergence* is a client-side minting convention pinned
// canvas-side (useChatSend.contextId.test.tsx) — this package cannot assert it
// without comparing two hardcoded literals, which tests nothing.

import (
	"encoding/json"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/sessionid"
)

// TestSessionContinuity_BeltUsesTheOneAuthority pins that the a2a proxy belt /
// platform self-turns derive their contextId from the SINGLE authority
// (sessionid.DefaultContextID) — the SAME value the provisioner injects into the
// workspace container as MOLECULE_DEFAULT_SESSION_CONTEXT_ID for the runtime's
// self-wakes. If a future refactor re-inlines a "canvas-" literal here, the
// platform id and the provisioned runtime id could drift; this fails first.
func TestSessionContinuity_BeltUsesTheOneAuthority(t *testing.T) {
	const ws = "11111111-2222-3333-4444-555555555555"
	if got, want := canvasSessionContextID(ws), sessionid.DefaultContextID(ws); got != want {
		t.Fatalf("belt/self-turn contextId %q diverged from the sessionid authority %q", got, want)
	}
}

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

	// Pin the ABSOLUTE format, not just convergence: if canvasSessionContextID
	// itself drifted (say, gained a boot counter), every surface would drift
	// WITH it and the convergence checks below would still pass while
	// cross-restart continuity silently broke. The canvas side pins the same
	// literal (useChatSend.contextId.test.tsx expects "canvas-ws-ctx"), so the
	// two repos jointly hold the wire format fixed.
	if want != "canvas-"+ws {
		t.Fatalf("canvasSessionContextID format drifted: %q (must be canvas-<workspaceID> — restart-stable and matching the canvas test's literal)", want)
	}

	// Property 1 — CONVERGENCE.
	restartBody, err := buildRestartA2APayload(ws, "=== WORKSPACE RESTART CONTEXT ===")
	if err != nil {
		t.Fatalf("buildRestartA2APayload: %v", err)
	}
	if got := ctxIDFromPayload(t, restartBody); got != want {
		t.Errorf("restart-context contextId = %q, want %q — a restart would land in a DIFFERENT session than the user's chat (continuity break)", got, want)
	}

	greetBody, err := buildFirstBootGreetPayload(ws, 0)
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

// NOTE: the New-Session carve-out (a caller-supplied / user-rotated contextId is
// preserved untouched) is gated by the belt test
// TestEnsureCanvasSessionContextID_PreservesCallerSupplied in
// a2a_proxy_contextid_belt_test.go — see the file header for why it is not
// duplicated here.

package handlers

// a2a_proxy_contextid_belt_test.go — unit cover for the server-side session-
// continuity belt (concierge double-greeting fix). The belt injects a stable,
// deterministic message.contextId into canvas-origin message/send turns that
// arrive without one, so the runtime a2a-sdk does not mint a fresh context_id
// per request and re-open (re-greet) a new session every turn.

import (
	"encoding/json"
	"strings"
	"testing"
)

// parseMsg is a tiny helper: unmarshal a normalized A2A envelope and return the
// params.message map so a test can assert on the injected/preserved contextId
// without dragging in the whole JSON-RPC shape.
func parseMsg(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("body is not valid JSON: %v; body=%s", err, body)
	}
	params, ok := payload["params"].(map[string]interface{})
	if !ok {
		t.Fatalf("payload has no params object; body=%s", body)
	}
	msg, ok := params["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("params has no message object; body=%s", body)
	}
	return msg
}

// TestEnsureCanvasSessionContextID_InjectsWhenMissing is the core regression:
// a canvas message/send with NO contextId gets the deterministic per-workspace
// id, and every other message field is preserved byte-for-byte.
func TestEnsureCanvasSessionContextID_InjectsWhenMissing(t *testing.T) {
	const ws = "9530ba7b-d9cf-514e-920a-4672ac714511"
	body := []byte(`{"jsonrpc":"2.0","id":"rpc-1","method":"message/send","params":{"message":{"role":"user","messageId":"m-1","parts":[{"kind":"text","text":"hi"}]}}}`)

	out, changed := ensureCanvasSessionContextID(body, ws)
	if !changed {
		t.Fatal("expected the belt to inject a contextId (changed=true), got changed=false")
	}
	msg := parseMsg(t, out)
	got, _ := msg["contextId"].(string)
	if want := canvasSessionContextID(ws); got != want {
		t.Errorf("injected contextId = %q, want %q", got, want)
	}
	// Untouched fields.
	if msg["role"] != "user" {
		t.Errorf("role clobbered: got %v", msg["role"])
	}
	if msg["messageId"] != "m-1" {
		t.Errorf("messageId clobbered: got %v", msg["messageId"])
	}
	parts, ok := msg["parts"].([]interface{})
	if !ok || len(parts) != 1 {
		t.Fatalf("parts clobbered: got %v", msg["parts"])
	}
	if p0, _ := parts[0].(map[string]interface{}); p0["text"] != "hi" {
		t.Errorf("part text clobbered: got %v", parts[0])
	}
}

// TestEnsureCanvasSessionContextID_PreservesCallerSupplied verifies an updated
// canvas's own contextId (which drives per-conversation session + "New session"
// rotation) is NEVER overwritten.
func TestEnsureCanvasSessionContextID_PreservesCallerSupplied(t *testing.T) {
	const ws = "ws-abc"
	const clientCtx = "conv-ws-abc-11112222"
	body := []byte(`{"params":{"message":{"role":"user","messageId":"m-2","contextId":"` + clientCtx + `","parts":[{"kind":"text","text":"hi"}]}}}`)

	out, changed := ensureCanvasSessionContextID(body, ws)
	if changed {
		t.Fatal("expected caller-supplied contextId to be preserved (changed=false), got changed=true")
	}
	msg := parseMsg(t, out)
	if got, _ := msg["contextId"].(string); got != clientCtx {
		t.Errorf("caller contextId altered: got %q, want %q", got, clientCtx)
	}
}

// TestEnsureCanvasSessionContextID_EmptyStringCtxIsInjected treats an empty /
// whitespace-only contextId as "missing" — the runtime would otherwise fall
// through to minting a fresh id.
func TestEnsureCanvasSessionContextID_EmptyStringCtxIsInjected(t *testing.T) {
	const ws = "ws-empty"
	for _, blank := range []string{`""`, `"   "`} {
		body := []byte(`{"params":{"message":{"role":"user","contextId":` + blank + `,"parts":[{"kind":"text","text":"hi"}]}}}`)
		out, changed := ensureCanvasSessionContextID(body, ws)
		if !changed {
			t.Fatalf("blank contextId %s should be injected, got changed=false", blank)
		}
		msg := parseMsg(t, out)
		if got, _ := msg["contextId"].(string); got != canvasSessionContextID(ws) {
			t.Errorf("blank contextId %s: injected %q, want %q", blank, got, canvasSessionContextID(ws))
		}
	}
}

// TestEnsureCanvasSessionContextID_Deterministic pins the two properties the
// runtime relies on: the id depends ONLY on the workspace id, and it is stable
// across repeated calls (so turn N+1 resumes turn N's session).
func TestEnsureCanvasSessionContextID_Deterministic(t *testing.T) {
	const ws = "ws-stable"
	body := []byte(`{"params":{"message":{"role":"user","parts":[{"kind":"text","text":"hi"}]}}}`)

	out1, _ := ensureCanvasSessionContextID(body, ws)
	out2, _ := ensureCanvasSessionContextID(body, ws)
	c1, _ := parseMsg(t, out1)["contextId"].(string)
	c2, _ := parseMsg(t, out2)["contextId"].(string)
	if c1 != c2 {
		t.Errorf("non-deterministic contextId across calls: %q vs %q", c1, c2)
	}
	if other, _ := parseMsg(t, mustInject(t, body, "ws-different"))["contextId"].(string); other == c1 {
		t.Errorf("distinct workspaces must derive distinct contextIds; both = %q", c1)
	}
	// No colons — the id must survive runtime session-id sanitisation intact.
	if strings.Contains(c1, ":") {
		t.Errorf("contextId %q contains a colon; would be mangled by session-id sanitisation", c1)
	}
}

func mustInject(t *testing.T, body []byte, ws string) []byte {
	t.Helper()
	out, changed := ensureCanvasSessionContextID(body, ws)
	if !changed {
		t.Fatalf("expected injection for ws=%s", ws)
	}
	return out
}

// TestEnsureCanvasSessionContextID_NoOpOnBadInput covers the non-destructive
// contract: an empty workspace id, malformed JSON, or a non-message shape all
// return the original body unchanged.
func TestEnsureCanvasSessionContextID_NoOpOnBadInput(t *testing.T) {
	cases := map[string][2]string{ // name -> {workspaceID, body}
		"empty workspace id": {"", `{"params":{"message":{"parts":[]}}}`},
		"not json":           {"ws", `not-json`},
		"no params":          {"ws", `{"jsonrpc":"2.0"}`},
		"no message":         {"ws", `{"params":{}}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ws, body := tc[0], []byte(tc[1])
			out, changed := ensureCanvasSessionContextID(body, ws)
			if changed {
				t.Errorf("expected no change for %q, got changed=true", name)
			}
			if string(out) != tc[1] {
				t.Errorf("body mutated on no-op path: got %s, want %s", out, tc[1])
			}
		})
	}
}

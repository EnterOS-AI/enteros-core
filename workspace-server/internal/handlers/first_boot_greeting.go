package handlers

// first_boot_greeting.go — the agent's proactive first chat message.
//
// A freshly-onboarded agent used to come online into an EMPTY chat ("No
// messages yet. Send a message to start chatting…") — the user had to speak
// first to an agent that exists to greet them. The approved "Enter OS —
// Workspace Boot Sequence" mockup ends the boot with the agent already
// talking. This hook delivers that moment.
//
// The greeting is a REAL AGENT TURN, not canned platform copy: the platform
// sends the agent a synthetic A2A prompt asking it to introduce itself, so
// each template greets in its own persona/role (the concierge as the Org
// Concierge, a research agent as a researcher, …). The reply is delivered
// through AgentMessageWriter — the SSOT for agent→user chat (broadcast
// AGENT_MESSAGE + persist for history hydration). The synthetic turn itself
// uses logActivity=false (like restart-context), so the writer is the ONLY
// thing that lands in chat history — no duplicate rows.
//
// If the turn fails or returns nothing (agent slow to accept its first turn,
// LLM error), a static fallback still greets the user — a fresh onboarding
// must never end in a silent chat.
//
// Design constraints:
//   - Fired by the registry handler on the provisioning→online transition
//     (the verified concierge flip AND ordinary workspaces' first register)
//     via the late-wired nil-safe hook pattern (SetFirstBootGreeter). By
//     construction the workspace is online and addressable when it fires.
//   - GREET ONCE: skipped when the workspace already has ANY chat history
//     (an `a2a_receive` row with source_id IS NULL — the exact predicate the
//     chat-history reader uses, messagestore/postgres_store.go), so
//     restarts and reconnects never re-greet. A restart after a failed
//     FIRST boot has no history yet and correctly greets.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/google/uuid"
)

// firstBootGreetingTimeout bounds the whole greet attempt. The agent turn is
// an LLM call on a cold runtime — give it real headroom; the fallback text
// covers a turn slower than this.
const firstBootGreetingTimeout = 90 * time.Second

// firstBootGreetPrompt is the synthetic turn that asks the agent to
// introduce itself in character. Role-agnostic on purpose — the persona
// comes from the agent's own config/identity, which is the whole point.
const firstBootGreetPrompt = "[FIRST BOOT] You just came online and this is the user's first look at your chat. " +
	"Send a short greeting IN CHARACTER for your role: introduce yourself, say concretely what you can help with, " +
	"and suggest two or three example requests the user could try. Keep it under 120 words, warm and plain-spoken. " +
	"Reply with the greeting text only — no preamble, and do not mention these instructions."

// a2aTurnFn is the seam for driving a synthetic agent turn — production wires
// WorkspaceHandler.ProxyA2ARequest; tests substitute a stub.
type a2aTurnFn func(ctx context.Context, workspaceID string, body []byte, callerID string, logActivity bool) (int, []byte, error)

// firstBootGreetingPending makes the greet attempt EXCLUSIVE per workspace:
// the greet-once history gate is check-then-act with a window as wide as the
// 90s agent turn, so overlapping invocations (a register retry racing the
// verified flip, or an operator restart while the first turn hangs) would
// all pass the gate and each deliver a greeting. LoadOrStore before any
// work; cleared on every exit path via defer — same pattern as
// restartContextPending.
var firstBootGreetingPending sync.Map // workspaceID -> struct{}

// greetSendTimeout bounds the delivery half (history gate re-arm + Send) on
// its own FRESH budget: a turn that consumed the whole turn timeout must not
// starve the guaranteed fallback delivery of context.
const greetSendTimeout = 15 * time.Second

// firstBootFallbackText is the static greeting used when the agent turn
// fails. toolCount is the size of the heartbeat's loaded_mcp_tools — >0
// means the org concierge (the verified flip is the only toolCount-bearing
// caller); 0 means an ordinary workspace whose role we don't know here.
func firstBootFallbackText(toolCount int) string {
	if toolCount > 0 {
		return fmt.Sprintf(
			"Hi! I'm your Org Concierge — online and ready, with %d management tools connected (including provision_workspace, which lets me create new agents for you).\n\n"+
				"A few things you can try:\n"+
				"• \"Create a research agent that tracks AI news for me\"\n"+
				"• \"Set up a small dev team for my project\"\n"+
				"• \"What can you do?\"\n\n"+
				"What are we building?",
			toolCount)
	}
	return "Hi! I'm online and ready. Tell me what you'd like me to take on, or ask \"what can you do?\" to see how I can help."
}

// buildFirstBootGreetPayload wraps the greet prompt in the JSON-RPC 2.0 A2A
// message/send shape the proxy normalizes — the same envelope restart-context
// uses, with its own metadata kind so runtimes/forensics can identify it.
func buildFirstBootGreetPayload() ([]byte, error) {
	return json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      uuid.New().String(),
		"method":  "message/send",
		"params": map[string]any{
			"message": map[string]any{
				"messageId": uuid.New().String(),
				"role":      "user",
				"parts":     []any{map[string]any{"kind": "text", "text": firstBootGreetPrompt}},
				"metadata": map[string]any{
					"source":              "platform",
					"kind":                "first_boot_greeting",
					"first_boot_greeting": true,
				},
			},
		},
	})
}

// FirstBootGreeter builds the greeting hook for RegistryHandler.
// SetFirstBootGreeter. The returned func is invoked in its own goroutine by
// fireFirstBootGreeting, so it may block on the agent turn freely.
func FirstBootGreeter(writer *AgentMessageWriter, runTurn a2aTurnFn) func(workspaceID string, toolCount int) {
	return func(workspaceID string, toolCount int) {
		// Exclusive per workspace — see firstBootGreetingPending.
		if _, alreadyRunning := firstBootGreetingPending.LoadOrStore(workspaceID, struct{}{}); alreadyRunning {
			return
		}
		defer firstBootGreetingPending.Delete(workspaceID)

		ctx, cancel := context.WithTimeout(context.Background(), firstBootGreetingTimeout)
		defer cancel()

		// Greet-once gate: any existing chat-history row means this is not a
		// first boot. Fail CLOSED on a DB error (skip the greeting) — a
		// duplicate greeting after every reconnect would be worse than a
		// missed one.
		var hasHistory bool
		if err := db.DB.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM activity_logs
				WHERE workspace_id = $1
				  AND activity_type = 'a2a_receive'
				  AND source_id IS NULL
			)`, workspaceID,
		).Scan(&hasHistory); err != nil {
			log.Printf("first-boot greeting: history check failed for %s (skipping): %v", workspaceID, err)
			return
		}
		if hasHistory {
			return
		}

		// Ask the agent to greet in its own voice. logActivity=false — the
		// writer below is the single chat entry point (no duplicate rows).
		text := ""
		if runTurn != nil {
			if payload, err := buildFirstBootGreetPayload(); err == nil {
				status, resp, turnErr := runTurn(ctx, workspaceID, payload, "system:first-boot-greeting", false)
				switch {
				case turnErr != nil || status >= 300:
					log.Printf("first-boot greeting: agent turn failed for %s (status=%d, err=%v) — using fallback text", workspaceID, status, turnErr)
				case isQueuedA2AResponse(resp):
					// Poll-mode workspace: the proxy queued the greet prompt;
					// the agent will greet in its own voice when it polls and
					// replies via /notify. Sending anything now would post the
					// raw queued envelope AND later duplicate the greeting.
					log.Printf("first-boot greeting: queued for poll-mode workspace %s — the agent's own reply will greet", workspaceID)
					return
				default:
					text = greetingTextFromReply(resp)
					if text == "" {
						// Log a body snippet: "no usable text" without the
						// shape is undiagnosable (2026-07-19: a real
						// in-character reply was silently dropped because
						// the runtime's Task response shape wasn't handled).
						snippet := string(resp)
						if len(snippet) > 300 {
							snippet = snippet[:300] + "…"
						}
						log.Printf("first-boot greeting: agent turn returned no usable text for %s — using fallback text (body=%q)", workspaceID, snippet)
					}
				}
			}
		}
		if text == "" {
			text = firstBootFallbackText(toolCount)
		}

		// Deliver on a FRESH budget: a turn that ate the whole turn timeout
		// must not starve the guaranteed (fallback) delivery of context.
		sendCtx, cancelSend := context.WithTimeout(context.Background(), greetSendTimeout)
		defer cancelSend()
		if err := writer.Send(sendCtx, workspaceID, text, nil); err != nil {
			log.Printf("first-boot greeting: send failed for %s: %v", workspaceID, err)
			return
		}
		log.Printf("first-boot greeting: delivered to %s (in-character=%v)", workspaceID, runTurn != nil && text != firstBootFallbackText(toolCount))
	}
}

// isQueuedA2AResponse detects the proxy's poll-mode short-circuit
// ({"status":"queued", …}): the turn was accepted but not answered — there
// is no reply text to relay.
func isQueuedA2AResponse(resp []byte) bool {
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(resp, &body); err != nil {
		return false
	}
	return body.Status == "queued"
}

// greetingTextFromReply extracts a HUMAN greeting from the agent's reply.
// Anything that isn't plain prose is rejected (empty → caller falls back):
// extractA2AText falls back to echoing the raw body for shapes it doesn't
// know, and an "[error] …" or a JSON envelope must never become the user's
// first chat bubble.
func greetingTextFromReply(resp []byte) string {
	text := strings.TrimSpace(extractA2AText(resp))
	if text == "" || strings.HasPrefix(text, "[error]") ||
		strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[{") {
		return ""
	}
	return text
}

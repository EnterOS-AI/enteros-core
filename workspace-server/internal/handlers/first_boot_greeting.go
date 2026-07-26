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
// If the greet turn is BUSY-QUEUED (the proxy returns a queued/busy ack rather
// than the reply), the static fallback does NOT fire: the agent's real reply is
// produced later and the queue drain delivers it via AgentMessageWriter
// (a2a_queue.go attachQueuedTurnCompletion's self-first-boot-greet exception).
// The static fallback still covers a turn that genuinely fails or returns
// nothing (agent slow to accept its first turn, LLM error) — a fresh onboarding
// must never end in a silent chat.
//
// Design constraints:
//   - Fired by the registry handler on the provisioning→online transition
//     (the verified concierge flip AND ordinary workspaces' first register)
//     via the late-wired nil-safe hook pattern (SetFirstBootGreeter). By
//     construction the workspace is online and addressable when it fires.
//   - GREET ONCE (RFC concierge rule 2): gated on the workspaces.has_greeted
//     boot marker — the SINGLE authoritative "has this box been greeted"
//     signal (SSOT), read here and by restart-context arbitration. The marker
//     is the AUTHORITATIVE atomic cross-wake dedup: every delivery point CLAIMS
//     it via a compare-and-set (UPDATE ... WHERE has_greeted = false) BEFORE
//     Send and delivers only if it won the claim; a claim whose Send fails is
//     ROLLED BACK to false. Net: the marker is durably true ONLY for a greeting
//     the user actually saw, so a decided-but-undelivered greet still greets on
//     the next boot and a delivered one never re-greets — even across two
//     DISTINCT wakes (the case the old in-memory sync.Map dedup could not
//     cover). This REPLACES the old derived activity_logs query (which answered
//     "has the USER chatted", not "has the box greeted" — the double-greet hole
//     where a greeted-but-silent box re-greeted on reconnect). A restart after a
//     FAILED first boot has no marker yet and correctly greets.

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

// firstBootGreetSourceType is the params.metadata.source_type marker stamped on
// the synthetic greet turn (mirrored in messagestore.selfSourceTypes and the
// runtime). It classifies a persisted/drained greet as an internal self-message
// (never a blue user bubble) AND is the key attachQueuedTurnCompletion matches
// to DELIVER — rather than skip — a busy-queued greet's real drained reply.
const firstBootGreetSourceType = "self-first-boot-greet"

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

// greetRollbackTimeout bounds the DETACHED has_greeted rollback (releaseGreetClaim)
// on its own fresh budget — see releaseGreetClaim for why the rollback must never
// ride the Send context that just failed.
const greetRollbackTimeout = 5 * time.Second

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
//
// toolCount rides in the metadata (additionalProperties-safe) so a busy-queued
// greet that drains later — or a terminal-failure fallback on the drain path —
// can recover the concierge's tool-count fallback text instead of degrading to
// the role-agnostic zero-tool greeting (rule-1 finding #3). Read back via
// firstBootToolCount.
func buildFirstBootGreetPayload(workspaceID string, toolCount int) ([]byte, error) {
	return json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      uuid.New().String(),
		"method":  "message/send",
		"params": map[string]any{
			"message": map[string]any{
				"messageId": uuid.New().String(),
				"contextId": platformTurnContextID(workspaceID),
				"role":      "user",
				"parts":     []any{map[string]any{"kind": "text", "text": firstBootGreetPrompt}},
				"metadata": map[string]any{
					"source": "platform",
					"kind":   "first_boot_greeting",
					// SSOT self-message classifier (messagestore.selfSourceTypes):
					// if this internal prompt is ever persisted (e.g. a
					// busy-queued greet drained later), it renders as a system
					// notice, never a blue user bubble.
					"source_type":         firstBootGreetSourceType,
					"first_boot_greeting": true,
					// Concierge tool count — threaded decision→drain so the drain
					// fallback keeps the tool-count greeting (rule-1 finding #3).
					"first_boot_tool_count": toolCount,
				},
			},
		},
	})
}

// claimGreetDelivery is the AUTHORITATIVE atomic cross-wake dedup for the
// first-boot greeting (replaces the old in-memory firstBootGreetDelivered
// sync.Map, which could not survive across two DISTINCT wake goroutines and
// grew unboundedly). It performs a compare-and-set on the has_greeted boot
// marker (SSOT) — `UPDATE ... WHERE has_greeted = false` — and reports whether
// THIS caller flipped the row (exactly one row affected = we won the claim).
//
// Every greeting delivery point claims BEFORE Send; only the winner delivers.
// Two distinct wakes racing to greet the same fresh box (K1's busy-queued drain
// and K2's synchronous greet) both call this, but only the first flips the
// marker and delivers — closing the double-greet-across-wakes hole. The loser
// skips silently. On a claimed-but-undelivered greet the winner rolls the
// marker back (releaseGreetClaim) so a future wake retries — commit-on-delivery
// is preserved (the marker is durably true ONLY once the user has seen it).
func claimGreetDelivery(ctx context.Context, workspaceID string) (won bool, err error) {
	res, err := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET has_greeted = true WHERE id = $1 AND has_greeted = false`, workspaceID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// releaseGreetClaim rolls the has_greeted marker back to false after a WON claim
// whose delivery FAILED — the greeting never reached the user, so a future wake
// must be free to re-claim and retry. This is what keeps commit-on-delivery
// honest: the marker is never left durably true for a greeting the user did not
// see. Best-effort: a failed rollback is logged (worst case a missed re-greet,
// strictly better than crashing the delivery goroutine).
//
// The rollback runs on a context DETACHED from the caller's Send context
// (context.WithoutCancel + a fresh greetRollbackTimeout budget). This is
// load-bearing: the rollback is triggered precisely when writer.Send FAILED, and
// the most common cause is that the greetSendTimeout/sendCtx deadline expired.
// Reusing that same already-expired ctx for the rollback UPDATE would make the
// write fail DETERMINISTICALLY (database/sql rejects an exec on a done context
// before it reaches the pool) — leaving has_greeted stuck true for a greeting the
// user never saw: nothing delivered, no future re-greet, and restart-context
// wrongly firing. WithoutCancel keeps any request-scoped values while dropping the
// dead deadline/cancellation; the fresh 5s timeout bounds the detached write.
// (Same WithoutCancel+WithTimeout shape the drain-path delivery budgets use.) A
// rollback that STILL fails on the fresh context is a real DB error — logged, and
// that residual missed re-greet is inherent and acceptable.
func releaseGreetClaim(ctx context.Context, workspaceID string) {
	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), greetRollbackTimeout)
	defer cancel()
	if _, err := db.DB.ExecContext(rbCtx,
		`UPDATE workspaces SET has_greeted = false WHERE id = $1`, workspaceID); err != nil {
		log.Printf("first-boot greeting: failed to roll back has_greeted claim for %s: %v", workspaceID, err)
	}
}

// workspaceHasGreeted reads the has_greeted boot marker (SSOT) — the single
// authoritative answer to "has this workspace been greeted/booted", read by
// BOTH the greet-once gate here AND restart-context arbitration.
func workspaceHasGreeted(ctx context.Context, workspaceID string) (bool, error) {
	var greeted bool
	err := db.DB.QueryRowContext(ctx,
		`SELECT has_greeted FROM workspaces WHERE id = $1`, workspaceID).Scan(&greeted)
	return greeted, err
}

// firstBootToolCount extracts the concierge tool count stamped by
// buildFirstBootGreetPayload. Checks params.metadata first (runtime-stamped
// primary) then params.message.metadata (platform-stamped) — mirroring the
// dual-location convention the greet-once/self-source readers use. Returns 0
// when absent or unparseable (the role-agnostic fallback, safe default). A JSON
// number decodes as float64 through interface{}, so accept that shape too.
func firstBootToolCount(reqBody []byte) int {
	var body struct {
		Params struct {
			Metadata struct {
				FirstBootToolCount *float64 `json:"first_boot_tool_count"`
			} `json:"metadata"`
			Message struct {
				Metadata struct {
					FirstBootToolCount *float64 `json:"first_boot_tool_count"`
				} `json:"metadata"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(reqBody, &body); err != nil {
		return 0
	}
	if body.Params.Metadata.FirstBootToolCount != nil {
		return int(*body.Params.Metadata.FirstBootToolCount)
	}
	if body.Params.Message.Metadata.FirstBootToolCount != nil {
		return int(*body.Params.Message.Metadata.FirstBootToolCount)
	}
	return 0
}

// deliverFirstBootGreeting is the SINGLE delivery seam for the first-boot
// greeting, shared by the synchronous greet path (FirstBootGreeter), the
// busy-queued drain path (deliverDrainedFirstBootGreeting), and the
// terminal-failure fallback (deliverFirstBootFallbackOnTerminalDrop). It
// enforces claim-on-delivery via the has_greeted boot marker (SSOT):
//   - CLAIM the marker atomically (claimGreetDelivery). If we did not win the
//     claim, another wake already greeted — skip silently (no double-greet).
//   - SEND only when we won the claim. On Send failure, ROLL BACK the claim
//     (releaseGreetClaim) so a future wake retries — the greeting never reached
//     the user, and commit-on-delivery must never leave the marker durably true
//     for an undelivered greeting.
func deliverFirstBootGreeting(ctx context.Context, writer *AgentMessageWriter, workspaceID, text string) error {
	won, err := claimGreetDelivery(ctx, workspaceID)
	if err != nil {
		return err
	}
	if !won {
		log.Printf("first-boot greeting: has_greeted already claimed for %s — another wake greeted (dedup, no re-greet)", workspaceID)
		return nil
	}
	if err := writer.Send(ctx, workspaceID, text, nil); err != nil {
		// Roll the claim back — the greeting did NOT reach the user, so a
		// future wake must be free to re-claim and retry.
		releaseGreetClaim(ctx, workspaceID)
		return err
	}
	return nil
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

		// Hold the platform-boot-turn gate for the greeting's duration: the
		// A2A proxy queues (rather than dispatches) caller turns while it is
		// up, and the queue drain waits on it. Without this, a user who types
		// the instant the workspace flips online races the in-character
		// greeting turn — hermes interrupts it and the caller's "answer" is
		// the "⚡ Interrupting current task…" ack (staging e2e failure,
		// 2026-07-20). The greeting itself dispatches as a system caller,
		// which passes the gate.
		markRestartContextPending(workspaceID)
		defer clearRestartContextPending(workspaceID)

		ctx, cancel := context.WithTimeout(context.Background(), firstBootGreetingTimeout)
		defer cancel()

		// Greet-once gate (RFC concierge rule 2): read the has_greeted boot
		// marker — the single authoritative "has this box been greeted" signal
		// (SSOT), the SAME marker restart-context arbitrates on. This is a
		// fast-path skip; the AUTHORITATIVE dedup is the atomic claim at delivery
		// (claimGreetDelivery), so a greeted box never re-greets and a failed
		// first boot still greets. Fail CLOSED on a DB error (skip) — a duplicate
		// greeting after every reconnect is worse than a missed one.
		greeted, err := workspaceHasGreeted(ctx, workspaceID)
		if err != nil {
			log.Printf("first-boot greeting: has_greeted check failed for %s (skipping): %v", workspaceID, err)
			return
		}
		if greeted {
			return
		}

		// Ask the agent to greet in its own voice. logActivity=false — the
		// writer below is the single chat entry point (no duplicate rows). The
		// toolCount is stamped into the payload metadata so a busy-queued drain
		// (or its terminal-failure fallback) keeps the concierge tool-count text.
		text := ""
		if runTurn != nil {
			if payload, err := buildFirstBootGreetPayload(workspaceID, toolCount); err == nil {
				status, resp, turnErr := runTurn(ctx, workspaceID, payload, "system:first-boot-greeting", false)
				queued, _ := QueuedA2AResponse(resp)
				switch {
				case turnErr != nil || status >= 300:
					log.Printf("first-boot greeting: agent turn failed for %s (status=%d, err=%v) — using fallback text", workspaceID, status, turnErr)
				case queued:
					// The greet turn was busy/poll queued: the proxy accepted it
					// but has NOT answered yet. The agent's REAL in-character
					// reply is produced later and delivered by the queue drain
					// (attachQueuedTurnCompletion's self-first-boot-greet
					// exception routes it to AgentMessageWriter) or, for a
					// poll-mode workspace, by the agent's own /notify. The drained
					// reply — NOT this ack — becomes the greeting, so send NOTHING
					// now: firing the static fallback here would race the real
					// reply and double-greet.
					log.Printf("first-boot greeting: greet turn queued for %s — the agent's own drained reply will greet", workspaceID)
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
		// must not starve the guaranteed (fallback) delivery of context. The
		// seam claims has_greeted BEFORE Send (claim-on-delivery) and rolls the
		// claim back if Send fails, so the marker is the authoritative dedup.
		sendCtx, cancelSend := context.WithTimeout(context.Background(), greetSendTimeout)
		defer cancelSend()
		if err := deliverFirstBootGreeting(sendCtx, writer, workspaceID, text); err != nil {
			log.Printf("first-boot greeting: send failed for %s: %v", workspaceID, err)
			return
		}
		log.Printf("first-boot greeting: delivered to %s (in-character=%v)", workspaceID, runTurn != nil && text != firstBootFallbackText(toolCount))
	}
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

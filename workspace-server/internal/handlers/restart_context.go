// Package handlers — restart_context.go implements Layer 1 of issue #19:
// after a workspace is restarted and comes back online, the platform
// generates a state snapshot (timestamp, previous session end, env-var
// keys now available) and delivers it as a synthetic A2A message/send
// so the agent sees what changed across the restart boundary.
//
// Layer 2 (user-defined restart_prompt via config.yaml / org.yaml) is
// out of scope for this file — tracked as a separate follow-up issue.
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/google/uuid"
)

// restartContextPending holds the workspaces whose post-restart boot turn has
// been SCHEDULED but not yet finished. It exists to make that boot turn
// EXCLUSIVE: an agent has one session, and while the platform is feeding it
// the restart-context message, no caller's turn may be dispatched into the
// same session.
//
// The race it closes (ephemeral-CP gate run 493034, and the class the #4147
// comment in a2a_proxy_helpers.go predicted):
//
//	20:44:50  A2AQueue: enqueued <id> for <ws>      (agent busy — restarting)
//	20:44:54  A2AQueue drain: DISPATCHING <id> → agent
//	20:44:55  restart-context: delivered to <ws>    ← 1s AFTER the dispatch
//	20:45:06  drain returns → caller's reply is the RESTART-CONTEXT's answer:
//	          "Workspace restarted and ready. What would you like me to help with?"
//
// Both the drain (registry.Heartbeat → DrainQueueForWorkspace, gated on
// ActiveTasks < maxConcurrent) and sendRestartContext (gated on online + fresh
// heartbeat) are woken by the SAME heartbeat, with nothing ordering them. The
// agent's single session then serves the two overlapping turns, and the caller's
// POST comes back holding the boot turn's text. The caller cannot tell it was
// not answered — a wrong answer, silently. That is strictly worse than a retry,
// which is the whole reason the queue exists.
//
// So: mark BEFORE the goroutine is spawned (a mark inside it would leave exactly
// the window we are closing), and clear on EVERY exit path (deliver, drop, or
// panic) via defer. The queued item is not lost while the gate is up — it stays
// queued and drains on the next heartbeat, once the agent is genuinely idle.
// sendRestartContext is bounded by its own context timeout, so the gate cannot
// wedge the queue indefinitely.
var restartContextPending sync.Map // workspaceID -> struct{}

func markRestartContextPending(workspaceID string) {
	restartContextPending.Store(workspaceID, struct{}{})
}

func clearRestartContextPending(workspaceID string) {
	restartContextPending.Delete(workspaceID)
}

// restartContextInFlight reports whether a post-restart boot turn is scheduled
// or running for workspaceID. The A2A queue drain consults this so a caller's
// turn never interleaves with the platform's own.
func restartContextInFlight(workspaceID string) bool {
	_, ok := restartContextPending.Load(workspaceID)
	return ok
}

// restartContextOnlineTimeout bounds how long we wait for a workspace
// to re-register after restart before dropping the context message.
// The Restart HTTP handler has already returned 200 by the time this
// waiter runs, so a timeout here is purely a best-effort skip.
const restartContextOnlineTimeout = 30 * time.Second

// restartContextOnlinePollInterval is the poll cadence while waiting
// for WORKSPACE_ONLINE. 500ms keeps the typical-case latency low
// without hammering Postgres.
const restartContextOnlinePollInterval = 500 * time.Millisecond

// restartContextData captures the platform-computed snapshot that will
// be rendered into a human-readable message. Keeping it as a struct
// (rather than building the string inline) makes the builder
// unit-testable without stubbing time/DB calls.
type restartContextData struct {
	RestartAt     time.Time
	PrevSessionAt time.Time // zero value = no prior session recorded
	EnvKeys       []string  // sorted list of env-var keys (no values)
}

// buildRestartContextMessage renders the restart context into the
// exact format proposed in issue #19. Fields that have no data (e.g.
// first-ever session) are rendered with a neutral placeholder so the
// agent always sees a consistent shape.
func buildRestartContextMessage(d restartContextData) string {
	msg := "=== WORKSPACE RESTART CONTEXT ===\n"
	msg += fmt.Sprintf("Restart at: %s\n", d.RestartAt.UTC().Format(time.RFC3339))

	if d.PrevSessionAt.IsZero() {
		msg += "Previous session ended: (no prior session on record)\n"
	} else {
		delta := d.RestartAt.Sub(d.PrevSessionAt)
		msg += fmt.Sprintf("Previous session ended: %s (%s ago)\n",
			d.PrevSessionAt.UTC().Format(time.RFC3339),
			humanDuration(delta))
	}

	if len(d.EnvKeys) == 0 {
		msg += "Env vars now available: (none)\n"
	} else {
		msg += fmt.Sprintf("Env vars now available: %s\n", joinStrings(d.EnvKeys, ", "))
	}

	msg += "=== END RESTART CONTEXT ===\n"
	return msg
}

// humanDuration formats a duration for display in the restart context.
// Keeps the output terse ("2h14m", "38s") without pulling in a
// humanize library. Negative/zero deltas render as "0s".
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// joinStrings is strings.Join — inlined to avoid an import cycle
// concern in a file that already carries a handful of stdlib deps.
func joinStrings(parts []string, sep string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	n := len(sep) * (len(parts) - 1)
	for i := 0; i < len(parts); i++ {
		n += len(parts[i])
	}
	b := make([]byte, 0, n)
	b = append(b, parts[0]...)
	for _, p := range parts[1:] {
		b = append(b, sep...)
		b = append(b, p...)
	}
	return string(b)
}

// loadRestartContextData gathers the snapshot inputs from the DB.
// Called *before* the restart mutates workspace state so the "previous
// session ended" timestamp reflects the pre-restart heartbeat, not the
// newly-provisioning row.
func loadRestartContextData(ctx context.Context, workspaceID string) restartContextData {
	d := restartContextData{RestartAt: time.Now()}

	var lastHB sql.NullTime
	if err := db.DB.QueryRowContext(ctx,
		`SELECT last_heartbeat_at FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&lastHB); err == nil && lastHB.Valid {
		d.PrevSessionAt = lastHB.Time
	}

	// Env-var keys: union of global secrets + workspace-specific
	// secrets. Values are NEVER included — only keys — so the agent
	// can reason about "did my missing credential arrive?" without
	// the platform ever echoing secret material back into the
	// message bus.
	keySet := map[string]struct{}{}
	if rows, err := db.DB.QueryContext(ctx, `SELECT key FROM global_secrets`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var k string
			if rows.Scan(&k) == nil {
				keySet[k] = struct{}{}
			}
		}
		if err := rows.Err(); err != nil {
			log.Printf("loadRestartContextData: global_secrets rows.Err: %v", err)
		}
	}
	if rows, err := db.DB.QueryContext(ctx,
		`SELECT key FROM workspace_secrets WHERE workspace_id = $1`, workspaceID,
	); err == nil {
		defer rows.Close()
		for rows.Next() {
			var k string
			if rows.Scan(&k) == nil {
				keySet[k] = struct{}{}
			}
		}
		if err := rows.Err(); err != nil {
			log.Printf("loadRestartContextData: workspace_secrets rows.Err: %v", err)
		}
	}
	for k := range keySet {
		d.EnvKeys = append(d.EnvKeys, k)
	}
	sort.Strings(d.EnvKeys)
	return d
}

// waitForWorkspaceOnline polls the workspaces table until the target
// workspace's status flips to 'online' or the deadline expires.
// Returns true on success; callers log+drop on false.
func waitForWorkspaceOnline(ctx context.Context, workspaceID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var status string
		if err := db.DB.QueryRowContext(ctx,
			`SELECT status FROM workspaces WHERE id = $1`, workspaceID,
		).Scan(&status); err == nil && status == "online" {
			return true
		}
		timer := time.NewTimer(restartContextOnlinePollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return false
}

// waitForFreshHeartbeat polls until the workspace has BOTH a non-empty
// url AND a last_heartbeat_at strictly after restartStartTs (i.e. the
// heartbeat we observe is NEW, not the stale pre-restart one carried
// across through the row update). Returns false on timeout or DB error.
//
// This is the Layer 2 gate for the 2026-05-19 ws-server self-fire restart
// loop fix. status='online' can flip while url=” is still in place (the
// status update happens in /registry/register; url is set at the same
// time but the read here may see a transient interleaving) and pre-fix
// the trailing restart-context probe could fire against a half-registered
// row, triggering the upstream-502 → maybeMarkContainerDead → self-fire
// chain we're closing. The url + heartbeat-freshness check is the
// strict, correlated end-state assertion that says "the new container is
// actually addressable" — not just "some heartbeat happened".
func waitForFreshHeartbeat(ctx context.Context, workspaceID string, restartStartTs time.Time, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var url sql.NullString
		var lastHB sql.NullTime
		err := db.DB.QueryRowContext(ctx,
			`SELECT url, last_heartbeat_at FROM workspaces WHERE id = $1`, workspaceID,
		).Scan(&url, &lastHB)
		if err == nil &&
			url.Valid && url.String != "" &&
			lastHB.Valid && lastHB.Time.After(restartStartTs) {
			return true
		}
		timer := time.NewTimer(restartContextOnlinePollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return false
}

// buildRestartA2APayload wraps the rendered context string in the
// JSON-RPC 2.0 / A2A message/send shape that the proxy already knows
// how to normalize. Returns the marshalled body ready for ProxyA2ARequest.
func buildRestartA2APayload(text string) ([]byte, error) {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      uuid.New().String(),
		"method":  "message/send",
		"params": map[string]any{
			"message": map[string]any{
				"messageId": uuid.New().String(),
				"role":      "user",
				"parts":     []any{map[string]any{"kind": "text", "text": text}},
				"metadata": map[string]any{
					"source": "platform",
					"kind":   "restart_context",
					// SSOT self-message classifier (messagestore.selfSourceTypes):
					// renders as a system notice, never a blue user bubble —
					// required since the durable-enqueue path routes this
					// through the ordinary ingest persist.
					"source_type":     "self-restart-context",
					"layer":           1,
					"restart_context": true,
				},
			},
		},
	}
	return json.Marshal(payload)
}

// sendRestartContext is called by the Restart handler in a background
// goroutine. It waits for the workspace to come online, then delivers
// the snapshot via the existing A2A proxy. Failures are logged and
// dropped — the restart itself is already considered successful at
// this point.
func (h *WorkspaceHandler) sendRestartContext(workspaceID string, data restartContextData) {
	// Release the drain gate on EVERY exit path — delivered, dropped, or
	// panicking. A leaked gate would stall this workspace's queue until the
	// process restarts, so this defer is load-bearing, not hygiene.
	// (The gate is SET by the caller, before the goroutine is spawned.)
	defer clearRestartContextPending(workspaceID)

	// Detach from any request context — this runs after the HTTP
	// response is flushed.
	ctx, cancel := context.WithTimeout(context.Background(), restartContextOnlineTimeout+30*time.Second)
	defer cancel()

	if !waitForWorkspaceOnline(ctx, workspaceID, restartContextOnlineTimeout) {
		// Do NOT drop: a first boot that builds a runtime image takes many
		// minutes (10m+ observed), far past any reasonable inline wait, and a
		// dropped context message is exactly the "agent forgot what it was
		// doing after the plugin-install restart" failure (2026-07-19). Durably
		// enqueue instead — the a2a queue drains on the workspace's first
		// heartbeat, and the expiry keeps a truly-dead workspace from replaying
		// a stale snapshot hours later.
		h.enqueueRestartContext(ctx, workspaceID, data, "online-wait timeout")
		return
	}
	// Self-fire guard (Layer 2 of the 2026-05-19 ws-server self-fire fix):
	// status='online' alone is not enough to safely fire the trailing
	// ProxyA2ARequest. The workspace must also have:
	//   - url != ''                            (the new container's URL has been registered)
	//   - last_heartbeat_at > data.RestartAt   (the heartbeat we're seeing is NEW, not stale)
	// Without those, ProxyA2ARequest can fail with a connect error or
	// upstream 502, hit handleA2ADispatchError → maybeMarkContainerDead →
	// RestartByID → self-fire. The Layer 1 isRestarting gate already
	// covers that, but this is a belt-and-suspenders so the probe never
	// even tries until the new container is actually addressable. Best-
	// effort: if the DB read errors out we proceed (preserves the legacy
	// behaviour of "online means online").
	if !waitForFreshHeartbeat(ctx, workspaceID, data.RestartAt, restartContextOnlineTimeout) {
		// Deliberate DROP, not enqueue: this arm means the workspace looks
		// online but no post-restart heartbeat has arrived — possibly the OLD
		// container is still heartbeating. A queued snapshot would be drained
		// by that old instance's next heartbeat (the queue drain re-checks
		// neither url freshness nor heartbeat > RestartAt), delivering "you
		// just restarted" to the pre-restart container and leaving the real
		// new instance with nothing.
		log.Printf("restart-context: workspace %s online but no fresh heartbeat or empty url — dropping context message (self-fire guard)", workspaceID)
		return
	}

	text := buildRestartContextMessage(data)
	body, err := buildRestartA2APayload(text)
	if err != nil {
		log.Printf("restart-context: failed to marshal payload for %s: %v", workspaceID, err)
		return
	}

	// "system:restart-context" prefix flags this as a trusted
	// non-workspace caller — bypasses CanCommunicate and the
	// caller-token check in a2a_proxy.go.
	status, _, proxyErr := h.ProxyA2ARequest(ctx, workspaceID, body, "system:restart-context", false)
	if proxyErr != nil {
		// Deliberate DROP, not enqueue: a proxy error does NOT prove the
		// message went undelivered — a response-read timeout after the agent
		// already received the turn is common on this path, and enqueueing
		// would double-deliver the snapshot (the idempotency key dedupes
		// concurrent enqueues, not push-then-queue). At-most-once beats
		// at-least-once for a context nudge.
		log.Printf("restart-context: ProxyA2ARequest failed for %s (status=%d): %v", workspaceID, status, proxyErr)
		return
	}
	log.Printf("restart-context: delivered to %s (status=%d, keys=%d)", workspaceID, status, len(data.EnvKeys))
}

// restartContextQueueTTL bounds how long an undelivered restart-context
// snapshot stays meaningful. Long enough to cover a first-boot runtime image
// build (10m+ observed); short enough that a workspace revived much later
// doesn't get a confusing stale "you just restarted" prompt.
const restartContextQueueTTL = 30 * time.Minute

// enqueueRestartContext durably buffers the restart-context snapshot in the
// a2a queue (drained on the workspace's heartbeat) instead of dropping it.
// The idempotency key is derived from the restart timestamp so retries of the
// same restart collapse to one queued message.
func (h *WorkspaceHandler) enqueueRestartContext(ctx context.Context, workspaceID string, data restartContextData, why string) {
	body, err := buildRestartA2APayload(buildRestartContextMessage(data))
	if err != nil {
		log.Printf("restart-context: failed to marshal payload for %s: %v", workspaceID, err)
		return
	}
	// The delivery ctx may already be exhausted (that can be WHY we are here);
	// the enqueue must not inherit its deadline.
	qCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	expires := time.Now().Add(restartContextQueueTTL)
	// Keyed per WORKSPACE, not per restart: rapid consecutive restarts (e.g.
	// image-build failures then success) must collapse to ONE queued context
	// snapshot — three restarts on 2026-07-19 delivered three stacked wake
	// messages into the same session. EnqueueA2A's active-row conflict keeps
	// the first pending snapshot; content staleness across a few minutes is
	// harmless (the message is a generic "you restarted" nudge).
	idem := "restart-context-" + workspaceID
	// Priority 90 (> default 50; drain is ORDER BY priority DESC) so the
	// context snapshot is dispatched BEFORE any queued user message in the
	// same drain pass — the boot turn should precede the user's turn, which
	// is the ordering the push path's exclusivity gate produced. (Two
	// CONCURRENT drains can still interleave rows via SKIP LOCKED; that is a
	// pre-existing property of the queue for all items, not new exposure.)
	if _, _, qErr := h.EnqueueA2A(qCtx, workspaceID, "system:restart-context", 90, body, "message/send", idem, &expires); qErr != nil {
		log.Printf("restart-context: enqueue failed for %s (%s): %v — context message lost", workspaceID, why, qErr)
		return
	}
	log.Printf("restart-context: %s for %s — queued for heartbeat drain (ttl=%s)", why, workspaceID, restartContextQueueTTL)
}

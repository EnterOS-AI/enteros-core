package handlers

// a2a_queue.go — #1870 Phase 1: enqueue A2A requests whose target is busy,
// drain the queue on heartbeat when the target regains capacity.
//
// Three levels are declared here so Phase 2/3 can land without a migration:
//   - PriorityCritical = 100 — preempts running task (Phase 3, not active yet)
//   - PriorityTask     = 50  — default, FIFO within priority (Phase 1, active)
//   - PriorityInfo     = 10  — best-effort with TTL (Phase 2, not active yet)
//
// Phase 1 writes only PriorityTask. The `priority` column tolerates all three.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/textutil"
)

// extractIdempotencyKey pulls params.message.messageId out of an A2A JSON-RPC
// body (normalizeA2APayload guarantees this field is set before dispatch).
// Empty string on parse failure — callers treat that as "no idempotency".
func extractIdempotencyKey(body []byte) string {
	var envelope struct {
		Params struct {
			Message struct {
				MessageID string `json:"messageId"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	return envelope.Params.Message.MessageID
}

// extractExpiresInSeconds pulls params.expires_in_seconds out of an A2A
// JSON-RPC body and returns it as a positive integer. A zero return means
// "no caller-specified TTL" — caller should leave expires_at NULL on the
// queue row, preserving today's infinite-TTL behaviour (the
// DropStaleQueueItems admin sweeper still drops entries past the
// platform-default age). Negative values and parse errors collapse to 0.
//
// Why params-level (not metadata): expires_in_seconds is a delivery
// directive, not a peer-to-peer message attribute. Putting it under
// `params` keeps it adjacent to other delivery hints (priority,
// idempotency) and out of `params.message.metadata` which the receiving
// agent can read.
func extractExpiresInSeconds(body []byte) int {
	var envelope struct {
		Params struct {
			ExpiresInSeconds interface{} `json:"expires_in_seconds"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0
	}
	var seconds int
	switch v := envelope.Params.ExpiresInSeconds.(type) {
	case float64:
		seconds = int(v)
	default:
		return 0
	}
	if seconds < 0 {
		return 0
	}
	return seconds
}

const (
	PriorityCritical = 100
	PriorityTask     = 50
	PriorityInfo     = 10
)

// A2A queue sweeper constants (#2930). The sweeper is an independent periodic
// drain fallback so that queued requests are not stranded when a workspace
// stops heartbeating (e.g., after the restart-trigger in #2929).
const (
	a2aQueueSweeperInterval    = 10 * time.Second
	a2aQueueSweeperBatchCap    = 8
	a2aQueueSweeperStatusAlert = 10 // log a warning every N stranded items
)

// transientRetryBackoffSecs is how long a MarkQueueItemTransientRetry
// row remains ineligible for re-dispatch, expressed in seconds (the
// integer form that PostgreSQL's make_interval(secs => $N) accepts).
//
// #3127 follow-up (Researcher REQUEST_CHANGES) — the transient-retry
// path requeues with status='queued' but the same DrainQueueForWorkspace
// for-loop can iterate up to capacity times. Without this backoff, a
// capacity>1 drain would re-claim the just-requeued row on the next
// iteration and hit the same gateway failure again, in a tight loop.
// 5s is long enough to break that loop (sweeper interval is 10s,
// heartbeats typically every 5-30s) and short enough that recovery on
// the next heartbeat is not perceptibly delayed.
const transientRetryBackoffSecs = 5

// QueuedItem is what the heartbeat drain path pulls off the queue.
type QueuedItem struct {
	ID          string
	WorkspaceID string
	CallerID    sql.NullString
	Priority    int
	Body        []byte
	Method      sql.NullString
	Attempts    int
	EnqueuedAt  time.Time
	// SettlingSince is when this item first entered the transient gateway-origin
	// retry path (NULL until then). The settling ceiling is measured from here,
	// not EnqueuedAt — see a2aSettlingRetryCeiling.
	SettlingSince sql.NullTime
}

// a2aSettlingRetryCeiling bounds the total wall-clock a single queued A2A turn
// may spend in the NON-cap-burning transient-retry path (a gateway-origin
// failure while the target is still heartbeating — see DrainQueueForWorkspace).
// That path deliberately does NOT burn the 5-attempt terminal cap so a brief
// blip can't strand a legitimate turn. But with NO other ceiling, a PERSISTENTLY
// settling target re-queues FOREVER: a force-hibernated box that was woken onto a
// fresh container while the in-flight turn still targets the dead one, or any
// workspace whose forward path stays broken while it keeps heartbeating. Each
// re-delivery attempt pokes the runtime and resets its idle clock, so the
// idle-digest (and every other idle-gated behaviour) is starved indefinitely —
// the RCA of the nondeterministic ephemeral happy-path gate (run 533322:
// digest armed=3, fired=0; task #124/#94). Past this ceiling we stop treating the
// failure as transient and DROP the turn (terminally, with a caller-visible
// reason) so it can never zombie-requeue. Sized generously (well beyond a normal
// hibernate wake, which settles in ~15-30s) so a merely-slow-but-recovering
// target is not dropped. Env override: A2A_SETTLING_RETRY_CEILING_S.
var a2aSettlingRetryCeiling = envDuration("A2A_SETTLING_RETRY_CEILING_S", 180*time.Second)

// EnqueueA2A inserts a busy-retry-eligible A2A request into a2a_queue and
// returns the new row ID + current queue depth. Caller MUST have already
// determined the target is busy — this function does not check.
//
// Idempotency: when idempotencyKey is non-empty, a duplicate active enqueue
// for the same (workspace, key) is collapsed rather than double-buffered. On
// a duplicate this returns the existing row's ID so the caller's log still
// points at the live queue entry.
func EnqueueA2A(
	ctx context.Context,
	workspaceID, callerID string,
	priority int,
	body []byte,
	method, idempotencyKey string,
	expiresAt *time.Time,
) (id string, depth int, err error) {
	var keyArg interface{}
	if idempotencyKey != "" {
		keyArg = idempotencyKey
	}
	// Normalize the callerID the same way nilIfEmpty does in
	// a2a_proxy_helpers.go: system-caller prefixes (webhook:,
	// system:, test:, channel:) are non-UUID routing markers, not real
	// workspace ids. Persisting them to a2a_queue.caller_id (a
	// UUID-typed column per migrations/042_a2a_queue.up.sql:21) would
	// trip a Postgres UUID cast failure → "invalid input syntax for
	// type uuid" → EnqueueA2A returns an error → the busy-A2A path
	// falls through to a 503 instead of queueing. See #2694 RC
	// #99248 for the symptom + #2693 for the broader #2680 lineage.
	//
	// Real workspace UUIDs are passed through unchanged so the
	// queue-row attribution is preserved.
	var callerArg interface{}
	if callerID != "" && !isSystemCaller(callerID) {
		callerArg = callerID
	}
	var methodArg interface{}
	if method != "" {
		methodArg = method
	}
	// expiresAtArg stays NULL when caller didn't specify a TTL. DequeueNext's
	// `expires_at IS NULL OR expires_at > now()` filter then preserves today's
	// infinite-TTL semantics for un-flagged messages.
	var expiresAtArg interface{}
	if expiresAt != nil {
		expiresAtArg = *expiresAt
	}

	// Supersede any already-expired pending row for this same key before we
	// insert. The drain path skips expired pending rows, so such a row never
	// completes on its own — it lingers in the active set and would block the
	// conflict check below, silently swallowing this fresh enqueue. Retiring
	// it here (a) frees the active set so the insert below proceeds and (b)
	// cleans the stale row up so expired rows don't accumulate. Scoped to the
	// idempotency key so unrelated traffic is untouched.
	if idempotencyKey != "" {
		if _, supErr := db.DB.ExecContext(ctx, `
			UPDATE a2a_queue
			SET status = 'dropped',
			    last_error = 'superseded: expired before drain; replaced by a fresh enqueue'
			WHERE workspace_id = $1
			  AND idempotency_key = $2
			  AND status = 'queued'
			  AND expires_at IS NOT NULL
			  AND expires_at <= now()
		`, workspaceID, idempotencyKey); supErr != nil {
			// Non-fatal: if the cleanup fails we still attempt the insert. Worst
			// case the conflict path returns the (stale) existing row's id, which
			// is the pre-fix behaviour — no new breakage introduced here.
			log.Printf("A2AQueue: supersede-expired cleanup failed for workspace %s key %s: %v",
				workspaceID, idempotencyKey, supErr)
		}
	}

	// INSERT ... ON CONFLICT DO NOTHING RETURNING id. The conflict target
	// must reference the partial unique INDEX columns + WHERE clause directly
	// (Postgres can't reference partial unique indexes by name in
	// ON CONFLICT — only true CONSTRAINTs work for that). On conflict we
	// then look up the existing row's id so the caller always receives a
	// valid queue entry reference.
	//ssot:allow-status-set a2a_queue is a DIFFERENT table with its own, smaller status
	// vocabulary — CHECK (status IN ('queued','dispatched','completed','dropped','failed')).
	// It has no in_progress and no stuck. This is that table's "still active" set, not a
	// copy of the delegations vocabulary, and it must NOT be derived from
	// DelegationInFlightStates: doing so would silently widen a2a_queue's idempotency
	// predicate to states its own CHECK constraint forbids. Conflating these two
	// vocabularies is what produced #4314 in the first place.
	err = db.DB.QueryRowContext(ctx, `
		INSERT INTO a2a_queue (workspace_id, caller_id, priority, body, method, idempotency_key, expires_at)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7)
		ON CONFLICT (workspace_id, idempotency_key)
			WHERE idempotency_key IS NOT NULL AND status IN ('queued','dispatched')
			DO NOTHING
		RETURNING id
	`, workspaceID, callerArg, priority, string(body), methodArg, keyArg, expiresAtArg).Scan(&id)

	if errors.Is(err, sql.ErrNoRows) && idempotencyKey != "" {
		// Conflict — look up the existing active row and use its id.
		//ssot:allow-status-set a2a_queue's own active-row set — see the INSERT above.
		err = db.DB.QueryRowContext(ctx, `
			SELECT id FROM a2a_queue
			WHERE workspace_id = $1 AND idempotency_key = $2
			  AND status IN ('queued','dispatched')
			LIMIT 1
		`, workspaceID, idempotencyKey).Scan(&id)
		if err != nil {
			return "", 0, err
		}
	} else if err != nil {
		return "", 0, err
	}

	// Return current queue depth for the caller's visibility.
	if err := db.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM a2a_queue
		WHERE workspace_id = $1 AND status = 'queued'
	`, workspaceID).Scan(&depth); err != nil {
		log.Printf("A2AQueue: depth query failed for workspace %s: %v", workspaceID, err)
	}

	log.Printf("A2AQueue: enqueued %s for workspace %s (priority=%d, depth=%d)", id, workspaceID, priority, depth)
	return id, depth, nil
}

// DequeueNext claims the next queued item for a workspace and marks it
// 'dispatched'. Uses SELECT ... FOR UPDATE SKIP LOCKED so two concurrent
// drain calls don't both claim the same row.
//
// Honors a per-row next_attempt_at backoff (added in #3127 follow-up
// migration 20260621120000). Rows whose next_attempt_at is in the future
// are SKIPPED — they remain 'queued' but are not eligible for dispatch
// until the backoff expires. This is the gate that breaks the
// capacity>1 tight-retry loop on a flapping gateway: when
// MarkQueueItemTransientRetry sets next_attempt_at = now() + 5s, the
// same for-loop iteration that just requeued the row cannot re-dequeue
// it on the very next iteration even if the row is still highest
// priority.
//
// Returns (nil, nil) when the queue is empty (or all eligible rows are
// backoff-gated) — not an error.
func DequeueNext(ctx context.Context, workspaceID string) (*QueuedItem, error) {
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var item QueuedItem
	var body string
	err = tx.QueryRowContext(ctx, `
		SELECT id, workspace_id, caller_id, priority, body::text, method, attempts, enqueued_at, settling_since
		FROM a2a_queue
		WHERE workspace_id = $1 AND status = 'queued'
		  AND (expires_at IS NULL OR expires_at > now())
		  AND (next_attempt_at IS NULL OR next_attempt_at <= now())
		ORDER BY priority DESC, enqueued_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`, workspaceID).Scan(
		&item.ID, &item.WorkspaceID, &item.CallerID, &item.Priority,
		&body, &item.Method, &item.Attempts, &item.EnqueuedAt, &item.SettlingSince,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	item.Body = []byte(body)

	if _, err := tx.ExecContext(ctx, `
		UPDATE a2a_queue
		SET status = 'dispatched', dispatched_at = now(), attempts = attempts + 1
		WHERE id = $1
	`, item.ID); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &item, nil
}

// MarkQueueItemCompleted flips the queue row to 'completed' on a successful
// drain dispatch. responseBody is persisted so callers polling
// GET /workspaces/:id/a2a/queue/:queue_id can retrieve the actual agent reply
// for non-delegation A2A queue items (e.g. message/send that got queued because
// the target was busy). Pass nil when no payload exists (re-queued drain).
func MarkQueueItemCompleted(ctx context.Context, id string, responseBody []byte) {
	var respBody any
	if len(responseBody) > 0 {
		// Store as a JSONB value. The column accepts text that parses as JSON;
		// passing the raw bytes through the driver is driver-dependent, so we
		// hand it a string explicitly.
		respBody = string(responseBody)
	}
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE a2a_queue SET status = 'completed', completed_at = now(), response_body = $2 WHERE id = $1`,
		id, respBody,
	); err != nil {
		log.Printf("A2AQueue: failed to mark %s completed: %v", id, err)
	}
}

// MarkQueueItemFailed returns a dispatched item back to 'queued' with an
// incremented attempts counter so the next drain tick picks it up. Hits
// an upper bound (5 attempts) to avoid wedging a stuck item in the queue
// forever.
func MarkQueueItemFailed(ctx context.Context, id, errMsg string) {
	const maxAttempts = 5
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE a2a_queue
		SET status = CASE WHEN attempts >= $2 THEN 'failed' ELSE 'queued' END,
		    last_error = $3,
		    dispatched_at = NULL
		WHERE id = $1
	`, id, maxAttempts, errMsg); err != nil {
		log.Printf("A2AQueue: failed to mark %s failed: %v", id, err)
	}
}

// MarkQueueItemTransientRetry returns a dispatched item to 'queued' WITHOUT
// burning the 5-attempt terminal cap. Used by DrainQueueForWorkspace for
// transient gateway-origin failures (Cloudflare 502, push-route blip, "no
// healthy upstream") where the workspace is online and heartbeating — the
// failure is in the path BETWEEN the platform and the agent, not in the
// agent itself. The PM 2026-06-21 RCA caught that the previous behaviour
// (always MarkQueueItemFailed) consumed the cap on healthy workspaces and
// stranded queued requests until TTL.
//
// Mechanism: DequeueNext (line 256-262 of this file) increments `attempts`
// at dispatch time under FOR UPDATE SKIP LOCKED. MarkQueueItemTransientRetry
// undoes that increment so a transient retry does not advance the cap
// counter. The row stays in 'queued' status with dispatched_at = NULL, so
// the next sweep / heartbeat-drain picks it up naturally.
//
// Backoff (Researcher #3127 REQUEST_CHANGES follow-up): sets
// next_attempt_at = now() + make_interval(secs => transientRetryBackoffSecs)
// so the row is backoff-gated against re-dispatch for the window. This
// is the gate that prevents a capacity>1 DrainQueueForWorkspace from
// tight-looping on the same row (the just-requeued row would otherwise
// be eligible for re-claim on the very next for-loop iteration, and
// would hit the same gateway failure again without ever burning an
// attempt or being delayed). DequeueNext's WHERE clause skips rows
// whose next_attempt_at is still in the future. The seconds count is
// passed as a parameter (rather than inlined as `interval '5 seconds'`)
// so the transientRetryBackoff Go constant drives the SQL behavior
// directly — golangci-lint flagged the previous unused-const shape.
//
// Race-safety note: between DequeueNext's COMMIT and this UPDATE, the row
// is in 'dispatched' status, so a concurrent DequeueNext call (sweeper
// tick, second heartbeat in flight) cannot re-claim it. The status='queued'
// transition is the only window during which re-claim is possible, and it
// is bounded by the time this UPDATE takes to commit.
func MarkQueueItemTransientRetry(ctx context.Context, id, errMsg string) {
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE a2a_queue
		SET status = 'queued',
		    attempts = GREATEST(attempts - 1, 0),
		    last_error = $2,
		    dispatched_at = NULL,
		    next_attempt_at = now() + make_interval(secs => $3),
		    settling_since = COALESCE(settling_since, now())
		WHERE id = $1
	`, id, errMsg, transientRetryBackoffSecs); err != nil {
		log.Printf("A2AQueue: failed to mark %s for transient retry: %v", id, err)
	}
}

// DropQueueItemTerminal terminally drops a currently-dispatched item to
// 'dropped' with a caller-visible reason. Unlike MarkQueueItemFailed (whose
// 'failed' transition only fires once attempts>=cap — and the transient-retry
// path deliberately keeps attempts at 0), this ends the item unconditionally so
// a persistently-settling target cannot zombie-requeue it forever. The status
// guard pins the write to the row THIS drain dispatched, so a racing enqueue/
// completion is not clobbered.
func DropQueueItemTerminal(ctx context.Context, id, reason string) {
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE a2a_queue
		SET status = 'dropped',
		    last_error = $2,
		    dispatched_at = NULL,
		    completed_at = now()
		WHERE id = $1 AND status = 'dispatched'
	`, id, reason); err != nil {
		log.Printf("A2AQueue: failed to terminally drop %s: %v", id, err)
	}
}

// DropStaleQueueItems marks queued items older than maxAge as 'dropped' with a
// system-generated reason so PM agents stop processing stale post-incident noise.
// Called with a workspaceID to scope cleanup to one workspace, or empty to sweep
// all workspaces.
//
// Returns the number of items dropped for visibility/audit logging.
func DropStaleQueueItems(ctx context.Context, workspaceID string, maxAgeMinutes int) (int, error) {
	var rows int64
	var err error
	if workspaceID != "" {
		err = db.DB.QueryRowContext(ctx, `
			WITH dropped AS (
				UPDATE a2a_queue
				SET status = 'dropped',
				    last_error = last_error ||
				        E'\n[DropStaleQueueItems] auto-dropped: queue item age exceeded the post-incident TTL. '
				        || 'Dropped at ' || now()::text
				WHERE id IN (
					SELECT id FROM a2a_queue
					WHERE workspace_id = $1
					  AND status = 'queued'
					  AND enqueued_at < now() - interval '1 minute' * $2
					FOR UPDATE SKIP LOCKED
				)
				RETURNING id
			)
			SELECT count(*) FROM dropped
		`, workspaceID, maxAgeMinutes).Scan(&rows)
	} else {
		err = db.DB.QueryRowContext(ctx, `
			WITH dropped AS (
				UPDATE a2a_queue
				SET status = 'dropped',
				    last_error = last_error ||
				        E'\n[DropStaleQueueItems] auto-dropped: queue item age exceeded the post-incident TTL. '
				        || 'Dropped at ' || now()::text
				WHERE id IN (
					SELECT id FROM a2a_queue
					WHERE status = 'queued'
					  AND enqueued_at < now() - interval '1 minute' * $1
					FOR UPDATE SKIP LOCKED
				)
				RETURNING id
			)
			SELECT count(*) FROM dropped
		`, maxAgeMinutes).Scan(&rows)
	}
	if err != nil {
		return 0, fmt.Errorf("DropStaleQueueItems: %w", err)
	}
	return int(rows), nil
}

// DrainQueueForWorkspace pulls queued items (up to `capacity`) and dispatches
// each via the same ProxyA2ARequest path a live caller would use. Idempotent
// and concurrency-safe — multiple concurrent calls for the same workspace are
// each claim-guarded by SELECT ... FOR UPDATE SKIP LOCKED in DequeueNext.
//
// Called from the Heartbeat handler's goroutine when the workspace reports
// spare capacity, and from the periodic A2A queue sweeper as a fallback when
// heartbeats stop (#2930). Errors here are logged but not returned — callers
// are fire-and-forget goroutines.
//
// #2026-06-21 PM RCA: distinguish GATEWAY-ORIGIN failures (transient
// Cloudflare 502 / push-route blip / "no healthy upstream") from TRUE
// dead-agent failures. Healthy workspaces that happened to get a 502
// from the CDN were terminal-failing the queue item under the previous
// behaviour — MarkQueueItemFailed increments attempts each tick, so a
// transient blip that lasted 5 ticks would burn the cap and strand the
// request at 'failed'. Now: gateway-origin failures with a recent
// heartbeat invalidate the cached URL, re-queue via
// MarkQueueItemTransientRetry (which DOES NOT advance the 5-attempt
// counter), and let the next sweep retry. Only confirmed-dead agents
// (Classification="upstream_dead") or non-gateway failures continue
// through MarkQueueItemFailed.
func (h *WorkspaceHandler) DrainQueueForWorkspace(ctx context.Context, workspaceID string, capacity int) {
	if capacity <= 0 {
		return
	}
	// The agent has ONE session. While the platform's post-restart boot turn is
	// in flight, dispatching a caller's turn into that same session means the
	// caller's POST comes back holding the BOOT TURN's answer ("Workspace
	// restarted and ready...") — a wrong answer the caller cannot distinguish
	// from a right one. Hold off: the item stays queued and drains on the next
	// heartbeat, once the agent is genuinely idle. Nothing is lost, and a loud
	// wait beats a quiet lie. See restartContextPending (restart_context.go).
	if restartContextInFlight(workspaceID) {
		log.Printf("A2AQueue drain: workspace %s has a restart-context boot turn in flight — deferring drain to the next heartbeat", workspaceID)
		return
	}
	for i := 0; i < capacity; i++ {
		item, err := DequeueNext(ctx, workspaceID)
		if err != nil {
			log.Printf("A2AQueue drain: dequeue failed for %s: %v", workspaceID, err)
			return
		}
		if item == nil {
			return // queue empty, no work
		}

		callerID := ""
		if item.CallerID.Valid {
			callerID = item.CallerID.String
		}
		// Re-resolve the agent URL FRESH from the DB (source of truth) at EACH
		// drain attempt — never trust the Redis URL cache here. A force-
		// hibernate→wake re-provisions a NEW container with a NEW host/URL;
		// Hibernate and WakeWorkspace blank workspaces.url in the DB, but the
		// 5-minute Redis URL cache (and a late re-register from the dying pre-
		// hibernate container) can still hold the DEAD container's URL. If the
		// drain trusted that stale cache it would dial a host that no longer
		// exists ("dial tcp: lookup <old-host>: no such host"), the forward
		// would fail as upstream_dead, and — with no recent heartbeat on a
		// just-woken box — burn the 5-attempt cap on a doomed dial, stranding
		// the turn forever (core#124, ephemeral-gate run 815192).
		//
		// ClearCachedURL evicts ONLY the url key (not the liveness key), so the
		// resolveAgentURL below re-reads the DB and RESEEDS the cache with the
		// truth: the woken workspace's fresh container URL (→ dispatched to the
		// NEW box), or a URL-less settling/provisioning row (→ classified
		// workspace_settling and re-queued WITHOUT a stale-url dial, preserving
		// the settling-ceiling wall-clock semantics of core#4459). The
		// proxyA2ARequest below then resolves the SAME reseeded cache value.
		// resolveAgentURL swallows its own errors into a proxyA2AError, so a
		// resolution failure here is rare — usually a workspace with no URL
		// row. Empty string is fine for the log; the dispatch below will
		// produce the structured error and we already log it.
		db.ClearCachedURL(ctx, workspaceID)
		resolvedURL, _ := h.resolveAgentURL(ctx, workspaceID)
		log.Printf("A2AQueue drain: dispatching queue_id=%s workspace_id=%s url=%s attempt=%d",
			item.ID, workspaceID, resolvedURL, item.Attempts)

		// logActivity=false: the original EnqueueA2A callsite already logged
		// the dispatch attempt; re-logging here would double-count events.
		status, respBody, proxyErr := h.proxyA2ARequest(ctx, workspaceID, item.Body, callerID, false, false)

		// 202 Accepted = the dispatch was itself queued again (target still busy).
		// That's not a failure — the queued item just stays queued naturally on
		// the next drain tick. Mark this attempt completed so we don't double-
		// count attempts; the new (re-)queue row already exists.
		if status == http.StatusAccepted {
			MarkQueueItemCompleted(ctx, item.ID, nil)
			log.Printf("A2AQueue drain: queue_id=%s workspace_id=%s re-queued (target still busy)",
				item.ID, workspaceID)
			continue
		}

		if proxyErr != nil {
			// Defensive: proxyErr.Response is gin.H (map[string]interface{}). The
			// "error" key is conventionally a string but can be missing or non-
			// string in edge paths (e.g. a future error builder using a typed
			// struct). Cast safely so a missing key doesn't crash the platform —
			// today's outage was caused by an unchecked .(string) here.
			errMsg, _ := proxyErr.Response["error"].(string)
			if errMsg == "" {
				errMsg = http.StatusText(proxyErr.Status)
				if errMsg == "" {
					errMsg = "unknown drain dispatch error"
				}
			}
			classification := proxyErr.Classification

			// #2026-06-21 PM RCA: transient gateway-origin failure (CF 5xx,
			// push-route blip, "no healthy upstream") on a workspace that is
			// still heartbeating → re-queue without burning the 5-attempt cap.
			// The agent is alive; the path between us and the agent is not.
			// Invalidate the cached URL so the next retry re-resolves, and
			// hand off to MarkQueueItemTransientRetry which undoes the
			// DequeueNext attempts-increment.
			if isGatewayOriginFailure(proxyErr) && h.hasRecentHeartbeat(ctx, workspaceID) {
				// The transient path deliberately does NOT burn the 5-attempt cap.
				// But a target that has been settling far past the point a blip can
				// explain (a woken box on a fresh container while this turn targets
				// the dead one, or any persistently-broken forward path) would
				// re-queue FOREVER, poking the runtime on every attempt and starving
				// idle-gated work (task #124/#94). Bound it: past
				// a2aSettlingRetryCeiling, stop treating it as transient and DROP it.
				// Measure the settling window from settling_since (the FIRST
				// gateway-origin failure), NOT enqueued_at. A turn that merely sat
				// queued a long time (target offline) whose settling_since is still
				// NULL falls through to the transient retry below — which STAMPS
				// settling_since — so it is never dropped on its first blip; only a
				// SUSTAINED settling past the ceiling drops (core#4459 re-review [0]).
				if item.SettlingSince.Valid && time.Since(item.SettlingSince.Time) >= a2aSettlingRetryCeiling {
					settlingFor := time.Since(item.SettlingSince.Time)
					dropMsg := fmt.Sprintf("target failed to settle within %s of the first gateway-origin failure (persistent, last status=%d classification=%s: %s); dropped to stop a zombie re-queue that starves the workspace (core#124)",
						a2aSettlingRetryCeiling, proxyErr.Status, classificationOrUnknown(classification), errMsg)
					DropQueueItemTerminal(ctx, item.ID, dropMsg)
					// The stale/dead cached URL is what caused the drop — invalidate it
					// so the REMAINING queued items of this workspace in this same drain
					// re-resolve instead of all hammering the same dead URL (re-review [3]).
					h.invalidateCachedURLForDrain(ctx, workspaceID)
					// If this was a DELEGATION, terminalize its delegate_result to
					// 'failed' NOW so the caller's check_task_status is unblocked
					// immediately, instead of hanging until the 6h delegation-sweeper
					// deadline (re-review [1]).
					if delegationID := extractDelegationIDFromBody(item.Body); delegationID != "" {
						h.stitchDroppedDelegationFailure(ctx, callerID, item.WorkspaceID, delegationID, dropMsg)
					}
					log.Printf("A2AQueue drain: queue_id=%s workspace_id=%s url=%s SETTLING CEILING EXCEEDED "+
						"(settling_for=%s ceiling=%s status=%d classification=%s) — dropped to stop zombie re-queue",
						item.ID, workspaceID, resolvedURL, settlingFor.Truncate(time.Second), a2aSettlingRetryCeiling,
						proxyErr.Status, classificationOrUnknown(classification))
					continue
				}
				h.invalidateCachedURLForDrain(ctx, workspaceID)
				MarkQueueItemTransientRetry(ctx, item.ID,
					fmt.Sprintf("transient gateway origin (%s, status=%d): %s",
						classificationOrUnknown(classification), proxyErr.Status, errMsg))
				settledFor := time.Duration(0)
				if item.SettlingSince.Valid {
					settledFor = time.Since(item.SettlingSince.Time)
				}
				log.Printf("A2AQueue drain: queue_id=%s workspace_id=%s url=%s transient gateway failure "+
					"(status=%d classification=%s) — re-queued without burning attempt cap (attempts preserved at %d, settling_for=%s/%s)",
					item.ID, workspaceID, resolvedURL, proxyErr.Status, classificationOrUnknown(classification), item.Attempts,
					settledFor.Truncate(time.Second), a2aSettlingRetryCeiling)
				continue
			}

			MarkQueueItemFailed(ctx, item.ID, errMsg)
			log.Printf("A2AQueue drain: queue_id=%s workspace_id=%s url=%s dispatch failed "+
				"(attempt=%d status=%d classification=%s): %s",
				item.ID, workspaceID, resolvedURL, item.Attempts, proxyErr.Status,
				classificationOrUnknown(classification), errMsg)
			continue
		}
		MarkQueueItemCompleted(ctx, item.ID, respBody)
		log.Printf("A2AQueue drain: queue_id=%s workspace_id=%s url=%s dispatched (attempt=%d)",
			item.ID, workspaceID, resolvedURL, item.Attempts)

		// Stitch the response back to the originating delegation row, if this
		// queue item was a delegation. Without this, check_task_status would
		// see status='queued' (set by the executeDelegation queued-branch) and
		// the LLM would think the work was never done. We embed delegation_id
		// in params.message.metadata at Delegate-handler time; pull it out
		// here and UPDATE the delegate_result row so the original caller can
		// observe the real reply.
		if delegationID := extractDelegationIDFromBody(item.Body); delegationID != "" {
			h.stitchDrainResponseToDelegation(ctx, callerID, item.WorkspaceID, delegationID, respBody)
		}
	}
}

// classificationOrUnknown renders an empty proxyA2AError.Classification as
// the literal "unknown" so the structured drain log line never has an empty
// classification field — makes log-scrapers and human readers happier than
// trailing whitespace.
func classificationOrUnknown(c string) string {
	if c == "" {
		return "unknown"
	}
	return c
}

// extractDelegationIDFromBody pulls params.message.metadata.delegation_id
// out of an A2A JSON-RPC body. Empty string when absent — drain treats
// that as "this queue item didn't originate from /workspaces/:id/delegate"
// and skips the stitch, so non-delegation queue uses (cross-workspace
// peer-direct A2A) aren't affected.
func extractDelegationIDFromBody(body []byte) string {
	var envelope struct {
		Params struct {
			Message struct {
				Metadata struct {
					DelegationID string `json:"delegation_id"`
				} `json:"metadata"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	return envelope.Params.Message.Metadata.DelegationID
}

// stitchDrainResponseToDelegation writes the drained response into the
// delegation's existing delegate_result row (created with status='queued'
// by executeDelegation when the proxy first returned queued). This is the
// other half of the loop that closes "queued → completed" so the LLM's
// check_task_status reflects ground truth.
//
// Errors are logged-only — drain is fire-and-forget from Heartbeat, and a
// stitch failure shouldn't block other queued items. The delegation will
// just remain stuck at 'queued' in this case, which is the pre-fix
// behaviour (no regression vs. shipping nothing).
func (h *WorkspaceHandler) stitchDrainResponseToDelegation(ctx context.Context, sourceID, targetID, delegationID string, respBody []byte) {
	if sourceID == "" || delegationID == "" {
		return
	}
	responseText := extractResponseText(respBody)
	respJSON, marshalErr := json.Marshal(map[string]interface{}{
		"text":          responseText,
		"delegation_id": delegationID,
	})
	if marshalErr != nil {
		log.Printf("a2aQueue stitch %s: json.Marshal respJSON failed: %v", delegationID, marshalErr)
		return
	}
	// AND status = 'queued' pins this to the PLACEHOLDER row that executeDelegation
	// wrote. Without it the UPDATE also matches the sweeper's own terminal
	// delegate_result row (same workspace/type/method/target/delegation_id), so a
	// late drain would silently rewrite a "Delegation failed" notice into
	// "completed" — leaving activity_logs saying completed while `delegations` says
	// failed, permanently disagreeing, with the caller having been told both.
	//
	// It also makes the stitch idempotent: a replayed drain matches zero rows and
	// skips the ledger terminalization below instead of double-applying it.
	res, err := db.DB.ExecContext(ctx, `
		UPDATE activity_logs
		   SET status        = 'completed',
		       summary       = $1,
		       response_body = $2::jsonb
		 WHERE workspace_id   = $3
		   AND activity_type  = 'delegation'
		   AND method         = 'delegate_result'
		   AND target_id      = $4
		   AND response_body->>'delegation_id' = $5
		   AND status         = 'queued'
	`, "Delegation completed ("+textutil.TruncateBytes(responseText, 80)+")", string(respJSON),
		sourceID, targetID, delegationID)
	if err != nil {
		log.Printf("A2AQueue drain stitch: update failed for delegation %s: %v", delegationID, err)
		return
	}
	rows, err := res.RowsAffected()
	if err != nil {
		log.Printf("A2AQueue drain stitch: RowsAffected error for delegation %s: %v", delegationID, err)
		return
	}
	if rows == 0 {
		log.Printf("A2AQueue drain stitch: no delegate_result row for delegation %s (queued-row may not exist yet)", delegationID)
		return
	}
	// TERMINALIZE THE LEDGER TOO — not just the activity_logs row.
	//
	// The drain path used to flip only activity_logs, leaving the `delegations`
	// row at 'queued' FOREVER. The sweeper selects
	// status IN ('queued','dispatched','in_progress'), so a delegation that
	// SUCCEEDED here stayed in the sweeper's candidate set until its 6h deadline
	// and was then marked 'failed' — and, once the sweeper notifies (#4316), the
	// caller's agent would be told a delegation that completed six hours ago had
	// failed. The forward-only terminal guard in delegation_ledger.go does not
	// help: it only protects rows the normal path terminalized, and this path
	// never did, so the guard was vacuous exactly where it was needed.
	//
	// It is also the direct cause of the digest counting a drained-completed
	// delegation as "awaiting a reply" for six hours (#4314).
	authority := recordLedgerStatus(ctx, delegationID, "completed", "", responseText)
	log.Printf("A2AQueue drain stitch: delegation %s queued → completed (%d chars)", delegationID, len(responseText))

	// AND TELL THE CALLER. This is the queued-drain path — the delegation whose
	// target was settling/restarting when the message was sent, so the platform HELD
	// it in a2a_queue and is only now delivering the answer. It is the scenario the
	// caller is most likely to have been left hanging on.
	//
	// Every other terminal transition emits exactly one delegate_result into the
	// caller's inbox (the single-reply-authority contract). This one did not — the
	// drained answer landed in the ledger and in the canvas feed, but never in the
	// caller's inbox. On main that was merely wrong-but-VISIBLE: the row stayed
	// 'queued', so the digest still listed the delegation as awaiting a reply.
	// Terminalizing it correctly without also replying would have made it INVISIBLE
	// — the row leaves the in-flight set and nothing ever tells the caller. Fixing
	// the ledger alone would have converted a visible bug into a silent one.
	//
	// Gate: mayReply() is true when WE terminalized this row (we own the single
	// reply) OR when the ledger cannot arbitrate at all — dark, or no row — in which
	// case nobody else will speak and we must. See ReplyAuthority. It preserves
	// behaviour — with no ledger there is no CAS to arbitrate on, and the drain is
	// the only writer on this path anyway.
	if mayReply(authority) {
		pushDelegationResultToInbox(ctx, sourceID, targetID, delegationID, "completed", responseText, "")
	}

	// Broadcast DELEGATION_COMPLETE so the canvas chat feed flips the
	// "⏸ queued" line to "✓ completed" in real time. Without this the
	// transition only surfaces after the user reloads or polls activity.
	if h.broadcaster != nil {
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationComplete), sourceID, map[string]interface{}{
			"delegation_id":    delegationID,
			"target_id":        targetID,
			"response_preview": textutil.TruncateBytes(responseText, 200),
			"via":              "queue_drain",
		})
	}
}

// stitchDroppedDelegationFailure is the FAILURE twin of
// stitchDrainResponseToDelegation, for the settling-ceiling drop path. When the
// drain terminally drops a queued item that was a DELEGATION, the caller's
// delegate_result placeholder (status='queued', written by executeDelegation)
// would otherwise sit until the 6h delegation-sweeper deadline — the caller's
// check_task_status reports the work still in-flight for hours after the platform
// abandoned it (core#4459 re-review [1]). This closes "queued → failed" the same
// way the success path closes "queued → completed": it flips the placeholder,
// terminalizes the ledger, and (single-reply-authority) notifies the caller.
//
// Best-effort / idempotent by the same rules as the success stitch: the
// AND status='queued' guard pins the write to the placeholder (so a race with the
// sweeper's own terminal row is not clobbered) and makes a replayed drop a no-op.
func (h *WorkspaceHandler) stitchDroppedDelegationFailure(ctx context.Context, sourceID, targetID, delegationID, reason string) {
	if sourceID == "" || delegationID == "" {
		return
	}
	errJSON, marshalErr := json.Marshal(map[string]interface{}{
		"text":          reason,
		"delegation_id": delegationID,
	})
	if marshalErr != nil {
		log.Printf("a2aQueue drop-stitch %s: json.Marshal failed: %v", delegationID, marshalErr)
		return
	}
	res, err := db.DB.ExecContext(ctx, `
		UPDATE activity_logs
		   SET status        = 'failed',
		       summary       = $1,
		       error_detail  = $2,
		       response_body = $3::jsonb
		 WHERE workspace_id   = $4
		   AND activity_type  = 'delegation'
		   AND method         = 'delegate_result'
		   AND target_id      = $5
		   AND response_body->>'delegation_id' = $6
		   AND status         = 'queued'
	`, "Delegation failed ("+textutil.TruncateBytes(reason, 80)+")", reason, string(errJSON),
		sourceID, targetID, delegationID)
	if err != nil {
		log.Printf("A2AQueue drain drop-stitch: update failed for delegation %s: %v", delegationID, err)
		return
	}
	rows, err := res.RowsAffected()
	if err != nil {
		log.Printf("A2AQueue drain drop-stitch: RowsAffected error for delegation %s: %v", delegationID, err)
		return
	}
	if rows == 0 {
		// Not a delegation with a live placeholder (or already terminalized) —
		// nothing to fail. No ledger/inbox side effects.
		log.Printf("A2AQueue drain drop-stitch: no queued delegate_result row for delegation %s", delegationID)
		return
	}
	authority := recordLedgerStatus(ctx, delegationID, "failed", reason, "")
	log.Printf("A2AQueue drain drop-stitch: delegation %s queued → failed (settling-ceiling drop)", delegationID)
	if mayReply(authority) {
		pushDelegationResultToInbox(ctx, sourceID, targetID, delegationID, "failed", "", reason)
	}
	if h.broadcaster != nil {
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventDelegationFailed), sourceID, map[string]interface{}{
			"delegation_id": delegationID,
			"target_id":     targetID,
			"error":         textutil.TruncateBytes(reason, 200),
			"via":           "queue_drain_drop",
		})
	}
}

// StartA2AQueueSweeper starts the independent periodic drain fallback required
// by #2930. It is intentionally decoupled from the target workspace's heartbeat:
// if a workspace stops heartbeating (offline, flapping, restart wedge), queued
// requests would otherwise sit until TTL and then be silently dropped.
//
// The sweeper runs on a fixed interval, scans for workspaces with pending
// non-expired queue rows and status 'online' or 'degraded', and drains up to
// max_concurrent_tasks items per workspace per tick. Drain dispatches are run
// via globalGoAsync so they are detached from the caller and use the same
// ProxyA2ARequest path as heartbeat-driven drains.
func (h *WorkspaceHandler) StartA2AQueueSweeper(ctx context.Context) {
	if !h.HasProvisioner() {
		// No provisioner means there is no local runtime to drain to (external/
		// mock-only deployment). Skip the sweeper rather than spin no-ops.
		return
	}
	log.Println("A2AQueue sweeper: starting independent periodic drain fallback")
	go func() {
		ticker := time.NewTicker(a2aQueueSweeperInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Println("A2AQueue sweeper: shutting down")
				return
			case <-ticker.C:
				h.sweepA2AQueue(ctx)
			}
		}
	}()
}

// sweepA2AQueue finds online/degraded workspaces with pending queued items and
// drains each in a detached goroutine.
func (h *WorkspaceHandler) sweepA2AQueue(ctx context.Context) {
	rows, err := db.DB.QueryContext(ctx, `
		SELECT q.workspace_id, COALESCE(w.max_concurrent_tasks, 1) AS capacity
		FROM a2a_queue q
		JOIN workspaces w ON w.id = q.workspace_id
		WHERE q.status = 'queued'
		  AND (q.expires_at IS NULL OR q.expires_at > now())
		  AND w.status IN ('online', 'degraded')
		GROUP BY q.workspace_id, w.max_concurrent_tasks
	`)
	if err != nil {
		log.Printf("A2AQueue sweeper: scan failed: %v", err)
		return
	}
	defer rows.Close()

	var wg sync.WaitGroup
	for rows.Next() {
		var workspaceID string
		var capacity int
		if err := rows.Scan(&workspaceID, &capacity); err != nil {
			log.Printf("A2AQueue sweeper: scan row failed: %v", err)
			continue
		}
		if capacity > a2aQueueSweeperBatchCap {
			capacity = a2aQueueSweeperBatchCap
		}
		if capacity <= 0 {
			continue
		}
		// Bound per-tick work: the heartbeat path already drains up to
		// (max_concurrent - active_tasks) on every beat. The sweeper is a
		// safety net, not a throughput pump.
		wg.Add(1)
		globalGoAsync(func(ws string, cap int) func() {
			return func() {
				defer wg.Done()
				h.DrainQueueForWorkspace(ctx, ws, cap)
			}
		}(workspaceID, capacity))
	}
	if err := rows.Err(); err != nil {
		log.Printf("A2AQueue sweeper: row iteration failed: %v", err)
	}
	// Wait for this tick's drains before returning so shutdown is clean.
	wg.Wait()
}

// CountStrandedQueueItems returns the number of queued items for workspaces
// that are not online/degraded (e.g., offline or provisioning). Used for
// alerting/metrics rather than silently dropping at TTL.
func CountStrandedQueueItems(ctx context.Context) (int, error) {
	var count int
	err := db.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM a2a_queue q
		JOIN workspaces w ON w.id = q.workspace_id
		WHERE q.status = 'queued'
		  AND (q.expires_at IS NULL OR q.expires_at > now())
		  AND w.status NOT IN ('online', 'degraded')
	`).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

package handlers

// a2a_proxy_helpers.go — A2A proxy error handling, activity logging,
// caller auth validation, token usage tracking, and SSRF safety checks.

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/middleware"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/orgtoken"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/wsauth"
	"github.com/gin-gonic/gin"
)

// proxyDispatchBuildError is a sentinel wrapper for failures inside
// http.NewRequestWithContext. handleA2ADispatchError unwraps it to emit the
// "failed to create proxy request" 500 instead of the standard 502/503 paths.
type proxyDispatchBuildError struct{ err error }

func (e *proxyDispatchBuildError) Error() string { return e.err.Error() }

// handleA2ADispatchError translates a forward-call failure into a proxyA2AError,
// runs the reactive container-health check, and records the outcome. Busy
// targets that are successfully queued are logged as queued, not failed.
func (h *WorkspaceHandler) handleA2ADispatchError(ctx context.Context, workspaceID, callerID string, body []byte, a2aMethod string, err error, durationMs int, logActivity bool) (int, []byte, *proxyA2AError) {
	// Build-time failure (couldn't even create the http.Request) — return
	// a 500 without the reactive-health / busy-retry paths.
	if buildErr, ok := err.(*proxyDispatchBuildError); ok {
		_ = buildErr
		return 0, nil, &proxyA2AError{
			Status:   http.StatusInternalServerError,
			Response: gin.H{"error": "failed to create proxy request"},
		}
	}

	log.Printf("ProxyA2A forward error: %v", err)

	containerDead := h.maybeMarkContainerDead(ctx, workspaceID)

	if containerDead {
		if logActivity {
			h.logA2AFailure(ctx, workspaceID, callerID, body, a2aMethod, err, durationMs)
		}
		return 0, nil, &proxyA2AError{
			Status:   http.StatusServiceUnavailable,
			Response: gin.H{"error": "workspace agent unreachable — container restart triggered", "restarting": true},
		}
	}
	// Container is alive but upstream Do() failed with a timeout/EOF-
	// shaped error — the agent is most likely mid-synthesis on a
	// previous request (single-threaded main loop). Surface as 503
	// Busy with a Retry-After hint so callers can distinguish this
	// from a real unreachable-agent (502) and retry with backoff.
	// Issue #110.
	//
	// #1870 Phase 1: before returning 503, enqueue the request for drain
	// on next heartbeat. Returning 202 Accepted {queued:true} as a SUCCESS
	// (not an error) means callers record this as "dispatched — queued"
	// not "failed", eliminating the fan-out-storm drop pattern.
	//
	// Critical: must return (status, body, NIL ERROR) so the caller's
	// `if proxyErr != nil` branch doesn't fire. Returning a proxyA2AError
	// with 202 status here was the original cycle 53 bug — callers saw
	// proxyErr != nil and logged "delegation failed: proxy a2a error".
	if isUpstreamBusyError(err) {
		// #1684 / Reno Stars: native_session adapters previously took a
		// 503-no-enqueue path here, on the assumption that the SDK owned
		// an inbound queue and the platform a2a_queue would double-buffer.
		// In practice, the common native_session SDKs (claude-agent-sdk,
		// codex app-server, hermes-agent) do NOT have an inbound queue —
		// new turns can only be pushed via the same HTTP POST that just
		// returned busy. So cron fires (and any A2A retry) bounce 503
		// every tick until the SDK voluntarily yields. Reno Stars #1684
		// observed 12 consecutive `*/30` cron fires lost over 6h while a
		// single native_session held the slot.
		//
		// The original concern — "drain timing has no relationship to SDK
		// readiness" — turns out to be unfounded: heartbeat→drain is
		// gated by `payload.ActiveTasks < maxConcurrent` in
		// registry.go:Heartbeat, so drain only fires when the workspace
		// itself reports spare capacity. That IS the session-ended
		// signal. The native_session SDK reports ActiveTasks=1 while in a
		// turn, ActiveTasks=0 when idle, and the next heartbeat after
		// idle triggers DrainQueueForWorkspace.
		//
		// So we collapse the two branches: both native_session and
		// non-native callers enqueue here. The native_session SDK's own
		// in-flight POST stays unaffected; the queued item drains on the
		// next post-idle heartbeat.
		idempotencyKey := extractIdempotencyKey(body)
		// Honor params.expires_in_seconds when the caller specifies one. Zero
		// (the unset default) → expiresAt = nil → infinite TTL preserved by
		// DequeueNext. RFC #2331 Tier 1.
		var expiresAt *time.Time
		if secs := extractExpiresInSeconds(body); secs > 0 {
			t := time.Now().Add(time.Duration(secs) * time.Second)
			expiresAt = &t
		}
		if qid, depth, qerr := EnqueueA2A(
			ctx, workspaceID, callerID, PriorityTask, body, a2aMethod, idempotencyKey, expiresAt,
		); qerr == nil {
			log.Printf("ProxyA2A: target %s busy — enqueued as %s (depth=%d)", workspaceID, qid, depth)
			if logActivity {
				h.logA2ABusyQueued(ctx, workspaceID, callerID, body, a2aMethod, durationMs)
			}
			respBody, marshalErr := json.Marshal(gin.H{
				"queued":      true,
				"queue_id":    qid,
				"queue_depth": depth,
				"message":     "workspace agent busy — request queued, will dispatch when capacity available",
			})
			if marshalErr != nil {
				log.Printf("ProxyA2A %s: json.Marshal respBody failed: %v", workspaceID, marshalErr)
			}
			return http.StatusAccepted, respBody, nil
		} else {
			// Queue insert failed — fall through to legacy 503 behavior
			// so callers still retry. We don't want a queue DB hiccup to
			// make delegation silently disappear.
			log.Printf("ProxyA2A: enqueue for %s failed (%v) — falling back to 503", workspaceID, qerr)
		}
		if logActivity {
			h.logA2AFailure(ctx, workspaceID, callerID, body, a2aMethod, err, durationMs)
		}
		return 0, nil, &proxyA2AError{
			Status:  http.StatusServiceUnavailable,
			Headers: map[string]string{"Retry-After": strconv.Itoa(busyRetryAfterSeconds)},
			Response: gin.H{
				"error":       "workspace agent busy — retry after a short backoff",
				"busy":        true,
				"retry_after": busyRetryAfterSeconds,
			},
		}
	}
	if logActivity {
		h.logA2AFailure(ctx, workspaceID, callerID, body, a2aMethod, err, durationMs)
	}
	return 0, nil, &proxyA2AError{
		Status:   http.StatusBadGateway,
		Response: gin.H{"error": "failed to reach workspace agent"},
	}
}

// maybeMarkContainerDead runs the reactive health check after a forward error.
// If the workspace's compute (Docker container OR EC2 instance) is no longer
// running (and the workspace isn't external), it marks the workspace offline,
// clears Redis state, broadcasts WORKSPACE_OFFLINE, and triggers an async
// restart. Returns true when the compute was found dead.
//
// Provisioner selection (mutually exclusive in production):
//   - h.provisioner != nil  → local Docker deployment; IsRunning does docker inspect.
//   - h.cpProv != nil       → SaaS / EC2 deployment; IsRunning calls CP's
//     /cp/workspaces/:id/status to read the EC2 state.
//
// Pre-fix this function ONLY consulted h.provisioner — for SaaS tenants
// (h.provisioner=nil, h.cpProv=set) it short-circuited to false on every
// call, so a dead EC2 agent would propagate upstream 502/503/504 to canvas
// with no auto-recovery and Cloudflare in front would mask the response with
// its own error page. The 2026-04-30 hongmingwang.moleculesai.app
// canvas-chat-to-dead-workspace incident traces to exactly this gap.
func (h *WorkspaceHandler) maybeMarkContainerDead(ctx context.Context, workspaceID string) bool {
	var wsRuntime string
	db.DB.QueryRowContext(ctx, `SELECT COALESCE(runtime, 'claude-code') FROM workspaces WHERE id = $1`, workspaceID).Scan(&wsRuntime)
	if isExternalLikeRuntime(wsRuntime) {
		return false
	}
	if !h.HasProvisioner() {
		return false
	}
	// Restart-aware short-circuit: during the 20-30s EC2-pending window of
	// an in-flight restart, the workspace's url='' and IsRunning() returns
	// false → looks indistinguishable from a dead container. Pre-fix this
	// fired a fresh RestartByID for the just-launched instance, which
	// coalesceRestart's pending-flag drained by running ANOTHER full
	// stop+provision cycle (= ec2_stopped of the still-pending instance
	// → re-provision). That's the 4x reprov thrash class. Skip the
	// container-dead path while a restart is in flight; the in-flight
	// restart's own provisionWorkspaceAutoSync will surface a real failure
	// (markProvisionFailed) if the new container never comes up. Issue
	// internal#544.
	if isRestarting(workspaceID) {
		log.Printf("ProxyA2A: maybeMarkContainerDead skipped for %s — restart already in flight (self-fire guard)", workspaceID)
		return false
	}

	var running bool
	var inspectErr error
	if h.provisioner != nil {
		running, inspectErr = h.provisioner.IsRunning(ctx, workspaceID)
	} else {
		// SaaS path: ask the CP about the EC2 state. Same (true, err) on
		// transport errors contract — keeps the caller on the alive path
		// instead of triggering a restart cascade on a flaky CP call.
		running, inspectErr = h.cpProv.IsRunning(ctx, workspaceID)
	}
	if inspectErr != nil {
		// Transient backend error (Docker daemon EOF, CP HTTP 5xx, etc.).
		// IsRunning's contract returns (true, err) in this case so we stay
		// on the alive path without triggering a restart cascade.
		log.Printf("ProxyA2A: IsRunning for %s returned transient error (assuming alive): %v", workspaceID, inspectErr)
	}
	if running {
		return false
	}
	log.Printf("ProxyA2A: container for %s is dead — marking offline and triggering restart", workspaceID)
	if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = $1, updated_at = now() WHERE id = $2 AND status NOT IN ('removed', 'provisioning')`, models.StatusOffline, workspaceID); err != nil {
		log.Printf("ProxyA2A: failed to mark workspace %s offline: %v", workspaceID, err)
	}
	db.ClearWorkspaceKeys(ctx, workspaceID)
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceOffline), workspaceID, map[string]interface{}{})
	// Tracked via goAsync (not bare `go`) so the asyncWG can be drained
	// before a test swaps the global db.DB. runRestartCycle reads db.DB
	// before its provisioner gate, so an untracked detached goroutine
	// races setupTestDB's t.Cleanup db.DB restore. Matches the already-
	// correct site at a2a_proxy.go:648.
	h.goAsync(func() { h.RestartByID(workspaceID) })
	return true
}

// preflightContainerHealth runs a proactive Provisioner.IsRunning check
// (#36) before dispatching the a2a forward. Routed through provisioner's
// SSOT IsRunning, which itself wraps RunningContainerName — same source
// as findRunningContainer in the plugins handler (#10/#12).
//
// Returns nil when the forward should proceed:
//   - container is running, OR
//   - daemon errored transiently (matches IsRunning's (true, err)
//     "fail-soft as alive" contract — let the optimistic forward run
//     and reactive maybeMarkContainerDead catch a real failure).
//
// Returns a structured 503 + triggers the same async restart that
// maybeMarkContainerDead would produce, when:
//   - container is genuinely not running (NotFound / Exited / Created…).
//
// The point of running this BEFORE the forward is to save the caller
// 2-30s of network-timeout cost when the container is missing — a common
// shape post-EC2-replace (see molecule-controlplane#20 incident
// 2026-05-07) where the reconciler hasn't respawned the agent yet.
func (h *WorkspaceHandler) preflightContainerHealth(ctx context.Context, workspaceID string) *proxyA2AError {
	// Restart-aware short-circuit (mirror of maybeMarkContainerDead): if a
	// restart cycle is in flight for this workspace, do not run the
	// IsRunning probe — it would observe the EC2-pending state as "not
	// running" and trigger RestartByID for an already-restarting workspace,
	// closing the self-fire loop. Returning nil lets the optimistic
	// forward proceed; the upstream Do() call will fail with a connection
	// error or 502, and the *post-restart* reactive path can decide what
	// to do once the cycle has actually completed. Issue internal#544.
	if isRestarting(workspaceID) {
		log.Printf("ProxyA2A preflight: %s — skipped, restart already in flight (self-fire guard)", workspaceID)
		return nil
	}
	running, err := h.provisioner.IsRunning(ctx, workspaceID)
	if err != nil {
		// Transient daemon error. Provisioner.IsRunning returns (true, err)
		// in this case — fall through to the optimistic forward, reactive
		// maybeMarkContainerDead handles a real failure later.
		log.Printf("ProxyA2A preflight: IsRunning transient error for %s: %v (proceeding with forward)", workspaceID, err)
		return nil
	}
	if running {
		// Container is running — forward as today.
		return nil
	}
	// Container is genuinely not running. Mark offline + trigger restart
	// (same effect as maybeMarkContainerDead's branch), and return the
	// structured 503 immediately so the caller skips the forward.
	log.Printf("ProxyA2A preflight: container for %s is not running — marking offline and triggering restart (#36)", workspaceID)
	if _, dbErr := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET status = $1, updated_at = now() WHERE id = $2 AND status NOT IN ('removed', 'provisioning')`,
		models.StatusOffline, workspaceID); dbErr != nil {
		log.Printf("ProxyA2A preflight: failed to mark workspace %s offline: %v", workspaceID, dbErr)
	}
	db.ClearWorkspaceKeys(ctx, workspaceID)
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceOffline), workspaceID, map[string]interface{}{})
	// Tracked via goAsync (see maybeMarkContainerDead): preflight's
	// detached restart must be drainable so it doesn't race the global
	// db.DB swap in test cleanup.
	h.goAsync(func() { h.RestartByID(workspaceID) })
	return &proxyA2AError{
		Status: http.StatusServiceUnavailable,
		Response: gin.H{
			"error":      "workspace container not running — restart triggered",
			"restarting": true,
			"preflight":  true, // distinguishes from reactive containerDead path
		},
	}
}

// logA2AFailure records a failed A2A attempt to activity_logs in a detached
// goroutine (the request context may already be done by the time it runs).
func (h *WorkspaceHandler) logA2AFailure(ctx context.Context, workspaceID, callerID string, body []byte, a2aMethod string, err error, durationMs int) {
	errMsg := err.Error()
	var errWsName string
	db.DB.QueryRowContext(ctx, `SELECT name FROM workspaces WHERE id = $1`, workspaceID).Scan(&errWsName)
	if errWsName == "" {
		errWsName = workspaceID
	}
	summary := "A2A request to " + errWsName + " failed: " + errMsg
	parent := ctx
	h.goAsync(func() {
		logCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
		defer cancel()
		LogActivity(logCtx, h.broadcaster, ActivityParams{
			WorkspaceID:  workspaceID,
			ActivityType: "a2a_receive",
			SourceID:     nilIfEmpty(callerID),
			TargetID:     &workspaceID,
			Method:       &a2aMethod,
			Summary:      &summary,
			RequestBody:  json.RawMessage(body),
			DurationMs:   &durationMs,
			Status:       "error",
			ErrorDetail:  &errMsg,
		})
	})
}

// logA2ABusyQueued records that a push attempt reached a live but busy
// workspace and was durably queued for heartbeat drain.
func (h *WorkspaceHandler) logA2ABusyQueued(ctx context.Context, workspaceID, callerID string, body []byte, a2aMethod string, durationMs int) {
	var wsName string
	db.DB.QueryRowContext(ctx, `SELECT name FROM workspaces WHERE id = $1`, workspaceID).Scan(&wsName)
	if wsName == "" {
		wsName = workspaceID
	}
	summary := a2aMethod + " → " + wsName + " (queued: target busy)"
	parent := ctx
	h.goAsync(func() {
		logCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
		defer cancel()
		LogActivity(logCtx, h.broadcaster, ActivityParams{
			WorkspaceID:  workspaceID,
			ActivityType: "a2a_receive",
			SourceID:     nilIfEmpty(callerID),
			TargetID:     &workspaceID,
			Method:       &a2aMethod,
			Summary:      &summary,
			RequestBody:  json.RawMessage(body),
			DurationMs:   &durationMs,
			Status:       "ok",
		})
	})
}

// logA2ASuccess records a successful A2A round-trip and (for canvas-initiated
// 2xx/3xx responses) broadcasts an A2A_RESPONSE event so the frontend can
// receive the reply without polling.
func (h *WorkspaceHandler) logA2ASuccess(ctx context.Context, workspaceID, callerID string, body, respBody []byte, a2aMethod string, statusCode, durationMs int) {
	logStatus := "ok"
	if statusCode >= 400 {
		logStatus = "error"
	}
	var wsNameForLog string
	db.DB.QueryRowContext(ctx, `SELECT name FROM workspaces WHERE id = $1`, workspaceID).Scan(&wsNameForLog)
	if wsNameForLog == "" {
		wsNameForLog = workspaceID
	}

	// #817: track outbound activity on the CALLER so orchestrators can detect
	// silent workspaces. Only update when callerID is a real workspace (not
	// canvas, not a system caller) and the target returned 2xx/3xx.
	if callerID != "" && !isSystemCaller(callerID) && statusCode < 400 {
		h.goAsync(func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := db.DB.ExecContext(bgCtx,
				`UPDATE workspaces SET last_outbound_at = NOW() WHERE id = $1`, callerID); err != nil {
				log.Printf("last_outbound_at update failed for %s: %v", callerID, err)
			}
		})
	}
	summary := a2aMethod + " → " + wsNameForLog
	toolTrace := extractToolTrace(respBody)

	// DATA-LOSS FIX (internal#470 / #1347 push-mode sibling): this
	// a2a_receive row is the ONLY durable record of a push-mode chat
	// round-trip — request_body carries the user's message, response_body
	// carries the agent's reply, and chat-history hydration
	// (messagestore.PostgresMessageStore) reads BOTH back to rebuild the
	// transcript on canvas reopen / reload. It MUST be written
	// SYNCHRONOUSLY, before proxyA2ARequest returns and ProxyA2A flushes
	// the 200 to the canvas — otherwise the canvas sees the reply
	// acknowledged (and rendered optimistically) while the row is still
	// racing in a detached goroutine, and a reload (or a workspace-server
	// restart / deploy / OOM) between the 200 and the goroutine's commit
	// loses the message permanently on reopen.
	//
	// This mirrors the discipline already applied to the poll-mode ingest
	// path (logA2AReceiveQueued / persistUserMessageAtIngest); the
	// push-mode counterpart was left async, which the E2E Chat
	// "history persists across reload" test surfaced as an intermittent
	// red (the reload out-raced the INSERT).
	//
	//   - context.WithoutCancel: a client disconnect on chat-exit (which
	//     cancels the inbound request ctx) MUST NOT abort this write.
	//   - SYNCHRONOUS (no goAsync): the row must be durable before the 200.
	//   - Best-effort: LogActivity logs+swallows INSERT errors internally,
	//     so a DB hiccup never blocks or fails the user's send — behaviour
	//     for that one request is never worse than the pre-fix async path.
	//   - The post-commit ACTIVITY_LOGGED broadcast still fires inside
	//     LogActivity; the durable row is the truth the canvas re-reads.
	logCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	LogActivity(logCtx, h.broadcaster, ActivityParams{
		WorkspaceID:  workspaceID,
		ActivityType: "a2a_receive",
		SourceID:     nilIfEmpty(callerID),
		TargetID:     &workspaceID,
		Method:       &a2aMethod,
		Summary:      &summary,
		RequestBody:  json.RawMessage(body),
		ResponseBody: json.RawMessage(respBody),
		ToolTrace:    toolTrace,
		DurationMs:   &durationMs,
		Status:       logStatus,
		MessageId:    extractMessageIdFromA2ABody(body),
	})

	if callerID == "" && statusCode < 400 {
		h.broadcaster.BroadcastOnly(workspaceID, string(events.EventA2AResponse), map[string]interface{}{
			"response_body": json.RawMessage(respBody),
			"method":        a2aMethod,
			"duration_ms":   durationMs,
		})
	}
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// validateCallerToken enforces the Phase 30.5 auth-token contract on the
// caller of an A2A proxy request. Same lazy-bootstrap shape as
// registry.requireWorkspaceToken: if the caller workspace has any live
// token on file, the Authorization header is mandatory and must match;
// if the caller has zero live tokens, they're grandfathered through
// (their next /registry/register will mint their first token, after
// which this branch never fires again for them).
//
// Post-RFC#637 addition: a request may instead be carrying a HUMAN's
// canvas-user identity (e.g. the 344a2623-… identity workspace from the
// RFC#637 rollout). That human sits OUTSIDE the workspace org hierarchy, so
// the returned isCanvasUser flag lets the A2A proxy bypass CanCommunicate for
// it. Canvas-user classification is decided by isGenuineCanvasUser using
// NON-FORGEABLE credentials only (see that function) — never by the caller's
// X-Workspace-ID alone, and never by a bare same-origin Host/Referer in a
// SaaS image (those are forgeable; see middleware.IsSameOriginCanvas).
//
// #1673: this canvas-user check is now evaluated BEFORE the HasAnyLiveToken
// peer-token contract. Previously it lived only in the !hasLive branch, so a
// canvas-user identity workspace that had acquired live tokens fell into the
// hasLive=true branch, which demands a bearer the canvas frontend never sends
// → silent 401 → the message was dropped before logA2AReceiveQueued wrote the
// activity_logs row, breaking canvas chat for poll-mode workspaces. A genuine
// canvas user is identified by the human's session/admin/org credential, which
// is independent of whether the identity workspace happens to hold peer tokens.
//
// On auth failure this writes the 401 via c and returns an error so the
// handler aborts without running the proxy.
func validateCallerToken(ctx context.Context, c *gin.Context, callerID string) (isCanvasUser bool, err error) {
	// Genuine canvas-user identity? Decided independently of the caller
	// workspace's token state (the #1673 fix) and using only non-forgeable
	// signals (the #1944 escalation guard).
	if isGenuineCanvasUser(ctx, c) {
		return true, nil
	}

	hasLive, dbErr := wsauth.HasAnyLiveToken(ctx, db.DB, callerID)
	if dbErr != nil {
		// Fail-open here matches the heartbeat path — A2A caller auth is
		// defense-in-depth on top of access-control hierarchy, not the
		// sole gate on the secret material. A DB hiccup shouldn't take
		// the whole A2A path down.
		log.Printf("wsauth: caller HasAnyLiveToken(%s) failed: %v — allowing A2A", callerID, dbErr)
		return false, nil
	}
	if !hasLive {
		// Tokenless, non-canvas-user workspace — legacy / pre-upgrade peer.
		// Grandfather it through (its next /registry/register mints its
		// first token, after which it lands in the hasLive=true branch).
		return false, nil
	}
	tok := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization"))
	if tok == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing caller auth token"})
		return false, errInvalidCallerToken
	}
	if err := wsauth.ValidateToken(ctx, db.DB, callerID, tok); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid caller auth token"})
		return false, err
	}
	return false, nil
}

// isGenuineCanvasUser reports whether the request is a real human acting
// through the canvas UI (RFC#637 canvas-user identity), as opposed to a peer
// workspace agent. A true result lets the A2A proxy bypass CanCommunicate, so
// it MUST only accept signals an attacker on the platform network cannot forge:
//
//   - A control-plane-verified canvas session: the WorkOS session cookie is
//     confirmed upstream to belong to a MEMBER of THIS tenant's org
//     (middleware.IsVerifiedCanvasSession → /cp/auth/tenant-member). This is
//     the production SaaS canvas path.
//   - An Authorization: Bearer matching ADMIN_TOKEN (break-glass / molecli).
//   - An Authorization: Bearer matching a live org_api_tokens row (user-minted
//     org-scoped API token).
//
// Deliberately NOT accepted as a canvas-user signal in a SaaS image:
//
//   - A bare same-origin Host/Referer/Origin (middleware.IsSameOriginCanvas).
//     Those headers are trivially forgeable by any container on the Docker
//     network, and the combined-tenant image (CANVAS_PROXY_URL set) is exactly
//     where a forged Referer + an arbitrary X-Workspace-ID could otherwise
//     bypass CanCommunicate and reach cross-workspace A2A — the PR #1944
//     privilege escalation. Same-origin is only honored as a fallback when CP
//     session verification is NOT configured (self-hosted / dev), a
//     single-tenant topology with no cross-tenant boundary to escalate across;
//     even there the org hierarchy still owns intra-org routing.
//
// Note this classification is about the human's credential, not the caller
// workspace's X-Workspace-ID — so it never trusts an attacker-supplied caller
// ID, and it is independent of whether that workspace holds peer tokens.
func isGenuineCanvasUser(ctx context.Context, c *gin.Context) bool {
	// Production SaaS: control-plane-verified org-member session cookie.
	if middleware.IsVerifiedCanvasSession(c) {
		return true
	}

	if tok := wsauth.BearerTokenFromHeader(c.GetHeader("Authorization")); tok != "" {
		adminSecret := os.Getenv("ADMIN_TOKEN")
		if adminSecret != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(adminSecret)) == 1 {
			return true
		}
		if _, _, _, err := orgtoken.Validate(ctx, db.DB, tok); err == nil {
			return true
		}
	}

	// Self-hosted / dev fallback ONLY: when upstream session verification is
	// not configured there is no verified-cookie signal to use, and the
	// deployment is single-tenant, so the forgeable same-origin check is an
	// acceptable canvas signal. In SaaS (CP session configured) this branch is
	// skipped, closing the forged-same-origin escalation.
	if !middleware.CPSessionConfigured() && middleware.IsSameOriginCanvas(c) {
		return true
	}
	return false
}

// errInvalidCallerToken is a sentinel for validateCallerToken's "missing
// token" branch so the handler-level guard can detect it without string
// matching (the wsauth errors are typed for the invalid case).
var errInvalidCallerToken = errors.New("missing caller auth token")

// extractToolTrace pulls metadata.tool_trace from an A2A JSON-RPC response.
// Returns nil when absent or malformed — callers can pass it straight through.
func extractToolTrace(respBody []byte) json.RawMessage {
	if len(respBody) == 0 {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(respBody, &top); err != nil {
		return nil
	}
	rawResult, ok := top["result"]
	if !ok {
		return nil
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(rawResult, &result); err != nil {
		return nil
	}
	rawMeta, ok := result["metadata"]
	if !ok {
		return nil
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(rawMeta, &meta); err != nil {
		return nil
	}
	trace, ok := meta["tool_trace"]
	if !ok || len(trace) == 0 || string(trace) == "null" || string(trace) == "[]" {
		return nil
	}
	return trace
}

// extractAndUpsertTokenUsage parses LLM usage from a raw A2A response body
// and persists it via upsertTokenUsage. Safe to call in a goroutine — logs
// errors but never panics. ctx must already be detached from the request.
func extractAndUpsertTokenUsage(ctx context.Context, workspaceID string, respBody []byte) {
	in, out := parseUsageFromA2AResponse(respBody)
	if in > 0 || out > 0 {
		upsertTokenUsage(ctx, workspaceID, in, out)
	}
}

// parseUsageFromA2AResponse extracts input_tokens / output_tokens from an A2A
// JSON-RPC response. Inspects two locations in order of preference:
//  1. result.usage — the JSON-RPC 2.0 result envelope from workspace agents.
//  2. usage — top-level, for non-JSON-RPC or direct Anthropic-shaped payloads.
//
// Returns (0, 0) when no recognisable usage data is found.
func parseUsageFromA2AResponse(body []byte) (inputTokens, outputTokens int64) {
	if len(body) == 0 {
		return 0, 0
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return 0, 0
	}

	// 1. result.usage (JSON-RPC 2.0 wrapper produced by workspace agents).
	if rawResult, ok := top["result"]; ok {
		var result map[string]json.RawMessage
		if err := json.Unmarshal(rawResult, &result); err == nil {
			if in, out, ok := readUsageMap(result); ok {
				return in, out
			}
		}
	}

	// 2. Fallback: top-level usage (direct Anthropic or non-JSON-RPC response).
	if in, out, ok := readUsageMap(top); ok {
		return in, out
	}
	return 0, 0
}

// lookupDeliveryMode returns the workspace's delivery_mode.
//
// internal#497 / RFC#497 fail-closed (SURGICAL scope): the *specific*
// failure mode that hid the ce2db75f regression for 5 days is now
// propagated instead of silently swallowed — a CONTEXT error
// (context.Canceled / context.DeadlineExceeded). Under ce2db75f the
// detached delegation goroutine ran on a cancelled request context, every
// `SELECT delivery_mode` failed `context canceled`, this function returned
// push, the poll-mode short-circuit in proxyA2ARequest was skipped, and
// poll-mode peers (e.g. an operator laptop on molecule-mcp-claude-channel)
// silently never got their a2a_receive inbox row. A transient,
// systematic-once-triggered context cancellation became permanent
// invisible misrouting. Returning that error lets the caller fail loud
// (mark the delegation failed) instead of mis-dispatching.
//
// Scope is deliberately narrow: only ctx errors propagate. Other DB
// errors retain the long-standing documented "fall back to push (today's
// synchronous behavior)" contract — that path is loud + recoverable
// (502 / SSRF reject / restart), unlike the silent poll-mode drop, and
// the surrounding proxy (incl. the sibling checkWorkspaceBudget) is
// intentionally built around that fail-open-to-push behavior. Widening
// further is an RFC#497 follow-up, not part of this P0 fix.
//
// A genuinely *absent* configuration is NOT an error and still resolves to
// push (the safe synchronous default): sql.ErrNoRows, a NULL/empty column,
// or an unrecognised value all return (push, nil).
//
// The function is intentionally lookup-only — it never mutates the row.
// The register handler (registry.go) is the only writer for delivery_mode.
//
// See #2339 PR 1 for the column + register-flow side; this is the
// proxy-side read used for the short-circuit in proxyA2ARequest.
func lookupDeliveryMode(ctx context.Context, workspaceID string) (string, error) {
	var mode sql.NullString
	err := db.DB.QueryRowContext(ctx,
		`SELECT delivery_mode FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&mode)
	if err != nil {
		// internal#497: a context cancellation/deadline MUST NOT be
		// swallowed into a silent push default — that is the exact 5-day
		// silent-misrouting vector. Propagate so the caller fails closed.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			log.Printf("ProxyA2A: lookupDeliveryMode(%s) context error (%v) — failing closed (NOT defaulting to push)", workspaceID, err)
			return "", err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("ProxyA2A: lookupDeliveryMode(%s) failed (%v) — defaulting to push (non-ctx DB error; legacy fail-open-to-push contract)", workspaceID, err)
		}
		return models.DeliveryModePush, nil
	}
	if !mode.Valid || mode.String == "" {
		return models.DeliveryModePush, nil
	}
	if !models.IsValidDeliveryMode(mode.String) {
		log.Printf("ProxyA2A: workspace %s has invalid delivery_mode=%q — defaulting to push", workspaceID, mode.String)
		return models.DeliveryModePush, nil
	}
	return mode.String, nil
}

// logA2AReceiveQueued records a poll-mode "queued" A2A receive into
// activity_logs. Same shape as logA2ASuccess but without ResponseBody
// (there is no response yet — the polling agent will produce one when
// it picks the request up). status="ok" because the request was
// successfully queued; the consume side reports its own outcome.
//
// The activity_logs row is what the polling agent's GET /activity?since_id=
// reads in PR 3 — that's how a poll-mode workspace receives inbound A2A
// without a public URL.
func (h *WorkspaceHandler) logA2AReceiveQueued(ctx context.Context, workspaceID, callerID string, body []byte, a2aMethod string) {
	// DATA-LOSS FIX (internal#471 — poll-mode sibling of #1347/internal#470):
	// this is the ONLY durable write of a poll-mode inbound message,
	// including a canvas_user message (callerID == "") typed in the canvas
	// chat. It MUST be SYNCHRONOUS and complete BEFORE the caller returns
	// the synthetic {status:"queued"} 200 — otherwise the canvas sees the
	// send acknowledged while the activity_logs row is still racing in a
	// detached goroutine, and a workspace-server restart / deploy / OOM /
	// EC2 hibernation between the 200 and the goroutine's commit loses the
	// user's message permanently (chat-history reads activity_logs, so a
	// missing row = message gone on reopen). Hongming's tenant is entirely
	// poll-mode (4 external workspaces, no URL — verified empirically), so
	// his reported loss is THIS path; #1347 (push-mode, persists AFTER the
	// poll short-circuit) structurally cannot cover it.
	//
	// #2560: this path ALSO sets the messageId on the activity row. If
	// persistUserMessageAtIngest already wrote the same messageId (e.g.
	// the poll short-circuit ran AFTER the ingest path on a request that
	// got demoted to poll), the partial unique index
	// (idx_activity_logs_msg_id) makes this a no-op via ON CONFLICT DO
	// NOTHING — the existing row keeps the user message; the queued
	// acknowledgment is still emitted; no duplicate bubble.
	//
	// Mirrors persistUserMessageAtIngest's discipline:
	//   - context.WithoutCancel: a client disconnect on chat-exit (which
	//     cancels the inbound request ctx) MUST NOT abort this write.
	//   - SYNCHRONOUS (no goAsync): the row must be durable before the
	//     queued 200 is returned to the caller.
	//   - Best-effort: LogActivity already logs+swallows INSERT errors, so
	//     a hiccup never blocks or fails the user's send (behavior for
	//     that one request is never worse than the pre-fix async path).
	// The post-commit broadcast still fires inside LogActivity; a missed
	// WebSocket event is not data loss (the durable row is the truth the
	// canvas re-reads on reopen).
	insCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	var wsName string
	db.DB.QueryRowContext(insCtx, `SELECT name FROM workspaces WHERE id = $1`, workspaceID).Scan(&wsName)
	if wsName == "" {
		wsName = workspaceID
	}
	summary := a2aMethod + " → " + wsName + " (queued for poll)"
	LogActivity(insCtx, h.broadcaster, ActivityParams{
		WorkspaceID:  workspaceID,
		ActivityType: "a2a_receive",
		SourceID:     nilIfEmpty(callerID),
		TargetID:     &workspaceID,
		Method:       &a2aMethod,
		Summary:      &summary,
		RequestBody:  json.RawMessage(body),
		Status:       "ok",
		MessageId:    extractMessageIdFromA2ABody(body),
	})
}

// extractMessageIdFromA2ABody reads params.message.messageId out of a
// normalized A2A JSON-RPC body. Returns "" when the field is absent or
// the body is malformed — the empty value opts the activity row out of
// the messageId-keyed conflict path (the existing always-INSERT
// behavior is preserved for non-per-message activity).
func extractMessageIdFromA2ABody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var env struct {
		Params struct {
			Message struct {
				MessageID string `json:"messageId"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return env.Params.Message.MessageID
}

// persistUserMessageAtIngest writes the canvas user's outbound chat
// message to activity_logs SYNCHRONOUSLY, BEFORE the agent dispatch
// runs. The completion path (logA2ASuccess / logA2AReceiveQueued) then
// uses ON CONFLICT (workspace_id, message_id) DO UPDATE to attach the
// agent's response_body onto the SAME row, so a single activity_logs
// row carries both the user message and the agent reply — the chat-
// history reader (messagestore.PostgresMessageStore) emits one
// (user, agent) pair per row, no duplicate bubble.
//
// Why before-dispatch (issue #2560): pre-fix the user message only
// landed in activity_logs at turn completion (logA2ASuccess). A
// mid-turn leave/refresh re-hydrated an empty pane plus typing dots
// (the agent's currentTask was set but the request_body was missing),
// and a workspace-server restart / deploy / OOM between the canvas
// 200 and the goroutine's commit lost the message permanently
// (chat-history is read back from activity_logs; no row == no message
// on reopen). The synchronous ingest-row write closes both holes:
// leave/refresh always sees the user message in chat-history
// (request_body is there), and the message is durable on disk before
// dispatch starts.
//
// Discipline (mirrors logA2AReceiveQueued):
//   - context.WithoutCancel: a client disconnect on chat-exit (which
//     cancels the inbound request ctx) MUST NOT abort this write.
//   - SYNCHRONOUS (no goAsync): the row must be durable before dispatch.
//   - Best-effort: LogActivity logs+swallows INSERT errors internally,
//     so a DB hiccup never blocks the dispatch — behavior for that one
//     request is never worse than the pre-fix "completion-only" path.
//   - The post-commit broadcast fires inside LogActivity; the canvas
//     may render the user message optimistically (it already has the
//     local-state copy). A missed WS event is not data loss (durable
//     row is the truth the canvas re-reads on reopen).
//
// When to call: for every A2A proxy entry that the user initiated
// (canvas / canvas-user, or workspace-to-workspace delegation), AFTER
// access control + budget + normalizeA2APayload, BEFORE the
// delivery-mode / mock / dispatch short-circuits. The completion
// path (logA2ASuccess for push, logA2AReceiveQueued for poll, and
// the poll-ingest-persist pre-existing test) keys on the same
// messageId, so a duplicated ingest collapses via ON CONFLICT to a
// single row.
func (h *WorkspaceHandler) persistUserMessageAtIngest(
	ctx context.Context,
	workspaceID, callerID string,
	body []byte,
	a2aMethod string,
) {
	messageId := extractMessageIdFromA2ABody(body)
	// Without a messageId the row is not message-keyed; LogActivity's
	// ON CONFLICT path won't fire (the partial unique index excludes
	// NULL message_id) and we'd get a duplicate row if both ingest
	// and completion paths ran. Skip the ingest — completion is
	// authoritative for the non-message-keyed case (legacy a2a_send
	// payloads, system callers, etc.).
	if messageId == "" {
		return
	}

	insCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	var wsName string
	db.DB.QueryRowContext(insCtx, `SELECT name FROM workspaces WHERE id = $1`, workspaceID).Scan(&wsName)
	if wsName == "" {
		wsName = workspaceID
	}
	summary := a2aMethod + " → " + wsName + " (ingest)"
	LogActivity(insCtx, h.broadcaster, ActivityParams{
		WorkspaceID:  workspaceID,
		ActivityType: "a2a_receive",
		SourceID:     nilIfEmpty(callerID),
		TargetID:     &workspaceID,
		Method:       &a2aMethod,
		Summary:      &summary,
		RequestBody:  json.RawMessage(body),
		Status:       "ok",
		MessageId:    messageId,
	})

	// Cross-device sync (core#2697). After the user message is durably
	// persisted, broadcast a USER_MESSAGE event so every other device
	// connected to this workspace renders the bubble in real time.
	// Origin device already optimistically added the message via
	// onUserMessage; on the WS echo the same id, the client-side id-
	// based dedup collapses the duplicate (only the first writer wins).
	// Fire only on a successful persist path: if LogActivity swallowed
	// an INSERT error, the durable row is missing and a reload from
	// chat-history would NOT show the message — the broadcast would be
	// a phantom. Skipping keeps the "ws event mirrors on-disk truth"
	// contract.
	broadcastUserMessageFromA2ABody(h.broadcaster, workspaceID, messageId, body)
}

// readUsageMap extracts input_tokens / output_tokens from the "usage" key of m.
// Returns (0, 0, false) when the key is absent or contains no non-zero values.
func readUsageMap(m map[string]json.RawMessage) (inputTokens, outputTokens int64, ok bool) {
	rawUsage, has := m["usage"]
	if !has {
		return 0, 0, false
	}
	var usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	}
	if err := json.Unmarshal(rawUsage, &usage); err != nil {
		return 0, 0, false
	}
	if usage.InputTokens == 0 && usage.OutputTokens == 0 {
		return 0, 0, false
	}
	return usage.InputTokens, usage.OutputTokens, true
}

// broadcastUserMessageFromA2ABody sends a USER_MESSAGE WebSocket event
// derived from a canvas user's outbound A2A envelope. The event lets
// every device connected to the workspace render the message in real
// time (cross-device sync, core#2697).
//
// Why a server-side parser rather than re-broadcasting the raw body:
//   - The payload shape needs to mirror the AGENT_MESSAGE wire shape
//     ({message_id, content, attachments, workspace_id}) so the
//     client's useChatSocket consumer can run a single appendMessage
//     path for both directions.
//   - The raw body is a full A2A JSON-RPC envelope (method, params,
//     id, jsonrpc); the canvas listener would have to re-parse to
//     extract the message bits, duplicating the client-side logic.
//
// Why no-op on parse failure: the persistUserMessageAtIngest caller
// has already LogActivity'd the row with the raw body. A phantom-free
// contract (ws event only when we successfully parsed the user
// message) is more important than broadcasting malformed payloads — a
// client receiving a broken USER_MESSAGE could not dedup or render
// it usefully anyway, and the on-disk row remains the truth the
// canvas re-reads on reload.
func broadcastUserMessageFromA2ABody(
	broadcaster events.EventEmitter,
	workspaceID string,
	messageId string,
	body []byte,
) {
	if broadcaster == nil || messageId == "" {
		return
	}
	text, attachments := extractUserTextAndAttachments(body)
	if text == "" && len(attachments) == 0 {
		return
	}
	payload := map[string]interface{}{
		"message_id":   messageId,
		"content":      text,
		"workspace_id": workspaceID,
	}
	if len(attachments) > 0 {
		payload["attachments"] = attachments
	}
	broadcaster.BroadcastOnly(workspaceID, string(events.EventUserMessage), payload)
}

// extractUserTextAndAttachments pulls the user-typed text + any
// file attachments from an A2A message/send body. Mirrors the
// canvas-side parts walker (canvas/.../message-parser.ts) so the
// broadcast payload matches what the renderer would draw.
//
//   - text: the FIRST text-kind part's `text` (parts is treated as
//     ordered; pre-existing canvas UI also renders the first text).
//     Empty when no text part exists (attachments-only is valid).
//   - attachments: every file-kind part's {name, mimeType, uri, size},
//     preserved in order so the receiving device shows them in the
//     same sequence the origin typed.
//
// Returns ("", nil) on any parse error or shape mismatch — the
// caller treats that as "no broadcastable content" and skips the
// event (the durable row is still authoritative).
func extractUserTextAndAttachments(body []byte) (string, []map[string]interface{}) {
	if len(body) == 0 {
		return "", nil
	}
	var env struct {
		Params struct {
			Message struct {
				Parts []map[string]interface{} `json:"parts"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", nil
	}
	var text string
	var attachments []map[string]interface{}
	for _, p := range env.Params.Message.Parts {
		if p == nil {
			continue
		}
		kind, _ := p["kind"].(string)
		if kind == "" {
			kind, _ = p["type"].(string)
		}
		switch kind {
		case "text":
			if text == "" {
				if t, _ := p["text"].(string); t != "" {
					text = t
				}
			}
		case "file":
			att := map[string]interface{}{}
			if file, ok := p["file"].(map[string]interface{}); ok && file != nil {
				if name, _ := file["name"].(string); name != "" {
					att["name"] = name
				}
				if mt, _ := file["mimeType"].(string); mt != "" {
					att["mimeType"] = mt
				}
				if uri, _ := file["uri"].(string); uri != "" {
					att["uri"] = uri
				}
				if sz, ok := numericSizeFromAny(file["size"]); ok {
					att["size"] = sz
				}
			}
			// Only attach when we extracted a uri — a file part with
			// no uri is malformed; the canvas can't render it.
			if att["uri"] != "" {
				if _, hasName := att["name"]; !hasName {
					att["name"] = "file"
				}
				attachments = append(attachments, att)
			}
		}
	}
	return text, attachments
}

// numericSizeFromAny is the public helper variant of the canvas
// parser's numericSize — Go side has no shared helper, so we keep
// the local one to avoid a cross-package dependency for a one-liner.
// Matches the JSON-decoded shapes: float64 (default), int64, int.
func numericSizeFromAny(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

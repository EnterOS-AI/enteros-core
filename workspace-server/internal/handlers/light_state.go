package handlers

// light_state.go — Enter OS Light state primitives (contract C5).
//
// GUARD POLICY. A guard is a read of workspaces.light_state that produces the
// 409 a caller sees. It FAILS OPEN on a database error (logged): the safety
// does not rest on it, because every write that wakes, re-provisions or revives
// a row also carries models.LightWakeAllowedSQL in its WHERE clause, so a
// parking or parked row cannot be claimed even when the read failed. The
// guards only decide what the caller is told.
//
// TRANSITIONS. Every light_state change is one guarded UPDATE (the from-set in
// the WHERE, setLightState) that stamps light_state_at, broadcasts
// WORKSPACE_UPDATED with the new state and logs one line (lightTransition).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
)

// The message a caller gets when it tries to reach a parked Light agent.
const (
	lightParkedMessage  = "This agent is a Light agent and is parked. Open it to chat."
	lightParkingMessage = "This agent is a Light agent and is being parked. Open it to chat."
)

// readLightState returns workspaces.light_state ("" = not a Light agent).
func readLightState(ctx context.Context, workspaceID string) (string, error) {
	var state string
	err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(light_state, '') FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&state)
	return state, err
}

// lightRefusal is a Light guard's (or a failed Light step's) answer: the status
// and body the caller gets, and a Retry-After for a 503.
type lightRefusal struct {
	Status     int
	Body       gin.H
	RetryAfter string
}

func (r *lightRefusal) write(c *gin.Context) {
	if r.RetryAfter != "" {
		c.Header("Retry-After", r.RetryAfter)
	}
	c.AbortWithStatusJSON(r.Status, r.Body)
}

// code is the refusal's error code ("" when it has none).
func (r *lightRefusal) code() string {
	if r == nil {
		return ""
	}
	s, _ := r.Body["code"].(string)
	return s
}

// newLightRefusal builds a refusal in core's error shape plus extras.
func newLightRefusal(status int, code, message string, extra gin.H) *lightRefusal {
	body := gin.H{"error": message, "code": code}
	for k, v := range extra {
		body[k] = v
	}
	return &lightRefusal{Status: status, Body: body}
}

// lightParkedRefusal is the 409 for a workspace in a wake-blocking state, nil
// for any other state.
func lightParkedRefusal(state string) *lightRefusal {
	switch state {
	case models.LightParked:
		return &lightRefusal{Status: http.StatusConflict, Body: gin.H{
			"error": lightParkedMessage, "code": "LIGHT_AGENT_PARKED", "light_state": state,
		}}
	case models.LightParking:
		return &lightRefusal{Status: http.StatusConflict, Body: gin.H{
			"error": lightParkingMessage, "code": "LIGHT_AGENT_PARKING", "light_state": state,
		}}
	}
	return nil
}

// lightWakeRefusal is the guard of every wake path: the 409 when the workspace
// is parking or parked, else nil. A read error (or a missing row, which the
// caller reports its own way) is nil: see GUARD POLICY.
func lightWakeRefusal(ctx context.Context, workspaceID, surface string) *lightRefusal {
	state, err := readLightState(ctx, workspaceID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("light guard (%s): cannot read light_state of %s, proceeding; the write predicates still refuse a parked row: %v", surface, workspaceID, err)
		}
		return nil
	}
	ref := lightParkedRefusal(state)
	if ref != nil {
		log.Printf("light guard (%s): refused for %s (light_state=%s)", surface, workspaceID, state)
	}
	return ref
}

// lightEnabledRefusal is the 409 for an operation a Light agent never takes
// (manual hibernate: park it instead; provider switch: disable Light first),
// nil when the workspace is not a Light agent. Fails open like every guard.
func lightEnabledRefusal(ctx context.Context, workspaceID, surface, hint string) *lightRefusal {
	state, err := readLightState(ctx, workspaceID)
	if err != nil || state == "" {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("light guard (%s): cannot read light_state of %s, proceeding: %v", surface, workspaceID, err)
		}
		return nil
	}
	log.Printf("light guard (%s): refused for Light agent %s (light_state=%s)", surface, workspaceID, state)
	return &lightRefusal{Status: http.StatusConflict, Body: gin.H{
		"error": "This agent is a Light agent: " + hint, "code": "LIGHT_ENABLED", "light_state": state,
	}}
}

// lightOpLocks serialises the Light operations of one workspace inside this
// process (park, open, disable, run now and the monitor's steps). A request
// that finds the lock held answers 409 LIGHT_TRANSITION_IN_PROGRESS; the
// monitor skips the workspace. The database transitions stay guarded on their
// own, so two processes (a rollout overlap) still cannot both claim a step.
var lightOpLocks sync.Map

func lightOpLock(workspaceID string) *sync.Mutex {
	v, _ := lightOpLocks.LoadOrStore(workspaceID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// setLightState moves light_state from one of from ("" = NULL) to to ("" =
// NULL) and stamps light_state_at. changed is false when the row was not in
// the from-set (a concurrent transition won, or the state moved on).
func setLightState(ctx context.Context, exec activityExecutor, workspaceID string, from []string, to string) (changed bool, err error) {
	res, err := exec.ExecContext(ctx, `
		UPDATE workspaces
		SET light_state = NULLIF($3, ''), light_state_at = now(), updated_at = now()
		WHERE id = $1 AND COALESCE(light_state, '') = ANY($2)`,
		workspaceID, pq.Array(from), to)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// lightTransition is setLightState plus the broadcast and the log line every
// transition emits.
func (h *WorkspaceHandler) lightTransition(ctx context.Context, workspaceID, op string, trig lightTrigger, from []string, to string) (bool, error) {
	changed, err := setLightState(ctx, db.DB, workspaceID, from, to)
	result := "ok"
	switch {
	case err != nil:
		result = "error"
	case !changed:
		result = "not-in-state"
	}
	log.Printf("light op=%s ws=%s from=%v to=%q trigger=%s result=%s", op, workspaceID, from, to, trig, result)
	if err == nil && changed {
		h.broadcastLightState(workspaceID, to)
	}
	return changed, err
}

// broadcastLightState tells the canvas the agent's new light_state ("" = not
// a Light agent) through WORKSPACE_UPDATED: no new event type.
func (h *WorkspaceHandler) broadcastLightState(workspaceID, state string) {
	if h == nil || h.broadcaster == nil {
		return
	}
	var v interface{}
	if state != "" {
		v = state
	}
	h.broadcaster.BroadcastOnly(workspaceID, string(events.EventWorkspaceUpdated), map[string]interface{}{"light_state": v})
}

// recordLightError stores the last background Light failure for GET /light.
func recordLightError(ctx context.Context, workspaceID, code, message string) {
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE workspaces
		SET light_last_error = jsonb_build_object('code', $2::text, 'message', $3::text), light_last_error_at = now()
		WHERE id = $1`, workspaceID, code, message); err != nil {
		log.Printf("light: recording error %s for %s failed: %v", code, workspaceID, err)
	}
}

// clearLightError forgets the last background failure once a park or open
// completed.
func clearLightError(ctx context.Context, workspaceID string) {
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE workspaces SET light_last_error = NULL, light_last_error_at = NULL
		WHERE id = $1 AND light_last_error IS NOT NULL`, workspaceID); err != nil {
		log.Printf("light: clearing the last error of %s failed: %v", workspaceID, err)
	}
}

// Error codes that are also markers the monitor acts on.
const (
	// lightCodeOpenIncomplete: an open paused the CronJobs and has not claimed
	// 'opening' yet. Left behind by an interrupted open; the monitor resumes
	// the CronJobs of a parked agent that carries it (review 21718 O3).
	lightCodeOpenIncomplete = "LIGHT_OPEN_INCOMPLETE"
	// lightCodeRefreshOpenFailed: a refresh open did not bring the agent up;
	// it was parked again on its previous snapshot. Kept across that park's
	// resume, so the refresh back-off applies (review 21718 R4).
	lightCodeRefreshOpenFailed = "LIGHT_REFRESH_OPEN_FAILED"
	// lightCodeActionFailed: a queued action's applier failed during a refresh
	// cycle. Kept across the park that ends the cycle, so the back-off spaces
	// the retries (review 21718 N7).
	lightCodeActionFailed = "LIGHT_ACTION_FAILED"
)

// lightParkKeptErrors are the errors a completed park keeps: their timestamp
// is the refresh back-off of the cycle that recorded them.
var lightParkKeptErrors = []string{lightCodeRefreshOpenFailed, lightCodeActionFailed}

// markLightOpenIncomplete records the open marker before an open pauses the
// CronJobs. Unlike recordLightError it reports a failure: an open must not
// pause what nothing would resume.
func markLightOpenIncomplete(ctx context.Context, workspaceID string) error {
	_, err := db.DB.ExecContext(ctx, `
		UPDATE workspaces
		SET light_last_error = jsonb_build_object('code', $2::text, 'message', $3::text), light_last_error_at = now()
		WHERE id = $1`, workspaceID, lightCodeOpenIncomplete, lightOpenIncompleteMessage)
	return err
}

// recordLightErrorKeepingOpenMarker is recordLightError, except over the open
// marker: the monitor needs it to resume the schedules an open left paused.
func recordLightErrorKeepingOpenMarker(ctx context.Context, workspaceID, code, message string) {
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE workspaces
		SET light_last_error = jsonb_build_object('code', $2::text, 'message', $3::text), light_last_error_at = now()
		WHERE id = $1 AND COALESCE(light_last_error->>'code', '') <> $4`,
		workspaceID, code, message, lightCodeOpenIncomplete); err != nil {
		log.Printf("light: recording error %s for %s failed: %v", code, workspaceID, err)
	}
}

// recordLightParkingBusy is the one write a heartbeat of a 'parking' agent
// makes: whether it is busy (active_tasks, is_busy as the main heartbeat
// derives it), only while its pod is still up. The park's busy check and its
// hibernate claim (active_tasks = 0) then see a turn that started around the
// claim (review 21718 R3). Nothing that wakes, routes or revives the agent is
// written; heartbeats of a parked agent write nothing at all.
func recordLightParkingBusy(ctx context.Context, workspaceID string, activeTasks int, isBusy *bool) {
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE workspaces SET active_tasks = $2, is_busy = COALESCE($3, ($2 > 0))
		WHERE id = $1 AND light_state = 'parking' AND status IN ('online', 'degraded')`,
		workspaceID, activeTasks, isBusy); err != nil {
		log.Printf("light: recording the busy state of parking %s failed: %v", workspaceID, err)
	}
}

// clearLightParkError is clearLightError after a park reached 'parked': it
// keeps lightParkKeptErrors, whose timestamp is the refresh back-off.
func clearLightParkError(ctx context.Context, workspaceID string) {
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE workspaces SET light_last_error = NULL, light_last_error_at = NULL
		WHERE id = $1 AND light_last_error IS NOT NULL AND NOT (COALESCE(light_last_error->>'code', '') = ANY($2))`,
		workspaceID, pq.Array(lightParkKeptErrors)); err != nil {
		log.Printf("light: clearing the last error of %s failed: %v", workspaceID, err)
	}
}

// Pending-action kinds. A pending action is work queued for a parked agent:
// the Light monitor opens the agent, applies each action once it is online and
// idle, and parks it again. 'refresh' needs nothing applied beyond the open
// itself (the control plane's env snapshot is retaken at the next park).
const lightActionRefresh = "refresh"

// Who opened a Light agent last (workspaces.light_opened_by). A refresh-opened
// agent parks again as soon as it is online and idle; an owner-opened one only
// after the auto-park idle window.
const (
	lightOpenedByOwner   = "owner"
	lightOpenedByRefresh = "refresh"
)

// queueLightAction queues one action for the workspace's next open/park cycle.
// With dedupe, an identical unapplied action of the same kind makes it a no-op
// (queued false).
func queueLightAction(ctx context.Context, workspaceID, kind, source string, payload interface{}, dedupe bool) (queued bool, err error) {
	raw := []byte("{}")
	if payload != nil {
		if raw, err = json.Marshal(payload); err != nil {
			return false, err
		}
	}
	query := `
		INSERT INTO light_pending_actions (workspace_id, kind, payload, source)
		VALUES ($1, $2, $3::jsonb, $4)`
	if dedupe {
		// While a park is in flight, an action is never merged into a row at or
		// below the park's watermark: the park marks those applied, and this
		// change may postdate its env snapshot (review 21718 R1).
		query = `
		INSERT INTO light_pending_actions (workspace_id, kind, payload, source)
		SELECT $1, $2, $3::jsonb, $4
		WHERE NOT EXISTS (
			SELECT 1 FROM light_pending_actions a
			WHERE a.workspace_id = $1 AND a.kind = $2 AND a.applied_at IS NULL AND a.payload = $3::jsonb
			  AND a.id > COALESCE((SELECT w.light_park_watermark FROM workspaces w
			                       WHERE w.id = $1 AND w.light_state = 'parking'), 0))`
	}
	res, err := db.DB.ExecContext(ctx, query, workspaceID, kind, string(raw), source)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// queueLightRefresh queues a refresh: a change (secret, plugin, config) that a
// parked agent's control-plane env snapshot must pick up.
func queueLightRefresh(ctx context.Context, workspaceID, source string) (bool, error) {
	queued, err := queueLightAction(ctx, workspaceID, lightActionRefresh, source, nil, true)
	if err != nil {
		log.Printf("light: queueing a refresh of %s (source %s) failed: %v", workspaceID, source, err)
	} else if queued {
		log.Printf("light op=queue ws=%s kind=refresh source=%s", workspaceID, source)
	}
	return queued, err
}

// queueLightRefreshIfBlocked queues a refresh when the workspace is a parking
// or parked Light agent: the restart chokepoint (runRestartCycle) calls it for
// a mutation-driven restart it refused. A read error queues nothing (logged).
func queueLightRefreshIfBlocked(ctx context.Context, workspaceID, source string) bool {
	state, err := readLightState(ctx, workspaceID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("light: cannot read light_state of %s to queue a refresh: %v", workspaceID, err)
		}
		return false
	}
	if !models.LightBlocksWake(state) {
		return false
	}
	queued, _ := queueLightRefresh(ctx, workspaceID, source)
	return queued
}

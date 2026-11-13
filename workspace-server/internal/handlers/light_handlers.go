package handlers

// light_handlers.go — the owner-facing Enter OS Light API (contract C5;
// docs/api-protocol/light-api.md). Every route sits in the WorkspaceAuth group
// of /workspaces/:id. Management routes refuse the workspace's own token (a run
// pod holds it, and must not park, open, disable or run itself).
//
// Error bodies follow core's convention: {"error": "<sentence>", "code":
// "<UPPER_SNAKE>"} plus named extras.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/crypto"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

const (
	// lightSecretKey is the workspace secret that reaches the agent as
	// MOLECULE_LIGHT=1 (contract C1): the runtime keeps its own scheduler off
	// and the control plane refuses to park an agent without it.
	lightSecretKey   = "MOLECULE_LIGHT"
	lightSecretValue = "1"
	// lightRuntime is the only runtime Light v1 runs: only the hermes image
	// ships the copy step's leave-out list.
	lightRuntime = "hermes"
	// lightBackendTimeout bounds one control-plane Light call; the CP bounds its
	// own substrate work at 60 s.
	lightBackendTimeout = 75 * time.Second
	// lightRetryAfter is the Retry-After of a 503 when the CP gave none.
	lightRetryAfter = "10"
)

// LightHandler serves the /workspaces/:id/light routes.
type LightHandler struct {
	wh *WorkspaceHandler
}

// NewLightHandler binds the Light API to the workspace handler (its control
// plane client, broadcaster, restart and wake paths).
func NewLightHandler(wh *WorkspaceHandler) *LightHandler {
	return &LightHandler{wh: wh}
}

// lightWorkspaceRow is what the Light routes read of a workspace.
type lightWorkspaceRow struct {
	Name           string
	Status         string
	Runtime        string
	Kind           string
	LightState     string
	ActiveTasks    int
	RunTokenID     sql.NullString
	RetiredTokenID sql.NullString
}

// loadLightWorkspace reads a non-removed workspace; sql.ErrNoRows when absent.
func loadLightWorkspace(ctx context.Context, workspaceID string) (lightWorkspaceRow, error) {
	var r lightWorkspaceRow
	err := db.DB.QueryRowContext(ctx, `
		SELECT name, status, COALESCE(runtime, 'claude-code'), COALESCE(kind, 'workspace'),
		       COALESCE(light_state, ''), COALESCE(active_tasks, 0), light_run_token_id, light_retired_token_id
		FROM workspaces WHERE id = $1 AND status != 'removed'`, workspaceID,
	).Scan(&r.Name, &r.Status, &r.Runtime, &r.Kind, &r.LightState, &r.ActiveTasks, &r.RunTokenID, &r.RetiredTokenID)
	return r, err
}

// lightCode writes one Light error body and its log line.
func lightCode(c *gin.Context, op, workspaceID string, status int, code, message string, extra gin.H) {
	body := gin.H{"error": message, "code": code}
	for k, v := range extra {
		body[k] = v
	}
	log.Printf("light op=%s ws=%s status=%d code=%s", op, workspaceID, status, code)
	c.AbortWithStatusJSON(status, body)
}

// lightPreamble is every Light route's start: a canonical id, and (for
// management routes) a caller that is not the workspace itself.
func lightPreamble(c *gin.Context, op string, management bool) (string, bool) {
	id := c.Param("id")
	if !uuidRegex.MatchString(id) {
		lightCode(c, op, id, http.StatusBadRequest, "LIGHT_BAD_REQUEST", "invalid workspace ID", nil)
		return "", false
	}
	if management && callerWorkspaceID(c) == id {
		lightCode(c, op, id, http.StatusForbidden, "LIGHT_SELF_FORBIDDEN",
			"a workspace cannot manage its own Light state; call with the owner's credentials", nil)
		return "", false
	}
	return id, true
}

// lightLoad reads the workspace or answers 404/500.
func lightLoad(c *gin.Context, op, workspaceID string) (lightWorkspaceRow, bool) {
	row, err := loadLightWorkspace(c.Request.Context(), workspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		lightCode(c, op, workspaceID, http.StatusNotFound, "WORKSPACE_NOT_FOUND", "workspace not found", nil)
		return row, false
	}
	if err != nil {
		log.Printf("light op=%s ws=%s: workspace lookup failed: %v", op, workspaceID, err)
		lightCode(c, op, workspaceID, http.StatusInternalServerError, "LIGHT_FAILED", "failed to look up the workspace", nil)
		return row, false
	}
	return row, true
}

// lightTryLock takes the workspace's Light operation lock or answers 409.
func lightTryLock(c *gin.Context, op, workspaceID string) (unlock func(), ok bool) {
	lock := lightOpLock(workspaceID)
	if !lock.TryLock() {
		lightCode(c, op, workspaceID, http.StatusConflict, "LIGHT_TRANSITION_IN_PROGRESS",
			"another Light operation on this agent is in progress; retry shortly", nil)
		return nil, false
	}
	return lock.Unlock, true
}

// lightCPUnsupported reports whether a C4 error means "this tenant cannot run
// Light" (no control plane, or a substrate that is not Kubernetes).
func lightCPUnsupported(err error) bool {
	if errors.Is(err, provisioner.ErrLightNotConfigured) {
		return true
	}
	var ce *provisioner.LightCPError
	return errors.As(err, &ce) && (ce.Code == "LIGHT_UNSUPPORTED" || ce.HTTPStatus == http.StatusNotImplemented)
}

// lightCPNothingToDelete reports whether a C4 DELETE error means no Light
// object can exist (unsupported substrate, or no tenant namespace).
func lightCPNothingToDelete(err error) bool {
	if lightCPUnsupported(err) {
		return true
	}
	return provisioner.LightCPCode(err) == "LIGHT_TENANT_NOT_FOUND"
}

// writeLightCPError answers a failed control-plane Light call with the same
// code the control plane gave (contract C4), mapped to the owner's API.
func writeLightCPError(c *gin.Context, op, workspaceID string, err error) {
	var ce *provisioner.LightCPError
	switch {
	case lightCPUnsupported(err):
		lightCode(c, op, workspaceID, http.StatusNotImplemented, "LIGHT_UNSUPPORTED",
			"Light runs on the Kubernetes substrate only; this tenant cannot run Light schedules", nil)
	case errors.Is(err, provisioner.ErrLightCPUnavailable):
		retry := lightRetryAfter
		if errors.As(err, &ce) && ce.RetryAfter != "" {
			retry = ce.RetryAfter
		}
		log.Printf("light op=%s ws=%s: control plane unavailable: %v", op, workspaceID, err)
		c.Header("Retry-After", retry)
		lightCode(c, op, workspaceID, http.StatusServiceUnavailable, "LIGHT_BACKEND_UNAVAILABLE",
			"the control plane did not answer; nothing was changed by this step, retry shortly", nil)
	case errors.As(err, &ce):
		extra := gin.H{}
		if ce.Key != "" {
			extra["key"] = ce.Key
		}
		if ce.RunID != "" {
			extra["run_id"] = ce.RunID
		}
		detail := ce.Detail
		if detail == "" {
			detail = "the control plane refused the request"
		}
		switch {
		case ce.Code == "LIGHT_INVALID":
			invalid := ce.Invalid
			if invalid == nil {
				invalid = []provisioner.LightFieldError{}
			}
			extra["invalid"] = invalid
			lightCode(c, op, workspaceID, http.StatusUnprocessableEntity, "LIGHT_INVALID", detail, extra)
		case ce.Code == "LIGHT_TOO_LARGE":
			lightCode(c, op, workspaceID, http.StatusRequestEntityTooLarge, "LIGHT_TOO_LARGE", detail, extra)
		case ce.Code == "LIGHT_NOT_ENABLED":
			lightCode(c, op, workspaceID, http.StatusConflict, "LIGHT_RESTART_REQUIRED",
				"the agent is running without MOLECULE_LIGHT=1; restart it, then retry", extra)
		case ce.Code == "LIGHT_RUNTIME_UNSUPPORTED":
			lightCode(c, op, workspaceID, http.StatusUnprocessableEntity, "LIGHT_RUNTIME_UNSUPPORTED", detail, extra)
		case ce.HTTPStatus == http.StatusConflict || ce.HTTPStatus == http.StatusNotFound:
			lightCode(c, op, workspaceID, ce.HTTPStatus, ce.Code, detail, extra)
		default:
			log.Printf("light op=%s ws=%s: control plane answered %d %s", op, workspaceID, ce.HTTPStatus, ce.Code)
			lightCode(c, op, workspaceID, http.StatusBadGateway, "LIGHT_FAILED",
				"the control plane failed the Light operation", nil)
		}
	default:
		log.Printf("light op=%s ws=%s: failed: %v", op, workspaceID, err)
		lightCode(c, op, workspaceID, http.StatusInternalServerError, "LIGHT_FAILED", "the Light operation failed", nil)
	}
}

// lightTruthy reads MOLECULE_LIGHT the way the runtime does (is_light).
func lightTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// writeLightSecrets makes MOLECULE_LIGHT=1 and MOLECULE_CONTINUOUS=false the
// workspace's own secrets (the 24/7 switch off: an open Light agent idles into
// auto-park instead of waking itself). changed is false when both already held
// those values.
func writeLightSecrets(ctx context.Context, tx *sql.Tx, workspaceID string) (changed bool, err error) {
	for _, s := range []struct {
		key, value string
		holds      func(string) bool
	}{
		{lightSecretKey, lightSecretValue, lightTruthy},
		{continuousSecretKey, "false", func(v string) bool { return !continuousFromValue(v) }},
	} {
		var enc []byte
		var ver int
		err := tx.QueryRowContext(ctx,
			`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1 AND key = $2`,
			workspaceID, s.key).Scan(&enc, &ver)
		switch {
		case err == nil:
			if plain, decErr := crypto.DecryptVersioned(enc, ver); decErr == nil && s.holds(string(plain)) {
				continue
			}
		case !errors.Is(err, sql.ErrNoRows):
			return false, err
		}
		cipher, err := crypto.Encrypt([]byte(s.value))
		if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workspace_secrets (workspace_id, key, encrypted_value, encryption_version)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (workspace_id, key) DO UPDATE
				SET encrypted_value = $3, encryption_version = $4, updated_at = now()`,
			workspaceID, s.key, cipher, crypto.CurrentEncryptionVersion()); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

// applyLightEnvChange makes the agent pick up a Light env change: a hibernated
// agent is woken, an awake (or failed) one restarted. "woken", "started" or
// "none".
func (h *WorkspaceHandler) applyLightEnvChange(workspaceID, status string) string {
	switch models.WorkspaceStatus(status) {
	case models.StatusHibernated:
		h.goAsync(func() { h.WakeWorkspace(workspaceID) })
		return "woken"
	case models.StatusPaused, models.StatusRemoved, models.StatusHibernating:
		return "none"
	}
	h.goAsync(func() { h.RestartByIDAfterMutation(workspaceID) })
	return "started"
}

// Enable handles POST /workspaces/:id/light/enable.
//
//	@Summary	Make a workspace an Enter OS Light agent
//	@Description	Hermes agents on the Kubernetes substrate only. Stores MOLECULE_LIGHT=1 and MOLECULE_CONTINUOUS=false as workspace secrets, sets light_state=open and restarts (or wakes) the agent so its env applies.
//	@Tags		light
//	@Produce	json
//	@Param		id	path		string	true	"Workspace ID"
//	@Success	200	{object}	map[string]interface{}
//	@Failure	403	{object}	map[string]interface{}	"LIGHT_SELF_FORBIDDEN"
//	@Failure	404	{object}	map[string]interface{}	"WORKSPACE_NOT_FOUND"
//	@Failure	409	{object}	map[string]interface{}	"LIGHT_AGENT_PAUSED, LIGHT_TRANSITION_IN_PROGRESS"
//	@Failure	422	{object}	map[string]interface{}	"LIGHT_RUNTIME_UNSUPPORTED, LIGHT_PLATFORM_AGENT"
//	@Failure	501	{object}	map[string]interface{}	"LIGHT_UNSUPPORTED"
//	@Failure	503	{object}	map[string]interface{}	"LIGHT_BACKEND_UNAVAILABLE"
//	@Router		/workspaces/{id}/light/enable [post]
//	@Security	BearerAuth
func (h *LightHandler) Enable(c *gin.Context) {
	const op = "enable"
	id, ok := lightPreamble(c, op, true)
	if !ok {
		return
	}
	unlock, ok := lightTryLock(c, op, id)
	if !ok {
		return
	}
	defer unlock()
	ctx := c.Request.Context()

	row, ok := lightLoad(c, op, id)
	if !ok {
		return
	}
	if row.Kind == models.KindPlatform {
		lightCode(c, op, id, http.StatusUnprocessableEntity, "LIGHT_PLATFORM_AGENT",
			"the platform agent is the org root and stays awake; it cannot be a Light agent", nil)
		return
	}
	if row.Runtime != lightRuntime {
		lightCode(c, op, id, http.StatusUnprocessableEntity, "LIGHT_RUNTIME_UNSUPPORTED",
			"Light supports the hermes runtime only; this workspace runs "+row.Runtime, gin.H{"runtime": row.Runtime})
		return
	}
	if row.Status == string(models.StatusPaused) {
		lightCode(c, op, id, http.StatusConflict, "LIGHT_AGENT_PAUSED", "resume the workspace first", nil)
		return
	}
	if h.wh.lightAPI == nil {
		writeLightCPError(c, op, id, provisioner.ErrLightNotConfigured)
		return
	}
	if row.LightState == "" {
		// Capability probe: the jobs listing answers 501 on a substrate that
		// cannot run Light, so the owner learns it now rather than at park.
		pctx, cancel := context.WithTimeout(ctx, lightBackendTimeout)
		_, err := h.wh.lightAPI.ListLightJobs(pctx, id)
		cancel()
		if err != nil {
			writeLightCPError(c, op, id, err)
			return
		}
	}

	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		lightCode(c, op, id, http.StatusInternalServerError, "LIGHT_FAILED", "failed to enable Light", nil)
		return
	}
	defer func() { _ = tx.Rollback() }()
	secretsChanged, err := writeLightSecrets(ctx, tx, id)
	if err != nil {
		log.Printf("light op=enable ws=%s: writing the Light secrets failed: %v", id, err)
		lightCode(c, op, id, http.StatusInternalServerError, "LIGHT_FAILED", "failed to enable Light", nil)
		return
	}
	enabled := false
	if row.LightState == "" {
		res, err := tx.ExecContext(ctx, `
			UPDATE workspaces SET light_state = 'open', light_state_at = now(), light_opened_by = 'owner', updated_at = now()
			WHERE id = $1 AND light_state IS NULL`, id)
		if err == nil {
			n, _ := res.RowsAffected()
			enabled = n > 0
		}
		if err != nil {
			log.Printf("light op=enable ws=%s: setting light_state failed: %v", id, err)
			lightCode(c, op, id, http.StatusInternalServerError, "LIGHT_FAILED", "failed to enable Light", nil)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		lightCode(c, op, id, http.StatusInternalServerError, "LIGHT_FAILED", "failed to enable Light", nil)
		return
	}

	restart := "none"
	switch {
	case !secretsChanged:
	case models.LightBlocksWake(row.LightState):
		// A parking or parked agent stays asleep: the restored secret reaches
		// its control-plane env snapshot through a refresh cycle.
		if _, err := queueLightRefresh(ctx, id, "enable"); err == nil {
			restart = "refresh_queued"
		}
	default:
		restart = h.wh.applyLightEnvChange(id, row.Status)
	}
	state := row.LightState
	status := "unchanged"
	if enabled {
		state, status = models.LightOpen, "enabled"
		h.wh.broadcaster.BroadcastOnly(id, string(events.EventWorkspaceUpdated), map[string]interface{}{"light_state": models.LightOpen})
	}
	log.Printf("light op=enable ws=%s status=200 result=%s restart=%s", id, status, restart)
	c.JSON(http.StatusOK, gin.H{"status": status, "light_state": state, "restart": restart})
}

// deleteLightObjects removes the agent's control-plane Light objects (C4
// DELETE). A tenant that cannot run Light has none: that is success.
func (h *WorkspaceHandler) deleteLightObjects(ctx context.Context, workspaceID string) (provisioner.LightDeleteResult, error) {
	if h.lightAPI == nil {
		return provisioner.LightDeleteResult{}, nil
	}
	dctx, cancel := context.WithTimeout(ctx, lightBackendTimeout)
	defer cancel()
	res, err := h.lightAPI.DeleteLight(dctx, workspaceID)
	if err != nil && lightCPNothingToDelete(err) {
		return provisioner.LightDeleteResult{}, nil
	}
	return res, err
}

// lightCascadeDeleteBudget bounds, in total, the control-plane Light deletes
// one workspace delete makes (they run concurrently, before any compute stops).
const lightCascadeDeleteBudget = 20 * time.Second

// lightWorkspaceIDs returns which of ids are Light agents. Fail-open: on a read
// error it returns none, and those rows keep their light_state, which marks the
// Light objects as still to be deleted.
func lightWorkspaceIDs(ctx context.Context, ids []string) map[string]bool {
	out := map[string]bool{}
	if db.DB == nil || len(ids) == 0 {
		return out
	}
	rows, err := db.DB.QueryContext(ctx,
		`SELECT id FROM workspaces WHERE id = ANY($1::uuid[]) AND light_state IS NOT NULL`, pq.Array(ids))
	if err != nil {
		log.Printf("light: reading which of %d workspaces are Light failed: %v — their Light objects stay for a later delete", len(ids), err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// deleteLightForCascade removes the control-plane Light objects (CronJobs, run
// Jobs, run Secret) of every Light agent among ids, before the workspace delete
// stops any compute, so no scheduled run starts for a workspace being removed.
// Once an agent's objects are gone its Light columns are cleared; on a failure
// the row keeps light_state (the marker of objects still to delete) and the
// error is returned for the delete to surface as a stop failure.
func (h *WorkspaceHandler) deleteLightForCascade(ctx context.Context, ids []string) []error {
	// Detached like the rest of the teardown: a cancelled DELETE request still
	// removes the Light objects.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lightCascadeDeleteBudget)
	defer cancel()
	light := lightWorkspaceIDs(dctx, ids)
	if len(light) == 0 {
		return nil
	}
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	for id := range light {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := h.deleteLightObjects(dctx, id); err != nil {
				log.Printf("CascadeDelete %s: deleting its Light objects failed: %v — the row keeps light_state", id, err)
				mu.Lock()
				errs = append(errs, fmt.Errorf("light delete %s: %w", id, err))
				mu.Unlock()
				return
			}
			if _, err := db.DB.ExecContext(dctx, `
				UPDATE workspaces
				SET light_state = NULL, light_state_at = now(), light_opened_by = NULL,
				    light_run_token_id = NULL, light_retired_token_id = NULL
				WHERE id = $1`, id); err != nil {
				log.Printf("CascadeDelete %s: clearing its Light columns failed: %v (its Light objects are gone)", id, err)
			}
		}(id)
	}
	wg.Wait()
	return errs
}

// disableLight turns a Light agent back into an ordinary one: the control
// plane's Light objects first (its runs stop), then, in one transaction, the
// run tokens, the Light secrets, the state, the rendered schedules and the
// queued actions. The ledger keeps its history.
func (h *WorkspaceHandler) disableLight(ctx context.Context, workspaceID string, row lightWorkspaceRow) (provisioner.LightDeleteResult, error) {
	deleted, err := h.deleteLightObjects(ctx, workspaceID)
	if err != nil {
		return deleted, err
	}
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return deleted, err
	}
	defer func() { _ = tx.Rollback() }()
	var tokens []string
	for _, t := range []sql.NullString{row.RunTokenID, row.RetiredTokenID} {
		if t.Valid && t.String != "" {
			tokens = append(tokens, t.String)
		}
	}
	if len(tokens) > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE workspace_auth_tokens SET revoked_at = now()
			WHERE workspace_id = $1 AND id::text = ANY($2) AND revoked_at IS NULL`,
			workspaceID, pq.Array(tokens)); err != nil {
			return deleted, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM workspace_secrets WHERE workspace_id = $1 AND key = ANY($2)`,
		workspaceID, pq.Array([]string{lightSecretKey, continuousSecretKey})); err != nil {
		return deleted, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workspaces
		SET light_state = NULL, light_state_at = now(), light_opened_by = NULL,
		    light_run_token_id = NULL, light_retired_token_id = NULL,
		    light_last_error = NULL, light_last_error_at = NULL, updated_at = now()
		WHERE id = $1`, workspaceID); err != nil {
		return deleted, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM light_schedules WHERE workspace_id = $1`, workspaceID); err != nil {
		return deleted, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM light_pending_actions WHERE workspace_id = $1 AND applied_at IS NULL`, workspaceID); err != nil {
		return deleted, err
	}
	if err := tx.Commit(); err != nil {
		return deleted, err
	}
	if h.broadcaster != nil {
		h.broadcaster.BroadcastOnly(workspaceID, string(events.EventWorkspaceUpdated), map[string]interface{}{"light_state": nil})
	}
	return deleted, nil
}

// Disable handles POST /workspaces/:id/light/disable.
//
//	@Summary	Turn a Light agent back into an ordinary agent
//	@Description	Deletes the control plane's Light objects (CronJobs, Jobs, Secret) first, then revokes the run tokens, removes the Light secrets, clears light_state and wakes (or restarts) the agent. Works from any Light state.
//	@Tags		light
//	@Produce	json
//	@Param		id	path		string	true	"Workspace ID"
//	@Success	200	{object}	map[string]interface{}
//	@Failure	403	{object}	map[string]interface{}	"LIGHT_SELF_FORBIDDEN"
//	@Failure	404	{object}	map[string]interface{}	"WORKSPACE_NOT_FOUND"
//	@Failure	409	{object}	map[string]interface{}	"LIGHT_TRANSITION_IN_PROGRESS"
//	@Failure	503	{object}	map[string]interface{}	"LIGHT_BACKEND_UNAVAILABLE"
//	@Router		/workspaces/{id}/light/disable [post]
//	@Security	BearerAuth
func (h *LightHandler) Disable(c *gin.Context) {
	const op = "disable"
	id, ok := lightPreamble(c, op, true)
	if !ok {
		return
	}
	unlock, ok := lightTryLock(c, op, id)
	if !ok {
		return
	}
	defer unlock()

	row, ok := lightLoad(c, op, id)
	if !ok {
		return
	}
	if row.LightState == "" {
		c.JSON(http.StatusOK, gin.H{"status": "unchanged", "light_state": nil})
		return
	}
	deleted, err := h.wh.disableLight(c.Request.Context(), id, row)
	if err != nil {
		writeLightCPError(c, op, id, err)
		return
	}
	restart := h.wh.applyLightEnvChange(id, row.Status)
	log.Printf("light op=disable ws=%s status=200 from=%s restart=%s", id, row.LightState, restart)
	c.JSON(http.StatusOK, gin.H{"status": "disabled", "light_state": nil, "deleted": deleted, "restart": restart})
}

// lightScheduleView is one rendered schedule as GET /light reports it.
type lightScheduleView struct {
	Name                string     `json:"name"`
	Cron                string     `json:"cron"`
	Timezone            string     `json:"timezone"`
	Prompt              string     `json:"prompt"`
	Enabled             bool       `json:"enabled"`
	Suspended           bool       `json:"suspended"`
	SuspendedReason     *string    `json:"suspended_reason"`
	SuspendedAt         *time.Time `json:"suspended_at"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	CronJob             *string    `json:"cronjob"`
}

// lightPendingView is one queued action as GET /light reports it.
type lightPendingView struct {
	Kind     string    `json:"kind"`
	Source   string    `json:"source"`
	QueuedAt time.Time `json:"queued_at"`
}

// Get handles GET /workspaces/:id/light.
//
//	@Summary	A workspace's Enter OS Light state
//	@Description	light_state (null when not a Light agent), the schedules rendered at the last park with core's suspension record, the work queued for the next open/park cycle, and the last background failure.
//	@Tags		light
//	@Produce	json
//	@Param		id	path		string	true	"Workspace ID"
//	@Success	200	{object}	map[string]interface{}
//	@Failure	404	{object}	map[string]interface{}	"WORKSPACE_NOT_FOUND"
//	@Router		/workspaces/{id}/light [get]
//	@Security	BearerAuth
func (h *LightHandler) Get(c *gin.Context) {
	const op = "get"
	id, ok := lightPreamble(c, op, false)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	var state string
	var stateAt, errorAt sql.NullTime
	var lastError []byte
	err := db.DB.QueryRowContext(ctx, `
		SELECT COALESCE(light_state, ''), light_state_at, light_last_error, light_last_error_at
		FROM workspaces WHERE id = $1 AND status != 'removed'`, id,
	).Scan(&state, &stateAt, &lastError, &errorAt)
	if errors.Is(err, sql.ErrNoRows) {
		lightCode(c, op, id, http.StatusNotFound, "WORKSPACE_NOT_FOUND", "workspace not found", nil)
		return
	}
	if err != nil {
		log.Printf("light op=get ws=%s: %v", id, err)
		lightCode(c, op, id, http.StatusInternalServerError, "LIGHT_FAILED", "failed to read the Light state", nil)
		return
	}

	schedules := []lightScheduleView{}
	rows, err := db.DB.QueryContext(ctx, `
		SELECT name, cron, timezone, prompt, enabled, suspended_reason, suspended_at, consecutive_failures, cronjob
		FROM light_schedules WHERE workspace_id = $1 ORDER BY name`, id)
	if err != nil {
		log.Printf("light op=get ws=%s: schedules: %v", id, err)
		lightCode(c, op, id, http.StatusInternalServerError, "LIGHT_FAILED", "failed to read the Light schedules", nil)
		return
	}
	for rows.Next() {
		var s lightScheduleView
		var reason, cronjob sql.NullString
		var at sql.NullTime
		if err := rows.Scan(&s.Name, &s.Cron, &s.Timezone, &s.Prompt, &s.Enabled, &reason, &at, &s.ConsecutiveFailures, &cronjob); err != nil {
			rows.Close()
			lightCode(c, op, id, http.StatusInternalServerError, "LIGHT_FAILED", "failed to read the Light schedules", nil)
			return
		}
		if reason.Valid {
			s.SuspendedReason, s.Suspended = &reason.String, true
		}
		if at.Valid {
			t := at.Time.UTC()
			s.SuspendedAt = &t
		}
		if cronjob.Valid {
			s.CronJob = &cronjob.String
		}
		schedules = append(schedules, s)
	}
	rows.Close()

	pending := []lightPendingView{}
	prows, err := db.DB.QueryContext(ctx, `
		SELECT kind, source, created_at FROM light_pending_actions
		WHERE workspace_id = $1 AND applied_at IS NULL ORDER BY id`, id)
	if err != nil {
		log.Printf("light op=get ws=%s: pending actions: %v", id, err)
		lightCode(c, op, id, http.StatusInternalServerError, "LIGHT_FAILED", "failed to read the queued Light actions", nil)
		return
	}
	refreshPending := false
	for prows.Next() {
		var p lightPendingView
		if err := prows.Scan(&p.Kind, &p.Source, &p.QueuedAt); err != nil {
			prows.Close()
			lightCode(c, op, id, http.StatusInternalServerError, "LIGHT_FAILED", "failed to read the queued Light actions", nil)
			return
		}
		p.QueuedAt = p.QueuedAt.UTC()
		refreshPending = refreshPending || p.Kind == lightActionRefresh
		pending = append(pending, p)
	}
	prows.Close()

	body := gin.H{
		"workspace_id":    id,
		"light_state":     nil,
		"light_state_at":  nil,
		"refresh_pending": refreshPending,
		"pending_actions": pending,
		"last_error":      nil,
		"schedules":       schedules,
	}
	if state != "" {
		body["light_state"] = state
	}
	if stateAt.Valid {
		body["light_state_at"] = stateAt.Time.UTC()
	}
	if len(lastError) > 0 && errorAt.Valid {
		var le struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(lastError, &le) == nil {
			body["last_error"] = gin.H{"code": le.Code, "message": le.Message, "at": errorAt.Time.UTC()}
		}
	}
	c.JSON(http.StatusOK, body)
}

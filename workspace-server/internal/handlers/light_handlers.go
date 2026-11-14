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
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/cronspec"
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
	// Part 2: the busy flag the park checks, how long the state has held, and
	// what a park in flight recorded (its trigger, watermark and unrecorded
	// run token).
	IsBusy         bool
	StateAt        sql.NullTime
	ParkTrigger    string
	ParkWatermark  int64
	PendingTokenID sql.NullString
}

// loadLightWorkspace reads a non-removed workspace; sql.ErrNoRows when absent.
func loadLightWorkspace(ctx context.Context, workspaceID string) (lightWorkspaceRow, error) {
	var r lightWorkspaceRow
	err := db.DB.QueryRowContext(ctx, `
		SELECT name, status, COALESCE(runtime, 'claude-code'), COALESCE(kind, 'workspace'),
		       COALESCE(light_state, ''), COALESCE(active_tasks, 0), light_run_token_id, light_retired_token_id,
		       COALESCE(is_busy, false), light_state_at, COALESCE(light_park_trigger, ''),
		       COALESCE(light_park_watermark, 0), light_pending_token_id
		FROM workspaces WHERE id = $1 AND status != 'removed'`, workspaceID,
	).Scan(&r.Name, &r.Status, &r.Runtime, &r.Kind, &r.LightState, &r.ActiveTasks, &r.RunTokenID, &r.RetiredTokenID,
		&r.IsBusy, &r.StateAt, &r.ParkTrigger, &r.ParkWatermark, &r.PendingTokenID)
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
	if management && lightCallerIsTheWorkspace(c, id) {
		lightCode(c, op, id, http.StatusForbidden, "LIGHT_SELF_FORBIDDEN",
			"a workspace cannot manage its own Light state; call with the owner's credentials", nil)
		return "", false
	}
	return id, true
}

// lightCallerIsTheWorkspace reports whether the caller authenticated as the
// workspace itself (any of its tokens, a run pod's run token included). It
// uses the identity WorkspaceAuth already proved and recorded — no second
// token lookup that could fail open — and also refuses a caller that names
// the workspace in X-Workspace-ID.
func lightCallerIsTheWorkspace(c *gin.Context, workspaceID string) bool {
	return c.GetString("authenticated_workspace_id") == workspaceID ||
		strings.TrimSpace(c.GetHeader("X-Workspace-ID")) == workspaceID
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
	r := lightCPRefusal(op, workspaceID, err)
	log.Printf("light op=%s ws=%s status=%d code=%s", op, workspaceID, r.Status, r.code())
	r.write(c)
}

// lightCPRefusal maps a failed control-plane Light call to the owner's API.
func lightCPRefusal(op, workspaceID string, err error) *lightRefusal {
	var ce *provisioner.LightCPError
	switch {
	case lightCPUnsupported(err):
		return newLightRefusal(http.StatusNotImplemented, "LIGHT_UNSUPPORTED",
			"Light runs on the Kubernetes substrate only; this tenant cannot run Light schedules", nil)
	case errors.Is(err, provisioner.ErrLightCPUnavailable):
		retry := lightRetryAfter
		if errors.As(err, &ce) && ce.RetryAfter != "" {
			retry = ce.RetryAfter
		}
		log.Printf("light op=%s ws=%s: control plane unavailable: %v", op, workspaceID, err)
		r := newLightRefusal(http.StatusServiceUnavailable, "LIGHT_BACKEND_UNAVAILABLE",
			"the control plane did not answer; nothing was changed by this step, retry shortly", nil)
		r.RetryAfter = retry
		return r
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
			return newLightRefusal(http.StatusUnprocessableEntity, "LIGHT_INVALID", detail, extra)
		case ce.Code == "LIGHT_TOO_LARGE":
			return newLightRefusal(http.StatusRequestEntityTooLarge, "LIGHT_TOO_LARGE", detail, extra)
		case ce.Code == "LIGHT_NOT_ENABLED":
			return newLightRefusal(http.StatusConflict, "LIGHT_RESTART_REQUIRED",
				"the agent is running without MOLECULE_LIGHT=1; restart it, then retry", extra)
		case ce.Code == "LIGHT_RUNTIME_UNSUPPORTED":
			return newLightRefusal(http.StatusUnprocessableEntity, "LIGHT_RUNTIME_UNSUPPORTED", detail, extra)
		case ce.HTTPStatus == http.StatusConflict || ce.HTTPStatus == http.StatusNotFound:
			return newLightRefusal(ce.HTTPStatus, ce.Code, detail, extra)
		}
		log.Printf("light op=%s ws=%s: control plane answered %d %s", op, workspaceID, ce.HTTPStatus, ce.Code)
		return newLightRefusal(http.StatusBadGateway, "LIGHT_FAILED", "the control plane failed the Light operation", nil)
	}
	log.Printf("light op=%s ws=%s: failed: %v", op, workspaceID, err)
	return newLightRefusal(http.StatusInternalServerError, "LIGHT_FAILED", "the Light operation failed", nil)
}

// lightTruthy reads MOLECULE_LIGHT the way the runtime does (is_light).
func lightTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// lightSecretBefore is a workspace secret row as it was before enable wrote
// it (workspaces.light_continuous_before), so disable can restore exactly it.
// EncryptedValue is base64 in JSON.
type lightSecretBefore struct {
	Present           bool   `json:"present"`
	EncryptedValue    []byte `json:"encrypted_value,omitempty"`
	EncryptionVersion int    `json:"encryption_version,omitempty"`
}

// writeLightSecrets makes MOLECULE_LIGHT=1 and MOLECULE_CONTINUOUS=false the
// workspace's own secrets (the 24/7 switch off: an open Light agent idles into
// auto-park instead of waking itself). changed is false when both already held
// those values. continuousBefore is the MOLECULE_CONTINUOUS row as it was.
func writeLightSecrets(ctx context.Context, tx *sql.Tx, workspaceID string) (changed bool, continuousBefore lightSecretBefore, err error) {
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
			if s.key == continuousSecretKey {
				continuousBefore = lightSecretBefore{Present: true, EncryptedValue: enc, EncryptionVersion: ver}
			}
			if plain, decErr := crypto.DecryptVersioned(enc, ver); decErr == nil && s.holds(string(plain)) {
				continue
			}
		case !errors.Is(err, sql.ErrNoRows):
			return false, continuousBefore, err
		}
		cipher, err := crypto.Encrypt([]byte(s.value))
		if err != nil {
			return false, continuousBefore, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workspace_secrets (workspace_id, key, encrypted_value, encryption_version)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (workspace_id, key) DO UPDATE
				SET encrypted_value = $3, encryption_version = $4, updated_at = now()`,
			workspaceID, s.key, cipher, crypto.CurrentEncryptionVersion()); err != nil {
			return false, continuousBefore, err
		}
		changed = true
	}
	return changed, continuousBefore, nil
}

// restoreLightContinuous puts the workspace's MOLECULE_CONTINUOUS secret back
// as it was before enable (disable): no row → no row; a row → that row, byte
// for byte. Without a record (before is NULL) the secret is left as it is.
func restoreLightContinuous(ctx context.Context, tx *sql.Tx, workspaceID string, before []byte) error {
	if len(before) == 0 {
		log.Printf("light op=disable ws=%s: no record of the 24/7 setting before enable; MOLECULE_CONTINUOUS left as it is", workspaceID)
		return nil
	}
	var b lightSecretBefore
	if err := json.Unmarshal(before, &b); err != nil {
		log.Printf("light op=disable ws=%s: unreadable record of the 24/7 setting (%v); MOLECULE_CONTINUOUS left as it is", workspaceID, err)
		return nil
	}
	if !b.Present {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM workspace_secrets WHERE workspace_id = $1 AND key = $2`, workspaceID, continuousSecretKey)
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO workspace_secrets (workspace_id, key, encrypted_value, encryption_version)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (workspace_id, key) DO UPDATE
			SET encrypted_value = EXCLUDED.encrypted_value, encryption_version = EXCLUDED.encryption_version, updated_at = now()`,
		workspaceID, continuousSecretKey, b.EncryptedValue, b.EncryptionVersion)
	return err
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
	secretsChanged, continuousBefore, err := writeLightSecrets(ctx, tx, id)
	if err != nil {
		log.Printf("light op=enable ws=%s: writing the Light secrets failed: %v", id, err)
		lightCode(c, op, id, http.StatusInternalServerError, "LIGHT_FAILED", "failed to enable Light", nil)
		return
	}
	enabled := false
	if row.LightState == "" {
		// The 24/7 setting as it was, for disable to restore (only on the
		// transition: a repeated enable must not record its own write).
		before, err := json.Marshal(continuousBefore)
		if err != nil {
			lightCode(c, op, id, http.StatusInternalServerError, "LIGHT_FAILED", "failed to enable Light", nil)
			return
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE workspaces SET light_state = 'open', light_state_at = now(), light_opened_by = 'owner',
			       light_continuous_before = $2::jsonb, updated_at = now()
			WHERE id = $1 AND light_state IS NULL`, id, string(before))
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
			// Under the agent's Light lock when it can be had in time, so a park
			// cannot write objects behind the delete (the park also re-deletes
			// when it finds the workspace removed; review 21718 O4).
			unlock := lockLightForDelete(dctx, id)
			defer unlock()
			if _, err := h.deleteLightObjects(dctx, id); err != nil {
				log.Printf("CascadeDelete %s: deleting its Light objects failed: %v — the row keeps light_state", id, err)
				mu.Lock()
				errs = append(errs, fmt.Errorf("light delete %s: %w", id, err))
				mu.Unlock()
				return
			}
			clearLightColumnsAfterDelete(dctx, id)
		}(id)
	}
	wg.Wait()
	return errs
}

// lightDeleteLockWait bounds how long a workspace delete waits for a Light
// operation on the agent to finish before deleting without the lock.
var lightDeleteLockWait = 10 * time.Second

// lockLightForDelete takes the agent's Light lock, waiting up to
// lightDeleteLockWait; the returned func releases it (a no-op when the wait
// ran out and the delete goes ahead without it).
func lockLightForDelete(ctx context.Context, workspaceID string) (unlock func()) {
	lock := lightOpLock(workspaceID)
	deadline := time.Now().Add(lightDeleteLockWait)
	for {
		if lock.TryLock() {
			return lock.Unlock
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			log.Printf("CascadeDelete %s: a Light operation still holds the agent; deleting its Light objects without the lock", workspaceID)
			return func() {}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// clearLightColumnsAfterDelete forgets a deleted workspace's Light state once
// its control-plane objects are gone (its tokens were revoked by the delete).
func clearLightColumnsAfterDelete(ctx context.Context, workspaceID string) {
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE workspaces
		SET light_state = NULL, light_state_at = now(), light_opened_by = NULL,
		    light_run_token_id = NULL, light_retired_token_id = NULL, light_pending_token_id = NULL,
		    light_park_trigger = NULL, light_park_watermark = NULL
		WHERE id = $1`, workspaceID); err != nil {
		log.Printf("light: clearing the Light columns of deleted %s failed: %v (its Light objects are gone)", workspaceID, err)
	}
}

// disableLight turns a Light agent back into an ordinary one: the control
// plane's Light objects first (its runs stop), then, in one transaction, the
// run tokens, the MOLECULE_LIGHT secret, the 24/7 setting as it was before
// enable, the state, the rendered schedules and the queued actions. The ledger
// keeps its history.
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
	var continuousBefore []byte
	if err := tx.QueryRowContext(ctx,
		`SELECT light_continuous_before FROM workspaces WHERE id = $1 FOR UPDATE`, workspaceID,
	).Scan(&continuousBefore); err != nil {
		return deleted, err
	}
	var tokens []string
	for _, t := range []sql.NullString{row.RunTokenID, row.RetiredTokenID, row.PendingTokenID} {
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
		`DELETE FROM workspace_secrets WHERE workspace_id = $1 AND key = $2`, workspaceID, lightSecretKey); err != nil {
		return deleted, err
	}
	if err := restoreLightContinuous(ctx, tx, workspaceID, continuousBefore); err != nil {
		return deleted, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workspaces
		SET light_state = NULL, light_state_at = now(), light_opened_by = NULL,
		    light_run_token_id = NULL, light_retired_token_id = NULL, light_continuous_before = NULL,
		    light_pending_token_id = NULL, light_park_trigger = NULL, light_park_watermark = NULL,
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
//	@Description	Deletes the control plane's Light objects (CronJobs, Jobs, Secret) first, then revokes the run tokens, removes MOLECULE_LIGHT, restores the 24/7 setting (MOLECULE_CONTINUOUS) exactly as it was before enable, clears light_state and wakes (or restarts) the agent. Works from any Light state.
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

// lightTransitionTimeout bounds a park's or an open's synchronous part. It runs
// detached from the request, so a client that hangs up does not leave the
// agent half-way.
const lightTransitionTimeout = 3 * time.Minute

// lightParkScheduleView is one schedule as a park's answer reports it.
type lightParkScheduleView struct {
	Name      string `json:"name"`
	Cron      string `json:"cron"`
	Timezone  string `json:"timezone"`
	Enabled   bool   `json:"enabled"`
	Suspended bool   `json:"suspended"`
	CronJob   string `json:"cronjob"`
}

// Park handles POST /workspaces/:id/light/park.
//
//	@Summary	Park a Light agent
//	@Description	Hands the agent's schedules to the control plane as CronJobs, stops its pod and resumes the CronJobs. Answers 202 once the CronJobs are written; light_state goes parking → parked in the background (poll GET /workspaces/{id}/light). A refusal before the parking claim changes nothing; one after it returns the agent to open and records last_error.
//	@Tags		light
//	@Produce	json
//	@Param		id	path		string	true	"Workspace ID"
//	@Success	202	{object}	map[string]interface{}	"{\"light_state\":\"parking\",\"schedules\":[...],\"removed\":[...]}"
//	@Success	200	{object}	map[string]interface{}	"already parked"
//	@Failure	400	{object}	map[string]interface{}	"LIGHT_BAD_REQUEST"
//	@Failure	403	{object}	map[string]interface{}	"LIGHT_SELF_FORBIDDEN"
//	@Failure	404	{object}	map[string]interface{}	"WORKSPACE_NOT_FOUND, LIGHT_TENANT_NOT_FOUND"
//	@Failure	409	{object}	map[string]interface{}	"LIGHT_NOT_ENABLED, LIGHT_AGENT_NOT_AWAKE, LIGHT_AGENT_BUSY, LIGHT_RESTART_REQUIRED, LIGHT_ENV_UNSUPPORTED, LIGHT_TRANSITION_IN_PROGRESS"
//	@Failure	413	{object}	map[string]interface{}	"LIGHT_TOO_LARGE"
//	@Failure	422	{object}	map[string]interface{}	"LIGHT_INVALID, LIGHT_RUNTIME_UNSUPPORTED"
//	@Failure	500	{object}	map[string]interface{}	"LIGHT_FAILED"
//	@Failure	501	{object}	map[string]interface{}	"LIGHT_UNSUPPORTED"
//	@Failure	502	{object}	map[string]interface{}	"LIGHT_SCHEDULES_UNREADABLE, LIGHT_FAILED"
//	@Failure	503	{object}	map[string]interface{}	"LIGHT_BACKEND_UNAVAILABLE"
//	@Router		/workspaces/{id}/light/park [post]
//	@Security	BearerAuth
func (h *LightHandler) Park(c *gin.Context) {
	const op = "park"
	id, ok := lightPreamble(c, op, true)
	if !ok {
		return
	}
	lock := lightOpLock(id)
	if !lock.TryLock() {
		lightCode(c, op, id, http.StatusConflict, "LIGHT_TRANSITION_IN_PROGRESS",
			"another Light operation on this agent is in progress; retry shortly", nil)
		return
	}
	// Released on every way out (a panic included) unless the background
	// finish took it over.
	handedOff := false
	defer func() {
		if !handedOff {
			lock.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), lightTransitionTimeout)
	res, ref := h.wh.parkLight(ctx, c, id, lightTriggerOwner)
	cancel()
	if ref != nil {
		log.Printf("light op=park ws=%s status=%d code=%s", id, ref.Status, ref.code())
		ref.write(c)
		return
	}
	if res.Unchanged {
		c.JSON(http.StatusOK, gin.H{"status": "unchanged", "light_state": models.LightParked})
		return
	}
	// The pod stop and the resume run after the answer, still under the lock.
	handedOff = true
	h.wh.goAsync(func() {
		defer lock.Unlock()
		fctx, fcancel := context.WithTimeout(context.Background(), lightTransitionTimeout)
		defer fcancel()
		if err := h.wh.finishLightPark(fctx, id, lightTriggerOwner, lightResumeInlineBudget); err != nil {
			log.Printf("light op=park ws=%s: finishing failed: %v (the monitor retries)", id, err)
		}
	})
	views := make([]lightParkScheduleView, 0, len(res.Rendered))
	cronjobs := map[string]string{}
	for _, s := range res.Put.Schedules {
		cronjobs[s.Name] = s.CronJob
	}
	for _, r := range res.Rendered {
		views = append(views, lightParkScheduleView{
			Name: r.Schedule.Name, Cron: r.Schedule.Cron, Timezone: r.Schedule.Timezone,
			Enabled: r.Schedule.Enabled == nil || *r.Schedule.Enabled, Suspended: r.KeptSuspended,
			CronJob: cronjobs[r.Schedule.Name],
		})
	}
	removed := res.Put.Removed
	if removed == nil {
		removed = []string{}
	}
	log.Printf("light op=park ws=%s status=202 schedules=%d", id, len(views))
	c.JSON(http.StatusAccepted, gin.H{"light_state": models.LightParking, "schedules": views, "removed": removed})
}

// Open handles POST /workspaces/:id/light/open.
//
//	@Summary	Open a parked Light agent
//	@Description	Pauses the agent's CronJobs (runs that had not started yet are cancelled), then wakes its pod for chat. Answers 202; light_state goes opening → open once the pod is up. A failed pause leaves the agent parked. Also recovers a park stuck in parking for three minutes.
//	@Tags		light
//	@Produce	json
//	@Param		id	path		string	true	"Workspace ID"
//	@Success	202	{object}	map[string]interface{}	"{\"light_state\":\"opening\",\"cancelled\":[...]}"
//	@Success	200	{object}	map[string]interface{}	"already open or opening (the owner takes it over)"
//	@Failure	400	{object}	map[string]interface{}	"LIGHT_BAD_REQUEST"
//	@Failure	403	{object}	map[string]interface{}	"LIGHT_SELF_FORBIDDEN"
//	@Failure	404	{object}	map[string]interface{}	"WORKSPACE_NOT_FOUND, LIGHT_TENANT_NOT_FOUND"
//	@Failure	409	{object}	map[string]interface{}	"LIGHT_NOT_ENABLED, LIGHT_TRANSITION_IN_PROGRESS"
//	@Failure	500	{object}	map[string]interface{}	"LIGHT_FAILED"
//	@Failure	501	{object}	map[string]interface{}	"LIGHT_UNSUPPORTED"
//	@Failure	502	{object}	map[string]interface{}	"LIGHT_FAILED"
//	@Failure	503	{object}	map[string]interface{}	"LIGHT_BACKEND_UNAVAILABLE"
//	@Router		/workspaces/{id}/light/open [post]
//	@Security	BearerAuth
func (h *LightHandler) Open(c *gin.Context) {
	const op = "open"
	id, ok := lightPreamble(c, op, true)
	if !ok {
		return
	}
	unlock, ok := lightTryLock(c, op, id)
	if !ok {
		return
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), lightTransitionTimeout)
	defer cancel()
	cancelled, unchanged, state, ref := h.wh.openLight(ctx, id, lightTriggerOwner)
	if ref != nil {
		log.Printf("light op=open ws=%s status=%d code=%s", id, ref.Status, ref.code())
		ref.write(c)
		return
	}
	if unchanged {
		c.JSON(http.StatusOK, gin.H{"status": "unchanged", "light_state": state})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"light_state": state, "cancelled": cancelled})
}

// RunNow handles POST /workspaces/:id/light/run.
//
//	@Summary	Run one schedule of a parked Light agent now
//	@Description	Starts one run of the named schedule outside its cron times. Parked agents only.
//	@Tags		light
//	@Accept		json
//	@Produce	json
//	@Param		id	path		string	true	"Workspace ID"
//	@Success	201	{object}	map[string]interface{}	"{\"run_id\":\"light-...\"}"
//	@Failure	400	{object}	map[string]interface{}	"LIGHT_BAD_REQUEST"
//	@Failure	403	{object}	map[string]interface{}	"LIGHT_SELF_FORBIDDEN"
//	@Failure	404	{object}	map[string]interface{}	"WORKSPACE_NOT_FOUND, LIGHT_SCHEDULE_NOT_FOUND, LIGHT_TENANT_NOT_FOUND"
//	@Failure	500	{object}	map[string]interface{}	"LIGHT_FAILED"
//	@Failure	502	{object}	map[string]interface{}	"LIGHT_FAILED"
//	@Failure	409	{object}	map[string]interface{}	"LIGHT_NOT_PARKED, LIGHT_RUN_ACTIVE, LIGHT_RUN_EXISTS, LIGHT_AGENT_AWAKE, LIGHT_TENANT_SUSPENDED, LIGHT_TRANSITION_IN_PROGRESS"
//	@Failure	422	{object}	map[string]interface{}	"LIGHT_INVALID"
//	@Failure	501	{object}	map[string]interface{}	"LIGHT_UNSUPPORTED"
//	@Failure	503	{object}	map[string]interface{}	"LIGHT_BACKEND_UNAVAILABLE"
//	@Router		/workspaces/{id}/light/run [post]
//	@Security	BearerAuth
func (h *LightHandler) RunNow(c *gin.Context) {
	const op = "run"
	id, ok := lightPreamble(c, op, true)
	if !ok {
		return
	}
	var body struct {
		Schedule *string `json:"schedule"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10))
	dec.DisallowUnknownFields()
	err := dec.Decode(&body)
	if err == nil {
		// One object and nothing after it, as the control plane requires.
		if extra := dec.Decode(&struct{}{}); extra != io.EOF {
			err = errors.New("trailing data after the JSON object")
		}
	}
	if err != nil {
		lightCode(c, op, id, http.StatusBadRequest, "LIGHT_BAD_REQUEST", `the body must be {"schedule": "<name>"}`, nil)
		return
	}
	schedule := ""
	if body.Schedule != nil {
		schedule = *body.Schedule
	}
	if strings.TrimSpace(schedule) == "" {
		lightCode(c, op, id, http.StatusUnprocessableEntity, "LIGHT_INVALID", "name the schedule to run",
			gin.H{"invalid": []provisioner.LightFieldError{{Field: "schedule", Reason: "required"}}})
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
	if row.LightState != models.LightParked {
		var state interface{}
		if row.LightState != "" {
			state = row.LightState
		}
		lightCode(c, op, id, http.StatusConflict, "LIGHT_NOT_PARKED",
			"run now works on a parked agent only", gin.H{"light_state": state})
		return
	}
	if h.wh.lightAPI == nil {
		writeLightCPError(c, op, id, provisioner.ErrLightNotConfigured)
		return
	}
	rctx, cancel := context.WithTimeout(c.Request.Context(), lightBackendTimeout)
	runID, err := h.wh.lightAPI.RunLightNow(rctx, id, schedule)
	cancel()
	if err != nil {
		writeLightCPError(c, op, id, err)
		return
	}
	log.Printf("light op=run ws=%s schedule=%q run=%s status=201", id, schedule, runID)
	c.JSON(http.StatusCreated, gin.H{"run_id": runID})
}

// lightNow is the clock GET /light computes next_run_at with (a test seam).
var lightNow = time.Now

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
	NextRunAt           *time.Time `json:"next_run_at"`
}

// lightNextRunAt is a schedule's next fire time after now, in UTC: its cron in
// its time zone (no zone is UTC, the schedule grid's default and what the
// control plane renders), parsed the way the schedules proxy parses crons
// (cronspec: five fields, no descriptors). nil when the schedule is off or
// suspended, or its cron or zone does not parse. It is the schedule's own
// minute, whatever the Light state: a CronJob the control plane fires a minute
// early (start early) holds its turn until this time.
func lightNextRunAt(s lightScheduleView, now time.Time) *time.Time {
	if !s.Enabled || s.Suspended {
		return nil
	}
	tz := s.Timezone
	if tz == "" {
		tz = "UTC"
	}
	next, err := cronspec.ComputeNextRun(s.Cron, tz, now)
	if err != nil {
		return nil
	}
	return &next
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
	now := lightNow()
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
		s.NextRunAt = lightNextRunAt(s, now)
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

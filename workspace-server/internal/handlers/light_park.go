package handlers

// light_park.go — Enter OS Light transitions (contract C5): park, open and
// run now.
//
// PARK (open → parking → parked). The synchronous part (parkLight) checks the
// agent, claims 'parking', reads the agent's schedule grid through the
// schedules proxy, renders it, mints a run token and PUTs the set to the
// control plane (C4), which writes one CronJob per schedule, suspended and
// paused. The asynchronous part (finishLightPark) hibernates the agent through
// the ordinary hibernate path and resumes the CronJobs; the control plane
// answers 409 LIGHT_AGENT_AWAKE until the agent's pod has stopped, so the
// resume is retried. Any refusal before the pod stops puts the agent back to
// 'open' (its CronJobs are still paused) and revokes the token it minted.
//
// OPEN (parked → opening → open). The CronJobs pause FIRST, while the agent is
// still 'parked' and every wake path still refuses it; only then is 'opening'
// claimed and the agent woken. The Light monitor flips 'opening' to 'open'
// once the pod is up (light_monitor.go).
//
// Every transition runs under the workspace's Light lock (lightOpLock) and
// moves light_state with one guarded UPDATE, so a core restart mid-way leaves
// a state the Light monitor knows how to finish.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/crypto"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/wsauth"
)

// lightRunDeadlineSeconds bounds one run (the Job's activeDeadlineSeconds).
const lightRunDeadlineSeconds = 3600

// The resume after a park's hibernate: the control plane refuses it while the
// agent's pod is still stopping (~30 s grace), so it is retried this often for
// this long inline; past that the Light monitor carries on.
var (
	lightResumeRetryEvery   = 5 * time.Second
	lightResumeInlineBudget = 2 * time.Minute
)

// lightTrigger names who asked for a transition (log lines, suspension rules).
type lightTrigger string

const (
	lightTriggerOwner   lightTrigger = "owner"   // the owner's API call
	lightTriggerAuto    lightTrigger = "auto"    // auto-park after the idle window
	lightTriggerRefresh lightTrigger = "refresh" // a refresh cycle (queued work)
	lightTriggerMonitor lightTrigger = "monitor" // the monitor finishing a transition
)

// lightRendered is one schedule as a park sends it, with the agent's own
// enabled flag (what light_schedules keeps).
type lightRendered struct {
	Schedule      provisioner.LightSchedule
	GridEnabled   bool
	KeptSuspended bool
}

// lightScheduleRow is one stored light_schedules row.
type lightScheduleRow struct {
	Name, Cron, Timezone, Prompt string
	Enabled                      bool
	SuspendedReason              *string
}

// lightParkResult is what a park's synchronous part did.
type lightParkResult struct {
	// Unchanged: the agent was already parked; nothing was done.
	Unchanged bool
	// FinishOnly: the agent was 'parking' with its pod stopping or stopped,
	// so only finishLightPark is left (the PUT needs the agent's Deployment).
	FinishOnly bool
	Put        provisioner.LightPutResult
	Rendered   []lightRendered
}

// errLightParkAbandoned: the agent took a task after the PUT, so the park was
// undone and the agent is open again.
var errLightParkAbandoned = errors.New("light park abandoned: the agent became busy")

// lightSecretPresent reports whether the workspace's MOLECULE_LIGHT secret is
// set and truthy (enable wrote it; the control plane refuses a park without it).
func lightSecretPresent(ctx context.Context, workspaceID string) (bool, error) {
	var enc []byte
	var ver int
	err := db.DB.QueryRowContext(ctx,
		`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1 AND key = $2`,
		workspaceID, lightSecretKey).Scan(&enc, &ver)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	plain, err := crypto.DecryptVersioned(enc, ver)
	if err != nil {
		return false, err
	}
	return lightTruthy(string(plain)), nil
}

// lightGridLimit is the largest schedule grid a park reads: the control
// plane's PUT body limit (8 MiB: 100 schedules with prompts of up to 64 KiB
// fit). fetchScheduleAPI's 1 MiB cap would truncate such a grid.
const lightGridLimit = 8 << 20

// errLightGridTooLarge: the agent's grid is over lightGridLimit.
var errLightGridTooLarge = errors.New("the agent's schedule grid is over 8 MiB")

// readAgentSchedules reads the agent's schedule grid from the runtime's
// /internal/schedules through the same SSRF-safe client and inbound secret
// as the schedules proxy (or the test seam).
func (h *WorkspaceHandler) readAgentSchedules(ctx context.Context, workspaceID string) ([]volumeEntry, error) {
	if h.lightScheduleReader != nil {
		return h.lightScheduleReader(ctx, workspaceID)
	}
	wsURL, secret, err := resolveScheduleFanoutTarget(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	fctx, cancel := context.WithTimeout(ctx, scheduleForwardTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, strings.TrimRight(wsURL, "/")+"/internal/schedules", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	client := &http.Client{
		Timeout:   scheduleForwardTimeout,
		Transport: &http.Transport{DialContext: safeDialer().DialContext, TLSHandshakeTimeout: 10 * time.Second},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, lightGridLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > lightGridLimit {
		return nil, errLightGridTooLarge
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the schedules API answered %d", resp.StatusCode)
	}
	var payload struct {
		Schedules []volumeEntry `json:"schedules"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("malformed schedule grid: %w", err)
	}
	if payload.Schedules == nil {
		// A grid without the key is not an empty grid: parking on it would
		// delete every CronJob.
		return nil, errors.New(`the schedule grid has no "schedules" list`)
	}
	return payload.Schedules, nil
}

// loadLightScheduleRows reads the stored set of the last park.
func loadLightScheduleRows(ctx context.Context, workspaceID string) (map[string]lightScheduleRow, error) {
	rows, err := db.DB.QueryContext(ctx, `
		SELECT name, cron, timezone, prompt, enabled, suspended_reason
		FROM light_schedules WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]lightScheduleRow{}
	for rows.Next() {
		var r lightScheduleRow
		var reason sql.NullString
		if err := rows.Scan(&r.Name, &r.Cron, &r.Timezone, &r.Prompt, &r.Enabled, &reason); err != nil {
			return nil, err
		}
		if reason.Valid {
			r.SuspendedReason = &reason.String
		}
		out[r.Name] = r
	}
	return out, rows.Err()
}

// renderLightSchedules turns the agent's grid into the PUT's schedule set
// (sorted by name). A schedule core suspended after failed runs goes out with
// enabled:false — a PUT clears the control plane's own suspension — unless the
// owner parked (an owner park clears suspensions) or its definition changed.
func renderLightSchedules(grid []volumeEntry, stored map[string]lightScheduleRow, trig lightTrigger) []lightRendered {
	out := make([]lightRendered, 0, len(grid))
	for _, e := range grid {
		enabled := e.Enabled
		kept := false
		if s, ok := stored[e.Name]; ok && s.SuspendedReason != nil && trig != lightTriggerOwner &&
			s.Cron == e.Cron && s.Timezone == e.Timezone && s.Prompt == e.Prompt {
			enabled, kept = false, true
		}
		on := enabled
		out = append(out, lightRendered{
			Schedule:      provisioner.LightSchedule{Name: e.Name, Cron: e.Cron, Timezone: e.Timezone, Prompt: e.Prompt, Enabled: &on},
			GridEnabled:   e.Enabled,
			KeptSuspended: kept,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Schedule.Name < out[j].Schedule.Name })
	return out
}

// mintLightRunToken mints the API-kind run token the control plane's Secret
// will hold, and audits the mint (prefix only). c is the owner's request, nil
// for the monitor.
func mintLightRunToken(ctx context.Context, c *gin.Context, workspaceID string, trig lightTrigger) (id, plaintext string, err error) {
	id, plaintext, err = wsauth.IssueAPITokenWithID(ctx, db.DB, workspaceID)
	if err != nil {
		return "", "", err
	}
	details := map[string]any{
		"workspace_id": workspaceID,
		"route":        "light-park",
		"trigger":      string(trig),
		"token_prefix": auditTokenPrefix(plaintext),
	}
	entry := auditEntryFromGin(c, workspaceID, AuditOpTokenMint, true, details)
	if c == nil {
		entry.Actor = "light-monitor"
	}
	RecordAuditEvent(ctx, db.DB, entry)
	return id, plaintext, nil
}

// revokeLightToken revokes one run token (logged on failure).
func revokeLightToken(ctx context.Context, workspaceID, tokenID string) {
	if tokenID == "" {
		return
	}
	if _, err := wsauth.RevokeTokenByID(ctx, db.DB, workspaceID, tokenID); err != nil {
		log.Printf("light: revoking run token %s of %s failed: %v", tokenID, workspaceID, err)
	}
}

// recordPendingLightToken notes the run token a park just minted, before the
// PUT hands it to the control plane: an interruption before commit leaves it
// here for the next park, an open or a disable to revoke (review 21718 N1). A
// pending token an earlier attempt left behind is revoked as it is replaced:
// once overwritten, nothing could reach it (review 21719 N-A).
func recordPendingLightToken(ctx context.Context, workspaceID, tokenID string) error {
	var prev sql.NullString
	err := db.DB.QueryRowContext(ctx, `
		UPDATE workspaces w SET light_pending_token_id = $2
		FROM workspaces prev
		WHERE w.id = $1 AND prev.id = $1
		RETURNING prev.light_pending_token_id`, workspaceID, tokenID).Scan(&prev)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if prev.Valid && prev.String != "" && prev.String != tokenID {
		log.Printf("light: revoking the run token an interrupted park of %s left pending", workspaceID)
		revokeLightToken(ctx, workspaceID, prev.String)
	}
	return nil
}

// dropPendingLightToken revokes a park's unrecorded run token and forgets it.
func dropPendingLightToken(ctx context.Context, workspaceID, tokenID string) {
	if tokenID == "" {
		return
	}
	revokeLightToken(ctx, workspaceID, tokenID)
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET light_pending_token_id = NULL WHERE id = $1 AND light_pending_token_id = $2`,
		workspaceID, tokenID); err != nil {
		log.Printf("light: forgetting the pending run token of %s failed: %v", workspaceID, err)
	}
}

// errLightWorkspaceRemoved: the workspace was deleted while a park ran.
var errLightWorkspaceRemoved = errors.New("light: the workspace was deleted during the park")

// commitLightRunToken points the workspace at the token the control plane now
// holds, in one transaction (Decision 6: at most two run tokens are live).
// Normally the previous token is retired (runs of the last park may still hold
// it) and the token retired before it is revoked. When the previous token was
// minted during this same park (a park continued after an interruption, which
// never resumed its CronJobs), no run ever held it: it is revoked instead and
// the retired one kept. A workspace deleted meanwhile commits nothing
// (errLightWorkspaceRemoved).
func commitLightRunToken(ctx context.Context, workspaceID, newID string) error {
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	var cur, ret sql.NullString
	var curThisPark bool
	if err := tx.QueryRowContext(ctx, `
		SELECT w.status, w.light_run_token_id, w.light_retired_token_id,
		       COALESCE(t.created_at >= w.light_state_at, false)
		FROM workspaces w
		LEFT JOIN workspace_auth_tokens t ON t.id = w.light_run_token_id AND t.workspace_id = w.id
		WHERE w.id = $1
		FOR UPDATE OF w`, workspaceID,
	).Scan(&status, &cur, &ret, &curThisPark); err != nil {
		return err
	}
	if status == string(models.StatusRemoved) {
		return errLightWorkspaceRemoved
	}
	revoke := ret
	update := `UPDATE workspaces SET light_retired_token_id = light_run_token_id, light_run_token_id = $2, light_pending_token_id = NULL WHERE id = $1`
	if curThisPark {
		revoke = cur
		update = `UPDATE workspaces SET light_run_token_id = $2, light_pending_token_id = NULL WHERE id = $1`
	}
	if _, err := tx.ExecContext(ctx, update, workspaceID, newID); err != nil {
		return err
	}
	if revoke.Valid && revoke.String != "" && revoke.String != newID {
		if _, err := tx.ExecContext(ctx, `
			UPDATE workspace_auth_tokens SET revoked_at = now()
			WHERE workspace_id = $1 AND id = $2 AND revoked_at IS NULL`, workspaceID, revoke.String); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// storeLightSchedules records the set the control plane now holds. An owner
// park or an edited definition clears core's suspension of a schedule; the
// time of the last suspension (suspended_at) stays, and only runs after it
// count toward a new failure streak.
func storeLightSchedules(ctx context.Context, workspaceID string, rendered []lightRendered, put provisioner.LightPutResult, trig lightTrigger) error {
	cronjobs := map[string]string{}
	for _, s := range put.Schedules {
		cronjobs[s.Name] = s.CronJob
	}
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	names := make([]string, 0, len(rendered))
	for _, r := range rendered {
		s := r.Schedule
		names = append(names, s.Name)
		var cronjob interface{}
		if cj := cronjobs[s.Name]; cj != "" {
			cronjob = cj
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO light_schedules (workspace_id, name, cron, timezone, prompt, enabled, cronjob, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, now())
			ON CONFLICT (workspace_id, name) DO UPDATE SET
				suspended_reason = CASE
					WHEN $8 OR light_schedules.cron <> EXCLUDED.cron OR light_schedules.timezone <> EXCLUDED.timezone
					     OR light_schedules.prompt <> EXCLUDED.prompt
					THEN NULL ELSE light_schedules.suspended_reason END,
				cron = EXCLUDED.cron, timezone = EXCLUDED.timezone, prompt = EXCLUDED.prompt,
				enabled = EXCLUDED.enabled, cronjob = EXCLUDED.cronjob, updated_at = now()`,
			workspaceID, s.Name, s.Cron, s.Timezone, s.Prompt, r.GridEnabled, cronjob, trig == lightTriggerOwner); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM light_schedules WHERE workspace_id = $1 AND NOT (name = ANY($2))`,
		workspaceID, pq.Array(names)); err != nil {
		return err
	}
	return tx.Commit()
}

// claimLightParking moves an open agent to 'parking' and records, in the same
// statement, who asked (a continued park keeps it) and the refresh watermark:
// the newest unapplied refresh at the claim. The park's env snapshot covers
// exactly those; a refresh queued after the claim stays queued (review 21718
// R1). claimed is false when the agent was no longer open.
func (h *WorkspaceHandler) claimLightParking(ctx context.Context, workspaceID string, trig lightTrigger) (watermark int64, claimed bool, err error) {
	err = db.DB.QueryRowContext(ctx, `
		UPDATE workspaces
		SET light_state = 'parking', light_state_at = now(), updated_at = now(),
		    light_park_trigger = $2,
		    light_park_watermark = (SELECT COALESCE(MAX(a.id), 0) FROM light_pending_actions a
		                            WHERE a.workspace_id = $1 AND a.kind = $3 AND a.applied_at IS NULL)
		WHERE id = $1 AND light_state = 'open'
		RETURNING light_park_watermark`, workspaceID, string(trig), lightActionRefresh,
	).Scan(&watermark)
	if errors.Is(err, sql.ErrNoRows) {
		log.Printf("light op=park ws=%s from=[open] to=\"parking\" trigger=%s result=not-in-state", workspaceID, trig)
		return 0, false, nil
	}
	if err != nil {
		log.Printf("light op=park ws=%s from=[open] to=\"parking\" trigger=%s result=error: %v", workspaceID, trig, err)
		return 0, false, err
	}
	log.Printf("light op=park ws=%s from=[open] to=\"parking\" trigger=%s result=ok watermark=%d", workspaceID, trig, watermark)
	h.broadcastLightState(workspaceID, models.LightParking)
	return watermark, true, nil
}

// markLightRefreshApplied marks the refreshes a park's PUT covered.
func markLightRefreshApplied(ctx context.Context, workspaceID string, upTo int64) {
	if upTo <= 0 {
		return
	}
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE light_pending_actions SET applied_at = now()
		WHERE workspace_id = $1 AND kind = $2 AND applied_at IS NULL AND id <= $3`,
		workspaceID, lightActionRefresh, upTo); err != nil {
		log.Printf("light: marking the refreshes of %s applied failed: %v", workspaceID, err)
	}
}

// lightParkPrecondition is what a park needs of the agent before anything is
// written: hermes, online and idle, its Light secret, a control plane.
func (h *WorkspaceHandler) lightParkPrecondition(ctx context.Context, workspaceID string, row lightWorkspaceRow, awake bool) *lightRefusal {
	if row.Runtime != lightRuntime {
		return newLightRefusal(http.StatusUnprocessableEntity, "LIGHT_RUNTIME_UNSUPPORTED",
			"Light supports the hermes runtime only; this workspace runs "+row.Runtime, gin.H{"runtime": row.Runtime})
	}
	if !awake {
		return newLightRefusal(http.StatusConflict, "LIGHT_AGENT_NOT_AWAKE",
			"only an online agent can be parked; wait until it is online", gin.H{"status": row.Status})
	}
	if row.ActiveTasks > 0 || row.IsBusy {
		return newLightRefusal(http.StatusConflict, "LIGHT_AGENT_BUSY",
			"the agent is working on a task; park it once it is idle", gin.H{"active_tasks": row.ActiveTasks, "is_busy": row.IsBusy})
	}
	ok, err := lightSecretPresent(ctx, workspaceID)
	if err != nil {
		log.Printf("light op=park ws=%s: reading the Light secret failed: %v", workspaceID, err)
		return newLightRefusal(http.StatusInternalServerError, "LIGHT_FAILED", "failed to read the agent's Light secret", nil)
	}
	if !ok {
		return newLightRefusal(http.StatusConflict, "LIGHT_RESTART_REQUIRED",
			"the agent's MOLECULE_LIGHT secret is missing; call POST /workspaces/:id/light/enable, then park", nil)
	}
	if h.lightAPI == nil {
		return lightCPRefusal("park", workspaceID, provisioner.ErrLightNotConfigured)
	}
	return nil
}

// parkLight is a park's synchronous part (see the file comment). c is the
// owner's request (audit), nil for the monitor. The caller holds the
// workspace's Light lock and, on success, runs finishLightPark.
func (h *WorkspaceHandler) parkLight(ctx context.Context, c *gin.Context, workspaceID string, trig lightTrigger) (lightParkResult, *lightRefusal) {
	var res lightParkResult
	row, err := loadLightWorkspace(ctx, workspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return res, newLightRefusal(http.StatusNotFound, "WORKSPACE_NOT_FOUND", "workspace not found", nil)
	}
	if err != nil {
		log.Printf("light op=park ws=%s: workspace lookup failed: %v", workspaceID, err)
		return res, newLightRefusal(http.StatusInternalServerError, "LIGHT_FAILED", "failed to look up the workspace", nil)
	}
	awake := row.Status == string(models.StatusOnline) || row.Status == string(models.StatusDegraded)
	continuing := false
	switch row.LightState {
	case "":
		return res, newLightRefusal(http.StatusConflict, "LIGHT_NOT_ENABLED", "this workspace is not a Light agent; enable Light first", nil)
	case models.LightParked:
		res.Unchanged = true
		return res, nil
	case models.LightOpening:
		return res, newLightRefusal(http.StatusConflict, "LIGHT_TRANSITION_IN_PROGRESS",
			"the agent is opening; park it once it is open", gin.H{"light_state": row.LightState})
	case models.LightParking:
		if !awake {
			res.FinishOnly = true
			return res, nil
		}
		continuing = true
		// A continued park is the original one: its trigger (an owner park
		// clears suspensions) and its refresh watermark (review 21718 N4, N5).
		if row.ParkTrigger != "" {
			trig = lightTrigger(row.ParkTrigger)
		}
	}

	// Once 'parking' is claimed, every refusal puts the agent back to open.
	undo := func(ref *lightRefusal) (lightParkResult, *lightRefusal) {
		if _, err := h.lightTransition(ctx, workspaceID, "park", trig, []string{models.LightParking}, models.LightOpen); err != nil {
			log.Printf("light op=park ws=%s: returning to open failed: %v (the monitor retries)", workspaceID, err)
		}
		recordLightError(ctx, workspaceID, ref.code(), fmt.Sprint(ref.Body["error"]))
		return lightParkResult{}, ref
	}
	if continuing && row.PendingTokenID.Valid {
		// The interrupted attempt minted a token it never recorded; no run ever
		// held it (the CronJobs were never resumed). Revoked before anything can
		// refuse the continuation (review 21719 N-A).
		dropPendingLightToken(ctx, workspaceID, row.PendingTokenID.String)
	}
	if ref := h.lightParkPrecondition(ctx, workspaceID, row, awake); ref != nil {
		if continuing {
			return undo(ref)
		}
		return res, ref
	}

	watermark := row.ParkWatermark
	if !continuing {
		wm, claimed, err := h.claimLightParking(ctx, workspaceID, trig)
		if err != nil {
			return res, newLightRefusal(http.StatusInternalServerError, "LIGHT_FAILED", "failed to start the park", nil)
		}
		if !claimed {
			return res, newLightRefusal(http.StatusConflict, "LIGHT_TRANSITION_IN_PROGRESS",
				"another Light operation on this agent is in progress; retry shortly", nil)
		}
		watermark = wm
	}

	grid, err := h.readAgentSchedules(ctx, workspaceID)
	if errors.Is(err, errLightGridTooLarge) {
		return undo(newLightRefusal(http.StatusRequestEntityTooLarge, "LIGHT_TOO_LARGE",
			"the agent's schedules are over 8 MiB; shorten prompts or remove schedules", nil))
	}
	if err != nil {
		log.Printf("light op=park ws=%s: reading the schedule grid failed: %v", workspaceID, err)
		return undo(newLightRefusal(http.StatusBadGateway, "LIGHT_SCHEDULES_UNREADABLE",
			"the agent's schedules could not be read; it stays open", nil))
	}
	stored, err := loadLightScheduleRows(ctx, workspaceID)
	if err != nil {
		log.Printf("light op=park ws=%s: reading the stored schedules failed: %v", workspaceID, err)
		return undo(newLightRefusal(http.StatusInternalServerError, "LIGHT_FAILED", "failed to read the stored schedules", nil))
	}
	rendered := renderLightSchedules(grid, stored, trig)

	tokenID, token, err := mintLightRunToken(ctx, c, workspaceID, trig)
	if err != nil {
		log.Printf("light op=park ws=%s: minting the run token failed: %v", workspaceID, err)
		return undo(newLightRefusal(http.StatusInternalServerError, "LIGHT_FAILED", "failed to mint the run token", nil))
	}
	if err := recordPendingLightToken(ctx, workspaceID, tokenID); err != nil {
		log.Printf("light op=park ws=%s: noting the run token failed: %v", workspaceID, err)
		revokeLightToken(ctx, workspaceID, tokenID)
		return undo(newLightRefusal(http.StatusInternalServerError, "LIGHT_FAILED", "failed to record the run token", nil))
	}
	req := provisioner.LightPutRequest{Schedules: make([]provisioner.LightSchedule, 0, len(rendered)), RunToken: token, DeadlineSeconds: lightRunDeadlineSeconds}
	for _, r := range rendered {
		req.Schedules = append(req.Schedules, r.Schedule)
	}
	pctx, cancel := context.WithTimeout(ctx, lightBackendTimeout)
	put, err := h.lightAPI.PutLight(pctx, workspaceID, req)
	cancel()
	if err != nil {
		dropPendingLightToken(ctx, workspaceID, tokenID)
		return undo(lightCPRefusal("park", workspaceID, err))
	}
	if err := commitLightRunToken(ctx, workspaceID, tokenID); err != nil {
		if errors.Is(err, errLightWorkspaceRemoved) {
			// Deleted while the PUT ran: the delete may have missed the objects
			// this PUT just wrote, so delete them again (review 21718 O4).
			log.Printf("light op=park ws=%s: the workspace was deleted during the park; deleting its Light objects again", workspaceID)
			if _, derr := h.deleteLightObjects(ctx, workspaceID); derr != nil {
				log.Printf("light op=park ws=%s: deleting the Light objects of the deleted workspace failed: %v (the monitor retries)", workspaceID, derr)
				// Keep the marker the removed-workspace sweep looks for.
				if _, err := db.DB.ExecContext(ctx, `
					UPDATE workspaces SET light_state = 'parking', light_state_at = now()
					WHERE id = $1 AND status = 'removed' AND light_state IS NULL`, workspaceID); err != nil {
					log.Printf("light op=park ws=%s: restoring the removed-workspace marker failed: %v", workspaceID, err)
				}
			}
			dropPendingLightToken(ctx, workspaceID, tokenID)
			return lightParkResult{}, newLightRefusal(http.StatusNotFound, "WORKSPACE_NOT_FOUND", "workspace not found", nil)
		}
		// The control plane's CronJobs are still paused, so open is consistent.
		log.Printf("light op=park ws=%s: recording the run token failed: %v", workspaceID, err)
		dropPendingLightToken(ctx, workspaceID, tokenID)
		return undo(newLightRefusal(http.StatusInternalServerError, "LIGHT_FAILED", "failed to record the run token", nil))
	}
	if err := storeLightSchedules(ctx, workspaceID, rendered, put, trig); err != nil {
		log.Printf("light op=park ws=%s: storing the schedule set failed: %v (the next park rewrites it)", workspaceID, err)
	}
	markLightRefreshApplied(ctx, workspaceID, watermark)
	log.Printf("light op=park ws=%s trigger=%s schedules=%d removed=%d continuing=%v", workspaceID, trig, len(rendered), len(put.Removed), continuing)
	res.Put, res.Rendered = put, rendered
	return res, nil
}

// readLightRow reads light_state and status.
func readLightRow(ctx context.Context, workspaceID string) (state, status string, err error) {
	err = db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(light_state, ''), status FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&state, &status)
	return state, status, err
}

// lightAgentBusy reports whether a 'parking' agent is doing something: a task
// its heartbeats report (they still record active_tasks and is_busy while
// 'parking'), or an activity row the agent itself wrote since the parking
// claim. A message can no longer reach it once 'parking' is claimed, so such a
// row comes from a turn that was already running (its tool calls, its log).
// Rows of its runs do not count (they carry a run_id; a run pod writes under
// the agent's id): a run of the last park may still be going while the agent
// is open (review 21719 O-A).
func lightAgentBusy(ctx context.Context, workspaceID string) (bool, error) {
	var busy bool
	err := db.DB.QueryRowContext(ctx, `
		SELECT COALESCE(w.active_tasks, 0) > 0 OR COALESCE(w.is_busy, false)
		       OR EXISTS (SELECT 1 FROM activity_logs a
		                  WHERE a.workspace_id = w.id AND a.run_id IS NULL AND a.created_at > w.light_state_at)
		FROM workspaces w WHERE w.id = $1`, workspaceID,
	).Scan(&busy)
	return busy, err
}

// finishLightPark is a park's asynchronous part: hibernate the agent, then
// resume its CronJobs and claim 'parked', retrying the resume for up to
// resumeBudget. Called after parkLight and by the Light monitor. A busy agent
// (the hibernate claim lost) goes back to open.
func (h *WorkspaceHandler) finishLightPark(ctx context.Context, workspaceID string, trig lightTrigger, resumeBudget time.Duration) error {
	state, status, err := readLightRow(ctx, workspaceID)
	if err != nil {
		return err
	}
	if state != models.LightParking {
		return nil
	}
	switch models.WorkspaceStatus(status) {
	case models.StatusOnline, models.StatusDegraded:
		// A turn that started around the claim: its heartbeat still records
		// active_tasks/is_busy while 'parking' (registry.go), so it shows here
		// or makes the hibernate claim miss. Either way the park is undone
		// rather than stopping the agent mid-turn (review 21718 R3).
		busy, err := lightAgentBusy(ctx, workspaceID)
		if err != nil {
			return err
		}
		herr := errHibernateNotClaimed
		if !busy {
			herr = h.hibernateWorkspace(ctx, workspaceID, false)
		}
		if errors.Is(herr, errHibernateNotClaimed) {
			// It took a task after the PUT; its CronJobs are still paused.
			if _, err := h.lightTransition(ctx, workspaceID, "park", trig, []string{models.LightParking}, models.LightOpen); err != nil {
				return err
			}
			recordLightError(ctx, workspaceID, "LIGHT_AGENT_BUSY", "the agent took a task while it was being parked; it stays open")
			return errLightParkAbandoned
		}
		if herr != nil {
			recordLightError(ctx, workspaceID, "LIGHT_PARK_INCOMPLETE", "stopping the agent failed; the park is retried")
			return herr
		}
	case models.StatusHibernated:
	default:
		// Stopping (hibernating) or anything else: the monitor finishes it.
		return nil
	}
	return h.resumeLightAfterPark(ctx, workspaceID, trig, resumeBudget)
}

// resumeLightAfterPark resumes the CronJobs of a stopped agent and claims
// 'parked', retrying LIGHT_AGENT_AWAKE (the pod is still stopping) for up to
// budget. Other failures are recorded; the monitor retries every one.
func (h *WorkspaceHandler) resumeLightAfterPark(ctx context.Context, workspaceID string, trig lightTrigger, budget time.Duration) error {
	if h.lightAPI == nil {
		return provisioner.ErrLightNotConfigured
	}
	deadline := time.Now().Add(budget)
	for {
		rctx, cancel := context.WithTimeout(ctx, lightBackendTimeout)
		_, err := h.lightAPI.ResumeLight(rctx, workspaceID)
		cancel()
		if err == nil {
			changed, terr := h.lightTransition(ctx, workspaceID, "park", trig, []string{models.LightParking}, models.LightParked)
			if terr == nil && changed {
				clearLightParkError(ctx, workspaceID)
			}
			return terr
		}
		code := provisioner.LightCPCode(err)
		if code == "LIGHT_AGENT_AWAKE" && time.Now().Add(lightResumeRetryEvery).Before(deadline) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(lightResumeRetryEvery):
			}
			continue
		}
		switch code {
		case "LIGHT_AGENT_AWAKE":
			// Expected while the pod stops; not a failure worth reporting.
		case "":
			recordLightError(ctx, workspaceID, "LIGHT_PARK_INCOMPLETE", "resuming the schedules failed; the park is retried")
		default:
			recordLightError(ctx, workspaceID, code, "resuming the schedules failed; the park is retried")
		}
		return err
	}
}

// lightCronJobOfRun is the CronJob a run's Job belongs to: scheduled Jobs are
// "<cronjob>-<minutes>", run-now Jobs "<cronjob>-m<unix seconds>".
func lightCronJobOfRun(runID string) string {
	if i := strings.LastIndexByte(runID, '-'); i > 0 {
		return runID[:i]
	}
	return runID
}

// recordCancelledLightRuns records the runs a pause deleted before they
// copied (they never ran): a row of their own, or a running row closed.
func recordCancelledLightRuns(ctx context.Context, workspaceID string, runIDs []string) {
	for _, runID := range runIDs {
		if !lightRunIDRe.MatchString(runID) {
			continue
		}
		cronjob := lightCronJobOfRun(runID)
		if _, err := db.DB.ExecContext(ctx, `
			INSERT INTO light_runs (workspace_id, run_id, schedule, status, source, finished_at)
			VALUES ($1, $2, COALESCE((SELECT name FROM light_schedules WHERE workspace_id = $1 AND cronjob = $3 LIMIT 1), $3), 'cancelled', 'job', now())
			ON CONFLICT (workspace_id, run_id) DO UPDATE
				SET status = 'cancelled', source = 'job', finished_at = now(), updated_at = now()
				WHERE light_runs.status = 'running'`,
			workspaceID, runID, cronjob); err != nil {
			log.Printf("light: recording cancelled run %s of %s failed: %v", runID, workspaceID, err)
		}
	}
}

// lightParkStuckOpenAfter is how long a park must have held 'parking' before
// an open may take the agent back: past every inline step of a park (its
// resume retries for up to two minutes), so no park step can still be running.
var lightParkStuckOpenAfter = 3 * time.Minute

// openLight opens a parked agent (see the file comment). The caller holds the
// workspace's Light lock. unchanged is true for an agent already open or
// opening; an owner's open of one takes it over (the auto-park idle window
// restarts). A park stuck in 'parking' (its resume keeps failing) is recovered
// once lightParkStuckOpenAfter has passed.
func (h *WorkspaceHandler) openLight(ctx context.Context, workspaceID string, trig lightTrigger) (cancelled []string, unchanged bool, state string, ref *lightRefusal) {
	row, err := loadLightWorkspace(ctx, workspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, "", newLightRefusal(http.StatusNotFound, "WORKSPACE_NOT_FOUND", "workspace not found", nil)
	}
	if err != nil {
		log.Printf("light op=open ws=%s: workspace lookup failed: %v", workspaceID, err)
		return nil, false, "", newLightRefusal(http.StatusInternalServerError, "LIGHT_FAILED", "failed to look up the workspace", nil)
	}
	fromParking := false
	switch row.LightState {
	case "":
		return nil, false, "", newLightRefusal(http.StatusConflict, "LIGHT_NOT_ENABLED", "this workspace is not a Light agent", nil)
	case models.LightOpen, models.LightOpening:
		if trig == lightTriggerOwner {
			// The owner takes over an agent a refresh opened, and restarts the
			// idle window either way (review 21718 R2).
			if _, err := db.DB.ExecContext(ctx, `
				UPDATE workspaces SET light_opened_by = 'owner', light_state_at = now(), updated_at = now()
				WHERE id = $1 AND light_state IN ('open', 'opening')`, workspaceID); err != nil {
				log.Printf("light op=open ws=%s: taking the open agent over failed: %v", workspaceID, err)
			}
		}
		return nil, true, row.LightState, nil
	case models.LightParking:
		stuck := row.StateAt.Valid && time.Since(row.StateAt.Time) >= lightParkStuckOpenAfter
		awake := row.Status == string(models.StatusOnline) || row.Status == string(models.StatusDegraded)
		recoverable := awake || row.Status == string(models.StatusHibernated)
		if !stuck || !recoverable {
			return nil, false, "", newLightRefusal(http.StatusConflict, "LIGHT_TRANSITION_IN_PROGRESS",
				"the agent is being parked; open it once it is parked, or after three minutes if the park cannot finish",
				gin.H{"light_state": row.LightState})
		}
		fromParking = true
	}
	if h.lightAPI == nil {
		return nil, false, "", lightCPRefusal("open", workspaceID, provisioner.ErrLightNotConfigured)
	}
	// The marker an interrupted open leaves behind: the monitor resumes the
	// CronJobs of a parked agent that still carries it (review 21718 O3). No
	// marker, no pause.
	if err := markLightOpenIncomplete(ctx, workspaceID); err != nil {
		log.Printf("light op=open ws=%s: recording the open marker failed: %v", workspaceID, err)
		return nil, false, "", newLightRefusal(http.StatusInternalServerError, "LIGHT_FAILED", "failed to open the agent", nil)
	}
	// Pause first, while still parked: no wake path can reach the agent and
	// no CronJob fires once this returns.
	pctx, cancel := context.WithTimeout(ctx, lightBackendTimeout)
	paused, err := h.lightAPI.PauseLight(pctx, workspaceID)
	cancel()
	if err != nil {
		h.resumeAfterFailedOpen(ctx, workspaceID, fromParking)
		return nil, false, "", lightCPRefusal("open", workspaceID, err)
	}
	recordCancelledLightRuns(ctx, workspaceID, paused.Cancelled)
	openedBy := lightOpenedByOwner
	if trig == lightTriggerRefresh {
		openedBy = lightOpenedByRefresh
	}
	from, to := models.LightParked, models.LightOpening
	if fromParking {
		from = models.LightParking
		if row.Status != string(models.StatusHibernated) {
			to = models.LightOpen // the park never stopped the pod
		}
		if row.PendingTokenID.Valid {
			dropPendingLightToken(ctx, workspaceID, row.PendingTokenID.String)
		}
	}
	res, err := db.DB.ExecContext(ctx, `
		UPDATE workspaces
		SET light_state = $3, light_state_at = now(), light_opened_by = $2, updated_at = now(),
		    light_last_error = NULL, light_last_error_at = NULL
		WHERE id = $1 AND light_state = $4`, workspaceID, openedBy, to, from)
	claimed := false
	if err == nil {
		n, _ := res.RowsAffected()
		claimed = n > 0
	}
	if !claimed {
		log.Printf("light op=open ws=%s: claiming %s failed (err=%v); resuming the paused schedules", workspaceID, to, err)
		h.resumeAfterFailedOpen(ctx, workspaceID, fromParking)
		if err != nil {
			return nil, false, "", newLightRefusal(http.StatusInternalServerError, "LIGHT_FAILED", "failed to open the agent", nil)
		}
		return nil, false, "", newLightRefusal(http.StatusConflict, "LIGHT_TRANSITION_IN_PROGRESS",
			"another Light operation on this agent is in progress; retry shortly", nil)
	}
	log.Printf("light op=open ws=%s from=[%s] to=%q trigger=%s result=ok cancelled=%d", workspaceID, from, to, trig, len(paused.Cancelled))
	h.broadcastLightState(workspaceID, to)
	if to == models.LightOpening {
		h.goAsync(func() { h.WakeWorkspace(workspaceID) })
	}
	if paused.Cancelled == nil {
		paused.Cancelled = []string{}
	}
	return paused.Cancelled, false, to, nil
}

// resumeAfterFailedOpen undoes an open's pause (best effort) when the open
// could not claim the agent: a parked agent's schedules must keep running.
// Only while the agent is still parked (another open may have claimed it);
// when the resume fails, the LIGHT_OPEN_INCOMPLETE marker stays and the
// monitor resumes them. A stuck park being recovered keeps its own state: its
// park resumes the CronJobs.
func (h *WorkspaceHandler) resumeAfterFailedOpen(ctx context.Context, workspaceID string, fromParking bool) {
	if fromParking || h.lightAPI == nil {
		return
	}
	if state, _, err := readLightRow(ctx, workspaceID); err != nil || state != models.LightParked {
		log.Printf("light op=open ws=%s: not resuming after the failed open (light_state=%q, err=%v); the monitor reconciles", workspaceID, state, err)
		return
	}
	if err := h.resumeAfterInterruptedOpen(ctx, workspaceID); err != nil {
		log.Printf("light op=open ws=%s: %v (the monitor retries)", workspaceID, err)
	}
}

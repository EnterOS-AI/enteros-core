package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/cronspec"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// AdminSchedulesHealthHandler serves GET /admin/schedules/health — a cross-workspace
// schedule monitoring view gated behind AdminAuth. Unlike the per-workspace
// GET /workspaces/:id/schedules/health (which requires caller identity + CanCommunicate),
// this endpoint is intended for operators and automated audit agents that hold a
// global admin bearer token. Issue #618.
type AdminSchedulesHealthHandler struct{}

// NewAdminSchedulesHealthHandler returns an AdminSchedulesHealthHandler.
func NewAdminSchedulesHealthHandler() *AdminSchedulesHealthHandler {
	return &AdminSchedulesHealthHandler{}
}

// adminScheduleHealth is the per-schedule entry in the health response.
type adminScheduleHealth struct {
	WorkspaceID           string     `json:"workspace_id"`
	WorkspaceName         string     `json:"workspace_name"`
	ScheduleID            string     `json:"schedule_id"`
	ScheduleName          string     `json:"schedule_name"`
	CronExpr              string     `json:"cron_expr"`
	LastRunAt             *time.Time `json:"last_run_at"`
	ExpectedNextRun       *time.Time `json:"expected_next_run"`
	Status                string     `json:"status"` // "ok" | "stale" | "never_run"
	StaleThresholdSeconds int64      `json:"stale_threshold_seconds"`
}

// computeStaleThreshold returns 2× the cron interval for the given expression
// and timezone. The interval is approximated as the gap between two consecutive
// scheduled fire times computed from now.
//
// Exported as a package-level function so it can be unit-tested independently
// from the handler.
func computeStaleThreshold(cronExpr, tz string, now time.Time) (time.Duration, error) {
	t1, err := cronspec.ComputeNextRun(cronExpr, tz, now)
	if err != nil {
		return 0, err
	}
	t2, err := cronspec.ComputeNextRun(cronExpr, tz, t1)
	if err != nil {
		return 0, err
	}
	return 2 * t2.Sub(t1), nil
}

// volumeAdminScheduleHealth builds the admin health entries for one
// volume-native workspace from its runtime grid + the trigger daemon's health
// file (P4b re-point — the workspace's DB rows, if any remain pre-migration,
// are stale strays). last_run_at carries the daemon's last tick: that is the
// RFC G6 liveness signal — an alive daemon classifies "ok", a dead one drifts
// to "stale" past the 2× cron-interval threshold, a never-started one reports
// "never_run".
func volumeAdminScheduleHealth(ctx context.Context, workspaceID, workspaceName string, now time.Time) ([]adminScheduleHealth, error) {
	wsURL, secret, err := resolveScheduleFanoutTarget(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	status, body, err := fetchScheduleAPI(ctx, wsURL, secret, http.MethodGet, "", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("grid list returned %d", status)
	}
	var grid struct {
		Schedules []volumeEntry `json:"schedules"`
	}
	if err := json.Unmarshal(body, &grid); err != nil {
		return nil, fmt.Errorf("malformed grid: %w", err)
	}
	hstatus, hbody, err := fetchScheduleAPI(ctx, wsURL, secret, http.MethodGet, "/health", nil)
	if err != nil {
		return nil, err
	}
	if hstatus != http.StatusOK {
		return nil, fmt.Errorf("health returned %d", hstatus)
	}
	var health volumeHealthPayload
	if err := json.Unmarshal(hbody, &health); err != nil {
		return nil, fmt.Errorf("malformed health: %w", err)
	}

	var lastTick *time.Time
	if health.LastTick != nil {
		if t, perr := time.Parse(time.RFC3339, *health.LastTick); perr == nil {
			lastTick = &t
		}
	}
	entries := make([]adminScheduleHealth, 0, len(grid.Schedules))
	for _, e := range grid.Schedules {
		staleThreshold, cronErr := computeStaleThreshold(e.Cron, e.Timezone, now)
		var staleThresholdSeconds int64
		if cronErr == nil {
			staleThresholdSeconds = int64(staleThreshold.Seconds())
		} else {
			log.Printf("AdminSchedulesHealth: cron parse error for volume schedule %s/%s (%q): %v",
				workspaceID, e.Name, e.Cron, cronErr)
		}
		var nextRun *time.Time
		if next, nerr := computeNextRunSafe(e.Cron, e.Timezone); nerr == nil {
			nextRun = &next
		}
		entries = append(entries, adminScheduleHealth{
			WorkspaceID:           workspaceID,
			WorkspaceName:         workspaceName,
			ScheduleID:            e.Name, // volume grid is name-keyed: id == name
			ScheduleName:          e.Name,
			CronExpr:              e.Cron,
			LastRunAt:             lastTick,
			ExpectedNextRun:       nextRun,
			Status:                classifyScheduleStatus(lastTick, staleThreshold, now),
			StaleThresholdSeconds: staleThresholdSeconds,
		})
	}
	return entries, nil
}

// Health handles GET /admin/schedules/health.
//
// It joins workspace_schedules with workspaces and, for each schedule, computes:
//   - status:                "never_run" (last_run_at IS NULL),
//     "stale" (now - last_run_at > 2 × cron interval), or
//     "ok" (recently run).
//   - stale_threshold_seconds: 2 × the cron interval derived from cron_expr.
//   - expected_next_run:     the next_run_at value stored by the scheduler.
//
// Volume-native workspaces (native `scheduler` capability, P4b re-point) are
// served from the runtime schedule API instead of the DB: their grid +
// bookkeeping live on the workspace volume, so any rows still in
// workspace_schedules are pre-migration strays and are skipped. The DB path
// stays for legacy stragglers. An unreachable volume workspace is logged and
// omitted rather than failing the whole aggregate.
//
// Returns 200 with a JSON array (empty if no schedules exist), 500 on DB error.
// Auth is enforced by the adminAuth() middleware registered in router.go.
func (h *AdminSchedulesHealthHandler) Health(c *gin.Context) {
	ctx := c.Request.Context()
	now := time.Now()

	volumeIDs := volumeSchedulerWorkspaceIDs()
	isVolume := make(map[string]bool, len(volumeIDs))
	for _, id := range volumeIDs {
		isVolume[id] = true
	}

	rows, err := db.DB.QueryContext(ctx, `
		SELECT
			w.id          AS workspace_id,
			w.name        AS workspace_name,
			s.id          AS schedule_id,
			s.name        AS schedule_name,
			s.cron_expr,
			s.timezone,
			s.last_run_at,
			s.next_run_at
		FROM workspace_schedules s
		JOIN workspaces w ON w.id = s.workspace_id
		WHERE w.status != 'removed'
		ORDER BY w.name ASC, s.name ASC
	`)
	if err != nil {
		log.Printf("AdminSchedulesHealth: query error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query schedules"})
		return
	}
	defer rows.Close()

	entries := make([]adminScheduleHealth, 0)
	for rows.Next() {
		var (
			workspaceID   string
			workspaceName string
			scheduleID    string
			scheduleName  string
			cronExpr      string
			timezone      string
			lastRunAt     *time.Time
			nextRunAt     *time.Time
		)
		if err := rows.Scan(
			&workspaceID, &workspaceName,
			&scheduleID, &scheduleName,
			&cronExpr, &timezone,
			&lastRunAt, &nextRunAt,
		); err != nil {
			log.Printf("AdminSchedulesHealth: scan error: %v", err)
			continue
		}

		// Volume-native workspace — its DB rows are pre-migration strays; the
		// live view comes from the runtime proxy loop below.
		if isVolume[workspaceID] {
			continue
		}

		// Compute stale threshold = 2 × cron interval.
		// On parse failure (malformed cron_expr in DB) we report 0 and still
		// classify the row — a bad cron_expr itself is worth surfacing in the
		// health view rather than silently skipping the row.
		staleThreshold, cronErr := computeStaleThreshold(cronExpr, timezone, now)
		var staleThresholdSeconds int64
		if cronErr == nil {
			staleThresholdSeconds = int64(staleThreshold.Seconds())
		} else {
			log.Printf("AdminSchedulesHealth: cron parse error for schedule %s (%q): %v",
				scheduleID, cronExpr, cronErr)
		}

		// Classify schedule status.
		status := classifyScheduleStatus(lastRunAt, staleThreshold, now)

		entries = append(entries, adminScheduleHealth{
			WorkspaceID:           workspaceID,
			WorkspaceName:         workspaceName,
			ScheduleID:            scheduleID,
			ScheduleName:          scheduleName,
			CronExpr:              cronExpr,
			LastRunAt:             lastRunAt,
			ExpectedNextRun:       nextRunAt,
			Status:                status,
			StaleThresholdSeconds: staleThresholdSeconds,
		})
	}
	if err := rows.Err(); err != nil {
		log.Printf("AdminSchedulesHealth: rows iteration error: %v", err)
	}

	// Volume-native workspaces: live schedule state via the runtime proxy.
	for _, wsID := range volumeIDs {
		var wsName string
		if err := db.DB.QueryRowContext(ctx,
			`SELECT name FROM workspaces WHERE id = $1 AND status != 'removed'`, wsID,
		).Scan(&wsName); err != nil {
			if err != sql.ErrNoRows {
				log.Printf("AdminSchedulesHealth: name lookup for %s failed: %v", wsID, err)
			}
			continue // removed/unknown workspace — nothing to report
		}
		ventries, err := volumeAdminScheduleHealth(ctx, wsID, wsName, now)
		if err != nil {
			log.Printf("AdminSchedulesHealth: runtime health for %s unavailable (omitted): %v", wsID, err)
			continue
		}
		entries = append(entries, ventries...)
	}

	c.JSON(http.StatusOK, entries)
}

// orphanScheduleEntry is one row in the Orphans response.
type orphanScheduleEntry struct {
	WorkspaceID     string `json:"workspace_id"`
	WorkspaceStatus string `json:"workspace_status"` // "removed" | "missing"
	ScheduleID      string `json:"schedule_id"`
	ScheduleName    string `json:"schedule_name"`
	Source          string `json:"source"`
	Enabled         bool   `json:"enabled"`
	CronExpr        string `json:"cron_expr"`
}

// Orphans handles GET /admin/schedules/orphans — the monitor surface for
// internal#2006. Health (above) reports only LIVE workspaces' schedules, so a
// schedule left on a removed/recreated workspace silently stops firing and
// never appears there. This endpoint lists exactly those orphans (workspace
// removed OR missing) so an operator/monitor can alert. Returns 200 + JSON
// array (empty when none). Auth via adminAuth() in router.go.
func (h *AdminSchedulesHealthHandler) Orphans(c *gin.Context) {
	ctx := c.Request.Context()
	rows, err := db.DB.QueryContext(ctx, `
		SELECT s.workspace_id,
		       CASE WHEN w.id IS NULL THEN 'missing' ELSE 'removed' END AS ws_status,
		       s.id, s.name, COALESCE(s.source, ''), s.enabled, s.cron_expr
		FROM workspace_schedules s
		LEFT JOIN workspaces w ON w.id = s.workspace_id
		WHERE w.id IS NULL OR w.status = 'removed'
		ORDER BY s.name ASC
	`)
	if err != nil {
		log.Printf("AdminSchedulesOrphans: query error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query orphans"})
		return
	}
	defer rows.Close()
	out := make([]orphanScheduleEntry, 0)
	for rows.Next() {
		var e orphanScheduleEntry
		if err := rows.Scan(&e.WorkspaceID, &e.WorkspaceStatus, &e.ScheduleID, &e.ScheduleName, &e.Source, &e.Enabled, &e.CronExpr); err != nil {
			log.Printf("AdminSchedulesOrphans: scan error: %v", err)
			continue
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		log.Printf("AdminSchedulesOrphans: rows iteration error: %v", err)
	}
	c.JSON(http.StatusOK, out)
}

// ReapOrphans handles POST /admin/schedules/reap-orphans — the orphan cleaner
// (internal#2006). For every schedule bound to a removed/nonexistent workspace
// it re-points runtime-created schedules onto the live successor agent (matched
// by role+parent, falling back to name+parent) when one exists and doesn't
// already carry a same-named schedule; schedules with no live successor are
// disabled (enabled=false) so the scheduler stops firing into a dead workspace.
// Idempotent: re-running with no orphans is a no-op. Returns a summary count.
// Auth is enforced by the adminAuth() middleware registered in router.go.
func (h *AdminSchedulesHealthHandler) ReapOrphans(c *gin.Context) {
	ctx := c.Request.Context()

	// 1. Re-point runtime schedules onto a live successor (same role+parent,
	//    else same name+parent). Skip names already present on the successor.
	repointed, err := db.DB.ExecContext(ctx, `
		WITH orphan AS (
			SELECT s.id, s.name, s.workspace_id, prev.role AS role, prev.parent_id AS parent_id
			FROM workspace_schedules s
			JOIN workspaces prev ON prev.id = s.workspace_id
			WHERE prev.status = 'removed' AND s.source = 'runtime'
		),
		successor AS (
			SELECT o.id AS schedule_id, o.name AS schedule_name,
			       (
			         SELECT w.id FROM workspaces w
			         WHERE w.status != 'removed'
			           AND w.parent_id IS NOT DISTINCT FROM o.parent_id
			           AND ((o.role IS NOT NULL AND w.role = o.role))
			         ORDER BY w.updated_at DESC NULLS LAST LIMIT 1
			       ) AS live_id
			FROM orphan o
		)
		UPDATE workspace_schedules s
		SET workspace_id = su.live_id, updated_at = now()
		FROM successor su
		WHERE s.id = su.schedule_id
		  AND su.live_id IS NOT NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM workspace_schedules t
		      WHERE t.workspace_id = su.live_id AND t.name = su.schedule_name
		  )
	`)
	if err != nil {
		log.Printf("ReapOrphans: re-point error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "re-point failed"})
		return
	}
	repointedN, err := repointed.RowsAffected()
	if err != nil {
		log.Printf("ReapOrphans: repointed rows affected: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "re-point failed"})
		return
	}

	// 2. Disable any remaining schedules still bound to a removed/missing
	//    workspace (no live successor, or template schedules on a dead row).
	disabled, err := db.DB.ExecContext(ctx, `
		UPDATE workspace_schedules s
		SET enabled = false, updated_at = now()
		WHERE s.enabled = true
		  AND NOT EXISTS (
		      SELECT 1 FROM workspaces w
		      WHERE w.id = s.workspace_id AND w.status != 'removed'
		  )
	`)
	if err != nil {
		log.Printf("ReapOrphans: disable error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "disable failed"})
		return
	}
	disabledN, err := disabled.RowsAffected()
	if err != nil {
		log.Printf("ReapOrphans: disabled rows affected: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "disable failed"})
		return
	}

	log.Printf("ReapOrphans: re-pointed %d, disabled %d orphaned schedule(s)", repointedN, disabledN)
	c.JSON(http.StatusOK, gin.H{"repointed": repointedN, "disabled": disabledN})
}

// classifyScheduleStatus returns the health status string for a schedule.
//   - "never_run"  — last_run_at is NULL (schedule has never fired)
//   - "stale"      — now - last_run_at > staleThreshold (and threshold > 0)
//   - "ok"         — recently run within the expected window
func classifyScheduleStatus(lastRunAt *time.Time, staleThreshold time.Duration, now time.Time) string {
	if lastRunAt == nil {
		return "never_run"
	}
	if staleThreshold > 0 && now.Sub(*lastRunAt) > staleThreshold {
		return "stale"
	}
	return "ok"
}

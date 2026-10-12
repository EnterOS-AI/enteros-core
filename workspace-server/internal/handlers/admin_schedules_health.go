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
// Every scheduled workspace is volume-native (native `scheduler` capability):
// its schedule grid + bookkeeping live on the workspace volume and are served
// via the runtime schedule API. For each schedule it reports:
//   - status:                 "never_run" | "stale" | "ok" (daemon-tick liveness)
//   - stale_threshold_seconds: 2 × the cron interval derived from the cron expr
//   - expected_next_run:       the next fire time computed from the cron expr
//
// An unreachable workspace is logged and omitted rather than failing the whole
// aggregate. Returns 200 with a JSON array (empty when nothing to report). Auth
// is enforced by the adminAuth() middleware registered in router.go.
func (h *AdminSchedulesHealthHandler) Health(c *gin.Context) {
	ctx := c.Request.Context()
	now := time.Now()

	entries := make([]adminScheduleHealth, 0)
	// Volume-native workspaces (native `scheduler` capability): live schedule
	// state via the runtime proxy. An unreachable workspace is logged and
	// omitted rather than failing the whole aggregate.
	for _, wsID := range volumeSchedulerWorkspaceIDs() {
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

package handlers

// P4b readiness + fleet migration tooling for retiring the workspace_schedules
// table (scheduler-as-trigger-plugin RFC, Option A).
//
// The DROP of workspace_schedules is irreversible and gated on a live precondition
// sequence: every workspace must be volume-native (advertising the `scheduler`
// capability), every source='runtime' row must be copied to its volume grid, the
// SCHEDULE_VOLUME_PROXY_DISABLED kill-switch must be off, and the dual-path DB
// readers/writers must be removed and soaked — THEN the table can be dropped.
//
// These two admin endpoints make that precondition measurable and executable
// without touching the irreversible parts:
//   - P4bReadiness      GET  /admin/schedules/p4b-readiness       (read-only audit)
//   - MigrateAllToVolume POST /admin/schedules/migrate-all-to-volume?apply=false
//
// Neither ever deletes a workspace_schedules row: migration is copy-then-verify,
// and the DB remains the fallback until an operator ships the reader removal.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// p4bWorkspaceReadiness is the per-workspace readiness row.
type p4bWorkspaceReadiness struct {
	WorkspaceID          string `json:"workspace_id"`
	VolumeNative         bool   `json:"volume_native"`
	DBRuntimeRows        int    `json:"db_runtime_rows"`
	DBTemplateRows       int    `json:"db_template_rows"`
	VolumeGridCount      int    `json:"volume_grid_count"`      // -1 if the runtime was unreachable
	RuntimeRowsUnsynced  int    `json:"runtime_rows_unsynced"`  // runtime rows not yet on the volume (-1 if unknown)
	TemplateRowsUnsynced int    `json:"template_rows_unsynced"` // template rows not yet on the volume (-1 if unknown)
	Blocking             bool   `json:"blocking"`
	Reason               string `json:"reason,omitempty"`
}

// P4bReadiness reports fleet-wide readiness to drop workspace_schedules. Read-only.
//
//	@Summary	Audit readiness to retire the workspace_schedules table (P4b)
//	@Tags		schedules
//	@Produce	json
//	@Success	200	{object}	map[string]interface{}
//	@Router		/admin/schedules/p4b-readiness [get]
func (h *ScheduleHandler) P4bReadiness(c *gin.Context) {
	ctx := c.Request.Context()

	// Row counts per LIVE workspace, split by source. Rows tied to removed/missing
	// workspaces are dead data the DROP simply discards — counted separately so
	// they never mask a real blocker.
	type counts struct{ runtime, template int }
	byWorkspace := map[string]*counts{}
	rows, err := db.DB.QueryContext(ctx, `
		SELECT s.workspace_id, s.source, COUNT(*)
		FROM workspace_schedules s
		JOIN workspaces w ON w.id = s.workspace_id
		WHERE w.status != 'removed'
		GROUP BY s.workspace_id, s.source
	`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read schedules"})
		return
	}
	defer rows.Close()
	for rows.Next() {
		var wsID, source string
		var n int
		if err := rows.Scan(&wsID, &source, &n); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to scan schedule counts"})
			return
		}
		if byWorkspace[wsID] == nil {
			byWorkspace[wsID] = &counts{}
		}
		if source == "template" {
			byWorkspace[wsID].template = n
		} else {
			byWorkspace[wsID].runtime = n
		}
	}

	// Orphan rows (removed/missing workspaces) — dead weight the DROP cleans up;
	// reported for completeness, never a blocker.
	var orphanRows int
	_ = db.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workspace_schedules s
		LEFT JOIN workspaces w ON w.id = s.workspace_id
		WHERE w.id IS NULL OR w.status = 'removed'
	`).Scan(&orphanRows)

	ids := make([]string, 0, len(byWorkspace))
	for id := range byWorkspace {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	report := make([]p4bWorkspaceReadiness, 0, len(ids))
	notNative, needMigration, needReseed := 0, 0, 0
	for _, id := range ids {
		cnt := byWorkspace[id]
		native := scheduleBackendIsVolume(id)
		row := p4bWorkspaceReadiness{
			WorkspaceID:          id,
			VolumeNative:         native,
			DBRuntimeRows:        cnt.runtime,
			DBTemplateRows:       cnt.template,
			VolumeGridCount:      -1,
			RuntimeRowsUnsynced:  -1,
			TemplateRowsUnsynced: -1,
		}
		if !native {
			// Still served by the DB path — the plugin must be armed (backfill)
			// before this workspace can leave the table behind.
			row.Blocking = true
			row.Reason = "not volume-native (scheduler plugin not armed); backfill required"
			notNative++
		} else {
			// Volume-native: verify EVERY DB row (both sources) is on the volume,
			// since the DROP discards all of them.
			s := rowsUnsyncedToVolume(ctx, id)
			row.VolumeGridCount = s.gridCount
			row.RuntimeRowsUnsynced, row.TemplateRowsUnsynced = s.runtimeMissing, s.templateMissing
			switch {
			case s.runtimeMissing < 0:
				row.Blocking = true
				row.Reason = "volume-native but runtime unreachable; cannot verify migration parity"
				needMigration++
			case s.runtimeMissing > 0:
				row.Blocking = true
				row.Reason = "volume-native but runtime rows not yet migrated; run migrate-all-to-volume"
				needMigration++
			case s.templateMissing > 0:
				// migrate-all does NOT fix these — template rows land on the volume
				// via the config.yaml re-seed at (re)provision/reload.
				row.Blocking = true
				row.Reason = "volume-native but template rows not on the volume; reprovision/reload to re-seed config.yaml before drop"
				needReseed++
			}
		}
		report = append(report, row)
	}

	killSwitch := scheduleProxyKillSwitchEnabled()
	droppable := !killSwitch && notNative == 0 && needMigration == 0 && needReseed == 0

	c.JSON(http.StatusOK, gin.H{
		"droppable":                    droppable,
		"kill_switch_enabled":          killSwitch,
		"workspaces_with_live_rows":    len(ids),
		"not_volume_native":            notNative,
		"workspaces_needing_migration": needMigration,
		"workspaces_needing_reseed":    needReseed,
		"orphan_rows":                  orphanRows,
		"workspaces":                   report,
	})
}

type volumeSyncState struct {
	gridCount       int
	runtimeMissing  int // source='runtime' DB rows absent from the volume grid; -1 if unreachable
	templateMissing int // source='template' DB rows absent from the volume grid; -1 if unreachable
}

// rowsUnsyncedToVolume compares ALL of a volume-native workspace's DB rows — both
// sources — against its live volume grid (quiet fan-out; never fails the caller).
// The DROP removes template rows too, so a template row absent from the volume is
// a real data-loss blocker (its fix is a reprovision/reload that re-delivers the
// config.yaml schedules block, NOT migrate-all which only copies runtime rows).
func rowsUnsyncedToVolume(ctx context.Context, workspaceID string) volumeSyncState {
	runtimeNames, templateNames, err := scheduleNamesBySource(ctx, workspaceID)
	if err != nil {
		return volumeSyncState{gridCount: -1, runtimeMissing: -1, templateMissing: -1}
	}
	onVolume, gridCount, err := volumeGridNames(ctx, workspaceID)
	if err != nil {
		return volumeSyncState{gridCount: -1, runtimeMissing: -1, templateMissing: -1}
	}
	countMissing := func(names []string) int {
		m := 0
		for _, n := range names {
			if !onVolume[n] {
				m++
			}
		}
		return m
	}
	return volumeSyncState{
		gridCount:       gridCount,
		runtimeMissing:  countMissing(runtimeNames),
		templateMissing: countMissing(templateNames),
	}
}

// scheduleNamesBySource returns a workspace's DB schedule names split by source.
func scheduleNamesBySource(ctx context.Context, workspaceID string) (runtime, template []string, err error) {
	rows, qerr := db.DB.QueryContext(ctx, `
		SELECT name, source FROM workspace_schedules
		WHERE workspace_id = $1
	`, workspaceID)
	if qerr != nil {
		return nil, nil, qerr
	}
	defer rows.Close()
	for rows.Next() {
		var name, source string
		if serr := rows.Scan(&name, &source); serr != nil {
			return nil, nil, serr
		}
		if source == "template" {
			template = append(template, name)
		} else {
			runtime = append(runtime, name)
		}
	}
	return runtime, template, rows.Err()
}

// volumeGridNames resolves fan-out creds then fetches the live volume grid.
func volumeGridNames(ctx context.Context, workspaceID string) (map[string]bool, int, error) {
	wsURL, secret, err := resolveScheduleFanoutTarget(ctx, workspaceID)
	if err != nil {
		return nil, 0, err
	}
	return volumeGridNamesWith(ctx, wsURL, secret)
}

// volumeGridNamesWith fetches a workspace's live volume grid with already-resolved
// creds (quiet) and returns the set of entry names + the grid size.
func volumeGridNamesWith(ctx context.Context, wsURL, secret string) (map[string]bool, int, error) {
	status, body, err := fetchScheduleAPI(ctx, wsURL, secret, http.MethodGet, "", nil)
	if err != nil {
		return nil, 0, err
	}
	if status != http.StatusOK {
		return nil, 0, fmt.Errorf("grid list returned %d", status)
	}
	var grid struct {
		Schedules []volumeEntry `json:"schedules"`
	}
	if err := json.Unmarshal(body, &grid); err != nil {
		return nil, 0, err
	}
	names := make(map[string]bool, len(grid.Schedules))
	for _, e := range grid.Schedules {
		names[e.Name] = true
	}
	return names, len(grid.Schedules), nil
}

// p4bMigrateResult is the per-workspace outcome of a fleet migration.
type p4bMigrateResult struct {
	WorkspaceID string `json:"workspace_id"`
	Migrated    int    `json:"migrated"`
	Skipped     int    `json:"skipped"` // already on the volume (idempotent)
	Failed      int    `json:"failed"`
	Unreachable bool   `json:"unreachable,omitempty"`
}

// MigrateAllToVolume copies every volume-native workspace's source='runtime'
// schedules from the DB into its volume grid. Dry-run by default (?apply=true to
// execute). Idempotent and copy-only — never deletes a DB row. Unreachable
// workspaces are logged and skipped, never failing the whole sweep.
//
//	@Summary	Fleet-migrate runtime schedules from the DB to volumes (P4b step 2)
//	@Tags		schedules
//	@Produce	json
//	@Param		apply	query	bool	false	"Execute the migration (default false = dry-run)"
//	@Success	200	{object}	map[string]interface{}
//	@Router		/admin/schedules/migrate-all-to-volume [post]
func (h *ScheduleHandler) MigrateAllToVolume(c *gin.Context) {
	ctx := c.Request.Context()
	apply := c.Query("apply") == "true"

	targets := volumeSchedulerWorkspaceIDs() // volume-native only, stable order
	results := make([]p4bMigrateResult, 0, len(targets))
	totalMigrated, totalSkipped, totalFailed, unreachable := 0, 0, 0, 0

	for _, id := range targets {
		res := migrateWorkspaceRuntimeToVolume(ctx, id, apply)
		if res.Unreachable {
			unreachable++
			log.Printf("MigrateAllToVolume: workspace %s unreachable, skipped", id)
		}
		totalMigrated += res.Migrated
		totalSkipped += res.Skipped
		totalFailed += res.Failed
		results = append(results, res)
	}

	c.JSON(http.StatusOK, gin.H{
		"apply":          apply,
		"workspaces":     len(targets),
		"total_migrated": totalMigrated, // for apply=false this is the WOULD-migrate count
		"total_skipped":  totalSkipped,
		"total_failed":   totalFailed,
		"unreachable":    unreachable,
		"results":        results,
	})
}

// migrateWorkspaceRuntimeToVolume copies one volume-native workspace's
// source='runtime' rows to its volume grid via the quiet fan-out client. On a
// dry-run (apply=false) it counts what WOULD migrate without writing. Idempotent:
// rows already on the volume are skipped.
func migrateWorkspaceRuntimeToVolume(ctx context.Context, workspaceID string, apply bool) p4bMigrateResult {
	res := p4bMigrateResult{WorkspaceID: workspaceID}

	wsURL, secret, err := resolveScheduleFanoutTarget(ctx, workspaceID)
	if err != nil {
		res.Unreachable = true
		return res
	}
	onVolume, _, err := volumeGridNamesWith(ctx, wsURL, secret)
	if err != nil {
		res.Unreachable = true
		return res
	}

	rows, err := db.DB.QueryContext(ctx, `
		SELECT name, cron_expr, timezone, prompt, enabled
		FROM workspace_schedules
		WHERE workspace_id = $1 AND source = 'runtime'
		ORDER BY created_at ASC
	`, workspaceID)
	if err != nil {
		res.Failed++
		return res
	}
	defer rows.Close()

	for rows.Next() {
		var e volumeEntry
		if err := rows.Scan(&e.Name, &e.Cron, &e.Timezone, &e.Prompt, &e.Enabled); err != nil {
			res.Failed++
			continue
		}
		if onVolume[e.Name] {
			res.Skipped++
			continue
		}
		if !apply {
			res.Migrated++ // dry-run: this WOULD be migrated
			continue
		}
		e.Source = "runtime"
		raw, _ := json.Marshal(e)
		st, _, ferr := fetchScheduleAPI(ctx, wsURL, secret, http.MethodPost, "", raw)
		if ferr != nil {
			res.Failed++
			continue
		}
		if st == http.StatusCreated {
			res.Migrated++
			onVolume[e.Name] = true
		} else {
			res.Failed++
		}
	}
	return res
}

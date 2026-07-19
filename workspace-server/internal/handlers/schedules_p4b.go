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
// IMPORTANT — what `data_drop_safe` does and does NOT mean. It is the DATA gate:
// true only when every live workspace's DB rows (both sources) are verified
// present-and-identical on its volume grid, the kill-switch is off, and every
// workspace is volume-native. It is NECESSARY-but-NOT-SUFFICIENT: the running
// core still contains the dual-path DB readers/writers, so an operator must ALSO
// ship + soak the reader/writer removal (a deploy-state fact this endpoint cannot
// observe) before the DROP. The response echoes that remaining precondition in
// `drop_still_requires`.
//
// Neither endpoint ever deletes a workspace_schedules row: migration is
// copy-then-verify, and the DB remains the fallback until the reader removal.
// P4bReadiness is strictly READ-ONLY — it uses a non-healing inbound-secret read
// so the audit never mints/persists a secret as a side effect.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/wsauth"
)

// dropStillRequires is the deploy-state precondition data_drop_safe cannot check.
const dropStillRequires = "the dual-path DB readers/writers must be removed from the running core and soaked before the DROP (a deploy fact this endpoint does not observe)"

// p4bWorkspaceReadiness is the per-workspace readiness row.
type p4bWorkspaceReadiness struct {
	WorkspaceID          string `json:"workspace_id"`
	VolumeNative         bool   `json:"volume_native"`
	DBRuntimeRows        int    `json:"db_runtime_rows"`
	DBTemplateRows       int    `json:"db_template_rows"`
	VolumeGridCount      int    `json:"volume_grid_count"`      // -1 if the runtime was unreachable
	RuntimeRowsUnsynced  int    `json:"runtime_rows_unsynced"`  // runtime rows absent-or-divergent on the volume (-1 if unverifiable)
	TemplateRowsUnsynced int    `json:"template_rows_unsynced"` // template rows absent-or-divergent on the volume (-1 if unverifiable)
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

	// Row counts per LIVE workspace, split by source. `IS DISTINCT FROM` (not
	// `!=`) so a NULL-status workspace — workspaces.status is nullable — is
	// treated as live, NOT silently excluded from the census (excluding it would
	// hide its unmigrated rows and let data_drop_safe go true unsafely).
	type counts struct{ runtime, template int }
	byWorkspace := map[string]*counts{}
	rows, err := db.DB.QueryContext(ctx, `
		SELECT s.workspace_id, s.source, COUNT(*)
		FROM workspace_schedules s
		JOIN workspaces w ON w.id = s.workspace_id
		WHERE w.status IS DISTINCT FROM 'removed'
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
	// A mid-stream read error ends rows.Next() with NO Scan error; only rows.Err()
	// surfaces it. Left unchecked, a truncated census omits workspaces and can
	// report data_drop_safe=true unsafely — fail closed instead.
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "schedule census read did not complete; cannot assess drop-safety"})
		return
	}

	// Orphan rows (removed/missing workspaces) — dead weight the DROP cleans up;
	// reported for completeness, never a blocker. On a query error report -1
	// (unknown) rather than a misleading 0.
	orphanRows := -1
	if oerr := db.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workspace_schedules s
		LEFT JOIN workspaces w ON w.id = s.workspace_id
		WHERE w.id IS NULL OR w.status = 'removed'
	`).Scan(&orphanRows); oerr != nil {
		log.Printf("P4bReadiness: orphan-row count failed: %v", oerr)
		orphanRows = -1
	}

	ids := make([]string, 0, len(byWorkspace))
	for id := range byWorkspace {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	report := make([]p4bWorkspaceReadiness, 0, len(ids))
	notNative, needMigration, needReseed, unverifiable := 0, 0, 0, 0
	killSwitch := scheduleProxyKillSwitchEnabled()
	for _, id := range ids {
		cnt := byWorkspace[id]
		// Label from the plugin capability itself (NOT scheduleBackendIsVolume,
		// which also folds in the global kill-switch). The kill-switch is a
		// fleet-wide blocker handled once in data_drop_safe below; folding it in
		// here would mislabel every plugin-armed workspace as "not native".
		native := ProvidesNativeScheduler(id)
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
			row.Blocking = true
			row.Reason = "not volume-native (scheduler plugin not armed); backfill required"
			notNative++
			report = append(report, row)
			continue
		}
		// Volume-native: verify EVERY DB row (both sources) is present AND
		// content-identical on the volume, since the DROP discards all of them.
		s := rowsUnsyncedToVolume(ctx, id)
		row.VolumeGridCount = s.gridCount
		row.RuntimeRowsUnsynced, row.TemplateRowsUnsynced = s.runtimeMissing, s.templateMissing
		switch {
		case s.runtimeMissing < 0 || s.templateMissing < 0:
			// Unreachable / unverifiable → fail closed (block); never assume synced.
			row.Blocking = true
			row.Reason = "volume-native but runtime unreachable; cannot verify migration parity"
			unverifiable++
		default:
			var reasons []string
			if s.runtimeMissing > 0 {
				needMigration++
				reasons = append(reasons, "runtime rows not yet migrated (run migrate-all-to-volume)")
			}
			if s.templateMissing > 0 {
				// migrate-all does NOT fix these — template rows land on the
				// volume via the config.yaml re-seed at (re)provision/reload.
				needReseed++
				reasons = append(reasons, "template rows not on the volume (reprovision/reload to re-seed config.yaml)")
			}
			if len(reasons) > 0 {
				row.Blocking = true
				row.Reason = "volume-native but " + strings.Join(reasons, " AND ")
			}
		}
		report = append(report, row)
	}

	dataSafe := !killSwitch && notNative == 0 && needMigration == 0 && needReseed == 0 && unverifiable == 0

	c.JSON(http.StatusOK, gin.H{
		"data_drop_safe":               dataSafe, // DATA gate only — see drop_still_requires
		"drop_still_requires":          dropStillRequires,
		"kill_switch_enabled":          killSwitch,
		"workspaces_with_live_rows":    len(ids),
		"not_volume_native":            notNative,
		"workspaces_needing_migration": needMigration,
		"workspaces_needing_reseed":    needReseed,
		"workspaces_unverifiable":      unverifiable,
		"orphan_rows":                  orphanRows, // -1 = count failed
		"workspaces":                   report,
	})
}

type volumeSyncState struct {
	gridCount       int
	runtimeMissing  int // source='runtime' DB rows absent-or-divergent on the volume; -1 if unverifiable
	templateMissing int // source='template' DB rows absent-or-divergent on the volume; -1 if unverifiable
}

// scheduleRow is one workspace_schedules row's definition (core `cron_expr`
// mapped to the volume grid's `cron`).
type scheduleRow struct {
	name, cron, timezone, prompt string
	enabled                      bool
}

// matchesGrid reports whether a live volume entry is present AND content-identical
// to this DB row. A same-named-but-divergent grid entry is NOT a match — the DROP
// would otherwise discard the DB's authoritative definition and leave the stale
// volume copy firing on the wrong cron/prompt.
func (r scheduleRow) matchesGrid(e volumeEntry, ok bool) bool {
	return ok && e.Cron == r.cron && e.Timezone == r.timezone && e.Prompt == r.prompt && e.Enabled == r.enabled
}

// rowsUnsyncedToVolume compares ALL of a volume-native workspace's DB rows — both
// sources, by CONTENT — against its live volume grid via a READ-ONLY quiet
// fan-out (never mints a secret, never fails the caller). Unreachable/unverifiable
// ⇒ -1 (the caller fails closed).
func rowsUnsyncedToVolume(ctx context.Context, workspaceID string) volumeSyncState {
	runtimeRows, templateRows, err := scheduleRowsBySource(ctx, workspaceID)
	if err != nil {
		return volumeSyncState{gridCount: -1, runtimeMissing: -1, templateMissing: -1}
	}
	grid, gridCount, err := volumeGridReadOnly(ctx, workspaceID)
	if err != nil {
		return volumeSyncState{gridCount: -1, runtimeMissing: -1, templateMissing: -1}
	}
	countUnsynced := func(rs []scheduleRow) int {
		m := 0
		for _, r := range rs {
			e, ok := grid[r.name]
			if !r.matchesGrid(e, ok) {
				m++
			}
		}
		return m
	}
	return volumeSyncState{
		gridCount:       gridCount,
		runtimeMissing:  countUnsynced(runtimeRows),
		templateMissing: countUnsynced(templateRows),
	}
}

// scheduleRowsBySource returns a workspace's DB schedule rows (full definitions)
// split by source.
func scheduleRowsBySource(ctx context.Context, workspaceID string) (runtime, template []scheduleRow, err error) {
	rows, qerr := db.DB.QueryContext(ctx, `
		SELECT name, cron_expr, timezone, prompt, enabled, source FROM workspace_schedules
		WHERE workspace_id = $1
	`, workspaceID)
	if qerr != nil {
		return nil, nil, qerr
	}
	defer rows.Close()
	for rows.Next() {
		var r scheduleRow
		var source string
		if serr := rows.Scan(&r.name, &r.cron, &r.timezone, &r.prompt, &r.enabled, &source); serr != nil {
			return nil, nil, serr
		}
		if source == "template" {
			template = append(template, r)
		} else {
			runtime = append(runtime, r)
		}
	}
	return runtime, template, rows.Err()
}

// volumeGridReadOnly fetches a workspace's live volume grid using a NON-healing
// inbound-secret read (so a readiness audit never mints a secret), returning the
// grid keyed by entry name + its size.
func volumeGridReadOnly(ctx context.Context, workspaceID string) (map[string]volumeEntry, int, error) {
	wsURL, secret, err := resolveScheduleFanoutTargetReadOnly(ctx, workspaceID)
	if err != nil {
		return nil, 0, err
	}
	return volumeGridEntries(ctx, wsURL, secret)
}

// volumeGridEntries fetches the grid with already-resolved creds (quiet), keyed
// by entry name.
func volumeGridEntries(ctx context.Context, wsURL, secret string) (map[string]volumeEntry, int, error) {
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
	byName := make(map[string]volumeEntry, len(grid.Schedules))
	for _, e := range grid.Schedules {
		byName[e.Name] = e
	}
	return byName, len(grid.Schedules), nil
}

// resolveScheduleFanoutTargetReadOnly is resolveScheduleFanoutTarget's read-only
// twin: it uses wsauth.ReadPlatformInboundSecret (NO lazy-heal/mint) so a
// read-only audit path can never persist a secret. A missing secret is returned
// as an error → the caller treats the workspace as unverifiable (fail closed).
func resolveScheduleFanoutTargetReadOnly(ctx context.Context, workspaceID string) (string, string, error) {
	var wsURL string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(url, '') FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&wsURL); err != nil {
		return "", "", fmt.Errorf("workspace lookup: %w", err)
	}
	if wsURL == "" {
		return "", "", errors.New("no callback URL (poll-mode / not registered)")
	}
	if err := isSafeURL(wsURL); err != nil {
		return "", "", fmt.Errorf("unsafe workspace URL rejected: %w", err)
	}
	secret, err := wsauth.ReadPlatformInboundSecret(ctx, db.DB, workspaceID)
	if err != nil {
		return "", "", fmt.Errorf("inbound secret unavailable (read-only): %w", err)
	}
	return wsURL, secret, nil
}

// p4bMigrateResult is the per-workspace outcome of a fleet migration.
type p4bMigrateResult struct {
	WorkspaceID string `json:"workspace_id"`
	Migrated    int    `json:"migrated"`
	Skipped     int    `json:"skipped"` // already on the volume (idempotent)
	Failed      int    `json:"failed"`
	Unreachable bool   `json:"unreachable,omitempty"`
	DBError     bool   `json:"db_error,omitempty"` // the source-row read errored (count unknown)
}

// MigrateAllToVolume copies every volume-native workspace's source='runtime'
// schedules from the DB into its volume grid. Dry-run by default (?apply=true to
// execute). Idempotent and copy-only — never deletes a DB row. Unreachable
// workspaces are logged and skipped, never failing the whole sweep. A DB read
// error on a workspace is surfaced distinctly (db_errors) — NOT hidden as green.
//
// Fan-out is SEQUENTIAL over the request context (each workspace bounded by
// scheduleForwardTimeout). On a very large/slow fleet a single call may exceed
// the caller's timeout; because the operation is idempotent (rows already on the
// volume are skipped), the safe recovery is simply to re-invoke — a follow-up may
// add bounded concurrency + an overall deadline.
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
	totalMigrated, totalSkipped, totalFailed, unreachable, dbErrors := 0, 0, 0, 0, 0

	for _, id := range targets {
		res := migrateWorkspaceRuntimeToVolume(ctx, id, apply)
		if res.Unreachable {
			unreachable++
			log.Printf("MigrateAllToVolume: workspace %s unreachable, skipped", id)
		}
		if res.DBError {
			dbErrors++
			log.Printf("MigrateAllToVolume: workspace %s source-row read errored; migration incomplete", id)
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
		"db_errors":      dbErrors, // >0 ⇒ some workspaces were NOT fully migrated (DB read failed)
		"results":        results,
	})
}

// migrateWorkspaceRuntimeToVolume copies one volume-native workspace's
// source='runtime' rows to its volume grid via the quiet fan-out client. On a
// dry-run (apply=false) it counts what WOULD migrate without writing. Idempotent:
// rows already on the volume are skipped. Copy-only — never deletes a DB row.
func migrateWorkspaceRuntimeToVolume(ctx context.Context, workspaceID string, apply bool) p4bMigrateResult {
	res := p4bMigrateResult{WorkspaceID: workspaceID}

	wsURL, secret, err := resolveScheduleFanoutTarget(ctx, workspaceID)
	if err != nil {
		res.Unreachable = true
		return res
	}
	grid, _, err := volumeGridEntries(ctx, wsURL, secret)
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
		res.DBError = true // systemic DB fault — surfaced, not a silent green
		return res
	}
	defer rows.Close()

	for rows.Next() {
		var e volumeEntry
		if err := rows.Scan(&e.Name, &e.Cron, &e.Timezone, &e.Prompt, &e.Enabled); err != nil {
			res.Failed++
			continue
		}
		if _, onVolume := grid[e.Name]; onVolume {
			res.Skipped++ // name present → never re-create (copy-only, idempotent)
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
			grid[e.Name] = e
		} else {
			res.Failed++
		}
	}
	// A mid-stream read error ends the loop with no Scan error — surface it so a
	// truncated migration is never reported as complete.
	if err := rows.Err(); err != nil {
		res.DBError = true
	}
	return res
}

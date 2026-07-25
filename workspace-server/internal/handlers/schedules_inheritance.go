package handlers

// schedules_inheritance.go — the RESTORE half of the P4b volume-side org-re-import
// schedule inheritance path (core#4435). Its capture counterpart lives in
// workspace_crud.go (captureRuntimeSchedulesForCarryover), invoked from
// CascadeDelete before the predecessor container is torn down.
//
// Why this exists: when a volume-native org agent is re-imported/re-added onto a
// FRESH data volume, the successor mints a NEW workspace id + volume and the
// predecessor's runtime-authored schedule grid (owned by the trigger daemon, on
// the OLD volume) is abandoned. The predecessor's /internal/schedules API dies the
// instant its container stops, so the grid cannot be read at restore time — it was
// CAPTURED at teardown into the removed predecessor row's carryover_runtime_schedules
// column. This replays that buffer onto the successor once it is online and
// advertises the scheduler capability, then clears the buffer (one-shot).
//
// ADDITIVE + DARK: fired alongside the plugin reconcile on transition-to-online.
// It no-ops for every workspace with no removed predecessor carrying a buffer
// (the overwhelmingly common case) and is fully idempotent, so it is safe to fire
// on every online transition. It replaces the legacy DB-world re-point path
// (removed in P4b) as the sole runtime-schedule inheritance mechanism.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// RestoreInheritedRuntimeSchedules replays a removed predecessor's captured
// runtime schedule grid onto the freshly-online workspace newID and clears the
// buffer on success. The removed-predecessor MATCH predicate is role when
// present, name fallback, same parent — so the successor inherits from exactly
// the predecessor it recreates.
//
// Signature matches ReconcileFunc so the registry heartbeat can fire it alongside
// the declared-plugin reconcile on transition-to-online. Best-effort throughout:
// every failure logs and returns WITHOUT clearing the buffer, so the next online
// transition retries and the whole thing converges. Idempotent: entries already
// present on the new volume are skipped (no duplicates).
func (h *ScheduleHandler) RestoreInheritedRuntimeSchedules(ctx context.Context, newID string) {
	if db.DB == nil {
		return
	}

	// (a) Load the new workspace's identity fields for the predecessor match.
	var role, name sql.NullString
	var parentID *string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT role, name, parent_id FROM workspaces WHERE id = $1`, newID,
	).Scan(&role, &name, &parentID); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("schedule-inherit: identity lookup for %s failed: %v — skipping restore", newID, err)
		}
		return
	}

	// (b) Find the most-recent removed predecessor of THIS agent that carries a
	// captured buffer. Reuse the exact match predicate the DB-world path uses
	// (role when stable, else name+parent), plus the cheap NOT NULL filter so the
	// common "no buffer" case selects zero rows and returns immediately.
	var predID string
	var carryover []byte
	var err error
	if role.Valid && role.String != "" {
		err = db.DB.QueryRowContext(ctx, `
			SELECT id, carryover_runtime_schedules FROM workspaces
			WHERE status = 'removed' AND role = $1
			  AND parent_id IS NOT DISTINCT FROM $2
			  AND id <> $3
			  AND carryover_runtime_schedules IS NOT NULL
			ORDER BY updated_at DESC NULLS LAST
			LIMIT 1
		`, role.String, parentID, newID).Scan(&predID, &carryover)
	} else {
		err = db.DB.QueryRowContext(ctx, `
			SELECT id, carryover_runtime_schedules FROM workspaces
			WHERE status = 'removed' AND name = $1
			  AND parent_id IS NOT DISTINCT FROM $2
			  AND id <> $3
			  AND carryover_runtime_schedules IS NOT NULL
			ORDER BY updated_at DESC NULLS LAST
			LIMIT 1
		`, name.String, parentID, newID).Scan(&predID, &carryover)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return // no removed predecessor carrying a buffer — clean no-op
	}
	if err != nil {
		log.Printf("schedule-inherit: predecessor lookup for %s failed: %v — skipping restore", newID, err)
		return
	}

	var carried []volumeEntry
	if err := json.Unmarshal(carryover, &carried); err != nil {
		log.Printf("schedule-inherit: malformed carryover buffer on predecessor %s: %v — skipping restore", predID, err)
		return
	}
	if len(carried) == 0 {
		// Nothing to restore; still clear so this degenerate buffer is one-shot.
		h.clearCarryover(ctx, predID)
		return
	}

	// (c) Declare the scheduler plugin so the daemon installs on the successor
	// even if its new template ships no schedules of its own — otherwise the
	// carried schedules would have no daemon to fire them.
	if err := ensureSchedulerPluginDeclared(ctx, newID); err != nil {
		log.Printf("schedule-inherit: declaring scheduler plugin on %s failed: %v — skipping restore (retries next online)", newID, err)
		return
	}

	// (d) If the successor is not YET volume-native (its scheduler capability
	// hasn't been advertised on a heartbeat), we cannot serve its volume grid.
	// Return WITHOUT clearing: declaring the plugin above will bring the daemon
	// up, and the next transition-to-online re-fires this and converges.
	if !ProvidesNativeScheduler(newID) {
		log.Printf("schedule-inherit: %s not yet volume-native (scheduler capability unadvertised) — deferring restore of %d schedule(s) from %s", newID, len(carried), predID)
		return
	}

	// (e) Resolve the successor runtime + read its current grid so we skip any
	// name already present (idempotency / template-collision safety). Unreachable
	// → return without clearing (retry next online).
	wsURL, secret, err := resolveScheduleFanoutTarget(ctx, newID)
	if err != nil {
		log.Printf("schedule-inherit: resolve %s failed: %v — deferring restore", newID, err)
		return
	}
	status, body, err := fetchScheduleAPI(ctx, wsURL, secret, http.MethodGet, "", nil)
	if err != nil {
		log.Printf("schedule-inherit: grid read %s failed: %v — deferring restore", newID, err)
		return
	}
	if status != http.StatusOK {
		log.Printf("schedule-inherit: grid read %s returned %d — deferring restore", newID, status)
		return
	}
	var grid struct {
		Schedules []volumeEntry `json:"schedules"`
	}
	if err := json.Unmarshal(body, &grid); err != nil {
		log.Printf("schedule-inherit: malformed grid from %s: %v — deferring restore", newID, err)
		return
	}
	present := make(map[string]bool, len(grid.Schedules))
	for _, e := range grid.Schedules {
		present[e.Name] = true
	}

	// (f) POST each carried entry whose name is not already present. The runtime's
	// create() re-stamps source='runtime', so we set it here too; a name already
	// on the grid is skipped (template wins on collision — preserved from the DB
	// world's NOT EXISTS guard).
	posted, skipped, failed := 0, 0, 0
	for _, e := range carried {
		if present[e.Name] {
			skipped++
			continue
		}
		e.Source = "runtime"
		raw, mErr := json.Marshal(e)
		if mErr != nil {
			failed++
			log.Printf("schedule-inherit: marshal carried entry %q for %s failed: %v", e.Name, newID, mErr)
			continue
		}
		st, _, pErr := fetchScheduleAPI(ctx, wsURL, secret, http.MethodPost, "", raw)
		if pErr != nil {
			failed++
			log.Printf("schedule-inherit: POST carried entry %q to %s failed: %v", e.Name, newID, pErr)
			continue
		}
		if st != http.StatusCreated {
			failed++
			log.Printf("schedule-inherit: POST carried entry %q to %s returned %d", e.Name, newID, st)
			continue
		}
		posted++
		present[e.Name] = true
	}

	// (g) Arm the running daemon so the carried schedules start ticking promptly
	// rather than waiting for a restart. Best-effort (armSchedulerPlugin swallows
	// its own errors; the reconcile-on-online safety net arms it regardless).
	armSchedulerPlugin(ctx, newID)

	log.Printf("schedule-inherit: restored predecessor %s → %s: posted=%d skipped=%d failed=%d (of %d carried)", predID, newID, posted, skipped, failed, len(carried))

	// (h) Clear the buffer ONLY when every carried entry landed (posted or already
	// present). A partial failure leaves the buffer set so the next online
	// transition retries just the missing ones (present-name skip makes that
	// convergent and duplicate-free).
	if failed == 0 {
		h.clearCarryover(ctx, predID)
	} else {
		log.Printf("schedule-inherit: leaving carryover buffer on %s set — %d entry(ies) failed, will retry on next online", predID, failed)
	}
}

// clearCarryover nulls the one-shot buffer on the removed predecessor row so a
// subsequent online transition does not re-restore. Best-effort: a failure just
// means the next transition re-runs (idempotent — already-present names skip).
func (h *ScheduleHandler) clearCarryover(ctx context.Context, predID string) {
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET carryover_runtime_schedules = NULL, updated_at = now() WHERE id = $1`,
		predID); err != nil {
		log.Printf("schedule-inherit: clearing carryover buffer on %s failed: %v (harmless — restore is idempotent)", predID, err)
	}
}

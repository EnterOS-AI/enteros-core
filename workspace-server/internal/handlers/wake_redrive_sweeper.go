package handlers

// wake_redrive_sweeper.go — the fleet-wide periodic driver for the
// generation-loop RE-DRIVE (wake_redrive.go). Modeled on StallWatchdog /
// RequestNudgeSweeper: a context-cancellable ticker that, each tick, runs ONE
// fleet query for the DISTINCT workspaces that currently have a STUCK wake
// intent, then calls the re-drive owner (ReDriveStuckWakes) once per such
// workspace.
//
// WHY A SWEEP, NOT THE HEARTBEAT. Re-drive is a slow-path janitor: a stuck
// intent is age-gated at redriveStuckAfter (10min), so nothing is ever "stuck"
// at sub-minute timescales. Running it on the heartbeat would mean a per-beat,
// per-workspace SELECT for a condition that changes on the order of minutes.
// One fleet query every few minutes is the right cost profile and matches the
// codebase's established janitor pattern; the heartbeat stays settle-only.

import (
	"context"
	"database/sql"
	"log"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

const (
	// defaultWakeRedriveInterval is the sweep cadence. A stuck intent is
	// age-gated at redriveStuckAfter (10min) and re-driven at most once per
	// redriveMinInterval (10min), so a 5min tick is comfortably frequent enough
	// to catch a newly-stuck intent promptly without ever re-driving one twice in
	// a window. Override via WAKE_REDRIVE_SWEEPER_INTERVAL_S.
	defaultWakeRedriveInterval = 5 * time.Minute

	// wakeRedriveFleetLimit bounds the DISTINCT workspaces re-driven per tick so
	// one sweep can never fan out unboundedly across a large fleet; the next tick
	// picks up any remainder. Each of those workspaces is itself bounded by
	// redriveBatchCap intents inside ReDriveStuckWakes.
	wakeRedriveFleetLimit = 500
)

// WakeRedriveSweeper runs the periodic stuck-wake re-drive sweep. Construct via
// NewWakeRedriveSweeper, then Start(ctx) (via supervised.RunWithRecover in
// main.go) to begin ticking.
type WakeRedriveSweeper struct {
	db       *sql.DB
	interval time.Duration
	limit    int

	// redrive is the re-drive owner (WorkspaceHandler.ReDriveStuckWakes in
	// production; a recorder in tests). Injected so the sweeper does not depend on
	// the WorkspaceHandler type. nil = the sweep is a no-op.
	redrive func(ctx context.Context, workspaceID string) (ReDriveResult, error)

	// reEmitWired reports whether the re-emit hook is set on the owner. When it
	// returns false the sweep is a full no-op — it does not even run the fleet
	// query (nothing to re-emit ⇒ nothing to drive), so an unwired deployment
	// never scans. A nil predicate is treated as "wired" so a test that injects
	// only `redrive` still sweeps.
	reEmitWired func() bool
}

// NewWakeRedriveSweeper builds a sweeper bound to the package db.DB (production)
// or a test handle. redrive is WorkspaceHandler.ReDriveStuckWakes and reEmitWired
// is WorkspaceHandler.WakeReEmitWired. Reads the optional interval override at
// construction time so a long-running process picks it up via restart, not
// mid-flight (mirrors NewStallWatchdog / NewRequestNudgeSweeper).
func NewWakeRedriveSweeper(
	handle *sql.DB,
	redrive func(ctx context.Context, workspaceID string) (ReDriveResult, error),
	reEmitWired func() bool,
) *WakeRedriveSweeper {
	if handle == nil {
		handle = db.DB
	}
	return &WakeRedriveSweeper{
		db:          handle,
		interval:    envDuration("WAKE_REDRIVE_SWEEPER_INTERVAL_S", defaultWakeRedriveInterval),
		limit:       wakeRedriveFleetLimit,
		redrive:     redrive,
		reEmitWired: reEmitWired,
	}
}

// Interval exposes the configured tick cadence — tests use it; main.go uses it
// implicitly via Start.
func (s *WakeRedriveSweeper) Interval() time.Duration { return s.interval }

// Start ticks Sweep() at the configured interval until ctx is cancelled. Defers
// panic recovery so a single bad row can't kill the sweeper. Mirrors
// StallWatchdog.Start: the first sweep fires immediately on startup.
func (s *WakeRedriveSweeper) Start(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	log.Printf("WakeRedriveSweeper: started (interval=%s, stale-after=%s, re-drive-interval=%s, max-attempts=%d)",
		s.interval, redriveStuckAfter, redriveMinInterval, redriveMaxAttempts)

	tickWithRecover := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("WakeRedriveSweeper: PANIC in tick — recovered: %v", r)
			}
		}()
		s.Sweep(ctx)
	}

	tickWithRecover()

	for {
		select {
		case <-ctx.Done():
			log.Printf("WakeRedriveSweeper: stopped")
			return
		case <-t.C:
			tickWithRecover()
		}
	}
}

// RedriveSweepResult records what the last sweep did — returned for
// observability and so tests assert behavior without diffing log lines.
type RedriveSweepResult struct {
	Workspaces int // distinct stuck workspaces driven this sweep
	Redriven   int // intents re-emitted across those workspaces
	Dropped    int // intents abandoned (attempt cap) across those workspaces
	Errors     int
}

// Sweep runs one pass: find the DISTINCT workspaces that currently have at least
// one stuck non-terminal wake intent, then re-drive each. The stuck predicate
// mirrors ReDriveStuckWakes's own selection (non-terminal status, past
// redriveStuckAfter, and either never re-driven or past redriveMinInterval) so a
// workspace is returned here iff the owner would find something to do for it —
// no wasted per-workspace calls.
//
// Full no-op when the re-emit hook is unwired (checked BEFORE the fleet query)
// or no owner is injected. Best-effort per workspace: a per-workspace error is
// logged and the sweep continues to the next.
func (s *WakeRedriveSweeper) Sweep(ctx context.Context) RedriveSweepResult {
	res := RedriveSweepResult{}
	if s.redrive == nil || (s.reEmitWired != nil && !s.reEmitWired()) {
		return res
	}

	//ssot:allow-status-set wake_intents is a SEPARATE table with its own status
	// vocabulary (pending/dispatched/delivered/settled/dropped); this non-terminal
	// filter is NOT the delegations lifecycle and must not derive from it.
	const fleetQuery = `
		SELECT DISTINCT workspace_id
		  FROM wake_intents
		 WHERE status IN ('pending','dispatched','delivered')
		   AND created_at < now() - ($1 * INTERVAL '1 second')
		   AND (last_redriven_at IS NULL OR last_redriven_at < now() - ($2 * INTERVAL '1 second'))
		 LIMIT $3
	`
	rows, err := s.db.QueryContext(ctx, fleetQuery,
		int(redriveStuckAfter.Seconds()), int(redriveMinInterval.Seconds()), s.limit)
	if err != nil {
		log.Printf("WakeRedriveSweeper: fleet query failed: %v", err)
		res.Errors++
		return res
	}
	var workspaceIDs []string
	for rows.Next() {
		var wsID string
		if scanErr := rows.Scan(&wsID); scanErr != nil {
			log.Printf("WakeRedriveSweeper: scan failed: %v", scanErr)
			res.Errors++
			continue
		}
		workspaceIDs = append(workspaceIDs, wsID)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		log.Printf("WakeRedriveSweeper: rows.Err: %v", rowsErr)
		res.Errors++
	}
	rows.Close()

	for _, wsID := range workspaceIDs {
		r, driveErr := s.redrive(ctx, wsID)
		if driveErr != nil {
			log.Printf("WakeRedriveSweeper: re-drive failed for %s: %v", wsID, driveErr)
			res.Errors++
			continue
		}
		res.Workspaces++
		res.Redriven += r.Redriven
		res.Dropped += r.Dropped
	}

	if res.Workspaces > 0 || res.Errors > 0 {
		log.Printf("WakeRedriveSweeper: sweep complete — workspaces=%d redriven=%d dropped=%d errors=%d",
			res.Workspaces, res.Redriven, res.Dropped, res.Errors)
	}
	return res
}

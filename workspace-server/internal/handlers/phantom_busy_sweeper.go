package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/metrics"
)

// phantom_busy_sweeper.go — task-lifecycle counter-drift reset.
//
// What it does
// ------------
// Periodically resets workspaces stuck with active_tasks > 0 but no recent
// activity_log entry (within phantomStaleThreshold). This happens when an agent
// errors out (MiniMax timeout, OOM, etc.) and the finally block fails to
// decrement active_tasks, leaving the counter elevated forever.
//
// Why it is its own worker (P4)
// -----------------------------
// This sweep used to live inside the core cron scheduler's tick loop, because a
// phantom-busy workspace made the scheduler skip its cron fires ("workspace
// busy — retry"). The scheduler-as-trigger-plugin RFC (P4) retired that loop —
// schedules now fire from the runtime daemon against the workspace volume — but
// the phantom-busy problem is NOT scheduler-specific: active_tasks > 0 gates
// A2A dispatch (a2a_queue), hibernation, discovery, the wedged-agent detector,
// and the request-nudge sweeper. A drifted counter therefore makes a workspace
// look permanently "busy" platform-wide. So the sweep is preserved here as a
// first-class worker rather than being dropped with the scheduler.
//
// Relation to the stall-watchdog
// -------------------------------
// Distinct remediations. The stall-watchdog restarts a workspace that is
// genuinely hung (online, active_tasks>0, producing NO activity past a grace
// window). This sweep is a pure counter reset for a workspace whose accounting
// drifted but is otherwise healthy — no restart, just UPDATE active_tasks = 0.
//
// Frequency
// ---------
// 5min default cadence (PHANTOM_BUSY_SWEEPER_INTERVAL_S to override), matching
// the cadence it ran at inside the scheduler. Disable via
// PHANTOM_BUSY_SWEEPER_DISABLED=true (checked in main.go wiring).

const (
	defaultPhantomBusyInterval = 5 * time.Minute

	// phantomBusyStaleThreshold — a workspace must have had NO activity_log
	// entry for at least this long before its elevated active_tasks counter is
	// treated as drift and reset. Gives an in-flight-but-quiet turn room to
	// finish before we zero its counter.
	phantomBusyStaleThreshold = 10 * time.Minute
)

// PhantomBusySweeper zeros drifted active_tasks counters. Mirrors the other
// handlers-package sweepers (DelegationSweeper, RequestNudgeSweeper,
// StallWatchdog): db handle + env-tunable interval, panic-guarded tick loop.
type PhantomBusySweeper struct {
	db         *sql.DB
	interval   time.Duration
	staleAfter time.Duration
}

// NewPhantomBusySweeper builds a sweeper bound to the package db.DB (production
// wiring) or a test handle. Reads its optional env override at construction
// time so a long-running process picks it up via restart, not mid-flight.
func NewPhantomBusySweeper(handle *sql.DB) *PhantomBusySweeper {
	if handle == nil {
		handle = db.DB
	}
	return &PhantomBusySweeper{
		db:         handle,
		interval:   envDuration("PHANTOM_BUSY_SWEEPER_INTERVAL_S", defaultPhantomBusyInterval),
		staleAfter: phantomBusyStaleThreshold,
	}
}

// Interval exposes the configured tick cadence (tests use it).
func (s *PhantomBusySweeper) Interval() time.Duration { return s.interval }

// Start ticks Sweep() at the configured interval until ctx is cancelled. First
// sweep fires immediately on startup; a panic in one tick can't kill the loop.
func (s *PhantomBusySweeper) Start(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	log.Printf("PhantomBusySweeper: started (interval=%s, stale-after=%s)", s.interval, s.staleAfter)

	tickWithRecover := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PhantomBusySweeper: PANIC in tick — recovered: %v", r)
			}
		}()
		s.Sweep(ctx)
	}

	tickWithRecover()

	for {
		select {
		case <-ctx.Done():
			log.Printf("PhantomBusySweeper: stopped")
			return
		case <-t.C:
			tickWithRecover()
		}
	}
}

// Sweep resets active_tasks for workspaces with a drifted counter and returns
// the number reset. The query mirrors the manual fix that was run every ~30min
// before this was automated:
//
//	UPDATE workspaces SET active_tasks = 0
//	WHERE active_tasks > 0
//	  AND id NOT IN (SELECT DISTINCT workspace_id FROM activity_logs
//	                 WHERE created_at > NOW() - INTERVAL '10 minutes')
func (s *PhantomBusySweeper) Sweep(ctx context.Context) int {
	handle := s.db
	if handle == nil {
		handle = db.DB
	}
	rows, err := handle.QueryContext(ctx, `
		UPDATE workspaces
		SET active_tasks = 0,
		    current_task = '',
		    updated_at   = now()
		WHERE active_tasks > 0
		  AND status != 'removed'
		  AND id NOT IN (
		      SELECT DISTINCT workspace_id
		      FROM activity_logs
		      WHERE created_at > NOW() - $1::interval
		  )
		RETURNING id, name
	`, fmt.Sprintf("%d minutes", int(s.staleAfter.Minutes())))
	if err != nil {
		log.Printf("PhantomBusySweeper: sweep query error: %v", err)
		return 0
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			log.Printf("PhantomBusySweeper: sweep scan error: %v", err)
			continue
		}
		log.Printf("PhantomBusySweeper: reset %s (no activity in %d min)", name, int(s.staleAfter.Minutes()))
		// #2865: surface as molecule_phantom_busy_resets_total. A high reset
		// rate signals task-lifecycle accounting regressions (e.g. missing env
		// vars causing claude --print timeouts that leave active_tasks elevated
		// until this sweep fires).
		metrics.TrackPhantomBusyReset()
		count++
	}
	if err := rows.Err(); err != nil {
		log.Printf("PhantomBusySweeper: sweep rows error: %v", err)
	}
	if count > 0 {
		log.Printf("PhantomBusySweeper: complete — reset %d workspace(s)", count)
	}
	return count
}

package handlers

import (
	"context"
	"log"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// desktopIdleSource is the read side the idle sweeper needs (the DB store
// satisfies it). Injected so the sweep DECISION logic is unit-testable without
// a Postgres.
type desktopIdleSource interface {
	RunningDesktopWorkspaceIDs(ctx context.Context) ([]string, error)
	LoadActivity(ctx context.Context, workspaceID string) (provisioner.DesktopActivity, error)
}

// DesktopIdleSweeper tears down desktop sidecars that have gone idle (design
// §10), reusing the tested DesktopIsIdle decision. It re-loads each desktop's
// activity immediately before deciding, so a desktop that went active between
// the running-scan and the check is NOT reaped (closes the hibernation-monitor
// TOCTOU the review flagged).
type DesktopIdleSweeper struct {
	src         desktopIdleSource
	teardown    func(ctx context.Context, workspaceID string) error
	idleTimeout time.Duration
	now         func() time.Time
}

// NewDesktopIdleSweeper builds the sweeper. teardown is typically
// WorkspaceHandler.StopDesktopAuto.
func NewDesktopIdleSweeper(src desktopIdleSource, teardown func(context.Context, string) error, idleTimeout time.Duration) *DesktopIdleSweeper {
	return &DesktopIdleSweeper{src: src, teardown: teardown, idleTimeout: idleTimeout, now: time.Now}
}

// Start ticks Sweep at interval until ctx is cancelled, recovering from a
// per-tick panic so a single bad row can't kill the reaper. Runs one sweep
// immediately so operators see it on startup, not after one interval. Wired
// into main.go as `go sweeper.Start(ctx, interval)`.
func (s *DesktopIdleSweeper) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	log.Printf("DesktopIdleSweeper: started (interval=%s, idle-timeout=%s)", interval, s.idleTimeout)

	tick := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("DesktopIdleSweeper: PANIC in sweep — recovered: %v", r)
			}
		}()
		if err := s.Sweep(ctx); err != nil {
			log.Printf("DesktopIdleSweeper: sweep error: %v", err)
		}
	}

	tick()
	for {
		select {
		case <-ctx.Done():
			log.Printf("DesktopIdleSweeper: stopped")
			return
		case <-t.C:
			tick()
		}
	}
}

// Sweep evaluates every running desktop once and tears down the idle ones.
func (s *DesktopIdleSweeper) Sweep(ctx context.Context) error {
	ids, err := s.src.RunningDesktopWorkspaceIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		act, err := s.src.LoadActivity(ctx, id)
		if err != nil {
			// Skip on a read error rather than risk reaping a desktop we
			// couldn't evaluate — fail toward keeping it up.
			continue
		}
		if provisioner.DesktopIsIdle(s.now(), act, s.idleTimeout) {
			if err := s.teardown(ctx, id); err != nil {
				log.Printf("DesktopIdleSweeper: teardown %s failed: %v", id, err)
			}
		}
	}
	return nil
}

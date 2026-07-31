package handlers

// plugin_drift_applier.go — the DETERMINISTIC consumer of plugin_update_queue
// (core#4977).
//
// # Why this exists
//
// The drift sweeper (plugins/drift_sweeper.go) DETECTS that a plugin's
// upstream ref moved and enqueues a pending row. Until this file, the only
// thing that ever APPLIED such a row was the concierge's
// `plugin-auto-update` agent schedule (cron 0 3 * * *) calling
// POST /admin/plugin-updates/:id/apply.
//
// That left convergence depending on an LLM agent waking up and choosing to
// call an admin endpoint — and on a tenant whose schedules were never seeded
// (workspace_schedules empty) nothing called it at all. Drift was detected,
// queued, and then sat forever: a running workspace never picked up a new
// commit on a branch-pinned plugin, which is exactly the reported symptom.
//
// Convergence is a correctness property, so it must not be agent-mediated.
// This drainer is a plain in-process loop, the same shape as the sweeper it
// pairs with.
//
// # Relationship to the HTTP endpoint
//
// Both paths call applyQueuedDrift — there is ONE apply implementation. The
// handler maps its result to HTTP; the drainer logs and continues. Adding a
// second copy of the apply logic here would guarantee the two drift apart.
//
// # Error posture
//
// Per-entry failures are isolated and non-fatal: one unreachable upstream
// must not stop the rest of the queue draining. Nothing here retries in a
// tight loop — a failed entry stays 'pending' and is retried on the next
// tick.

import (
	"context"
	"errors"
	"log"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// DriftApplyInterval is the cadence of the queue drainer.
//
// Deliberately shorter than the sweeper's 1h detection interval: the drainer
// is cheap (a SELECT that is empty in the common case) and this bounds the
// tail between "drift detected" and "workspace converged". End-to-end worst
// case for a branch-pinned plugin is therefore ~1h (detect) + ~10m (apply).
const DriftApplyInterval = 10 * time.Minute

// driftApplyMaxPerTick bounds how many entries one tick will apply. Each apply
// does a git fetch + (usually) a workspace restart, so an unbounded drain
// after a large fan-out event would restart the whole fleet at once. Excess
// entries are picked up on the next tick.
const driftApplyMaxPerTick = 25

// errDriftEntryNotFound / errDriftEntryDismissed / errDriftEntryAlreadyApplied
// are the sentinel outcomes the HTTP handler maps to 404 / 409 / 200. The
// drainer treats them as "nothing to do" and moves on.
var (
	errDriftEntryNotFound       = errors.New("drift queue entry not found")
	errDriftEntryDismissed      = errors.New("drift queue entry was dismissed")
	errDriftEntryAlreadyApplied = errors.New("drift queue entry already applied")
	errDriftPluginRowMissing    = errors.New("workspace_plugins row not found — plugin may have been uninstalled")
)


// Stage/deliver seams, mirroring the `resolveSourceSHA` seam in
// plugins_reconcile.go. Production points them at the real pipeline; tests
// swap them so the apply STATE MACHINE (what gets persisted after a deferred
// restart) is exercisable without a git fetch or a Docker daemon — the branch
// that decides whether a box is reported converged is exactly the branch that
// must not be left untested.
var (
	driftStageFn = func(h *PluginsHandler, ctx context.Context, req installRequest) (*stageResult, error) {
		return h.ResolveAndStageForApply(ctx, req)
	}
	driftDeliverFn = func(h *PluginsHandler, ctx context.Context, workspaceID string, r *stageResult) error {
		return h.DeliverForApply(ctx, workspaceID, r)
	}
)

// driftApplyOutcome is the result of applying one queue entry.
type driftApplyOutcome struct {
	WorkspaceID string
	PluginName  string
	// InstalledSHA is the SHA that was staged. It is only PERSISTED to
	// workspace_plugins when Materialized is true — see the Materialized note.
	InstalledSHA string
	// Restarting reports whether a workspace restart was dispatched.
	Restarting bool
	// Materialized reports whether the new bytes will actually reach the box.
	//
	// On a docker-less tenant the apply path copies NO bytes (errNoPushTarget
	// — "deliver by pull"); the RESTART is what makes the boot installer pull
	// the new commit. So when the restart is deferred by the self-brick guard
	// (a kind=platform concierge in its fragile window), nothing materializes.
	//
	// In that case we must NOT advance installed_sha. Doing so would tell the
	// next drift sweep "already at the new SHA" while the on-disk tree is
	// still the old one — a permanently stale box that reports converged.
	// Leaving the SHA behind means the sweeper re-detects and re-queues, so
	// the drift stays VISIBLE until the concierge is deliberately restarted.
	Materialized bool
}

// selectPendingDriftIDs returns the ids of pending queue entries, oldest
// first, capped at limit. Oldest-first so a backlog drains in the order drift
// was detected rather than starving early entries.
func selectPendingDriftIDs(ctx context.Context, limit int) ([]string, error) {
	rows, err := db.DB.QueryContext(ctx, `
		SELECT id
		  FROM plugin_update_queue
		 WHERE status = 'pending'
		 ORDER BY created_at ASC
		 LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			return nil, scanErr
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DrainPendingDrift applies every pending queue entry (up to
// driftApplyMaxPerTick) and returns how many applied and how many failed.
//
// applyFn is a seam: production passes h.applyQueuedDrift; tests pass a stub
// so the drain CONTRACT (selection, ordering, per-entry error isolation) is
// testable without a live git fetch or a Docker daemon.
func DrainPendingDrift(
	ctx context.Context,
	applyFn func(context.Context, string) (*driftApplyOutcome, error),
) (applied int, failed int) {
	ids, err := selectPendingDriftIDs(ctx, driftApplyMaxPerTick)
	if err != nil {
		log.Printf("Plugin drift applier: SELECT pending failed: %v", err)
		return 0, 0
	}
	for _, id := range ids {
		outcome, applyErr := applyFn(ctx, id)
		if applyErr != nil {
			// Sentinels mean the row is no longer actionable — not a failure.
			if errors.Is(applyErr, errDriftEntryNotFound) ||
				errors.Is(applyErr, errDriftEntryDismissed) ||
				errors.Is(applyErr, errDriftEntryAlreadyApplied) {
				continue
			}
			failed++
			log.Printf("Plugin drift applier: apply %s failed: %v — stays pending, retried next tick", id, applyErr)
			continue
		}
		applied++
		log.Printf("Plugin drift applier: applied %s (workspace=%s plugin=%s restarting=%t materialized=%t)",
			id, outcome.WorkspaceID, outcome.PluginName, outcome.Restarting, outcome.Materialized)
	}
	return applied, failed
}

// StartPluginDriftApplier runs the drain loop until ctx is cancelled. Mirrors
// plugins.StartPluginDriftSweeper's shape (immediate first run, ctx-guarded
// ticker) so the detect/apply pair behaves identically under shutdown.
func StartPluginDriftApplier(ctx context.Context, h *AdminPluginDriftHandler) {
	if h == nil || h.pluginsHandler == nil {
		log.Println("Plugin drift applier: no plugins handler — applier disabled")
		return
	}
	log.Printf("Plugin drift applier started — interval %s", DriftApplyInterval)
	ticker := time.NewTicker(DriftApplyInterval)
	defer ticker.Stop()

	DrainPendingDrift(ctx, h.applyQueuedDrift)
	for {
		select {
		case <-ctx.Done():
			log.Println("Plugin drift applier: shutdown")
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				continue
			}
			DrainPendingDrift(ctx, h.applyQueuedDrift)
		}
	}
}

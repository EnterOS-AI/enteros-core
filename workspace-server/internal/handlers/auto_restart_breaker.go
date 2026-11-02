package handlers

// auto_restart_breaker.go — crash-loop breaker for the platform's automatic
// restarts (core#5241).
//
// Every automatic recovery path — the liveness monitor, the health sweep, the
// CP instance reconciler, the A2A proxy's dead-container detection and the
// stall watchdog — restarts a workspace it finds down, and none of them knew
// how often that had already happened. A workspace whose replacements die the
// same way every time was restarted every few minutes for as long as the cause
// lasted. After autoRestartLimit automatic restarts inside autoRestartWindow
// the breaker trips: the workspace is marked failed with the reason, which the
// canvas shows on the node and in its Events tab, and no automatic path
// restarts it again until someone acts on it (resetAutoRestarts).
//
// Only a restart that stopped the failed container counts. One declined at the
// pinned-image check left the container as it was; counting it would let a few
// minutes of image outage trip the breaker, and keep the workspace down after
// the image is back.
//
// The count and the trip live in process memory, on restartState, next to the
// debounce and coalescing state they extend. The verdict is durable — status
// 'failed', last_sample_error and the structure_events row — and none of the
// sweeps select a failed row. A workspace-server restart therefore loses only
// the bookkeeping, and that is bounded: at worst autoRestartLimit more
// automatic restarts before the breaker trips again.

import (
	"context"
	"fmt"
	"log"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
)

const (
	// One automatic restart is a recovery; autoRestartLimit of them inside
	// autoRestartWindow mean each replacement went down again, and another
	// restart will not change that. The self-fire debounce keeps automatic
	// restarts at least RestartDebounceWindow apart, so a burst of probes
	// reporting one death cannot reach the limit on its own.
	autoRestartLimit  = 3
	autoRestartWindow = 30 * time.Minute
)

// AutoRestartByID is RestartByID for the automatic recovery paths, behind the
// crash-loop breaker. trigger names the caller in the log line and in the
// failure event. User and mutation restarts call RestartByID and are never
// refused.
func (h *WorkspaceHandler) AutoRestartByID(workspaceID, trigger string) {
	if !h.HasProvisioner() {
		return
	}
	// A probe inside the self-fire debounce reports a restart that is already
	// under way: RestartByID drops it, so it must not trip the breaker either.
	if !shouldDebounceRestart(workspaceID) && autoRestartHalted(workspaceID, time.Now()) {
		h.haltAutoRestarts(workspaceID, trigger)
		return
	}
	restartByIDWithCycle(workspaceID, true, true, autoRestartCycle(trigger, func(autoTrigger string) {
		h.runRestartCycle(workspaceID, autoTrigger)
	}))
}

// autoRestartCycle hands trigger to the first cycle only. Any later cycle of
// the same coalesceRestart loop serves whoever coalesced into it meanwhile —
// possibly a user or mutation restart, which the breaker must never refuse.
func autoRestartCycle(trigger string, run func(autoTrigger string)) func() {
	first := true
	return func() {
		autoTrigger := ""
		if first {
			autoTrigger, first = trigger, false
		}
		run(autoTrigger)
	}
}

// autoRestartHalted reports whether the breaker refuses an automatic restart of
// workspaceID, tripping it once autoRestartLimit restarts were counted inside
// autoRestartWindow.
func autoRestartHalted(workspaceID string, now time.Time) bool {
	sv, _ := restartStates.LoadOrStore(workspaceID, &restartState{})
	state := sv.(*restartState)
	state.mu.Lock()
	defer state.mu.Unlock()

	recent := state.autoRestarts[:0]
	for _, at := range state.autoRestarts {
		if now.Sub(at) < autoRestartWindow {
			recent = append(recent, at)
		}
	}
	state.autoRestarts = recent
	if len(recent) >= autoRestartLimit {
		state.autoRestartsHalted = true
	}
	return state.autoRestartsHalted
}

// recordAutoRestart counts an automatic restart that has stopped the failed
// container. It never refuses: the container is already gone, and refusing
// now would leave the workspace with none.
func recordAutoRestart(workspaceID string, now time.Time) {
	sv, _ := restartStates.LoadOrStore(workspaceID, &restartState{})
	state := sv.(*restartState)
	state.mu.Lock()
	state.autoRestarts = append(state.autoRestarts, now)
	state.mu.Unlock()
}

// resetAutoRestarts puts a workspace back to how a manual restart behaved before
// any automatic one: the full automatic-restart budget, and no queue hold left
// over from the last automatic restart.
//
// Only deliberate operator recoveries call it: Restart, AdminRestart, Resume and
// the concierge repair button. Restarts the platform issues for other reasons —
// plugin reconcile on every transition to online, file and secret writes an
// agent can make, Wake on an incoming message — leave it alone: one of those on
// every boot would otherwise keep a crash loop from ever tripping it.
func resetAutoRestarts(workspaceID string) {
	sv, ok := restartStates.Load(workspaceID)
	if !ok {
		return
	}
	state := sv.(*restartState)
	state.mu.Lock()
	state.autoRestarts = nil
	state.autoRestartsHalted = false
	state.queueHeldSince = time.Time{}
	state.queueReadyHeartbeat = time.Time{}
	state.mu.Unlock()
}

// haltAutoRestarts is what a tripped breaker does instead of a restart: mark the
// workspace failed with the reason and tell the canvas.
func (h *WorkspaceHandler) haltAutoRestarts(workspaceID, trigger string) {
	log.Printf("Auto-restart: CRASH LOOP — not restarting %s (trigger=%s): %d automatic restarts within %s did not keep it up; it stays down until restarted manually (core#5241)",
		workspaceID, trigger, autoRestartLimit, autoRestartWindow)
	if db.DB == nil {
		return
	}
	msg := fmt.Sprintf("Automatic restarts stopped: %d automatic restarts within %s did not keep this workspace up (last trigger: %s). It will not be restarted automatically again until it is restarted manually.",
		autoRestartLimit, autoRestartWindow, trigger)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A removed, paused, hibernated or provisioning row keeps its state, and a
	// row that is already failed is not announced a second time.
	res, err := db.DB.ExecContext(ctx, `
		UPDATE workspaces
		   SET status = $2, last_sample_error = $3, updated_at = now()
		 WHERE id = $1
		   AND status NOT IN ('removed', 'provisioning', 'paused', 'hibernated', 'failed')`,
		workspaceID, models.StatusFailed, msg)
	if err != nil {
		log.Printf("Auto-restart: could not mark crash-looping %s failed: %v", workspaceID, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 || h.broadcaster == nil {
		return
	}
	// WORKSPACE_PROVISION_FAILED is the event the canvas turns into a failed
	// node showing this message; source tells it apart from a failed provision.
	if err := h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceProvisionFailed), workspaceID, map[string]interface{}{
		"error":   msg,
		"source":  "auto_restart_crash_loop",
		"trigger": trigger,
	}); err != nil {
		log.Printf("Auto-restart: crash-loop event for %s not recorded: %v", workspaceID, err)
	}
}

package handlers

// restart_queue_hold.go — keep queued A2A messages away from a container an
// automatic restart has just brought up until it has stayed ready for a while
// (core#5241).
//
// The drain used to hand every queued turn to a replacement container the
// moment it reported ready. A replacement that then died took those turns down
// with it, the next one got them again, and after a few rounds the settling
// ceiling dropped them. Holding the queue until the replacement has proven it
// stays up costs some latency after a restart and nothing else: the queue is
// durable, and a held turn is not dequeued, so neither its attempts nor its
// settling clock move.
//
// Only an automatic restart arms the hold (runRestartCycle, once the failed
// container has been stopped): that is the restart whose replacement has a
// reason to fail again. Its restart-context snapshot waits in the held queue
// too, instead of being pushed (sendRestartContext). Manual and mutation
// restarts, first boots and wakes — and every drain that is not inside a hold —
// keep the existing behaviour, settling ceiling included. A deliberate recovery
// also drops a hold an automatic restart left behind (resetAutoRestarts).

import (
	"context"
	"database/sql"
	"log"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
)

// restartQueueHoldGrace is how long a container a restart brought up must keep
// heartbeating as ready before the queue drains into it.
//
// 90s is longer than the CP instance reconciler's 60s pass, so at least one
// authoritative is-it-running check sees the new container first; and it is
// three heartbeat intervals (the runtime beats every 30s), so the hold ends on
// a beat sent a full grace after the container was first seen ready. Any longer
// only delays work a healthy restart could already be doing.
const restartQueueHoldGrace = 90 * time.Second

// armRestartQueueHold starts holding the queue of a workspace whose failed
// container an automatic restart has just stopped. A newer restart re-arms it
// from scratch.
func armRestartQueueHold(workspaceID string) {
	sv, _ := restartStates.LoadOrStore(workspaceID, &restartState{})
	state := sv.(*restartState)
	state.mu.Lock()
	state.queueHeldSince = time.Now()
	state.queueReadyHeartbeat = time.Time{}
	state.mu.Unlock()
}

// restartQueueHeld reports whether workspaceID's queue is held for the container
// its last automatic restart brought up.
func restartQueueHeld(workspaceID string) bool {
	sv, ok := restartStates.Load(workspaceID)
	if !ok {
		return false
	}
	state := sv.(*restartState)
	state.mu.Lock()
	defer state.mu.Unlock()
	return !state.queueHeldSince.IsZero()
}

// holdQueueAfterRestart reports whether the drain must leave workspaceID's
// queue alone because the container its last restart brought up has not yet
// stayed ready for restartQueueHoldGrace.
//
// Ready is online or degraded (the states the sweeper drains) with a heartbeat
// from after the restart, so the old container's last beats never count.
// Staying ready is measured between heartbeats, both stamped by the database:
// the hold ends on the first beat at least restartQueueHoldGrace after the
// first ready one, so a container that died meanwhile — its heartbeats stopped
// — never releases it. Leaving ready restarts the clock.
//
// Fails open: a status it cannot read does not hold the queue.
func holdQueueAfterRestart(ctx context.Context, workspaceID string) bool {
	sv, ok := restartStates.Load(workspaceID)
	if !ok {
		return false
	}
	state := sv.(*restartState)
	state.mu.Lock()
	heldSince := state.queueHeldSince
	state.mu.Unlock()
	if heldSince.IsZero() {
		return false
	}

	var status string
	var lastHeartbeat sql.NullTime
	if err := db.DB.QueryRowContext(ctx,
		`SELECT status, last_heartbeat_at FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&status, &lastHeartbeat); err != nil {
		log.Printf("A2AQueue drain: post-restart hold check for %s failed (%v) — draining without it", workspaceID, err)
		return false
	}
	ready := (status == string(models.StatusOnline) || status == string(models.StatusDegraded)) &&
		lastHeartbeat.Valid && lastHeartbeat.Time.After(heldSince)

	state.mu.Lock()
	defer state.mu.Unlock()
	switch {
	case state.queueHeldSince.IsZero():
		return false // a concurrent drain released it
	case !state.queueHeldSince.Equal(heldSince):
		return true // re-armed by a newer restart; this read describes the old container
	case !ready:
		state.queueReadyHeartbeat = time.Time{}
		return true
	case state.queueReadyHeartbeat.IsZero():
		state.queueReadyHeartbeat = lastHeartbeat.Time
		return true
	case lastHeartbeat.Time.Sub(state.queueReadyHeartbeat) < restartQueueHoldGrace:
		return true
	}
	state.queueHeldSince = time.Time{}
	state.queueReadyHeartbeat = time.Time{}
	return false
}

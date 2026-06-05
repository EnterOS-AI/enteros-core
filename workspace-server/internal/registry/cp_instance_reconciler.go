package registry

// cp_instance_reconciler.go — authoritative EC2-state reconcile for
// SaaS workspaces (core#2261).
//
// Root cause (core#2247): every existing liveness pass keys off a PROXY
// for "is this workspace alive?":
//
//   - StartLivenessMonitor   — Redis TTL expiry (agent stopped heartbeating).
//   - StartHealthSweep (Docker pass) — local Docker daemon (prov != nil only).
//   - StartHealthSweep (remote pass) — last_heartbeat_at freshness for
//     runtime='external' rows.
//   - StartCPOrphanSweeper    — status='removed' rows with a stray instance_id.
//
// A SaaS claude-code workspace whose EC2 was terminated/stopped out from
// under us (manual AWS action, spot reclaim, CP-side reap, etc.) falls
// through ALL of them: it's not 'removed' (so the orphan sweeper skips
// it), it's not runtime='external' (so the heartbeat pass skips it), and
// on a pure-SaaS front-door prov == nil so the Docker pass never runs.
// The registry kept status='online' pointing at a dead instance forever.
//
// This sweeper closes that gap with the ONE authoritative check the
// others lack: CPProvisioner.IsRunning, which ultimately asks the
// control-plane "is this EC2 actually running?" (DescribeInstances-
// equivalent). When the answer is a CLEAN "no" it feeds the workspace
// into the EXISTING offline/auto-heal machinery (onOffline → status flip
// + RestartByID reprovision with the existing volume) — no new healing
// path, just real ground truth driving the one we already have.
//
// Guardrails:
//   - FAIL-SAFE: IsRunning is (true, err) on any transient DB/transport
//     error and (false, nil) ONLY when CP genuinely reports the instance
//     is not running. We act ONLY on (false, nil); any err short-circuits
//     to "leave it alone" so a CP blip never flips a healthy workspace.
//   - ONLINE + SaaS ONLY: status='online', instance_id present, and
//     runtime <> 'external'. Paused/hibernated/removed/provisioning/
//     awaiting_agent rows are out of scope; external rows are covered by
//     the remote-heartbeat pass.
//   - Per-cycle row cap + per-workspace timeout so one slow CP call can't
//     stall the sweep.

import (
	"context"
	"log"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// InstanceRunningChecker is the narrow dependency the reconciler takes
// from the CP provisioner. *provisioner.CPProvisioner satisfies this
// naturally; tests inject fakes.
//
// Contract (load-bearing): IsRunning is FAIL-SAFE — it returns
// (true, err) on transient DB/transport errors and (false, nil) ONLY
// when CP reports the instance is genuinely not running. The reconciler
// flips a workspace offline strictly on (false, nil).
type InstanceRunningChecker interface {
	IsRunning(ctx context.Context, workspaceID string) (bool, error)
}

// CPInstanceReconcileLimit caps the per-cycle row count so a sustained
// CP slowdown can't make a single sweep cycle run unbounded. With a 60s
// cadence and a per-workspace timeout below, this bounds worst-case
// cycle wall-time and lets subsequent cycles drain any backlog.
const CPInstanceReconcileLimit = 200

// cpInstanceCheckTimeout bounds a single IsRunning call so one slow CP
// round-trip can't stall the whole sweep. Each workspace gets its own
// timeout context derived from the cycle context.
const cpInstanceCheckTimeout = 10 * time.Second

// StartCPInstanceReconciler runs the authoritative EC2-state reconcile
// loop until ctx is cancelled. A nil checker makes the loop a no-op
// (matches the nil-tolerant pattern of the sibling CP sweeper).
//
// Caller is expected to gate on `cpProv != nil` (matching how
// StartCPOrphanSweeper is gated at the wiring site in cmd/server/main.go)
// — passing a nil *CPProvisioner here would also short-circuit, but the
// gate at the call site keeps the call shape symmetric across sweepers.
//
// interval <= 0 falls back to the default 60s cadence so a misconfigured
// caller can't spin a zero-duration ticker (which panics).
func StartCPInstanceReconciler(ctx context.Context, checker InstanceRunningChecker, onOffline OfflineHandler, interval time.Duration) {
	if checker == nil {
		log.Println("cp-instance-reconciler: checker is nil — reconciler disabled")
		return
	}
	if interval <= 0 {
		interval = 60 * time.Second
	}
	log.Printf("cp-instance-reconciler started — reconciling online SaaS workspaces against real EC2 state every %s", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Kick once at boot so a platform restart starts healing immediately
	// rather than waiting a full interval.
	reconcileOnce(ctx, checker, onOffline)
	for {
		select {
		case <-ctx.Done():
			log.Println("cp-instance-reconciler: shutdown")
			return
		case <-ticker.C:
			reconcileOnce(ctx, checker, onOffline)
		}
	}
}

// reconcileOnce executes one reconcile pass. Defensive against db.DB
// being nil so a misconfigured boot doesn't panic.
//
// Scope: online + SaaS-EC2 workspaces only. runtime='external' rows are
// excluded (covered by the remote-heartbeat pass); paused/hibernated/
// removed/provisioning/awaiting_agent are excluded by the status filter.
func reconcileOnce(ctx context.Context, checker InstanceRunningChecker, onOffline OfflineHandler) {
	if db.DB == nil {
		return
	}

	rows, err := db.DB.QueryContext(ctx, `
		SELECT id::text
		  FROM workspaces
		 WHERE status = 'online'
		   AND instance_id IS NOT NULL
		   AND instance_id != ''
		   AND COALESCE(runtime, '') <> 'external'
		 ORDER BY updated_at DESC
		 LIMIT $1
	`, CPInstanceReconcileLimit)
	if err != nil {
		log.Printf("cp-instance-reconciler: DB query failed: %v", err)
		return
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			log.Printf("cp-instance-reconciler: row scan failed: %v", scanErr)
			continue
		}
		ids = append(ids, id)
	}
	if iterErr := rows.Err(); iterErr != nil {
		log.Printf("cp-instance-reconciler: rows iteration failed: %v", iterErr)
		return
	}

	for _, id := range ids {
		// Per-workspace timeout so one slow CP round-trip can't stall
		// the whole sweep.
		checkCtx, cancel := context.WithTimeout(ctx, cpInstanceCheckTimeout)
		running, checkErr := checker.IsRunning(checkCtx, id)
		cancel()

		if checkErr != nil {
			// FAIL-SAFE: transient DB/transport error (or a no-backend
			// signal). IsRunning returns (true, err) on these, so never
			// flip — leave the row online and retry next cycle.
			log.Printf("cp-instance-reconciler: IsRunning(%s) errored, leaving online (fail-safe): %v", id, checkErr)
			continue
		}
		if running {
			continue
		}

		// CLEAN "not running" — CP authoritatively reports the EC2 is
		// terminated/stopped/absent. Feed it into the existing offline +
		// auto-heal machinery: onOffline flips the row offline and
		// triggers RestartByID, which reprovisions with the existing
		// volume.
		log.Printf("cp-instance-reconciler: workspace %s is status=online but its EC2 is not running (terminated/stopped) — flipping offline + triggering reprovision", id)
		if onOffline != nil {
			onOffline(ctx, id)
		}
	}
}

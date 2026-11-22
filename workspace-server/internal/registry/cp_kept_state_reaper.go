package registry

// cp_kept_state_reaper.go — erases the state a plain delete kept, once the
// retention window has passed (molecule-core#5193).
//
// A delete without erase_data / purge stops the workspace KEEP-STATE and marks
// the row status='removed'. Keeping the data is deliberate — it is what lets a
// removed concierge be revived in place with its memory
// (handlers/platform_agent_ensure.go), and it is the "undo" window of a delete
// the user did not ask to erase — but it was never bounded on the control
// plane's Kubernetes substrate. On AWS the control plane's orphan sweep reaps a
// plain delete's data volume after 24 h; on Kubernetes nothing did, so every
// plain delete left its identity claim (plus the config seed Secret and the
// Service) behind for good. Each claim counts against the tenant namespace's
// persistentvolumeclaims quota, and a tenant that recreated workspaces a dozen
// times (prod minori, 2026-08-15: 9 orphaned claims, quota 12/12) could
// provision nothing at all.
//
// This loop bounds it the way AWS does. The delete paths keep the stopped
// instance's id in kept_state_instance_id (db.ClearInstanceIDKeepingStateSQL);
// once a removed row has kept it for KeptStateRetention, the loop asks the
// control plane to erase the workspace through the ordinary teardown with
// prune=true and that instance id. The control plane's ledger already records
// that instance as torn down, so it does not touch compute again: it erases
// what the workspace still owns in the org's namespace (EraseWorkspaceState:
// Deployment, pods, Service, seed Secret, identity claims) and answers 200.
//
// Safety:
//   - Only status='removed' rows, re-checked under a row lock (FOR UPDATE SKIP
//     LOCKED) that is held across the control-plane call. A concierge revived
//     in place flips its row out of 'removed' with an UPDATE that waits for
//     that lock, so the reaper either erases before the revive (the revived
//     concierge then provisions fresh) or sees the row no longer removed and
//     skips it. It can never erase a workspace that is live again.
//   - The instance id must still equal the one the candidate query read: a
//     row that was revived, deleted again and given a newer kept id is left
//     for the next cycle, with its own clock.
//   - A failed erase keeps the id and moves kept_state_since forward by
//     keptStateEraseBackoff, so a control plane that keeps refusing is asked
//     again an hour later, not every minute (the 2026-07-30 orphan-sweeper
//     retry storm is the shape this avoids).
//
// Rows deleted before this column existed have no kept id and are not
// reachable from here; their leftover claims need a one-off cleanup
// (molecule-core#5193).

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// KeptStateEraser erases the state a keep-state teardown left behind, through
// the control plane's teardown for the named (already stopped) instance.
// *provisioner.CPProvisioner implements it; tests inject fakes.
type KeptStateEraser interface {
	EraseKeptState(ctx context.Context, workspaceID, instanceID string) error
}

// KeptStateRetention is how long a plain delete keeps a workspace's state
// before it is erased: the platform's volume grace (rescue.RescueVolumeGrace,
// the 24 h the control plane's AWS orphan sweep also gives a plain delete's
// data volume).
const KeptStateRetention = 24 * time.Hour

// keptStateEraseBackoff is how far a failed erase pushes the row back.
const keptStateEraseBackoff = time.Hour

// keptStateReapLimit caps one cycle. Each erase is one control-plane call that
// lists and deletes a handful of objects; 20 a minute drains any realistic
// backlog within the hour.
const keptStateReapLimit = 20

// keptStateEraseTimeout bounds one erase (the control plane's own erase budget
// is 60 s).
const keptStateEraseTimeout = 75 * time.Second

// StartCPKeptStateReaper runs the loop until ctx is cancelled. A nil eraser
// disables it (no control plane wired).
func StartCPKeptStateReaper(ctx context.Context, eraser KeptStateEraser) {
	if eraser == nil {
		log.Println("CP kept-state reaper: eraser is nil — reaper disabled")
		return
	}
	log.Printf("CP kept-state reaper started — erasing state kept longer than %s, every %s", KeptStateRetention, OrphanSweepInterval)
	ticker := time.NewTicker(OrphanSweepInterval)
	defer ticker.Stop()
	cpKeptStateReapOnce(ctx, eraser)
	for {
		select {
		case <-ctx.Done():
			log.Println("CP kept-state reaper: shutdown")
			return
		case <-ticker.C:
			cpKeptStateReapOnce(ctx, eraser)
		}
	}
}

type keptStateCandidate struct{ id, instanceID string }

// cpKeptStateReapOnce runs one pass.
func cpKeptStateReapOnce(ctx context.Context, eraser KeptStateEraser) {
	if db.DB == nil || eraser == nil {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, orphanSweepDeadline)
	rows, err := db.DB.QueryContext(qctx, `
		SELECT id::text, kept_state_instance_id
		  FROM workspaces
		 WHERE status = 'removed'
		   AND kept_state_instance_id IS NOT NULL
		   AND kept_state_instance_id != ''
		   AND kept_state_since < now() - make_interval(secs => $2)
		 ORDER BY kept_state_since
		 LIMIT $1
	`, keptStateReapLimit, KeptStateRetention.Seconds())
	if err != nil {
		cancel()
		log.Printf("CP kept-state reaper: DB query failed: %v", err)
		return
	}
	var candidates []keptStateCandidate
	for rows.Next() {
		var c keptStateCandidate
		if scanErr := rows.Scan(&c.id, &c.instanceID); scanErr != nil {
			log.Printf("CP kept-state reaper: row scan failed: %v", scanErr)
			continue
		}
		candidates = append(candidates, c)
	}
	iterErr := rows.Err()
	rows.Close()
	cancel()
	if iterErr != nil {
		log.Printf("CP kept-state reaper: rows iteration failed: %v", iterErr)
		return
	}
	for _, c := range candidates {
		if ctx.Err() != nil {
			return
		}
		reapKeptState(ctx, eraser, c)
	}
}

// reapKeptState erases one row's kept state under its row lock.
func reapKeptState(parent context.Context, eraser KeptStateEraser, c keptStateCandidate) {
	ctx, cancel := context.WithTimeout(parent, keptStateEraseTimeout+10*time.Second)
	defer cancel()
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("CP kept-state reaper: begin for %s failed: %v", c.id, err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var locked string
	err = tx.QueryRowContext(ctx, `
		SELECT kept_state_instance_id
		  FROM workspaces
		 WHERE id = $1
		   AND status = 'removed'
		   AND kept_state_instance_id = $2
		 FOR UPDATE SKIP LOCKED
	`, c.id, c.instanceID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		// Revived, re-deleted with a newer id, erased by another replica, or
		// locked by one right now: not ours this cycle.
		return
	}
	if err != nil {
		log.Printf("CP kept-state reaper: lock %s failed: %v", c.id, err)
		return
	}

	ectx, ecancel := context.WithTimeout(ctx, keptStateEraseTimeout)
	eraseErr := eraser.EraseKeptState(ectx, c.id, c.instanceID)
	ecancel()
	if eraseErr != nil {
		log.Printf("CP kept-state reaper: erasing the kept state of removed workspace %s (instance %s) failed: %v — retrying in %s",
			c.id, c.instanceID, eraseErr, keptStateEraseBackoff)
		if _, err := tx.ExecContext(ctx,
			`UPDATE workspaces SET kept_state_since = kept_state_since + make_interval(secs => $2) WHERE id = $1`,
			c.id, keptStateEraseBackoff.Seconds()); err != nil {
			log.Printf("CP kept-state reaper: backoff write for %s failed: %v", c.id, err)
			return
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE workspaces SET kept_state_instance_id = NULL, kept_state_since = NULL WHERE id = $1`,
			c.id); err != nil {
			// The erase is idempotent: the next cycle repeats it and clears.
			log.Printf("CP kept-state reaper: clearing the kept state of %s failed: %v", c.id, err)
			return
		}
		log.Printf("CP kept-state reaper: erased the kept state of removed workspace %s (instance %s)", c.id, c.instanceID)
	}
	if err := tx.Commit(); err != nil {
		log.Printf("CP kept-state reaper: commit for %s failed: %v", c.id, err)
		return
	}
	committed = true
}

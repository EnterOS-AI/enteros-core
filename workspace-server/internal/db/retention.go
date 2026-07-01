package db

import (
	"context"
	"fmt"
)

// PruneActivityLogs deletes stale activity_logs rows using the acked-AND-age
// predicate introduced by MUST-FIX 3, with a hard time ceiling as a backstop.
//
// A row is deleted iff:
//
//	(created_at older than softDays  AND  its seq has been acked by the
//	 workspace's inbox poller — seq <= last_acked_seq)
//	OR
//	(created_at older than hardDays)
//
// where last_acked_seq is read from inbox_delivery_state (defaulting to 0 —
// "nothing acked" — for a workspace with no cursor row yet, via COALESCE).
//
// STRICTLY MORE CONSERVATIVE than the pre-ack age-only prune
// (`DELETE ... WHERE created_at < now() - softDays`). Proof that the new
// predicate never deletes a row the old one would have kept — i.e. the deleted
// set is a subset of the old deleted set:
//
//   - Soft branch: `created_at < now()-soft AND seq <= acked` is a strict
//     subset of the old predicate `created_at < now()-soft`.
//   - Hard branch: with hardDays >= softDays we have now()-hard <= now()-soft,
//     so `created_at < now()-hard` implies `created_at < now()-soft`; the hard
//     branch is also a subset of the old predicate.
//
// The union of two subsets of the old predicate is itself a subset — so every
// row this deletes, the old prune deleted too; and any old row that is old but
// UNACKED and younger than hardDays is now retained. Never prunes earlier.
//
// The hardDays >= softDays invariant is REQUIRED for the above proof, so we
// clamp it up here rather than trusting the caller/env — a misconfigured
// hard < soft would otherwise reintroduce the "prune an unacked row early"
// behaviour this fix removes.
func PruneActivityLogs(ctx context.Context, softDays, hardDays int) (int64, error) {
	if softDays <= 0 {
		softDays = 7
	}
	if hardDays < softDays {
		hardDays = softDays
	}
	res, err := DB.ExecContext(ctx, `
		DELETE FROM activity_logs a
		WHERE (
		        a.created_at < now() - make_interval(days => $1::int)
		    AND a.seq <= COALESCE(
		            (SELECT s.last_acked_seq
		               FROM inbox_delivery_state s
		              WHERE s.workspace_id = a.workspace_id),
		            0)
		      )
		   OR a.created_at < now() - make_interval(days => $2::int)
	`, softDays, hardDays)
	if err != nil {
		return 0, fmt.Errorf("prune activity_logs: %w", err)
	}
	return res.RowsAffected()
}

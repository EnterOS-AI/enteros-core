package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/metrics"
)

// DegradedPluginSweepInterval is how often the degraded-install gauge is
// refreshed. Deliberately unhurried: this is a fleet-health reading, not a
// control loop, and the condition it reports persists across boots (core#4997
// went unnoticed for five days, not five minutes).
const DegradedPluginSweepInterval = 5 * time.Minute

// degradedCountQuery builds the count query FROM the shared relation AND the
// shared predicate rather than restating either.
//
// admin_plugin_install_reports.go already flags the SQL/Go duplication of this
// rule as a drift risk and pins the two to each other with a test. A third,
// hand-written copy here is precisely how the alert would keep reporting 0
// after the rule changed — a guard that reports success while covering nothing.
//
// core#5025 finding 8: reusing the PREDICATE was not enough, because the two
// statements also disagreed about the RELATION. This query read
// workspace_plugin_install_reports on its own while the contract endpoint
// evaluated the same predicate over reports joined to workspaces, and deletes
// here are soft — so a removed workspace's report row kept satisfying the
// predicate and the gauge could never return to zero. Both readers now derive
// from degradedFleetRelation, so there is one definition of "in scope" to
// disagree about instead of two.
func degradedCountQuery() string {
	return fmt.Sprintf(
		`SELECT count(*) FROM %s WHERE %s`,
		degradedFleetRelation,
		degradedFleetPredicate,
	)
}

// sweepDegradedPluginInstallsOnce measures the fleet and publishes the gauge.
//
// Read-only by construction: it counts rows and never touches a workspace. On
// a query error it returns the error and leaves the previous reading in place —
// publishing 0 for a failed measurement would report a healthy fleet we did not
// observe, which is the same confident-lie shape as the bug this exists to catch.
func sweepDegradedPluginInstallsOnce(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("degraded plugin sweep: nil db")
	}
	var n int64
	if err := db.QueryRowContext(ctx, degradedCountQuery()).Scan(&n); err != nil {
		return fmt.Errorf("degraded plugin sweep: %w", err)
	}
	metrics.SetDegradedPluginWorkspaces(n)
	if n > 0 {
		// Logged as well as gauged: the metric answers "how many", the log is
		// what an operator greps when the alert fires. The workspace ids and
		// failing sources are already available at
		// GET /admin/plugin-install-reports?status=degraded.
		log.Printf("Degraded plugin installs: %d live workspace(s) have a declared plugin that failed to install "+
			"(see GET /admin/plugin-install-reports?status=degraded)", n)
	}
	return nil
}

// StartDegradedPluginSweeper refreshes the degraded-install gauge on a ticker.
// Mirrors StartPluginDriftSweeper's shape: one immediate pass so a restart does
// not blind the dashboard for a full interval, then tick until ctx is done.
func StartDegradedPluginSweeper(ctx context.Context, db *sql.DB) {
	if db == nil {
		log.Println("Degraded plugin sweeper: nil db — sweeper disabled")
		return
	}
	log.Printf("Degraded plugin sweeper started — interval %s", DegradedPluginSweepInterval)
	ticker := time.NewTicker(DegradedPluginSweepInterval)
	defer ticker.Stop()

	if err := sweepDegradedPluginInstallsOnce(ctx, db); err != nil {
		log.Printf("Degraded plugin sweeper: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			log.Println("Degraded plugin sweeper: shutdown")
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			if err := sweepDegradedPluginInstallsOnce(ctx, db); err != nil {
				log.Printf("Degraded plugin sweeper: %v", err)
			}
		}
	}
}

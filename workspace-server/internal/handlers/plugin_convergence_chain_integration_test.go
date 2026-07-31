//go:build integration
// +build integration

// plugin_convergence_chain_integration_test.go — the CHAIN gate for core#4977.
//
// Run with:
//
//	INTEGRATION_DB_URL="postgres://postgres:test@127.0.0.1:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run TestIntegration_ConvergenceChain -v
//
// Use 127.0.0.1, NOT localhost, when the DB is a `-p` published container on
// Docker Desktop for Windows. localhost resolves to ::1 first, and the IPv6
// loopback proxy drops fresh connections: measured 7 failures in 38 runs, every
// one a 30s `Ping` stall ending in "wsarecv: An existing connection was forcibly
// closed", versus 0 in 40 runs on 127.0.0.1. It reads exactly like a flaky test
// and is not one. CI is unaffected — it reaches postgres by container hostname
// over the molecule-core-net bridge, so no port proxy is involved.
//
// WHY THIS EXISTS, AND WHY IT IS DIFFERENT FROM THE OTHER GATES
//
// core#4977 was FOUR sequential defects in one pipeline:
//
//	 1. nothing was tracked          -> the eligibility SELECT matched zero rows
//	 2. nothing consumed the queue   -> detected drift was never applied
//	 3. convergence could be faked   -> installed_sha advanced when nothing landed
//	 4. nothing could be enqueued    -> ON CONFLICT could not infer a PARTIAL index
//
// Each was fixed with its own unit + integration coverage, and each time the
// symptom survived, because every gate tested a LINK and none tested the CHAIN.
// Defect 4 in particular passed everything: it compiles, it vets, and sqlmock
// returns a canned OK for a statement Postgres rejects outright. It was found by
// hand on a live tenant, after shipping, by a customer.
//
// This test walks the whole state machine against REAL Postgres with only the
// network/Docker edges stubbed.
//
// MUTATION SCORE — 4/4 killed, each at its own link (measured, not asserted):
//
//	defect 1  trackFromSource returns "none"          -> link 1 (detect)
//	defect 4  ON CONFLICT drops WHERE status='pending' -> link 2 (enqueue),
//	          with the real pq error: "there is no unique or exclusion
//	          constraint matching the ON CONFLICT specification"
//	defect 2  driftApplyMaxPerTick = 0                -> link 3 (drain)
//	defect 3  Materialized forced true                -> deferred arm, which
//	          catches the re-pin that fakes convergence
//
// Re-run that matrix if you restructure this file: a chain gate that survives
// its own defects is decoration. Note link 1 derives tracked_ref via
// trackFromSource rather than seeding the literal — seeding it by hand would
// hard-code the fix and let defect 1 back in with this test still green.
//
// NOT SAFE FOR t.Parallel() — seeds shared tables and rebinds db.DB.

package handlers

import (
	"context"
	"database/sql"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/plugins"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

const (
	chainOldSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	chainNewSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	chainSource = "gitea://molecule-ai/probe-plugin#main"
)

func integrationDB_Chain(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("postgres", requireIntegrationDBURL(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	clear := func() {
		if _, err := conn.ExecContext(context.Background(),
			`DELETE FROM workspaces WHERE name LIKE 'itest-chain-%';`); err != nil {
			t.Fatalf("clear: %v", err)
		}
	}
	clear()
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() { db.DB = prev; clear(); conn.Close() })
	return conn
}

// eligibleFor reports whether the sweeper's OWN selection query returns this
// plugin, and the installed SHA it would compare against upstream.
func eligibleFor(t *testing.T, conn *sql.DB, wsID, plugin string) (bool, string) {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(), plugins.DriftEligibleQuery)
	if err != nil {
		t.Fatalf("DriftEligibleQuery: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, ws, name, src, ref, sha string
		if err := rows.Scan(&id, &ws, &name, &src, &ref, &sha); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if ws == wsID && name == plugin {
			return true, sha
		}
	}
	return false, ""
}

// TestIntegration_ConvergenceChain_DetectQueueApplyConverge walks every link
// with real SQL: eligibility -> enqueue -> drain -> apply -> re-pin -> quiet.
//
// This is the gate that would have caught the ON CONFLICT defect at
// introduction. Link-level gates did not, because each link passed in isolation.
func TestIntegration_ConvergenceChain_DetectQueueApplyConverge(t *testing.T) {
	conn := integrationDB_Chain(t)
	ctx := context.Background()

	wsID := uuid.NewString()
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO workspaces (id, name, status, kind) VALUES ($1,$2,'online','workspace')`,
		wsID, "itest-chain-"+wsID[:8]); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	// tracked_ref is DERIVED, never a literal: defect 1 was that the install
	// path recorded 'none' for every branch pin, so the sweeper's `!= 'none'`
	// filter matched nothing. Seeding the string by hand would hard-code the
	// fix and let that exact regression back in with the chain still green.
	trackedRef := trackFromSource(chainSource)
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO workspace_plugins (workspace_id, plugin_name, source_raw, tracked_ref, installed_sha)
		 VALUES ($1,'probe-plugin',$2,$3,$4)`,
		wsID, chainSource, trackedRef, chainOldSHA); err != nil {
		t.Fatalf("seed plugin: %v", err)
	}

	// LINK 1 — DETECT. Defect 1 (everything recorded as tracked_ref='none')
	// made this select nothing at all.
	ok, installed := eligibleFor(t, conn, wsID, "probe-plugin")
	if !ok {
		t.Fatal("link 1 (detect): the sweeper's eligibility query does not select a " +
			"tracked, branch-pinned plugin — nothing downstream can ever run")
	}
	if installed != chainOldSHA {
		t.Fatalf("link 1: installed_sha = %q, want %q", installed, chainOldSHA)
	}

	// LINK 2 — ENQUEUE. Defect 4 lived here: ON CONFLICT could not infer the
	// PARTIAL unique index, so every insert was rejected at runtime and the
	// queue stayed permanently empty while drift was logged every sweep.
	if err := plugins.QueueDriftEntryForTest(ctx, wsID, "probe-plugin", trackedRef,
		chainOldSHA, chainNewSHA); err != nil {
		t.Fatalf("link 2 (enqueue): %v", err)
	}
	var pending int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM plugin_update_queue WHERE workspace_id=$1 AND status='pending'`,
		wsID).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 1 {
		t.Fatalf("link 2 (enqueue): pending rows = %d, want 1 — detected drift never "+
			"reached the queue", pending)
	}

	// LINK 3 + 4 — DRAIN and APPLY. Defect 2 was that nothing drained this
	// queue; defect 3 was advancing installed_sha when nothing materialised.
	stubDriftStaging(t, "probe-plugin", chainNewSHA, false /* docker-less: restart is the delivery */)
	spy := &restartSpy{}
	h := NewAdminPluginDriftHandler(NewPluginsHandler(t.TempDir(), nil, spy.fn))

	applied, failed := DrainPendingDrift(ctx, h.applyQueuedDrift)
	waitGlobalAsyncForTest()
	if applied != 1 || failed != 0 {
		t.Fatalf("link 3 (drain): applied=%d failed=%d, want 1/0 — the queue is not "+
			"being consumed", applied, failed)
	}

	// The workspace must actually be restarted: on a docker-less tenant that IS
	// the delivery mechanism.
	if calls := spy.snapshot(); len(calls) != 1 || calls[0] != wsID {
		t.Errorf("link 4 (deliver): restart dispatches = %v, want [%s]", calls, wsID)
	}

	// LINK 5 — RE-PIN. The new SHA is recorded only because the bytes landed.
	var got string
	if err := conn.QueryRowContext(ctx,
		`SELECT installed_sha FROM workspace_plugins WHERE workspace_id=$1 AND plugin_name='probe-plugin'`,
		wsID).Scan(&got); err != nil {
		t.Fatalf("read installed_sha: %v", err)
	}
	if got != chainNewSHA {
		t.Fatalf("link 5 (re-pin): installed_sha = %q, want %q — the workspace did "+
			"not converge", got, chainNewSHA)
	}

	// The queue entry is retired, so the drainer does not re-fetch it forever.
	var status string
	if err := conn.QueryRowContext(ctx,
		`SELECT status FROM plugin_update_queue WHERE workspace_id=$1`, wsID).Scan(&status); err != nil {
		t.Fatalf("read queue status: %v", err)
	}
	if status != "applied" {
		t.Errorf("queue status = %q, want %q", status, "applied")
	}

	// LINK 6 — QUIET. Converged means the sweeper no longer sees a difference.
	// Without this a "fix" that re-queues forever would pass everything above.
	_, nowInstalled := eligibleFor(t, conn, wsID, "probe-plugin")
	if nowInstalled != chainNewSHA {
		t.Errorf("link 6 (quiet): sweeper still sees installed=%q vs upstream %q — "+
			"it would re-queue and restart this workspace on every sweep",
			nowInstalled, chainNewSHA)
	}
}

// TestIntegration_ConvergenceChain_DeferredRestartKeepsDriftVisible is the
// chain's negative arm: when delivery does NOT land, the chain must stay noisy
// rather than record a convergence that did not happen (defect 3).
func TestIntegration_ConvergenceChain_DeferredRestartKeepsDriftVisible(t *testing.T) {
	conn := integrationDB_Chain(t)
	ctx := context.Background()

	wsID := uuid.NewString()
	// kind=platform + online => the self-brick guard defers the restart.
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO workspaces (id, name, status, kind) VALUES ($1,$2,'online','platform')`,
		wsID, "itest-chain-"+wsID[:8]); err != nil {
		t.Skipf("cannot seed a platform workspace (one may already exist): %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO workspace_plugins (workspace_id, plugin_name, source_raw, tracked_ref, installed_sha)
		 VALUES ($1,'probe-plugin',$2,'ref:main',$3)`,
		wsID, chainSource, chainOldSHA); err != nil {
		t.Fatalf("seed plugin: %v", err)
	}
	if err := plugins.QueueDriftEntryForTest(ctx, wsID, "probe-plugin", "ref:main",
		chainOldSHA, chainNewSHA); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	stubDriftStaging(t, "probe-plugin", chainNewSHA, false)
	spy := &restartSpy{}
	h := NewAdminPluginDriftHandler(NewPluginsHandler(t.TempDir(), nil, spy.fn))
	DrainPendingDrift(ctx, h.applyQueuedDrift)
	waitGlobalAsyncForTest()

	if calls := spy.snapshot(); len(calls) != 0 {
		t.Errorf("restart dispatched for a platform concierge (%v) — self-brick guard "+
			"should defer it", calls)
	}
	var got string
	if err := conn.QueryRowContext(ctx,
		`SELECT installed_sha FROM workspace_plugins WHERE workspace_id=$1 AND plugin_name='probe-plugin'`,
		wsID).Scan(&got); err != nil {
		t.Fatalf("read installed_sha: %v", err)
	}
	if got != chainOldSHA {
		t.Fatalf("installed_sha = %q, want the OLD %q — nothing reached the box, so "+
			"recording the new SHA would hide the staleness from every future sweep",
			got, chainOldSHA)
	}
	if _, nowInstalled := eligibleFor(t, conn, wsID, "probe-plugin"); nowInstalled != chainOldSHA {
		t.Errorf("sweeper no longer reports the stale SHA — drift became invisible")
	}
}

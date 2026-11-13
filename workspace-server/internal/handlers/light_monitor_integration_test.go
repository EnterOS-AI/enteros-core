//go:build integration
// +build integration

// light_monitor_integration_test.go — real-Postgres coverage of the SQL the
// Enter OS Light park and monitor rely on: the auto-park and refresh
// candidate predicates, the run-token rotation, the stored schedule set and
// its suspensions, the failure streak's order, the job-derived ledger rows and
// retention. sqlmock checks statement shape only; these check semantics.
//
// Run as light_schema_integration_test.go says. NOT SAFE FOR t.Parallel().

package handlers

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/wsauth"
)

func lightExec(t *testing.T, conn *sql.DB, query string, args ...interface{}) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func lightIDSet(rows [][]string) map[string]string {
	out := map[string]string{}
	for _, r := range rows {
		v := ""
		if len(r) > 1 {
			v = r[1]
		}
		out[r[0]] = v
	}
	return out
}

func TestIntegration_LightMonitor_AutoParkPredicate(t *testing.T) {
	conn := integrationDB_Light(t)
	idle := seedLightWorkspace(t, conn, "test-light-idle", "online", "open")
	busy := seedLightWorkspace(t, conn, "test-light-busy", "online", "open")
	recent := seedLightWorkspace(t, conn, "test-light-recent", "online", "open")
	refresh := seedLightWorkspace(t, conn, "test-light-refresh", "online", "open")
	failed := seedLightWorkspace(t, conn, "test-light-failed", "online", "open")
	parked := seedLightWorkspace(t, conn, "test-light-parked", "hibernated", "parked")
	ordinary := seedLightWorkspace(t, conn, "test-light-ordinary", "online", "")
	old := `UPDATE workspaces SET light_state_at = now() - interval '45 minutes', last_activity_at = now() - interval '40 minutes' WHERE id = ANY($1::uuid[])`
	for _, id := range []string{idle, busy, recent, failed, parked, ordinary} {
		lightExec(t, conn, old, "{"+id+"}")
	}
	lightExec(t, conn, `UPDATE workspaces SET active_tasks = 1 WHERE id = $1`, busy)
	lightExec(t, conn, `UPDATE workspaces SET last_activity_at = now() - interval '5 minutes' WHERE id = $1`, recent)
	lightExec(t, conn, `UPDATE workspaces SET light_opened_by = 'refresh' WHERE id = $1`, refresh)
	lightExec(t, conn, `UPDATE workspaces SET light_last_error = '{"code":"X","message":"y"}', light_last_error_at = now() - interval '1 minute' WHERE id = $1`, failed)

	rows, err := lightAutoParkCandidates(context.Background(), 30*time.Minute)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	got := lightIDSet(rows)
	if got[idle] != "owner" || got[refresh] != "refresh" {
		t.Errorf("want idle (owner) and refresh-opened (refresh); got %v", got)
	}
	for name, id := range map[string]string{"busy": busy, "recent activity": recent, "recent failure": failed, "parked": parked, "not Light": ordinary} {
		if _, ok := got[id]; ok {
			t.Errorf("%s agent must not be auto-parked", name)
		}
	}
}

func TestIntegration_LightMonitor_RefreshCandidates(t *testing.T) {
	conn := integrationDB_Light(t)
	pending := seedLightWorkspace(t, conn, "test-light-pending", "hibernated", "parked")
	unknown := seedLightWorkspace(t, conn, "test-light-unknown", "hibernated", "parked")
	applied := seedLightWorkspace(t, conn, "test-light-applied", "hibernated", "parked")
	open := seedLightWorkspace(t, conn, "test-light-open", "online", "open")
	lightExec(t, conn, `INSERT INTO light_pending_actions (workspace_id, kind, source) VALUES ($1, 'refresh', 'secret')`, pending)
	lightExec(t, conn, `INSERT INTO light_pending_actions (workspace_id, kind, source) VALUES ($1, 'future_kind', 'owner')`, unknown)
	lightExec(t, conn, `INSERT INTO light_pending_actions (workspace_id, kind, source, applied_at) VALUES ($1, 'refresh', 'secret', now())`, applied)
	lightExec(t, conn, `INSERT INTO light_pending_actions (workspace_id, kind, source) VALUES ($1, 'refresh', 'secret')`, open)

	rows, err := lightRefreshCandidates(context.Background(), lightKnownActionKinds(), 10)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	got := lightIDSet(rows)
	if _, ok := got[pending]; !ok {
		t.Errorf("a parked agent with a queued refresh must be opened: %v", got)
	}
	for name, id := range map[string]string{"unknown kind": unknown, "applied": applied, "open": open} {
		if _, ok := got[id]; ok {
			t.Errorf("%s must not be refreshed", name)
		}
	}

	// The park's watermark covers exactly the refreshes queued before it.
	var watermark = lightRefreshWatermark(context.Background(), pending)
	lightExec(t, conn, `INSERT INTO light_pending_actions (workspace_id, kind, source) VALUES ($1, 'refresh', 'plugin')`, pending)
	markLightRefreshApplied(context.Background(), pending, watermark)
	var left int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM light_pending_actions WHERE workspace_id = $1 AND applied_at IS NULL`, pending).Scan(&left); err != nil || left != 1 {
		t.Errorf("unapplied after the park = %d (%v), want only the refresh queued after the watermark", left, err)
	}
}

func lightTokenRevoked(t *testing.T, conn *sql.DB, id string) bool {
	t.Helper()
	var revoked bool
	if err := conn.QueryRow(`SELECT revoked_at IS NOT NULL FROM workspace_auth_tokens WHERE id = $1`, id).Scan(&revoked); err != nil {
		t.Fatalf("token %s: %v", id, err)
	}
	return revoked
}

func TestIntegration_LightRunToken_RetireThenRevoke(t *testing.T) {
	conn := integrationDB_Light(t)
	ctx := context.Background()
	ws := seedLightWorkspace(t, conn, "test-light-tokens", "online", "open")
	older, _, err := wsauth.IssueAPITokenWithID(ctx, conn, ws)
	if err != nil {
		t.Fatal(err)
	}
	current, _, err := wsauth.IssueAPITokenWithID(ctx, conn, ws)
	if err != nil {
		t.Fatal(err)
	}
	lightExec(t, conn, `UPDATE workspaces SET light_run_token_id = $2, light_retired_token_id = $3, light_state = 'parking', light_state_at = now() + interval '1 second' WHERE id = $1`, ws, current, older)

	// A park from open: current retires, the older one is revoked.
	first, _, err := wsauth.IssueAPITokenWithID(ctx, conn, ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitLightRunToken(ctx, ws, first); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var cur, ret string
	if err := conn.QueryRow(`SELECT light_run_token_id, light_retired_token_id FROM workspaces WHERE id = $1`, ws).Scan(&cur, &ret); err != nil {
		t.Fatal(err)
	}
	if cur != first || ret != current || !lightTokenRevoked(t, conn, older) || lightTokenRevoked(t, conn, current) {
		t.Fatalf("after a park: cur=%s ret=%s; want cur=new, ret=previous, the older one revoked", cur, ret)
	}

	// The same park continued: the token it minted (after light_state_at) was
	// never used; it is revoked and the retired one kept.
	lightExec(t, conn, `UPDATE workspaces SET light_state_at = now() - interval '1 minute' WHERE id = $1`, ws)
	second, _, err := wsauth.IssueAPITokenWithID(ctx, conn, ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitLightRunToken(ctx, ws, second); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := conn.QueryRow(`SELECT light_run_token_id, light_retired_token_id FROM workspaces WHERE id = $1`, ws).Scan(&cur, &ret); err != nil {
		t.Fatal(err)
	}
	if cur != second || ret != current || !lightTokenRevoked(t, conn, first) || lightTokenRevoked(t, conn, current) {
		t.Fatalf("continued park: cur=%s ret=%s; want cur=second, ret unchanged, the unused first revoked", cur, ret)
	}

	if !revokeRetiredLightToken(ctx, ws, current) || !lightTokenRevoked(t, conn, current) {
		t.Fatal("the retired token must be revoked once no run holds it")
	}
}

func TestIntegration_LightSchedules_StoreAndSuspensions(t *testing.T) {
	conn := integrationDB_Light(t)
	ctx := context.Background()
	ws := seedLightWorkspace(t, conn, "test-light-sched", "online", "parking")
	lightExec(t, conn, `INSERT INTO light_schedules (workspace_id, name, cron, prompt, suspended_reason, suspended_at) VALUES
		($1, 'kept', '0 9 * * *', 'p', 'r', now()), ($1, 'edited', '0 9 * * *', 'old', 'r', now()), ($1, 'gone', '0 9 * * *', 'p', NULL, NULL)`, ws)
	on := true
	rendered := []lightRendered{
		{Schedule: provisioner.LightSchedule{Name: "kept", Cron: "0 9 * * *", Prompt: "p", Enabled: &on}, GridEnabled: true, KeptSuspended: true},
		{Schedule: provisioner.LightSchedule{Name: "edited", Cron: "0 9 * * *", Prompt: "new", Enabled: &on}, GridEnabled: true},
	}
	put := provisioner.LightPutResult{Schedules: []provisioner.LightScheduleState{{Name: "kept", CronJob: "cj-kept"}, {Name: "edited", CronJob: "cj-edited"}}}
	if err := storeLightSchedules(ctx, ws, rendered, put, lightTriggerAuto); err != nil {
		t.Fatalf("store: %v", err)
	}
	reasons := map[string]sql.NullString{}
	rows, err := conn.Query(`SELECT name, suspended_reason FROM light_schedules WHERE workspace_id = $1`, ws)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		var r sql.NullString
		_ = rows.Scan(&n, &r)
		reasons[n] = r
	}
	rows.Close()
	if _, ok := reasons["gone"]; ok || len(reasons) != 2 {
		t.Errorf("a schedule missing from the park must be dropped: %v", reasons)
	}
	if !reasons["kept"].Valid || reasons["edited"].Valid {
		t.Errorf("auto park: kept stays suspended, an edited definition clears it: %v", reasons)
	}
	if err := storeLightSchedules(ctx, ws, rendered, put, lightTriggerOwner); err != nil {
		t.Fatalf("store: %v", err)
	}
	var stillSuspended int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM light_schedules WHERE workspace_id = $1 AND suspended_reason IS NOT NULL`, ws).Scan(&stillSuspended); err != nil || stillSuspended != 0 {
		t.Errorf("an owner park clears every suspension: %d left (%v)", stillSuspended, err)
	}
}

func TestIntegration_LightStreak_OrderAndCancelled(t *testing.T) {
	conn := integrationDB_Light(t)
	ctx := context.Background()
	ws := seedLightWorkspace(t, conn, "test-light-streak", "hibernated", "parked")
	lightExec(t, conn, `INSERT INTO light_schedules (workspace_id, name, cron, prompt) VALUES ($1, 'daily', '0 9 * * *', 'p')`, ws)
	at := func(h int) time.Time { return time.Date(2026, 11, 10+h, 9, 0, 0, 0, time.UTC) }
	ins := `INSERT INTO light_runs (workspace_id, run_id, schedule, status, source, scheduled_for, error) VALUES ($1, $2, 'daily', $3, 'job', $4, $5)`
	lightExec(t, conn, ins, ws, "r1", "succeeded", at(0), "")
	lightExec(t, conn, ins, ws, "r2", "timed_out", at(1), "")
	lightExec(t, conn, ins, ws, "r3", "failed", at(2), "")
	lightExec(t, conn, ins, ws, "r4", "cancelled", at(3), "")
	lightExec(t, conn, ins, ws, "r5", "failed", at(4), "OOMKilled (exit 137)")

	api := &fakeLightAPI{}
	wh := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	wh.SetLightAPI(api)
	suspended, err := wh.evaluateLightStreak(ctx, ws, "daily")
	if err != nil || !suspended {
		t.Fatalf("evaluate = %v, %v; want r5, r3, r2 (cancelled r4 skipped) to suspend", suspended, err)
	}
	var reason sql.NullString
	var count int
	if err := conn.QueryRow(`SELECT suspended_reason, consecutive_failures FROM light_schedules WHERE workspace_id = $1 AND name = 'daily'`, ws).Scan(&reason, &count); err != nil {
		t.Fatal(err)
	}
	if !reason.Valid || count != 3 || len(api.Calls()) != 1 {
		t.Fatalf("reason=%v count=%d calls=%v", reason, count, api.Calls())
	}

	// After an owner clears it, the old streak no longer counts.
	lightExec(t, conn, `UPDATE light_schedules SET suspended_reason = NULL WHERE workspace_id = $1`, ws)
	if suspended, err := wh.evaluateLightStreak(ctx, ws, "daily"); err != nil || suspended {
		t.Fatalf("re-evaluate = %v, %v; runs before the last suspension must not count", suspended, err)
	}
}

func TestIntegration_LightRuns_JobsNeverOverwriteAReport(t *testing.T) {
	conn := integrationDB_Light(t)
	ctx := context.Background()
	ws := seedLightWorkspace(t, conn, "test-light-jobs", "hibernated", "parked")
	lightExec(t, conn, `INSERT INTO light_schedules (workspace_id, name, cron, prompt, cronjob) VALUES ($1, 'daily', '0 9 * * *', 'p', 'light-x-aaaaaaaa')`, ws)
	lightExec(t, conn, `INSERT INTO light_runs (workspace_id, run_id, schedule, status, source, output) VALUES ($1, 'light-x-aaaaaaaa-1', 'daily', 'succeeded', 'report', 'done')`, ws)
	lightExec(t, conn, `INSERT INTO light_runs (workspace_id, run_id, schedule, status, source) VALUES ($1, 'light-x-aaaaaaaa-2', 'daily', 'running', 'report')`, ws)
	exit := int32(137)
	sched := "2026-11-13T09:00:00Z"
	failed, _ := recordLightJobs(ctx, ws, []provisioner.LightJob{
		{RunID: "light-x-aaaaaaaa-1", Schedule: "daily", Phase: "failed", Reason: "Error", ExitCode: &exit, ScheduledFor: &sched},
		{RunID: "light-x-aaaaaaaa-2", Schedule: "daily", Phase: "failed", Reason: "OOMKilled", ExitCode: &exit},
		{RunID: "light-x-aaaaaaaa-3", Schedule: "daily", Phase: "succeeded"},
	})
	if len(failed) != 1 {
		t.Errorf("failed schedules = %v", failed)
	}
	status := func(run string) (string, string, sql.NullTime) {
		var s, e string
		var sf sql.NullTime
		if err := conn.QueryRow(`SELECT status, error, scheduled_for FROM light_runs WHERE workspace_id = $1 AND run_id = $2`, ws, run).Scan(&s, &e, &sf); err != nil {
			t.Fatalf("%s: %v", run, err)
		}
		return s, e, sf
	}
	if s, _, sf := status("light-x-aaaaaaaa-1"); s != "succeeded" || !sf.Valid {
		t.Errorf("a reported outcome must win (got %s); scheduled_for is still filled (%v)", s, sf)
	}
	if s, e, _ := status("light-x-aaaaaaaa-2"); s != "failed" || e != "OOMKilled (exit 137)" {
		t.Errorf("a run that only reported started is closed from its Job: %s %q", s, e)
	}
	if s, _, _ := status("light-x-aaaaaaaa-3"); s != "succeeded" {
		t.Errorf("a run that never reported is recorded from its Job: %s", s)
	}

	recordCancelledLightRuns(ctx, ws, []string{"light-x-aaaaaaaa-9"})
	var schedule, st string
	if err := conn.QueryRow(`SELECT schedule, status FROM light_runs WHERE workspace_id = $1 AND run_id = 'light-x-aaaaaaaa-9'`, ws).Scan(&schedule, &st); err != nil || schedule != "daily" || st != "cancelled" {
		t.Errorf("cancelled run: schedule=%q status=%q (%v); want its schedule found from its CronJob", schedule, st, err)
	}

	lightExec(t, conn, `UPDATE light_runs SET created_at = now() - interval '31 days' WHERE workspace_id = $1 AND run_id = 'light-x-aaaaaaaa-1'`, ws)
	if n, err := pruneLightRuns(ctx, lightRunRetention); err != nil || n < 1 {
		t.Fatalf("prune = %d, %v", n, err)
	}
	var left int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM light_runs WHERE workspace_id = $1`, ws).Scan(&left); err != nil || left != 3 {
		t.Errorf("rows left = %d (%v), want the three recent ones", left, err)
	}
}

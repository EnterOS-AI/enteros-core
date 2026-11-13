//go:build integration
// +build integration

// light_park_integration_test.go — real-Postgres coverage of park, open and
// the monitor's recovery paths, with the real handlers and a fake control
// plane: a full park/open cycle's run tokens, a change made while a park runs,
// the owner taking over a refresh open, a task that starts around the parking
// claim, failed refresh opens, refused auto-parks, a failure streak that
// completes during a park, and an interrupted open (review 21718, probes
// P0-P6).
//
// Run as light_schema_integration_test.go says. NOT SAFE FOR t.Parallel().

package handlers

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/crypto"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/wsauth"
)

func lightIntegrationHandler(t *testing.T, api provisioner.LightAPI) *WorkspaceHandler {
	t.Helper()
	wh := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	wh.SetLightAPI(api)
	waitForHandlerAsyncBeforeDBCleanup(t, wh)
	return wh
}

func lightIntegrationGrid() []volumeEntry {
	return []volumeEntry{{Name: "daily", Cron: "0 9 * * *", Timezone: "UTC", Prompt: "p", Enabled: true}}
}

func lightIntegrationPut() provisioner.LightPutResult {
	return provisioner.LightPutResult{Schedules: []provisioner.LightScheduleState{{Name: "daily", CronJob: "light-x-aaaaaaaa"}}}
}

// seedLightSecret writes the MOLECULE_LIGHT secret enable writes (a park needs it).
func seedLightSecret(t *testing.T, conn *sql.DB, ws string) {
	t.Helper()
	cipher, err := crypto.Encrypt([]byte("1"))
	if err != nil {
		t.Fatal(err)
	}
	lightExec(t, conn, `INSERT INTO workspace_secrets (workspace_id, key, encrypted_value, encryption_version) VALUES ($1, 'MOLECULE_LIGHT', $2, $3)`,
		ws, cipher, crypto.CurrentEncryptionVersion())
}

func lightUnappliedRefreshes(t *testing.T, conn *sql.DB, ws string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM light_pending_actions WHERE workspace_id = $1 AND kind = 'refresh' AND applied_at IS NULL`, ws).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func lightLastErrorCode(t *testing.T, conn *sql.DB, ws string) string {
	t.Helper()
	var code sql.NullString
	if err := conn.QueryRow(`SELECT light_last_error->>'code' FROM workspaces WHERE id = $1`, ws).Scan(&code); err != nil {
		t.Fatal(err)
	}
	return code.String
}

// hookLightAPI runs a hook inside a control-plane call, as if something else
// happened while the call was in flight.
type hookLightAPI struct {
	*fakeLightAPI
	beforePut   func()
	beforePause func()
}

func (p *hookLightAPI) PutLight(ctx context.Context, ws string, req provisioner.LightPutRequest) (provisioner.LightPutResult, error) {
	if p.beforePut != nil {
		p.beforePut()
	}
	return p.fakeLightAPI.PutLight(ctx, ws, req)
}

func (p *hookLightAPI) PauseLight(ctx context.Context, ws string) (provisioner.LightStateResult, error) {
	if p.beforePause != nil {
		p.beforePause()
	}
	return p.fakeLightAPI.PauseLight(ctx, ws)
}

// P0: park, open, park, open, park: at most two run tokens are live, and the
// monitor revokes the retired one once no run is unfinished.
func TestIntegration_LightPark_ParkOpenCycleKeepsAtMostTwoRunTokens(t *testing.T) {
	conn := integrationDB_Light(t)
	setupTestRedis(t)
	ctx := context.Background()
	ws := seedLightWorkspace(t, conn, "test-light-cycle", "online", "open")
	seedLightSecret(t, conn, ws)
	api := &fakeLightAPI{putRes: lightIntegrationPut()}
	wh := lightIntegrationHandler(t, api)
	wh.lightScheduleReader = func(context.Context, string) ([]volumeEntry, error) { return lightIntegrationGrid(), nil }
	var stops int32
	wh.stopFnOverride = func(context.Context, string) { atomic.AddInt32(&stops, 1) }
	tokens := func() (cur, ret string, live int) {
		_ = conn.QueryRow(`SELECT COALESCE(light_run_token_id::text,''), COALESCE(light_retired_token_id::text,'') FROM workspaces WHERE id = $1`, ws).Scan(&cur, &ret)
		_ = conn.QueryRow(`SELECT count(*) FROM workspace_auth_tokens WHERE workspace_id = $1 AND revoked_at IS NULL`, ws).Scan(&live)
		return
	}
	park := func(n int) {
		t.Helper()
		if _, ref := wh.parkLight(ctx, nil, ws, lightTriggerOwner); ref != nil {
			t.Fatalf("park %d refused: %d %v", n, ref.Status, ref.Body)
		}
		if err := wh.finishLightPark(ctx, ws, lightTriggerOwner, time.Second); err != nil {
			t.Fatalf("park %d finish: %v", n, err)
		}
		if st, ls := lightRowStatus(t, conn, ws); st != "hibernated" || ls != "parked" {
			t.Fatalf("park %d: status=%s light_state=%s", n, st, ls)
		}
	}
	open := func(n int) {
		t.Helper()
		_, unchanged, state, ref := wh.openLight(ctx, ws, lightTriggerOwner)
		if ref != nil || unchanged || state != "opening" {
			t.Fatalf("open %d: %v %q %v", n, unchanged, state, ref)
		}
		wh.waitAsyncForTest()
		lightExec(t, conn, `UPDATE workspaces SET status = 'online' WHERE id = $1`, ws) // the pod came up
		if res := NewLightMonitor(wh).SweepTransitions(ctx); res.Acted != 1 {
			t.Fatalf("open %d: monitor %+v", n, res)
		}
		if _, ls := lightRowStatus(t, conn, ws); ls != "open" {
			t.Fatalf("open %d: light_state=%s", n, ls)
		}
	}
	park(1)
	t1, r1, live1 := tokens()
	if t1 == "" || r1 != "" || live1 != 1 {
		t.Fatalf("after park 1: cur=%q ret=%q live=%d", t1, r1, live1)
	}
	open(1)
	park(2)
	t2, r2, live2 := tokens()
	if t2 == t1 || r2 != t1 || live2 != 2 {
		t.Fatalf("after park 2: cur=%q ret=%q live=%d (want ret = park 1's token, 2 live)", t2, r2, live2)
	}
	open(2)
	park(3)
	_, r3, live3 := tokens()
	if r3 != t2 || live3 != 2 {
		t.Fatalf("after park 3: ret=%q live=%d (want ret = park 2's token, park 1's revoked)", r3, live3)
	}
	if res := NewLightMonitor(wh).SweepRuns(ctx); res.Acted != 1 {
		t.Fatalf("runs sweep with no unfinished job: %+v", res)
	}
	if _, r4, live4 := tokens(); r4 != "" || live4 != 1 {
		t.Fatalf("after the runs sweep: ret=%q live=%d (want the retired token revoked)", r4, live4)
	}
	if got := atomic.LoadInt32(&stops); got != 3 {
		t.Errorf("pod stops = %d, want 3", got)
	}
	if calls := strings.Join(api.Calls(), ","); strings.Count(calls, "put ") != 3 || strings.Count(calls, "pause ") != 2 {
		t.Errorf("CP calls = %v", calls)
	}
}

// P1: a change made while a park runs (after the 'parking' claim, before the
// control plane snapshots the env) stays queued after the park, so a refresh
// cycle re-snapshots it (review 21718 R1).
func TestIntegration_LightPark_ChangeDuringAParkStaysQueued(t *testing.T) {
	conn := integrationDB_Light(t)
	setupTestRedis(t)
	ctx := context.Background()
	ws := seedLightWorkspace(t, conn, "test-light-change", "online", "open")
	seedLightSecret(t, conn, ws)
	// The refresh that made the monitor open this agent (change A while parked).
	lightExec(t, conn, `INSERT INTO light_pending_actions (workspace_id, kind, source) VALUES ($1, 'refresh', 'restart')`, ws)
	lightExec(t, conn, `UPDATE workspaces SET light_opened_by = 'refresh' WHERE id = $1`, ws)
	api := &fakeLightAPI{putRes: lightIntegrationPut()}
	wh := lightIntegrationHandler(t, api)
	wh.SetCPProvisioner(&trackingCPProv{})
	wh.stopFnOverride = func(context.Context, string) {}
	queued := false
	wh.lightScheduleReader = func(context.Context, string) ([]volumeEntry, error) {
		// Change B lands now: its restart is refused for a 'parking' agent and
		// turned into a refresh (workspace_restart.go).
		before := lightUnappliedRefreshes(t, conn, ws)
		wh.runRestartCycle(ws, "")
		queued = lightUnappliedRefreshes(t, conn, ws) > before
		return lightIntegrationGrid(), nil
	}
	if _, ref := wh.parkLight(ctx, nil, ws, lightTriggerRefresh); ref != nil {
		t.Fatalf("park refused: %d %v", ref.Status, ref.Body)
	}
	if !queued {
		t.Fatal("change B was merged into the refresh the park was about to mark applied")
	}
	if n := lightUnappliedRefreshes(t, conn, ws); n != 1 {
		t.Fatalf("unapplied refreshes after the park = %d, want change B's (change A's is covered by the PUT)", n)
	}
	if err := wh.finishLightPark(ctx, ws, lightTriggerRefresh, time.Second); err != nil {
		t.Fatalf("finish: %v", err)
	}
	rows, err := lightRefreshCandidates(ctx, lightKnownActionKinds(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lightIDSet(rows)[ws]; !ok {
		t.Error("the parked agent must be refreshed again for change B")
	}
}

// P2: the owner's open of an agent a refresh opened takes it over: it is no
// longer an auto-park candidate until the idle window has passed (review 21718
// R2).
func TestIntegration_LightOpen_OwnerTakesOverARefreshOpen(t *testing.T) {
	conn := integrationDB_Light(t)
	setupTestRedis(t)
	ctx := context.Background()
	ws := seedLightWorkspace(t, conn, "test-light-owner", "online", "open")
	lightExec(t, conn, `UPDATE workspaces SET light_opened_by = 'refresh', light_state_at = now() - interval '10 minutes', last_activity_at = now() - interval '5 minutes' WHERE id = $1`, ws)
	rows, err := lightAutoParkCandidates(ctx, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lightIDSet(rows)[ws]; !ok {
		t.Fatal("setup: a quiet refresh-opened agent is a candidate")
	}
	wh := lightIntegrationHandler(t, &fakeLightAPI{})
	_, unchanged, state, ref := wh.openLight(ctx, ws, lightTriggerOwner)
	if ref != nil || !unchanged || state != "open" {
		t.Fatalf("owner open = %v %q %v", unchanged, state, ref)
	}
	var by string
	if err := conn.QueryRow(`SELECT light_opened_by FROM workspaces WHERE id = $1`, ws).Scan(&by); err != nil || by != "owner" {
		t.Fatalf("light_opened_by = %q (%v), want owner", by, err)
	}
	rows, err = lightAutoParkCandidates(ctx, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lightIDSet(rows)[ws]; ok {
		t.Error("right after the owner's open the agent is an auto-park candidate")
	}
}

// P3: a task that started just before the parking claim. Its heartbeat is
// refused but still records active_tasks, and nothing else; or the turn writes
// activity after the claim. Either way the park is undone (LIGHT_AGENT_BUSY)
// instead of stopping the pod mid-turn (review 21718 R3).
func TestIntegration_LightPark_TaskAroundTheClaimUndoesThePark(t *testing.T) {
	t.Run("its heartbeat reports the task", func(t *testing.T) {
		conn := integrationDB_Light(t)
		setupTestRedis(t)
		ctx := context.Background()
		id := lightTestWS
		seedLightWorkspaceWithID(t, conn, id, "test-light-task", "online", "parking")
		rh := NewRegistryHandler(newTestBroadcaster())
		hb := `{"workspace_id":"` + id + `","error_rate":0,"sample_error":"","active_tasks":1,"uptime_seconds":60}`
		if w := registryCall(t, rh.Heartbeat, "/registry/heartbeat", hb, ""); w.Code != 409 {
			t.Fatalf("heartbeat = %d %s, want 409 LIGHT_AGENT_PARKING", w.Code, w.Body.String())
		}
		var active int
		var beat sql.NullTime
		if err := conn.QueryRow(`SELECT COALESCE(active_tasks, 0), last_heartbeat_at FROM workspaces WHERE id = $1`, id).Scan(&active, &beat); err != nil {
			t.Fatal(err)
		}
		if active != 1 || beat.Valid {
			t.Fatalf("active_tasks=%d last_heartbeat_at=%v, want the task recorded and nothing else", active, beat)
		}
		wh := lightIntegrationHandler(t, &fakeLightAPI{})
		var stops int32
		wh.stopFnOverride = func(context.Context, string) { atomic.AddInt32(&stops, 1) }
		if err := wh.finishLightPark(ctx, id, lightTriggerRefresh, 0); !errors.Is(err, errLightParkAbandoned) {
			t.Fatalf("finish = %v, want errLightParkAbandoned", err)
		}
		if st, ls := lightRowStatus(t, conn, id); atomic.LoadInt32(&stops) != 0 || st != "online" || ls != "open" {
			t.Fatalf("stops=%d status=%s light_state=%s, want the pod left running and the agent open", stops, st, ls)
		}
		if code := lightLastErrorCode(t, conn, id); code != "LIGHT_AGENT_BUSY" {
			t.Errorf("last_error = %q, want LIGHT_AGENT_BUSY", code)
		}
	})
	t.Run("it writes activity after the claim", func(t *testing.T) {
		conn := integrationDB_Light(t)
		setupTestRedis(t)
		ctx := context.Background()
		ws := seedLightWorkspace(t, conn, "test-light-activity", "online", "parking")
		lightExec(t, conn, `UPDATE workspaces SET light_state_at = now() - interval '5 seconds', last_activity_at = now() WHERE id = $1`, ws)
		wh := lightIntegrationHandler(t, &fakeLightAPI{})
		var stops int32
		wh.stopFnOverride = func(context.Context, string) { atomic.AddInt32(&stops, 1) }
		if err := wh.finishLightPark(ctx, ws, lightTriggerOwner, 0); !errors.Is(err, errLightParkAbandoned) || atomic.LoadInt32(&stops) != 0 {
			t.Fatalf("finish = %v stops=%d, want the park undone", err, stops)
		}
	})
}

// P4: refresh opens whose agents failed to boot do not hold the refresh
// slots: they are parked again on their previous snapshot, with a back-off,
// and the next parked agent's queued refresh starts (review 21718 R4).
func TestIntegration_LightMonitor_FailedRefreshOpensReleaseTheirSlots(t *testing.T) {
	conn := integrationDB_Light(t)
	setupTestRedis(t)
	ctx := context.Background()
	a := seedLightWorkspace(t, conn, "test-light-fa", "failed", "open")
	b := seedLightWorkspace(t, conn, "test-light-fb", "failed", "opening")
	c := seedLightWorkspace(t, conn, "test-light-wait", "hibernated", "parked")
	lightExec(t, conn, `UPDATE workspaces SET light_opened_by = 'refresh', light_last_error = '{"code":"LIGHT_OPEN_FAILED","message":"m"}'::jsonb, light_last_error_at = now() - interval '1 hour' WHERE id = ANY($1::uuid[])`, "{"+a+","+b+"}")
	for _, id := range []string{a, b, c} {
		lightExec(t, conn, `INSERT INTO light_pending_actions (workspace_id, kind, source) VALUES ($1, 'refresh', 'secret')`, id)
	}
	api := &fakeLightAPI{}
	wh := lightIntegrationHandler(t, api)
	m := NewLightMonitor(wh)
	for i := 0; i < 3; i++ {
		m.SweepTransitions(ctx)
		m.SweepAutoPark(ctx)
		m.SweepRefresh(ctx)
	}
	if _, ls := lightRowStatus(t, conn, c); ls != "opening" {
		t.Errorf("the queued refresh of the third agent never started (light_state=%s, calls=%v)", ls, api.Calls())
	}
	for name, id := range map[string]string{"a": a, "b": b} {
		st, ls := lightRowStatus(t, conn, id)
		if st != "hibernated" || ls != "parked" || lightLastErrorCode(t, conn, id) != "LIGHT_REFRESH_OPEN_FAILED" {
			t.Errorf("%s: status=%s light_state=%s last_error=%s, want parked again with LIGHT_REFRESH_OPEN_FAILED", name, st, ls, lightLastErrorCode(t, conn, id))
		}
	}
	rows, err := lightRefreshCandidates(ctx, lightKnownActionKinds(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]string{"a": a, "b": b} {
		if _, ok := lightIDSet(rows)[id]; ok {
			t.Errorf("%s is retried before its back-off ends", name)
		}
	}
	if n := lightUnappliedRefreshes(t, conn, a); n != 1 {
		t.Errorf("a's refresh must stay queued for the retry: %d unapplied", n)
	}
}

// P5: an auto park refused before its claim records the refusal, so the error
// back-off keeps the agent out of the next sweeps (review 21718 O2).
func TestIntegration_LightMonitor_AutoParkRefusalBacksOff(t *testing.T) {
	conn := integrationDB_Light(t)
	setupTestRedis(t)
	ctx := context.Background()
	ws := seedLightWorkspace(t, conn, "test-light-runtime", "online", "open")
	lightExec(t, conn, `UPDATE workspaces SET runtime = 'claude-code', light_state_at = now() - interval '45 minutes', last_activity_at = now() - interval '40 minutes' WHERE id = $1`, ws)
	wh := lightIntegrationHandler(t, &fakeLightAPI{})
	if res := NewLightMonitor(wh).SweepAutoPark(ctx); res.Errors != 1 {
		t.Fatalf("res = %+v, want the refusal counted", res)
	}
	if code := lightLastErrorCode(t, conn, ws); code != "LIGHT_RUNTIME_UNSUPPORTED" {
		t.Errorf("last_error = %q, want the refusal recorded", code)
	}
	rows, err := lightAutoParkCandidates(ctx, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lightIDSet(rows)[ws]; ok {
		t.Error("a refused agent is a candidate again at the next sweep")
	}
}

// P6: a failure streak that completes while an auto park is between its render
// and its PUT must not be lost: the evaluation needs the agent's Light lock,
// which the park holds, so it waits, and the CP's suspend lands after the PUT
// rather than being undone by it (review 21718 O1).
func TestIntegration_LightStreak_NoSuspensionLandsInsideAPark(t *testing.T) {
	conn := integrationDB_Light(t)
	setupTestRedis(t)
	ctx := context.Background()
	ws := seedLightWorkspace(t, conn, "test-light-race", "online", "open")
	seedLightSecret(t, conn, ws)
	lightExec(t, conn, `INSERT INTO light_schedules (workspace_id, name, cron, timezone, prompt) VALUES ($1, 'daily', '0 9 * * *', 'UTC', 'p')`, ws)
	for i, run := range []string{"r1", "r2", "r3"} {
		lightExec(t, conn, `INSERT INTO light_runs (workspace_id, run_id, schedule, status, source, scheduled_for) VALUES ($1, $2, 'daily', 'failed', 'report', now() - make_interval(hours => $3))`, ws, run, 3-i)
	}
	inner := &fakeLightAPI{putRes: lightIntegrationPut()}
	api := &hookLightAPI{fakeLightAPI: inner}
	wh := lightIntegrationHandler(t, api)
	wh.lightScheduleReader = func(context.Context, string) ([]volumeEntry, error) { return lightIntegrationGrid(), nil }
	var during error
	var duringSuspended bool
	api.beforePut = func() {
		// What a run's report and the jobs poll do, while the park holds the lock.
		duringSuspended, during = wh.evaluateLightStreakLocked(ctx, ws, "daily")
	}
	lock := lightOpLock(ws)
	lock.Lock() // as SweepAutoPark holds it for the park
	_, ref := wh.parkLight(ctx, nil, ws, lightTriggerAuto)
	lock.Unlock()
	if ref != nil {
		t.Fatalf("park refused: %d %v", ref.Status, ref.Body)
	}
	if !errors.Is(during, errLightLockHeld) || duringSuspended {
		t.Fatalf("evaluation inside the park = %v, %v; want it refused (errLightLockHeld)", duringSuspended, during)
	}
	var reason sql.NullString
	_ = conn.QueryRow(`SELECT suspended_reason FROM light_schedules WHERE workspace_id = $1 AND name = 'daily'`, ws).Scan(&reason)
	if reason.Valid {
		t.Fatalf("core recorded a suspension (%q) during the park whose PUT re-enabled the CronJob", reason.String)
	}
	if suspended, err := wh.evaluateLightStreakLocked(ctx, ws, "daily"); err != nil || !suspended {
		t.Fatalf("evaluation after the park = %v, %v", suspended, err)
	}
	if calls := inner.Calls(); len(calls) != 2 || !strings.HasPrefix(calls[0], "put ") || !strings.HasPrefix(calls[1], "suspend ") {
		t.Errorf("CP calls = %v, want the suspend after the PUT", calls)
	}
}

// An open records its marker before it pauses, and its claim clears it; a
// marker an interrupted open left on a parked agent is resumed by the monitor
// once it is two minutes old, and a younger one is left alone (review 21718
// O3).
func TestIntegration_LightMonitor_InterruptedOpenIsResumed(t *testing.T) {
	conn := integrationDB_Light(t)
	setupTestRedis(t)
	ctx := context.Background()
	ws := seedLightWorkspace(t, conn, "test-light-open", "hibernated", "parked")
	inner := &fakeLightAPI{}
	api := &hookLightAPI{fakeLightAPI: inner}
	wh := lightIntegrationHandler(t, api)
	var duringPause string
	api.beforePause = func() { duringPause = lightLastErrorCode(t, conn, ws) }
	if _, _, state, ref := wh.openLight(ctx, ws, lightTriggerOwner); ref != nil || state != "opening" {
		t.Fatalf("open = %q %v", state, ref)
	}
	wh.waitAsyncForTest()
	if duringPause != lightCodeOpenIncomplete || lightLastErrorCode(t, conn, ws) != "" {
		t.Fatalf("marker during the pause = %q, after the claim = %q; want it set, then cleared", duringPause, lightLastErrorCode(t, conn, ws))
	}

	stuck := seedLightWorkspace(t, conn, "test-light-stuck-open", "hibernated", "parked")
	young := seedLightWorkspace(t, conn, "test-light-young-open", "hibernated", "parked")
	mark := `UPDATE workspaces SET light_last_error = jsonb_build_object('code', 'LIGHT_OPEN_INCOMPLETE', 'message', 'm'), light_last_error_at = now() - make_interval(secs => $2) WHERE id = $1`
	lightExec(t, conn, mark, stuck, 180)
	lightExec(t, conn, mark, young, 30)
	NewLightMonitor(wh).SweepTransitions(ctx)
	calls := strings.Join(inner.Calls(), ",")
	if !strings.Contains(calls, "resume "+stuck) || lightLastErrorCode(t, conn, stuck) != "" {
		t.Errorf("calls=%s marker=%q: the interrupted open's schedules must be resumed and its marker cleared", calls, lightLastErrorCode(t, conn, stuck))
	}
	if strings.Contains(calls, "resume "+young) || lightLastErrorCode(t, conn, young) != lightCodeOpenIncomplete {
		t.Errorf("a marker younger than two minutes may belong to an open still running: calls=%s", calls)
	}
}

// A completed park clears the agent's Light error, except the refresh
// back-off errors whose timestamp the refresh sweep waits on (review 21718 R4,
// N7).
func TestIntegration_LightPark_CompletedParkKeepsTheRefreshBackoff(t *testing.T) {
	conn := integrationDB_Light(t)
	ctx := context.Background()
	got := map[string]string{}
	for _, code := range []string{"LIGHT_PARK_INCOMPLETE", lightCodeRefreshOpenFailed, lightCodeActionFailed} {
		ws := seedLightWorkspace(t, conn, "test-light-keep-"+strings.ToLower(code), "hibernated", "parked")
		lightExec(t, conn, `UPDATE workspaces SET light_last_error = jsonb_build_object('code', $2::text, 'message', 'm'), light_last_error_at = now() WHERE id = $1`, ws, code)
		clearLightParkError(ctx, ws)
		got[code] = lightLastErrorCode(t, conn, ws)
	}
	if got["LIGHT_PARK_INCOMPLETE"] != "" || got[lightCodeRefreshOpenFailed] != lightCodeRefreshOpenFailed || got[lightCodeActionFailed] != lightCodeActionFailed {
		t.Errorf("after a completed park: %v", got)
	}
}

// A park that a core restart interrupted with the pod still up is continued
// by the monitor after two minutes as the original park (here the owner's):
// the run token the interrupted attempt minted is revoked, the refreshes up
// to its claim's watermark are marked applied (a later one stays queued), the
// owner's park clears the schedule's suspension, and the agent ends parked
// (plan Step 3.13; review 21718 N1, N4, N5).
func TestIntegration_LightMonitor_ContinuesAnInterruptedPark(t *testing.T) {
	conn := integrationDB_Light(t)
	setupTestRedis(t)
	ctx := context.Background()
	ws := seedLightWorkspace(t, conn, "test-light-continue", "online", "open")
	seedLightSecret(t, conn, ws)
	lightExec(t, conn, `INSERT INTO light_schedules (workspace_id, name, cron, timezone, prompt, suspended_reason, suspended_at) VALUES ($1, 'daily', '0 9 * * *', 'UTC', 'p', '3 runs in a row failed', now())`, ws)
	lightExec(t, conn, `INSERT INTO light_pending_actions (workspace_id, kind, source) VALUES ($1, 'refresh', 'secret')`, ws)
	api := &fakeLightAPI{putRes: lightIntegrationPut()}
	wh := lightIntegrationHandler(t, api)
	wh.lightScheduleReader = func(context.Context, string) ([]volumeEntry, error) { return lightIntegrationGrid(), nil }
	var stops int32
	wh.stopFnOverride = func(context.Context, string) { atomic.AddInt32(&stops, 1) }

	// The interrupted attempt: claimed, minted and noted its token; then core
	// restarted. A refresh queued after its claim must survive the park.
	if _, claimed, err := wh.claimLightParking(ctx, ws, lightTriggerOwner); err != nil || !claimed {
		t.Fatalf("claim = %v %v", claimed, err)
	}
	lost, _, err := wsauth.IssueAPITokenWithID(ctx, conn, ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordPendingLightToken(ctx, ws, lost); err != nil {
		t.Fatal(err)
	}
	lightExec(t, conn, `INSERT INTO light_pending_actions (workspace_id, kind, source) VALUES ($1, 'refresh', 'plugin')`, ws)
	lightExec(t, conn, `UPDATE workspaces SET light_state_at = now() - interval '3 minutes' WHERE id = $1`, ws)

	if res := NewLightMonitor(wh).SweepTransitions(ctx); res.Acted != 1 || res.Errors != 0 {
		t.Fatalf("sweep = %+v", res)
	}
	if st, ls := lightRowStatus(t, conn, ws); st != "hibernated" || ls != "parked" {
		t.Fatalf("status=%s light_state=%s, want the park finished", st, ls)
	}
	if !lightTokenRevoked(t, conn, lost) {
		t.Error("the run token of the interrupted attempt is still live")
	}
	var pending, reason sql.NullString
	if err := conn.QueryRow(`SELECT light_pending_token_id FROM workspaces WHERE id = $1`, ws).Scan(&pending); err != nil || pending.Valid {
		t.Errorf("light_pending_token_id = %v (%v), want it cleared by the commit", pending, err)
	}
	if err := conn.QueryRow(`SELECT suspended_reason FROM light_schedules WHERE workspace_id = $1 AND name = 'daily'`, ws).Scan(&reason); err != nil || reason.Valid {
		t.Errorf("suspended_reason = %v (%v), want it cleared: the continued park is the owner's", reason, err)
	}
	if n := lightUnappliedRefreshes(t, conn, ws); n != 1 {
		t.Errorf("unapplied refreshes = %d, want only the one queued after the claim", n)
	}
	if got := atomic.LoadInt32(&stops); got != 1 {
		t.Errorf("pod stops = %d, want 1", got)
	}
}

// A park stuck in 'hibernating' for five minutes is finished by the monitor
// (marked hibernated, CronJobs resumed, parked); a younger one is left to the
// hibernate path still running (plan Step 3.13).
func TestIntegration_LightMonitor_FinishesAStuckHibernate(t *testing.T) {
	conn := integrationDB_Light(t)
	setupTestRedis(t)
	ctx := context.Background()
	stuck := seedLightWorkspace(t, conn, "test-light-hibernating", "hibernating", "parking")
	young := seedLightWorkspace(t, conn, "test-light-hibernating-young", "hibernating", "parking")
	lightExec(t, conn, `UPDATE workspaces SET light_state_at = now() - interval '6 minutes' WHERE id = $1`, stuck)
	api := &fakeLightAPI{}
	wh := lightIntegrationHandler(t, api)
	NewLightMonitor(wh).SweepTransitions(ctx)
	if st, ls := lightRowStatus(t, conn, stuck); st != "hibernated" || ls != "parked" {
		t.Errorf("stuck: status=%s light_state=%s, want hibernated/parked", st, ls)
	}
	if st, ls := lightRowStatus(t, conn, young); st != "hibernating" || ls != "parking" {
		t.Errorf("young: status=%s light_state=%s, want it left alone", st, ls)
	}
	if calls := strings.Join(api.Calls(), ","); calls != "resume "+stuck {
		t.Errorf("CP calls = %s, want only the stuck park resumed", calls)
	}
}

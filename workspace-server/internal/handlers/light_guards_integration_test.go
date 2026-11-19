//go:build integration
// +build integration

// light_guards_integration_test.go — Enter OS Light's wake-path closures on
// real Postgres: a parking or parked row is never claimed by a wake, restart
// or provisioning write (whatever the guard read said), a restart already in
// flight when the agent parks does not re-provision it, and a refused
// heartbeat, register, card update or A2A message leaves the row, Redis and
// the activity log exactly as they were.
//
// Run as light_schema_integration_test.go says. NOT SAFE FOR t.Parallel().

package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"
)

func lightRowStatus(t *testing.T, conn *sql.DB, id string) (status, lightState string) {
	t.Helper()
	var ls sql.NullString
	if err := conn.QueryRow(`SELECT status, light_state FROM workspaces WHERE id = $1`, id).Scan(&status, &ls); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return status, ls.String
}

func TestIntegration_LightGuards_ParkedRowIsNeverRevived(t *testing.T) {
	conn := integrationDB_Light(t)
	ctx := context.Background()
	setupTestRedis(t)
	blocked := map[string]bool{"parked": true, "parking": true}
	states := []string{"parked", "parking", "open", "opening", ""}

	// POST /restart's claim.
	for _, st := range states {
		id := seedLightWorkspace(t, conn, "test-light-guard-restart-"+st, "online", st)
		claimed := markProvisioningForRestart(ctx, id)
		status, _ := lightRowStatus(t, conn, id)
		if claimed == blocked[st] || (blocked[st] && status != "online") || (!blocked[st] && status != "provisioning") {
			t.Errorf("restart claim, light_state=%q: claimed=%v status=%s", st, claimed, status)
		}
	}

	// The A2A auto-wake's claim.
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(&trackingCPProv{})
	waitForHandlerAsyncBeforeDBCleanup(t, h)
	for _, st := range states {
		id := seedLightWorkspace(t, conn, "test-light-guard-wake-"+st, "hibernated", st)
		h.WakeWorkspace(id)
		status, _ := lightRowStatus(t, conn, id)
		if blocked[st] && status != "hibernated" {
			t.Errorf("WakeWorkspace woke a %s Light agent: status %s", st, status)
		}
		if !blocked[st] && status == "hibernated" {
			t.Errorf("WakeWorkspace did not wake a workspace with light_state=%q", st)
		}
	}
	h.waitAsyncForTest()

	// Every programmatic restart (runRestartCycle): a mutation-driven one on a
	// parked agent queues one refresh (deduplicated), a crash probe none, and
	// the row is untouched.
	id := seedLightWorkspace(t, conn, "test-light-guard-cycle", "online", "parked")
	h.runRestartCycle(id, "")
	h.runRestartCycle(id, "")
	h.runRestartCycle(id, "liveness-monitor")
	var refreshes int
	if err := conn.QueryRow(`SELECT count(*) FROM light_pending_actions WHERE workspace_id = $1 AND kind = 'refresh' AND applied_at IS NULL`, id).Scan(&refreshes); err != nil {
		t.Fatal(err)
	}
	if status, ls := lightRowStatus(t, conn, id); refreshes != 1 || status != "online" || ls != "parked" {
		t.Errorf("restart cycles on a parked agent: refreshes=%d status=%s light_state=%s, want 1 refresh and the row untouched", refreshes, status, ls)
	}
}

// parkingCPProv is a CP fake whose Stop parks the agent: a park landing while
// a restart cycle has the container stopped.
type parkingCPProv struct {
	*trackingCPProv
	onStop func()
}

func (p *parkingCPProv) Stop(ctx context.Context, workspaceID string) error {
	if p.onStop != nil {
		p.onStop()
	}
	return p.trackingCPProv.Stop(ctx, workspaceID)
}

// A restart whose SELECT saw the agent open must not re-provision it when a
// park claimed it during the stop (review 21716, R4).
func TestIntegration_LightGuards_RestartInFlightCannotReprovision(t *testing.T) {
	conn := integrationDB_Light(t)
	setupTestRedis(t)
	id := seedLightWorkspace(t, conn, "test-light-guard-inflight", "online", "open")
	cp := &parkingCPProv{trackingCPProv: &trackingCPProv{}}
	cp.onStop = func() {
		if _, err := conn.Exec(`UPDATE workspaces SET light_state = 'parked', status = 'hibernated', url = '' WHERE id = $1`, id); err != nil {
			t.Errorf("park during the stop: %v", err)
		}
	}
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(cp)
	waitForHandlerAsyncBeforeDBCleanup(t, h)

	h.runRestartCycle(id, "")
	h.waitAsyncForTest()

	if got := cp.startedSnapshot(); len(got) != 0 {
		t.Fatalf("a restart in flight re-provisioned a parked agent: CP starts %v", got)
	}
	if len(cp.stoppedSnapshot()) == 0 {
		t.Fatal("the cycle never reached its stop: the test did not exercise the provisioning write")
	}
	status, ls := lightRowStatus(t, conn, id)
	var refreshes int
	if err := conn.QueryRow(`SELECT count(*) FROM light_pending_actions WHERE workspace_id = $1 AND kind = 'refresh' AND applied_at IS NULL`, id).Scan(&refreshes); err != nil {
		t.Fatal(err)
	}
	if status != "hibernated" || ls != "parked" || refreshes != 1 {
		t.Errorf("after the cycle: status=%s light_state=%s refreshes=%d, want hibernated/parked and the change queued as a refresh", status, ls, refreshes)
	}
}

func TestIntegration_LightGuards_RefusalsWriteNothing(t *testing.T) {
	conn := integrationDB_Light(t)
	mr := setupTestRedis(t)
	id := lightTestWS
	seedLightWorkspaceWithID(t, conn, id, "test-light-guard-refusals", "hibernated", "parked")
	if _, err := conn.Exec(`UPDATE workspaces SET agent_card = '{"name":"original"}'::jsonb WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	rh := NewRegistryHandler(newTestBroadcaster())
	hb := `{"workspace_id":"` + id + `","error_rate":0,"sample_error":"","active_tasks":3,"uptime_seconds":60}`
	if w := registryCall(t, rh.Heartbeat, "/registry/heartbeat", hb, ""); w.Code != http.StatusConflict {
		t.Fatalf("heartbeat = %d %s", w.Code, w.Body.String())
	}
	reg := `{"id":"` + id + `","url":"http://ws.example:8000","agent_card":{"name":"x"}}`
	if w := registryCall(t, rh.Register, "/registry/register", reg, ""); w.Code != http.StatusConflict {
		t.Fatalf("register = %d %s", w.Code, w.Body.String())
	}
	card := `{"workspace_id":"` + id + `","agent_card":{"name":"rewritten"}}`
	if w := registryCall(t, rh.UpdateCard, "/registry/update-card", card, ""); w.Code != http.StatusConflict {
		t.Fatalf("update-card = %d %s", w.Code, w.Body.String())
	}

	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(&trackingCPProv{})
	waitForHandlerAsyncBeforeDBCleanup(t, h)
	_, _, perr := h.proxyA2ARequest(context.Background(), id, []byte(lightMsgSend), "", true, true)
	if perr == nil || perr.Status != http.StatusConflict {
		t.Fatalf("a2a = %+v, want 409", perr)
	}
	h.waitAsyncForTest()
	time.Sleep(100 * time.Millisecond) // the register handler's deferred stamp runs on return; settle

	var hbAt, regFail sql.NullTime
	var active int
	var status string
	var cardText sql.NullString
	if err := conn.QueryRow(`
		SELECT last_heartbeat_at, last_register_failure_at, COALESCE(active_tasks, 0), status, agent_card::text
		FROM workspaces WHERE id = $1`, id).Scan(&hbAt, &regFail, &active, &status, &cardText); err != nil {
		t.Fatal(err)
	}
	if hbAt.Valid || regFail.Valid || active != 0 || status != "hibernated" || !strings.Contains(cardText.String, "original") {
		t.Errorf("a refused call wrote the parked row: last_heartbeat_at=%v last_register_failure_at=%v active_tasks=%d status=%s card=%s",
			hbAt, regFail, active, status, cardText.String)
	}
	for _, key := range []string{"ws:" + id, "ws:" + id + ":url", "ws:" + id + ":internal_url"} {
		if mr.Exists(key) {
			t.Errorf("Redis key %s written for a parked agent", key)
		}
	}
	var rows int
	if err := conn.QueryRow(`SELECT count(*) FROM activity_logs WHERE workspace_id = $1`, id).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("a refused A2A message to a parked agent left %d activity row(s)", rows)
	}
}

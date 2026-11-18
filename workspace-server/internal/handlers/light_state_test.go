package handlers

import (
	"context"
	"errors"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
)

func TestLightWakeRefusalOfAParkedAgent(t *testing.T) {
	mock := setupTestDB(t)
	expectLightState(mock, lightTestWS, "parked")

	r := lightWakeRefusal(context.Background(), lightTestWS, "test")
	if r == nil || r.Status != http.StatusConflict {
		t.Fatalf("refusal = %+v, want a 409", r)
	}
	want := gin.H{"error": "This agent is a Light agent and is parked. Open it to chat.", "code": "LIGHT_AGENT_PARKED", "light_state": "parked"}
	if !reflect.DeepEqual(r.Body, want) {
		t.Errorf("body = %v, want %v", r.Body, want)
	}
}

func TestLightWakeRefusalOfAParkingAgent(t *testing.T) {
	mock := setupTestDB(t)
	expectLightState(mock, lightTestWS, "parking")

	r := lightWakeRefusal(context.Background(), lightTestWS, "test")
	if r == nil || r.Body["code"] != "LIGHT_AGENT_PARKING" || r.Body["error"] != "This agent is a Light agent and is being parked. Open it to chat." {
		t.Fatalf("refusal = %+v", r)
	}
}

func TestLightWakeRefusalLetsEveryOtherStateThrough(t *testing.T) {
	for _, state := range []string{"", "open", "opening"} {
		mock := setupTestDB(t)
		expectLightState(mock, lightTestWS, state)
		if r := lightWakeRefusal(context.Background(), lightTestWS, "test"); r != nil {
			t.Errorf("state %q: refusal %+v, want nil", state, r)
		}
	}
}

func TestLightWakeRefusalFailsOpenOnADatabaseError(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(lightStateQuery).WithArgs(lightTestWS).WillReturnError(errors.New("db down"))
	if r := lightWakeRefusal(context.Background(), lightTestWS, "test"); r != nil {
		t.Fatalf("refusal = %+v, want nil (the write predicates are the safety)", r)
	}
}

func TestLightEnabledRefusalNamesAnyLightState(t *testing.T) {
	for _, state := range []string{"open", "opening", "parking", "parked"} {
		mock := setupTestDB(t)
		expectLightState(mock, lightTestWS, state)
		r := lightEnabledRefusal(context.Background(), lightTestWS, "test", "park it with POST /workspaces/:id/light/park")
		if r == nil || r.Status != http.StatusConflict || r.Body["code"] != "LIGHT_ENABLED" || r.Body["light_state"] != state {
			t.Errorf("state %q: refusal = %+v", state, r)
		}
	}
	mock := setupTestDB(t)
	expectLightState(mock, lightTestWS, "")
	if r := lightEnabledRefusal(context.Background(), lightTestWS, "test", "x"); r != nil {
		t.Errorf("not Light: refusal = %+v, want nil", r)
	}
}

func TestSetLightStateIsAGuardedTransition(t *testing.T) {
	mock := setupTestDB(t)
	update := `UPDATE workspaces\s+SET light_state = NULLIF\(\$3, ''\), light_state_at = now\(\), updated_at = now\(\)\s+WHERE id = \$1 AND COALESCE\(light_state, ''\) = ANY\(\$2\)`
	mock.ExpectExec(update).WithArgs(lightTestWS, pqArrayArg{"open"}, "parking").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(update).WithArgs(lightTestWS, pqArrayArg{"open", "parking"}, "parking").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(update).WithArgs(lightTestWS, pqArrayArg{"parked"}, "").WillReturnResult(sqlmock.NewResult(0, 1))

	ctx := context.Background()
	if changed, err := setLightState(ctx, db.DB, lightTestWS, []string{"open"}, "parking"); err != nil || changed {
		t.Errorf("not in the from-set: changed=%v err=%v, want false", changed, err)
	}
	if changed, err := setLightState(ctx, db.DB, lightTestWS, []string{"open", "parking"}, "parking"); err != nil || !changed {
		t.Errorf("in the from-set: changed=%v err=%v, want true", changed, err)
	}
	if changed, err := setLightState(ctx, db.DB, lightTestWS, []string{"parked"}, ""); err != nil || !changed {
		t.Errorf("to NULL: changed=%v err=%v, want true", changed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestQueueLightRefreshDedupesAnUnappliedRefresh(t *testing.T) {
	mock := setupTestDB(t)
	insert := `INSERT INTO light_pending_actions \(workspace_id, kind, payload, source\)\s+SELECT \$1, \$2, \$3::jsonb, \$4\s+WHERE NOT EXISTS \(\s+SELECT 1 FROM light_pending_actions a\s+WHERE a.workspace_id = \$1 AND a.kind = \$2 AND a.applied_at IS NULL AND a.payload = \$3::jsonb\s+AND a.id > COALESCE\(\(SELECT w.light_park_watermark FROM workspaces w\s+WHERE w.id = \$1 AND w.light_state = 'parking'\), 0\)\)`
	mock.ExpectExec(insert).WithArgs(lightTestWS, "refresh", "{}", "secret-change").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(insert).WithArgs(lightTestWS, "refresh", "{}", "secret-change").WillReturnResult(sqlmock.NewResult(0, 0))

	ctx := context.Background()
	if queued, err := queueLightRefresh(ctx, lightTestWS, "secret-change"); err != nil || !queued {
		t.Errorf("first refresh: queued=%v err=%v", queued, err)
	}
	if queued, err := queueLightRefresh(ctx, lightTestWS, "secret-change"); err != nil || queued {
		t.Errorf("second refresh while the first is unapplied: queued=%v err=%v, want false", queued, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestQueueLightActionWithoutDedupeAlwaysQueues(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectExec(`INSERT INTO light_pending_actions \(workspace_id, kind, payload, source\)\s+VALUES \(\$1, \$2, \$3::jsonb, \$4\)`).
		WithArgs(lightTestWS, "schedule_edit", `{"name":"daily"}`, "owner").
		WillReturnResult(sqlmock.NewResult(1, 1))
	if queued, err := queueLightAction(context.Background(), lightTestWS, "schedule_edit", "owner", map[string]string{"name": "daily"}, false); err != nil || !queued {
		t.Fatalf("queued=%v err=%v", queued, err)
	}
}

func TestLightOpLockIsPerWorkspace(t *testing.T) {
	a, b := "lock-test-a", "lock-test-b"
	if !lightOpLock(a).TryLock() {
		t.Fatal("first TryLock of a failed")
	}
	defer lightOpLock(a).Unlock()
	if lightOpLock(a).TryLock() {
		t.Fatal("second TryLock of a succeeded; the lock must be held")
	}
	if !lightOpLock(b).TryLock() {
		t.Fatal("TryLock of b failed; locks must be per workspace")
	}
	lightOpLock(b).Unlock()
}

// TestLightMigrationChecksMatchTheGoConstants keeps the migration's CHECK lists
// and the Go constants one set.
func TestLightMigrationChecksMatchTheGoConstants(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/20261113120000_light.up.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	up := string(raw)
	checkList := func(marker string) []string {
		re := regexp.MustCompile(regexp.QuoteMeta(marker) + `\s*\(([^)]*)\)`)
		m := re.FindStringSubmatch(up)
		if m == nil {
			t.Fatalf("no CHECK list after %q", marker)
		}
		var out []string
		for _, part := range strings.Split(m[1], ",") {
			out = append(out, strings.Trim(strings.TrimSpace(part), "'"))
		}
		return out
	}
	if got := checkList("CHECK (light_state IN"); !reflect.DeepEqual(got, models.LightStates) {
		t.Errorf("light_state CHECK = %v, models.LightStates = %v", got, models.LightStates)
	}
	if got := checkList("CHECK (status IN"); !reflect.DeepEqual(got, models.LightRunStatuses) {
		t.Errorf("light_runs.status CHECK = %v, models.LightRunStatuses = %v", got, models.LightRunStatuses)
	}
	if got := checkList("CHECK (source IN"); !reflect.DeepEqual(got, []string{"report", "job"}) {
		t.Errorf("light_runs.source CHECK = %v", got)
	}
	if got := checkList("CHECK (light_opened_by IN"); !reflect.DeepEqual(got, []string{lightOpenedByOwner, lightOpenedByRefresh}) {
		t.Errorf("light_opened_by CHECK = %v", got)
	}
}

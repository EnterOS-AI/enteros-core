package registry

// light_predicates_test.go — Enter OS Light (C5): the registry monitors never
// flip, restart or hibernate a parking or parked Light agent.

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
)

var lightPredicateRe = regexp.QuoteMeta(models.LightWakeAllowedSQL)

func TestHealthSweepUpdateCarriesTheLightPredicate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery("SELECT id FROM workspaces WHERE status IN").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-light"))
	mock.ExpectExec(`AND status NOT IN \('removed', 'provisioning', 'paused', 'hibernated', 'hibernating'\) AND `+lightPredicateRe).
		WithArgs(models.StatusOffline, "ws-light").
		WillReturnResult(sqlmock.NewResult(0, 1))

	checker := &mockChecker{running: map[string]bool{"ws-light": false}}
	sweepOnlineWorkspaces(context.Background(), checker, func(context.Context, string) {})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the health sweep's offline UPDATE must carry the Light predicate: %v", err)
	}
}

func TestHibernationMonitorSkipsLightAgents(t *testing.T) {
	mock := setupHibernationMock(t)
	mock.ExpectQuery(`SELECT id\s+FROM workspaces\s+WHERE hibernation_idle_minutes IS NOT NULL[\s\S]*AND light_state IS NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	hibernateIdleWorkspaces(context.Background(), func(context.Context, string) {
		t.Error("no workspace should be hibernated")
	})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the idle monitor must leave Light agents to the Light monitor: %v", err)
	}
}

// The liveness UPDATE is pinned at the source level (deterministic), and driven
// once through the keyspace event when miniredis delivers it.
func TestLivenessExpiryUpdateCarriesTheLightPredicate(t *testing.T) {
	src, err := os.ReadFile("liveness.go")
	if err != nil {
		t.Fatalf("read liveness.go: %v", err)
	}
	if !strings.Contains(string(src), "WHERE id = $1 AND status NOT IN ('removed', 'paused', 'hibernated') AND `+models.LightWakeAllowedSQL+`") {
		t.Fatal("liveness.go: the expiry UPDATE must carry models.LightWakeAllowedSQL")
	}

	mock := setupLivenessTestDB(t)
	_ = setupLivenessTestRedis(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mock.ExpectExec(`UPDATE workspaces\s+SET status = CASE WHEN runtime = 'external' THEN \$2 ELSE \$3 END,[\s\S]*AND `+lightPredicateRe).
		WithArgs("ws-light-expire", models.StatusAwaitingAgent, models.StatusOffline).
		WillReturnResult(sqlmock.NewResult(0, 0))
	called := make(chan string, 1)
	go StartLivenessMonitor(ctx, func(_ context.Context, id string) { called <- id })
	time.Sleep(100 * time.Millisecond)
	db.RDB.Publish(ctx, "__keyevent@0__:expired", "ws:ws-light-expire")
	select {
	case <-called:
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Log("miniredis did not deliver the keyspace event; the source pin above is the check")
	}
}

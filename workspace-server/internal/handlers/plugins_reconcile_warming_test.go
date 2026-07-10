package handlers

import (
	"context"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
)

// TestPlatformWorkspaceIsWarming_TrueForPlatformProvisioning proves the reconcile
// warming-guard recognizes a kind=platform concierge held in provisioning - the
// state in which a post-delivery restart would interrupt the boot and start the
// ~18s auto-restart loop.
func TestPlatformWorkspaceIsWarming_TrueForPlatformProvisioning(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery("SELECT kind, status FROM workspaces WHERE id =").
		WithArgs("ws-warm").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "status"}).
			AddRow(models.KindPlatform, string(models.StatusProvisioning)))

	if !platformConciergeReconcileShouldSkipRestart(context.Background(), "ws-warm") {
		t.Fatal("expected warming=true for platform/provisioning")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestPlatformConciergeSkipRestart_TrueWhenJustOnline: a kind=platform concierge
// that has JUST reached online must still SKIP the reconcile-restart - the
// reconcile fires on the transition-to-online, pluginPresentOnBox false-negatives
// on hermes, and restarting the just-online concierge bounces it back to
// provisioning (the ~36s online<->provisioning loop observed live 2026-07-09).
func TestPlatformConciergeSkipRestart_TrueWhenJustOnline(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery("SELECT kind, status FROM workspaces WHERE id =").
		WithArgs("ws-online").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "status"}).
			AddRow(models.KindPlatform, string(models.StatusOnline)))

	if !platformConciergeReconcileShouldSkipRestart(context.Background(), "ws-online") {
		t.Fatal("expected skip-restart=TRUE for platform/online (just-promoted concierge must not be reconcile-restarted)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestPlatformWorkspaceIsWarming_FalseForNonPlatform: an ordinary workspace in
// provisioning is not subject to the concierge warming hold - not warming here.
func TestPlatformWorkspaceIsWarming_FalseForNonPlatform(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery("SELECT kind, status FROM workspaces WHERE id =").
		WithArgs("ws-plain").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "status"}).
			AddRow(models.KindWorkspace, string(models.StatusProvisioning)))

	if platformConciergeReconcileShouldSkipRestart(context.Background(), "ws-plain") {
		t.Fatal("expected warming=false for non-platform workspace")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestPlatformWorkspaceIsWarming_FailOpenOnQueryError: a DB error must fail-open
// to false (restart-as-before) - a transient blip must never mask a genuinely
// needed restart on a settled workspace.
func TestPlatformWorkspaceIsWarming_FailOpenOnQueryError(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery("SELECT kind, status FROM workspaces WHERE id =").
		WithArgs("ws-err").
		WillReturnError(context.DeadlineExceeded)

	if platformConciergeReconcileShouldSkipRestart(context.Background(), "ws-err") {
		t.Fatal("expected warming=false (fail-open) on query error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestReconcile_PlatformConciergeSuppressesAutomaticRestart(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	h, _ := newReconcileHandler(t)

	expectDeclared(mock,
		[]DeclaredPlugin{{PluginName: "seo-all", SourceRaw: "local://seo-all"}},
		nil,
	)
	mock.ExpectQuery("SELECT kind, status FROM workspaces WHERE id =").
		WithArgs("ws-platform-warming").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "status"}).
			AddRow(models.KindPlatform, string(models.StatusProvisioning)))

	suppressed := false
	h.deliverOverride = func(_ context.Context, _ string, stage *stageResult) error {
		suppressed = stage.SuppressRestart
		return nil
	}
	mock.ExpectExec(`INSERT INTO workspace_plugins`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	h.ReconcileWorkspacePlugins(context.Background(), "ws-platform-warming")

	if !suppressed {
		t.Fatal("platform concierge reconcile delivered without SuppressRestart")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/metrics"
)

// core#4997 "also fix the silence". The degraded-install rule already exists and
// already fires -- GET /admin/plugin-install-reports?status=degraded names the
// affected workspace and the failing source exactly. Nothing consumed it, so a
// customer's declared plugin (`enteros-wechat-channel`) failed to install on
// EVERY boot from 2026-07-27 and was found only by accident while investigating
// something else. A detection nobody reads is not a detection.
//
// This sweep gives the existing rule a number that a dashboard can alert on.
// It is observability only: it reads, it never writes a workspace row.

func TestDegradedSweep_UsesTheSharedPredicateNotACopy(t *testing.T) {
	// The SQL/Go duplication of this rule is already called out as a drift
	// risk in admin_plugin_install_reports.go. A third hand-written copy here
	// is exactly how the alert would keep reporting 0 after the rule changed,
	// so the query must be BUILT from the constant.
	q := degradedCountQuery()
	if !strings.Contains(q, degradedFleetPredicate) {
		t.Errorf("degraded count query does not embed degradedFleetPredicate;\n query = %s\n want it to contain = %s", q, degradedFleetPredicate)
	}
	if !strings.Contains(q, "workspace_plugin_install_reports") {
		t.Errorf("query should read the install-report table, got %s", q)
	}
}

func TestDegradedSweep_PublishesTheCount(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = mockDB.Close() }()

	mock.ExpectQuery("workspace_plugin_install_reports").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))

	metrics.SetDegradedPluginWorkspaces(0)
	if err := sweepDegradedPluginInstallsOnce(context.Background(), mockDB); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := metrics.DegradedPluginWorkspaces(); got != 3 {
		t.Errorf("gauge = %d, want 3", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A zero must be PUBLISHED, not skipped. If the sweep only wrote the gauge when
// something was wrong, the metric would stay at its last bad value after the
// fleet recovered and the alert would never clear -- the mirror image of the
// bug this exists to catch.
func TestDegradedSweep_PublishesZeroSoTheAlertCanClear(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = mockDB.Close() }()

	mock.ExpectQuery("workspace_plugin_install_reports").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	metrics.SetDegradedPluginWorkspaces(7)
	if err := sweepDegradedPluginInstallsOnce(context.Background(), mockDB); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := metrics.DegradedPluginWorkspaces(); got != 0 {
		t.Errorf("gauge = %d after a clean sweep, want 0 so the alert clears", got)
	}
}

// A failed sweep must NOT leave a stale number looking like a fresh reading.
// Reporting the previous count as if it were current is the confident-lie
// failure mode; the error is returned so the caller can count it separately.
func TestDegradedSweep_QueryErrorDoesNotOverwriteWithAFakeZero(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = mockDB.Close() }()

	mock.ExpectQuery("workspace_plugin_install_reports").
		WillReturnError(errors.New("connection reset"))

	metrics.SetDegradedPluginWorkspaces(4)
	if err := sweepDegradedPluginInstallsOnce(context.Background(), mockDB); err == nil {
		t.Fatal("a failing query must return an error")
	}
	if got := metrics.DegradedPluginWorkspaces(); got != 4 {
		t.Errorf("gauge = %d after a FAILED sweep, want the prior 4 left untouched "+
			"(a failed read must not be published as 0)", got)
	}
}

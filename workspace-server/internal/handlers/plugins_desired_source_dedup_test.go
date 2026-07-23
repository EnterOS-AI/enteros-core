package handlers

// plugins_desired_source_dedup_test.go — pins the cross-NAME source dedup in
// desiredPluginSources (2026-07-23 boot-install regression).
//
// One plugin can be tracked under TWO names: the scheduler is DECLARED as
// "molecule-scheduler" (SchedulerPluginName, ensureSchedulerPluginDeclared)
// but the post-install reconcile records it in workspace_plugins under its
// repo-derived name ("molecule-ai-plugin-scheduler"). Name-keyed dedup kept
// both, MOLECULE_DECLARED_PLUGINS listed the identical source twice, and the
// template's boot-install destination-conflict gate failed BOTH copies
// ("duplicate install destination ... — keeping existing plugins tree
// intact"): installed=0, and a FRESH volume boots with no plugins at all.
//
// Property 1: identical sources under different names collapse to one.
// Property 2 (negative control): same destination but DIFFERENT refs both
// survive — that is a genuine ambiguity the template gate must still see.

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

func TestDesiredPluginSources_IdenticalSourceAcrossNames_Collapses(t *testing.T) {
	mockDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()
	orig := db.DB
	db.DB = mockDB
	defer func() { db.DB = orig }()

	const wsID = "ws-dedup"
	const schedSrc = "gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0"

	mock.ExpectQuery(`FROM workspace_declared_plugins`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}).
			AddRow("molecule-scheduler", schedSrc))
	mock.ExpectQuery(`FROM workspace_plugins`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}).
			AddRow("molecule-ai-plugin-scheduler", schedSrc))

	srcs, err := desiredPluginSources(context.Background(), wsID)
	if err != nil {
		t.Fatalf("desiredPluginSources: %v", err)
	}
	if len(srcs) != 1 || srcs[0] != schedSrc {
		t.Fatalf("identical source under two names must collapse to one; got %v", srcs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

func TestDesiredPluginSources_DifferentRefsSameDest_BothSurvive(t *testing.T) {
	mockDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()
	orig := db.DB
	db.DB = mockDB
	defer func() { db.DB = orig }()

	const wsID = "ws-ambig"

	mock.ExpectQuery(`FROM workspace_declared_plugins`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}).
			AddRow("molecule-scheduler", "gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0"))
	mock.ExpectQuery(`FROM workspace_plugins`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}).
			AddRow("molecule-ai-plugin-scheduler", "gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.3.0"))

	srcs, err := desiredPluginSources(context.Background(), wsID)
	if err != nil {
		t.Fatalf("desiredPluginSources: %v", err)
	}
	if len(srcs) != 2 {
		t.Fatalf("same dest with DIFFERENT refs must both survive (template gate flags the ambiguity); got %v", srcs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

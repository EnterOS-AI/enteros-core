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
// Property 2: DIFFERENT refs for the same destination resolve via
// installed-wins (review wf_7cb5003d finding #1: emitting both let the
// template's duplicate-destination gate reject BOTH after a routine
// user re-pin — a plugin-less boot one ref-bump away).
// Property 3 (negative control): genuinely distinct plugins both survive.

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

func TestDesiredPluginSources_InstalledRefWinsAcrossAliasedNames(t *testing.T) {
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
	want := "gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.3.0"
	if len(srcs) != 1 || srcs[0] != want {
		t.Fatalf("same destination under aliased names must resolve installed-wins (%s); got %v", want, srcs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

func TestDesiredPluginSources_DistinctPluginsBothSurvive(t *testing.T) {
	mockDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer mockDB.Close()
	orig := db.DB
	db.DB = mockDB
	defer func() { db.DB = orig }()

	const wsID = "ws-distinct"

	mock.ExpectQuery(`FROM workspace_declared_plugins`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}).
			AddRow("molecule-scheduler", "gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0"))
	mock.ExpectQuery(`FROM workspace_plugins`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}).
			AddRow("lark-channel", "gitea://molecule-ai/lark-channel-molecule#main"))

	srcs, err := desiredPluginSources(context.Background(), wsID)
	if err != nil {
		t.Fatalf("desiredPluginSources: %v", err)
	}
	if len(srcs) != 2 {
		t.Fatalf("distinct plugins must both survive; got %v", srcs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

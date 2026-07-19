package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// The declared source is load-bearing: it is the exact string the boot-installer
// / reconcile git-clones. A typo (wrong repo, missing #ref) silently installs
// nothing, so the daemon never arms and schedules fire nowhere. Guard the exact
// name + source, not just "some insert happened".
func TestEnsureSchedulerPluginDeclared_DeclaresPinnedSource(t *testing.T) {
	mock := setupTestDB(t)
	// molecule-scheduler is not the privileged concierge MCP → no kind precheck,
	// straight to the declared-plugins upsert.
	mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).
		WithArgs("ws-1", "molecule-scheduler", "gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.1.0").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := ensureSchedulerPluginDeclared(context.Background(), "ws-1"); err != nil {
		t.Fatalf("declare must succeed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (wrong/absent declared-plugin write): %v", err)
	}
}

// A source-string regression (scheme, repo, or pin) would be invisible at
// runtime — the clone just fails and the plugin never lands. Pin the shape.
func TestSchedulerPluginSourceWellFormed(t *testing.T) {
	if SchedulerPluginName != "molecule-scheduler" {
		t.Fatalf("plugin name must match plugin.yaml `name`, got %q", SchedulerPluginName)
	}
	if !strings.HasPrefix(SchedulerPluginSource, "gitea://molecule-ai/molecule-ai-plugin-scheduler") {
		t.Fatalf("source must point at the scheduler plugin repo, got %q", SchedulerPluginSource)
	}
	if !strings.Contains(SchedulerPluginSource, "#v") {
		t.Fatalf("source must pin a version tag (#vX.Y.Z), got %q", SchedulerPluginSource)
	}
}

// Arming is best-effort: a workspace with no callback URL (poll-mode / not yet
// registered) must NOT attempt a forward — it arms on the next reconcile. The
// negative control is the absence of a secret read / any further query after the
// url lookup returns empty; ExpectationsWereMet fails if arm reached further.
func TestArmSchedulerPlugin_NoForwardWithoutURL(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT COALESCE\(url, ''\) FROM workspaces WHERE id =`).
		WithArgs("ws-nourl").
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow(""))
	// NO further expectations: empty url ⇒ arm returns before the secret read.

	armSchedulerPlugin(context.Background(), "ws-nourl")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("arm must stop at the empty-url check (no secret read / forward): %v", err)
	}
}

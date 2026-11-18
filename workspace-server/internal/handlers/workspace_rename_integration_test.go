//go:build integration

package handlers

import (
	"net/http"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/google/uuid"
)

// Selected by the required handlers-postgres-integration CI gate.
func TestIntegration_WorkspaceRename_PreservesStateAndRejectsCollisions(t *testing.T) {
	conn := integrationDB_AdminTokenGate(t)
	previous := db.DB
	db.DB = conn
	t.Cleanup(func() { db.DB = previous })
	prefix := "rename-itest-" + uuid.NewString()
	id, _ := seedRoutableWorkspace(t, conn, prefix)
	_, _ = seedRoutableWorkspace(t, conn, prefix+"-taken")
	snapshot := func() string {
		t.Helper()
		var state string
		if err := conn.QueryRow(`SELECT (to_jsonb(w) - 'name' - 'updated_at')::text FROM workspaces w WHERE id = $1`, id).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := snapshot()
	emitter := &capturingEmitter{}
	h := NewWorkspaceHandler(emitter, nil, "", "")
	for _, tc := range []struct {
		name string
		code int
	}{
		{prefix + "-taken", http.StatusConflict},
		{prefix + " Personal Brand", http.StatusOK},
		{prefix + " Personal Brand", http.StatusOK}, // Idempotent retry.
	} {
		eventsBefore := len(emitter.events)
		w := patchWorkspaceName(t, h, id, tc.name)
		if w.Code != tc.code {
			t.Fatalf("rename %q: got %d, want %d: %s", tc.name, w.Code, tc.code, w.Body)
		}
		wantName := tc.name
		if tc.code != http.StatusOK {
			wantName = prefix
			if len(emitter.events) != eventsBefore {
				t.Fatal("rejected rename emitted an update")
			}
		}
		var name string
		if err := conn.QueryRow(`SELECT name FROM workspaces WHERE id = $1`, id).Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name != wantName || snapshot() != before {
			t.Fatalf("name=%q, want=%q; fields other than name/updated_at must remain unchanged", name, wantName)
		}
	}
}

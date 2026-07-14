//go:build integration
// +build integration

package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	_ "github.com/lib/pq"
)

func TestIntegration_NativeChannelCutoverInventoryExecutesOnPostgres(t *testing.T) {
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Skip("INTEGRATION_DB_URL not set")
	}
	database, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Ping(); err != nil {
		t.Fatalf("ping postgres: %v", err)
	}

	const (
		activeID  = "9d3bfd26-f808-4a9b-879b-1a02085af001"
		emptyID   = "9d3bfd26-f808-4a9b-879b-1a02085af002"
		removedID = "9d3bfd26-f808-4a9b-879b-1a02085af003"
	)
	ids := []any{activeID, emptyID, removedID}
	if _, err := database.Exec(`DELETE FROM workspaces WHERE id IN ($1, $2, $3)`, ids...); err != nil {
		t.Fatalf("clear prior fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = database.Exec(`DELETE FROM workspaces WHERE id IN ($1, $2, $3)`, ids...)
	})

	if _, err := database.Exec(`
		INSERT INTO workspaces (id, name, status) VALUES
			($1, 'cutover-active', 'online'),
			($2, 'cutover-empty', 'online'),
			($3, 'cutover-removed', 'removed')
	`, ids...); err != nil {
		t.Fatalf("insert workspaces: %v", err)
	}
	if _, err := database.Exec(`
		INSERT INTO workspace_channels (workspace_id, channel_type) VALUES
			($1, 'slack'),
			($1, 'lark'),
			($2, 'telegram')
	`, activeID, removedID); err != nil {
		t.Fatalf("insert channel rows: %v", err)
	}

	w := runNativeChannelCutoverInventory(t, database)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got nativeChannelCutoverInventoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.TableState != "present" || got.WorkspaceRowSum+got.OrphanRows != got.TotalRows {
		t.Fatalf("invalid global accounting: %+v", got)
	}
	if got.OrphanRows < 1 {
		t.Fatalf("removed-workspace row was not classified as orphan: %+v", got)
	}
	counts := make(map[string]int64, len(got.Workspaces))
	for _, entry := range got.Workspaces {
		counts[entry.WorkspaceID] = entry.RowCount
	}
	if counts[activeID] != 2 {
		t.Fatalf("active workspace count = %d, want 2", counts[activeID])
	}
	if count, ok := counts[emptyID]; !ok || count != 0 {
		t.Fatalf("zero-row active workspace missing: count=%d present=%v", count, ok)
	}
	if _, ok := counts[removedID]; ok {
		t.Fatal("removed workspace must be represented in orphan_rows, not the active roster")
	}
}

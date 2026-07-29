package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

// Real-PG test for DesktopControlStatus (GET /desktop/control) — the endpoint the
// agent's desktop_wait_for_control tool polls. Runs only when INTEGRATION_DB_URL
// is set. Verifies human_in_control across the four lock states.
func TestDesktopControlStatus_Integration(t *testing.T) {
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Skip("INTEGRATION_DB_URL not set; skipping DesktopControlStatus real-PG test")
	}
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	prevDB := db.DB
	db.DB = conn // the handler reads the package-global DB
	defer func() { db.DB = prevDB }()

	for _, s := range []string{
		`DROP SCHEMA public CASCADE`, `CREATE SCHEMA public`,
		`CREATE TABLE workspaces (id uuid PRIMARY KEY)`,
		`CREATE TABLE workspace_display_control_locks (
			workspace_id uuid PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
			controller text NOT NULL CHECK (controller IN ('user','agent')),
			controlled_by text NOT NULL,
			expires_at timestamptz NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now())`,
	} {
		if _, err := conn.Exec(s); err != nil {
			t.Fatalf("setup: %v\n%s", err, s)
		}
	}
	const ws = "33333333-3333-3333-3333-333333333333"
	if _, err := conn.Exec(`INSERT INTO workspaces(id) VALUES ($1)`, ws); err != nil {
		t.Fatal(err)
	}

	h := &WorkspaceHandler{}
	// DesktopControlStatus now 503s when the feature is unavailable (nil gateway),
	// so wire a stub gateway to exercise the enabled/200 path — the status endpoint
	// itself reads only the lock via the package-global DB, never the gateway.
	h.SetDesktopGateway(&fakeDesktopGW{})
	call := func() (humanInControl, agentCanControl bool) {
		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/desktop/control", nil)
		c.Params = gin.Params{{Key: "id", Value: ws}}
		h.DesktopControlStatus(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200", w.Code)
		}
		var body struct {
			HumanInControl  bool `json:"human_in_control"`
			AgentCanControl bool `json:"agent_can_control"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
		return body.HumanInControl, body.AgentCanControl
	}

	// 1) No lock → agent can control.
	if hic, acc := call(); hic || !acc {
		t.Fatalf("no lock: human=%v agent_can=%v, want (false,true)", hic, acc)
	}
	// 2) Agent holds → not human-in-control.
	if _, err := conn.Exec(`INSERT INTO workspace_display_control_locks(workspace_id,controller,controlled_by,expires_at) VALUES ($1,'agent','agent',now()+interval '5 min')`, ws); err != nil {
		t.Fatal(err)
	}
	if hic, acc := call(); hic || !acc {
		t.Fatalf("agent lock: human=%v agent_can=%v, want (false,true)", hic, acc)
	}
	// 3) Human holds → human-in-control (agent must wait).
	if _, err := conn.Exec(`UPDATE workspace_display_control_locks SET controller='user',controlled_by='user-a',expires_at=now()+interval '5 min' WHERE workspace_id=$1`, ws); err != nil {
		t.Fatal(err)
	}
	if hic, acc := call(); !hic || acc {
		t.Fatalf("human lock: human=%v agent_can=%v, want (true,false)", hic, acc)
	}
	// 4) Human lock expired → agent can control again.
	if _, err := conn.Exec(`UPDATE workspace_display_control_locks SET expires_at=now()-interval '1 min' WHERE workspace_id=$1`, ws); err != nil {
		t.Fatal(err)
	}
	if hic, acc := call(); hic || !acc {
		t.Fatalf("expired human lock: human=%v agent_can=%v, want (false,true)", hic, acc)
	}
}

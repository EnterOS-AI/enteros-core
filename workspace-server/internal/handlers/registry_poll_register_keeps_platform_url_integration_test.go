//go:build integration
// +build integration

// registry_poll_register_keeps_platform_url_integration_test.go — REAL Postgres
// proof that a poll-mode /registry/register cannot detach a PLATFORM-PROVISIONED
// workspace from its platform URL, while register-created external peers keep
// the push→poll switch poll mode exists for.
//
// Live 2026-11-13 (reno-stars, workspace ca17a092 — claude-code, provisioned by
// the platform at http://enteros-ws-ca17a092fecb.<ns>.svc.cluster.local:8000):
// an external client holding that workspace's token POSTed /registry/register
// with delivery_mode=poll and no url (04:50–06:06Z, from the ingress, not the
// box). The upsert wrote url=NULL and delivery_mode='poll' over the box's row.
// Every URL forward (schedules, chat files) then answered 422 "no callback URL"
// while the box was up, until a restart re-registered it (09:31Z).
//
// The discriminator is instance_id (written by the CP provisioner for every
// workspace it starts), NOT runtime: register-created external peers get the
// runtime column default ('hermes') and look hosted by runtime — reno-stars'
// own external poll peers 841b2f58… / 21e7d53f… are exactly that shape.
//
// Run with:
//
//	INTEGRATION_DB_URL="postgres://postgres:test@HOST:5432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ \
//	  -run Integration_Register_Poll -v

package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/wsauth"
)

const platformHostedURL = "http://enteros-ws-abc.enteros-test.svc.cluster.local:8000"

// seedRegisteredWorkspace seeds an online row. instanceID "" leaves the column
// NULL (a register-created peer); runtime "" leaves the column default.
func seedRegisteredWorkspace(t *testing.T, conn *sql.DB, id, runtime, instanceID, url, mode string) {
	t.Helper()
	var rt, inst sql.NullString
	if runtime != "" {
		rt = sql.NullString{String: runtime, Valid: true}
	}
	if instanceID != "" {
		inst = sql.NullString{String: instanceID, Valid: true}
	}
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO workspaces (id, name, status, kind, runtime, instance_id, url, delivery_mode)
		 VALUES ($1, $2, 'online'::workspace_status, 'workspace', COALESCE($3, 'hermes'), $4, $5, $6)`,
		id, "integ-dormant-pollkeep-"+id, rt, inst, url, mode); err != nil {
		t.Fatalf("seed workspace (runtime=%q instance_id=%q): %v", runtime, instanceID, err)
	}
}

func urlAndModeOf(t *testing.T, conn *sql.DB, id string) (string, string) {
	t.Helper()
	var url, mode sql.NullString
	if err := conn.QueryRowContext(context.Background(),
		`SELECT url, delivery_mode FROM workspaces WHERE id = $1`, id).Scan(&url, &mode); err != nil {
		t.Fatalf("read url/mode: %v", err)
	}
	return url.String, mode.String
}

// callPollRegister sends delivery_mode=poll with an optional top-level url —
// the exact shape the external client sent was url="".
func callPollRegister(t *testing.T, h *RegistryHandler, id, url string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	urlField := ""
	if url != "" {
		urlField = fmt.Sprintf(`"url":%q,`, url)
	}
	body := fmt.Sprintf(`{"id":%q,%s"delivery_mode":"poll","agent_card":{"name":"external-peer","version":"1.0"}}`, id, urlField)
	c.Request = httptest.NewRequest("POST", "/registry/register", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Register(c)
	return w.Code, w.Body.String()
}

func TestIntegration_Register_PollOnProvisionedWorkspace_Refused(t *testing.T) {
	conn := integrationDB_DormantRegister(t) // cleanup matches 'integ-dormant-%'
	setupTestRedis(t)
	h := &RegistryHandler{broadcaster: newTestBroadcaster()}

	for _, tc := range []struct{ name, url string }{
		{"no url (the live shape)", ""},
		{"with a url (poll urls are stored unvalidated — must not bypass)", "https://attacker.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.NewString()
			seedRegisteredWorkspace(t, conn, id, "claude-code", "enteros-ws-"+id[:12], platformHostedURL, "push")

			code, body := callPollRegister(t, h, id, tc.url)

			if code != http.StatusConflict {
				t.Errorf("poll register on a provisioned workspace: http=%d, want 409 (%s)", code, body)
			}
			if !strings.Contains(body, "platform-hosted") || !strings.Contains(body, "their own workspace ids") {
				t.Errorf("409 must say the workspace is platform-hosted and where external peers belong: %s", body)
			}
			if !strings.Contains(body, `"delivery_mode":"push"`) {
				t.Errorf("409 must report the STORED mode (push), never the requested poll: %s", body)
			}
			url, mode := urlAndModeOf(t, conn, id)
			if url != platformHostedURL || mode != "push" {
				t.Errorf("provisioned row changed: url=%q mode=%q, want %q/push", url, mode, platformHostedURL)
			}
		})
	}
}

func TestIntegration_Register_PollOnRegisterCreatedHermesPeer_SwitchesToPoll(t *testing.T) {
	// The external-peer shape the runtime discriminator got wrong: runtime is the
	// column default 'hermes', NO instance_id, a previously-registered url.
	conn := integrationDB_DormantRegister(t)
	setupTestRedis(t)
	h := &RegistryHandler{broadcaster: newTestBroadcaster()}

	id := uuid.NewString()
	seedRegisteredWorkspace(t, conn, id, "", "", "https://old-tunnel.example.com", "push")

	code, body := callPollRegister(t, h, id, "")

	if code != http.StatusOK {
		t.Fatalf("poll register on an unprovisioned peer: http=%d, want 200 (%s)", code, body)
	}
	url, mode := urlAndModeOf(t, conn, id)
	if url != "" || mode != "poll" {
		t.Errorf("hermes-default peer without instance_id: url=%q mode=%q, want url cleared and mode=poll", url, mode)
	}
}

func TestIntegration_Register_PollOnExternalPeer_SwitchesToPoll(t *testing.T) {
	conn := integrationDB_DormantRegister(t)
	setupTestRedis(t)
	h := &RegistryHandler{broadcaster: newTestBroadcaster()}

	id := uuid.NewString()
	seedRegisteredWorkspace(t, conn, id, "external", "", "https://old-tunnel.example.com", "push")

	code, body := callPollRegister(t, h, id, "")

	if code != http.StatusOK {
		t.Fatalf("external poll register: http=%d, want 200 (%s)", code, body)
	}
	if url, mode := urlAndModeOf(t, conn, id); url != "" || mode != "poll" {
		t.Errorf("external workspace poll register: url=%q mode=%q, want url cleared and mode=poll", url, mode)
	}
}

func TestIntegration_Register_PushOnProvisionedWorkspace_StillAccepted(t *testing.T) {
	// Positive control: the box's own (push) register is untouched by the guard.
	conn := integrationDB_DormantRegister(t)
	setupTestRedis(t)
	h := &RegistryHandler{broadcaster: newTestBroadcaster()}

	id := uuid.NewString()
	seedRegisteredWorkspace(t, conn, id, "claude-code", "enteros-ws-"+id[:12], platformHostedURL, "push")

	if code := callRegister(t, h, id, "http://localhost:9100"); code != http.StatusOK {
		t.Fatalf("push register from the box: http=%d, want 200", code)
	}
	if _, mode := urlAndModeOf(t, conn, id); mode != "push" {
		t.Errorf("push register left mode=%q, want push", mode)
	}
}

func TestIntegration_Register_PollOnDockerProvisionedWorkspace_Refused(t *testing.T) {
	// Self-host local-docker provisioner shape (workspace_provision.go
	// h.provisioner.Start): it does NOT write instance_id; it writes the
	// host-port URL it advertises (http://localhost:<port>, port != 8000).
	conn := integrationDB_DormantRegister(t)
	setupTestRedis(t)
	h := &RegistryHandler{broadcaster: newTestBroadcaster()}

	const dockerURL = "http://localhost:41234"
	id := uuid.NewString()
	seedRegisteredWorkspace(t, conn, id, "claude-code", "", dockerURL, "push")

	code, body := callPollRegister(t, h, id, "")

	if code != http.StatusConflict {
		t.Errorf("poll register on a docker-provisioned workspace: http=%d, want 409 (%s)", code, body)
	}
	if url, mode := urlAndModeOf(t, conn, id); url != dockerURL || mode != "push" {
		t.Errorf("docker-provisioned row changed: url=%q mode=%q, want %q/push", url, mode, dockerURL)
	}
}

// TestIntegration_Register_PollOnProvisionedWorkspace_SQLArmHoldsWithoutPrecheck
// disables the Go 409 precheck and proves the upsert's own CASE arm keeps url
// AND delivery_mode — the protection is atomic in SQL, not only a
// check-then-act in Go (a raced or failed-open precheck cannot detach the box).
func TestIntegration_Register_PollOnProvisionedWorkspace_SQLArmHoldsWithoutPrecheck(t *testing.T) {
	conn := integrationDB_DormantRegister(t)
	setupTestRedis(t)
	h := &RegistryHandler{broadcaster: newTestBroadcaster()}

	prev := pollRegisterPrecheck
	pollRegisterPrecheck = func(*RegistryHandler, context.Context, *gin.Context, string) bool { return false }
	t.Cleanup(func() { pollRegisterPrecheck = prev })

	for _, tc := range []struct{ name, instanceID, url string }{
		{"CP-provisioned (instance_id)", "enteros-ws-sqlarm", platformHostedURL},
		{"docker-provisioned (host-port url)", "", "http://127.0.0.1:41235"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.NewString()
			seedRegisteredWorkspace(t, conn, id, "claude-code", tc.instanceID, tc.url, "push")

			_, _ = callPollRegister(t, h, id, "https://attacker.example.com")

			if url, mode := urlAndModeOf(t, conn, id); url != tc.url || mode != "push" {
				t.Errorf("SQL arm did not hold: url=%q mode=%q, want %q/push", url, mode, tc.url)
			}
		})
	}
}

// A platform-hosted box may legitimately run poll (MOLECULE_DELIVERY_MODE /
// config.a2a.delivery_mode; POST /workspaces stores delivery_mode=poll). Its
// row is stored poll, and its own poll register must go through — neither
// the Go 409 nor the SQL arm may force it back to push.
func TestIntegration_Register_PollOnProvisionedWorkspaceStoredPoll_Accepted(t *testing.T) {
	conn := integrationDB_DormantRegister(t)
	setupTestRedis(t)
	h := &RegistryHandler{broadcaster: newTestBroadcaster()}

	id := uuid.NewString()
	seedRegisteredWorkspace(t, conn, id, "claude-code", "enteros-ws-"+id[:12], platformHostedURL, "poll")

	code, body := callPollRegister(t, h, id, "")

	if code != http.StatusOK {
		t.Fatalf("a provisioned box stored poll registering poll: http=%d, want 200 (%s)", code, body)
	}
	if _, mode := urlAndModeOf(t, conn, id); mode != "poll" {
		t.Errorf("stored-poll provisioned row: mode=%q after its own poll register, want poll", mode)
	}
}

// The 409 is about the CALLER. It must not stamp last_register_failure_at on
// the workspace, or a looping rogue client could flap a healthy box to
// degraded via the heartbeat failure window.
func TestIntegration_Register_PollRefusal_DoesNotStampRegisterFailure(t *testing.T) {
	conn := integrationDB_DormantRegister(t)
	setupTestRedis(t)
	h := &RegistryHandler{broadcaster: newTestBroadcaster()}

	id := uuid.NewString()
	seedRegisteredWorkspace(t, conn, id, "claude-code", "enteros-ws-"+id[:12], platformHostedURL, "push")

	// Authenticated caller (a live instance token for this workspace), so the
	// deferred stamp's authOK gate is genuinely passed.
	tok, err := wsauth.IssueToken(context.Background(), conn, id)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := fmt.Sprintf(`{"id":%q,"delivery_mode":"poll","agent_card":{"name":"external-peer","version":"1.0"}}`, id)
	c.Request = httptest.NewRequest("POST", "/registry/register", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Authorization", "Bearer "+tok)
	h.Register(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("authenticated poll register on a provisioned push workspace: http=%d, want 409 (%s)", w.Code, w.Body.String())
	}
	var stamped sql.NullTime
	if err := conn.QueryRowContext(context.Background(),
		`SELECT last_register_failure_at FROM workspaces WHERE id = $1`, id).Scan(&stamped); err != nil {
		t.Fatalf("read last_register_failure_at: %v", err)
	}
	if stamped.Valid {
		t.Errorf("refused poll register stamped last_register_failure_at=%v on the victim workspace; want NULL", stamped.Time)
	}
}

// A self-host EXTERNAL peer that once registered push at a localhost port is
// not docker-provisioned; it must still be able to switch to poll.
func TestIntegration_Register_PollOnExternalLocalhostPeer_SwitchesToPoll(t *testing.T) {
	conn := integrationDB_DormantRegister(t)
	setupTestRedis(t)
	h := &RegistryHandler{broadcaster: newTestBroadcaster()}

	id := uuid.NewString()
	seedRegisteredWorkspace(t, conn, id, "external", "", "http://localhost:41236", "push")

	code, body := callPollRegister(t, h, id, "")

	if code != http.StatusOK {
		t.Fatalf("external localhost peer poll register: http=%d, want 200 (%s)", code, body)
	}
	if _, mode := urlAndModeOf(t, conn, id); mode != "poll" {
		t.Errorf("external localhost peer: mode=%q, want poll", mode)
	}
}

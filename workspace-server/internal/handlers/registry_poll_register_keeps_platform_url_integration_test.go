//go:build integration
// +build integration

// registry_poll_register_keeps_platform_url_integration_test.go — REAL Postgres
// proof that a URL-less /registry/register cannot detach a PLATFORM-HOSTED
// workspace from its platform URL.
//
// Live 2026-11-13 (reno-stars, workspace ca17a092 — claude-code, provisioned by
// the platform at http://enteros-ws-ca17a092fecb.<ns>.svc.cluster.local:8000):
// an external client holding that workspace's token POSTed /registry/register
// with delivery_mode=poll and no url (04:50–06:06Z, from the ingress, not the
// box). The upsert wrote url=NULL and delivery_mode='poll' over the box's row.
// From then until the box next re-registered (a restart at 09:31Z), every
// forward that needs the box's URL — schedules, chat files — answered 422 "no
// callback URL … requires push-mode", although the box was up and serving at
// the URL the platform itself had given it. The 201 the tenant ADMIN token got
// on the same POST /workspaces/:id/schedules was AFTER that restart, not a
// difference between the two credentials: the 422 judges the target row, and
// the row had been rewritten.
//
// The register comment already promised "the CASE below also preserves an
// existing provisioner-set URL … for hybrid setups where a workspace was
// previously push and is being re-registered as poll" — but the CASE only kept
// localhost/127.0.0.1 host-port URLs. This pins the promise for platform-hosted
// rows (any runtime outside externalLikeRuntimes), and pins that external-like
// rows keep today's push→poll switch.
//
// Run with:
//
//	INTEGRATION_DB_URL="postgres://postgres:test@HOST:5432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ \
//	  -run Integration_Register_PollWithoutURL -v

package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const platformHostedURL = "http://enteros-ws-abc.enteros-test.svc.cluster.local:8000"

func seedRegisteredWorkspace(t *testing.T, conn *sql.DB, id, runtime, url, mode string) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO workspaces (id, name, status, kind, runtime, url, delivery_mode)
		 VALUES ($1, $2, 'online'::workspace_status, 'workspace', $3, $4, $5)`,
		id, "integ-dormant-pollkeep-"+id, runtime, url, mode); err != nil {
		t.Fatalf("seed workspace (runtime=%s): %v", runtime, err)
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

// callPollRegisterWithoutURL is the exact shape the external client sent:
// delivery_mode=poll, no top-level url, an agent_card with no url.
func callPollRegisterWithoutURL(t *testing.T, h *RegistryHandler, id string) int {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := fmt.Sprintf(`{"id":%q,"delivery_mode":"poll","agent_card":{"name":"external-peer","version":"1.0"}}`, id)
	c.Request = httptest.NewRequest("POST", "/registry/register", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Register(c)
	return w.Code
}

func TestIntegration_Register_PollWithoutURL_KeepsPlatformHostedURL(t *testing.T) {
	// Reuses the dormant-register harness (its cleanup matches 'integ-dormant-%').
	conn := integrationDB_DormantRegister(t)
	setupTestRedis(t)
	h := &RegistryHandler{broadcaster: newTestBroadcaster()}

	id := uuid.NewString()
	seedRegisteredWorkspace(t, conn, id, "claude-code", platformHostedURL, "push")

	code := callPollRegisterWithoutURL(t, h, id)

	url, mode := urlAndModeOf(t, conn, id)
	if url != platformHostedURL {
		t.Errorf("URL-less poll register CLEARED a platform-hosted workspace's URL: url=%q, want %q [register http=%d] — "+
			"every URL forward (schedules, chat files) now 422s although the box is serving", url, platformHostedURL, code)
	}
	if mode != "push" {
		t.Errorf("URL-less poll register flipped a platform-hosted workspace to delivery_mode=%q, want push [register http=%d] — "+
			"A2A for the box would be parked for a poller the box is not running", mode, code)
	}
}

func TestIntegration_Register_PollWithoutURL_ExternalStillSwitchesToPoll(t *testing.T) {
	// Control: an external-like runtime is exactly the BYO-compute case poll
	// mode exists for. A laptop that used to be push (tunnel URL) and now
	// registers poll must still drop the stale URL and become poll.
	conn := integrationDB_DormantRegister(t)
	setupTestRedis(t)
	h := &RegistryHandler{broadcaster: newTestBroadcaster()}

	id := uuid.NewString()
	seedRegisteredWorkspace(t, conn, id, "external", "https://old-tunnel.example.com", "push")

	code := callPollRegisterWithoutURL(t, h, id)

	url, mode := urlAndModeOf(t, conn, id)
	if url != "" || mode != "poll" {
		t.Errorf("external workspace poll register: url=%q mode=%q, want url cleared and mode=poll [register http=%d]", url, mode, code)
	}
}

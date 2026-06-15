// Package handlers — transcript proxy.
//
// GET /workspaces/:id/transcript proxies to the workspace's own
// /transcript endpoint, which surfaces the live agent session log
// (claude-code reads ~/.claude/projects/<cwd>/<session>.jsonl). Other
// runtimes return supported:false.
//
// Why this lives in the platform: docker exec works for local dev but
// not for remote (Phase 30) workspaces on Fly Machines. The platform's
// network proxy is the only path that scales to both.
package handlers

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

// TranscriptHandler proxies /workspaces/:id/transcript to the workspace agent.
type TranscriptHandler struct {
	httpClient *http.Client
}

func NewTranscriptHandler() *TranscriptHandler {
	return &TranscriptHandler{
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// Get handles GET /workspaces/:id/transcript?since=N&limit=N.
//
// Looks up the workspace's URL, mints a workspace-scoped bearer token,
// forwards the GET, and streams the response back. Caps payload at 1MB
// to keep a runaway transcript from saturating canvas.
func (h *TranscriptHandler) Get(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var workspaceURL string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT agent_card->>'url' FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&workspaceURL); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if workspaceURL == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace not registered (no URL on file)"})
		return
	}

	// workspaceURL comes from agent_card which is attacker-writable via
	// /registry/register — treat it as untrusted and validate before the
	// outbound HTTP call to prevent SSRF (issue #272 / #2130).
	// isSafeURL is the production policy used by A2A/MCP dispatch; it
	// includes DNS resolution checks and blocks loopback/private/metadata
	// targets that validateWorkspaceURL previously allowed.
	if err := isSafeURL(workspaceURL); err != nil {
		log.Printf("transcript: workspace %s URL rejected: %v", workspaceID, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace URL not allowed"})
		return
	}

	target, err := url.Parse(workspaceURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace URL"})
		return
	}
	target.Path = "/transcript"

	// Don't forward the raw query string — an attacker-controlled caller
	// could smuggle params the upstream endpoint didn't intend to expose.
	// Allowlist the two params the transcript endpoint actually uses.
	q := url.Values{}
	if since := c.Query("since"); since != "" {
		q.Set("since", since)
	}
	if limit := c.Query("limit"); limit != "" {
		q.Set("limit", limit)
	}
	target.RawQuery = q.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", target.String(), nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build request"})
		return
	}

	// Forward the caller's bearer token so the workspace /transcript handler
	// (secured by #287/#328) can authenticate the proxied request.
	// WorkspaceAuth has already validated the token above — forwarding is safe.
	// Without this the workspace fails-closed (401) and the transcript feature
	// is non-functional in production. Fixes the QA-2026-04-16 finding.
	if auth := c.GetHeader("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		// Log the real error server-side (includes the target URL), but
		// don't leak it to the caller — that would reveal internal host
		// names / IPs reachable from the platform.
		log.Printf("transcript: workspace %s unreachable: %v", workspaceID, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "workspace unreachable"})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Cap at 1 MB so a giant transcript doesn't melt the canvas.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read workspace response"})
		return
	}
	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}


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
	"net"
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
	// SSRF hardening (Researcher #2132 / RC 103771). The transcript
	// proxy is a known SSRF surface: the front-door isSafeURL check
	// happens BEFORE the outbound call (workspaceURL came from
	// agent_card in the DB, which is attacker-writable via
	// /registry/register), but two vectors remain after that gate:
	//
	//   (1) DNS-rebinding TOCTOU: the front-door check resolves the
	//       hostname, sees a public IP, returns success. A split-ms
	//       later the DNS TTL flips and the second resolution (at
	//       actual TCP-dial time) returns 169.254.169.254 (IMDS). The
	//       dialer now connects to the metadata endpoint and forwards
	//       the caller's Authorization bearer to it.
	//
	//   (2) Unvalidated redirects: the upstream returns 302 →
	//       somewhere.evil.example. The default http.Client follows
	//       the redirect with the same Authorization header (per RFC
	//       7235; the bearer is forwarded to the redirect target). If
	//       the redirect target is a private IP (IMDS, internal
	//       services), the bearer leaks.
	//
	// Both vectors are closed by a single mechanism: net.Dialer.Control
	// inspects the POST-resolution net.IP on EVERY dial (catches
	// (1) re-binding AND (2) redirect-target rebinding) + CheckRedirect
	// returns http.ErrUseLastResponse (catches the rest of (2) by
	// refusing to follow). The dialer's Control is called after
	// getaddrinfo, so it sees the IP the dialer is about to use,
	// not the IP the front-door saw.
	return &TranscriptHandler{
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
			// Disable redirects entirely: the upstream 302 response is
			// surfaced to the caller (with its body / location) but
			// the client does NOT chase it. This kills the redirect
			// surface + the token-leak (a redirect target cannot
			// receive the caller's Authorization header when no
			// chase happens). The dial-time IP guard (below) is the
			// belt-and-suspenders for any future code that does
			// re-enable redirects.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
			// Dial-time IP guard. net.Dialer.Control runs after
			// getaddrinfo but before the TCP SYN, on every dial
			// (initial + every connection in a redirect chain). The
			// isSafeURL helper reuses the same allow/deny policy as
			// the front-door (loopback / private / metadata /
			// link-local blocked in production; the SaaS-mode
			// private-range relaxation is honored here too so a
			// intra-VPC workspace target still works).
			//
			// The Control callback runs in the dialer's goroutine, so
			// it must not block. isSafeURL is in-memory + fast.
			Transport: &http.Transport{
				DialContext:           safeDialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   5 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}
}

// safeDialContext is the dial-context function used by the
// transcript proxy. It wraps net.Dialer's DialContext with an
// isSafeURL post-resolution check on the IP the dialer is about to
// use (closes the DNS-rebinding TOCTOU + redirect-target rebinding
// vectors in #2132 / RC 103771).
//
// The function signature matches net.Dialer's DialContext so it can
// be passed via http.Transport.DialContext. The IP guard runs in
// the dialer's goroutine, so it must not block.
func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	// POST-resolution IP guard. The conn.LocalAddr is the local
	// side; the RemoteAddr is the resolved IP we just dialed. We
	// validate the RemoteAddr's IP against isSafeURL's policy. Note
	// the host:port form; the IP is the first component.
	remote := conn.RemoteAddr().String()
	if host, _, splitErr := net.SplitHostPort(remote); splitErr == nil {
		if ip := net.ParseIP(host); ip != nil {
			// Reuse the SSRF policy. isSafeURL takes a URL but
			// the policy is purely IP-based; we construct a
			// throwaway URL to reuse the helper.
			if err := isSafeURL("http://" + ip.String() + "/"); err != nil {
				_ = conn.Close()
				return nil, &ssrfDialError{ip: ip, reason: err}
			}
		}
	}
	return conn, nil
}

// ssrfDialError is the error type returned by safeDialContext when
// the post-resolution IP fails the isSafeURL policy. The error
// message includes the IP and the policy reason so the platform
// log surfaces the SSRF attempt (the workspace agent_card.url
// embedding attack in #2132 / RC 103771).
type ssrfDialError struct {
	ip     net.IP
	reason error
}

func (e *ssrfDialError) Error() string {
	return "ssrf: dial-time IP " + e.ip.String() + " blocked: " + e.reason.Error()
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


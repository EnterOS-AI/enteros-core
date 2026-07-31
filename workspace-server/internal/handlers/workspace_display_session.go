package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"github.com/gin-gonic/gin"
)

const workspaceDisplaySessionTimeout = 12 * time.Hour
const displaySessionTokenProtocolPrefix = "molecule-display-token."

var displayForward = realDisplayForward

const desktopNoVNCPort = "6080"

// desktopDisplayForward dials a Docker/k8s desktop SIDECAR's noVNC listener
// directly over the per-workspace network — the §13 display re-home. Unlike
// realDisplayForward (EC2 EIC SSH tunnel) there is NO tunnel: the sidecar is a
// name on the workspace network, so the reverse proxy is handed an
// http://<sidecarHost> target directly (sidecarHost e.g. "wsdesk-<id>:6080").
func desktopDisplayForward(_ context.Context, sidecarHost string, fn func(target *url.URL) error) error {
	return fn(&url.URL{Scheme: "http", Host: sidecarHost})
}

// DisplaySession proxies noVNC/websockify requests for a display-enabled EC2
// workspace through the existing EIC SSH path. The EC2 :6080 listener stays
// private to the VPC; the browser only sees this same-origin route.
func (h *WorkspaceHandler) DisplaySession(c *gin.Context) {
	workspaceID := c.Param("id")
	display, instanceID, err := loadWorkspaceDisplaySessionTarget(c.Request.Context(), workspaceID)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return
		}
		log.Printf("DisplaySession: load target for %s failed: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load display session"})
		return
	}
	// Display re-home (§13): a deployment with a desktop-sidecar backend wired
	// reaches the sidecar's noVNC directly over the per-workspace network (no
	// EC2 instance_id / EIC tunnel). EC2 deployments keep the instance_id gate.
	// (Per-workspace sidecar-vs-EC2 selection from the lifecycle table is a
	// follow-up; the wired-backend flag is the deployment-level discriminator.)
	useSidecar := h.sidecarProv != nil
	// Display is enabled when the workspace declares a legacy display mode
	// (EC2/DCV) OR the sidecar computer-use backend is wired — the latter needs no
	// per-workspace compute.display opt-in (see the Display availability handler).
	if (display.Mode == "" || display.Mode == "none") && !useSidecar {
		c.JSON(http.StatusNotFound, gin.H{"error": "display not enabled"})
		return
	}
	if !useSidecar && instanceID == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "display session unavailable"})
		return
	}
	// Bring the sidecar desktop up if it was scaled to zero, so a human opening
	// the viewer connects even when no agent has started it. Routed through the
	// gateway's EnsureRunning (not StartDesktop directly) so the 'running'
	// lifecycle state is recorded and the idle sweeper can still reap it.
	// Best-effort: if it errors we still attempt the proxy (it may already be up);
	// a slow cold start surfaces as a proxy retry the frontend reconnects through.
	if useSidecar && h.desktopGateway != nil {
		if _, err := h.desktopGateway.EnsureRunning(c.Request.Context(), workspaceID); err != nil {
			log.Printf("DisplaySession: ensure desktop running for %s failed (best-effort): %v", workspaceID, err)
		}
	}

	proxyPath := c.Param("proxyPath")
	if proxyPath != "/websockify" {
		c.JSON(http.StatusNotFound, gin.H{"error": "display session path not found"})
		return
	}
	lock, found, err := h.loadActiveDisplayControl(c, workspaceID)
	if err != nil {
		log.Printf("DisplaySession: load active lock for %s failed: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load display control"})
		return
	}
	// View/control split (§8): accept EITHER a control token bound to the current
	// lock holder OR a workspace-bound VIEW-ONLY token. Sight is never arbitrated,
	// so a viewer who does not hold the lock can still watch; only /input is gated
	// on holding control (in the gateway). (Reviewer N1: previously only the
	// control token was accepted, so the viewer token was dead and this claim was
	// false — now both are honored.)
	tok := displaySessionTokenFromRequest(c.Request)
	controlOK := found && validateDisplaySessionToken(tok, workspaceID, lock.ControlledBy, lock.ExpiresAt)
	viewOK := validateDisplayViewerToken(tok, workspaceID)
	if !controlOK && !viewOK {
		c.JSON(http.StatusForbidden, gin.H{"error": "display control or view token required"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), workspaceDisplaySessionTimeout)
	defer cancel()
	// Count this human viewer for the desktop's whole session so the idle sweeper
	// does not reap the sidecar out from under a watching human (the agent-activity
	// timer is usually already stale when a human takes over). Balanced on return,
	// when the websocket has closed.
	if useSidecar && h.desktopGateway != nil {
		h.desktopGateway.ViewerConnected(ctx, workspaceID)
		defer h.desktopGateway.ViewerDisconnected(ctx, workspaceID)
	}
	fwd := func(fn func(target *url.URL) error) error {
		if useSidecar {
			sidecarHost := provisioner.DesktopContainerName(workspaceID) + ":" + desktopNoVNCPort
			// SSRF allowlist (§13): the host is server-built from the authed
			// route's workspace id, but pin its shape so a malformed id can never
			// steer the noVNC reverse proxy to an arbitrary host.
			if err := provisioner.ValidateSidecarTarget(sidecarHost); err != nil {
				return err
			}
			return desktopDisplayForward(ctx, sidecarHost, fn)
		}
		return displayForward(ctx, instanceID, fn)
	}
	err = fwd(func(target *url.URL) error {
		proxy := newDisplaySessionReverseProxy(target)
		proxy.ServeHTTP(c.Writer, c.Request.WithContext(ctx))
		return nil
	})
	if err != nil {
		log.Printf("DisplaySession: proxy for %s instance=%s failed: %v", workspaceID, instanceID, err)
		if !c.Writer.Written() {
			c.JSON(http.StatusBadGateway, gin.H{"error": "display session proxy failed"})
		}
	}
}

func loadWorkspaceDisplaySessionTarget(ctx context.Context, workspaceID string) (models.WorkspaceComputeDisplay, string, error) {
	var raw, instanceID string
	err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(compute, '{}'::jsonb), COALESCE(instance_id, '') FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&raw, &instanceID)
	if err != nil {
		return models.WorkspaceComputeDisplay{}, "", err
	}
	var compute models.WorkspaceCompute
	if raw != "" && raw != "{}" {
		if err := json.Unmarshal([]byte(raw), &compute); err != nil {
			return models.WorkspaceComputeDisplay{}, "", fmt.Errorf("invalid compute JSON: %w", err)
		}
		if err := validateWorkspaceDisplayConfig(compute.Display); err != nil {
			return models.WorkspaceComputeDisplay{}, "", err
		}
	}
	return compute.Display, instanceID, nil
}

func newDisplaySessionReverseProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = "/websockify"
			req.URL.RawPath = ""
			req.URL.RawQuery = ""
			req.Host = target.Host
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			req.Header.Set("Sec-WebSocket-Protocol", "binary")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("DisplaySession: upstream proxy error: %v", err)
			http.Error(w, "display session proxy failed", http.StatusBadGateway)
		},
	}
}

func displaySessionTokenFromRequest(r *http.Request) string {
	for _, part := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		protocol := strings.TrimSpace(part)
		if strings.HasPrefix(protocol, displaySessionTokenProtocolPrefix) {
			return strings.TrimPrefix(protocol, displaySessionTokenProtocolPrefix)
		}
	}
	return ""
}

func realDisplayForward(ctx context.Context, instanceID string, fn func(target *url.URL) error) error {
	if instanceID == "" {
		return fmt.Errorf("workspace has no instance_id")
	}
	return withEICTunnel(ctx, instanceID, func(s eicSSHSession) error {
		localPort, err := pickFreePort()
		if err != nil {
			return fmt.Errorf("pick display forward port: %w", err)
		}
		cmd := exec.CommandContext(ctx, "ssh",
			"-i", s.keyPath,
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "LogLevel=ERROR",
			"-o", "ExitOnForwardFailure=yes",
			"-N",
			"-L", fmt.Sprintf("%d:127.0.0.1:6080", localPort),
			"-p", fmt.Sprintf("%d", s.localPort),
			fmt.Sprintf("%s@127.0.0.1", s.osUser),
		)
		cmd.Env = os.Environ()
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("display forward start: %w", err)
		}
		defer func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		}()
		if err := waitForPort(ctx, "127.0.0.1", localPort, 10*time.Second); err != nil {
			return fmt.Errorf("display forward never listened: %w", err)
		}
		return fn(&url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", localPort)})
	})
}

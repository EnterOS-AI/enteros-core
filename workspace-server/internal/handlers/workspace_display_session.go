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
	if display.Mode == "" || display.Mode == "none" {
		c.JSON(http.StatusNotFound, gin.H{"error": "display not enabled"})
		return
	}
	// Display re-home (§13): a deployment with a desktop-sidecar backend wired
	// reaches the sidecar's noVNC directly over the per-workspace network (no
	// EC2 instance_id / EIC tunnel). EC2 deployments keep the instance_id gate.
	// (Per-workspace sidecar-vs-EC2 selection from the lifecycle table is a
	// follow-up; the wired-backend flag is the deployment-level discriminator.)
	useSidecar := h.sidecarProv != nil
	if !useSidecar && instanceID == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "display session unavailable"})
		return
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
	if !found || !validateDisplaySessionToken(displaySessionTokenFromRequest(c.Request), workspaceID, lock.ControlledBy, lock.ExpiresAt) {
		c.JSON(http.StatusForbidden, gin.H{"error": "display control required"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), workspaceDisplaySessionTimeout)
	defer cancel()
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

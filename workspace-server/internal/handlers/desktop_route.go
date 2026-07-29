package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/desktopgateway"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"github.com/gin-gonic/gin"
)

// desktopGateway is the enforcement gateway (internal/desktopgateway) the
// desktop HTTP route proxies to. Local interface so the field type doesn't
// hard-couple the handlers package to the concrete gateway.
//
// DESIGN (decision B, plugin-driven): this authenticated HTTP route — NOT a
// platform MCP tool — is the seam the IN-CONTAINER agent desktop tool
// (molecule-ai-workspace-runtime a2a_tools_desktop.py, later a native kind:mcp
// plugin) calls. The platform layer stays a gateway, so the agent surface
// remains in the runtime and stays plugin-extractable — the static mcp.go
// bridge can never host a plugin-contributed tool.
type desktopGateway interface {
	Screenshot(ctx context.Context, workspaceID string) ([]byte, error)
	Input(ctx context.Context, workspaceID string, action json.RawMessage) error
}

// SetDesktopGateway wires the desktop enforcement gateway. Unset (nil) makes the
// desktop routes report the feature unavailable — the per-tier gate.
func (h *WorkspaceHandler) SetDesktopGateway(g desktopGateway) { h.desktopGateway = g }

// DesktopScreenshot handles GET /workspaces/:id/desktop/screenshot — the agent's
// eyes. Sight is not lock-gated (§8); the gateway scales the desktop from zero
// and records activity.
func (h *WorkspaceHandler) DesktopScreenshot(c *gin.Context) {
	if h.desktopGateway == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "desktop not available"})
		return
	}
	png, err := h.desktopGateway.Screenshot(c.Request.Context(), c.Param("id"))
	if err != nil {
		if errors.Is(err, provisioner.ErrDesktopBackendUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "desktop not available"})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "screenshot failed"})
		return
	}
	c.Data(http.StatusOK, "image/png", png)
}

// DesktopControlStatus handles GET /workspaces/:id/desktop/control — reports
// whether a HUMAN currently holds the display-control lock. The agent's
// desktop_wait_for_control tool polls this to pause while a human is driving and
// resume when control frees (§8 arbitration). Workspace-token gated (same wsAuth
// group as screenshot/input) so the agent can call it directly; it reads only
// the lock, never touches the sidecar, so it works regardless of scale state.
func (h *WorkspaceHandler) DesktopControlStatus(c *gin.Context) {
	if h.desktopGateway == nil {
		// Mirror DesktopScreenshot/DesktopInput: when the feature is unavailable
		// the status endpoint must agree with /input (503) instead of claiming the
		// agent can drive — otherwise desktop_wait_for_control believes it may
		// control and every subsequent /input returns 503.
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "desktop not available"})
		return
	}
	lock, found, err := h.loadActiveDisplayControl(c, c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load display control"})
		return
	}
	humanInControl := found && lock.Controller == "user"
	c.JSON(http.StatusOK, gin.H{
		"human_in_control":  humanInControl,
		"agent_can_control": !humanInControl,
	})
}

// DesktopInput handles POST /workspaces/:id/desktop/input — the agent's hands.
// The gateway is FAIL-CLOSED on the control lock (§8): a human holding control
// yields 409 so the agent pauses instead of fighting for the cursor.
func (h *WorkspaceHandler) DesktopInput(c *gin.Context) {
	if h.desktopGateway == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "desktop not available"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body"})
		return
	}
	if err := h.desktopGateway.Input(c.Request.Context(), c.Param("id"), json.RawMessage(body)); err != nil {
		switch {
		case errors.Is(err, desktopgateway.ErrHumanInControl):
			c.JSON(http.StatusConflict, gin.H{"error": "human in control"})
		case errors.Is(err, provisioner.ErrDesktopBackendUnavailable):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "desktop not available"})
		default:
			c.JSON(http.StatusBadGateway, gin.H{"error": "input failed"})
		}
		return
	}
	c.Status(http.StatusNoContent)
}

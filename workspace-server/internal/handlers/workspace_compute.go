package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/models"
	"github.com/gin-gonic/gin"
)

const (
	workspaceComputeDiskFloorGB   = 30
	workspaceComputeDiskCeilingGB = 500
	workspaceDisplayMinWidth      = 800
	workspaceDisplayMaxWidth      = 3840
	workspaceDisplayMinHeight     = 600
	workspaceDisplayMaxHeight     = 2160
)

type workspaceDisplayResponse struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Status    string `json:"status,omitempty"`
}

var workspaceComputeInstanceAllowlist = map[string]struct{}{
	"t3.medium":  {},
	"t3.large":   {},
	"t3.xlarge":  {},
	"t3.2xlarge": {},
	"m6i.large":  {},
	"m6i.xlarge": {},
	"c6i.xlarge": {},
}

func validateWorkspaceCompute(compute models.WorkspaceCompute) error {
	if compute.InstanceType != "" {
		if _, ok := workspaceComputeInstanceAllowlist[compute.InstanceType]; !ok {
			return fmt.Errorf("unsupported compute.instance_type")
		}
	}
	if compute.Volume.RootGB != 0 {
		if compute.Volume.RootGB < workspaceComputeDiskFloorGB || compute.Volume.RootGB > workspaceComputeDiskCeilingGB {
			return fmt.Errorf("compute.volume.root_gb must be between %d and %d", workspaceComputeDiskFloorGB, workspaceComputeDiskCeilingGB)
		}
	}
	switch compute.Display.Mode {
	case "", "none", "desktop-control", "gpu-desktop-control":
	default:
		return fmt.Errorf("unsupported compute.display.mode")
	}
	switch compute.Display.Protocol {
	case "", "dcv", "novnc":
	default:
		return fmt.Errorf("unsupported compute.display.protocol")
	}
	if err := validateWorkspaceDisplayDimensions(compute.Display.Width, compute.Display.Height); err != nil {
		return err
	}
	return nil
}

func validateWorkspaceDisplayConfig(display models.WorkspaceComputeDisplay) error {
	switch display.Mode {
	case "", "none", "desktop-control", "gpu-desktop-control":
	default:
		return fmt.Errorf("unsupported compute.display.mode")
	}
	switch display.Protocol {
	case "", "dcv", "novnc":
	default:
		return fmt.Errorf("unsupported compute.display.protocol")
	}
	if err := validateWorkspaceDisplayDimensions(display.Width, display.Height); err != nil {
		return err
	}
	return nil
}

func validateWorkspaceDisplayDimensions(width, height int) error {
	if width < 0 || height < 0 {
		return fmt.Errorf("compute.display width/height must be non-negative")
	}
	if width != 0 && (width < workspaceDisplayMinWidth || width > workspaceDisplayMaxWidth) {
		return fmt.Errorf("compute.display.width must be between %d and %d", workspaceDisplayMinWidth, workspaceDisplayMaxWidth)
	}
	if height != 0 && (height < workspaceDisplayMinHeight || height > workspaceDisplayMaxHeight) {
		return fmt.Errorf("compute.display.height must be between %d and %d", workspaceDisplayMinHeight, workspaceDisplayMaxHeight)
	}
	return nil
}

func workspaceComputeIsZero(compute models.WorkspaceCompute) bool {
	return compute.InstanceType == "" &&
		compute.Volume.RootGB == 0 &&
		compute.Display.Mode == "" &&
		compute.Display.Width == 0 &&
		compute.Display.Height == 0 &&
		compute.Display.Protocol == ""
}

func workspaceComputeJSON(compute models.WorkspaceCompute) (string, error) {
	if workspaceComputeIsZero(compute) {
		return "{}", nil
	}
	out := map[string]interface{}{}
	if compute.InstanceType != "" {
		out["instance_type"] = compute.InstanceType
	}
	if compute.Volume.RootGB != 0 {
		out["volume"] = map[string]interface{}{"root_gb": compute.Volume.RootGB}
	}
	display := map[string]interface{}{}
	if compute.Display.Mode != "" {
		display["mode"] = compute.Display.Mode
	}
	if compute.Display.Width != 0 {
		display["width"] = compute.Display.Width
	}
	if compute.Display.Height != 0 {
		display["height"] = compute.Display.Height
	}
	if compute.Display.Protocol != "" {
		display["protocol"] = compute.Display.Protocol
	}
	if len(display) > 0 {
		out["display"] = display
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func withStoredCompute(ctx context.Context, workspaceID string, payload models.CreateWorkspacePayload) models.CreateWorkspacePayload {
	if !workspaceComputeIsZero(payload.Compute) || db.DB == nil {
		return payload
	}
	var raw string
	err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(compute, '{}'::jsonb) FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&raw)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("withStoredCompute: load compute for %s failed: %v", workspaceID, err)
		}
		return payload
	}
	if raw == "" || raw == "{}" {
		return payload
	}
	var compute models.WorkspaceCompute
	if err := json.Unmarshal([]byte(raw), &compute); err != nil {
		log.Printf("withStoredCompute: invalid compute JSON for %s: %v", workspaceID, err)
		return payload
	}
	if err := validateWorkspaceCompute(compute); err != nil {
		log.Printf("withStoredCompute: stored compute for %s failed validation: %v", workspaceID, err)
		return payload
	}
	payload.Compute = compute
	return payload
}

// Display handles GET /workspaces/:id/display.
func (h *WorkspaceHandler) Display(c *gin.Context) {
	workspaceID := c.Param("id")
	var raw, instanceID string
	err := db.DB.QueryRowContext(c.Request.Context(),
		`SELECT COALESCE(compute, '{}'::jsonb), COALESCE(instance_id, '') FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&raw, &instanceID)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(404, gin.H{"error": "workspace not found"})
			return
		}
		log.Printf("Display: load compute for %s failed: %v", workspaceID, err)
		c.JSON(500, gin.H{"error": "failed to load display config"})
		return
	}
	var compute models.WorkspaceCompute
	if raw != "" && raw != "{}" {
		if err := json.Unmarshal([]byte(raw), &compute); err != nil {
			log.Printf("Display: invalid compute JSON for %s: %v", workspaceID, err)
			c.JSON(500, gin.H{"error": "invalid display config"})
			return
		}
		if err := validateWorkspaceDisplayConfig(compute.Display); err != nil {
			log.Printf("Display: invalid stored compute for %s: %v", workspaceID, err)
			c.JSON(500, gin.H{"error": "invalid display config"})
			return
		}
	}
	if compute.Display.Mode == "" || compute.Display.Mode == "none" {
		c.JSON(200, workspaceDisplayResponse{
			Available: false,
			Reason:    "display_not_enabled",
		})
		return
	}
	if instanceID != "" {
		c.JSON(200, workspaceDisplayResponse{
			Available: true,
			Mode:      compute.Display.Mode,
			Protocol:  compute.Display.Protocol,
			Width:     compute.Display.Width,
			Height:    compute.Display.Height,
			Status:    "ready",
		})
		return
	}
	c.JSON(200, workspaceDisplayResponse{
		Available: false,
		Reason:    "display_session_unavailable",
		Mode:      compute.Display.Mode,
		Protocol:  compute.Display.Protocol,
		Width:     compute.Display.Width,
		Height:    compute.Display.Height,
		Status:    "not_configured",
	})
}

package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
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

// SSOT for cloud-provider + instance-type metadata (core#2489).
//
// This file is the SINGLE source of truth the workspace-server validates
// against AND the canvas Container-Config tab renders its dropdowns from (via
// GET /workspaces/:id/compute-options, see ComputeOptions below). Previously the
// canvas hardcoded a parallel copy of these lists in ContainerConfigTab.tsx; the
// two could drift so the UI offered a (provider, instance-type) the backend
// allowlist then rejected with a 400. The canvas now derives its options from
// this endpoint, so drift is impossible by construction.
//
// The ordered slices below are the canonical form. workspaceComputeInstanceAllowlist
// (the O(1) validation set) is DERIVED from them in init(), so the ordered list
// the canvas renders and the set the backend validates can never disagree.
//
// Mirrors the CP provider SSOT — keep in lock-step with the controlplane provider
// configs (Hetzner ServerType cpx*/cax*, GCP MachineType e2-*, AWS EC2
// t3*/m6i*/c6i*). TestValidateWorkspaceCompute_Provider / _InstanceTypePerProvider
// pin the sets. "" provider = AWS default.

// workspaceComputeProvidersOrdered is the canonical provider order (AWS first =
// default). The canvas renders the provider dropdown in this order.
var workspaceComputeProvidersOrdered = []string{"aws", "hetzner", "gcp"}

// workspaceComputeInstanceTypesOrdered lists each provider's machine sizes in the
// order the canvas should render them. An AWS t3.* is meaningless on Hetzner, and
// vice-versa, so the set is provider-scoped.
var workspaceComputeInstanceTypesOrdered = map[string][]string{
	"aws": {
		"t3.medium", "t3.large", "t3.xlarge", "t3.2xlarge",
		"m6i.large", "m6i.xlarge", "c6i.xlarge",
	},
	"hetzner": {
		"cpx11", "cpx21", "cpx31", "cpx41", "cpx51",
		"cax11", "cax21", "cax31", "cax41",
	},
	"gcp": {
		"e2-small", "e2-medium",
		"e2-standard-2", "e2-standard-4", "e2-standard-8",
	},
}

// workspaceComputeDefaultInstanceByProvider is the per-provider default machine
// size the canvas pre-selects when switching providers (an AWS t3.* is invalid on
// Hetzner, so the switch resets to the new provider's default).
var workspaceComputeDefaultInstanceByProvider = map[string]string{
	"aws":     "t3.medium",
	"hetzner": "cpx31",
	"gcp":     "e2-standard-2",
}

// workspaceComputeInstanceAllowlist is the O(1) validation set, keyed by cloud
// provider. DERIVED from workspaceComputeInstanceTypesOrdered in init() so the
// ordered list (what the canvas renders) and the set (what the backend validates)
// stay in lock-step — you cannot add an instance type to one without the other.
var workspaceComputeInstanceAllowlist = map[string]map[string]struct{}{}

func init() {
	for provider, types := range workspaceComputeInstanceTypesOrdered {
		set := make(map[string]struct{}, len(types))
		for _, t := range types {
			set[t] = struct{}{}
		}
		workspaceComputeInstanceAllowlist[provider] = set
	}
}

// normalizeCloudProvider maps "" → "aws" so the in-place switch comparison
// treats the default and an explicit "aws" as the same cloud (no spurious switch).
func normalizeCloudProvider(p string) string {
	if p == "" {
		return "aws"
	}
	return p
}

// instanceTypeAllowedForProvider reports whether instanceType is valid for the
// given provider ("" → aws). Empty instanceType is always allowed (CP defaults).
func instanceTypeAllowedForProvider(provider, instanceType string) bool {
	if instanceType == "" {
		return true
	}
	p := provider
	if p == "" {
		p = "aws"
	}
	set, ok := workspaceComputeInstanceAllowlist[p]
	if !ok {
		return false
	}
	_, ok = set[instanceType]
	return ok
}

// workspaceComputeProviderAllowlist mirrors the controlplane cloud-provider SSOT
// (controlplane internal/cloudprovider.Supported = {aws, hetzner, gcp}).
// ws-server lives in a different repo and cannot import that package, so this is
// a DELIBERATE mirror; TestValidateWorkspaceCompute_Provider pins the exact set
// and this doc-comment names the SSOT, so a CP-side change forces a matching
// change here (and the CP itself fail-closes an unwired provider with a 422).
// "" = default (AWS) and is always accepted. This is the gate the switch-provider
// flow reuses to reject a bad provider with a clean 400 before any CP round-trip.
// DERIVED from workspaceComputeProvidersOrdered (the SSOT, core#2489) in init() so
// the set the backend validates and the ordered list the canvas renders cannot
// drift.
var workspaceComputeProviderAllowlist = map[string]struct{}{}

func init() {
	for _, p := range workspaceComputeProvidersOrdered {
		workspaceComputeProviderAllowlist[p] = struct{}{}
	}
}

func validateWorkspaceCompute(compute models.WorkspaceCompute) error {
	// Provider first (so the instance-type check below can be provider-scoped).
	// "" = default (AWS). CP fail-closes an unwired provider with a 422; validating
	// here gives a clean 400 before the round-trip and is the gate reused by the
	// switch-provider flow. Mirrors the controlplane cloudprovider SSOT.
	if compute.Provider != "" {
		if _, ok := workspaceComputeProviderAllowlist[compute.Provider]; !ok {
			return fmt.Errorf("unsupported compute.provider (want aws|gcp|hetzner)")
		}
	}
	// Instance type must belong to the chosen provider (an AWS t3.* is invalid on
	// Hetzner, etc.). Empty = CP default for the provider.
	if !instanceTypeAllowedForProvider(compute.Provider, compute.InstanceType) {
		prov := compute.Provider
		if prov == "" {
			prov = "aws"
		}
		return fmt.Errorf("unsupported compute.instance_type %q for provider %q", compute.InstanceType, prov)
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
	// internal#734: the durable-data choice. CP re-validates the same enum at
	// its provision edge (IsValidDataPersistence → 400); validating here too
	// gives the user a clear workspace-server error before the CP round-trip.
	switch compute.DataPersistence {
	case "", "persist", "ephemeral":
	default:
		return fmt.Errorf("unsupported compute.data_persistence (want persist|ephemeral)")
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

type computeProviderMetadata struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	DefaultInstance string   `json:"default_instance"`
	Instances       []string `json:"instances"`
}

type computeMetadataResponse struct {
	Providers []computeProviderMetadata `json:"providers"`
}

// ComputeMetadata handles GET /compute/metadata — SSOT for cloud-provider +
// instance-type allowlists consumed by the canvas ContainerConfigTab (and any
// other client that needs to render a provider/instance selector).
// Public, no auth: the data is platform constraints, not org secrets.
func ComputeMetadata(c *gin.Context) {
	// Deterministic order so tests (and UI dropdowns) are stable.
	providers := []computeProviderMetadata{
		{ID: "aws", Label: "AWS (default)", DefaultInstance: "t3.medium", Instances: []string{
			"t3.medium", "t3.large", "t3.xlarge", "t3.2xlarge", "m6i.large", "m6i.xlarge", "c6i.xlarge",
		}},
		{ID: "gcp", Label: "GCP", DefaultInstance: "e2-standard-2", Instances: []string{
			"e2-small", "e2-medium", "e2-standard-2", "e2-standard-4", "e2-standard-8",
		}},
		{ID: "hetzner", Label: "Hetzner", DefaultInstance: "cpx31", Instances: []string{
			"cpx11", "cpx21", "cpx31", "cpx41", "cpx51", "cax11", "cax21", "cax31", "cax41",
		}},
	}
	c.JSON(200, computeMetadataResponse{Providers: providers})
}

func workspaceComputeIsZero(compute models.WorkspaceCompute) bool {
	return compute.InstanceType == "" &&
		compute.Volume.RootGB == 0 &&
		compute.Display.Mode == "" &&
		compute.Display.Width == 0 &&
		compute.Display.Height == 0 &&
		compute.Display.Protocol == "" &&
		// A provider- or persistence-only compute is NOT zero — it must
		// round-trip so GET returns those fields (canvas provider badge +
		// data-persistence selector both read them back).
		compute.Provider == "" &&
		compute.DataPersistence == ""
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
	// Cloud/compute provider + durable-data choice. These were FORWARDED to CP
	// at provision time but never serialized back here, so GET /workspaces
	// dropped them — the canvas provider badge always showed the default AWS and
	// the data-persistence selector always showed "auto". Round-trip them (still
	// omit-when-empty, so existing AWS/default rows serialize unchanged).
	if compute.Provider != "" {
		out["provider"] = compute.Provider
	}
	if compute.DataPersistence != "" {
		out["data_persistence"] = compute.DataPersistence
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

// workspaceComputeOptionsResponse is the SSOT payload the canvas Container-Config
// tab consumes to populate its provider + instance-type dropdowns (core#2489).
// It is derived entirely from the allowlist + defaults in this file, so the UI
// can never offer a (provider, instance-type) the backend then rejects.
type workspaceComputeOptionsResponse struct {
	// Providers in canonical render order (AWS first = default).
	Providers []string `json:"providers"`
	// InstanceTypes per provider, in canonical render order.
	InstanceTypes map[string][]string `json:"instanceTypes"`
	// Defaults maps each provider → its default instance type (the canvas
	// pre-selects this when switching providers).
	Defaults map[string]string `json:"defaults"`
}

// buildComputeOptions assembles the SSOT response from the allowlist + defaults.
// Pure (no DB / no gin) so it can be unit-tested directly and reused.
func buildComputeOptions() workspaceComputeOptionsResponse {
	providers := make([]string, len(workspaceComputeProvidersOrdered))
	copy(providers, workspaceComputeProvidersOrdered)

	instanceTypes := make(map[string][]string, len(workspaceComputeInstanceTypesOrdered))
	for _, p := range providers {
		src := workspaceComputeInstanceTypesOrdered[p]
		dst := make([]string, len(src))
		copy(dst, src)
		instanceTypes[p] = dst
	}

	defaults := make(map[string]string, len(workspaceComputeDefaultInstanceByProvider))
	for k, v := range workspaceComputeDefaultInstanceByProvider {
		defaults[k] = v
	}

	return workspaceComputeOptionsResponse{
		Providers:     providers,
		InstanceTypes: instanceTypes,
		Defaults:      defaults,
	}
}

// ComputeOptions handles GET /workspaces/:id/compute-options. It returns the
// cloud-provider + instance-type metadata the canvas Container-Config tab renders
// its dropdowns from — the SAME data validateWorkspaceCompute enforces (core#2489).
// Static (derived from the in-binary allowlist), so it needs no DB round-trip; the
// :id is scoped only by the WorkspaceAuth middleware on the route group.
func (h *WorkspaceHandler) ComputeOptions(c *gin.Context) {
	c.JSON(200, buildComputeOptions())
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

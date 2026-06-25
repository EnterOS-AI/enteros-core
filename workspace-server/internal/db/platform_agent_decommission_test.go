package db

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlatformAgentImage_DeadArtifactsIsGone pins the decommissioning of the
// baked molecule-platform-agent image. If any of the retired artifacts
// reappear (cherry-pick, revert, well-meaning refactor), this test fails
// closed in unit tests before review.
//
// Background: the platform-agent image (claude-code runtime base + baked
// org-management MCP at /opt/molecule-mcp-server) was retired in favor of the
// molecule-platform-mcp plugin installed on the ordinary claude-code runtime
// image. The concierge (kind=platform) now provisions on the same claude-code
// image as any other workspace; the plugin is delivered via the platform MCP
// plugin delivery contract (mcp_plugin_delivery_contract.go).
func TestPlatformAgentImage_DeadArtifactsIsGone(t *testing.T) {
	repoRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("could not resolve repo root: %v", err)
	}

	// 1. The baked Dockerfile must not return.
	bakedDockerfile := filepath.Join(repoRoot, "Dockerfile.platform-agent")
	if _, err := os.Stat(bakedDockerfile); err == nil {
		t.Errorf("baked platform-agent Dockerfile reappeared: %s. The molecule-platform-agent image is decommissioned; use the molecule-platform-mcp plugin on the ordinary claude-code runtime image instead.", bakedDockerfile)
	}

	// 2. The manifest must not pin a platform-agent workspace template.
	manifestPath := filepath.Join(repoRoot, "manifest.json")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("could not read manifest.json: %v", err)
	}
	var manifest struct {
		WorkspaceTemplates []struct {
			Name string `json:"name"`
		} `json:"workspace_templates"`
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("could not parse manifest.json: %v", err)
	}
	for _, tmpl := range manifest.WorkspaceTemplates {
		if tmpl.Name == "platform-agent" {
			t.Errorf("manifest.json workspace_templates still contains a 'platform-agent' entry. The platform-agent template is decommissioned; the concierge identity now lives in the molecule-platform-mcp plugin delivery contract.")
		}
	}

	// 3. The workspace-server publish workflow must not reference the baked image.
	publishWorkflow := filepath.Join(repoRoot, ".gitea", "workflows", "publish-workspace-server-image.yml")
	workflowData, err := os.ReadFile(publishWorkflow)
	if err != nil {
		t.Fatalf("could not read publish-workspace-server-image.yml: %v", err)
	}
	workflow := string(workflowData)
	banned := []string{
		"molecule-platform-agent",
		"publish-platform-agent",
		"promote-platform-agent-pin",
	}
	for _, s := range banned {
		if strings.Contains(workflow, s) {
			t.Errorf("publish-workspace-server-image.yml still references baked platform-agent artifact %q. The molecule-platform-agent image build is decommissioned.", s)
		}
	}
}

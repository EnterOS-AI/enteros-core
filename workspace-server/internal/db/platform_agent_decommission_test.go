package db

import (
	"encoding/json"
	"errors"
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
// Background: the platform-agent IMAGE (claude-code runtime base + baked
// org-management MCP at /opt/molecule-mcp-server) was retired. The concierge
// (kind=platform) now provisions on the ordinary runtime image like any other
// workspace, and gets its two identity halves at provision time:
//   - management MCP TOOLS via the platform MCP plugin delivery contract
//     (mcp_plugin_delivery_contract.go), and
//   - its PERSONA (system prompt) via the runtime-agnostic "platform-agent"
//     workspace TEMPLATE asset channel (manifest.json workspace_templates).
// This test pins the IMAGE decommission (no baked Dockerfile / publish workflow),
// and — since tenant-agent BUG 1 (P0) — asserts the persona TEMPLATE entry is
// PRESENT (item #2 below); the two are orthogonal (dead image vs live template).
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

	// 2. The manifest MUST pin a runtime-agnostic platform-agent workspace TEMPLATE
	//    (SSOT flipped by tenant-agent BUG 1, P0). This decommission test covers the
	//    baked platform-agent IMAGE (item #1 Dockerfile, item #3 publish workflow) —
	//    NOT the persona TEMPLATE. The template-asset persona channel is how the
	//    concierge gets its identity on the ordinary runtime image (no baked image);
	//    conciergeTemplateForRuntime stamps "platform-agent" for every runtime, and
	//    resolveTemplateIdentity fail-closes to an EMPTY identity (concierge boots
	//    with no persona) unless the manifest registers this template. So the entry
	//    MUST be present. A future decommission that removes it must also remove the
	//    conciergeTemplateForRuntime stamp — this assertion is the fail-closed pin.
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
	hasPlatformAgentTemplate := false
	for _, tmpl := range manifest.WorkspaceTemplates {
		if tmpl.Name == "platform-agent" {
			hasPlatformAgentTemplate = true
		}
	}
	if !hasPlatformAgentTemplate {
		t.Errorf("manifest.json workspace_templates is MISSING the 'platform-agent' entry. The concierge persona is delivered via the platform-agent template-asset channel; without this entry resolveTemplateIdentity fail-closes to an empty identity and the concierge boots with NO persona (tenant-agent BUG 1). Re-register it, or remove the conciergeTemplateForRuntime stamp if the persona channel truly moves.")
	}

	// 3. If the workspace-server publish workflow exists, it must not reference
	//    the baked image. #3391 retired the former ECR publisher; the current
	//    molecules-server publisher later reused this filename and is scanned
	//    below. An absent publisher also satisfies the invariant, exactly like
	//    the Dockerfile.platform-agent absence check above.
	publishWorkflow := filepath.Join(repoRoot, ".gitea", "workflows", "publish-workspace-server-image.yml")
	workflowData, err := os.ReadFile(publishWorkflow)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// No publisher file to scan; the baked image cannot be reintroduced here.
	case err != nil:
		t.Fatalf("could not read publish-workspace-server-image.yml: %v", err)
	default:
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
}

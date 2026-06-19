package handlers

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMCPPluginDeliveryContract_MatchesSSOT pins the producer side of the
// MCP-plugin delivery contract. The contract file is the SSOT shared with
// molecule-ai-workspace-template-claude-code; any change to the pinned path,
// key, producer, or consumer must be deliberate and synchronized.
func TestMCPPluginDeliveryContract_MatchesSSOT(t *testing.T) {
	c, err := LoadMCPPluginDeliveryContract()
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}

	if c.SettingsPath != "/configs/.claude/settings.json" {
		t.Errorf("settings_path = %q, want /configs/.claude/settings.json", c.SettingsPath)
	}
	if c.Key != "mcpServers" {
		t.Errorf("key = %q, want mcpServers", c.Key)
	}
	if c.EntryShape != "name->{command,args?,env?}" {
		t.Errorf("entry_shape = %q, want name->{command,args?,env?}", c.EntryShape)
	}
	if c.Producer != "MCPServerAdaptor" {
		t.Errorf("producer = %q, want MCPServerAdaptor", c.Producer)
	}
	if c.Consumer != "claude_sdk_executor._load_settings_mcp" {
		t.Errorf("consumer = %q, want claude_sdk_executor._load_settings_mcp", c.Consumer)
	}
}

// TestMCPPluginDeliveryContract_LoadableFromRepoRoot guards against a moved
// or missing contract file, which would silently break the cross-repo drift
// gate and any code that loads the contract at runtime.
func TestMCPPluginDeliveryContract_LoadableFromRepoRoot(t *testing.T) {
	if _, err := LoadMCPPluginDeliveryContract(); err != nil {
		t.Fatalf("contract must be loadable from repo root: %v", err)
	}
}

// TestMCPPluginDeliveryContract_MCPServerAdaptorWritesMcpServers asserts the
// producer side of the contract by exercising the REAL production
// MCPServerAdaptor from molecule-ai-workspace-runtime. It merges an MCP-server
// plugin's settings-fragment.json into the exact settings_path and key pinned
// by the contract. This catches real producer drift; the previous test-local
// helper that modelled the adaptor has been removed.
func TestMCPPluginDeliveryContract_MCPServerAdaptorWritesMcpServers(t *testing.T) {
	contract, err := LoadMCPPluginDeliveryContract()
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}

	runtimePath := os.Getenv("MOLECULE_WORKSPACE_RUNTIME")
	if runtimePath == "" {
		// Default sibling checkout relative to the core repo root.
		repoRoot := filepath.Join("..", "..", "..")
		sibling := filepath.Join(repoRoot, "molecule-ai-workspace-runtime")
		if _, err := os.Stat(filepath.Join(sibling, "molecule_runtime", "plugins_registry", "builtins.py")); err == nil {
			runtimePath = sibling
		}
	}

	pyScript := `
import asyncio, json, sys
from pathlib import Path
sys.path.insert(0, sys.argv[1])
from molecule_runtime.plugins_registry.builtins import MCPServerAdaptor
from molecule_runtime.plugins_registry.protocol import InstallContext

plugin_root = Path(sys.argv[2])
configs_dir = Path(sys.argv[3])
configs_dir.mkdir(parents=True, exist_ok=True)

async def main():
    ctx = InstallContext(
        configs_dir=configs_dir,
        workspace_id="test-ws",
        runtime="claude_code",
        plugin_root=plugin_root,
    )
    adaptor = MCPServerAdaptor("molecule-platform-mcp", "claude_code")
    await adaptor.install(ctx)

asyncio.run(main())
`

	configsDir := t.TempDir()
	pluginRoot := t.TempDir()

	fragment := map[string]any{
		contract.Key: map[string]any{
			"molecule-platform": map[string]any{
				"command": "molecule-mcp",
				"env": map[string]string{
					"MOLECULE_MCP_MODE": "management",
				},
			},
		},
	}
	fragmentBytes, err := json.Marshal(fragment)
	if err != nil {
		t.Fatalf("marshal fragment: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, "settings-fragment.json"), fragmentBytes, 0o644); err != nil {
		t.Fatalf("write settings-fragment.json: %v", err)
	}

	python := os.Getenv("MOLECULE_RUNTIME_PYTHON")
	if python == "" {
		python = "python3"
	}
	var pythonPath string
	if runtimePath != "" {
		pythonPath = runtimePath
	}

	cmd := exec.Command(python, "-", runtimePath, pluginRoot, configsDir)
	cmd.Env = os.Environ()
	if pythonPath != "" {
		cmd.Env = append(cmd.Env, "PYTHONPATH="+pythonPath)
	}
	cmd.Stdin = strings.NewReader(strings.TrimSpace(pyScript))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if runtimePath == "" {
			t.Skipf("molecule-ai-workspace-runtime not available (%v); skipping real-producer test", err)
		}
		t.Fatalf("run MCPServerAdaptor: %v\nstderr: %s", err, stderr.String())
	}

	rel := strings.TrimPrefix(contract.SettingsPath, "/configs/")
	settingsPath := filepath.Join(configsDir, rel)
	gotBytes, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read produced settings at %s: %v", contract.SettingsPath, err)
	}
	var got map[string]any
	if err := json.Unmarshal(gotBytes, &got); err != nil {
		t.Fatalf("parse produced settings: %v", err)
	}
	if _, ok := got[contract.Key]; !ok {
		t.Fatalf("real MCPServerAdaptor produced settings missing contract key %q", contract.Key)
	}
	mcpServers, ok := got[contract.Key].(map[string]any)
	if !ok {
		t.Fatalf("real MCPServerAdaptor produced settings %q is not an object: %T", contract.Key, got[contract.Key])
	}
	if _, ok := mcpServers["molecule-platform"]; !ok {
		t.Fatalf("real MCPServerAdaptor produced settings %q does not contain the molecule-platform entry", contract.Key)
	}
}

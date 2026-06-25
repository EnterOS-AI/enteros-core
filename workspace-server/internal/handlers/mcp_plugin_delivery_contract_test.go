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

	// MatchesSSOT asserts the scalar fields AND the #3159 extensions: the
	// MCP-wiring PORT symbol names (port.hook/impl/present_probe/dispatch/
	// resolver_default) and the per-runtime native delivery surfaces
	// (claude_code/codex implemented, gemini/hermes declared-but-todo). A drift
	// in any of these — e.g. the PORT being collapsed back into a hard-coded
	// Claude write, or a runtime silently dropping out — fails here.
	if diffs := c.MatchesSSOT(); len(diffs) > 0 {
		for _, d := range diffs {
			t.Errorf("contract SSOT drift: %s", d)
		}
	}
}

// TestSSOT_DegradeGateToolDerivesFromContract enforces that the online/degraded
// gate's expected platform tool id is DERIVED from the contract's
// mcp_server_name (the SSOT), not an independent hardcode. If the contract's
// server name changes (or the constant drifts), this fails — preventing the
// class of bug where the gate looked for mcp__molecule-platform__create_workspace
// while the runtime (mcp_servers.yaml name: platform) emitted
// mcp__platform__create_workspace, marking every concierge degraded.
func TestSSOT_DegradeGateToolDerivesFromContract(t *testing.T) {
	c, err := LoadMCPPluginDeliveryContract()
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	if c.MCPServerName == "" {
		t.Fatal("contract mcp_server_name is empty — SSOT for the platform MCP tool prefix")
	}
	if c.RequiredTool == "" {
		t.Fatal("contract required_tool is empty — SSOT for the management MCP's required tool verb")
	}
	// Derive the FULL id entirely from the contract (server name + required tool
	// verb) — no hardcoded "create_workspace" here. This closes the SSOT gap: the
	// verb is now contract-pinned, not re-spelled in this test.
	want := "mcp__" + c.MCPServerName + "__" + c.RequiredTool
	if conciergePlatformMCPCreateWorkspaceTool != want {
		t.Errorf("SSOT drift: conciergePlatformMCPCreateWorkspaceTool = %q, but contract implies %q (mcp__%s__%s).\n"+
			"The degraded gate must look for the tool id the runtime actually emits (mcp__<server>__<required_tool>).",
			conciergePlatformMCPCreateWorkspaceTool, want, c.MCPServerName, c.RequiredTool)
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

// TestMCPPluginDeliveryContract_MCPServerAdaptorRoutesThroughPort asserts the
// producer side of the contract by exercising the REAL production
// MCPServerAdaptor from molecule-ai-workspace-runtime. The runtime#3159 PORT is
// the delivery seam: the adaptor must route the molecule-platform descriptor
// through ctx.register_mcp_server and must NOT write /configs/.claude/settings.json
// directly (the legacy path that bypasses the PORT).
func TestMCPPluginDeliveryContract_MCPServerAdaptorRoutesThroughPort(t *testing.T) {
	contract, err := LoadMCPPluginDeliveryContract()
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}

	// The python harness installs the REAL MCPServerAdaptor and binds a SPY for
	// ctx.register_mcp_server. The spy ONLY records every (name, spec) the adaptor
	// routes through the PORT; it deliberately does NOT render, so a direct
	// .claude/settings.json write (the #3159 regression) would leave the recorder
	// empty AND create the legacy file. The harness emits recorded calls as a
	// JSON line `RECORDER=[...]` on stdout.
	pyScript := `
import asyncio, json, sys
from pathlib import Path
sys.path.insert(0, sys.argv[1])
from molecule_runtime.plugins_registry.builtins import MCPServerAdaptor
from molecule_runtime.plugins_registry.protocol import InstallContext

plugin_root = Path(sys.argv[2])
configs_dir = Path(sys.argv[3])
configs_dir.mkdir(parents=True, exist_ok=True)

recorded = []

async def main():
    # Runtime-agnostic PORT (#3159): the adaptor must route mcpServers through
    # ctx.register_mcp_server (it must NOT write .claude/settings.json directly).
    # The spy records the call and deliberately does not render, so a direct
    # write regression is caught by both the empty recorder and the leftover file.
    def register_mcp_server(name, spec):
        recorded.append({"name": name, "spec": spec})

    ctx = InstallContext(
        configs_dir=configs_dir,
        workspace_id="test-ws",
        runtime="claude_code",
        plugin_root=plugin_root,
        register_mcp_server=register_mcp_server,
    )
    adaptor = MCPServerAdaptor("molecule-platform-mcp", "claude_code")
    await adaptor.install(ctx)

asyncio.run(main())
print("RECORDER=" + json.dumps(recorded))
`

	configsDir := t.TempDir()
	pluginRoot := t.TempDir()
	writePlatformFragment(t, contract, pluginRoot)

	stdout := runMCPAdaptorHarness(t, pyScript, pluginRoot, configsDir, nil)

	// (1) KEYSTONE: assert the adaptor actually CALLED ctx.register_mcp_server
	// with [(molecule-platform, spec)]. A direct .claude write would leave the
	// recorder empty → this fails.
	recorded := parseRecorder(t, stdout)
	if len(recorded) != 1 {
		t.Fatalf("register_mcp_server PORT was not called exactly once with the platform server: recorded=%v\n"+
			"A direct .claude/settings.json write (the #3159 regression) bypasses the PORT and leaves this empty.", recorded)
	}
	if recorded[0].Name != "molecule-platform" {
		t.Errorf("register_mcp_server called with name %q, want molecule-platform", recorded[0].Name)
	}
	if cmd, _ := recorded[0].Spec["command"].(string); cmd != "molecule-mcp" {
		t.Errorf("register_mcp_server spec.command = %q, want molecule-mcp", cmd)
	}
	if env, ok := recorded[0].Spec["env"].(map[string]any); !ok || env["MOLECULE_MCP_MODE"] != "management" {
		t.Errorf("register_mcp_server spec.env = %v, want MOLECULE_MCP_MODE=management", recorded[0].Spec["env"])
	}

	// (2) The legacy /configs/.claude/settings.json must NOT be created by the
	// adaptor itself. The PORT is the only legitimate delivery path.
	rel := strings.TrimPrefix(contract.SettingsPath, "/configs/")
	claudeSettings := filepath.Join(configsDir, rel)
	if _, err := os.Stat(claudeSettings); err == nil {
		t.Errorf("MCPServerAdaptor wrote %s directly — it bypassed the PORT (#3159 regression)", contract.SettingsPath)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", claudeSettings, err)
	}
}

// TestMCPPluginDeliveryContract_CodexRoutesToCodexConfig is the codex-runtime
// regression for #3159. It installs the REAL MCPServerAdaptor with runtime
// "codex" and a spy that renders via the codex renderer, then asserts the codex
// native config ($HOME/.codex/config.toml) declares [mcp_servers.molecule-platform]
// with env MOLECULE_MCP_MODE=management — and that NO /configs/.claude/settings.json
// was produced. This catches the exact #3159 bug class: a hard-coded Claude
// write would mis-wire a codex concierge (MCP written to a file codex never
// reads), leaving create_workspace absent.
func TestMCPPluginDeliveryContract_CodexRoutesToCodexConfig(t *testing.T) {
	contract, err := LoadMCPPluginDeliveryContract()
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}

	pyScript := `
import asyncio, json, sys
from pathlib import Path
sys.path.insert(0, sys.argv[1])
from molecule_runtime.plugins_registry.builtins import MCPServerAdaptor
from molecule_runtime.plugins_registry.protocol import InstallContext
from molecule_runtime.mcp_render import render_for_runtime

plugin_root = Path(sys.argv[2])
configs_dir = Path(sys.argv[3])
configs_dir.mkdir(parents=True, exist_ok=True)

recorded = []

async def main():
    # codex PORT: the active codex renderer writes ~/.codex/config.toml. HOME is
    # pinned to a temp dir by the Go harness so the codex renderer's $HOME lookup
    # lands inside the sandbox.
    def register_mcp_server(name, spec):
        recorded.append({"name": name, "spec": spec})
        render_for_runtime("codex", str(configs_dir), name, spec)

    ctx = InstallContext(
        configs_dir=configs_dir,
        workspace_id="test-ws",
        runtime="codex",
        plugin_root=plugin_root,
        register_mcp_server=register_mcp_server,
    )
    adaptor = MCPServerAdaptor("molecule-platform-mcp", "codex")
    await adaptor.install(ctx)

asyncio.run(main())
print("RECORDER=" + json.dumps(recorded))
`

	configsDir := t.TempDir()
	pluginRoot := t.TempDir()
	homeDir := t.TempDir()
	writePlatformFragment(t, contract, pluginRoot)

	// Pin HOME so the codex renderer's ~/.codex/config.toml lands in the sandbox.
	stdout := runMCPAdaptorHarness(t, pyScript, pluginRoot, configsDir, []string{"HOME=" + homeDir})

	// The PORT must have been called (same keystone guard, codex side).
	recorded := parseRecorder(t, stdout)
	if len(recorded) != 1 || recorded[0].Name != "molecule-platform" {
		t.Fatalf("codex: register_mcp_server PORT not called with the platform server: recorded=%v", recorded)
	}

	// codex native config must declare [mcp_servers.molecule-platform] + env.
	codexConfig := filepath.Join(homeDir, ".codex", "config.toml")
	tomlBytes, err := os.ReadFile(codexConfig)
	if err != nil {
		t.Fatalf("codex config not produced at %s: %v\n"+
			"A hard-coded Claude write (the #3159 bug) would mis-wire a codex concierge.", codexConfig, err)
	}
	toml := string(tomlBytes)
	if !strings.Contains(toml, "[mcp_servers.molecule-platform]") {
		t.Errorf("codex config missing [mcp_servers.molecule-platform] table:\n%s", toml)
	}
	if !strings.Contains(toml, "[mcp_servers.molecule-platform.env]") || !strings.Contains(toml, `MOLECULE_MCP_MODE = "management"`) {
		t.Errorf("codex config missing env MOLECULE_MCP_MODE=management:\n%s", toml)
	}

	// And the Claude settings.json must be ABSENT — proving the MCP did NOT get
	// mis-routed to the Claude file on a codex runtime.
	rel := strings.TrimPrefix(contract.SettingsPath, "/configs/")
	claudeSettings := filepath.Join(configsDir, rel)
	if _, err := os.Stat(claudeSettings); err == nil {
		t.Errorf("codex runtime produced %s — the MCP was mis-routed to the Claude file (#3159 regression)", contract.SettingsPath)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", claudeSettings, err)
	}
}

// recordedCall mirrors one ctx.register_mcp_server(name, spec) call captured by
// the python spy and emitted as JSON.
type recordedCall struct {
	Name string         `json:"name"`
	Spec map[string]any `json:"spec"`
}

// parseRecorder extracts the `RECORDER=[...]` JSON line the harness prints.
func parseRecorder(t *testing.T, stdout string) []recordedCall {
	t.Helper()
	const marker = "RECORDER="
	var payload string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), marker) {
			payload = strings.TrimSpace(line)[len(marker):]
		}
	}
	if payload == "" {
		t.Fatalf("harness did not emit a RECORDER= line; stdout:\n%s", stdout)
	}
	var calls []recordedCall
	if err := json.Unmarshal([]byte(payload), &calls); err != nil {
		t.Fatalf("parse RECORDER payload %q: %v", payload, err)
	}
	return calls
}

// writePlatformFragment writes the platform MCP plugin's settings-fragment.json.
func writePlatformFragment(t *testing.T, contract *MCPPluginDeliveryContract, pluginRoot string) {
	t.Helper()
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
}

// runMCPAdaptorHarness runs the given python harness against the REAL
// molecule-ai-workspace-runtime MCPServerAdaptor and returns its stdout. extraEnv
// entries (e.g. HOME=...) are appended to the child env. It fails the test (never
// skips) when the runtime source can't be found — a skip here would re-introduce
// the CR2 #12653 false-green where the Platform (Go) job passed without ever
// exercising the real adaptor.
func runMCPAdaptorHarness(t *testing.T, pyScript, pluginRoot, configsDir string, extraEnv []string) string {
	t.Helper()

	runtimePath := os.Getenv("MOLECULE_WORKSPACE_RUNTIME")
	if runtimePath == "" {
		repoRoot := filepath.Join("..", "..", "..")
		sibling := filepath.Join(repoRoot, "molecule-ai-workspace-runtime")
		if _, err := os.Stat(filepath.Join(sibling, "molecule_runtime", "plugins_registry", "builtins.py")); err == nil {
			runtimePath = sibling
		}
	}

	python := os.Getenv("MOLECULE_RUNTIME_PYTHON")
	if python == "" {
		python = "python3"
	}

	// Build the child's PYTHONPATH. It must contain the runtime SOURCE tree (so
	// `import molecule_runtime.*` resolves) AND the interpreter's resolved
	// site-packages dirs (so the runtime's own deps — notably `a2a-sdk`, which
	// adapter_base imports at module level — resolve too).
	//
	// Why the explicit site-packages: the codex sub-test pins HOME to a sandbox
	// so the codex renderer's `~/.codex/config.toml` lands inside the temp dir.
	// But when the runtime's deps were `pip install`ed into the *user* site
	// (CI does this — "Defaulting to user installation because normal
	// site-packages is not writeable"), Python derives the user-site location
	// from $HOME. Overriding HOME therefore relocates user-site to the empty
	// sandbox and `a2a` vanishes → `ModuleNotFoundError: No module named 'a2a'`
	// (RC 13567 — the claude sub-test passes only because it does NOT override
	// HOME). We resolve the real site-packages dirs HERE, under the inherited
	// HOME, and pin them onto PYTHONPATH so the import survives the HOME swap.
	pythonPath := buildHarnessPythonPath(python, runtimePath)

	cmd := exec.Command(python, "-", runtimePath, pluginRoot, configsDir)
	cmd.Env = os.Environ()
	if pythonPath != "" {
		cmd.Env = append(cmd.Env, "PYTHONPATH="+pythonPath)
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdin = strings.NewReader(strings.TrimSpace(pyScript))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if runtimePath == "" {
			t.Fatalf("CR2 #12653: molecule-ai-workspace-runtime source not found — this test must exercise the REAL MCPServerAdaptor. "+
				"Set MOLECULE_WORKSPACE_RUNTIME=/path/to/molecule-ai-workspace-runtime or check out the runtime as a sibling of the repo root. "+
				"Underlying python error (if any): %v\nstderr: %s", err, stderr.String())
		}
		t.Fatalf("run MCPServerAdaptor: %v\nstderr: %s", err, stderr.String())
	}
	return stdout.String()
}

// buildHarnessPythonPath returns the PYTHONPATH for the harness child: the
// runtime source tree (so `molecule_runtime.*` imports) followed by the
// interpreter's resolved site-packages dirs (so the runtime's deps, e.g.
// `a2a-sdk`, import). It queries `python` for its site dirs while the inherited
// HOME is still in effect, which is what makes the user-site path stable across
// a subsequent HOME override (see runMCPAdaptorHarness). Existing PYTHONPATH
// entries are preserved. A query failure degrades to just the runtime path
// rather than aborting — the real assertion lives in the harness run.
func buildHarnessPythonPath(python, runtimePath string) string {
	var parts []string
	if runtimePath != "" {
		parts = append(parts, runtimePath)
	}

	// Resolve user-site + global site-packages from the SAME interpreter the
	// harness uses, under the current (un-overridden) HOME.
	const siteQuery = `import site,json
dirs=[]
try:
    u=site.getusersitepackages()
    if u: dirs.append(u)
except Exception: pass
try:
    dirs+= [d for d in site.getsitepackages() if d]
except Exception: pass
print(json.dumps(dirs))`
	out, err := exec.Command(python, "-c", siteQuery).Output()
	if err == nil {
		var siteDirs []string
		if json.Unmarshal(bytes.TrimSpace(out), &siteDirs) == nil {
			parts = append(parts, siteDirs...)
		}
	}

	// Preserve any inherited PYTHONPATH so we add to it rather than replace it.
	if existing := os.Getenv("PYTHONPATH"); existing != "" {
		parts = append(parts, existing)
	}

	return strings.Join(parts, string(os.PathListSeparator))
}

package handlers

// MCP-plugin delivery contract (core#3080).
//
// This is the producer-side guard for the MCP-plugin delivery contract
// between molecule-core (which ships plugin declarations) and the
// claude-code workspace template (whose runtime writes/reads
// /configs/.claude/settings.json under the mcpServers key).
//
// The contract file is duplicated byte-for-byte in both repos and kept
// honest by a cross-repo drift gate.

import (
	"encoding/json"
	"os"
)

const mcpPluginDeliveryContractPath = "../../../contracts/mcp-plugin-delivery.contract.json"

// MCPPluginDeliveryContract describes the pinned MCP-plugin delivery surface.
type MCPPluginDeliveryContract struct {
	SettingsPath string `json:"settings_path"`
	Key          string `json:"key"`
	EntryShape   string `json:"entry_shape"`
	Producer     string `json:"producer"`
	Consumer     string `json:"consumer"`
	// MCPServerName is the SSOT for the platform MCP server NAME — the mcpServers
	// key the runtime registers the server under, which Claude Code turns into
	// the tool-id prefix `mcp__<MCPServerName>__<tool>`. The platform-agent
	// template's mcp_servers.yaml and the online/degraded gate's expected tool id
	// (conciergePlatformMCPCreateWorkspaceTool) both derive from this value — see
	// TestSSOT_DegradeGateToolDerivesFromContract.
	MCPServerName string `json:"mcp_server_name"`

	// Descriptor is the prose SSOT describing the runtime-agnostic MCP descriptor
	// shape and the wiring-PORT indirection (register_mcp_server →
	// register_mcp_server_hook). It is pinned so a refactor that collapses the
	// PORT back into a hard-coded Claude write is caught by MatchesSSOT.
	Descriptor string `json:"descriptor"`

	// Port pins the MCP-wiring PORT symbol names on the runtime side. These are
	// the seam #3159 introduced: an adaptor calls Port.Hook; the active runtime's
	// Port.Impl renders the descriptor into the native config it reads. If any of
	// these symbols is renamed or removed, the contract (and this struct) must be
	// updated deliberately.
	Port Port `json:"port"`

	// Runtimes maps each known runtime id to its native MCP-config delivery
	// surface. claude_code/codex are implemented; gemini_cli/hermes are todo
	// stubs (fail-loud renderers). MatchesSSOT asserts per-runtime so a runtime
	// silently dropping out of the contract is caught.
	Runtimes map[string]Runtime `json:"runtimes"`
}

// Port names the MCP-wiring PORT symbols on the runtime side (#3159). The
// indirection is what lets a single runtime-agnostic descriptor reach the
// native config of whichever runtime is active.
type Port struct {
	// Hook is the InstallContext seam an adaptor calls to wire an MCP server
	// (e.g. "InstallContext.register_mcp_server").
	Hook string `json:"hook"`
	// Impl is the BaseAdapter method bound behind Hook at install time.
	Impl string `json:"impl"`
	// PresentProbe is the BaseAdapter method that reports whether the management
	// MCP is present in the active runtime's native config.
	PresentProbe string `json:"present_probe"`
	// Dispatch describes how the default hook dispatches on the runtime name.
	Dispatch string `json:"dispatch"`
	// ResolverDefault describes the registry default that routes an
	// mcpServers-shaped plugin to MCPServerAdaptor for ANY runtime.
	ResolverDefault string `json:"resolver_default"`
}

// Runtime is a single runtime's native MCP-config delivery surface.
type Runtime struct {
	SettingsPath string `json:"settings_path"`
	Format       string `json:"format"`
	// Key is the JSON object key (claude_code/gemini_cli); empty for TOML runtimes.
	Key string `json:"key,omitempty"`
	// Table is the TOML table prefix (codex); empty for JSON runtimes.
	Table string `json:"table,omitempty"`
	// Renderer names the mcp_render function that writes this runtime's config.
	Renderer string `json:"renderer"`
	// Status is "implemented" or a "todo-*" marker for unverified runtimes.
	Status string `json:"status"`
}

// MatchesSSOT asserts that the loaded contract matches the pinned SSOT values:
// the core scalar fields, the MCP-wiring PORT symbol names, and each runtime's
// native delivery surface (claude_code/codex implemented; gemini/hermes todo).
// It returns the list of mismatches (empty == matches). The test wrapper turns
// a non-empty list into a failure. Keeping the assertion here (rather than only
// in the test) lets any contract consumer self-check the SSOT.
func (c *MCPPluginDeliveryContract) MatchesSSOT() []string {
	var diffs []string
	eq := func(field, got, want string) {
		if got != want {
			diffs = append(diffs, field+" = "+got+", want "+want)
		}
	}

	eq("settings_path", c.SettingsPath, "/configs/.claude/settings.json")
	eq("key", c.Key, "mcpServers")
	eq("entry_shape", c.EntryShape, "name->{command,args?,env?}")
	eq("producer", c.Producer, "MCPServerAdaptor")
	eq("consumer", c.Consumer, "claude_sdk_executor._load_settings_mcp")
	eq("mcp_server_name", c.MCPServerName, "molecule-platform")

	// PORT symbols (#3159). These pin the wiring seam: if the adaptor regresses
	// to a hard-coded Claude write, the hook/impl indirection disappears and this
	// catches the contract drift.
	eq("port.hook", c.Port.Hook, "InstallContext.register_mcp_server")
	eq("port.impl", c.Port.Impl, "BaseAdapter.register_mcp_server_hook")
	eq("port.present_probe", c.Port.PresentProbe, "BaseAdapter.management_mcp_present")
	if c.Port.Dispatch == "" {
		diffs = append(diffs, "port.dispatch is empty — must describe runtime-name dispatch")
	}
	if c.Port.ResolverDefault == "" {
		diffs = append(diffs, "port.resolver_default is empty — must describe the mcpServers->MCPServerAdaptor default")
	}

	if c.Descriptor == "" {
		diffs = append(diffs, "descriptor is empty — must pin the runtime-agnostic descriptor SSOT")
	}

	// Per-runtime native delivery surfaces. claude_code/codex must be present and
	// implemented with the renderer the runtime really uses; gemini/hermes must be
	// declared but flagged todo (so a runtime can't silently fall out of the
	// contract, and so an "implemented" claim for a stub is caught).
	wantRuntimes := map[string]Runtime{
		"claude_code": {SettingsPath: "/configs/.claude/settings.json", Format: "json", Key: "mcpServers", Renderer: "mcp_render.render_claude_settings", Status: "implemented"},
		"codex":       {SettingsPath: "~/.codex/config.toml", Format: "toml", Table: "mcp_servers", Renderer: "mcp_render.render_codex_config", Status: "implemented"},
	}
	for name, want := range wantRuntimes {
		got, ok := c.Runtimes[name]
		if !ok {
			diffs = append(diffs, "runtimes missing required runtime "+name)
			continue
		}
		eq("runtimes."+name+".settings_path", got.SettingsPath, want.SettingsPath)
		eq("runtimes."+name+".format", got.Format, want.Format)
		eq("runtimes."+name+".key", got.Key, want.Key)
		eq("runtimes."+name+".table", got.Table, want.Table)
		eq("runtimes."+name+".renderer", got.Renderer, want.Renderer)
		eq("runtimes."+name+".status", got.Status, want.Status)
	}
	// gemini/hermes are declared-but-todo: present, and NOT claiming implemented.
	for _, name := range []string{"gemini_cli", "hermes"} {
		got, ok := c.Runtimes[name]
		if !ok {
			diffs = append(diffs, "runtimes missing declared-todo runtime "+name)
			continue
		}
		if got.Status == "implemented" {
			diffs = append(diffs, "runtimes."+name+".status claims implemented, but its renderer is a fail-loud stub (expected a todo-* status)")
		}
		if got.Renderer == "" {
			diffs = append(diffs, "runtimes."+name+".renderer is empty")
		}
	}
	return diffs
}

// LoadMCPPluginDeliveryContract loads the contract from the repo root.
func LoadMCPPluginDeliveryContract() (*MCPPluginDeliveryContract, error) {
	data, err := os.ReadFile(mcpPluginDeliveryContractPath)
	if err != nil {
		return nil, err
	}
	var c MCPPluginDeliveryContract
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

package handlers

// MCP-plugin delivery contract (core#3080).
//
// Core now CONSUMES this contract from the molecule-contracts SSOT (RFC
// molecule-core#3285 §10 "consume, never two copies"): it imports the generated
// Go binding (go.moleculesai.app/molecule-contracts/gen/go) instead of reading
// its own JSON file. The import IS the link — core's runtime + tests can no
// longer drift from the SSOT.
//
// The on-disk contracts/mcp-plugin-delivery.contract.json is RETAINED only as a
// CI cross-repo comparison anchor (the template/runtime drift gate + the
// verb-manifest check still byte-compare against it), kept byte-equal to the
// SSOT by contract-ssot-sync. Core CODE no longer reads it. Fully removing the
// JSON — repointing those cross-repo gates straight at the molecule-contracts
// SSOT — is a deliberate follow-up (one of those gates is the SEV1-relevant
// verb-manifest check, so it is sequenced after the incident).

import (
	molcontracts "go.moleculesai.app/molecule-contracts/gen/go"
)

// MCPPluginDeliveryContract / Port / Runtime are the generated SSOT types,
// re-exported as aliases so existing references compile unchanged.
type (
	MCPPluginDeliveryContract = molcontracts.MCPPluginDeliveryContract
	Port                      = molcontracts.Port
	Runtime                   = molcontracts.Runtime
)

// LoadMCPPluginDeliveryContract returns the contract from the molecule-contracts
// SSOT binding. The signature (and the always-nil error) is kept so existing
// callers/tests are unchanged; there is no longer a file to read or fail on.
func LoadMCPPluginDeliveryContract() (*MCPPluginDeliveryContract, error) {
	c := molcontracts.Contract
	return &c, nil
}

// MatchesSSOT asserts the contract carries the exact values core depends on.
// With the binding this is a value-pin: a contract change in molecule-contracts
// reaches core only via a module bump, and this catches a bumped value that
// would break the degrade gate or the PORT wiring. Kept as a function (not a
// method) since the type is now an alias of the imported struct.
func MatchesSSOT(c *MCPPluginDeliveryContract) []string {
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
	eq("required_tool", c.RequiredTool, "provision_workspace")
	eq("loaded_mcp_tools_field", c.LoadedMCPToolsField, "loaded_mcp_tools")

	// PORT symbols (#3159): if the adaptor regresses to a hard-coded Claude
	// write, the hook/impl indirection disappears and this catches the drift.
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
	// implemented; hermes must be declared but flagged todo.
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
	for _, name := range []string{"hermes"} {
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

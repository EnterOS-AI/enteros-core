package handlers

// plugin_registry.go — core consumes the SDK native-plugins registry as the
// single source of truth for which first-party plugins the platform delivers.
//
// RFC molecule-core#4413 (digest-providers-as-native-plugins) + the
// scheduler-as-trigger-plugin RFC. WHICH platform-owned plugins exist, WHERE
// they are fetched from, and ON WHICH workspaces they install, is owned by ONE
// contract: the SDK contracts/plugin/native-plugins.registry.json, consumed
// here through its generated Go binding
// (go.moleculesai.app/sdk/gen/go/molcontracts) exactly like
// mcp_plugin_delivery_contract.go consumes the MCP-delivery contract. Before
// this file, each plugin's install source was a hand-written Go const
// (SchedulerPluginSource, conciergePlatformMCPSource) kept in lockstep with the
// plugin repos BY HAND; now the import is the link and core cannot drift from
// the registry — a source change reaches core only via a molcontracts bump.

import (
	"context"
	"log"
	"os"
	"strings"

	molcontracts "go.moleculesai.app/sdk/gen/go/molcontracts"
)

// mustNativePluginSource returns the registry install source for the native
// plugin with the given name, or PANICS at init if the registry no longer
// carries it. Core hard-depends on a couple of these names by identity (the
// scheduler and the concierge management MCP, whose declarations are wired into
// dedicated code paths and whose name is the entitlement-gate key). An SDK bump
// that renames or drops one must fail LOUD at startup — never silently record an
// empty source the box then cannot fetch.
func mustNativePluginSource(name string) string {
	for _, p := range molcontracts.NativePlugins {
		if p.Name == name {
			return p.Source
		}
	}
	panic("native-plugins registry (SDK molcontracts) is missing required plugin: " + name)
}

// nativePluginSourcesForInstall returns the registry sources whose install
// policy equals the given policy, preserving registry order.
func nativePluginSourcesForInstall(install string) []string {
	entries := molcontracts.NativePluginsForInstall(install)
	out := make([]string, 0, len(entries))
	for _, p := range entries {
		out = append(out, p.Source)
	}
	return out
}

// defaultNativePluginSources are the sources declared on EVERY workspace
// (install: default): the per-workspace scheduler + the idle-digest providers.
func defaultNativePluginSources() []string {
	return nativePluginSourcesForInstall(molcontracts.NativePluginInstallDefault)
}

// defaultNativePluginSourcesForDeclare is the install:default set
// declareDefaultNativePlugins actually seeds: defaultNativePluginSources() MINUS
// the scheduler source. The scheduler is owned by the dedicated
// ensureSchedulerPluginDeclared path (workspace_provision_shared.go), which
// declares it under the const SchedulerPluginName ("molecule-scheduler"). This
// path derives an install name from the source via PluginNameFromSource, which
// for the scheduler source yields a DIFFERENT name ("molecule-ai-plugin-
// scheduler") — so seeding the scheduler here too would create a SECOND,
// differently-named workspace_declared_plugins row for one plugin and a duplicate
// boot-install. Filtering here (not in defaultNativePluginSources) keeps the
// registry SSOT untouched — the concierge-exclusion test and other consumers
// still see the full registry-derived set.
func defaultNativePluginSourcesForDeclare() []string {
	all := defaultNativePluginSources()
	out := make([]string, 0, len(all))
	for _, s := range all {
		if s == SchedulerPluginSource {
			continue
		}
		out = append(out, s)
	}
	return out
}

// declareDefaultNativePluginsEnv gates the universal install:default
// declaration below.
const declareDefaultNativePluginsEnv = "MOLECULE_DECLARE_DEFAULT_NATIVE_PLUGINS"

// declareDefaultNativePluginsEnabled reports whether every workspace should
// declare the install:default native plugins at provision.
//
// Default OFF. With the flag off, merging this consumer is byte-identical to
// today: the scheduler is still declared on-demand by ensureSchedulerPluginDeclared
// when a workspace gains a schedule, the concierge MCP by the kind-gated
// applyConciergeProvisionConfig, and no idle-digest plugin is declared anywhere.
// The owner flips this ON during the fleet rollout, once the runtime
// digest-provider loader (MOLECULE_DIGEST_PROVIDER_PLUGINS) and the scheduler
// daemon are live fleet-wide — at which point every workspace declares the full
// native set from the registry SSOT (RFC molecule-core#4413 D3 / scheduler P5).
func declareDefaultNativePluginsEnabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(declareDefaultNativePluginsEnv)))
	return v != "" && v != "0" && v != "false" && v != "no"
}

// declareDefaultNativePlugins records the install:default native plugins on the
// workspace (idempotent upsert into workspace_declared_plugins), so the box
// boot-install fetches them and the runtime loaders pick them up on their next
// boot/reconcile. It is:
//   - flag-gated (declareDefaultNativePluginsEnabled) — a no-op until the owner
//     arms the fleet rollout;
//   - non-fatal — seedTemplatePlugins logs and skips any single bad source, so a
//     registry entry that fails to record never blocks provisioning;
//   - safe on every provision path (create/restart/resume) — the upsert is
//     idempotent, so re-declaring on each beat is harmless.
//
// The install:concierge plugin (the management MCP) is NOT declared here — it is
// privileged and stays gated to the org-root kind=platform concierge via
// applyConciergeProvisionConfig + the recordDeclaredPlugin entitlement gate. The
// scheduler is likewise NOT declared here — the dedicated ensureSchedulerPluginDeclared
// path owns it (see defaultNativePluginSourcesForDeclare).
func declareDefaultNativePlugins(ctx context.Context, workspaceID string) {
	if !declareDefaultNativePluginsEnabled() {
		return
	}
	sources := defaultNativePluginSourcesForDeclare()
	if len(sources) == 0 {
		return
	}
	rec, skip := seedTemplatePlugins(ctx, workspaceID, sources)
	log.Printf("native-plugins: workspace %s declared %d install:default plugins from the registry (%d skipped)", workspaceID, rec, skip)
}

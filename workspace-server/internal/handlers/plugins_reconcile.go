package handlers

// plugins_reconcile.go — post-online declared-plugin reconcile (RFC#2843 #32).
//
// THE RULE (CTO): agent-skills are PLUGINS. They install DYNAMICALLY after a
// workspace boots online, via the EXISTING plugin install pipeline — NEVER via
// the provisioning channel (Secrets Manager or the template-asset relay). The
// old org_import.go path that copied declared plugins into `configFiles` (the
// provisioning channel) is removed; this reconcile replaces it.
//
// TRIGGER CHOICE — the registry heartbeat's transition-to-online, NOT a new
// sweeper or a hook buried in the provisioner:
//
//   - The heartbeat handler (registry.go Heartbeat) is the SINGLE place every
//     workspace flips to `online` from ANY prior state — provisioning,
//     offline, awaiting_agent, failed, and degraded→online recovery. A
//     workspace is only reachable for an install once it is online (the
//     install pipeline needs reachable provider compute), so
//     "just reached online" is exactly the right moment.
//   - The existing drift_sweeper reconciles UPDATE drift only (tracked_ref
//     moved) and explicitly does NOT install-missing. Extending it would mean
//     a periodic full-fleet scan for a one-shot, event-driven need, and would
//     install into workspaces that are offline (and fail). The event hook is
//     cheaper and lands the plugin within one heartbeat of boot.
//   - Wiring matches the existing SetQueueDrainFunc pattern: RegistryHandler
//     holds a nil-safe ReconcileFunc, the router wires it to
//     PluginsHandler.ReconcileWorkspacePlugins after both are constructed, and
//     the heartbeat fires it fire-and-forget via globalGoAsync on each
//     transition-to-online.
//
// IDEMPOTENT + RETRY-SAFE: the reconcile diffs the DECLARED set
// (workspace_declared_plugins) against the INSTALLED set (workspace_plugins).
// An already-installed plugin is skipped (no-op). A failed install leaves no
// workspace_plugins row, so the NEXT transition-to-online retries it. Each
// install is logged. The install pipeline itself is idempotent (atomic
// stage→swap on Docker, rm -rf + re-extract on EIC).

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/plugins"
	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

// ReconcileFunc installs any declared-but-missing plugins for a workspace
// that has just reached `online`. Injected into RegistryHandler at router
// wiring time to avoid a handler→handler import cycle (same pattern as
// QueueDrainFunc). nil-safe: the heartbeat skips reconcile when unset.
type ReconcileFunc func(ctx context.Context, workspaceID string)

// reconcileDeadline bounds one workspace's full reconcile pass (all its
// declared plugins). A single gitea clone of the seo-all package (~668 KiB)
// finishes in seconds; the per-fetch PLUGIN_INSTALL_FETCH_TIMEOUT (5m
// default) still bounds each individual install inside this budget.
const reconcileDeadline = 10 * time.Minute

// resolveSourceSHA is the testable seam for re-resolving a branch-pinned
// plugin's current upstream SHA. Production is plugins.ResolveSourceSHA (a
// --depth=1 fetch + rev-parse against the source's own #branch fragment);
// tests stub it to assert staleness handling without real git operations.
var resolveSourceSHA = plugins.ResolveSourceSHA

// ReconcileWorkspacePlugins is the production ReconcileFunc. It is exported so
// the router can wire it into RegistryHandler.SetReconcileFunc.
//
// Flow:
//  1. Load the declared set (workspace_declared_plugins) — desired state.
//  2. Load the installed set (workspace_plugins names) — current state.
//  3. For each declared plugin NOT installed: resolveAndStage(source_raw) →
//     deliverToContainer → record the install. Skip already-installed (no-op).
//
// Errors per plugin are logged and DO NOT abort the rest of the pass — a
// single bad source must not starve the other plugins. A plugin that fails
// here is retried on the next transition-to-online (no install row written).
func (h *PluginsHandler) ReconcileWorkspacePlugins(ctx context.Context, workspaceID string) {
	if workspaceID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, reconcileDeadline)
	defer cancel()

	declared, err := listDeclaredPlugins(ctx, workspaceID)
	if err != nil {
		log.Printf("Plugin reconcile: workspace=%s list declared failed: %v", workspaceID, err)
		return
	}
	if len(declared) == 0 {
		return // common case: workspace declares no plugins
	}

	installed, err := listInstalledPluginRecords(ctx, workspaceID)
	if err != nil {
		log.Printf("Plugin reconcile: workspace=%s list installed failed: %v", workspaceID, err)
		return
	}

	// A platform concierge reconcile can run while it is provisioning and after
	// each transition to online. Deliver missing bytes in either state, but do
	// not let reconcile restart the concierge and create a provisioning loop.
	suppressRestart := platformConciergeReconcileShouldSkipRestart(ctx, workspaceID)

	var installedCount, skipped int
	for _, d := range declared {
		presentButStale := false
		if rec, ok := installed[d.PluginName]; ok {
			// Stale-row guard: the workspace_plugins row may survive a fresh
			// image boot or de-baked box where /configs/plugins is empty. Trust
			// the row ONLY when the plugin is also confirmed present on the box.
			if h.pluginPresentOnBox(ctx, workspaceID, d.PluginName) {
				// Content-aware reconcile (fix (b)): a present, DB-recorded plugin
				// is normally an idempotent no-op — UNLESS it is branch-pinned
				// (track=none) and its upstream tip has advanced past the SHA we
				// installed. The drift sweeper only chases tag:/sha: pins, so a
				// moving branch (e.g. the concierge management-MCP fragment on
				// #main) would otherwise never propagate a merged change without a
				// full reboot. Re-deliver when — and only when — the fragment moved.
				if !h.pluginFragmentStale(ctx, d.SourceRaw, rec.InstalledSHA) {
					skipped++
					continue // installed, present, and up-to-date — idempotent no-op
				}
				presentButStale = true
				log.Printf("Plugin reconcile: workspace=%s plugin=%s present but fragment moved (installed_sha stale) — re-delivering",
					workspaceID, d.PluginName)
				// Fall through to (re)delivery so the new fragment bytes land.
			} else {
				log.Printf("Plugin reconcile: workspace=%s plugin=%s installed-row present but not on box — re-delivering",
					workspaceID, d.PluginName)
				// Fall through to (re)delivery so the MCP actually lands in settings.json.
			}
		}

		// Track value: a tag:/sha: ref opts the plugin into drift tracking;
		// a branch ref or no ref → "none" (the install records the SHA but
		// the sweeper won't chase a moving branch).
		track := trackFromSource(d.SourceRaw)

		stage, stageErr := h.resolveAndStage(ctx, installRequest{Source: d.SourceRaw, Track: track})
		if stageErr != nil {
			log.Printf("Plugin reconcile: workspace=%s plugin=%s stage failed (source=%s): %v",
				workspaceID, d.PluginName, d.SourceRaw, stageErr)
			continue
		}

		// RFC#2843 #38: the runtime-image entrypoint boot-installs declared
		// plugins to the box on EVERY boot, BEFORE this online-transition
		// reconcile runs — so by now the plugin is typically already on the box.
		// If present, record the tracking row WITHOUT re-delivering via EIC +
		// restarting: the EIC push + restartFunc was the redundant churn (one
		// wasted full re-provision per fresh workspace — the observed ~12-min
		// reprovision). Only deliver when the plugin is NOT already present
		// (boot-install disabled/failed, or a non-boot-install path) — the
		// safety net. pluginPresentOnBox is conservative (false on any
		// uncertainty) so a genuinely-missing install is never silently skipped.
		//
		// A plugin flagged presentButStale above bypasses this dedup: its bytes
		// ARE on the box, but they are the OLD fragment, so we must deliver the
		// re-staged (current) bytes rather than merely re-record the tracking row.
		if !presentButStale && h.pluginPresentOnBox(ctx, workspaceID, stage.PluginName) {
			log.Printf("Plugin reconcile: workspace=%s plugin=%s already on box (boot-installed) — recording tracking row only, no re-deliver/restart",
				workspaceID, stage.PluginName)
		} else {
			// Platform concierge lifecycle guard: preserve idempotent delivery,
			// but leave any required restart to an explicit operator action.
			if suppressRestart {
				stage.SuppressRestart = true
				log.Printf("Plugin reconcile: workspace=%s plugin=%s not on box during platform concierge provisioning/online lifecycle — delivering WITHOUT automatic restart",
					workspaceID, stage.PluginName)
			}
			if deliverErr := h.deliver(ctx, workspaceID, stage); deliverErr != nil {
				stage.cleanup()
				if errors.Is(deliverErr, errNoPushTarget) {
					// Docker-less tenant (#206): the docker-push is RETIRED. The plugin
					// is declared, so the runtime's boot materializer is responsible for
					// PULLING it into /configs/plugins/<name>/ on boot. We do NOT copy
					// bytes in, and we do NOT restart here — the boot installer already
					// ran before this online-transition reconcile, so a restart would
					// just loop on a genuinely-unfetchable source. The interactive
					// install path owns the on-demand re-materialize; here we log and
					// move on (retried on the next transition-to-online).
					log.Printf("Plugin reconcile: workspace=%s plugin=%s not on box, no docker-push target (docker-less) — pull (boot materializer) is the SSOT; skipping push",
						workspaceID, d.PluginName)
					continue
				}
				log.Printf("Plugin reconcile: workspace=%s plugin=%s deliver failed: %v",
					workspaceID, d.PluginName, deliverErr)
				continue
			}
		}
		stage.cleanup()

		if recErr := recordWorkspacePluginInstall(
			ctx, workspaceID, stage.PluginName, stage.Source.Raw(), track, stage.InstalledSHA,
		); recErr != nil {
			// Install succeeded on the box; the tracking row failed. Log it —
			// the next reconcile will see the plugin missing from
			// workspace_plugins and re-install (idempotent on the box). This is
			// the same trade-off the interactive install path makes.
			log.Printf("Plugin reconcile: workspace=%s plugin=%s install delivered but tracking-row write failed: %v",
				workspaceID, stage.PluginName, recErr)
			continue
		}

		installedCount++
		log.Printf("Plugin reconcile: workspace=%s installed declared plugin %s from %s (sha=%s)",
			workspaceID, stage.PluginName, d.SourceRaw, shortSHA(stage.InstalledSHA))
	}

	if installedCount > 0 {
		log.Printf("Plugin reconcile: workspace=%s complete — installed=%d skipped(already-present)=%d declared=%d",
			workspaceID, installedCount, skipped, len(declared))
	}
}

// pluginFragmentStale reports whether a present, DB-recorded plugin's installed
// content is behind its source — the signal fix (b) uses to re-deliver a
// branch-pinned fragment whose upstream tip has moved. It returns true ONLY for
// a branch-pinned (track=none) source whose current upstream SHA differs from
// the recorded installed SHA — the case the drift sweeper (tag:/sha: only)
// structurally never covers.
//
// Fail-CLOSED to not-stale on every uncertainty so a transient fetch blip, a
// NULL/empty baseline, or an immutable tag/sha pin never triggers a churny
// re-deliver (and never an online↔provisioning restart bounce):
//   - installedSHA == ""            → no content baseline to compare against
//   - trackFromSource != "none"     → tag:/sha: pin, owned by the drift sweeper
//   - resolve error / empty result  → self-heals on a later beat, never churn
//
// Cost note: this adds one --depth=1 fetch per present branch-pinned plugin per
// reconcile. The reconcile fires on transition-to-online (not a tight loop), and
// the set is small (chiefly the concierge management-MCP + any user branch pins),
// so the cost is bounded; a TTL cache is a documented follow-up if it ever bites.
func (h *PluginsHandler) pluginFragmentStale(ctx context.Context, sourceRaw, installedSHA string) bool {
	if installedSHA == "" {
		return false // no baseline — can't tell if it moved; never churn
	}
	if trackFromSource(sourceRaw) != "none" {
		return false // tag:/sha: pin is immutable and owned by the drift sweeper
	}
	cur, err := resolveSourceSHA(ctx, h.sources, sourceRaw)
	if err != nil || cur == "" {
		return false // transient resolve failure — self-heals next beat, never churn
	}
	if cur == installedSHA {
		return false // up-to-date
	}
	log.Printf("Plugin reconcile: fragment change detected source=%s installed=%s upstream=%s",
		sourceRaw, shortSHA(installedSHA), shortSHA(cur))
	return true
}

// PluginFragmentStaleForWorkspace reports whether a specific installed plugin on
// a workspace is behind its branch tip. Exported for the fragment-changed
// trigger (fix (c)), which uses it to decide — BEFORE firing the reconcile
// (which re-records the SHA) — whether a box's fragment actually moved and so
// warrants a deliberate restart. Fail-closed to not-stale on any load error,
// missing row, immutable pin, or unchanged tip (same posture as
// pluginFragmentStale), so the trigger never restarts a box that didn't change.
func (h *PluginsHandler) PluginFragmentStaleForWorkspace(ctx context.Context, workspaceID, pluginName string) bool {
	installed, err := listInstalledPluginRecords(ctx, workspaceID)
	if err != nil {
		return false
	}
	rec, ok := installed[pluginName]
	if !ok {
		return false
	}
	return h.pluginFragmentStale(ctx, rec.SourceRaw, rec.InstalledSHA)
}

// pluginPresentOnBox reports whether the plugin is already installed on the
// workspace box — e.g. delivered by the runtime-image boot-install
// (RFC#2843 #32) before this online-transition reconcile runs. Used to skip the
// redundant EIC re-deliver + restart that caused the per-fresh-workspace
// reprovision churn (#38), and to detect stale workspace_plugins rows on a
// fresh/de-baked box. CONSERVATIVE by design: returns false on any uncertainty
// (no container / no instance / read error / empty manifest), so the caller
// falls back to delivering — a genuinely-missing install is never silently
// skipped, only a confirmed-present one is deduped.
//
// For the concierge's management MCP (molecule-ai-plugin-molecule-platform-mcp),
// file presence is not enough. The executor's native runtime MCP config
// (Claude settings.json, Codex config.toml, OpenClaw openclaw.json, Hermes
// config.yaml, etc.) must actually contain the molecule-platform server entry;
// on a de-baked SaaS box the plugin files can be present while the runtime
// config was generated before the plugin landed, so a restart is required.
// This check uses EIC / docker exec, never the nil local-Docker provisioner
// backend, so SaaS tenants without a Docker daemon still recover.
func (h *PluginsHandler) pluginPresentOnBox(ctx context.Context, workspaceID, pluginName string) bool {
	var containerName, instanceID, runtime string
	// Local Docker path: if the workspace container is running, read its
	// /configs/plugins/<name>/plugin.yaml directly. This covers local/dev boxes
	// where there is no EC2 instance to probe via EIC.
	if containerName = h.findRunningContainer(ctx, workspaceID); containerName != "" {
		out, err := h.execInContainer(ctx, containerName, []string{
			"cat", "/configs/plugins/" + pluginName + "/plugin.yaml",
		})
		if err != nil || len(out) == 0 {
			return false // not present or unreadable on the box
		}
		if pluginName == conciergePlatformMCPName &&
			!h.managementMCPRuntimeConfigPresent(ctx, workspaceID, containerName, "", "") {
			return false // plugin files present but runtime config hasn't picked it up
		}
		return true
	}

	// SaaS EC2 path: probe the remote instance via EIC SSH.
	instanceID, runtime = h.lookupSaaSDispatch(workspaceID)
	if instanceID == "" {
		return false // not a SaaS box we can probe — deliver as before
	}
	data, err := readPluginManifestViaEIC(ctx, instanceID, runtime, pluginName)
	if err != nil || len(data) == 0 {
		return false // can't confirm presence — fall back to deliver
	}
	if pluginName == conciergePlatformMCPName &&
		!h.managementMCPRuntimeConfigPresent(ctx, workspaceID, "", instanceID, runtime) {
		return false // plugin files present but runtime config hasn't picked it up
	}
	return true
}

// managementMCPRuntimeConfigPresent reports whether the executor's runtime
// config already references the molecule-platform MCP server. This closes both
// the post-de-bake gap where plugin files exist on disk but the runtime config
// was generated before they landed, and the cross-runtime gap where a healthy
// non-Claude concierge was mis-read through Claude's settings.json path. Probes
// via the same Docker/EIC primitives pluginPresentOnBox uses — no local
// provisioner backend required.
// readRuntimeConfigViaEIC is the testable hook for reading a runtime-config
// file from a SaaS workspace EC2. Production uses readFileViaEIC; tests stub it
// to avoid standing up AWS EIC tunnels.
var readRuntimeConfigViaEIC = func(ctx context.Context, instanceID, runtime, relPath string) ([]byte, error) {
	return readFileViaEIC(ctx, instanceID, runtime, "/configs", relPath)
}

type managementMCPConfigProbe struct {
	containerPath string
	eicRuntime    string
	eicRelPath    string
	hasServer     func([]byte) bool
}

var managementMCPConfigProbes = map[string]managementMCPConfigProbe{
	"claude_code": {
		containerPath: "/configs/.claude/settings.json",
		eicRuntime:    "claude-code",
		eicRelPath:    ".claude/settings.json",
		hasServer:     claudeConfigHasPlatformMCP,
	},
	"codex": {
		containerPath: "/home/agent/.codex/config.toml",
		eicRuntime:    "codex",
		eicRelPath:    "config.toml",
		hasServer:     codexConfigHasPlatformMCP,
	},
	"openclaw": {
		containerPath: "/home/agent/.openclaw/openclaw.json",
		eicRuntime:    "openclaw",
		eicRelPath:    "openclaw.json",
		hasServer:     openclawConfigHasPlatformMCP,
	},
	"hermes": {
		// The RUNTIME config start.sh renders lives at /tmp/.hermes (the
		// gateway runs with HOME=/tmp; /tmp/.hermes is a symlink onto the
		// persisted /configs/.hermes since 2026-07-19). /home/agent/.hermes
		// is the hermes INSTALL dir whose installer-seeded stock config never
		// gains the molecule entry — probing it made the reconciler think the
		// management MCP was absent on every cycle and re-deliver forever.
		containerPath: "/tmp/.hermes/config.yaml",
		eicRuntime:    "hermes",
		eicRelPath:    "config.yaml",
		hasServer:     hermesConfigHasPlatformMCP,
	},
}

func managementMCPConfigProbeFor(runtime string) (managementMCPConfigProbe, bool) {
	key := strings.ToLower(strings.TrimSpace(runtime))
	key = strings.ReplaceAll(key, "-", "_")
	if key == "" {
		key = "claude_code" // preserves the legacy behavior when runtime lookup is not wired.
	}
	probe, ok := managementMCPConfigProbes[key]
	return probe, ok
}

func (h *PluginsHandler) managementMCPRuntimeConfigPresent(ctx context.Context, workspaceID, containerName, instanceID, runtime string) bool {
	if strings.TrimSpace(runtime) == "" && h.runtimeLookup != nil && workspaceID != "" {
		if rt, err := h.runtimeLookup(workspaceID); err == nil {
			runtime = rt
		}
	}
	probe, ok := managementMCPConfigProbeFor(runtime)
	if !ok {
		return false
	}
	var data []byte
	var err error
	if containerName != "" {
		var out string
		out, err = h.execInContainer(ctx, containerName, []string{"cat", probe.containerPath})
		data = []byte(out)
	} else if instanceID != "" {
		data, err = readRuntimeConfigViaEIC(ctx, instanceID, probe.eicRuntime, probe.eicRelPath)
	} else {
		return false
	}
	if err != nil || len(data) == 0 {
		return false
	}
	return probe.hasServer(data)
}

func claudeConfigHasPlatformMCP(data []byte) bool {
	var cfg struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return false
	}
	_, ok := cfg.MCPServers[conciergePlatformMCPServerName]
	return ok
}

func codexConfigHasPlatformMCP(data []byte) bool {
	var cfg struct {
		MCPServers map[string]map[string]any `toml:"mcp_servers"`
	}
	if toml.Unmarshal(data, &cfg) != nil {
		return false
	}
	_, ok := cfg.MCPServers[conciergePlatformMCPServerName]
	return ok
}

func openclawConfigHasPlatformMCP(data []byte) bool {
	var cfg struct {
		MCP struct {
			Servers map[string]json.RawMessage `json:"servers"`
		} `json:"mcp"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return false
	}
	_, ok := cfg.MCP.Servers[conciergePlatformMCPServerName]
	return ok
}

func hermesConfigHasPlatformMCP(data []byte) bool {
	var cfg struct {
		MCPServers map[string]any `yaml:"mcp_servers"`
	}
	if yaml.Unmarshal(data, &cfg) != nil {
		return false
	}
	_, ok := cfg.MCPServers[conciergePlatformMCPServerName]
	return ok
}

// cleanup removes the staged tempdir. Mirrors the defer the interactive
// install path uses; the reconcile must clean up explicitly because it stages
// many plugins in a loop rather than one-per-request.
func (s *stageResult) cleanup() {
	if s != nil && s.StagedDir != "" {
		_ = os.RemoveAll(s.StagedDir)
	}
}

// trackFromSource maps a source-contract ref to a workspace_plugins.tracked_ref
// value. Only tag:/sha: refs are tracked; a branch ref (e.g. "#main") or no
// ref → "none" (the sweeper can't meaningfully chase a branch tip via the
// tracked_ref model).
func trackFromSource(source string) string {
	src, err := plugins.ParseSource(source)
	if err != nil {
		return "none"
	}
	idx := strings.Index(src.Spec, "#")
	if idx < 0 || idx+1 >= len(src.Spec) {
		return "none"
	}
	ref := src.Spec[idx+1:]
	if strings.HasPrefix(ref, "tag:") && len(ref) > 4 {
		return ref
	}
	if strings.HasPrefix(ref, "sha:") && len(ref) > 4 {
		return ref
	}
	return "none"
}

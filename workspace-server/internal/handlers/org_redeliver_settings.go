package handlers

// RE-DELIVERING an EXISTING workspace's declared plugin settings on org import.
//
// THE GAP THIS CLOSES
// -------------------
// M6 wired an org node's `template:` into config generation — but ONLY on the
// CREATE path. Verified against production on 2026-07-28/29: there was no way
// to get a node's declared config onto a workspace that already existed.
// Every lever fails for a different reason:
//
//	workspace restart   re-provisions the container, reuses the STORED config
//	tenant redeploy     replaces the control plane, not workspace config
//	org re-import       hits ON CONFLICT and returns "skipped": true BEFORE
//	                    config generation — so the M6 wiring is never reached
//
// Consequence: a live customer workspace served the stock runtime template
// (`name: Hermes Agent`, no plugins, no schedules) while its org.yaml declared
// 11 schedules, and its volume-backed grid was empty. The declarations were
// correct; nothing carried them to the box.
//
// This makes /org/import the platform ability for that: re-importing an org
// RE-DELIVERS each existing node's declared `plugins[].config`. Post-M3 a
// schedule grid IS plugin config (`plugins[].config.schedules`), so there is
// nothing to re-render — the settings channel already carries it.
//
// WHY AN INTERFACE RATHER THAN THREADING TemplatesHandler
// -------------------------------------------------------
// The delivery primitive lives on *TemplatesHandler, but it needs only THREE
// of that struct's members — findContainer, copyFilesToContainer,
// writeViaEphemeral — all of them "write files into a workspace". It touches
// none of configsDir/cacheDir/wh/refreshCache. Threading the whole handler in
// would couple org import to four unrelated concerns and invite the next
// caller to reach through for them. So org import depends on the CAPABILITY.
//
// NIL-TOLERANT BY DESIGN: with no deliverer wired, every call here is a no-op
// and org import behaves byte-for-byte as before. That is what makes this safe
// to land on a path that provisions whole orgs — the blast radius is opt-in at
// the router, exactly like WithTemplateCacheDir.

import (
	"context"
	"log"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/plugins"
)

// pluginSettingsDeliverer writes one plugin's per-install settings into a LIVE
// workspace and arms the scheduler reload — no restart. *TemplatesHandler
// satisfies this as written.
type pluginSettingsDeliverer interface {
	deliverPluginSettings(ctx context.Context, workspaceID, installName string,
		settings map[string]any) (reloaded bool, err error)
}

// WithPluginSettingsDeliverer opts org import into re-delivering declared
// plugin settings to workspaces that already exist. Option-style so every
// existing NewOrgHandler call site keeps compiling and keeps today's behaviour
// until the router opts in.
func (h *OrgHandler) WithPluginSettingsDeliverer(d pluginSettingsDeliverer) *OrgHandler {
	h.settingsDeliverer = d
	return h
}

// redeliverDeclaredPluginSettings pushes every `plugins[].config` block this
// node declares to an ALREADY-EXISTING workspace.
//
// Best-effort per entry, and non-fatal overall: org import must never fail
// because one plugin's settings could not be delivered. Each outcome is logged
// with the install name so a silent no-delivery is visible in the import log
// rather than being indistinguishable from "nothing declared".
//
// Install-name keying is the landmine here and it is deliberate: settings are
// addressed by the INSTALL directory derived from the source
// (PluginNameFromSource — e.g. "molecule-ai-plugin-scheduler"), NOT by the
// manifest's own `name:` ("molecule-scheduler"). Keying on the manifest name
// writes a file nothing ever opens, and because a missing settings file is a
// clean no-op on the runtime side it fails SILENTLY.
func (h *OrgHandler) redeliverDeclaredPluginSettings(
	ctx context.Context, workspaceID, wsName string, entries []templatePluginEntry,
) (delivered int) {
	if h.settingsDeliverer == nil || len(entries) == 0 {
		return 0
	}
	for _, entry := range entries {
		if len(entry.Config) == 0 {
			continue
		}
		// "!name" / "-name" are REMOVALS in the merge grammar, not installs —
		// they carry no settings and must not be delivered.
		if entry.Source == "" || entry.Source[0] == '!' || entry.Source[0] == '-' {
			continue
		}
		installName, err := plugins.PluginNameFromSource(entry.Source)
		if err != nil {
			log.Printf("Org import: re-deliver skipped for %s — cannot derive install name from %q: %v",
				wsName, entry.Source, err)
			continue
		}
		reloaded, derr := h.settingsDeliverer.deliverPluginSettings(ctx, workspaceID, installName, entry.Config)
		if derr != nil {
			log.Printf("Org import: re-deliver FAILED for %s plugin %s (non-fatal): %v", wsName, installName, derr)
			continue
		}
		delivered++
		log.Printf("Org import: re-delivered settings for %s plugin %s (scheduler reload armed=%t)",
			wsName, installName, reloaded)
	}
	return delivered
}

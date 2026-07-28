package handlers

// The plugin-settings WRITER — layer 6's load-bearing half.
//
// Everything else in the layered-config design (RFC M4) is bookkeeping around
// one question: can a settings change reach a RUNNING workspace? Four previous
// attempts at layer 6 failed by designing the storage and the API first and
// assuming this part. So it is built first, on its own, and proven against a
// real container before any column or route depends on it.
//
// WHY NOT THE FILES API. `PUT /workspaces/:id/files/*path` can already write
// into /configs, and it is the wrong tool here: its side effect is
// `maybeRestartAfterFileWrite` → a workspace RESTART. A live settings edit
// wants the opposite — the file changes and the DAEMON RE-READS IT, with no
// restart and no lost session. The runtime resolves settings at daemon
// DISCOVERY time (molecule_runtime/plugin_daemons.discover_daemon_specs), and
// `POST /internal/daemons/reload` re-runs exactly that path, which is what
// makes a no-restart settings change possible at all.
//
// So this reuses the same three-way backend dispatch the Files API uses — EC2
// via EIC, a running local container via docker cp, an offline container via an
// ephemeral mount — and pairs it with a daemon reload instead of a restart.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"path"
	"sort"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// pluginSettingsRelPath is the /configs-relative path of one plugin's settings.
// It is keyed on the INSTALL name — the /configs/plugins/<name>/ directory,
// derived from the plugin source repo — NOT the manifest's own `name:`. Core
// writes this file and the runtime reads it back with the same key
// (molecule_runtime/plugin_settings.settings_path). Keying it on the manifest
// name would write a file nothing reads, and because a missing settings file is
// a clean no-op on the runtime side, that failure would be SILENT.
func pluginSettingsRelPath(installName string) (string, error) {
	if installName == "" || installName != path.Base(installName) || installName == "." || installName == ".." {
		return "", fmt.Errorf("unsafe plugin install name %q", installName)
	}
	return path.Join(pluginSettingsDirName, installName+".json"), nil
}

// renderPluginSettingsJSON encodes one plugin's effective settings exactly as
// the provision-time renderer does, so a live write and a re-provision produce
// byte-identical content for the same values. Keys are sorted by
// encoding/json's map ordering, and the trailing newline matches
// renderPluginSettingsFiles.
func renderPluginSettingsJSON(settings map[string]any) ([]byte, error) {
	if settings == nil {
		settings = map[string]any{}
	}
	body, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("plugin settings are not JSON-encodable: %w", err)
	}
	body = append(body, '\n')
	if len(body) > maxPluginSettingsBytes {
		return nil, fmt.Errorf("plugin settings are %d bytes, over the %d cap", len(body), maxPluginSettingsBytes)
	}
	return body, nil
}

// writePluginSettingsToWorkspace puts one plugin's settings file on a
// workspace's config volume, whichever backend that workspace runs on.
//
// It deliberately does NOT reload the daemon: the caller decides, because the
// durable write and the best-effort arm have different failure semantics. A
// write failure is an error the operator must see; a failed reload is
// recoverable (the next boot or reconcile picks the file up).
func (h *TemplatesHandler) writePluginSettingsToWorkspace(
	ctx context.Context, workspaceID, installName string, settings map[string]any,
) error {
	rel, err := pluginSettingsRelPath(installName)
	if err != nil {
		return err
	}
	body, err := renderPluginSettingsJSON(settings)
	if err != nil {
		return err
	}
	files := map[string]string{rel: string(body)}

	// Backend selection mirrors WriteFile's dispatch exactly (templates.go):
	// a real EC2 instance id routes to the EIC tunnel, while a local-docker
	// workspace persists its container NAME in instance_id and must NOT.
	var instanceID, runtime string
	if db.DB != nil {
		if err := db.DB.QueryRowContext(ctx,
			`SELECT COALESCE(instance_id, ''), COALESCE(runtime, '') FROM workspaces WHERE id = $1`,
			workspaceID,
		).Scan(&instanceID, &runtime); err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("plugin settings write: workspace lookup for %s: %w", workspaceID, err)
		}
	}

	// 1. EC2 — write through the EIC tunnel. /configs is the only root this
	//    writer ever targets; settings never live outside it.
	if isEC2InstanceID(instanceID) {
		if err := writeFileViaEIC(ctx, instanceID, runtime, "/configs", rel, body); err != nil {
			return fmt.Errorf("plugin settings write (EIC) for %s: %w", workspaceID, err)
		}
		return nil
	}

	// 2. Local docker, container UP — docker cp into the live /configs. This is
	//    the path that matters for a no-restart edit: the file changes
	//    underneath a running daemon.
	if containerName := h.findContainer(ctx, workspaceID); containerName != "" {
		if err := h.copyFilesToContainer(ctx, containerName, "/configs", files); err != nil {
			return fmt.Errorf("plugin settings write (container) for %s: %w", workspaceID, err)
		}
		return nil
	}

	// 3. Container down — mount the config volume in an ephemeral container (or
	//    the host-side mirror when there is no docker socket). The workspace
	//    picks the change up on its next boot.
	volName := provisioner.ConfigVolumeName(workspaceID)
	if err := h.writeViaEphemeral(ctx, volName, workspaceID, files); err != nil {
		return fmt.Errorf("plugin settings write (ephemeral) for %s: %w", workspaceID, err)
	}
	return nil
}

// deliverPluginSettings writes the file and then best-effort re-reads it into
// the running daemons. Returns whether the daemon actually picked it up, so a
// caller can tell the operator "saved and live" from "saved, applies on next
// boot" instead of implying the stronger one.
func (h *TemplatesHandler) deliverPluginSettings(
	ctx context.Context, workspaceID, installName string, settings map[string]any,
) (reloaded bool, err error) {
	if err := h.writePluginSettingsToWorkspace(ctx, workspaceID, installName, settings); err != nil {
		return false, err
	}
	// armSchedulerPlugin resolves the workspace URL + platform-inbound secret
	// and POSTs /internal/daemons/reload, which re-runs plugin discovery — the
	// same path that resolves ${config:} references. Named for the scheduler
	// because that was its first caller; it is the generic daemon-reload arm,
	// and it already returns false for a poll-mode/unregistered workspace.
	if !armSchedulerPlugin(ctx, workspaceID) {
		log.Printf("plugin settings: wrote %s for %s but the daemon reload did not confirm — the change applies on next boot", installName, workspaceID)
		return false, nil
	}
	return true, nil
}

// sortedSettingKeys is a small helper for deterministic logging — callers log
// which keys moved, and an unstable order makes those logs undiffable.
func sortedSettingKeys(settings map[string]any) []string {
	out := make([]string, 0, len(settings))
	for k := range settings {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

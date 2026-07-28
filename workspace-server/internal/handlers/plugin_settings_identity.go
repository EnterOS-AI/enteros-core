package handlers

// PINNING THE LOOKUP TO THE RESOLVED DESTINATION NAME.
//
// A plugin has TWO names and only ONE of them addresses its settings:
//
//	plugin.name           the INSTALL / destination directory, derived from the
//	                      source repo by plugins.PluginNameFromSource — e.g.
//	                      "molecule-ai-plugin-scheduler". Core writes
//	                      plugin-settings/<this>.json and the runtime reads it
//	                      back with the same key.
//	plugin.manifest.name  the manifest's own `name:` — e.g. "molecule-scheduler".
//	                      Daemon provenance for kind: channel / kind: trigger.
//	                      It addresses NOTHING in the settings channel.
//
// Both are live at once inside a single plugin, so they are easy to confuse.
// Before this file, `:plugin` from the URL was used verbatim as the DB key AND
// the settings filename. A caller that passed the MANIFEST name — the obvious
// mistake, because that is the name a plugin calls itself — got HTTP 200 and
// {"status":"saved"}, having written plugin-settings/molecule-scheduler.json,
// a file nothing on the box ever opens. It fails SILENTLY, because a missing
// settings file is a clean no-op by design on the runtime side.
//
// So the request name is now RESOLVED against the workspace's real install
// names before it is used for anything. An unresolvable name is refused, and
// the refusal NAMES the install names that would have worked — turning the
// milestone's worst failure class (a success response for work that did not
// happen) into a loud, actionable 404.
//
// UNINSTALL / REINSTALL SEMANTICS — decided here, deliberately:
//
//	An uninstall does NOT delete the plugin's settings row. Overrides are
//	RETAINED across uninstall/reinstall.
//
// Rationale. The override column is the OPERATOR'S INTENT, not the plugin's
// state; the plugin is the thing being removed, not the decision about how it
// should be configured. Uninstall+reinstall is also the routine remedy for a
// broken or drifted install — the reconcile itself replaces a stale install —
// so deleting overrides there would silently wipe an operator's settings during
// ordinary maintenance. That is exactly the "the edit died quietly" failure the
// two-column split exists to prevent; re-deriving `config` from the template
// while preserving `overrides` is the whole design.
//
// The residue this accepts is a stale override key that a reinstalled version
// no longer declares. That is benign and visible: the runtime resolves declared
// keys and ignores unknown ones, and GET still reports the key with layer
// "override" so an operator can see and clear it. A silently-lost setting is
// not recoverable; a visible stale key is. Rows are removed only when the
// workspace itself is deleted (ON DELETE CASCADE on workspace_id).
//
// Note that resolution keeps working across an uninstall: PluginsHandler.
// Uninstall deletes the workspace_plugins (installed) row and leaves
// workspace_declared_plugins (declared) intact, and this resolver accepts
// either. An operator can therefore still read and edit the settings of a
// declared plugin between its uninstall and the reconcile that puts it back —
// which is the point of retaining them.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// errPluginNotOnWorkspace is returned when the requested :plugin does not
// resolve to any install name on the workspace. Callers map it to 404.
var errPluginNotOnWorkspace = errors.New("plugin is not declared or installed on this workspace")

// workspacePluginInstallNames returns every INSTALL name known for a workspace:
// the DECLARED set (workspace_declared_plugins — desired state, written by
// seedTemplatePlugins/recordDeclaredPlugin) unioned with the INSTALLED set
// (workspace_plugins — current state, written by the install pipeline).
//
// Both tables key on the install/destination name, and both are populated only
// from plugins.PluginNameFromSource, so neither can contain a manifest-name
// alias. That is what makes them a trustworthy allow-list: the resolver cannot
// be taught an alias by a bad write.
//
// workspace_plugin_settings is deliberately NOT part of the union. It is the
// table this resolver protects; sourcing the allow-list from it would let the
// first mis-keyed PATCH legitimise its own alias for every PATCH after it.
func workspacePluginInstallNames(ctx context.Context, database *sql.DB, workspaceID string) (map[string]struct{}, error) {
	names := map[string]struct{}{}
	if database == nil {
		return names, nil
	}
	rows, err := database.QueryContext(ctx, `
		SELECT plugin_name FROM workspace_declared_plugins WHERE workspace_id = $1
		UNION
		SELECT plugin_name FROM workspace_plugins          WHERE workspace_id = $1
	`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("workspacePluginInstallNames: query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr != nil {
			return nil, fmt.Errorf("workspacePluginInstallNames: scan: %w", scanErr)
		}
		if name = strings.TrimSpace(name); name != "" {
			names[name] = struct{}{}
		}
	}
	return names, rows.Err()
}

// resolvePluginInstallName pins a caller-supplied :plugin to the install name
// core actually keys the settings file and the settings row on.
//
// It returns the resolved name on an exact match, and errPluginNotOnWorkspace
// otherwise — never the requested name as a fallback. Falling back is what
// creates the phantom file: the whole value of this function is that it
// REFUSES rather than writes somewhere nothing reads.
func resolvePluginInstallName(
	ctx context.Context, database *sql.DB, workspaceID, requested string,
) (string, error) {
	if _, err := pluginSettingsRelPath(requested); err != nil {
		return "", err
	}
	known, err := workspacePluginInstallNames(ctx, database, workspaceID)
	if err != nil {
		return "", err
	}
	if _, ok := known[requested]; ok {
		return requested, nil
	}
	return "", errPluginNotOnWorkspace
}

// sortedNames renders an install-name set for an error body, so a caller who
// passed the manifest name is told the name that would have worked instead of
// being left to guess which of a plugin's two identities the API wanted.
func sortedNames(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

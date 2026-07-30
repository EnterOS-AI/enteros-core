package handlers

// plugin_settings_delivery.go — core's WRITER for per-install plugin settings.
//
// This is the other half of the slice landed in the runtime
// (molecule-ai-workspace-runtime, plugin_settings.py). The runtime resolves a
// plugin's DECLARED defaults (layer 1, which lives in the manifest on the box)
// and merges a delivered file over them; nothing there authors that file. This
// authors it.
//
//	template config.yaml            core (here)                    runtime
//	  plugins:                        renders one JSON per          resolves layer 1
//	    - source: gitea://…#sha   ->  plugin into ConfigFiles   ->  from the manifest,
//	      config: {timezone: …}       plugin-settings/<n>.json      merges this over it
//	                                                                -> DaemonSpec.env
//
// WHY THIS PATH
//
// ConfigFiles is core-GENERATED content, distinct from TemplateAssets
// (template-FETCHED). Both provisioner legs already carry nested paths:
// buildConfigFilesTar creates parent tar dir headers, and the CP addFile
// path-cleans and rejects traversal. `system-prompt.md` is the existing
// precedent for core adding a non-config.yaml file here.
//
// Settings deliberately do NOT go to /configs/plugins/<name>/ — the install
// pipeline owns that directory and re-stages it (EIC does a literal `rm -rf`),
// so anything written there dies on the next reconcile.
//
// SCOPE — layers 2..5 only, at provision time.
//
// Layer 1 is NOT rendered here and cannot be: core holds no plugin manifest at
// provision time (plugins install post-online via the reconcile, strictly after
// this file ships). The runtime supplies it. Layer 6 (live operator overrides)
// is not built — see the RFC; this writer is the provision-time leg only.
//
// A template that declares no plugin config produces NO file and leaves
// ConfigFiles byte-identical, so every existing workspace is unaffected.

import (
	"encoding/json"
	"fmt"
	"log"
	"path"
	"sort"

	"gopkg.in/yaml.v3"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/plugins"
)

// pluginSettingsDirName mirrors PLUGIN_SETTINGS_DIRNAME in the runtime's
// plugin_settings.py. Both sides must agree or the file lands where nothing
// reads it.
const pluginSettingsDirName = "plugin-settings"

// maxPluginSettingsBytes bounds ONE plugin's rendered settings. The aggregate
// ConfigFiles budget is enforced separately and is CP-only
// (cpConfigFilesMaxBytes, 256 KiB, fail-closed); the local-Docker leg has no
// cap. This per-entry bound keeps one hostile template from consuming the whole
// aggregate before the provisioner ever sees it.
const maxPluginSettingsBytes = 64 << 10

// templatePluginEntry is one `plugins:` entry. It accepts BOTH the historical
// bare-string form and the object form carrying config:
//
//	plugins:
//	  - gitea://owner/repo#sha                    # unchanged, still valid
//	  - source: gitea://owner/repo#sha
//	    config: {timezone: America/Vancouver}
//
// Widening rather than replacing is deliberate: every template in the fleet
// uses the string form today and must keep parsing byte-identically.
type templatePluginEntry struct {
	Source string
	Config map[string]any
}

// UnmarshalYAML accepts a scalar (string → source only) or a mapping.
func (e *templatePluginEntry) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		return value.Decode(&e.Source)
	}
	var alt struct {
		Source string         `yaml:"source"`
		Config map[string]any `yaml:"config"`
	}
	if err := value.Decode(&alt); err != nil {
		return fmt.Errorf("plugin entry must be a source string or {source, config}: %w", err)
	}
	if alt.Source == "" {
		return fmt.Errorf("plugin entry object is missing `source`")
	}
	e.Source, e.Config = alt.Source, alt.Config
	return nil
}

// UnmarshalJSON mirrors UnmarshalYAML: a JSON string is a bare source, a JSON
// object is {source, config}.
//
// This is NOT symmetry for its own sake — it is load-bearing. `plugins:` is
// decoded from YAML on the file path but from JSON on POST /org/import with an
// INLINE template (OrgTemplate binds via c.ShouldBindJSON). #4944 changed
// OrgDefaults/OrgWorkspace.Plugins from []string to []templatePluginEntry and
// gave the type only an UnmarshalYAML, so the inline-import path started
// rejecting the BARE-STRING form that every template in the fleet uses:
//
//	json: cannot unmarshal string into Go struct field
//	OrgDefaults.plugins of type handlers.templatePluginEntry
//
// Found by actually exercising /org/import against a live tenant, whose
// defaults were `plugins: [browser-automation]` — the whole import 400'd with
// "invalid request body". The object form decoded fine, which is why unit tests
// over the YAML path stayed green: a widened type is only as wide as its
// NARROWEST decoder.
func (e *templatePluginEntry) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		e.Source = s
		return nil
	}
	var alt struct {
		Source string         `json:"source"`
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(data, &alt); err != nil {
		return fmt.Errorf("plugin entry must be a source string or {source, config}: %w", err)
	}
	if alt.Source == "" {
		return fmt.Errorf("plugin entry object is missing `source`")
	}
	e.Source, e.Config = alt.Source, alt.Config
	return nil
}

// MarshalJSON round-trips the same two forms so a re-serialised template stays
// loadable: config-less entries collapse back to a plain string.
func (e templatePluginEntry) MarshalJSON() ([]byte, error) {
	if len(e.Config) == 0 {
		return json.Marshal(e.Source)
	}
	return json.Marshal(struct {
		Source string         `json:"source"`
		Config map[string]any `json:"config"`
	}{e.Source, e.Config})
}

// pluginEntrySources projects entries down to the source strings the merge /
// dedup / opt-out layer works in. Kept as a projection rather than changing
// mergePlugins' signature: the "!"/"-" opt-out grammar, the collision rules and
// every existing caller stay exactly as they are, and the config half travels
// separately to renderPluginSettingsFiles. One type, two views.
func pluginEntrySources(entries []templatePluginEntry) []string {
	if len(entries) == 0 {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Source != "" {
			out = append(out, e.Source)
		}
	}
	return out
}

// templateConfigPluginEntries is the settings-aware view of the same
// `plugins:` block templateConfigPlugins reads. Kept separate so the existing
// declared-set parse is untouched.
type templateConfigPluginEntries struct {
	Plugins []templatePluginEntry `yaml:"plugins"`
}

// renderPluginSettingsFiles turns a template's `plugins:` block into the
// ConfigFiles entries that deliver each plugin's settings.
//
// Returns a map of RELATIVE path → JSON content, ready to merge into
// configFiles. A plugin with no `config:` produces no entry — so a template
// that uses only the string form yields an empty map and changes nothing.
//
// Per-entry validate-and-skip, matching the schedule renderer: one unusable
// entry never drops its siblings. Returns an error only when the block itself
// cannot be parsed, which callers should log and continue past (a broken
// plugins block must never block provisioning).
func renderPluginSettingsFiles(configYAML []byte, wsName string) (map[string][]byte, error) {
	if len(configYAML) == 0 {
		return nil, nil
	}
	var parsed templateConfigPluginEntries
	if err := yaml.Unmarshal(configYAML, &parsed); err != nil {
		return nil, fmt.Errorf("parse template plugins for settings: %w", err)
	}
	return renderPluginSettingsFromEntries(wsName, parsed.Plugins), nil
}

// templateConfigPluginEntriesFrom decodes a template config.yaml's `plugins:`
// block into entries, for callers that want to combine it with OTHER layers
// rather than render it alone.
//
// Fail-soft: an unparseable block yields no entries and a log line. A malformed
// template config must never block an org import — the same contract
// renderPluginSettingsFiles' caller already honours by logging and continuing.
func templateConfigPluginEntriesFrom(configYAML []byte, wsName string) []templatePluginEntry {
	if len(configYAML) == 0 {
		return nil
	}
	var parsed templateConfigPluginEntries
	if err := yaml.Unmarshal(configYAML, &parsed); err != nil {
		log.Printf("%s: plugin settings: cannot parse template plugins block: %v — template layer skipped", wsName, err)
		return nil
	}
	return parsed.Plugins
}

// renderPluginSettingsFromEntries is the same renderer addressed by ENTRIES
// rather than by a config.yaml document.
//
// Two callers need the identical projection from different starting points, and
// only one of them has a config.yaml to parse:
//
//	WorkspaceHandler.Create   holds the template's config.yaml bytes
//	POST /org/import          holds the org node's decoded `plugins:` lists and
//	                          NEVER writes them into the generated config.yaml
//
// That asymmetry is why the create half of org import silently produced no
// settings at all. Verified in production 2026-07-30 on the founder org: a node
// declaring `plugins[].config.schedules` imported to a workspace whose /configs
// carried no plugin-settings file, so the runtime's trigger-plugin seeding had
// nothing to read and the schedule grid came back `[]`. Re-importing the SAME
// template then delivered it via the skip path (#4947) — i.e. the first import
// lost the config and the second one fixed it, with nothing to tell an operator
// they had to run it twice.
//
// LAYERS are applied in order, last wins per install name, matching the
// TEMPLATE -> org DEFAULTS -> NODE precedence the declaration merge already
// uses. Passing one layer is the single-source case.
//
// Per-entry validate-and-skip: one unusable entry never drops its siblings.
func renderPluginSettingsFromEntries(wsName string, layers ...[]templatePluginEntry) map[string][]byte {
	out := map[string][]byte{}
	for _, layer := range layers {
		if len(layer) == 0 {
			continue
		}
		// Deterministic order so a re-provision produces byte-identical output
		// and does not churn the delivered bundle.
		entries := make([]templatePluginEntry, len(layer))
		copy(entries, layer)
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].Source < entries[j].Source })
		renderPluginSettingsLayer(out, wsName, entries)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// renderPluginSettingsLayer renders one already-sorted layer into out.
func renderPluginSettingsLayer(out map[string][]byte, wsName string, entries []templatePluginEntry) {
	for _, entry := range entries {
		if len(entry.Config) == 0 {
			continue
		}
		// The opt-out markers the merge layer understands ("!name" / "-name")
		// are removals, not installs — they carry no settings.
		if len(entry.Source) > 0 && (entry.Source[0] == '!' || entry.Source[0] == '-') {
			continue
		}
		name, err := plugins.PluginNameFromSource(entry.Source)
		if err != nil {
			log.Printf("%s: plugin settings: cannot derive install name from %q: %v — skipping", wsName, entry.Source, err)
			continue
		}
		// The install name is the /configs/plugins/<name>/ key AND the settings
		// filename. Both sides key on it; a name with a separator would escape
		// the settings dir.
		if name == "" || name != path.Base(name) {
			log.Printf("%s: plugin settings: refusing unsafe install name %q — skipping", wsName, name)
			continue
		}
		body, err := json.MarshalIndent(entry.Config, "", "  ")
		if err != nil {
			log.Printf("%s: plugin settings: %s config is not JSON-encodable: %v — skipping", wsName, name, err)
			continue
		}
		if len(body) > maxPluginSettingsBytes {
			log.Printf("%s: plugin settings: %s config is %d bytes (cap %d) — skipping", wsName, name, len(body), maxPluginSettingsBytes)
			continue
		}
		out[path.Join(pluginSettingsDirName, name+".json")] = append(body, '\n')
	}
}

// mergePluginSettingsIntoConfigFiles adds rendered settings to a ConfigFiles
// map, allocating it only when there is something to add. Returns the map and
// how many files were added, so the caller can log a real count.
func mergePluginSettingsIntoConfigFiles(configFiles map[string][]byte, rendered map[string][]byte) (map[string][]byte, int) {
	if len(rendered) == 0 {
		return configFiles, 0
	}
	if configFiles == nil {
		configFiles = map[string][]byte{}
	}
	for name, body := range rendered {
		configFiles[name] = body
	}
	return configFiles, len(rendered)
}

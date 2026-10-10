package handlers

// Proves the Create path actually DELIVERS plugin settings, not merely that the
// renderer can produce them.
//
// The renderer had 13 tests before this and none of them proved a workspace
// would ever receive a file — the call site was deliberately absent so neither
// repo referenced the other's unmerged code. This closes that gap by exercising
// the exact sequence Create runs: read the template's config.yaml → render →
// merge into the ConfigFiles map handed to the provisioner.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeSettingsTemplate lays down a template dir the way resolveTemplateDir would find it.
func writeSettingsTemplate(t *testing.T, configYAML string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write template config.yaml: %v", err)
	}
	return dir
}

// deliverFromTemplate mirrors the Create call site verbatim.
func deliverFromTemplate(t *testing.T, templatePath string, configFiles map[string][]byte) (map[string][]byte, int) {
	t.Helper()
	base, err := os.ReadFile(filepath.Join(templatePath, "config.yaml"))
	if err != nil {
		t.Fatalf("read template config.yaml: %v", err)
	}
	rendered, rErr := renderPluginSettingsFiles(base, "test-ws")
	if rErr != nil {
		t.Fatalf("render: %v", rErr)
	}
	return mergePluginSettingsIntoConfigFiles(configFiles, rendered)
}

func TestCallSite_DeliversSettingsForAConfiguredPlugin(t *testing.T) {
	tmpl := writeSettingsTemplate(t, `
name: Demo
runtime: openclaw
plugins:
  - source: gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0
    config:
      timezone: America/Vancouver
      max_concurrent: 3
`)
	files, n := deliverFromTemplate(t, tmpl, nil)
	if n != 1 {
		t.Fatalf("expected 1 settings file delivered, got %d", n)
	}
	body, ok := files["plugin-settings/molecule-ai-plugin-scheduler.json"]
	if !ok {
		t.Fatalf("settings not keyed on the install name; got %v", settingsFileNames(files))
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("delivered settings must be valid JSON: %v", err)
	}
	if got["timezone"] != "America/Vancouver" || got["max_concurrent"] != float64(3) {
		t.Errorf("delivered values wrong: %v", got)
	}
}

// The property every existing workspace depends on. Every template in the fleet
// uses the bare-string form; the Create path must produce a byte-identical
// provision for them — configFiles stays nil, not an allocated empty map.
func TestCallSite_TemplateWithoutPluginConfigIsByteIdentical(t *testing.T) {
	tmpl := writeSettingsTemplate(t, `
name: Demo
runtime: openclaw
plugins:
  - gitea://molecule-ai/molecule-ai-plugin-superpowers#a009fc91a83b1d7bc03a60e267ab966bc43ceb4e
  - ecc
`)
	files, n := deliverFromTemplate(t, tmpl, nil)
	if n != 0 {
		t.Fatalf("bare-string plugins must deliver nothing, got %d: %v", n, settingsFileNames(files))
	}
	if files != nil {
		t.Fatalf("configFiles must stay nil so the provision is byte-identical, got %v", settingsFileNames(files))
	}
}

func TestCallSite_NoPluginsBlockAtAll(t *testing.T) {
	tmpl := writeSettingsTemplate(t, "name: Demo\nruntime: openclaw\n")
	files, n := deliverFromTemplate(t, tmpl, nil)
	if n != 0 || files != nil {
		t.Fatalf("no plugins block -> no delivery, nil map; got %d / %v", n, settingsFileNames(files))
	}
}

// Settings must not disturb what the schedules render and the persona write
// already put in the map — the Create path runs all three against one map.
func TestCallSite_CoexistsWithConfigYAMLAndPersona(t *testing.T) {
	tmpl := writeSettingsTemplate(t, `
plugins:
  - source: gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0
    config: {timezone: UTC}
`)
	existing := map[string][]byte{
		"config.yaml":      []byte("name: Demo\nschedules:\n  - name: x\n"),
		"system-prompt.md": []byte("you are a demo agent"),
	}
	files, n := deliverFromTemplate(t, tmpl, existing)
	if n != 1 {
		t.Fatalf("expected 1 delivered, got %d", n)
	}
	if string(files["config.yaml"]) != "name: Demo\nschedules:\n  - name: x\n" {
		t.Errorf("settings delivery disturbed config.yaml")
	}
	if string(files["system-prompt.md"]) != "you are a demo agent" {
		t.Errorf("settings delivery disturbed system-prompt.md")
	}
}

// On SaaS the fetched bytes ARE the real template; templatePath may be a
// <runtime>-default fallback that never declared these plugins. So the fetched
// render must win on collision.
func TestCallSite_FetchedRenderWinsOnCollision(t *testing.T) {
	local := writeSettingsTemplate(t, `
plugins:
  - source: gitea://molecule-ai/plug/demo#v1
    config: {origin: local}
`)
	files, _ := deliverFromTemplate(t, local, nil)

	fetched := []byte("plugins:\n  - source: gitea://molecule-ai/plug/demo#v1\n    config: {origin: fetched}\n")
	rendered, err := renderPluginSettingsFiles(fetched, "test-ws")
	if err != nil {
		t.Fatalf("render fetched: %v", err)
	}
	files, _ = mergePluginSettingsIntoConfigFiles(files, rendered)

	var got map[string]any
	if err := json.Unmarshal(files["plugin-settings/demo.json"], &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["origin"] != "fetched" {
		t.Errorf("fetched render must win on collision, got origin=%v", got["origin"])
	}
}

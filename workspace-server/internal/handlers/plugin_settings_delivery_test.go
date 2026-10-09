package handlers

// Tests for core's plugin-settings WRITER.
//
// The load-bearing property is backward compatibility: every template in the
// fleet uses the bare-string `plugins:` form, and widening the parse must leave
// them byte-identical. Several tests below exist only to pin that.
//
// The cross-repo contract test is the one that would catch the worst failure:
// core writing to a path the runtime does not read. Both sides hardcode
// "plugin-settings"; if either moves, settings silently reach nothing — which is
// exactly the silent-drop class this whole workstream exists to remove.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderPluginSettings_BareStringFormProducesNothing(t *testing.T) {
	// Every template in the fleet today. Must stay byte-identical.
	cfg := []byte(`
name: Demo
plugins:
  - gitea://molecule-ai/molecule-ai-plugin-superpowers#a009fc91a83b1d7bc03a60e267ab966bc43ceb4e
  - ecc
`)
	out, err := renderPluginSettingsFiles(cfg, "demo")
	if err != nil {
		t.Fatalf("bare-string plugins must parse: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("bare-string plugins must produce NO settings files, got %v", settingsFileNames(out))
	}
}

func TestRenderPluginSettings_NoPluginsBlockAtAll(t *testing.T) {
	out, err := renderPluginSettingsFiles([]byte("name: Demo\nruntime: openclaw\n"), "demo")
	if err != nil || len(out) != 0 {
		t.Fatalf("no plugins block -> no files, no error; got %v / %v", settingsFileNames(out), err)
	}
}

func TestRenderPluginSettings_ObjectFormRendersOneFilePerPlugin(t *testing.T) {
	cfg := []byte(`
plugins:
  - source: gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0
    config:
      timezone: America/Vancouver
      max_concurrent: 3
`)
	out, err := renderPluginSettingsFiles(cfg, "demo")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	body, ok := out["plugin-settings/molecule-ai-plugin-scheduler.json"]
	if !ok {
		t.Fatalf("expected settings keyed on the INSTALL name; got %v", settingsFileNames(out))
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("delivered settings must be valid JSON: %v", err)
	}
	if got["timezone"] != "America/Vancouver" {
		t.Errorf("timezone = %v", got["timezone"])
	}
	if got["max_concurrent"] != float64(3) {
		t.Errorf("max_concurrent = %v (%T)", got["max_concurrent"], got["max_concurrent"])
	}
}

func TestRenderPluginSettings_MixedFormsCoexist(t *testing.T) {
	cfg := []byte(`
plugins:
  - ecc
  - source: gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0
    config: {timezone: UTC}
  - gitea://molecule-ai/molecule-ai-plugin-superpowers#a009fc9
`)
	out, err := renderPluginSettingsFiles(cfg, "demo")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("only the configured plugin gets a file; got %v", settingsFileNames(out))
	}
}

func TestRenderPluginSettings_ObjectWithoutConfigProducesNothing(t *testing.T) {
	cfg := []byte("plugins:\n  - source: gitea://o/r#sha\n")
	out, _ := renderPluginSettingsFiles(cfg, "demo")
	if len(out) != 0 {
		t.Fatalf("source-only object must produce no file; got %v", settingsFileNames(out))
	}
}

func TestRenderPluginSettings_OptOutMarkersCarryNoSettings(t *testing.T) {
	for _, src := range []string{"!molecule-audit-trail", "-molecule-audit-trail"} {
		cfg := []byte("plugins:\n  - source: \"" + src + "\"\n    config: {a: 1}\n")
		out, _ := renderPluginSettingsFiles(cfg, "demo")
		if len(out) != 0 {
			t.Errorf("opt-out %q is a removal, not an install; got %v", src, settingsFileNames(out))
		}
	}
}

func TestRenderPluginSettings_OversizeConfigIsSkippedNotTruncated(t *testing.T) {
	cfg := []byte("plugins:\n  - source: gitea://o/r/big#sha\n    config:\n      blob: \"" +
		strings.Repeat("x", maxPluginSettingsBytes+100) + "\"\n")
	out, err := renderPluginSettingsFiles(cfg, "demo")
	if err != nil {
		t.Fatalf("an oversize entry must be skipped, not error the block: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("oversize config must be skipped; got %v", settingsFileNames(out))
	}
}

func TestRenderPluginSettings_OneBadEntryDoesNotDropItsSiblings(t *testing.T) {
	cfg := []byte(`
plugins:
  - source: "!!!not a parseable source"
    config: {a: 1}
  - source: gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0
    config: {timezone: UTC}
`)
	out, err := renderPluginSettingsFiles(cfg, "demo")
	if err != nil {
		t.Fatalf("per-entry skip, not a block-level error: %v", err)
	}
	if _, ok := out["plugin-settings/molecule-ai-plugin-scheduler.json"]; !ok {
		t.Fatalf("the good sibling must survive; got %v", settingsFileNames(out))
	}
}

func TestRenderPluginSettings_Deterministic(t *testing.T) {
	cfg := []byte(`
plugins:
  - source: gitea://o/r/zzz#sha
    config: {k: 1}
  - source: gitea://o/r/aaa#sha
    config: {k: 2}
`)
	first, _ := renderPluginSettingsFiles(cfg, "demo")
	for i := 0; i < 5; i++ {
		again, _ := renderPluginSettingsFiles(cfg, "demo")
		for k, v := range first {
			if string(again[k]) != string(v) {
				t.Fatalf("re-provision must produce byte-identical output for %s", k)
			}
		}
	}
}

func TestMergePluginSettings_NilConfigFilesUntouchedWhenNothingToAdd(t *testing.T) {
	files, n := mergePluginSettingsIntoConfigFiles(nil, nil)
	if files != nil || n != 0 {
		t.Fatalf("no settings must leave ConfigFiles nil (byte-identical provision); got %v / %d", files, n)
	}
}

func TestMergePluginSettings_AllocatesOnlyWhenNeeded(t *testing.T) {
	files, n := mergePluginSettingsIntoConfigFiles(nil, map[string][]byte{"plugin-settings/a.json": []byte("{}")})
	if n != 1 || files == nil || files["plugin-settings/a.json"] == nil {
		t.Fatalf("expected one added file; got %v / %d", settingsFileNames(files), n)
	}
}

func TestMergePluginSettings_DoesNotClobberExistingConfigFiles(t *testing.T) {
	existing := map[string][]byte{"config.yaml": []byte("name: x"), "system-prompt.md": []byte("hi")}
	files, _ := mergePluginSettingsIntoConfigFiles(existing, map[string][]byte{"plugin-settings/a.json": []byte("{}")})
	if string(files["config.yaml"]) != "name: x" || string(files["system-prompt.md"]) != "hi" {
		t.Fatalf("settings delivery must not disturb existing entries: %v", settingsFileNames(files))
	}
}

// The cross-repo contract. Core WRITES to this directory; the runtime READS it
// (molecule_runtime/plugin_settings.py PLUGIN_SETTINGS_DIRNAME). If either side
// moves, settings silently reach nothing — the exact silent-drop class this
// workstream exists to remove. Pin it on the core side.
func TestPluginSettingsDirName_MatchesTheRuntimeContract(t *testing.T) {
	if pluginSettingsDirName != "plugin-settings" {
		t.Fatalf("pluginSettingsDirName = %q; the runtime's plugin_settings.PLUGIN_SETTINGS_DIRNAME "+
			"is \"plugin-settings\". Changing one side without the other means core writes "+
			"where nothing reads.", pluginSettingsDirName)
	}
}

func settingsFileNames(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

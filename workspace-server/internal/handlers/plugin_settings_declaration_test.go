package handlers

// M5: the plugin tab renders a form for a plugin the frontend has never seen.
//
// The whole point is that nothing here is plugin-specific. These tests use a
// manifest the frontend has no knowledge of and assert the declaration is
// sufficient to build a form from — and that `.example` generation can never
// leak a credential.

import (
	"context"
	"strings"
	"testing"
)

// The REAL molecule-scheduler manifest shape, as merged in
// molecule-ai-plugin-scheduler#5 and #6.
const realSchedulerManifest = `
name: molecule-scheduler
version: 0.2.0
description: Default platform scheduler
kind: trigger
contributes:
  configuration:
    title: Scheduler
    description: Tuning for the per-workspace scheduling daemon.
    properties:
      poll_seconds:
        type: integer
        default: 30
        description: How often the daemon scans the grid for due schedules.
      schedules:
        type: array
        default: []
        description: Schedules this install seeds into the workspace's grid.
  daemons:
    - name: scheduler
      command: python
      args: [scheduler.py]
`

// A plugin core has never heard of, with every rendering hint exercised.
const unknownPluginManifest = `
name: some-third-party-plugin
version: 9.9.9
description: core has never seen this
kind: trigger
contributes:
  configuration:
    title: Third Party
    description: Settings for a plugin added after the build.
    properties:
      api_key:
        type: string
        sensitive: true
        required: true
        description: Upstream API credential.
      region:
        type: string
        enum: [us-east-1, eu-west-1]
        default: us-east-1
        description: Which region to call.
      retries:
        type: integer
        default: 3
      verbose:
        type: boolean
        default: false
`

func TestDeclaration_ParsesTheRealSchedulerManifest(t *testing.T) {
	decl, err := parsePluginDeclaration([]byte(realSchedulerManifest))
	if err != nil {
		t.Fatal(err)
	}
	if decl.Title != "Scheduler" {
		t.Errorf("title = %q", decl.Title)
	}
	if len(decl.Properties) != 2 {
		t.Fatalf("expected 2 declared keys, got %d: %+v", len(decl.Properties), decl.Properties)
	}
	// Deterministic order — a form whose fields reshuffle is unusable.
	if decl.Properties[0].Key != "poll_seconds" || decl.Properties[1].Key != "schedules" {
		t.Errorf("properties not in stable key order: %v %v", decl.Properties[0].Key, decl.Properties[1].Key)
	}
	if decl.Properties[0].Default != 30 {
		t.Errorf("poll_seconds default = %v, want 30", decl.Properties[0].Default)
	}
}

// THE DONE CONDITION: a form can be built for a plugin the frontend has never
// seen, from the declaration alone.
func TestDeclaration_IsSufficientToRenderAFormForAnUnknownPlugin(t *testing.T) {
	decl, err := parsePluginDeclaration([]byte(unknownPluginManifest))
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]declaredProperty{}
	for _, p := range decl.Properties {
		byKey[p.Key] = p
	}

	// A masked field, because a plaintext box for a credential is the bug.
	if !byKey["api_key"].Sensitive {
		t.Error("api_key must be marked sensitive so the tab renders a reference picker")
	}
	if !byKey["api_key"].Required {
		t.Error("api_key required flag lost")
	}
	// A select, not a free text box.
	if len(byKey["region"].Enum) != 2 {
		t.Errorf("region should carry its enum so the tab renders a select: %+v", byKey["region"])
	}
	// Typed inputs.
	if byKey["retries"].Type != "integer" || byKey["verbose"].Type != "boolean" {
		t.Errorf("types lost: %+v %+v", byKey["retries"], byKey["verbose"])
	}
	// Help text.
	if byKey["region"].Description == "" {
		t.Error("description lost — the tab has nothing to explain the field with")
	}
}

// Tolerant by contract: the manifest schema declares `configuration` as an open
// anyOf precisely so a malformed block cannot brick the plugin.
func TestDeclaration_MalformedBlockYieldsNoFormNotAnError(t *testing.T) {
	for _, manifest := range []string{
		"name: x\ncontributes:\n  configuration: not-an-object\n",
		"name: x\ncontributes:\n  configuration:\n    properties: nope\n",
		"name: x\ncontributes: {}\n",
		"name: x\n",
		"",
	} {
		decl, _ := parsePluginDeclaration([]byte(manifest))
		if decl.Properties == nil {
			t.Errorf("properties must be an empty slice, never nil (the tab renders it): %q", manifest)
		}
		if len(decl.Properties) != 0 {
			t.Errorf("expected no form for %q, got %+v", manifest, decl.Properties)
		}
	}
}

// --- .example generation ------------------------------------------------

// The load-bearing safety property: a sensitive key NEVER carries a value.
func TestExample_SensitiveKeysCarryAPlaceholderNeverAValue(t *testing.T) {
	decl, err := parsePluginDeclaration([]byte(unknownPluginManifest))
	if err != nil {
		t.Fatal(err)
	}
	out := renderSettingsExample("some-third-party-plugin", decl)

	if !strings.Contains(out, `api_key: "<API_KEY>"`) {
		t.Errorf("sensitive key should render a placeholder:\n%s", out)
	}
	if !strings.Contains(out, "SENSITIVE") {
		t.Error("the example should say why the value is withheld")
	}
	// Non-sensitive keys DO carry usable values — an example nobody can copy is
	// pointless.
	if !strings.Contains(out, `region: "us-east-1"`) {
		t.Errorf("non-sensitive key should carry its default:\n%s", out)
	}
	if !strings.Contains(out, "retries: 3") || !strings.Contains(out, "verbose: false") {
		t.Errorf("typed defaults should be rendered:\n%s", out)
	}
	if !strings.Contains(out, "one of: us-east-1, eu-west-1") {
		t.Errorf("enum should be documented:\n%s", out)
	}
}

// Even if a plugin author puts a default on a sensitive key, it must not be
// echoed — a sensitive default is still a secret-shaped string.
func TestExample_SensitiveDefaultIsNeverEchoed(t *testing.T) {
	decl, err := parsePluginDeclaration([]byte(`
name: leaky
contributes:
  configuration:
    properties:
      token:
        type: string
        sensitive: true
        default: "sk-live-REAL-LOOKING-SECRET"
`))
	if err != nil {
		t.Fatal(err)
	}
	out := renderSettingsExample("leaky", decl)
	if strings.Contains(out, "sk-live-REAL-LOOKING-SECRET") {
		t.Fatalf("a sensitive DEFAULT leaked into the example:\n%s", out)
	}
	if !strings.Contains(out, `token: "<TOKEN>"`) {
		t.Errorf("expected a placeholder:\n%s", out)
	}
}

// The generator takes only the declaration — it has no access to live settings
// by construction. This pins that shape so a future refactor cannot quietly
// hand it real values.
func TestExample_GeneratorCannotSeeLiveValues(t *testing.T) {
	decl, err := parsePluginDeclaration([]byte(realSchedulerManifest))
	if err != nil {
		t.Fatal(err)
	}
	out := renderSettingsExample("molecule-scheduler", decl)
	if !strings.Contains(out, "poll_seconds: 30") {
		t.Errorf("declared default missing:\n%s", out)
	}
	// The scheduler's grid default is empty on purpose; the example must not
	// invent entries.
	if strings.Contains(out, "name:") {
		t.Errorf("example invented schedule entries:\n%s", out)
	}
}

func TestExample_IsValidYAMLShaped(t *testing.T) {
	decl, err := parsePluginDeclaration([]byte(unknownPluginManifest))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(renderSettingsExample("p", decl), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.Contains(trimmed, ":") {
			t.Errorf("non-comment line is not a key/value pair: %q", line)
		}
	}
}

func TestDeclaration_UnsafeInstallNameIsRefusedBeforeAnyRead(t *testing.T) {
	h := &TemplatesHandler{}
	for _, bad := range []string{"../escape", "a/b", "..", "", "/abs"} {
		if _, err := h.readPluginManifestFromWorkspace(context.Background(), "ws", bad); err == nil {
			t.Errorf("install name %q must be refused", bad)
		}
	}
}

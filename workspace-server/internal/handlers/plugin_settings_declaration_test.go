package handlers

// M5: the plugin tab renders a form for a plugin the frontend has never seen.
//
// The whole point is that nothing here is plugin-specific. These tests use a
// manifest the frontend has no knowledge of and assert the declaration is
// sufficient to build a form from — and that `.example` generation can never
// leak a credential.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
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

// The path-traversal guard, asserted on the ERROR IT RAISES.
//
// This test used to assert only `err != nil`. With no reachable backend EVERY
// name errors, so deleting the guard entirely left it PASSING — it proved
// nothing about traversal. It now pins the specific sentinel, and table-drives
// a SAFE name that must NOT produce it, so the guard is shown to discriminate
// rather than to reject everything.
func TestDeclaration_UnsafeInstallNameIsRefusedBeforeAnyRead(t *testing.T) {
	h := &TemplatesHandler{} // no docker, no host mirror: every read fails
	for _, tc := range []struct {
		name   string
		unsafe bool
	}{
		{"../escape", true},
		{"a/b", true},
		{"..", true},
		{".", true},
		{"", true},
		{"/abs", true},
		{"plugins/../../etc/passwd", true},
		// The control. A legitimate install name must get PAST the guard and
		// fail for a backend reason instead — otherwise "refused" would just be
		// "everything is refused" and the guard could be deleted unnoticed.
		{"molecule-ai-plugin-scheduler", false},
	} {
		_, err := h.readPluginManifestFromWorkspace(context.Background(), "ws", tc.name)
		if err == nil {
			t.Errorf("install name %q: expected an error from the unreachable backend at least", tc.name)
			continue
		}
		got := errors.Is(err, errUnsafeInstallName)
		if got != tc.unsafe {
			t.Errorf("install name %q: errors.Is(err, errUnsafeInstallName) = %v, want %v (err = %v)",
				tc.name, got, tc.unsafe, err)
		}
		if !tc.unsafe && !errors.Is(err, errWorkspaceUnreachable) {
			t.Errorf("install name %q: a safe name must fail on the BACKEND, not the guard: %v", tc.name, err)
		}
	}
}

// B3: the endpoint was dead off local Docker. readPluginManifestFromWorkspace
// had ONE branch — findContainer → docker exec — and findContainer returns ""
// whenever h.docker == nil, i.e. on the docker-less CP tenant shape and on every
// SaaS/EC2 workspace. The three failure modes must stay distinguishable.
func TestDeclaration_UnreadableBoxIsNotReportedAsAMissingPlugin(t *testing.T) {
	h := &TemplatesHandler{} // docker nil, hostStateDir empty: no backend at all
	_, err := h.readPluginManifestFromWorkspace(context.Background(), "ws", "molecule-ai-plugin-scheduler")
	if !errors.Is(err, errWorkspaceUnreachable) {
		t.Fatalf("no reachable backend must report UNREACHABLE (→503), not absent (→404): %v", err)
	}
	if errors.Is(err, errWorkspaceFileAbsent) {
		t.Error("an unreachable box must never be reported as 'the plugin is not installed'")
	}
}

// The host-side /configs mirror carries the rendered bundle only. The plugin
// tree is staged INSIDE the box by the post-online reconcile and has no
// host-side copy, so a miss there is "cannot see the box", never "not
// installed" — the distinction that keeps a 503 from masquerading as a 404.
func TestDeclaration_HostSideMirrorCannotAnswerForThePluginTree(t *testing.T) {
	if hostSideMirrorCanServe("plugins/molecule-ai-plugin-scheduler/plugin.yaml") {
		t.Error("the mirror does not carry plugins/ — claiming it does turns 'box unreadable' into a false 404")
	}
	// ...but it DOES carry what the provisioner actually persisted there.
	for _, rel := range []string{
		"config.yaml",
		"prompts/system.md",
		pluginSettingsDirName + "/molecule-ai-plugin-scheduler.json",
	} {
		if !hostSideMirrorCanServe(rel) {
			t.Errorf("the mirror does carry %q; refusing it would break docker-less delivery read-back", rel)
		}
	}
}

// The mirror leg really does serve a settings read on the docker-less shape —
// the leg B1's config seed depends on.
func TestDeclaration_MirrorLegServesADeliveredSettingsFile(t *testing.T) {
	base := t.TempDir()
	const ws = "mirror-read-ws"
	h := &TemplatesHandler{hostStateDir: base}

	rel := pluginSettingsDirName + "/molecule-ai-plugin-scheduler.json"
	mirror := provisioner.HostSideConfigsDir(base, ws)
	if err := os.MkdirAll(filepath.Join(mirror, pluginSettingsDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirror, rel), []byte(`{"timezone":"UTC"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := h.readWorkspaceConfigFile(context.Background(), ws, rel)
	if err != nil {
		t.Fatalf("the docker-less mirror leg did not serve the read: %v", err)
	}
	if string(got) != `{"timezone":"UTC"}` {
		t.Errorf("read back %q", got)
	}

	// A file the mirror genuinely does not hold is ABSENT (→404), not unreachable.
	_, err = h.readWorkspaceConfigFile(context.Background(), ws,
		pluginSettingsDirName+"/never-delivered.json")
	if !errors.Is(err, errWorkspaceFileAbsent) {
		t.Errorf("a real miss inside a present mirror must be absent, got %v", err)
	}
}

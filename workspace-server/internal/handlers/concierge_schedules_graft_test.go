package handlers

// concierge_schedules_graft_test.go — proves the ONE-TIME, GENERIC enablement
// that makes the platform-agent template's `schedules:` node survive concierge
// config composition (composeConciergeRuntimeConfig → graftConciergeSchedules).
//
// The concierge (kind=platform) is composed from its ACTUAL runtime's base
// template (see concierge_runtime_agnostic_config_test.go). On SELF-HOST
// deployments (MOLECULE_ORG_ID unset) the platform-agent template carries the
// org's schedules; those must be grafted onto the composed config.yaml so the
// concierge boots with them. On SaaS (MOLECULE_ORG_ID set) the concierge config
// stays byte-identical — the graft is gated off.
//
// The template schedules are already runtime-native (cron/inline prompt), so the
// graft is a pure passthrough (renderTemplateSchedulesYAML is NOT involved).
//
// Boot-safety: an unloadable config.yaml bricks workspace boot, so a template
// with no schedules node, or a malformed one, must ship the composed config
// UNCHANGED (never brick). Every assertion below is negative-controlled:
//   (1) self-host + schedules-carrying template → both schedule names present.
//   (2) SaaS (MOLECULE_ORG_ID set) → NO schedules (proves the self-host gate).
//   (3) template WITHOUT a schedules node → composed config byte-identical to the
//       ungrafted compose (no-op).
//   (4) MALFORMED schedules node (unparseable template) → composed config shipped
//       WITHOUT it, byte-identical to the ungrafted compose (boot-safe).

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// A 2-entry, runtime-native schedules block (cron + inline prompt) — exactly
// what a platform-agent template ships. graftConciergeSchedules must pass this
// through verbatim; the schedule content is never hardcoded in Go.
const conciergeGraftSchedulesBlock = "schedules:\n" +
	"    - name: daily-standup\n" +
	"      cron: 0 9 * * *\n" +
	"      timezone: UTC\n" +
	"      prompt: Post the daily standup summary.\n" +
	"      enabled: true\n" +
	"    - name: weekly-report\n" +
	"      cron: 0 17 * * 5\n" +
	"      timezone: UTC\n" +
	"      prompt: Compile the weekly report.\n" +
	"      enabled: true\n"

// writeConciergeScheduleGraftFixtures writes a hermes base config (the compose
// source) plus a platform-agent template config.yaml. The platform-agent
// config.yaml body is caller-supplied so each test can vary the `schedules:`
// node (present / absent / malformed).
func writeConciergeScheduleGraftFixtures(t *testing.T, configsDir, platformAgentConfig string) {
	t.Helper()
	fixtures := map[string]string{
		"hermes/config.yaml": "name: Hermes Agent\n" +
			"runtime: hermes\n" +
			"prompt_files:\n" +
			"- system-prompt.md\n" +
			"runtime_config:\n" +
			"  model: minimax/MiniMax-M2.7\n" +
			"  required_env: [MINIMAX_API_KEY]\n",
		"platform-agent/config.yaml":          platformAgentConfig,
		"platform-agent/prompts/concierge.md": "# You are {{CONCIERGE_NAME}}\n",
	}
	for rel, content := range fixtures {
		p := filepath.Join(configsDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// parseComposedScheduleNames extracts the top-level schedules[].name list from a
// composed config (nil when there is no schedules node). Also asserts the whole
// config re-parses — a composed config.yaml MUST always be loadable.
func parseComposedScheduleNames(t *testing.T, cfg []byte) []string {
	t.Helper()
	var doc struct {
		Schedules []struct {
			Name string `yaml:"name"`
		} `yaml:"schedules"`
	}
	if err := yaml.Unmarshal(cfg, &doc); err != nil {
		t.Fatalf("composed config.yaml is NOT parseable (would brick boot): %v\n%s", err, cfg)
	}
	if doc.Schedules == nil {
		return nil
	}
	names := make([]string, 0, len(doc.Schedules))
	for _, s := range doc.Schedules {
		names = append(names, s.Name)
	}
	return names
}

// TestGraftConciergeSchedules_SelfHost is the POSITIVE case: on self-host
// (MOLECULE_ORG_ID unset) the platform-agent template's runtime-native schedules
// are grafted onto the composed concierge config, and the merged config parses.
func TestGraftConciergeSchedules_SelfHost(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "") // self-host: SelfHostPlatformSeedEnabled()==true
	dir := t.TempDir()
	writeConciergeScheduleGraftFixtures(t, dir, "name: Org Concierge\n"+
		"runtime: hermes\n"+conciergeGraftSchedulesBlock)
	h := &WorkspaceHandler{configsDir: dir}

	composed, err := h.composeConciergeRuntimeConfig("hermes")
	if err != nil {
		t.Fatalf("composeConciergeRuntimeConfig error: %v", err)
	}
	names := parseComposedScheduleNames(t, composed)
	if len(names) != 2 || names[0] != "daily-standup" || names[1] != "weekly-report" {
		t.Fatalf("grafted schedule names = %v, want [daily-standup weekly-report]\n%s", names, composed)
	}
	// The composed config still declares the concierge's runtime (compose intact).
	if got := parseTopLevelRuntime(composed); got != "hermes" {
		t.Errorf("composed runtime = %q, want hermes", got)
	}
}

// TestGraftConciergeSchedules_SaaSGate is the NEGATIVE CONTROL for the self-host
// gate: with MOLECULE_ORG_ID set the graft is skipped even though the template
// carries schedules, so the composed config has NONE. Proves SaaS concierge
// config is untouched.
func TestGraftConciergeSchedules_SaaSGate(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "org-abc123") // SaaS: SelfHostPlatformSeedEnabled()==false
	dir := t.TempDir()
	writeConciergeScheduleGraftFixtures(t, dir, "name: Org Concierge\n"+
		"runtime: hermes\n"+conciergeGraftSchedulesBlock)
	h := &WorkspaceHandler{configsDir: dir}

	composed, err := h.composeConciergeRuntimeConfig("hermes")
	if err != nil {
		t.Fatalf("composeConciergeRuntimeConfig error: %v", err)
	}
	if names := parseComposedScheduleNames(t, composed); names != nil {
		t.Fatalf("SaaS concierge config carries schedules %v — the self-host gate leaked", names)
	}
}

// composeWithoutScheduleTemplate returns the ungrafted composed bytes: a compose
// run with a platform-agent template that has NO schedules node (self-host on).
// This is the "unchanged" reference for the no-op / boot-safe controls.
func composeWithoutScheduleTemplate(t *testing.T) []byte {
	t.Helper()
	t.Setenv("MOLECULE_ORG_ID", "")
	dir := t.TempDir()
	writeConciergeScheduleGraftFixtures(t, dir, "name: Org Concierge\nruntime: hermes\n")
	h := &WorkspaceHandler{configsDir: dir}
	out, err := h.composeConciergeRuntimeConfig("hermes")
	if err != nil {
		t.Fatalf("reference compose error: %v", err)
	}
	return out
}

// TestGraftConciergeSchedules_NoScheduleNodeIsNoop: self-host, but the
// platform-agent template declares NO schedules → the composed config is
// byte-identical to the ungrafted reference (pure no-op).
func TestGraftConciergeSchedules_NoScheduleNodeIsNoop(t *testing.T) {
	reference := composeWithoutScheduleTemplate(t)

	t.Setenv("MOLECULE_ORG_ID", "")
	dir := t.TempDir()
	writeConciergeScheduleGraftFixtures(t, dir, "name: Org Concierge\nruntime: hermes\n")
	h := &WorkspaceHandler{configsDir: dir}

	composed, err := h.composeConciergeRuntimeConfig("hermes")
	if err != nil {
		t.Fatalf("composeConciergeRuntimeConfig error: %v", err)
	}
	if names := parseComposedScheduleNames(t, composed); names != nil {
		t.Fatalf("no-schedules template yielded schedules %v", names)
	}
	if string(composed) != string(reference) {
		t.Fatalf("no-op graft changed the composed config:\n--- got ---\n%s\n--- want ---\n%s", composed, reference)
	}
}

// TestGraftConciergeSchedules_MalformedIsBootSafe: self-host, but the
// platform-agent template's schedules node is MALFORMED (unparseable YAML) → the
// composed config is shipped WITHOUT schedules, byte-identical to the ungrafted
// reference. Proves an unparseable template never bricks boot.
func TestGraftConciergeSchedules_MalformedIsBootSafe(t *testing.T) {
	reference := composeWithoutScheduleTemplate(t)

	t.Setenv("MOLECULE_ORG_ID", "")
	dir := t.TempDir()
	// Unbalanced flow sequence in the schedules node → the whole config.yaml
	// fails to parse, so graftConciergeSchedules must bail before mutating.
	malformed := "name: Org Concierge\nruntime: hermes\n" +
		"schedules: [ {name: broken, cron: '0 9 * * *'\n"
	writeConciergeScheduleGraftFixtures(t, dir, malformed)
	h := &WorkspaceHandler{configsDir: dir}

	composed, err := h.composeConciergeRuntimeConfig("hermes")
	if err != nil {
		t.Fatalf("composeConciergeRuntimeConfig error: %v", err)
	}
	if names := parseComposedScheduleNames(t, composed); names != nil {
		t.Fatalf("malformed schedules were grafted %v — boot-safety failed", names)
	}
	if string(composed) != string(reference) {
		t.Fatalf("malformed graft changed the composed config:\n--- got ---\n%s\n--- want ---\n%s", composed, reference)
	}
}

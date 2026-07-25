package handlers

// concierge_default_schedules_payload_test.go — the SELF-HOST DEFAULT-SCHEDULES
// contract gate. concierge_schedules_graft_test.go proves the graft MECHANISM
// with synthetic schedule content; this file proves the ACTUAL FEATURE PAYLOAD —
// the two default schedules the self-host concierge ships
// (project_selfhost_concierge_default_schedules) — survives composition into the
// self-host concierge's delivered config.yaml intact, on the DEFAULT concierge
// runtime (hermes), and is gated OFF on SaaS.
//
// WHY A SEPARATE, PAYLOAD-PINNED GATE: the graft is a generic passthrough, so a
// refactor that added key-filtering, name-normalization, or prompt truncation
// could still pass the generic 2-item graft test while silently corrupting the
// REAL schedules — whose value is entirely in (a) the exact names/crons the
// runtime scheduler seeds from and (b) the load-bearing tool references embedded
// in each prompt (send_message_to_user for delivery; check_plugin_updates +
// apply_plugin_update for the auto-update audit). This gate couples core's graft
// to that concrete feature contract so a regression in either surfaces here.
//
// SCOPE — what this gate DOES and does NOT prove (see the task coverage note):
//   DOES: self-host (MOLECULE_ORG_ID unset) → the concierge's composed
//         /configs/config.yaml carries BOTH default schedules with their exact
//         names, crons, and the tool references their prompts depend on, and the
//         merged config still parses (an unloadable config bricks boot).
//   DOES: SaaS (MOLECULE_ORG_ID set) → NEITHER default schedule leaks into the
//         concierge config (self-host gate, negative control).
//   DOES NOT: prove the RUNTIME seeds a schedule grid from this config, that the
//         cron FIRES, or that send_message_to_user actually DELIVERS. That is
//         runtime-repo behavior; the generic scheduler fire+deliver path is
//         covered by the E2E_SCHEDULER_CHECK sub-step (test_staging_full_saas.sh),
//         and a self-host-concierge-specific fire+deliver e2e is filed as a
//         follow-up.
//
// The schedules block below is a VERBATIM copy of the platform-agent template's
// config.yaml `schedules:` node (molecule-ai-workspace-template-platform-agent).
// If the template's default schedules change, update this fixture to match — the
// point of this gate is to pin the two, so a drift is a deliberate edit here.

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// conciergeRealDefaultSchedulesBlock is the platform-agent template's actual
// `schedules:` node — the two self-host concierge default schedules, verbatim.
const conciergeRealDefaultSchedulesBlock = `schedules:
  - name: daily-activity-report
    cron: "0 9 * * *"
    timezone: UTC
    enabled: true
    prompt: "Every morning, report what happened across this deployment in the last 24 hours. Retrieve recent activity (e.g. GET /workspaces/<your id>/activity?since_secs=86400, and /mail/summary if available), write a concise 'Yesterday's report' covering agents/tasks active, work completed, notable events, and any errors, then deliver it to the user with the send_message_to_user tool. If nothing of note happened, send a brief 'quiet day' note."

  - name: plugin-auto-update
    cron: "0 3 * * *"
    timezone: UTC
    enabled: true
    prompt: "Keep this self-hosted deployment up to date. Use check_plugin_updates to list plugins with a newer version available, and for each, use apply_plugin_update to apply it (re-pins and restarts the affected workspace). Also check whether a newer core or runtime version is available — you CANNOT apply those (operator deploy needed), so only report them. Then send the user (send_message_to_user) an audit: which plugins you auto-updated (name old->new) and any core/runtime updates available to deploy. If the update tools are not available yet, just report that update tooling is not yet installed and do nothing else."
`

// scheduleByName returns the parsed schedule with the given name from a composed
// config, and whether it was found. It re-parses the FULL composed config (so a
// corrupt config surfaces as an fatal parse error, not a silent miss) and reads
// name + cron + prompt so the assertions can pin the load-bearing fields.
func scheduleByName(t *testing.T, cfg []byte, name string) (cron, prompt string, found bool) {
	t.Helper()
	var doc struct {
		Schedules []struct {
			Name   string `yaml:"name"`
			Cron   string `yaml:"cron"`
			Prompt string `yaml:"prompt"`
		} `yaml:"schedules"`
	}
	// Re-parse the FULL composed config — a corrupt config.yaml (which would
	// brick boot) must surface as a fatal here, never a silent miss.
	if err := yaml.Unmarshal(cfg, &doc); err != nil {
		t.Fatalf("composed config.yaml is NOT parseable (would brick boot): %v\n%s", err, cfg)
	}
	for _, s := range doc.Schedules {
		if s.Name == name {
			return s.Cron, s.Prompt, true
		}
	}
	return "", "", false
}

// TestConciergeDefaultSchedules_SelfHostDeliversBothWithToolRefs is the POSITIVE
// contract gate: on self-host, composing the DEFAULT-runtime (hermes) concierge
// config from a platform-agent template carrying the two real default schedules
// yields a config.yaml that carries BOTH, with their exact crons and the tool
// references their prompts depend on.
func TestConciergeDefaultSchedules_SelfHostDeliversBothWithToolRefs(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "") // self-host: SelfHostPlatformSeedEnabled()==true
	dir := t.TempDir()
	writeConciergeScheduleGraftFixtures(t, dir, "name: Org Concierge\n"+
		"runtime: hermes\n"+conciergeRealDefaultSchedulesBlock)
	h := &WorkspaceHandler{configsDir: dir}

	composed, err := h.composeConciergeRuntimeConfig("hermes")
	if err != nil {
		t.Fatalf("composeConciergeRuntimeConfig error: %v", err)
	}

	// Both default schedules present, in order, with exact names — the runtime
	// scheduler seeds the grid keyed on these.
	names := parseComposedScheduleNames(t, composed)
	if len(names) != 2 || names[0] != "daily-activity-report" || names[1] != "plugin-auto-update" {
		t.Fatalf("delivered schedule names = %v, want [daily-activity-report plugin-auto-update]\n%s", names, composed)
	}

	// daily-activity-report: 0 9 * * *, and its prompt must invoke the delivery
	// tool (send_message_to_user) — without it the report never reaches the user.
	cron, prompt, ok := scheduleByName(t, composed, "daily-activity-report")
	if !ok {
		t.Fatalf("daily-activity-report missing from composed config\n%s", composed)
	}
	if cron != "0 9 * * *" {
		t.Errorf("daily-activity-report cron = %q, want %q", cron, "0 9 * * *")
	}
	if !strings.Contains(prompt, "send_message_to_user") {
		t.Errorf("daily-activity-report prompt lost the send_message_to_user delivery tool ref:\n%s", prompt)
	}

	// plugin-auto-update: 0 3 * * *, and its prompt must invoke BOTH plugin-update
	// verbs (check + apply) — the auto-apply is the whole point of this schedule.
	cron, prompt, ok = scheduleByName(t, composed, "plugin-auto-update")
	if !ok {
		t.Fatalf("plugin-auto-update missing from composed config\n%s", composed)
	}
	if cron != "0 3 * * *" {
		t.Errorf("plugin-auto-update cron = %q, want %q", cron, "0 3 * * *")
	}
	for _, tool := range []string{"check_plugin_updates", "apply_plugin_update", "send_message_to_user"} {
		if !strings.Contains(prompt, tool) {
			t.Errorf("plugin-auto-update prompt lost the %q tool ref:\n%s", tool, prompt)
		}
	}

	// The composed config still declares the concierge's runtime (compose intact).
	if got := parseTopLevelRuntime(composed); got != "hermes" {
		t.Errorf("composed runtime = %q, want hermes", got)
	}
}

// TestConciergeDefaultSchedules_SaaSSeedsNeither is the NEGATIVE CONTROL: with
// MOLECULE_ORG_ID set, the SAME template carrying the two real default schedules
// yields a concierge config with NEITHER — the self-host gate holds for the real
// payload, not just the synthetic one.
func TestConciergeDefaultSchedules_SaaSSeedsNeither(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "org-saas-xyz") // SaaS: SelfHostPlatformSeedEnabled()==false
	dir := t.TempDir()
	writeConciergeScheduleGraftFixtures(t, dir, "name: Org Concierge\n"+
		"runtime: hermes\n"+conciergeRealDefaultSchedulesBlock)
	h := &WorkspaceHandler{configsDir: dir}

	composed, err := h.composeConciergeRuntimeConfig("hermes")
	if err != nil {
		t.Fatalf("composeConciergeRuntimeConfig error: %v", err)
	}
	if names := parseComposedScheduleNames(t, composed); names != nil {
		t.Fatalf("SaaS concierge config leaked default schedules %v — the self-host gate failed for the real payload", names)
	}
}

package handlers

// org_import_schedule_render_test.go — unit tests for
// renderTemplateSchedulesYAML, the core leg of the scheduler-as-trigger-plugin
// RFC §8A P3 template-seeding seam (issue #4411 decision, 2026-07-17).
//
// The rendered block is CONSUMED by the runtime's
// seed_schedules_from_workspace_config (molecule-ai-workspace-runtime#318),
// which reads the delivered config.yaml's top-level `schedules:` list with
// keys name / cron / timezone / prompt / enabled. These tests parse the
// rendered block back with the same YAML semantics (yaml.Unmarshal ≈
// yaml.safe_load) and assert the consumer-visible shape — not the exact byte
// layout — so a yaml.v3 style change can't red the suite while a KEY change
// (which would break the runtime) does.
//
// Negative controls per feedback_negative_control_every_test:
//   - the no-schedules case asserts the config bytes stay BYTE-IDENTICAL and
//     the `schedules:` key is ABSENT (not merely "no error");
//   - the cap-violation case asserts the offending entries are ABSENT while
//     the valid sibling IS present (skip must not become drop-all or keep-all);
//   - the prompt_file case asserts the rendered entry carries the file BODY
//     and that no `prompt_file` key ships (a shipped ref would dangle
//     in-container).

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// parsedScheduleBlock re-parses a rendered block the way the runtime consumer
// does: full-document yaml unmarshal, top-level `schedules:` list.
type parsedScheduleBlock struct {
	Schedules []map[string]interface{} `yaml:"schedules"`
}

func parseRenderedSchedules(t *testing.T, doc []byte) []map[string]interface{} {
	t.Helper()
	var parsed parsedScheduleBlock
	if err := yaml.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("rendered config.yaml does not parse: %v\n---\n%s", err, doc)
	}
	return parsed.Schedules
}

func boolPtr(b bool) *bool { return &b }

// TestRenderTemplateSchedulesYAML_GoldenInlinesPromptFile — (a) golden: a
// template with 2 schedules, one inline prompt and one prompt_file, renders a
// `schedules:` block whose prompt_file entry carries the FILE CONTENT inlined.
func TestRenderTemplateSchedulesYAML_GoldenInlinesPromptFile(t *testing.T) {
	orgDir := t.TempDir()
	filesDir := "reports"
	if err := os.MkdirAll(filepath.Join(orgDir, filesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	promptBody := "Daily digest:\n- check the queue: status\n- summarize\n"
	if err := os.WriteFile(filepath.Join(orgDir, filesDir, "daily.md"), []byte(promptBody), 0o644); err != nil {
		t.Fatal(err)
	}

	schedules := []OrgSchedule{
		{Name: "morning-inline", CronExpr: "0 9 * * *", Timezone: "America/Vancouver", Prompt: "Say good morning"},
		{Name: "daily-from-file", CronExpr: "30 18 * * 1-5", PromptFile: "daily.md", Enabled: boolPtr(false)},
	}

	block, rendered, skipped := renderTemplateSchedulesYAML(schedules, orgDir, filesDir, "WS Golden")
	if rendered != 2 || skipped != 0 {
		t.Fatalf("rendered=%d skipped=%d, want 2/0 (block:\n%s)", rendered, skipped, block)
	}

	// The delivered file is base-config + appended block; parse the WHOLE
	// document the way the runtime does to prove the block lands top-level.
	base := []byte("runtime: claude-code\nmodel: \"anthropic:claude-opus-4-7\"")
	delivered := appendYAMLBlock(base, block)
	entries := parseRenderedSchedules(t, delivered)
	if len(entries) != 2 {
		t.Fatalf("delivered config.yaml has %d schedules, want 2:\n%s", len(entries), delivered)
	}

	first, second := entries[0], entries[1]
	if first["name"] != "morning-inline" || first["cron"] != "0 9 * * *" ||
		first["timezone"] != "America/Vancouver" || first["prompt"] != "Say good morning" ||
		first["enabled"] != true {
		t.Errorf("inline entry wrong: %#v", first)
	}
	if second["name"] != "daily-from-file" || second["cron"] != "30 18 * * 1-5" {
		t.Errorf("file entry identity wrong: %#v", second)
	}
	// The load-bearing golden bit: the prompt_file BODY is inlined…
	if second["prompt"] != promptBody {
		t.Errorf("prompt_file content not inlined: got %q want %q", second["prompt"], promptBody)
	}
	// …the entry's explicit enabled:false survives…
	if second["enabled"] != false {
		t.Errorf("enabled:false not preserved: %#v", second)
	}
	// …timezone defaults to UTC (legacy DB-seed parity)…
	if second["timezone"] != "UTC" {
		t.Errorf("timezone default: got %v want UTC", second["timezone"])
	}
	// …and no prompt_file ref ships (it would dangle inside the container).
	if strings.Contains(block, "prompt_file") {
		t.Errorf("rendered block must never carry prompt_file refs:\n%s", block)
	}
}

// TestRenderTemplateSchedulesYAML_NoSchedules_ByteIdentical — (b) negative
// control: a template with no schedules must leave the delivered config.yaml
// BYTE-IDENTICAL (empty block, nothing appended, `schedules:` key absent).
func TestRenderTemplateSchedulesYAML_NoSchedules_ByteIdentical(t *testing.T) {
	for _, schedules := range [][]OrgSchedule{nil, {}} {
		block, rendered, skipped := renderTemplateSchedulesYAML(schedules, t.TempDir(), "", "WS Empty")
		if block != "" || rendered != 0 || skipped != 0 {
			t.Fatalf("no-schedules render must be empty: block=%q rendered=%d skipped=%d", block, rendered, skipped)
		}
		// Mirror the org_import assembly contract: an empty block is never
		// appended, so the config bytes are byte-identical to today.
		base := []byte("runtime: claude-code\nmodel: m\n")
		delivered := base
		if block != "" { // the org_import.go guard under test's contract
			delivered = appendYAMLBlock(delivered, block)
		}
		if !bytes.Equal(delivered, base) {
			t.Errorf("delivered config.yaml changed for a no-schedules template:\n%s", delivered)
		}
		if strings.Contains(string(delivered), "schedules:") {
			t.Errorf("schedules block must be ABSENT for a no-schedules template:\n%s", delivered)
		}
	}
}

// TestRenderTemplateSchedulesYAML_SkipsViolationsKeepsSiblings — (c) per-entry
// validate-and-skip: oversize prompt, invalid cron, over-long cron, empty
// name, and an unresolvable prompt_file are each SKIPPED while the valid
// sibling still renders. Also proves the empty-render edge: when EVERY entry
// is invalid the block is empty (nothing to deliver).
func TestRenderTemplateSchedulesYAML_SkipsViolationsKeepsSiblings(t *testing.T) {
	orgDir := t.TempDir()
	oversize := strings.Repeat("x", maxSchedulePromptBytes+1)
	longCron := strings.Repeat("*", maxScheduleCronExprLen+1)

	schedules := []OrgSchedule{
		{Name: "too-big", CronExpr: "0 9 * * *", Prompt: oversize},
		{Name: "bad-cron", CronExpr: "not a cron", Prompt: "p"},
		{Name: "long-cron", CronExpr: longCron, Prompt: "p"},
		{Name: "", CronExpr: "0 9 * * *", Prompt: "p"},
		{Name: "no-such-file", CronExpr: "0 9 * * *", PromptFile: "missing.md"},
		{Name: "escape", CronExpr: "0 9 * * *", PromptFile: "../../etc/passwd"},
		{Name: "valid-sibling", CronExpr: "*/5 * * * *", Prompt: "still here"},
	}

	block, rendered, skipped := renderTemplateSchedulesYAML(schedules, orgDir, "", "WS Hostile")
	if rendered != 1 || skipped != 6 {
		t.Fatalf("rendered=%d skipped=%d, want 1/6\n%s", rendered, skipped, block)
	}
	entries := parseRenderedSchedules(t, []byte(block))
	if len(entries) != 1 || entries[0]["name"] != "valid-sibling" || entries[0]["prompt"] != "still here" {
		t.Fatalf("valid sibling must survive alone, got: %#v", entries)
	}
	for _, absent := range []string{"too-big", "bad-cron", "long-cron", "no-such-file", "escape"} {
		if strings.Contains(block, absent) {
			t.Errorf("skipped entry %q leaked into the rendered block", absent)
		}
	}

	// All-invalid grid → empty block (delivery stays byte-identical).
	block, rendered, skipped = renderTemplateSchedulesYAML(schedules[:2], orgDir, "", "WS AllBad")
	if block != "" || rendered != 0 || skipped != 2 {
		t.Errorf("all-invalid render: block=%q rendered=%d skipped=%d, want empty/0/2", block, rendered, skipped)
	}
}

// TestRenderTemplateSchedulesYAML_EntryCapSkipsOverflow — entries beyond
// maxTemplateSchedules (== the runtime store's MAX_ENTRIES) are skipped
// wholesale so the delivered block can never trip the runtime's atomic
// whole-grid MAX_ENTRIES rejection (which would drop VALID entries too).
func TestRenderTemplateSchedulesYAML_EntryCapSkipsOverflow(t *testing.T) {
	schedules := make([]OrgSchedule, maxTemplateSchedules+2)
	for i := range schedules {
		schedules[i] = OrgSchedule{
			Name:     fmt.Sprintf("s-%03d", i),
			CronExpr: "0 9 * * *",
			Prompt:   "p",
		}
	}
	block, rendered, skipped := renderTemplateSchedulesYAML(schedules, t.TempDir(), "", "WS Cap")
	if rendered != maxTemplateSchedules || skipped != 2 {
		t.Fatalf("rendered=%d skipped=%d, want %d/2", rendered, skipped, maxTemplateSchedules)
	}
	if got := len(parseRenderedSchedules(t, []byte(block))); got != maxTemplateSchedules {
		t.Errorf("block carries %d entries, want %d", got, maxTemplateSchedules)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// PR #4444 review findings — CRITICAL (yaml.v3 block-scalar emitter) +
// REQUIRED (name contract) + caps/contract parity.
// ─────────────────────────────────────────────────────────────────────────────

// TestRenderTemplateSchedulesYAML_IndentedFirstLinePromptsRenderPortably —
// review CRITICAL regression test. The yaml.v3 emitter encodes a multi-line
// prompt whose FIRST line starts with whitespace as a broken `|4-` block
// scalar that neither yaml.v3 nor PyYAML can re-parse; delivered as-is it
// bricks workspace boot (runtime config.py load_config has no try/except).
// With schedulePromptNode normalization + the scheduleEntryRoundTrips guard,
// both the reviewer's minimal trigger (" x\ny") and a realistic
// indented-first-line prompt_file must render SAFELY (content preserved
// byte-exact) and the assembled document must parse.
//
// Negative-control evidence (recorded in the PR): with the normalization
// removed, the guard skips both entries and this test fails on rendered=3;
// with normalization AND guard removed, parseRenderedSchedules fails on the
// unparseable assembled doc — both fail arms are reachable.
func TestRenderTemplateSchedulesYAML_IndentedFirstLinePromptsRenderPortably(t *testing.T) {
	orgDir := t.TempDir()
	filesPromptBody := "  Please review:\n- item one\n    indented code\ndone"
	if err := os.WriteFile(filepath.Join(orgDir, "review.md"), []byte(filesPromptBody), 0o644); err != nil {
		t.Fatal(err)
	}

	minimalTrigger := " x\ny" // the reviewer's minimal PyYAML-breaking repro
	schedules := []OrgSchedule{
		{Name: "minimal-trigger", CronExpr: "0 9 * * *", Prompt: minimalTrigger},
		{Name: "indented-review", CronExpr: "0 18 * * *", PromptFile: "review.md"},
		{Name: "plain-sibling", CronExpr: "*/5 * * * *", Prompt: "plain\nmultiline"},
	}

	block, rendered, skipped := renderTemplateSchedulesYAML(schedules, orgDir, "", "WS Indent")
	if rendered != 3 || skipped != 0 {
		t.Fatalf("rendered=%d skipped=%d, want 3/0 — indented-first-line prompts must render safely, not be dropped\n%s", rendered, skipped, block)
	}

	// The ASSEMBLED document (base + block, as delivered) must parse — the
	// pre-fix emission fails right here with `did not find expected key`.
	delivered := appendYAMLBlock([]byte("runtime: claude-code\nmodel: m\n"), block)
	entries := parseRenderedSchedules(t, delivered)
	if len(entries) != 3 {
		t.Fatalf("assembled config.yaml carries %d schedules, want 3:\n%s", len(entries), delivered)
	}
	// Content preserved byte-exact through the normalized emission.
	if entries[0]["prompt"] != minimalTrigger {
		t.Errorf("minimal trigger prompt not preserved: %q want %q", entries[0]["prompt"], minimalTrigger)
	}
	if entries[1]["prompt"] != filesPromptBody {
		t.Errorf("indented prompt_file body not preserved: %q want %q", entries[1]["prompt"], filesPromptBody)
	}
	if entries[2]["prompt"] != "plain\nmultiline" {
		t.Errorf("plain sibling prompt not preserved: %q", entries[2]["prompt"])
	}
	// And the known-broken emission shape must be absent from the block.
	if strings.Contains(block, "|4") {
		t.Errorf("block still contains an explicit block-scalar indentation indicator (the PyYAML-breaking emission):\n%s", block)
	}
}

// TestRenderTemplateSchedulesYAML_RejectsNonContractNames — review REQUIRED:
// the runtime schema enforces ^[a-z0-9]+(?:-[a-z0-9]+)*$ (≤128 chars) on
// names; a name that fails it renders core-side but is silently skipped by
// the runtime's seeding → split-brain vs the DB grid now, silent loss at
// P4b. Core must skip exactly what the runtime would skip.
func TestRenderTemplateSchedulesYAML_RejectsNonContractNames(t *testing.T) {
	schedules := []OrgSchedule{
		{Name: "Morning Digest", CronExpr: "0 9 * * *", Prompt: "p"},                          // space + uppercase
		{Name: "digest_1", CronExpr: "0 9 * * *", Prompt: "p"},                                // underscore
		{Name: "check-CI", CronExpr: "0 9 * * *", Prompt: "p"},                                // uppercase
		{Name: "-leading", CronExpr: "0 9 * * *", Prompt: "p"},                                // leading dash
		{Name: strings.Repeat("a", maxScheduleNameLen+1), CronExpr: "0 9 * * *", Prompt: "p"}, // over maxLength
		{Name: "kebab-sibling-2", CronExpr: "0 9 * * *", Prompt: "p"},                         // valid
	}
	block, rendered, skipped := renderTemplateSchedulesYAML(schedules, t.TempDir(), "", "WS Names")
	if rendered != 1 || skipped != 5 {
		t.Fatalf("rendered=%d skipped=%d, want 1/5\n%s", rendered, skipped, block)
	}
	entries := parseRenderedSchedules(t, []byte(block))
	if len(entries) != 1 || entries[0]["name"] != "kebab-sibling-2" {
		t.Fatalf("only the kebab sibling may render, got: %#v", entries)
	}
	for _, absent := range []string{"Morning Digest", "digest_1", "check-CI", "-leading"} {
		if strings.Contains(block, absent) {
			t.Errorf("non-contract name %q leaked into the rendered block", absent)
		}
	}
}

// TestScheduleRenderContract_MatchesRuntimeSchema — caps + name-grammar
// parity gate (review CONSIDER 1). The expected values are hardcoded from
// the runtime contract SSOT: molecule-ai-workspace-runtime
// molecule_runtime/contracts/schedule.schema.json ($defs.scheduleEntry.name
// pattern/maxLength; caps: max_entries=100, max_cron_len=128,
// max_prompt_bytes=16384 — mirrored by schedule_store.py MAX_ENTRIES /
// MAX_CRON_LEN / MAX_PROMPT_BYTES). If either side moves, this fails and the
// two legs must be re-synced deliberately (a fetch-based cross-repo gate is
// the #4443-style follow-up).
func TestScheduleRenderContract_MatchesRuntimeSchema(t *testing.T) {
	if maxTemplateSchedules != 100 {
		t.Errorf("maxTemplateSchedules=%d, runtime contract max_entries=100", maxTemplateSchedules)
	}
	if maxScheduleCronExprLen != 128 {
		t.Errorf("maxScheduleCronExprLen=%d, runtime contract max_cron_len=128", maxScheduleCronExprLen)
	}
	if maxSchedulePromptBytes != 16384 {
		t.Errorf("maxSchedulePromptBytes=%d, runtime contract max_prompt_bytes=16384", maxSchedulePromptBytes)
	}
	if maxScheduleNameLen != 128 {
		t.Errorf("maxScheduleNameLen=%d, runtime schema name.maxLength=128", maxScheduleNameLen)
	}
	const runtimeNamePattern = `^[a-z0-9]+(?:-[a-z0-9]+)*$`
	if got := scheduleNamePattern.String(); got != runtimeNamePattern {
		t.Errorf("scheduleNamePattern=%q, runtime schema name.pattern=%q — must be byte-identical", got, runtimeNamePattern)
	}
}

// TestAppendYAMLBlockChecked_RevertsOnUnparseableAssembly — the belt+braces
// delivery guard: whatever upstream guards miss, an assembled config.yaml
// that fails to parse must never ship — the block is dropped and the prior
// bytes returned. The broken fixture is the VERBATIM pre-fix yaml.v3
// emission for the reviewer's minimal trigger (explicit `|4-` indicator with
// mis-indented content).
func TestAppendYAMLBlockChecked_RevertsOnUnparseableAssembly(t *testing.T) {
	base := []byte("runtime: claude-code\nmodel: m\n")

	// Positive arm: a valid block appends and the assembly parses.
	good, ok := appendYAMLBlockChecked(base, "schedules:\n    - name: ok\n      cron: 0 9 * * *\n      timezone: UTC\n      prompt: p\n      enabled: true\n", "schedules", "WS Guard")
	if !ok || !strings.Contains(string(good), "name: ok") {
		t.Fatalf("valid block must append (ok=%v):\n%s", ok, good)
	}

	// The verbatim broken emission (captured from yaml.v3 pre-fix). First pin
	// that the fixture is still load-bearing: it must fail to parse on its
	// own — if a future yaml.v3 learns to read it, this test needs a new
	// fixture rather than a vacuous pass.
	broken := "schedules:\n    - name: t\n      cron: 0 9 * * *\n      timezone: UTC\n      prompt: |4-\n         x\n        y\n      enabled: true\n"
	var probe map[string]interface{}
	if err := yaml.Unmarshal([]byte(broken), &probe); err == nil {
		t.Fatal("fixture no longer unparseable under yaml.v3 — replace it with a currently-broken emission")
	}

	got, ok := appendYAMLBlockChecked(base, broken, "schedules", "WS Guard")
	if ok {
		t.Error("unparseable assembly must report ok=false")
	}
	if !bytes.Equal(got, base) {
		t.Errorf("unparseable assembly must revert to the prior bytes:\n%s", got)
	}
	if strings.Contains(string(got), "schedules:") {
		t.Errorf("broken schedules block leaked into the delivered config.yaml:\n%s", got)
	}
}

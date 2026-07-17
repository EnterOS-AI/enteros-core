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

// TestStripTopLevelYAMLKey_RemovesScheduleBlockPreservesRest pins the
// direct-create replace-in-place helper (P4b): the template config.yaml's RAW
// authoring `schedules:` block (cron_expr alias / prompt_file refs) must be
// stripped before the runtime-native rendered block is appended in its place —
// otherwise the delivered config.yaml carries a DUPLICATE `schedules:` key and
// is unloadable. Every OTHER line — keys, nested maps, comments — must survive
// byte-for-byte.
func TestStripTopLevelYAMLKey_RemovesScheduleBlockPreservesRest(t *testing.T) {
	src := []byte("name: Agent\n" +
		"runtime: claude-code\n" +
		"# author's note\n" +
		"schedules:\n" +
		"  - name: raw-authoring\n" +
		"    cron_expr: \"0 9 * * *\"\n" +
		"    prompt_file: daily.md\n" +
		"runtime_config:\n" +
		"  model: sonnet\n")

	stripped := stripTopLevelYAMLKey(src, "schedules")
	s := string(stripped)
	// The raw block and its authoring-only keys are gone…
	if strings.Contains(s, "schedules:") || strings.Contains(s, "cron_expr") || strings.Contains(s, "prompt_file") || strings.Contains(s, "raw-authoring") {
		t.Errorf("raw schedules block not fully stripped:\n%s", s)
	}
	// …every unrelated line survives, including the comment and the sibling
	// nested map that FOLLOWS the block (the scanner must resume after the block).
	for _, want := range []string{"name: Agent", "runtime: claude-code", "# author's note", "runtime_config:", "  model: sonnet"} {
		if !strings.Contains(s, want) {
			t.Errorf("stripped output dropped unrelated line %q:\n%s", want, s)
		}
	}
	// Negative control 1: a document with no such key is returned unchanged
	// (modulo the trailing-newline normalization of split/join, which is
	// byte-stable for a \n-terminated input).
	noKey := []byte("name: Agent\nruntime: claude-code\n")
	if got := stripTopLevelYAMLKey(noKey, "schedules"); !bytes.Equal(got, noKey) {
		t.Errorf("no-op strip changed a document without the key:\ngot=%q want=%q", got, noKey)
	}
	// Negative control 2: the strip result plus a rendered block must parse as a
	// SINGLE schedules key (the duplicate-key boot-brick the helper prevents).
	block, _, _ := renderTemplateSchedulesYAML(
		[]OrgSchedule{{Name: "runtime-native", CronExpr: "0 9 * * *", Prompt: "go"}},
		t.TempDir(), "", "WS Strip")
	combined, ok := appendYAMLBlockChecked(stripped, block, "schedules", "WS Strip")
	if !ok {
		t.Fatalf("strip+append must yield a loadable config.yaml:\n%s", combined)
	}
	if got := len(parseRenderedSchedules(t, combined)); got != 1 {
		t.Errorf("combined config.yaml carries %d schedules, want exactly 1 (rendered replaces raw):\n%s", got, combined)
	}
}

// TestStripTopLevelYAMLKey_StripsFlushStyleSequence pins the review finding:
// the strip must remove a FLUSH-STYLE (zero-indent) `schedules:` sequence body
// too — the exact style a template author can legally write and the sibling
// parseTemplateSchedules accepts. Pre-fix the scanner removed only the
// `schedules:` KEY line and left the orphaned `- name:` / `cron_expr:` body,
// so the combined doc was invalid → appendYAMLBlockChecked's round-trip guard
// returned ok=false → the UNCHANGED template copy shipped, the rendered
// runtime-native block never reached the volume grid, AND the raw block reached
// the runtime (schedules VANISH at the later DB-seed-retirement cutover).
//
// This drives the SAME strip→render→append sequence workspace.go runs on the
// direct-create path (workspace.go: appendYAMLBlockChecked(
// stripTopLevelYAMLKey(baseCfg, "schedules"), schedBlock, …)).
//
// Negative control (recorded in the PR): on the pre-fix stripTopLevelYAMLKey
// every assertion below fails — `appended` comes back FALSE (orphaned flush
// body makes the assembly unparseable), the delivered doc is byte-equal to the
// raw template (so the rendered `cron:` block is ABSENT and the raw `cron_expr`
// / `prompt_file` keys are PRESENT), and parseRenderedSchedules reads the raw
// entries, not the single rendered one. Both fail arms are reachable.
func TestStripTopLevelYAMLKey_StripsFlushStyleSequence(t *testing.T) {
	// Template config.yaml with a FLUSH-STYLE (zero-indent) schedules sequence:
	// the "- name:" items sit at column 0, valid YAML the author may write.
	src := []byte("name: Agent\n" +
		"runtime: claude-code\n" +
		"# author's note\n" +
		"schedules:\n" +
		"- name: raw-authoring\n" +
		"  cron_expr: \"0 9 * * *\"\n" +
		"  prompt_file: daily.md\n" +
		"- name: second-raw\n" +
		"  cron_expr: \"30 18 * * 1-5\"\n" +
		"  prompt: inline body\n" +
		"runtime_config:\n" +
		"  model: sonnet\n")

	stripped := stripTopLevelYAMLKey(src, "schedules")

	// (c) All OTHER config.yaml keys survive BYTE-IDENTICAL — the entire
	// flush-style value (both entries, all continuation lines) is gone, and the
	// sibling `runtime_config:` map that FOLLOWS the block resumes untouched.
	wantStripped := "name: Agent\n" +
		"runtime: claude-code\n" +
		"# author's note\n" +
		"runtime_config:\n" +
		"  model: sonnet\n"
	if string(stripped) != wantStripped {
		t.Fatalf("flush-style strip not byte-identical on non-schedules keys:\ngot:\n%q\nwant:\n%q", stripped, wantStripped)
	}
	for _, gone := range []string{"schedules:", "cron_expr", "prompt_file", "raw-authoring", "second-raw"} {
		if strings.Contains(string(stripped), gone) {
			t.Errorf("flush-style strip left raw authoring token %q behind:\n%s", gone, stripped)
		}
	}

	// Render the runtime-native replacement block and append it exactly as the
	// direct-create path does.
	block, rendered, _ := renderTemplateSchedulesYAML(
		[]OrgSchedule{{Name: "runtime-native", CronExpr: "0 9 * * *", Prompt: "go"}},
		t.TempDir(), "", "WS Flush")
	if rendered != 1 {
		t.Fatalf("precondition: rendered=%d want 1\n%s", rendered, block)
	}
	combined, appended := appendYAMLBlockChecked(stripped, block, "schedules", "WS Flush")
	if !appended {
		// The pre-fix fail arm: the orphaned flush body makes the assembly
		// unparseable, so the guard reverts and the raw template ships.
		t.Fatalf("strip+append of a flush-style template must yield a LOADABLE config.yaml (ok=false means the orphaned body survived):\n%s", combined)
	}

	// (a) the delivered doc carries the rendered runtime-native block, and (b)
	// it parses cleanly as a SINGLE schedules list (no orphaned body).
	entries := parseRenderedSchedules(t, combined)
	if len(entries) != 1 || entries[0]["name"] != "runtime-native" || entries[0]["cron"] != "0 9 * * *" {
		t.Fatalf("delivered config.yaml must carry exactly the 1 rendered runtime-native schedule, got: %#v\n%s", entries, combined)
	}
	// The runtime-native `cron:` key is present and the raw `cron_expr` alias is
	// gone — proof the rendered block replaced the raw one rather than the
	// unchanged template copy shipping.
	if !strings.Contains(string(combined), "cron: 0 9 * * *") {
		t.Errorf("delivered config.yaml missing the rendered runtime-native cron key:\n%s", combined)
	}
	if strings.Contains(string(combined), "cron_expr") || strings.Contains(string(combined), "prompt_file") {
		t.Errorf("raw authoring keys leaked into the delivered config.yaml:\n%s", combined)
	}
	// The non-schedules head survives byte-for-byte into the delivered doc.
	if !strings.HasPrefix(string(combined), wantStripped) {
		t.Errorf("delivered config.yaml did not preserve the non-schedules keys byte-for-byte:\n%s", combined)
	}
}

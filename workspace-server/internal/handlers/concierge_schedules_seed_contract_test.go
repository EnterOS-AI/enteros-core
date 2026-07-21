package handlers

// concierge_schedules_seed_contract_test.go — LINKS the core graft seam to the
// runtime schedule-seed seam so a divergence in the grafted byte shape can no
// longer pass BOTH suites (the audit gap this file closes).
//
// THE GAP (audit): core#4556 (concierge_schedules_graft_test.go) asserts what
// graftConciergeSchedules COMPOSES — schedule names survive, config re-parses —
// but never asserts the grafted entries satisfy the shape the RUNTIME seeder
// will accept. runtime#341 (molecule_runtime tests/test_schedule_seed_workspace_config.py)
// asserts the runtime seeds correctly from a HAND-WRITTEN fixture, but that
// fixture is authored independently of core's graft output. Nothing links the
// two, so if the grafted key shape drifts (a key renamed, an unrecognized key
// added, an out-of-contract value) core CI stays green (names still parse) AND
// runtime CI stays green (its own fixture is unchanged) — yet on a live box the
// concierge would graft schedules the runtime silently drops or rejects.
//
// THE LINK (approach (b), in-core, no cross-repo run): this test feeds
// graftConciergeSchedules' REAL output through a Go transcription of the runtime
// seeder's ACCEPTED-KEY contract and asserts every grafted entry is seed-valid.
// A grafted shape the runtime would not accept therefore fails CORE CI here.
//
// CONTRACT SOURCE (molecule-ai-workspace-runtime, read at authoring time):
//   - molecule_runtime/schedule_seed.py::_normalize_config_entry — the config.yaml
//     seed path the graft feeds: cron_expr→cron and prompt_file→prompt aliases,
//     then a filter to (name, cron, timezone, prompt, enabled) → validate_entry.
//   - molecule_runtime/schedule_store.py::validate_entry — the scheduleEntry
//     JSON-Schema (additionalProperties:false; required name/cron/prompt; name
//     pattern; length caps) + the 16384-byte prompt cap + cronspec.validate.
//   - molecule_runtime/contracts/schedule.schema.json ($defs/scheduleEntry) — the
//     accepted-key SSOT (name pattern ^[a-z0-9]+(?:-[a-z0-9]+)*$, caps).
//
// The CRON leg is checked with internal/cronspec.Validate — the SAME shared cron
// contract the runtime's validate_entry calls (molecule_runtime/cronspec, gated
// cross-language-equivalent to internal/cronspec via the shared cron fixtures;
// see cronspec.go package doc). So the cron half of "seed-valid" is a REAL shared
// contract, not a hand-mirror.
//
// RESIDUAL (honest): the field-shape rules below are a Go transcription of a
// Python contract that lives in another repo; they are not executed by the actual
// Python seeder. The exact-byte end-to-end closure is #4555 (the live graft→seed
// loop). This test closes the SHAPE-DRIFT gap — a grafted key/shape the runtime's
// documented accepted contract rejects fails here — which is the class the audit
// flagged. Every rule is negative-controlled below so the guard has a REACHABLE
// fail arm (a divergent grafted key fails the assertion).

import (
	"fmt"
	"regexp"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/cronspec"
	"gopkg.in/yaml.v3"
)

// Runtime accepted-key contract constants (schedule.schema.json $defs/scheduleEntry
// + schedule_store.py caps). Kept as named locals so a drift in the runtime caps
// is a one-line, reviewable edit here.
const (
	seedNameMaxLen    = 128   // scheduleEntry.name maxLength
	seedPromptMaxByte = 16384 // schedule_store.MAX_PROMPT_BYTES
)

// seedNamePattern mirrors scheduleEntry.name.pattern in schedule.schema.json.
var seedNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// seedRecognizedKeys are the ONLY keys the runtime config.yaml seed path knows.
// scheduleEntry is additionalProperties:false and _normalize_config_entry filters
// to the contract fields; a grafted key outside this set is a silent divergence
// (core believes it delivered something the runtime never reads), so we FLAG it —
// that is exactly the drift this linkage exists to catch.
var seedRecognizedKeys = map[string]bool{
	"name": true, "cron": true, "cron_expr": true,
	"prompt": true, "prompt_file": true,
	"timezone": true, "enabled": true,
}

// validateGraftedEntrySeedValid returns nil iff the raw grafted schedule entry
// satisfies the runtime seeder's accepted-key contract. It is a faithful Go
// transcription of molecule_runtime schedule_seed._normalize_config_entry +
// schedule_store.validate_entry (see file header for the exact source seams).
func validateGraftedEntrySeedValid(raw map[string]any) error {
	// (0) additionalProperties:false — no key the runtime does not recognize.
	for k := range raw {
		if !seedRecognizedKeys[k] {
			return fmt.Errorf("unrecognized key %q (scheduleEntry is additionalProperties:false; runtime seeder does not read it)", k)
		}
	}

	// (1) name — required, string, 1..128, slug pattern.
	name, ok := raw["name"].(string)
	if !ok || name == "" {
		return fmt.Errorf("missing/empty required string `name`")
	}
	if len(name) > seedNameMaxLen {
		return fmt.Errorf("name exceeds %d chars", seedNameMaxLen)
	}
	if !seedNamePattern.MatchString(name) {
		return fmt.Errorf("name %q violates pattern %s", name, seedNamePattern.String())
	}

	// (2) cron — required; `cron_expr` is the authoring alias (schedule_seed.py).
	cron, hasCron := stringField(raw, "cron")
	if !hasCron {
		cron, hasCron = stringField(raw, "cron_expr")
	}
	if !hasCron || cron == "" {
		return fmt.Errorf("missing/empty required cron (`cron` or alias `cron_expr`)")
	}

	// (3) timezone — optional; defaults to UTC; must be a non-empty string if set.
	tz := "UTC"
	if v, present := raw["timezone"]; present {
		s, isStr := v.(string)
		if !isStr || s == "" {
			return fmt.Errorf("timezone must be a non-empty string when present")
		}
		tz = s
	}

	// cronspec.Validate is the SHARED cron contract the runtime's validate_entry
	// calls — enforces the 128-char cap, the timezone, and 5-field grammar.
	if err := cronspec.Validate(cron, tz); err != nil {
		return fmt.Errorf("cron fails the shared cron contract: %w", err)
	}

	// (4) prompt — required; inline `prompt` (byte-capped) OR `prompt_file` alias.
	if prompt, hasInline := stringField(raw, "prompt"); hasInline {
		if prompt == "" {
			return fmt.Errorf("`prompt` is present but empty")
		}
		if len([]byte(prompt)) > seedPromptMaxByte {
			return fmt.Errorf("prompt exceeds %d bytes", seedPromptMaxByte)
		}
	} else if pf, hasFile := stringField(raw, "prompt_file"); hasFile {
		if pf == "" {
			return fmt.Errorf("`prompt_file` is present but empty")
		}
		// Content/size of a prompt_file is only knowable with the configs dir at
		// seed time; the graft ships inline prompts, so presence is the contract
		// check here (confinement is covered by the runtime seed suite).
	} else {
		return fmt.Errorf("missing required prompt (`prompt` inline or `prompt_file`)")
	}

	// (5) enabled — optional; must be a bool if present (schema type:boolean).
	if v, present := raw["enabled"]; present {
		if _, isBool := v.(bool); !isBool {
			return fmt.Errorf("enabled must be a boolean when present")
		}
	}
	return nil
}

// stringField returns (value,true) only when key is present AND a string.
func stringField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, isStr := v.(string)
	return s, isStr
}

// extractGraftedScheduleEntries composes the concierge config from the given
// platform-agent `schedules:` block, runs the REAL graft, and returns the grafted
// entries as raw maps (the exact byte shape a runtime box would seed). It fails
// the test if the composed config does not parse — a config that bricks boot is
// never "seed-valid".
func extractGraftedScheduleEntries(t *testing.T, schedulesBlock string) []map[string]any {
	t.Helper()
	t.Setenv("MOLECULE_ORG_ID", "") // self-host: the graft is enabled.
	dir := t.TempDir()
	writeConciergeScheduleGraftFixtures(t, dir,
		"name: Org Concierge\nruntime: hermes\n"+schedulesBlock)
	h := &WorkspaceHandler{configsDir: dir}

	composed, err := h.composeConciergeRuntimeConfig("hermes")
	if err != nil {
		t.Fatalf("composeConciergeRuntimeConfig error: %v", err)
	}
	var doc struct {
		Schedules []map[string]any `yaml:"schedules"`
	}
	if err := yaml.Unmarshal(composed, &doc); err != nil {
		t.Fatalf("composed config.yaml does not parse (would brick boot): %v\n%s", err, composed)
	}
	return doc.Schedules
}

// TestGraftedSchedulesSatisfyRuntimeSeedContract is the LINKAGE: the schedules
// the core graft actually emits are fed through the runtime seeder's accepted-key
// contract, and every entry must be seed-valid. If the grafted byte shape drifts
// out of the shape the runtime accepts, THIS core test fails — closing the audit
// gap where such a drift passed both the core graft suite and the runtime seed
// suite.
func TestGraftedSchedulesSatisfyRuntimeSeedContract(t *testing.T) {
	entries := extractGraftedScheduleEntries(t, conciergeGraftSchedulesBlock)
	if len(entries) != 2 {
		t.Fatalf("expected 2 grafted schedule entries, got %d: %+v", len(entries), entries)
	}
	for i, e := range entries {
		if err := validateGraftedEntrySeedValid(e); err != nil {
			t.Errorf("grafted schedule[%d] %+v is NOT runtime-seed-valid: %v", i, e, err)
		}
	}
}

// TestGraftedScheduleSeedContract_NegativeControls proves the guard has a
// REACHABLE fail arm: for each divergent schedules block, running the REAL graft
// output through the seed contract must FAIL. Each case is a concrete shape drift
// the audit gap would otherwise let through both suites. A wantErr==false control
// (the canonical block) confirms the pipeline passes valid input, so the failures
// are the contract biting — not the pipeline being broken.
func TestGraftedScheduleSeedContract_NegativeControls(t *testing.T) {
	// Single-entry blocks so the failing entry is unambiguous.
	entry := func(lines string) string {
		return "schedules:\n    - " + lines + "\n"
	}
	cases := []struct {
		name    string
		block   string
		wantErr bool
	}{
		{
			name:    "canonical-block-is-valid",
			block:   conciergeGraftSchedulesBlock,
			wantErr: false,
		},
		{
			// `prompt` renamed to `message` — required prompt now missing.
			name:    "prompt-key-renamed-to-message",
			block:   entry("name: daily-standup\n      cron: 0 9 * * *\n      message: Post the standup."),
			wantErr: true,
		},
		{
			// `cron` renamed to `schedule` — required cron now missing.
			name:    "cron-key-renamed-to-schedule",
			block:   entry("name: daily-standup\n      schedule: 0 9 * * *\n      prompt: Post the standup."),
			wantErr: true,
		},
		{
			// An extra key the runtime does not read (additionalProperties:false).
			name:    "unrecognized-interval-key",
			block:   entry("name: daily-standup\n      cron: 0 9 * * *\n      prompt: Post the standup.\n      interval: 5m"),
			wantErr: true,
		},
		{
			// name violates the slug pattern (underscore + uppercase).
			name:    "name-violates-slug-pattern",
			block:   entry("name: Daily_Standup\n      cron: 0 9 * * *\n      prompt: Post the standup."),
			wantErr: true,
		},
		{
			// cron is not a valid 5-field expression (shared cron contract).
			name:    "cron-not-a-valid-expression",
			block:   entry("name: daily-standup\n      cron: not-a-cron\n      prompt: Post the standup."),
			wantErr: true,
		},
		{
			// enabled is a string, not a boolean.
			name:    "enabled-wrong-type",
			block:   entry("name: daily-standup\n      cron: 0 9 * * *\n      prompt: Post the standup.\n      enabled: \"yes\""),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := extractGraftedScheduleEntries(t, tc.block)
			if len(entries) == 0 {
				t.Fatalf("no schedules were grafted for block:\n%s", tc.block)
			}
			var firstErr error
			for _, e := range entries {
				if err := validateGraftedEntrySeedValid(e); err != nil {
					firstErr = err
					break
				}
			}
			if tc.wantErr && firstErr == nil {
				t.Errorf("expected a seed-contract violation, but every grafted entry was seed-valid: %+v", entries)
			}
			if !tc.wantErr && firstErr != nil {
				t.Errorf("expected all grafted entries seed-valid, got: %v", firstErr)
			}
		})
	}
}

package handlers

// Slugify-on-write for template schedule names.
//
// The renderer used to SKIP any name that failed the runtime's grid grammar
// (^[a-z0-9]+(?:-[a-z0-9]+)*$). Template authors write display names, so 35 of
// the 37 schedules declared across the template repos have never fired — every
// one for this reason alone. The grammar belongs to the grid, not to the author,
// so the renderer now slugifies and logs the rename.
//
// Two properties have to hold together, and these tests pin both:
//
//   1. a name that is ALREADY valid renders byte-identical — the schedules that
//      work today must not move (their name is the grid's state key, so a
//      "harmless" normalisation would orphan their run state);
//   2. two authored names collapsing onto one key are REFUSED, not merged —
//      first claimant keeps the key, the collider is skipped loudly.

import (
	"regexp"
	"strings"
	"testing"
)

func slugSchedule(name, cron string) OrgSchedule {
	return OrgSchedule{Name: name, CronExpr: cron, Prompt: "do the thing"}
}

// renderedNames pulls the emitted `name:` values out of the YAML block, so the
// assertions read what a workspace would actually receive.
func renderedNames(t *testing.T, block string) []string {
	t.Helper()
	var out []string
	re := regexp.MustCompile(`(?m)^\s*-?\s*name:\s*"?([^"\n]+)"?\s*$`)
	for _, m := range re.FindAllStringSubmatch(block, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

func TestSlugify_MapsAuthoredDisplayNamesOntoTheGridGrammar(t *testing.T) {
	// Verbatim names from the template repos that have never fired.
	cases := map[string]string{
		"SEO Weekly Report (Monday 8:03 AM)":       "seo-weekly-report-monday-8-03-am",
		"B) Tue defensive: hreflang":               "b-tue-defensive-hreflang",
		"Heartbeat (every 30m)":                    "heartbeat-every-30m",
		"Daily Summary (9 PM Vancouver)":           "daily-summary-9-pm-vancouver",
		"Hourly plugin curation":                   "hourly-plugin-curation",
		"B) Twice-monthly audit (1st & 15th)":      "b-twice-monthly-audit-1st-15th",
		"Email Classification Review (daily 9 AM)": "email-classification-review-daily-9-am",
		"Orchestrator pulse":                       "orchestrator-pulse",
	}
	for authored, want := range cases {
		if got := slugifyScheduleName(authored); got != want {
			t.Errorf("slugify(%q) = %q, want %q", authored, got, want)
		}
		if !scheduleNamePattern.MatchString(want) {
			t.Errorf("expectation %q is itself not a valid grid key", want)
		}
	}
}

func TestSlugify_AlreadyValidNamesAreByteIdentical(t *testing.T) {
	// The two schedules that render today must not move: their name IS the
	// grid's state key, so renaming them would orphan their run state.
	for _, name := range []string{"a", "idle-digest", "seo-all-tick", "x9", "0-1-2"} {
		if got := slugifyScheduleName(name); got != name {
			t.Errorf("slugify(%q) = %q — an already-valid name must pass through unchanged", name, got)
		}
	}
}

func TestSlugify_ProducesEitherAValidKeyOrNothing(t *testing.T) {
	for _, name := range []string{
		"", "!!!", "---", "  ", "***  ***", "日本語", "-leading", "trailing-",
		"Multiple   Spaces", "punct.....heavy!!!", strings.Repeat("a", 200),
		strings.Repeat("ab ", 90), "MiXeD CaSe",
	} {
		got := slugifyScheduleName(name)
		if got == "" {
			continue // legitimately nothing survives; the caller skips it
		}
		if !scheduleNamePattern.MatchString(got) {
			t.Errorf("slugify(%q) = %q which is NOT a valid grid key", name, got)
		}
		if len(got) > maxScheduleNameLen {
			t.Errorf("slugify(%q) = %d chars, over the %d cap", name, len(got), maxScheduleNameLen)
		}
	}
}

func TestRender_SlugifiesInsteadOfSkipping(t *testing.T) {
	block, rendered, skipped := renderTemplateSchedulesYAML([]OrgSchedule{
		slugSchedule("SEO Weekly Report (Monday 8:03 AM)", "3 8 * * 1"),
		slugSchedule("B) Tue defensive: hreflang", "0 9 * * 2"),
	}, "", "", "ws")

	if rendered != 2 || skipped != 0 {
		t.Fatalf("rendered=%d skipped=%d — both should render now; block:\n%s", rendered, skipped, block)
	}
	got := renderedNames(t, block)
	want := []string{"seo-weekly-report-monday-8-03-am", "b-tue-defensive-hreflang"}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Errorf("emitted names %v, want %v", got, want)
			break
		}
	}
}

func TestRender_UnslugifiableNameIsStillSkipped(t *testing.T) {
	// Tolerance is bounded: if nothing in [a-z0-9] survives there is no key to
	// write, and the entry must still be dropped rather than emitted blank.
	_, rendered, skipped := renderTemplateSchedulesYAML([]OrgSchedule{
		slugSchedule("!!!", "0 9 * * *"),
	}, "", "", "ws")
	if rendered != 0 || skipped != 1 {
		t.Fatalf("rendered=%d skipped=%d — an unslugifiable name must be skipped", rendered, skipped)
	}
}

func TestRender_CollidingNamesAreRefusedNotMerged(t *testing.T) {
	// "Daily Report" and "daily report!" both slug to "daily-report". The name
	// is the grid's STATE KEY — emitting both would make the second silently
	// overwrite the first's run state.
	block, rendered, skipped := renderTemplateSchedulesYAML([]OrgSchedule{
		slugSchedule("Daily Report", "0 9 * * *"),
		slugSchedule("daily report!", "0 10 * * *"),
	}, "", "", "ws")

	if rendered != 1 || skipped != 1 {
		t.Fatalf("rendered=%d skipped=%d — the collider must be refused; block:\n%s", rendered, skipped, block)
	}
	got := renderedNames(t, block)
	if len(got) != 1 || got[0] != "daily-report" {
		t.Fatalf("emitted %v, want exactly [daily-report] (first claimant keeps the key)", got)
	}
	// The FIRST entry must be the survivor — its cron proves which one landed.
	if !strings.Contains(block, "0 9 * * *") || strings.Contains(block, "0 10 * * *") {
		t.Errorf("the first claimant must survive, not the collider; block:\n%s", block)
	}
}

func TestRender_ARejectedEntryDoesNotBlockAValidLaterCollision(t *testing.T) {
	// A slug is claimed only once an entry has survived every check. If the
	// first entry is thrown out for an invalid cron, a later entry slugging to
	// the same key is still valid and must render.
	block, rendered, skipped := renderTemplateSchedulesYAML([]OrgSchedule{
		slugSchedule("Daily Report", "not-a-cron"),
		slugSchedule("daily report", "0 10 * * *"),
	}, "", "", "ws")

	if rendered != 1 || skipped != 1 {
		t.Fatalf("rendered=%d skipped=%d — the valid later entry must render; block:\n%s", rendered, skipped, block)
	}
	if !strings.Contains(block, "0 10 * * *") {
		t.Errorf("the surviving entry should be the one with the valid cron; block:\n%s", block)
	}
}

func TestRender_ValidNamesStillRenderUnchanged(t *testing.T) {
	// Regression guard for the schedules that already work.
	block, rendered, skipped := renderTemplateSchedulesYAML([]OrgSchedule{
		slugSchedule("idle-digest", "*/30 * * * *"),
	}, "", "", "ws")
	if rendered != 1 || skipped != 0 {
		t.Fatalf("rendered=%d skipped=%d", rendered, skipped)
	}
	if got := renderedNames(t, block); len(got) != 1 || got[0] != "idle-digest" {
		t.Errorf("emitted %v, want [idle-digest] byte-identical", got)
	}
}

package handlers

// M2's done condition, executable: every schedule declared in the fleet's
// template repos renders, with zero template edits.
//
// Before slugify-on-write, renderTemplateSchedulesYAML skipped 35 of the 37
// entries in realTemplateSchedules — all for the same reason, an authored
// display name that is not a valid grid key. Those schedules have never fired
// in any workspace. This asserts the whole set now renders, per repo, and that
// nothing regressed for the two that already worked.

import (
	"testing"
)

func TestFleet_EveryDeclaredTemplateScheduleRenders(t *testing.T) {
	if len(realTemplateSchedules) == 0 {
		t.Fatal("fixture is empty — the done condition would be vacuous")
	}

	// Group by the DECLARING FILE, not the repo: renderTemplateSchedulesYAML
	// runs per workspace, so the grid namespace is one workspace's schedule
	// list. molecule-dev declares an "Orchestrator pulse" in three separate
	// team files — three different workspaces, no collision between them.
	byWorkspace := map[string][]OrgSchedule{}
	for _, s := range realTemplateSchedules {
		key := s.Repo + "/" + s.Path
		byWorkspace[key] = append(byWorkspace[key], OrgSchedule{
			Name:     s.Name,
			CronExpr: s.Cron,
			Timezone: s.TZ,
			// The fixture records whether the source used prompt/prompt_file;
			// prompt resolution is a separate concern with its own tests, so a
			// non-empty prompt is supplied here to isolate the NAME grammar —
			// the single reason all 35 were being skipped.
			Prompt: "scheduled work",
		})
	}

	totalRendered, totalSkipped := 0, 0
	for ws, scheds := range byWorkspace {
		_, rendered, skipped := renderTemplateSchedulesYAML(scheds, "", "", ws)
		totalRendered += rendered
		totalSkipped += skipped
		if skipped != 0 {
			t.Errorf("%s: %d of %d schedules still skipped", ws, skipped, len(scheds))
		}
	}

	if totalSkipped != 0 {
		t.Errorf("skipped=%d, want 0 — the done condition is 'all of them fire'", totalSkipped)
	}
	if totalRendered != len(realTemplateSchedules) {
		t.Errorf("rendered=%d of %d declared", totalRendered, len(realTemplateSchedules))
	}
	t.Logf("fleet: %d/%d schedules render across %d workspaces (was 2/%d before slugify)",
		totalRendered, len(realTemplateSchedules), len(byWorkspace), len(realTemplateSchedules))
}

func TestFleet_NoTwoSchedulesInAWorkspaceCollideOnOneGridKey(t *testing.T) {
	// Slugify is only safe if it does not silently merge two real schedules.
	// If this ever fires, that repo genuinely needs a rename — the collision is
	// refused at render time, so the failure is loud rather than data loss.
	seen := map[string]map[string]string{}
	for _, s := range realTemplateSchedules {
		key := slugifyScheduleName(s.Name)
		if key == "" {
			t.Errorf("%s: %q slugifies to nothing", s.Repo, s.Name)
			continue
		}
		ws := s.Repo + "/" + s.Path
		if seen[ws] == nil {
			seen[ws] = map[string]string{}
		}
		if prior, dup := seen[ws][key]; dup {
			t.Errorf("%s: %q and %q both slug to %q — needs a real rename", ws, prior, s.Name, key)
		}
		seen[ws][key] = s.Name
	}
}

func TestFleet_AlreadyValidNamesAreUntouched(t *testing.T) {
	// The two that render today must survive byte-identical: their name is the
	// grid's state key.
	untouched := 0
	for _, s := range realTemplateSchedules {
		if !scheduleNamePattern.MatchString(s.Name) {
			continue
		}
		untouched++
		if got := slugifyScheduleName(s.Name); got != s.Name {
			t.Errorf("%s: already-valid %q was rewritten to %q", s.Repo, s.Name, got)
		}
	}
	if untouched == 0 {
		t.Error("no already-valid names in the fixture — this guard would be vacuous")
	}
	t.Logf("%d already-valid names pass through unchanged", untouched)
}

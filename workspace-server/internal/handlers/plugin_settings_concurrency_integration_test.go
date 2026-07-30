//go:build integration

package handlers

// B2 — THE LOST UPDATE ON THE FIRST WRITE.
//
// patchOverrides guards concurrent edits with `SELECT … FOR UPDATE` + a
// compare-and-set on overrides_version. FOR UPDATE takes NO LOCK when the row
// DOES NOT EXIST — there is no tuple to lock. So two transactions racing the
// FIRST write both read ErrNoRows, both see current=0, both pass the CAS at
// expectedVersion==0, both compute newVersion=1, and the second
// `INSERT … ON CONFLICT DO UPDATE` overwrites `overrides` WHOLESALE. Both
// callers get 200 and version:1. No 409 is ever raised.
//
// TestIntegration_PluginSettings_ConcurrentEditIsRefusedNotLost does not catch
// this: it seeds the row via writeTemplateConfig first (so the row EXISTS and
// FOR UPDATE really locks) and it is sequential. Because of B1 the row comes
// into existence only on the first PATCH, so "first write" is the state EVERY
// workspace is in — the one case the existing test excludes.
//
// This control is therefore genuinely CONCURRENT and starts from an ABSENT row.

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
)

// seedTrialWorkspace is seedSettingsWorkspace with a per-trial unique name —
// workspaces has a (parent_id, name) uniqueness constraint, so a loop of trials
// cannot reuse one name.
func seedTrialWorkspace(t *testing.T, conn *sql.DB, trial int) string {
	t.Helper()
	id := seedWorkspace(t, conn, fmt.Sprintf("plugin-settings-race-%s-%d", t.Name(), trial))
	t.Cleanup(func() { conn.Exec(`DELETE FROM workspaces WHERE id = $1`, id) })
	return id
}

// concurrentFirstWriteTrials is high enough that a race this wide (the whole
// read-modify-write, not a narrow window) shows up every run. The measured
// pre-fix loss rate was 24/25.
const concurrentFirstWriteTrials = 25

// racePatch runs two patchOverrides calls against an ABSENT row at the same
// instant, each setting its OWN key, and reports what survived.
func racePatch(t *testing.T, conn *sql.DB, ws string) (aliceErr, bobErr error, survived map[string]settingValue) {
	t.Helper()
	ctx := context.Background()

	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	done.Add(2)

	run := func(key string, val any, actor string, out *error) {
		defer done.Done()
		start.Wait()
		_, err := patchOverrides(ctx, conn, ws, testPlugin, map[string]any{key: val}, nil, actor, 0)
		*out = err
	}
	go run("poll_seconds", 5, "alice", &aliceErr)
	go run("timezone", "Asia/Tokyo", "bob", &bobErr)

	start.Done() // release both at once
	done.Wait()

	_, overrides, _, err := loadPluginSettings(ctx, conn, ws, testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	return aliceErr, bobErr, overrides
}

// THE ASSERTION. Two operators making the FIRST edit concurrently: either both
// land, or one is REFUSED with a conflict. Neither may be silently discarded
// behind a 200.
func TestIntegration_PluginSettings_ConcurrentFirstWriteIsNeverLost(t *testing.T) {
	conn := settingsTestDB(t)

	lost, refused, bothLanded := 0, 0, 0
	for i := 0; i < concurrentFirstWriteTrials; i++ {
		ws := seedTrialWorkspace(t, conn, i) // fresh workspace ⇒ ABSENT row
		aliceErr, bobErr, overrides := racePatch(t, conn, ws)

		_, haveAlice := overrides["poll_seconds"]
		_, haveBob := overrides["timezone"]

		switch {
		case aliceErr == errOverridesVersionConflict || bobErr == errOverridesVersionConflict:
			refused++
		case aliceErr != nil:
			t.Fatalf("trial %d: alice failed unexpectedly: %v", i, aliceErr)
		case bobErr != nil:
			t.Fatalf("trial %d: bob failed unexpectedly: %v", i, bobErr)
		case haveAlice && haveBob:
			bothLanded++
		default:
			// Both callers were told 200/version:1 and one edit is GONE.
			lost++
		}
	}

	t.Logf("concurrent first-write trials=%d  both-landed=%d  refused-with-409=%d  SILENTLY-LOST=%d",
		concurrentFirstWriteTrials, bothLanded, refused, lost)
	if lost > 0 {
		t.Fatalf("LOST UPDATE: %d/%d concurrent first writes discarded an edit while both callers "+
			"were told the write succeeded. SELECT … FOR UPDATE locks nothing when the row does "+
			"not exist, so both transactions read current=0, both pass the CAS, and the second "+
			"INSERT … ON CONFLICT DO UPDATE overwrites `overrides` wholesale.",
			lost, concurrentFirstWriteTrials)
	}
}

// The existing test's guarantee must still hold on an EXISTING row: a caller
// quoting a stale version is refused, not applied.
func TestIntegration_PluginSettings_StaleVersionStillRefusedAfterLocking(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	v1, err := patchOverrides(ctx, conn, ws, testPlugin, map[string]any{"poll_seconds": 5}, nil, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := patchOverrides(ctx, conn, ws, testPlugin, map[string]any{"poll_seconds": 7}, nil, "bob", v1); err != nil {
		t.Fatalf("bob at the current version should succeed: %v", err)
	}
	if _, err := patchOverrides(ctx, conn, ws, testPlugin, map[string]any{"poll_seconds": 9}, nil, "alice", v1); err != errOverridesVersionConflict {
		t.Fatalf("stale-version write should be refused with a conflict, got %v", err)
	}
}

// A concurrent race on an EXISTING row: exactly one of two callers holding the
// same version may win. This is the shape the pre-fix test asserted only
// sequentially.
func TestIntegration_PluginSettings_ConcurrentEditOnExistingRowRefusesExactlyOne(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		ws := seedTrialWorkspace(t, conn, i)
		if err := writeTemplateConfig(ctx, conn, ws, testPlugin, map[string]any{"poll_seconds": 30}); err != nil {
			t.Fatal(err)
		}
		aliceErr, bobErr, _ := racePatch(t, conn, ws)
		conflicts := 0
		for _, err := range []error{aliceErr, bobErr} {
			if err == errOverridesVersionConflict {
				conflicts++
			} else if err != nil {
				t.Fatalf("trial %d: unexpected error %v", i, err)
			}
		}
		if conflicts != 1 {
			t.Fatalf("trial %d: two callers at version 0 must produce exactly ONE conflict, got %d "+
				"(alice=%v bob=%v)", i, conflicts, aliceErr, bobErr)
		}
	}
}

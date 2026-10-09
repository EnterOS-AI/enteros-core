package staginge2e

// plugin_install_report_freshness_test.go — the FAIL-BEFORE / GREEN-AFTER proof
// for the core#5026 reporting guard, runnable in the normal `go test ./...` gate
// (NO live tenant, NO build tag).
//
// The fail-before observations are the REAL ones, taken off the boxes:
//
//   - prod `enteros-ws-reno-stars-c4ebf3dc96f9` on runtime 0.4.72: booted
//     2026-08-01T20:29:40Z, POSTed the report twice (20:29:47, 20:30:02), both
//     401, registration only minted the bearer at 20:30:05 — and the stored row
//     still read 2026-07-31T19:04:32Z. That is the ADVANCED case: present, and
//     frozen across the restart that produced the boot.
//   - a workspace whose FIRST boot is on 0.4.72 never gets a row at all — prod
//     `enteros-ws-reno-stars-3c38d78e0a09` (runtime 0.4.71) has none. That is the
//     LANDED case.
//   - staging `enteros-ws-gm360repro-4c91e1bf6cca` on runtime 0.4.75: token minted
//     20:34:24.928Z, report stored 20:34:24.963Z — 35ms later, and it advanced
//     from the previous boot's. That is GREEN for both rules.
//
// Each rule is paired with a negative control so that "the gate is green" is a
// statement about the fleet and not about the assertion having no teeth.

import (
	"strings"
	"testing"
	"time"
)

// The real timestamps above, so the fixtures are the incident and not a
// plausible-looking invention.
var (
	renoStarsFrozenReport = time.Date(2026, 7, 31, 19, 4, 32, 0, time.UTC)
	renoStarsRestartBoot  = time.Date(2026, 8, 1, 20, 29, 40, 0, time.UTC)
	gm360FreshReport      = time.Date(2026, 8, 1, 20, 34, 24, 963561000, time.UTC)
)

func TestEvaluateBootInstallReportLanded_FailBeforeGreenAfter(t *testing.T) {
	cases := []struct {
		name    string
		got     InstallReportSnapshot
		wantOK  bool
		wantSub string
	}{
		{
			// THE fail-before case. A fresh workspace on 0.4.72 reaches online and
			// the control plane has nothing: the report 401'd on the only boot
			// there has ever been.
			name:    "RED_no_report_at_all_the_0_4_72_first_boot",
			got:     InstallReportSnapshot{Present: false},
			wantOK:  false,
			wantSub: "has NO plugin-install report",
		},
		{
			// A 200 carrying no reported_at is not evidence either — freshness is
			// the whole question, so a row that cannot answer it must not pass.
			name:    "RED_present_but_no_reported_at",
			got:     InstallReportSnapshot{Present: true},
			wantOK:  false,
			wantSub: "no reported_at",
		},
		{
			// GREEN: the 0.4.75 observation off the staging box.
			name:    "GREEN_report_landed",
			got:     InstallReportSnapshot{Present: true, ReportedAt: gm360FreshReport},
			wantOK:  true,
			wantSub: "reported its boot-install",
		},
		{
			// Negative control for the "any old row passes" mistake: a report that
			// is present and STALE still satisfies the first-boot rule, because
			// this rule deliberately makes no claim about age — that is the
			// restart rule's job. Asserting it here with a runner-side clock is
			// what would have made the gate a skew detector.
			name:    "GREEN_stale_row_is_out_of_scope_for_the_first_boot_rule",
			got:     InstallReportSnapshot{Present: true, ReportedAt: renoStarsFrozenReport},
			wantOK:  true,
			wantSub: "reported its boot-install",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := EvaluateBootInstallReportLanded("platform agent (concierge)", tc.got)
			if ok != tc.wantOK {
				t.Fatalf("EvaluateBootInstallReportLanded(%+v) ok=%v, want %v — reason: %s",
					tc.got, ok, tc.wantOK, reason)
			}
			if !strings.Contains(reason, tc.wantSub) {
				t.Fatalf("reason %q does not name the class %q", reason, tc.wantSub)
			}
			if !strings.Contains(reason, "platform agent (concierge)") {
				t.Fatalf("reason %q does not name WHICH workspace failed", reason)
			}
		})
	}
}

func TestEvaluateBootInstallReportAdvanced_FailBeforeGreenAfter(t *testing.T) {
	cases := []struct {
		name          string
		before, after InstallReportSnapshot
		wantOK        bool
		wantSub       string
	}{
		{
			// THE fail-before case, and the literal core#5026 incident: the box
			// restarted at 20:29:40 and the row still reads the previous day.
			name:    "RED_report_frozen_across_the_restart_core5026",
			before:  InstallReportSnapshot{Present: true, ReportedAt: renoStarsFrozenReport},
			after:   InstallReportSnapshot{Present: true, ReportedAt: renoStarsFrozenReport},
			wantOK:  false,
			wantSub: "did NOT advance",
		},
		{
			// The same class expressed as a clock going backwards (a rebuilt row,
			// a restored dump). Still not a report from the boot that is running.
			name:    "RED_report_went_backwards",
			before:  InstallReportSnapshot{Present: true, ReportedAt: gm360FreshReport},
			after:   InstallReportSnapshot{Present: true, ReportedAt: renoStarsFrozenReport},
			wantOK:  false,
			wantSub: "did NOT advance",
		},
		{
			// A restart must not be able to DELETE the control plane's view.
			name:    "RED_report_disappeared_after_the_restart",
			before:  InstallReportSnapshot{Present: true, ReportedAt: gm360FreshReport},
			after:   InstallReportSnapshot{Present: false},
			wantOK:  false,
			wantSub: "has NONE",
		},
		{
			// No baseline: this is the first-boot rule's failure, not the restart
			// rule's. Returning "advanced" here would be a vacuous pass over an
			// input the rule cannot actually judge.
			name:    "RED_no_baseline_belongs_to_the_first_boot_rule",
			before:  InstallReportSnapshot{Present: false},
			after:   InstallReportSnapshot{Present: true, ReportedAt: gm360FreshReport},
			wantOK:  false,
			wantSub: "no baseline",
		},
		{
			// GREEN: the 0.4.75 observation — the restart produced a NEW report.
			name:    "GREEN_report_advanced_across_the_restart",
			before:  InstallReportSnapshot{Present: true, ReportedAt: renoStarsRestartBoot},
			after:   InstallReportSnapshot{Present: true, ReportedAt: gm360FreshReport},
			wantOK:  true,
			wantSub: "re-reported its boot-install",
		},
		{
			// Negative control on the comparison's strictness: one microsecond of
			// advance is a genuine re-report (the handler's upsert sets
			// reported_at = NOW()), and must not be rounded away into a pass-by-
			// equality or a fail-by-threshold.
			name:    "GREEN_one_microsecond_of_advance_is_still_a_re_report",
			before:  InstallReportSnapshot{Present: true, ReportedAt: gm360FreshReport},
			after:   InstallReportSnapshot{Present: true, ReportedAt: gm360FreshReport.Add(time.Microsecond)},
			wantOK:  true,
			wantSub: "re-reported its boot-install",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := EvaluateBootInstallReportAdvanced("workspace", tc.before, tc.after)
			if ok != tc.wantOK {
				t.Fatalf("EvaluateBootInstallReportAdvanced(before=%+v, after=%+v) ok=%v, want %v — reason: %s",
					tc.before, tc.after, ok, tc.wantOK, reason)
			}
			if !strings.Contains(reason, tc.wantSub) {
				t.Fatalf("reason %q does not name the class %q", reason, tc.wantSub)
			}
		})
	}
}

// The two rules must not be able to agree that the 0.4.72 fleet was fine. This is
// the whole-file negative control: feed BOTH rules the real prod observation and
// require at least one of them to be RED, so a future edit that softens one of
// them cannot leave the incident silently passing.
func TestTheRealProdObservationIsRedSomewhere(t *testing.T) {
	// enteros-ws-reno-stars-c4ebf3dc96f9, runtime 0.4.72, boot 2026-08-01T20:29:40Z.
	frozen := InstallReportSnapshot{Present: true, ReportedAt: renoStarsFrozenReport}
	landedOK, landedWhy := EvaluateBootInstallReportLanded("reno-stars workspace", frozen)
	advancedOK, advancedWhy := EvaluateBootInstallReportAdvanced("reno-stars workspace", frozen, frozen)
	if landedOK && advancedOK {
		t.Fatalf("BOTH rules passed the actual core#5026 production observation — the guard covers nothing.\n"+
			"  landed:   %s\n  advanced: %s", landedWhy, advancedWhy)
	}
	// And it must be the restart rule that catches it: the row IS present, so a
	// presence-only guard false-passes here. Pinning which rule fires stops a
	// future refactor from quietly moving the coverage.
	if advancedOK {
		t.Fatalf("the restart rule passed a report frozen across the restart that produced the boot — "+
			"that is exactly core#5026: %s", advancedWhy)
	}
	if !landedOK {
		t.Fatalf("the first-boot rule REDded a present report; it is not supposed to judge age "+
			"(that would make it a clock-skew detector): %s", landedWhy)
	}
}

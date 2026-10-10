package staginge2e

// plugin_install_report_freshness.go — the PURE verdict for "did this boot's
// plugin-install report actually LAND in the control plane?" (core#5026).
//
// WHY THIS EXISTS
//
// workspace_plugin_install_reports is the control plane's ONLY view of declared-
// plugin health. GET /admin/plugin-install-reports answers from it and the
// molecule_plugin_install_degraded_workspaces gauge (core#5022) is computed from
// it. Both are therefore only as trustworthy as the arrival of the report.
//
// On runtime 0.4.72 the report could not arrive AT ALL. molecule_runtime.main
// posted it as boot step 1 of 8 — before /registry/register minted the workspace
// bearer the route requires — and the restart that produced the boot had already
// revoked the previous bearer (workspace_provision.go issueAndInjectToken revokes
// live instance tokens FIRST). Every boot POSTed with no Authorization header,
// core answered 401, the runtime's fail-soft arm swallowed it, and the stored row
// stayed frozen at whatever a much older boot had written. Measured on a live
// prod workspace: the row read 2026-07-31T19:04 days and many boots later.
//
// The defect is not what makes this file necessary. What makes it necessary is
// that NOTHING NOTICED. Every gate on the 0.4.72 promote was green: the runtime's
// own tests pass without a platform, the handler tests pass by writing the row
// themselves, and the staging e2e provisioned real workspaces and never once
// asked whether a report came out the other end. A monitor whose input silently
// freezes reports a fleet nobody measured — the vacuous-pass shape.
//
// WHY THE RULES ARE PURE AND UNTAGGED (mirrors platform_agent_mgmt_mcp_gate.go)
//
// The live assertions need a real staging tenant and live behind `staging_e2e`.
// The DECISION — "given what we observed about this workspace's report, did the
// reporting chain work?" — is data → verdict, so it lives here, in the normal
// `go test ./...` gate, with a fail-before proof against the exact 0.4.72
// observation (absent report → RED) and the 0.4.74+ one (report present, and
// advancing across a restart → GREEN). The live tests FEED these functions, so
// the rule that runs in the deploy path and the rule the unit test proves are the
// same code rather than two drifting copies.
//
// WHY SERVER TIMESTAMPS ONLY
//
// Neither rule compares a control-plane timestamp against the test runner's
// clock. reported_at is written by the tenant database (`reported_at = NOW()` in
// the handler's upsert), so comparing it to a runner-side time.Now() would make
// the gate a clock-skew detector as well as a reporting detector, and the first
// false RED would get the whole assertion switched off. Both rules below compare
// a server timestamp either to nothing at all (presence) or to ANOTHER server
// timestamp read from the same database (advancement).

import (
	"fmt"
	"time"
)

// InstallReportSnapshot is one observation of GET
// /workspaces/:id/plugin-install-report.
//
// Present distinguishes 200 from the handler's deliberate 404 ("this workspace
// has never reported a plugin boot-install"). That 404 is not an error condition
// to be smoothed over — it is the single most important observation this file
// makes, because it is exactly what a workspace whose report 401s looks like.
type InstallReportSnapshot struct {
	// Present is true iff the control plane HAS a report row for the workspace,
	// i.e. the read returned 200. A 404 means the box has never successfully
	// reported.
	Present bool
	// ReportedAt is the row's reported_at, as served by the control plane. Zero
	// when Present is false. Written by the tenant DB clock, never the runner's.
	ReportedAt time.Time
}

// EvaluateBootInstallReportLanded is the FIRST-BOOT rule: a workspace that this
// test run created, and then watched reach online, must have a boot-install
// report in the control plane.
//
// The implication is one-directional and exact, which is why this is a hard gate
// and not a warning:
//
//   - the runtime sends the report from the serve path, before heartbeat.start();
//   - a workspace cannot reach status=online without a heartbeat;
//   - therefore, by the time the caller has OBSERVED online, the report has
//     already been attempted. If it is absent, the attempt FAILED.
//
// There is no race to be tolerant of and no timing threshold to tune. `what`
// names the workspace class in the failure text (e.g. "platform agent
// (concierge)") so a red gate says which box, not just which assertion.
func EvaluateBootInstallReportLanded(what string, got InstallReportSnapshot) (ok bool, reason string) {
	if !got.Present {
		return false, fmt.Sprintf(
			"%s reached online but the control plane has NO plugin-install report for it "+
				"(GET /workspaces/:id/plugin-install-report is 404). The runtime sends that report "+
				"before heartbeat.start(), and online REQUIRES a heartbeat — so the send was "+
				"attempted and did not land. This is core#5026: on runtime 0.4.72 the report was "+
				"posted as boot step 1, before /registry/register minted the workspace bearer the "+
				"route requires, so it 401'd by construction on every boot and the runtime's "+
				"fail-soft arm swallowed it. The fleet read and the "+
				"molecule_plugin_install_degraded_workspaces gauge are computed from that row, so "+
				"a runtime that cannot report leaves both reporting a fleet nobody measured. "+
				"Ship a runtime that sends the report AFTER registration (>= 0.4.74)", what)
	}
	if got.ReportedAt.IsZero() {
		return false, fmt.Sprintf(
			"%s has a plugin-install report but it carries no reported_at. Freshness is the only "+
				"thing that distinguishes a report describing THIS boot from one frozen by a "+
				"much older one (core#5026), so a row without it cannot be trusted as evidence "+
				"of anything", what)
	}
	return true, fmt.Sprintf(
		"%s reported its boot-install to the control plane (reported_at=%s) — the reporting chain "+
			"the fleet read and the degraded gauge depend on is intact",
		what, got.ReportedAt.UTC().Format(time.RFC3339Nano))
}

// EvaluateBootInstallReportAdvanced is the RESTART rule, and it is the one that
// names core#5026 exactly: the restart that produces a boot revokes the token
// that boot's report authenticates with.
//
// A first-boot presence check cannot see that class on its own. A runtime could
// report fine on the very first boot (where nothing was revoked because nothing
// existed) and then be permanently silent on every subsequent restart — which is
// the shape prod was actually in, since prod workspaces restart constantly and
// first-boot happened long ago. So: take the report before a restart, restart the
// workspace, wait for it back online, and require reported_at to ADVANCE.
//
// Both timestamps come from the same tenant database via the same endpoint, so
// the comparison is immune to runner clock skew. Strictly-after, not
// after-or-equal: the handler's upsert sets `reported_at = NOW()` on conflict, so
// a genuine re-report always moves it, and a restart takes seconds while Postgres
// keeps microseconds. An unchanged timestamp therefore means the row was not
// rewritten — the new boot did not report.
func EvaluateBootInstallReportAdvanced(what string, before, after InstallReportSnapshot) (ok bool, reason string) {
	if !after.Present {
		return false, fmt.Sprintf(
			"%s had a plugin-install report before the restart but the control plane has NONE "+
				"after it. A restart must not be able to destroy the control plane's view of "+
				"plugin health (core#5026)", what)
	}
	if !before.Present {
		// Nothing to advance FROM. The first-boot rule owns this case and names it
		// far better; saying "advanced" here would be a vacuous pass, and saying
		// "did not advance" would blame the restart for a report that never
		// existed.
		return false, fmt.Sprintf(
			"%s has a report after the restart but had NONE before it, so the restart rule has no "+
				"baseline to compare against. Run EvaluateBootInstallReportLanded on the initial "+
				"boot first — that is the assertion this state actually violated", what)
	}
	if !after.ReportedAt.After(before.ReportedAt) {
		return false, fmt.Sprintf(
			"%s restarted and came back online, but its plugin-install report did NOT advance "+
				"(reported_at is still %s). THIS IS core#5026: the restart revokes the workspace "+
				"token (issueAndInjectToken revokes live instance tokens first) and a runtime that "+
				"sends the report before /registry/register mints the replacement posts it with a "+
				"dead credential, gets 401, and swallows it. The row then describes a boot that no "+
				"longer exists, so GET /admin/plugin-install-reports and the "+
				"molecule_plugin_install_degraded_workspaces gauge answer from a snapshot that can "+
				"neither clear when the fleet recovers nor fire when it breaks. The report must be "+
				"sent AFTER registration (runtime >= 0.4.74)",
			what, before.ReportedAt.UTC().Format(time.RFC3339Nano))
	}
	return true, fmt.Sprintf(
		"%s re-reported its boot-install across the restart (%s → %s) — the report describes the "+
			"boot that is running, not one the restart already replaced",
		what,
		before.ReportedAt.UTC().Format(time.RFC3339Nano),
		after.ReportedAt.UTC().Format(time.RFC3339Nano))
}

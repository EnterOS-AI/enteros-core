//go:build staging_e2e

package staginge2e

// plugin_install_report_freshness_e2e_test.go — the LIVE half of the core#5026
// reporting guard: read GET /workspaces/:id/plugin-install-report off a real
// tenant and hand the observation to the pure rules in
// plugin_install_report_freshness.go.
//
// There is no test function here. These are helpers, called from the two live
// suites that already provision real workspaces in the deploy gate
// (staging-tenant-cd.yml job e2e-smoke runs exactly
// TestWorkspaceLifecycle_Staging and TestPlatformAgentMgmtMCP_Staging):
//
//   - TestPlatformAgentMgmtMCP_Staging — a FRESH org's concierge reached online,
//     so its report must EXIST (assertBootInstallReportLanded).
//   - TestWorkspaceLifecycle_Staging — a workspace RESTARTED and came back
//     online, so its report must have ADVANCED (assertBootInstallReportAdvanced).
//
// AUTH — this route is mounted under wsAuth (router.go), which binds a workspace
// bearer to :id but also accepts the tenant ADMIN_TOKEN (middleware.WorkspaceAuth
// admin fallback). These suites hold exactly that token, so the read needs no new
// credential and no new endpoint. Verified live against a staging tenant before
// this was written: the admin bearer plus X-Molecule-Org-Id returns 200 with the
// row, and the handler's honest 404 when the workspace has never reported.

import (
	"net/http"
	"testing"
	"time"
)

// installReportPollInterval / installReportBudget bound the live reads.
//
// The budget is NOT a race window to be tuned. The runtime sends the report
// before heartbeat.start() and a workspace cannot reach online without a
// heartbeat, so by the time these helpers run the send has already happened and a
// correct fleet answers on the first poll (measured on staging: the row lands
// ~35ms after the token is minted). The budget exists so that a slow COMMIT or a
// slow tenant read is not misreported as "the runtime cannot report" — the thing
// being asserted is arrival, not latency. A broken fleet spends the budget and
// then REDs with the class named.
const (
	installReportPollInterval = 10 * time.Second
	installReportBudget       = 3 * time.Minute
)

// readInstallReport performs one read. A 200 yields Present=true plus the row's
// reported_at; the handler's 404 ("this workspace has never reported a plugin
// boot-install") yields Present=false, which is the observation the whole guard
// turns on. Any other status is treated as "not observed" rather than "absent" —
// a 503 from a restarting tenant must not be reported as a runtime that cannot
// report, so those simply consume budget.
func readInstallReport(t *testing.T, host, token, orgID, wsID string) (InstallReportSnapshot, int) {
	t.Helper()
	u := "https://" + host + "/workspaces/" + wsID + "/plugin-install-report"
	hs, body := doTenantJSON(t, "GET", u, token, orgID, "")
	switch hs {
	case http.StatusOK:
		return InstallReportSnapshot{Present: true, ReportedAt: parseReportedAt(body)}, hs
	case http.StatusNotFound:
		return InstallReportSnapshot{Present: false}, hs
	default:
		return InstallReportSnapshot{}, hs
	}
}

// parseReportedAt pulls the row's reported_at. Top-level decode (not the flat
// jsonField scanner) for the same reason workspaceStatusAndURL uses
// topLevelString: a nested key must not be able to shadow the load-bearing one.
// An unparseable value yields the zero time, which the pure rule REDs rather than
// silently treating as "fresh".
func parseReportedAt(body string) time.Time {
	raw := topLevelString(body, "reported_at")
	if raw == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return ts
}

// assertBootInstallReportLanded is the FIRST-BOOT assertion: this run created the
// workspace and watched it reach online, so the control plane must now hold a
// boot-install report for it.
//
// Polls until a report appears or the budget runs out, then hands the LAST
// observation to the pure rule — the verdict and its wording live there, so the
// gate that runs in the deploy path is the gate the untagged unit test proves.
func assertBootInstallReportLanded(t *testing.T, host, token, orgID, wsID, what string) InstallReportSnapshot {
	t.Helper()
	deadline := time.Now().Add(installReportBudget)
	var (
		got      InstallReportSnapshot
		lastCode int
	)
	for {
		got, lastCode = readInstallReport(t, host, token, orgID, wsID)
		if got.Present {
			break
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(installReportPollInterval)
	}
	ok, reason := EvaluateBootInstallReportLanded(what, got)
	if !ok {
		t.Fatalf("core#5026 reporting gate RED (workspace=%s last HTTP %d after %s): %s",
			wsID, lastCode, installReportBudget, reason)
	}
	t.Logf("core#5026 reporting gate GREEN: %s", reason)
	return got
}

// assertBootInstallReportAdvanced is the RESTART assertion, and the one that
// names the defect exactly: the restart revokes the token the report
// authenticates with, so a runtime that reports before registration goes
// permanently silent from the second boot onward.
//
// `before` must have been captured BEFORE the restart was requested. Polls until
// the report advances past it or the budget runs out; the pure rule decides.
func assertBootInstallReportAdvanced(t *testing.T, host, token, orgID, wsID, what string, before InstallReportSnapshot) InstallReportSnapshot {
	t.Helper()
	deadline := time.Now().Add(installReportBudget)
	var (
		after    InstallReportSnapshot
		lastCode int
	)
	for {
		after, lastCode = readInstallReport(t, host, token, orgID, wsID)
		if after.Present && after.ReportedAt.After(before.ReportedAt) {
			break
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(installReportPollInterval)
	}
	ok, reason := EvaluateBootInstallReportAdvanced(what, before, after)
	if !ok {
		t.Fatalf("core#5026 reporting gate RED (workspace=%s last HTTP %d after %s): %s",
			wsID, lastCode, installReportBudget, reason)
	}
	t.Logf("core#5026 reporting gate GREEN: %s", reason)
	return after
}

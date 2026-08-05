package staginge2e

// admin_diagnostics.go — the best-effort CP admin diagnostic fetch used to
// ANNOTATE a Guard B provisioning-timeout failure.
//
// WHY THIS EXISTS
//
//	adminCreateOrg used to fail with exactly one line — "org <slug> did not
//	reach instance_status=running within timeout" — and nothing else. That line
//	is what the staging-tenant-cd HARD GATE printed on runs 616479 and 617016.
//	It named neither the last status the control plane reported nor the reason
//	provisioning stalled. The actual cause was a CP-side check-constraint
//	rejection (organizations_provider_check: migration 071 narrowed the allowed
//	set while the local-docker provisioner still wrote the literal 'local'), a
//	fact the control plane already knew and this gate simply never asked for.
//
//	Worse, the gate DELETES its own evidence: the t.Cleanup teardown purges the
//	throwaway org moments after the failure, so by the time anyone reads the log
//	the tenant it is asking about no longer exists and the only route left is a
//	live reproduction. Two separate investigations of a red Guard B were spent
//	re-deriving the constraint error by hand for exactly that reason.
//
//	So the failure now carries the two admin surfaces the CP already exposes for
//	this question — /cp/admin/tenants/:slug/diagnostics (org/instance presence
//	and status, DNS, tunnel) and /cp/admin/tenants/:slug/boot-events — captured
//	BEFORE teardown runs.
//
// WHY UNTAGGED
//
//	Its caller lives in a `staging_e2e`-tagged file, which the normal
//	`go test ./...` gate never type-checks. Kept here (untagged, with an
//	untagged test) so a compile break or a behaviour regression in the
//	diagnostic path is caught by ordinary CI rather than by the next red deploy
//	— the same reason orginstancestatus.go and platform_agent_mgmt_mcp_gate.go
//	are untagged.

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// adminDiagnosticTimeout bounds a single diagnostic fetch. It is short on
// purpose: this runs on an already-failing path, and a slow control plane must
// not turn a clear verdict into a hung test.
const adminDiagnosticTimeout = 20 * time.Second

// bestEffortAdminGet fetches a CP admin diagnostic surface for inclusion in a
// FAILURE message.
//
// It NEVER fails the test and never panics: every error path returns a
// human-readable string. Its only caller is already failing for another reason
// and must not have its verdict replaced by a secondary transport error — which
// is precisely why it does not reuse doJSON, whose transport-error path calls
// t.Fatalf and would swap the real diagnosis for "connection reset".
//
// The body is capped at a2aTurnLogCap for log hygiene. That cap is wide (4000)
// because the failure this annotates is a multi-field JSON object and the field
// that names the cause can be anywhere in it — the same lesson core#5052 learned
// when a 200-char cap destroyed the A2A diagnostic on every red run.
func bestEffortAdminGet(cpBase, adminToken, path string) string {
	base := strings.TrimRight(strings.TrimSpace(cpBase), "/")
	if base == "" {
		return "<unavailable: no CP base URL>"
	}
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return "<unavailable: " + err.Error() + ">"
	}
	if adminToken != "" {
		req.Header.Set("Authorization", "Bearer "+adminToken)
	}
	client := &http.Client{Timeout: adminDiagnosticTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "<unavailable: " + err.Error() + ">"
	}
	defer resp.Body.Close()
	return fmt.Sprintf("HTTP %d %s", resp.StatusCode, truncate(readBody(resp), a2aTurnLogCap))
}

// readBody drains an HTTP response into a string, bounded at 1 MiB.
//
// Moved here from the `staging_e2e`-tagged workspace_lifecycle_test.go so the
// normal CI gate type-checks it and the untagged diagnostic path above can
// share the ONE reader instead of growing a second copy that could drift.
func readBody(resp *http.Response) string {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, e := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if e != nil || len(buf) > 1<<20 {
			break
		}
	}
	return string(buf)
}

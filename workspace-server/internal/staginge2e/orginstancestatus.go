package staginge2e

import "encoding/json"

// orgInstanceStatus returns the instance_status for exactly the org whose slug
// matches, from a GET /cp/admin/orgs list response ({"orgs":[{...}]}).
//
// It MUST parse the list as real JSON and key on the exact slug — NOT a
// character-window scan. The prior implementation grabbed the FIRST
// "instance_status" within ±600 chars of the slug, but the admin summary omits
// instance_status entirely for a not-yet-provisioned org (json:",omitempty"),
// and the list is ordered created_at DESC (a fresh org is first). So the window
// bled straight into the NEXT (older, already-"running") org and returned ITS
// status — making adminCreateOrg return "running" ~instantly for a tenant that
// had not even inserted its org_instances row yet. The e2e then POSTed
// /workspaces before the tenant ws-server bound :8080 → the CP reverse-proxy
// EOF'd → 502 (the deterministic staging e2e-smoke HARD GATE failure). A
// missing/omitted field now correctly reads as "" (not running), so the wait
// actually waits for the CP's genuine readiness signal.
//
// Kept in an UNTAGGED file (its test is untagged too) so the regression guard
// runs in the normal `go test ./...` CI gate, not only under -tags staging_e2e.
func orgInstanceStatus(listBody, slug string) string {
	status, _, _ := orgInstanceRow(listBody, slug)
	return status
}

// orgInstanceRow returns everything the admin list already knows about exactly
// one org: its instance_status, the control plane's OWN failure reason
// (org_instances.last_error, surfaced as `last_error` and populated on
// instance_status="failed" — CP issue #285), and whether the row was present at
// all.
//
// The last_error half is not decorative. adminCreateOrg used to poll this list
// for instance_status=="running" for a full 7 minutes and then report only
// "did not reach instance_status=running within timeout" — while the SAME JSON
// object it had just parsed carried the reason the provision failed. That is
// the org-level half of the positive-edge-only defect described in
// readiness_terminal_signal.go, and it is why nine e2e-smoke reds in retention
// spent 7 minutes each producing a log line that named nothing.
//
// `present` discriminates "the org row is not in the list at all" (a genuinely
// different bug — the create did not land, or the list is paginated past it)
// from "the row is there but its instance_status is omitted because the
// org_instances row does not exist yet" (the normal early state, since the
// admin summary marks the field json:",omitempty").
func orgInstanceRow(listBody, slug string) (status, lastError string, present bool) {
	var parsed struct {
		Orgs []struct {
			Slug           string `json:"slug"`
			InstanceStatus string `json:"instance_status"`
			LastError      string `json:"last_error"`
		} `json:"orgs"`
	}
	if err := json.Unmarshal([]byte(listBody), &parsed); err != nil {
		return "", "", false
	}
	for _, o := range parsed.Orgs {
		if o.Slug == slug {
			return o.InstanceStatus, o.LastError, true
		}
	}
	return "", "", false
}

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
	var parsed struct {
		Orgs []struct {
			Slug           string `json:"slug"`
			InstanceStatus string `json:"instance_status"`
		} `json:"orgs"`
	}
	if err := json.Unmarshal([]byte(listBody), &parsed); err != nil {
		return ""
	}
	for _, o := range parsed.Orgs {
		if o.Slug == slug {
			return o.InstanceStatus
		}
	}
	return ""
}

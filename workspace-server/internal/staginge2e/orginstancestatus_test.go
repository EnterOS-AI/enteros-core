package staginge2e

import "testing"

// TestOrgInstanceStatus pins the fresh-tenant workspace-CREATE 502 root fix on
// the harness side: adminCreateOrg must read the instance_status of EXACTLY the
// org it created, and treat a not-yet-provisioned org (instance_status omitted by
// the admin summary's json:",omitempty") as NOT running — so it keeps waiting
// instead of latching onto a neighbouring org's "running" and POSTing /workspaces
// before the tenant is up.
func TestOrgInstanceStatus(t *testing.T) {
	// Real admin-list shape: {"orgs":[...]} ordered created_at DESC (fresh org
	// first). The fresh org "e2e-life-1" has NO instance_status field yet; the
	// next (older) org "test13" is "running". The buggy ±600-char window parser
	// returned test13's "running" for e2e-life-1 — this asserts we now don't.
	freshFirst := `{"limit":100,"offset":0,"orgs":[` +
		`{"id":"a","slug":"e2e-life-1","name":"E2E","plan":"free","member_count":0,"created_at":"2026-07-05T02:45:27Z","updated_at":"2026-07-05T02:45:27Z"},` +
		`{"id":"b","slug":"test13","name":"T13","plan":"free","member_count":1,"instance_status":"running","created_at":"2026-07-05T01:00:00Z","updated_at":"2026-07-05T01:00:00Z"}` +
		`]}`

	cases := []struct {
		name string
		body string
		slug string
		want string
	}{
		{"fresh_org_no_status_field_is_not_running", freshFirst, "e2e-life-1", ""},
		{"neighbour_running_is_read_correctly", freshFirst, "test13", "running"},
		{"provisioning_reads_provisioning", `{"orgs":[{"slug":"x","instance_status":"provisioning"}]}`, "x", "provisioning"},
		{"running_reads_running", `{"orgs":[{"slug":"x","instance_status":"running"}]}`, "x", "running"},
		{"unknown_slug_is_empty", freshFirst, "does-not-exist", ""},
		{"garbage_body_is_empty_not_panic", `not json`, "x", ""},
		{"empty_orgs_is_empty", `{"orgs":[]}`, "x", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := orgInstanceStatus(c.body, c.slug); got != c.want {
				t.Fatalf("orgInstanceStatus(%s) = %q, want %q", c.slug, got, c.want)
			}
		})
	}
}

package handlers

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// ════════════════════════════════════════════════════════════════════════════
// core#5137 — "54 loaded, zero callable"
//
// These tests are the mutation harness for the corroboration record. The point
// of the record is that it can report FAILURE; a classifier that has only ever
// been seen returning "fine" is not a probe, so every direction below is driven
// explicitly and the captured production shape is pinned as a fixture.
// ════════════════════════════════════════════════════════════════════════════

// capturedReportedInventory is the VERBATIM loaded_mcp_tools inventory a real
// concierge published, captured from this fleet's Loki:
//
//	container enteros-ws-e2e-life-619276-4822-3a49a308a8c6
//	2026-08-05T08:18:56Z  (ts_ns 1785917936966421190)
//	"loaded_mcp_tools: enumerated 54 MCP tool id(s) from 2 declared server(s)"
//
// It is 54 ids, every one of them in the `molecule-platform` namespace — and it
// CONTAINS mcp__molecule-platform__provision_workspace, so the pre-existing
// membership gate (conciergePlatformMCPProvisionWorkspaceTool) passed on it.
// That is what made the old signal a vacuous pass: it went green on an
// inventory the model could not dispatch a single entry of.
var capturedReportedInventory = []string{
	"mcp__molecule-platform__add_request_message",
	"mcp__molecule-platform__apply_plugin_update",
	"mcp__molecule-platform__cancel_request",
	"mcp__molecule-platform__check_plugin_updates",
	"mcp__molecule-platform__check_requests",
	"mcp__molecule-platform__create_approval",
	"mcp__molecule-platform__create_issue",
	"mcp__molecule-platform__create_org_from_template",
	"mcp__molecule-platform__create_request",
	"mcp__molecule-platform__create_schedule",
	"mcp__molecule-platform__delete_org_secret",
	"mcp__molecule-platform__delete_schedule",
	"mcp__molecule-platform__delete_workspace_secret",
	"mcp__molecule-platform__deprovision_workspace",
	"mcp__molecule-platform__export_bundle",
	"mcp__molecule-platform__get_conversation_history",
	"mcp__molecule-platform__get_org",
	"mcp__molecule-platform__get_org_plugin_allowlist",
	"mcp__molecule-platform__get_request",
	"mcp__molecule-platform__get_schedule_history",
	"mcp__molecule-platform__get_workspace",
	"mcp__molecule-platform__get_workspace_migration_status",
	"mcp__molecule-platform__import_bundle",
	"mcp__molecule-platform__import_template",
	"mcp__molecule-platform__install_plugin",
	"mcp__molecule-platform__list_available_plugins",
	"mcp__molecule-platform__list_inbox",
	"mcp__molecule-platform__list_org_events",
	"mcp__molecule-platform__list_org_secrets",
	"mcp__molecule-platform__list_org_templates",
	"mcp__molecule-platform__list_org_tokens",
	"mcp__molecule-platform__list_orgs",
	"mcp__molecule-platform__list_pending_approvals",
	"mcp__molecule-platform__list_schedules",
	"mcp__molecule-platform__list_templates",
	"mcp__molecule-platform__list_workspace_secrets",
	"mcp__molecule-platform__list_workspaces",
	"mcp__molecule-platform__migrate_workspace_provider",
	"mcp__molecule-platform__mint_org_token",
	"mcp__molecule-platform__mint_workspace_token",
	"mcp__molecule-platform__pause_workspace",
	"mcp__molecule-platform__promote_to_production",
	"mcp__molecule-platform__provision_workspace",
	"mcp__molecule-platform__respond_request",
	"mcp__molecule-platform__restart_workspace",
	"mcp__molecule-platform__resume_workspace",
	"mcp__molecule-platform__revoke_org_token",
	"mcp__molecule-platform__run_schedule",
	"mcp__molecule-platform__set_llm_billing_mode",
	"mcp__molecule-platform__set_org_plugin_allowlist",
	"mcp__molecule-platform__set_org_secret",
	"mcp__molecule-platform__set_workspace_budget",
	"mcp__molecule-platform__set_workspace_secret",
	"mcp__molecule-platform__update_schedule",
}

// capturedDispatchedToolIDs are tool ids the MODEL actually dispatched, taken
// from the same fleet's recorded turns over the same window. Every recorded
// dispatch is in the `molecule` namespace (the A2A sidecar the runtime template
// wires into the executor's own config) or a per-workspace plugin namespace —
// never `molecule-platform`.
var capturedDispatchedToolIDs = []string{
	"mcp__molecule__send_message_to_user",
	"mcp__molecule__desktop_open_url",
	"mcp__molecule__desktop_screenshot",
	"mcp__molecule__chat_history",
	"mcp__molecule__list_peers",
	"mcp__molecule__recall_memory",
}

// capturedArg is a permissive sqlmock argument matcher that RECORDS the value
// it saw. Used for the mcp_surface JSON document: the assertions belong in the
// test body (where a mismatch can print the whole document) rather than in an
// opaque "expectation unmet".
type capturedArg struct{ seen *string }

func (m capturedArg) Match(v driver.Value) bool {
	switch s := v.(type) {
	case string:
		*m.seen = s
	case []byte:
		*m.seen = string(s)
	default:
		return false
	}
	return true
}

func dispatchSet(ids ...string) (map[string]struct{}, int) {
	out := map[string]struct{}{}
	n := 0
	for _, id := range ids {
		if ns := mcpToolIDNamespace(id); ns != "" {
			out[ns] = struct{}{}
			n++
		}
	}
	return out, n
}

// ── The headline: the probe reporting the real production failure ───────────

// TestMCPSurface_CapturedFleetShape_IsNotYetExercised drives the classifier with
// the EXACT production inputs — 54 reported ids, the real dispatch record — and
// pins what they DO and DO NOT establish.
//
// They establish that nothing has exercised the advertised namespace. They do
// NOT establish that it is unreachable: the 54 could be perfectly callable and
// simply never called, which on this fleet is the common case. So the verdict is
// an "unknown:", and the count a caller reads (dispatch_corroborated_count = 0)
// says "no corroboration" rather than "no capability".
func TestMCPSurface_CapturedFleetShape_IsNotYetExercised(t *testing.T) {
	if len(capturedReportedInventory) != 54 {
		t.Fatalf("fixture drift: captured inventory has %d ids, the captured log line said 54", len(capturedReportedInventory))
	}
	// Guard the vacuity of the fixture itself: the old membership gate must
	// PASS on it, otherwise this test would be proving something easier than
	// the real failure.
	found := false
	for _, id := range capturedReportedInventory {
		if id == conciergePlatformMCPProvisionWorkspaceTool {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("fixture drift: the captured inventory must contain %q — the whole point is that the OLD gate went green on it",
			conciergePlatformMCPProvisionWorkspaceTool)
	}

	dispatched, records := dispatchSet(capturedDispatchedToolIDs...)
	got := classifyMCPSurface(capturedReportedInventory, dispatched, records, nil, time.Now())

	if got.Verdict != verdictNotYetExercised {
		t.Errorf("verdict = %q, want %q — the captured shape is 54 advertised with no management dispatch on record, "+
			"which is NOT evidence the tools are unreachable", got.Verdict, verdictNotYetExercised)
	}
	// The strength prefix is the whole point: this must never read as a fault.
	if !strings.HasPrefix(string(got.Verdict), "unknown:") {
		t.Errorf("verdict %q does not carry an unknown: prefix — absence of a dispatch is not negative evidence", got.Verdict)
	}
	if got.ReportedCount != 54 {
		t.Errorf("reported_count = %d, want 54", got.ReportedCount)
	}
	if got.DispatchCorroboratedCount != 0 {
		t.Errorf("dispatch_corroborated_count = %d, want 0 — no dispatch has been observed in the reported namespace", got.DispatchCorroboratedCount)
	}
	if got.AdvertisedOnlyCount != 54 {
		t.Errorf("advertised_only_count = %d, want 54", got.AdvertisedOnlyCount)
	}
	if len(got.ReportedNamespaces) != 1 || got.ReportedNamespaces[0] != "molecule_platform" {
		// Canonical (folded) spelling — see mcpToolIDNamespace.
		t.Errorf("reported_namespaces = %v, want [molecule_platform]", got.ReportedNamespaces)
	}
	if len(got.DispatchedNamespaces) != 1 || got.DispatchedNamespaces[0] != "molecule" {
		t.Errorf("dispatched_namespaces = %v, want [molecule]", got.DispatchedNamespaces)
	}
}

// ── Mutation: BOTH directions ───────────────────────────────────────────────

// TestMCPSurface_Mutation_BothDirections breaks and unbreaks the loading path
// and asserts the classifier's output MOVES each time. A classifier that
// returned the same verdict for all four input classes would pass any
// single-direction test; this one pins that the four are distinct.
func TestMCPSurface_Mutation_BothDirections(t *testing.T) {
	platformOnly, _ := dispatchSet(capturedDispatchedToolIDs...) // dispatches only mcp__molecule__*
	// The "fixed" input must be an id the fleet can ACTUALLY emit. hermes
	// sanitises the server name, so a healthy concierge dispatches the
	// UNDERSCORE spelling; an earlier draft used the hyphenated form, which no
	// hermes workspace ever registers — that case would have asserted a state
	// production cannot reach.
	matching, matchingN := dispatchSet("mcp__molecule_platform__list_orgs")
	none := map[string]struct{}{}

	cases := []struct {
		name     string
		reported []string
		disp     map[string]struct{}
		records  int
		want     mcpSurfaceVerdict
		wantCorr int
	}{
		{
			// NOT EXERCISED: the model dispatches, but never these. On this
			// fleet that is the ORDINARY healthy state, so it must be reported
			// as unknown, never as a fault.
			name: "dispatches, but never the advertised namespace", reported: capturedReportedInventory,
			disp: platformOnly, records: len(capturedDispatchedToolIDs),
			want: verdictNotYetExercised, wantCorr: 0,
		},
		{
			// FIXED: same 54 ids, but now the model has actually dispatched one
			// of them. Same code path, opposite verdict.
			name: "fixed — reported namespace really dispatched", reported: capturedReportedInventory,
			disp: matching, records: matchingN,
			want: verdictCorroborated, wantCorr: 54,
		},
		{
			// NOT YET KNOWN: a fresh concierge nobody has talked to. Must NOT be
			// reported as contradicted (that would be a false accusation) and
			// must NOT be reported as corroborated (that would be the vacuous
			// pass again).
			name: "unknown — no dispatch record at all", reported: capturedReportedInventory,
			disp: none, records: 0,
			want: verdictNoDispatchRecord, wantCorr: 0,
		},
		{
			// PRODUCER DEAD: the enumeration published nothing. Distinct from
			// "published something wrong". The dispatch set is EMPTY here
			// because that is the only input the caller can produce: the
			// empty-inventory short-circuit in recordMCPSurfaceCorroboration
			// returns before the dispatch read runs, so a non-empty set paired
			// with a nil inventory is unreachable in production.
			name: "unknown — nothing reported", reported: nil,
			disp: none, records: 0,
			want: verdictNoInventory, wantCorr: 0,
		},
	}

	seen := map[mcpSurfaceVerdict]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyMCPSurface(tc.reported, tc.disp, tc.records, nil, time.Now())
			if got.Verdict != tc.want {
				t.Errorf("verdict = %q, want %q", got.Verdict, tc.want)
			}
			if got.DispatchCorroboratedCount != tc.wantCorr {
				t.Errorf("dispatch_corroborated_count = %d, want %d", got.DispatchCorroboratedCount, tc.wantCorr)
			}
			seen[got.Verdict] = true
		})
	}

	// Non-vacuity: four input classes, four distinct verdicts. If a future
	// simplification collapses any pair, the record stops discriminating and
	// this fails.
	if len(seen) != 4 {
		t.Errorf("the classifier produced %d distinct verdicts across 4 materially different inputs (%v) — a record that cannot discriminate is not a probe", len(seen), seen)
	}
}

// TestMCPSurface_PartialCorroboration covers the mixed fleet: a runtime that
// advertises TWO namespaces, only one of which the model dispatches from. The
// corroborated count must be the per-id count in the dispatched namespace, not
// the whole inventory — otherwise one real dispatch would launder an entire
// mis-namespaced list.
// The two servers here are the two STDIO servers a real concierge declares —
// `molecule-platform` and `molecule-self`, both seen in the captured boot. The
// url-transport `molecule` a2a sidecar is deliberately NOT used: the probe's
// _read_hermes_mcp_servers keeps only command-based servers, so the sidecar can
// never appear in a reported inventory and an earlier draft that put it there
// was asserting an impossible input.
func TestMCPSurface_PartialCorroboration(t *testing.T) {
	reported := []string{
		"mcp__molecule-self__list_schedules",
		"mcp__molecule-self__run_schedule",
		"mcp__molecule-platform__provision_workspace",
		"mcp__molecule-platform__list_orgs",
		"mcp__molecule-platform__get_org",
	}
	disp, n := dispatchSet("mcp__molecule_self__list_schedules")
	got := classifyMCPSurface(reported, disp, n, nil, time.Now())

	if got.Verdict != verdictCorroborated {
		t.Errorf("verdict = %q, want %q", got.Verdict, verdictCorroborated)
	}
	if got.DispatchCorroboratedCount != 2 {
		t.Errorf("dispatch_corroborated_count = %d, want 2 (only the molecule-self namespace is backed)", got.DispatchCorroboratedCount)
	}
	if got.AdvertisedOnlyCount != 3 {
		t.Errorf("advertised_only_count = %d, want 3", got.AdvertisedOnlyCount)
	}
}

// ── The direction-of-effect guard ───────────────────────────────────────────

// TestMCPSurface_DoesNotMoveThePromotionPredicate is the "state which direction
// it moves the outcome" check, executed rather than asserted in prose. The
// promotion predicate is (mcpToolsReady || provisionToolLoaded); corroboration
// is deliberately NOT a term of it, in either polarity. If a future edit wires
// the verdict into the flip — as a disjunct (promotes MORE) or as a blocker
// (denies the whole hermes fleet) — the promoted set changes and this fails.
func TestMCPSurface_DoesNotMoveThePromotionPredicate(t *testing.T) {
	verdicts := []mcpSurfaceVerdict{
		"", verdictNoInventory, verdictNoDispatchRecord, verdictNotYetExercised, verdictCorroborated,
	}
	for _, ready := range []bool{false, true} {
		for _, loaded := range []bool{false, true} {
			// The predicate as it is spelled in evaluateStatus.
			wantPromote := ready || loaded
			for _, v := range verdicts {
				gotLabel := conciergeOnlineEvidence(ready, loaded, v)
				// A label is emitted exactly when the promotion happens, and
				// never otherwise — that equivalence is the promoted set.
				gotPromote := gotLabel != ""
				if gotPromote != wantPromote {
					t.Errorf("verdict %q changed the PROMOTED SET for (ready=%v, loaded=%v): label=%q. Corroboration may only re-label; widening it promotes more rows on less evidence, and blocking on it denies online to every concierge that dispatches from another namespace.",
						v, ready, loaded, gotLabel)
				}
			}
		}
	}
}

// ── Sticky corroboration / windowed non-corroboration ───────────────────────

// TestMCPSurface_CorroborationIsSticky is the anti-flap proof. The dispatch read
// is a WINDOW; if the verdict were computed from the window alone, a
// corroborated workspace would silently revert as its platform dispatches aged
// out behind ordinary sidecar chatter — the label oscillating on traffic MIX
// while nothing about the workspace changed.
//
// Beat 1 observes a management dispatch. Beat 2's window contains ONLY sidecar
// traffic — exactly the ageing-out case — and must still read corroborated,
// because "this model has dispatched from namespace X" is an existence claim
// that a shorter window cannot falsify.
func TestMCPSurface_CorroborationIsSticky(t *testing.T) {
	reported := []string{conciergePlatformMCPProvisionWorkspaceTool}

	beat1Disp, beat1N := dispatchSet("mcp__molecule_platform__provision_workspace")
	beat1 := classifyMCPSurface(reported, beat1Disp, beat1N, nil, time.Now())
	if beat1.Verdict != verdictCorroborated {
		t.Fatalf("beat 1 verdict = %q, want %q", beat1.Verdict, verdictCorroborated)
	}
	if beat1.FirstCorroboratedAt == nil {
		t.Fatal("beat 1 did not stamp first_corroborated_at — the age of a sticky claim must be visible")
	}

	// Beat 2: the management dispatch has aged out of the window entirely.
	beat2Disp, beat2N := dispatchSet("mcp__molecule__send_message_to_user")
	beat2 := classifyMCPSurface(reported, beat2Disp, beat2N, &beat1, time.Now())

	if beat2.Verdict != verdictCorroborated {
		t.Errorf("verdict reverted to %q when the management dispatch aged out of the %d-row window — "+
			"corroboration is an existence claim and must not flap on traffic mix",
			beat2.Verdict, mcpDispatchLookbackRows)
	}
	if beat2.DispatchCorroboratedCount != 1 {
		t.Errorf("dispatch_corroborated_count = %d, want 1", beat2.DispatchCorroboratedCount)
	}
	// The windowed half must still tell the truth about THIS window.
	if len(beat2.DispatchedNamespaces) != 1 || beat2.DispatchedNamespaces[0] != "molecule" {
		t.Errorf("dispatched_namespaces = %v, want [molecule] — the windowed half must report the window, not the union", beat2.DispatchedNamespaces)
	}
	// And the sticky claim must not silently re-date itself.
	if beat2.FirstCorroboratedAt == nil || !beat2.FirstCorroboratedAt.Equal(*beat1.FirstCorroboratedAt) {
		t.Errorf("first_corroborated_at moved (%v -> %v) — the age of the claim must be its ORIGINAL age",
			beat1.FirstCorroboratedAt, beat2.FirstCorroboratedAt)
	}
}

// TestMCPSurface_StickinessCannotInventCorroboration guards the other
// direction: a prior record must only ever carry corroboration FORWARD, never
// manufacture it. A malformed/absent prior (nil) and a prior whose union is
// empty must both leave the verdict unchanged.
func TestMCPSurface_StickinessCannotInventCorroboration(t *testing.T) {
	reported := []string{conciergePlatformMCPProvisionWorkspaceTool}
	disp, n := dispatchSet("mcp__molecule__send_message_to_user")

	empty := mcpSurfaceReport{} // e.g. a pre-sticky document, or a parse miss
	for name, prior := range map[string]*mcpSurfaceReport{"nil prior": nil, "empty union": &empty} {
		got := classifyMCPSurface(reported, disp, n, prior, time.Now())
		if got.Verdict != verdictNotYetExercised {
			t.Errorf("%s: verdict = %q, want %q — a prior record must not be able to mint corroboration",
				name, got.Verdict, verdictNotYetExercised)
		}
		if got.FirstCorroboratedAt != nil {
			t.Errorf("%s: first_corroborated_at set without corroboration", name)
		}
	}
}

// TestMCPSurface_NoVerdictIsNegativeEvidence pins the vocabulary itself: every
// verdict this classifier can produce is either a positive observation
// ("dispatch_observed:") or an admission of ignorance ("unknown:"). Nothing it
// emits may read as a fault finding, because nothing it can observe establishes
// one — dispatch records are existential, and no quantity of non-observation
// proves unreachability. A future value spelled "contradicted:"/"degraded:"/
// "failed:" fails here and should instead be derived from the runtime's
// loaded_not_model_facing.
func TestMCPSurface_NoVerdictIsNegativeEvidence(t *testing.T) {
	for _, v := range []mcpSurfaceVerdict{
		verdictNoInventory, verdictNoDispatchRecord, verdictNotYetExercised, verdictCorroborated,
	} {
		s := string(v)
		if !strings.HasPrefix(s, "unknown:") && !strings.HasPrefix(s, "dispatch_observed:") {
			t.Errorf("verdict %q is neither an observation nor an admission of ignorance — "+
				"a fault finding cannot be derived from dispatch absence", s)
		}
		for _, forbidden := range []string{"contradict", "degraded", "failed", "broken", "callable"} {
			if strings.Contains(strings.ToLower(s), forbidden) {
				t.Errorf("verdict %q contains %q; this classifier cannot establish that", s, forbidden)
			}
		}
	}
}

// ── End-to-end: the real predicate, the real SQL ────────────────────────────
//
// The two tests below drive handler.Heartbeat through the actual mock DB, so
// they cover what no pure-function test can:
//
//   - recordMCPSurfaceCorroboration and its SQL (previously exercised by NO
//     test at all — the reviewer had to validate it by hand);
//   - the mcp_surface document that lands in the column;
//   - evaluateStatus's REAL promotion predicate, so a blocker wired in there
//     changes the promoted set and fails.
//
// They are mirror images: identical except for the SPELLING of the dispatched
// tool id, which is the whole defect.

// mcpSurfaceHeartbeatCase runs one Heartbeat with a reported inventory of the
// required (hyphenated) verb and a single tool_trace row carrying dispatchedID,
// and returns the persisted mcp_surface JSON. It ALWAYS expects the
// provisioning->online status write: if a mutant makes the verdict block
// promotion, that expectation goes unmet and the test fails.
func mcpSurfaceHeartbeatCase(t *testing.T, wsID, dispatchedID string, wantEvidence conciergeReadinessEvidence) string {
	t.Helper()
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status", "desired_generation"}).AddRow("", 0, "provisioning", int64(0)))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs(wsID, 0.0, "", 0, 60, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The raw inventory write — unchanged by this feature, pinned so a
	// regression that starts laundering the runtime's claim is caught.
	mock.ExpectExec("UPDATE workspaces SET\\s+loaded_mcp_tools").
		WithArgs([]byte(`["`+conciergePlatformMCPProvisionWorkspaceTool+`"]`), wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The sticky half: the prior record. NULL here exercises the
	// first-evaluation path (empty union), so these two cases test the
	// WINDOW alone — stickiness has its own tests.
	mock.ExpectQuery("SELECT COALESCE\\(mcp_surface::text").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"mcp_surface"}).AddRow(""))
	// The corroboration read. Asserting the SQL shape here is the point: it
	// must hit activity_logs.tool_trace and NOTHING else (the agent_log
	// summaries are workspace-forgeable and deliberately not read).
	mock.ExpectQuery("SELECT COALESCE\\(tool_trace::text, ''\\)\\s+FROM activity_logs\\s+WHERE workspace_id = \\$1 AND tool_trace IS NOT NULL").
		WithArgs(wsID, int64(mcpDispatchLookbackRows)).
		WillReturnRows(sqlmock.NewRows([]string{"trace"}).
			AddRow(`[{"tool":"` + dispatchedID + `"}]`))
	var surface string
	mock.ExpectExec("UPDATE workspaces SET mcp_surface").
		WithArgs(capturedArg{seen: &surface}, wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs(wsID).
		WillReturnRows(evalStatusRows("provisioning", "platform", nil, nil))
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// THE DIRECTION GUARD. A blocker wired into evaluateStatus's predicate
	// leaves this expectation unmet.
	mock.ExpectExec("UPDATE workspaces SET status = .*mcp_unloaded_since = NULL").
		WithArgs(models.StatusOnline, wsID, "provisioning").
		WillReturnResult(sqlmock.NewResult(0, 1))
	var payload string
	mock.ExpectExec("INSERT INTO structure_events").
		WithArgs(string(events.EventWorkspaceOnline), wsID, onlineEventPayload{
			mustContain: []string{`"readiness_evidence":"` + string(wantEvidence) + `"`},
			seen:        &payload,
		}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"workspace_id":"` + wsID + `","error_rate":0.0,"sample_error":"","active_tasks":0,` +
		`"uptime_seconds":60,"mcp_server_present":true,"mcp_tools_ready":true,` +
		`"loaded_mcp_tools":["` + conciergePlatformMCPProvisionWorkspaceTool + `"]}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v\nmcp_surface written: %s\nWORKSPACE_ONLINE payload: %s", err, surface, payload)
	}
	return surface
}

// TestMCPSurface_NotYetExercised_StillReachesOnline is the direction guard that
// covers the PREDICATE, not the label (reviewer mutation M3b: wiring a blocker
// into evaluateStatus's real predicate was green against the pure-label test).
//
// This input is the ORDINARY fleet state — inventory advertises the management
// namespace, the model has only dispatched the a2a sidecar — so a blocker here
// would deny online to essentially every concierge. The row must promote, and
// the event must carry the UNCHANGED self-report label, because nothing
// negative was established.
func TestMCPSurface_NotYetExercised_StillReachesOnline(t *testing.T) {
	surface := mcpSurfaceHeartbeatCase(t, "ws-surface-notyet",
		"mcp__molecule__send_message_to_user", evidenceSelfReportMCPToolsReady)

	for _, want := range []string{
		`"verdict":"` + string(verdictNotYetExercised) + `"`,
		`"dispatch_corroborated_count":0`,
		`"reported_count":1`,
	} {
		if !strings.Contains(surface, want) {
			t.Errorf("persisted mcp_surface missing %s\ngot: %s", want, surface)
		}
	}
}

// TestMCPSurface_HermesUnderscoreSpelling_Corroborates is the proof that
// verdictCorroborated is REACHABLE on real fleet data.
//
// Every `loaded_mcp_tools: enumerated` event on this fleet comes from the hermes
// template, and hermes registers the sanitised spelling — captured verbatim:
//
//	tools.mcp_tool: MCP server 'molecule-platform' (stdio): registered 54
//	tool(s): mcp__molecule_platform__list_workspaces, …
//
// So the ONLY dispatch id a healthy hermes concierge can ever produce for the
// management server is the UNDERSCORE form, while the inventory reports the
// HYPHEN form. Before the canonicalisation in mcpToolIDNamespace this test's
// input produced "contradicted" — making verdictCorroborated unreachable
// fleet-wide and firing the loud CONTRADICTED log on every healthy concierge.
func TestMCPSurface_HermesUnderscoreSpelling_Corroborates(t *testing.T) {
	surface := mcpSurfaceHeartbeatCase(t, "ws-surface-hermes",
		"mcp__molecule_platform__provision_workspace", evidenceDispatchObservedMCPSurface)

	for _, want := range []string{
		`"verdict":"` + string(verdictCorroborated) + `"`,
		`"dispatch_corroborated_count":1`,
		`"advertised_only_count":0`,
	} {
		if !strings.Contains(surface, want) {
			t.Errorf("persisted mcp_surface missing %s\ngot: %s", want, surface)
		}
	}
}

// TestMCPSurface_SpellingsAreEquivalent pins the fold directly. The runtime repo
// wrote canonical_tool_id for exactly this and warned in writing that a raw
// comparison is "a CONSTANT FALSE on hermes"; this is that warning as an
// executable assertion.
func TestMCPSurface_SpellingsAreEquivalent(t *testing.T) {
	hyphen := mcpToolIDNamespace("mcp__molecule-platform__provision_workspace")
	under := mcpToolIDNamespace("mcp__molecule_platform__provision_workspace")
	if hyphen != under {
		t.Fatalf("mcp__molecule-platform__ folds to %q but mcp__molecule_platform__ folds to %q — "+
			"a raw comparison is a constant FALSE on hermes, which would report a divergence on every "+
			"healthy beat (molecule_runtime canonical_tool_id exists to prevent exactly this)", hyphen, under)
	}
	if hyphen != "molecule_platform" {
		t.Errorf("canonical namespace = %q, want %q (mirror of canonical_tool_id's [^A-Za-z0-9_] -> _)", hyphen, "molecule_platform")
	}

	// The fold must not smear DIFFERENT servers onto each other.
	if mcpToolIDNamespace("mcp__molecule__x") == mcpToolIDNamespace("mcp__molecule-platform__x") {
		t.Error("the a2a sidecar 'molecule' and the management server folded to the same namespace — the fold is over-broad")
	}
}

// ── Parsers ─────────────────────────────────────────────────────────────────

func TestMCPToolIDNamespace(t *testing.T) {
	cases := map[string]string{
		"mcp__molecule-platform__provision_workspace": "molecule_platform", // folded
		"mcp__molecule_platform__provision_workspace": "molecule_platform", // hermes spelling, same fold
		"mcp__molecule__send_message_to_user":         "molecule",
		"mcp__reno_work__mark_task_done":              "reno_work",
		// A tool name containing "__" must not shift the namespace.
		"mcp__molecule__weird__tool_name": "molecule",
		"  mcp__molecule__x  ":            "molecule",
		// Not dispatcher ids.
		"Bash":              "",
		"mcp__molecule":     "",
		"mcp__molecule__":   "",
		"mcp____tool":       "",
		"":                  "",
		"molecule__tool":    "",
		"xmcp__molecule__t": "",
	}
	for in, want := range cases {
		if got := mcpToolIDNamespace(in); got != want {
			t.Errorf("mcpToolIDNamespace(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every fixture below is a value a `jsonb` column can actually hold. An earlier
// draft used `{not json` and a raw `[]`, neither of which Postgres can return
// from this query (invalid JSON cannot be stored, and extractToolTrace drops
// empty arrays before the insert) — testing the parser against inputs the
// source cannot produce proves nothing about the source.
func TestMCPDispatchNamespacesFrom(t *testing.T) {
	traces := [][]byte{
		[]byte(`[{"tool":"mcp__molecule__send_message_to_user"},{"tool":"Bash"}]`),
		// bare-string element shape (older writers)
		[]byte(`["mcp__reno_work__list_due_tasks"]`),
		// hermes' sanitised spelling folds onto the declared one
		[]byte(`[{"tool":"mcp__molecule_platform__list_orgs"}]`),
		// jsonb-legal but carrying nothing usable: must be DROPPED, never
		// guessed into a namespace.
		//
		// DEFENSIVE ONLY — `null` cannot reach this parser from the column:
		// extractToolTrace rejects both "null" and "[]" before the insert, so
		// the sole writer never stores it. Kept because the parser must not
		// depend on that invariant holding forever, and marked so a reader does
		// not go hunting for the path that emits it.
		[]byte(`null`),
		[]byte(`{"tool":"mcp__ghost__tool"}`), // object, not the array shape
		[]byte(`[{"tool":123}]`),
		[]byte(`["Read"]`),
	}

	got, records := mcpDispatchNamespacesFrom(traces)
	want := []string{"molecule", "reno_work", "molecule_platform"}
	for _, ns := range want {
		if _, ok := got[ns]; !ok {
			t.Errorf("namespace %q missing from %v", ns, got)
		}
	}
	if _, ok := got["ghost"]; ok {
		t.Error("a non-array tool_trace document manufactured a namespace — corroboration must never be invented from a shape the writer does not produce")
	}
	if len(got) != 3 {
		t.Errorf("got %d namespaces (%v), want exactly 3", len(got), got)
	}
	if records != 3 {
		t.Errorf("dispatch_records = %d, want 3", records)
	}
}

// TestMCPDispatchNamespaces_IgnoresForgeableAgentLogSummaries pins the trust
// boundary as an executable assertion. POST /workspaces/:id/activity accepts
// activity_type:"agent_log" with an arbitrary summary from the authenticated
// workspace, so a workspace could post a tool-use-shaped line having dispatched
// nothing. Only tool_trace — whose sole writer is core's own extractToolTrace,
// and which that endpoint has no field for — may back a dispatch_observed:
// label. If a future edit re-adds the summary arm, this fails.
func TestMCPDispatchNamespaces_IgnoresForgeableAgentLogSummaries(t *testing.T) {
	// Built with json.Marshal so the marker is the REAL U+1F6E0 rune. An earlier
	// draft hand-wrote "\U0001F6E0" inside a Go RAW string literal: that is a
	// literal backslash-U, which is not a valid JSON escape, so json.Unmarshal
	// rejected the element and the test passed without ever reaching the parser
	// logic it claimed to cover. It survived a mutation that re-added the
	// marker-stripping arm — a vacuous pass. Marshalling makes the fixture the
	// string a forger would actually produce.
	forgedSummary := "\U0001F6E0 mcp__molecule_platform__list_orgs(limit=10)"
	forged, err := json.Marshal([]string{forgedSummary})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Guard the fixture itself: the marker must survive into the JSON, or this
	// test is again asserting nothing.
	if !strings.Contains(string(forged), "\U0001F6E0") {
		t.Fatalf("fixture lost the tool-use marker: %s", forged)
	}

	got, records := mcpDispatchNamespacesFrom([][]byte{forged})
	if len(got) != 0 || records != 0 {
		t.Errorf("a tool-use-shaped SUMMARY string produced namespaces %v (records=%d) — "+
			"only core-written tool_trace entries may corroborate; accepting a marker-prefixed "+
			"string would launder a workspace-postable self-report behind a dispatch_observed: label", got, records)
	}
}

// TestMCPSurface_ReadTouchesOnlyToolTrace pins the TRUST BOUNDARY at the SQL,
// which is where it is actually enforced. The corroboration read must issue
// exactly one query, against tool_trace, and must never reach
// activity_logs.summary — the column POST /workspaces/:id/activity lets a
// workspace write. Re-adding the agent_log UNION branch changes the query text
// and leaves this expectation unmatched.
func TestMCPSurface_ReadTouchesOnlyToolTrace(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	// The sticky half: the prior record. NULL here exercises the
	// first-evaluation path (empty union).
	mock.ExpectQuery("SELECT COALESCE\\(mcp_surface::text").
		WithArgs("ws-trust").
		WillReturnRows(sqlmock.NewRows([]string{"mcp_surface"}).AddRow(""))
	mock.ExpectQuery("^\\s*SELECT COALESCE\\(tool_trace::text, ''\\)\\s+FROM activity_logs\\s+WHERE workspace_id = \\$1 AND tool_trace IS NOT NULL\\s+ORDER BY created_at DESC\\s+LIMIT \\$2\\s*$").
		WithArgs("ws-trust", int64(mcpDispatchLookbackRows)).
		WillReturnRows(sqlmock.NewRows([]string{"trace"}).
			AddRow(`[{"tool":"mcp__molecule_platform__list_orgs"}]`))
	var surface string
	mock.ExpectExec("UPDATE workspaces SET mcp_surface").
		WithArgs(capturedArg{seen: &surface}, "ws-trust").
		WillReturnResult(sqlmock.NewResult(0, 1))

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	handler.recordMCPSurfaceCorroboration(c, c, "ws-trust",
		[]string{conciergePlatformMCPProvisionWorkspaceTool})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the corroboration read must be exactly one tool_trace query and nothing else "+
			"(reading activity_logs.summary would admit workspace-posted rows): %v", err)
	}
	if !strings.Contains(surface, string(verdictCorroborated)) {
		t.Errorf("expected %q in the persisted document, got: %s", verdictCorroborated, surface)
	}
	// The verdict must also be published to the request for evaluateStatus.
	if got := mcpSurfaceVerdictFromContext(c); got != verdictCorroborated {
		t.Errorf("request-context verdict = %q, want %q", got, verdictCorroborated)
	}
}

// TestMCPSurface_ReadFailure_LeavesNoVerdict covers the best-effort contract:
// a corroboration read that errors must NOT write the column, must NOT set a
// verdict, and must not panic — a DB blip must read as "unknown", never as
// "the evidence went away".
func TestMCPSurface_ReadFailure_LeavesNoVerdict(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(mcp_surface::text").
		WithArgs("ws-readfail").
		WillReturnRows(sqlmock.NewRows([]string{"mcp_surface"}).AddRow(""))
	mock.ExpectQuery("SELECT COALESCE\\(tool_trace::text").
		WithArgs("ws-readfail", int64(mcpDispatchLookbackRows)).
		WillReturnError(errors.New("connection reset"))

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	handler.recordMCPSurfaceCorroboration(c, c, "ws-readfail",
		[]string{conciergePlatformMCPProvisionWorkspaceTool})

	if got := mcpSurfaceVerdictFromContext(c); got != "" {
		t.Errorf("a failed read produced the verdict %q — it must fail to the neutral value", got)
	}
	// No mcp_surface UPDATE was expected; if one fired, ExpectationsWereMet
	// reports the unexpected call.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a failed corroboration read must leave the previous record untouched: %v", err)
	}
}

// TestMCPSurface_EmptyInventory_SkipsTheRead pins the short-circuit: an empty
// reported list must persist the unknown:no_inventory_reported verdict WITHOUT
// issuing the dispatch query (its answer cannot change the outcome). Only the
// UPDATE is expected, so an unexpected SELECT fails the test.
func TestMCPSurface_EmptyInventory_SkipsTheRead(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	var surface string
	mock.ExpectExec("UPDATE workspaces SET mcp_surface").
		WithArgs(capturedArg{seen: &surface}, "ws-empty").
		WillReturnResult(sqlmock.NewResult(0, 1))

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	handler.recordMCPSurfaceCorroboration(c, c, "ws-empty", []string{})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("empty inventory must persist a verdict without reading dispatches: %v", err)
	}
	if !strings.Contains(surface, string(verdictNoInventory)) {
		t.Errorf("expected %q, got: %s", verdictNoInventory, surface)
	}
	if got := mcpSurfaceVerdictFromContext(c); got != verdictNoInventory {
		t.Errorf("request-context verdict = %q, want %q", got, verdictNoInventory)
	}
}

// ── Fail-neutral reader ─────────────────────────────────────────────────────

func TestMCPSurfaceVerdictFromContext_FailsNeutral(t *testing.T) {
	if got := mcpSurfaceVerdictFromContext(nil); got != "" {
		t.Errorf("nil context yielded %q, want the neutral value", got)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if got := mcpSurfaceVerdictFromContext(c); got != "" {
		t.Errorf("absent key yielded %q, want the neutral value", got)
	}
	c.Set(mcpSurfaceContextKey, "some_verdict_this_build_does_not_know")
	if got := mcpSurfaceVerdictFromContext(c); got != "" {
		t.Errorf("wrong-typed value yielded %q, want the neutral value", got)
	}
	c.Set(mcpSurfaceContextKey, mcpSurfaceVerdict("dispatch_observed:invented"))
	if got := mcpSurfaceVerdictFromContext(c); got != "" {
		t.Errorf("unknown verdict yielded %q — an unrecognised string must not be honoured as evidence", got)
	}
	c.Set(mcpSurfaceContextKey, verdictNotYetExercised)
	if got := mcpSurfaceVerdictFromContext(c); got != verdictNotYetExercised {
		t.Errorf("known verdict yielded %q, want %q", got, verdictNotYetExercised)
	}
}

// TestMCPSurfaceReport_JSONShapeIsStable pins the wire shape callers read off
// GET /workspaces/:id, including the field a caller should read INSTEAD of the
// raw count.
func TestMCPSurfaceReport_JSONShapeIsStable(t *testing.T) {
	disp, n := dispatchSet(capturedDispatchedToolIDs...)
	raw, err := json.Marshal(classifyMCPSurface(capturedReportedInventory, disp, n, nil, time.Now()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{
		`"reported_count":54`,
		`"dispatch_corroborated_count":0`,
		`"advertised_only_count":54`,
		`"verdict":"` + string(verdictNotYetExercised) + `"`,
		`"corroborated_namespaces":["molecule"]`,
		`"reported_namespaces":["molecule_platform"]`,
		`"dispatched_namespaces":["molecule"]`,
	} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("mcp_surface JSON missing %s\ngot: %s", field, raw)
		}
	}
}

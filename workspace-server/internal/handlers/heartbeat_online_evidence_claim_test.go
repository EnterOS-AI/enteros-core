package handlers

import (
	"bytes"
	"database/sql/driver"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// HONEST-CLAIM suite for the platform concierge's provisioning→online flip.
//
// The flip is authorised by RUNTIME SELF-REPORTS only (mcp_tools_ready, i.e. a
// tools/list success; or membership of the required verb in the runtime's own
// loaded_mcp_tools). Neither is a tools/call, so `online` cannot mean the verb
// is CALLABLE — and `online` is load-bearing: it lifts the a2a_proxy warming
// 503, enables the canvas composer, and (via platformAgentHealthy) SUPPRESSES
// the concierge repair/re-provision path.
//
// These tests do NOT claim to gate callability — no run-time callability signal
// exists at the flip (see the HONEST-CLAIM CONTRACT in registry.go). What they
// pin is that the flip STOPS OVER-CLAIMING: the WORKSPACE_ONLINE event names
// WHICH self-report authorised it and never re-asserts the old unqualified
// `verified_ready: true`.
//
// Both directions are real and discriminating:
//   - a workspace whose ONLY evidence is the tools/list probe must be labelled
//     the tools/list self-report — and must NOT carry a verification claim;
//   - a workspace whose ONLY evidence is the loaded_mcp_tools list must be
//     labelled the (weaker, distinct) list self-report;
//   - a legacy runtime, promoted on liveness with NO evidence examined, must be
//     labelled as such rather than sharing either self-report label.
//
// Collapsing any two labels, emitting a constant, or restoring `verified_ready`
// turns these red.

// onlineEventPayload is a sqlmock argument matcher over the JSON payload the
// broadcaster passes as $3 of `INSERT INTO structure_events`. Substring
// matching is deliberate: the assertion is about what the event CLAIMS, and a
// full-JSON equality would also pin unrelated fields (recovered_from) and make
// the test brittle to additions rather than to claim changes.
type onlineEventPayload struct {
	mustContain    []string
	mustNotContain []string
	// seen captures the last payload the matcher inspected so a failure can
	// report the actual claim instead of only "expectation unmet".
	seen *string
}

func (m onlineEventPayload) Match(v driver.Value) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	if m.seen != nil {
		*m.seen = s
	}
	for _, want := range m.mustContain {
		if !strings.Contains(s, want) {
			return false
		}
	}
	for _, bad := range m.mustNotContain {
		if strings.Contains(s, bad) {
			return false
		}
	}
	return true
}

// TestOnlineEvidence_LabelsAreStrengthPrefixedAndDistinct pins the naming
// contract of the evidence vocabulary itself. Every value must declare its own
// strength ("self_report:" = the runtime said so about itself, "none:" =
// nothing was examined), and no value may read as proof. A mutant that renames
// a label to something claiming verification/callability, or that collapses two
// labels onto the same string, fails here.
func TestOnlineEvidence_LabelsAreStrengthPrefixedAndDistinct(t *testing.T) {
	all := []conciergeReadinessEvidence{
		evidenceSelfReportMCPToolsReady,
		evidenceSelfReportLoadedMCPTools,
		evidenceNoneLegacyRuntime,
		// core#5137 added the first non-self-reported strength to this
		// vocabulary. It is admitted here on the SAME terms as the others: it
		// must carry its strength in the string and must not read as proof.
		evidenceDispatchObservedMCPSurface,
		evidenceSelfReportContradicted,
	}

	// "dispatch_observed:" = core's own turn record shows the model dispatched
	// from this namespace. Strictly stronger than a self-report (it cannot exist
	// without a real invocation) and strictly weaker than a platform-executed
	// call — which is exactly why it is a THIRD prefix rather than a reuse of
	// "self_report:" or a bare "callable".
	allowedPrefixes := []string{"self_report:", "none:", "dispatch_observed:"}

	seen := map[conciergeReadinessEvidence]bool{}
	for _, e := range all {
		s := string(e)
		if s == "" {
			t.Errorf("evidence label is empty — an unlabelled promotion is the over-claim this suite exists to stop")
			continue
		}
		prefixed := false
		for _, p := range allowedPrefixes {
			if strings.HasPrefix(s, p) {
				prefixed = true
				break
			}
		}
		if !prefixed {
			t.Errorf("evidence label %q must declare its strength with one of %v; an unprefixed label reads as proof", s, allowedPrefixes)
		}
		// The flip has NO callability evidence. A label implying otherwise would
		// re-introduce the over-claim in new words.
		for _, forbidden := range []string{"verified", "callable", "proven", "confirmed"} {
			if strings.Contains(strings.ToLower(s), forbidden) {
				t.Errorf("evidence label %q contains %q — the online flip verifies no such thing", s, forbidden)
			}
		}
		if seen[e] {
			t.Errorf("evidence label %q is duplicated — the arms must stay distinguishable", s)
		}
		seen[e] = true
	}
}

// TestOnlineEvidence_NamesTheStrongestSelfReport is the pure-function proof that
// the label is DERIVED from the actual inputs, not stamped. mcp_tools_ready
// (turn-independent prober) outranks the under-emitting loaded_mcp_tools list,
// and neither-input yields "" — the verified arm is guarded by
// mcpSurfaceSelfReported so "" can never reach the wire, and an empty label
// appearing there would itself be the bug signal.
func TestOnlineEvidence_NamesTheStrongestSelfReport(t *testing.T) {
	cases := []struct {
		name                string
		mcpToolsReady       bool
		provisionToolLoaded bool
		surface             mcpSurfaceVerdict
		want                conciergeReadinessEvidence
	}{
		{"tools_ready only", true, false, "", evidenceSelfReportMCPToolsReady},
		{"loaded list only", false, true, "", evidenceSelfReportLoadedMCPTools},
		{"both — prober wins", true, true, "", evidenceSelfReportMCPToolsReady},
		{"neither — no label", false, false, "", ""},

		// core#5137. The consumer-derived verdict only ever RE-LABELS a
		// promotion the self-report already authorised; it never authorises one
		// on its own (the "neither" rows below stay empty whatever the verdict).
		{"corroborated upgrades the label", true, false, verdictCorroborated, evidenceDispatchObservedMCPSurface},
		{"corroborated upgrades the back-compat arm too", false, true, verdictCorroborated, evidenceDispatchObservedMCPSurface},
		{"contradicted downgrades the label", true, false, verdictContradicted, evidenceSelfReportContradicted},
		{"unknown:no_dispatch_record changes nothing", true, false, verdictNoDispatchRecord, evidenceSelfReportMCPToolsReady},
		{"unknown:no_inventory changes nothing", false, true, verdictNoInventory, evidenceSelfReportLoadedMCPTools},
		{"corroborated cannot mint a label from nothing", false, false, verdictCorroborated, ""},
		{"contradicted cannot mint a label from nothing", false, false, verdictContradicted, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := conciergeOnlineEvidence(tc.mcpToolsReady, tc.provisionToolLoaded, tc.surface)
			if got != tc.want {
				t.Errorf("conciergeOnlineEvidence(%v, %v, %q) = %q, want %q",
					tc.mcpToolsReady, tc.provisionToolLoaded, tc.surface, got, tc.want)
			}
		})
	}
}

// TestOnlineEvidence_ToolsListProbeArm_DoesNotClaimVerified is the headline
// direction: a concierge whose ONLY evidence is the runtime's own tools/list
// probe still reaches online (we are not gating it — no signal exists to gate
// on), but the event it emits says exactly that and no more. The old payload
// `{"verified_ready": true}` is pinned as forbidden.
func TestOnlineEvidence_ToolsListProbeArm_DoesNotClaimVerified(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-evidence-probe").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status", "desired_generation"}).AddRow("", 0, "provisioning", int64(0)))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-evidence-probe", 0.0, "", 0, 60, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-evidence-probe").
		WillReturnRows(evalStatusRows("provisioning", "platform", nil, nil))
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-evidence-probe").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec("UPDATE workspaces SET status = .*mcp_unloaded_since = NULL").
		WithArgs(models.StatusOnline, "ws-evidence-probe", "provisioning").
		WillReturnResult(sqlmock.NewResult(0, 1))

	var payload string
	mock.ExpectExec("INSERT INTO structure_events").
		WithArgs(string(events.EventWorkspaceOnline), "ws-evidence-probe", onlineEventPayload{
			mustContain: []string{
				`"readiness_evidence":"` + string(evidenceSelfReportMCPToolsReady) + `"`,
			},
			mustNotContain: []string{"verified_ready", string(evidenceSelfReportLoadedMCPTools), string(evidenceNoneLegacyRuntime)},
			seen:           &payload,
		}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// mcp_tools_ready=true and NOTHING else: no loaded_mcp_tools list at all.
	body := `{"workspace_id":"ws-evidence-probe","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"mcp_tools_ready":true}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (tools/list-probe arm must emit the self_report:mcp_tools_ready label and no verification claim): %v\nactual WORKSPACE_ONLINE payload: %s", err, payload)
	}
}

// TestOnlineEvidence_LoadedToolsListArm_LabelsTheWeakerSelfReport is the second
// direction: the SAME promotion, reached by the OTHER arm (no mcp_tools_ready;
// the required verb present in the runtime's self-composed loaded_mcp_tools),
// must carry the DISTINCT, weaker label. This is what stops the fix degenerating
// into a constant string: a mutant that hardcodes either label fails one of
// these two tests.
func TestOnlineEvidence_LoadedToolsListArm_LabelsTheWeakerSelfReport(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-evidence-list").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status", "desired_generation"}).AddRow("", 0, "provisioning", int64(0)))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-evidence-list", 0.0, "", 0, 60, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The list IS reported, so it is persisted to the row.
	mock.ExpectExec("UPDATE workspaces SET loaded_mcp_tools").
		WithArgs(sqlmock.AnyArg(), "ws-evidence-list").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-evidence-list").
		WillReturnRows(evalStatusRows("provisioning", "platform", nil, nil))
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-evidence-list").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec("UPDATE workspaces SET status = .*mcp_unloaded_since = NULL").
		WithArgs(models.StatusOnline, "ws-evidence-list", "provisioning").
		WillReturnResult(sqlmock.NewResult(0, 1))

	var payload string
	mock.ExpectExec("INSERT INTO structure_events").
		WithArgs(string(events.EventWorkspaceOnline), "ws-evidence-list", onlineEventPayload{
			mustContain: []string{
				`"readiness_evidence":"` + string(evidenceSelfReportLoadedMCPTools) + `"`,
			},
			mustNotContain: []string{"verified_ready", string(evidenceSelfReportMCPToolsReady), string(evidenceNoneLegacyRuntime)},
			seen:           &payload,
		}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// No mcp_tools_ready field at all — only the self-composed tool list.
	body := `{"workspace_id":"ws-evidence-list","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60,"mcp_server_present":true,"loaded_mcp_tools":["a2a","` + conciergePlatformMCPProvisionWorkspaceTool + `"]}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (loaded_mcp_tools arm must emit the DISTINCT self_report:loaded_mcp_tools label): %v\nactual WORKSPACE_ONLINE payload: %s", err, payload)
	}
}

// TestOnlineEvidence_LegacyArm_LabelsTheAbsenceOfEvidence covers the weakest
// promotion of all: a pre-#147 runtime that cannot speak the readiness contract
// is promoted on LIVENESS ALONE. Previously it broadcast a payload with no
// readiness field whatsoever, which is indistinguishable from "the field was
// dropped". Labelling it keeps "no evidence was taken" on the wire.
func TestOnlineEvidence_LegacyArm_LabelsTheAbsenceOfEvidence(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery("SELECT COALESCE\\(current_task").
		WithArgs("ws-evidence-legacy").
		WillReturnRows(sqlmock.NewRows([]string{"current_task", "monthly_spend", "status", "desired_generation"}).AddRow("", 0, "provisioning", int64(0)))
	mock.ExpectExec("UPDATE workspaces SET").
		WithArgs("ws-evidence-legacy", 0.0, "", 0, 60, "", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT status, kind, last_register_failure_at, mcp_unloaded_since FROM workspaces WHERE id =").
		WithArgs("ws-evidence-legacy").
		WillReturnRows(evalStatusRows("provisioning", "platform", nil, nil))
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("ws-evidence-legacy").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// Legacy flip: no mcp_unloaded_since clause.
	mock.ExpectExec("UPDATE workspaces SET status = \\$1::workspace_status, updated_at = now").
		WithArgs(models.StatusOnline, "ws-evidence-legacy", "provisioning").
		WillReturnResult(sqlmock.NewResult(0, 1))

	var payload string
	mock.ExpectExec("INSERT INTO structure_events").
		WithArgs(string(events.EventWorkspaceOnline), "ws-evidence-legacy", onlineEventPayload{
			mustContain: []string{
				`"readiness_evidence":"` + string(evidenceNoneLegacyRuntime) + `"`,
			},
			mustNotContain: []string{"verified_ready", "self_report:"},
			seen:           &payload,
		}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// Pre-#147: no mcp_server_present, no mcp_tools_ready, no loaded_mcp_tools.
	body := `{"workspace_id":"ws-evidence-legacy","error_rate":0.0,"sample_error":"","active_tasks":0,"uptime_seconds":60}`
	c.Request = httptest.NewRequest("POST", "/registry/heartbeat", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Heartbeat(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (legacy arm must label the ABSENCE of evidence, not share a self_report label): %v\nactual WORKSPACE_ONLINE payload: %s", err, payload)
	}
}

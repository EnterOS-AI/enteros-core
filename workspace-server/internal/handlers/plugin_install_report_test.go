package handlers

// The property under test is NOT "does a JSON body decode" — it is "can core
// answer whether a workspace's plugins actually went live". Those came apart in
// production: on 2026-07-30 every kind=workspace workspace on both prod tenants had
// zero plugins loaded and core had no way to say so, because the boot-install
// report went to a stdout nobody can read and the telemetry that would have carried
// it is concierge-gated + broadcast-only (#4953).
//
// So the tests here concentrate on the three ways this endpoint could look like it
// works and not:
//
//  1. the wire field names silently diverging from the SDK contract the runtime
//     generates its producer from
//  2. `absent` being recorded as `false` — which turns "core never asked for a
//     plugin" into "core asked and nothing went live", a different bug in a
//     different repo
//  3. liveness being computed from `installed` instead of the contract's rule,
//     which reports a staged-but-never-promoted build as a success
//
// Each has a paired negative control, so a failure says which direction broke.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.moleculesai.app/sdk/gen/go/molcontracts"
)

// --- 1. the wire shape must equal the SDK contract -------------------------
//
// The handler comment claims "the struct tags MUST equal those constants and a
// test asserts it". This is that test — a claim in a comment that nothing checks
// is how the two halves of a contract drift apart while both look right.

func TestPluginInstallReport_StructTagsMatchTheContract(t *testing.T) {
	want := map[string]string{
		"Declared":   molcontracts.PluginInstallReportFieldDeclared,
		"PluginsDir": molcontracts.PluginInstallReportFieldPluginsDir,
		"Installed":  molcontracts.PluginInstallReportFieldInstalled,
		"Skipped":    molcontracts.PluginInstallReportFieldSkipped,
		"Failed":     molcontracts.PluginInstallReportFieldFailed,
		"Swapped":    molcontracts.PluginInstallReportFieldSwapped,
	}
	rt := reflect.TypeOf(pluginInstallReportBody{})
	if rt.NumField() != len(want) {
		t.Fatalf("body has %d fields, contract has %d — a field was added or dropped on one side only",
			rt.NumField(), len(want))
	}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		expected, known := want[f.Name]
		if !known {
			t.Errorf("field %s is not in the contract", f.Name)
			continue
		}
		if tag != expected {
			t.Errorf("field %s serialises as %q, contract says %q", f.Name, tag, expected)
		}
	}
}

// The generated binding must list exactly the fields the body carries, in case a
// contract gains a field that core then silently ignores.
func TestPluginInstallReport_ContractFieldNamesAreAllConsumed(t *testing.T) {
	rt := reflect.TypeOf(pluginInstallReportBody{})
	have := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		have[strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	for _, name := range molcontracts.PluginInstallReportFieldNames {
		if !have[name] {
			t.Errorf("contract field %q has no home in pluginInstallReportBody — a report field core throws away", name)
		}
	}
}

// --- 2. liveness is the contract rule, not a count -------------------------

func TestReportIsLive_FollowsTheContractRule(t *testing.T) {
	cases := []struct {
		name     string
		declared bool
		swapped  bool
		failed   []string
		want     bool
	}{
		{"declared+swapped+no failures is live", true, true, nil, true},
		{"staged but never swapped is NOT live", true, false, nil, false},
		{"swapped with a failure is NOT live", true, true, []string{"gitea://o/r"}, false},
		{"nothing declared is NOT live", false, true, nil, false},
		{"empty failed slice counts as no failures", true, true, []string{}, true},
	}
	for _, tc := range cases {
		if got := reportIsLive(tc.declared, tc.swapped, tc.failed); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// THE case that motivated the rule. A build can stage every source and promote
// none of them — a partial build is never swapped in. Counting `installed` reports
// that as a success, which is exactly how the production symptom stayed invisible.
func TestReportIsLive_StagedEverythingPromotedNothingIsNotLive(t *testing.T) {
	installed := []string{"a", "b", "c", "d", "e", "f"}
	if reportIsLive(true, false, nil) {
		t.Fatal("swapped=false must never be live")
	}
	// The negative control for the mistake itself: the naive predicate says yes.
	if !(len(installed) > 0) {
		t.Fatal("control is not exercising the hazard")
	}
}

// --- 3. absent is not false ------------------------------------------------

func postReport(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPluginInstallReportHandler()
	r.POST("/workspaces/:id/plugin-install-report", h.Report)
	req := httptest.NewRequest(http.MethodPost,
		"/workspaces/6ac59acb-a79d-4686-8669-c5a2c077d69d/plugin-install-report",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestPluginInstallReport_MissingDeclaredIsRefused(t *testing.T) {
	w := postReport(t, `{"swapped":true,"installed":[],"skipped":[],"failed":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing `declared` must be refused, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "absent is not the same as false") {
		t.Errorf("the refusal must say WHY absent differs from false, got %s", w.Body.String())
	}
}

func TestPluginInstallReport_MissingSwappedIsRefused(t *testing.T) {
	w := postReport(t, `{"declared":true,"installed":[],"skipped":[],"failed":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing `swapped` must be refused, got %d: %s", w.Code, w.Body.String())
	}
}

// The positive arm: a complete report is accepted with the contract's status. db.DB
// is nil under unit test so persist is a no-op — the assertion is the contract
// surface (accepted, correct status), not the row.
func TestPluginInstallReport_CompleteReportIsAcceptedWithContractStatus(t *testing.T) {
	w := postReport(t, `{"declared":true,"swapped":true,"plugins_dir":"/configs/plugins","installed":["gitea://o/r#v1"],"skipped":[],"failed":[]}`)
	if w.Code != molcontracts.PluginInstallReportSuccessStatus {
		t.Fatalf("want %d, got %d: %s", molcontracts.PluginInstallReportSuccessStatus, w.Code, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Errorf("a 204 must carry no body, got %q", w.Body.String())
	}
}

func TestPluginInstallReport_NonUUIDIsRefused(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPluginInstallReportHandler()
	r.POST("/workspaces/:id/plugin-install-report", h.Report)
	req := httptest.NewRequest(http.MethodPost, "/workspaces/not-a-uuid/plugin-install-report",
		strings.NewReader(`{"declared":true,"swapped":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("non-UUID :id must be refused at the trust boundary, got %d", w.Code)
	}
}

// --- bounds ----------------------------------------------------------------

func TestPluginInstallReport_OversizedListIsRefusedNotTruncated(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"declared":true,"swapped":true,"installed":[`)
	for i := 0; i < maxInstallReportSources+1; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`"s"`)
	}
	sb.WriteString(`]}`)
	w := postReport(t, sb.String())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an oversized list must be REFUSED (a truncated report is a lying report), got %d", w.Code)
	}
}

func TestPluginInstallReport_OverLongSourceIsRefused(t *testing.T) {
	long := strings.Repeat("x", maxInstallReportSourceLen+1)
	w := postReport(t, `{"declared":true,"swapped":true,"failed":["`+long+`"]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an over-long source must be refused, got %d", w.Code)
	}
}

// --- nil-slice normalisation ----------------------------------------------
//
// A reader must never have to tell "reported no failures" apart from "reported
// nothing about failures". The producer always sends all three lists, and the
// stored jsonb is `[]` not `null`.

func TestNonNilStrings_NormalisesNilToEmpty(t *testing.T) {
	got := nonNilStrings(nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("nil must normalise to an empty slice, got %#v", got)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[]" {
		t.Errorf("must serialise as [], got %s", b)
	}
	// Non-nil passes through unchanged.
	in := []string{"a"}
	if out := nonNilStrings(in); !reflect.DeepEqual(in, out) {
		t.Errorf("non-nil must pass through, got %#v", out)
	}
}

// --- the contract's two invariants are what core relies on ----------------

func TestPluginInstallReport_ContractInvariantsHold(t *testing.T) {
	if molcontracts.PluginInstallReportConciergeGated {
		t.Error("core's whole reason for this endpoint is that reporting is NOT gated on kind")
	}
	if !molcontracts.PluginInstallReportDurable {
		t.Error("core persists this report; a non-durable contract would contradict the handler")
	}
	if !strings.Contains(molcontracts.PluginInstallReportOutcomeRule, "swapped") {
		t.Errorf("the outcome rule must consult swapped, got %q", molcontracts.PluginInstallReportOutcomeRule)
	}
}

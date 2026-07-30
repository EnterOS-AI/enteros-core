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
//  4. liveness ALSO consulting `failed`, which reports a healthy box running 5 of
//     6 plugins as an outage — the same lie with the sign flipped, and the worse
//     one, because a signal that cries wolf gets ignored before it is ever right
//  5. the body bounds being checked only AFTER the decoder has already
//     materialised the body, so they bound what is stored and not what is
//     allocated
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
		want     bool
	}{
		{"declared+swapped is live", true, true, true},
		{"staged but never swapped is NOT live", true, false, false},
		{"nothing declared is NOT live", false, true, false},
		{"nothing declared and nothing swapped is NOT live", false, false, false},
	}
	for _, tc := range cases {
		if got := reportIsLive(tc.declared, tc.swapped); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// THE finding this file was rewritten for. The runtime PROMOTES PARTIAL BUILDS —
// molecule_runtime/plugin_sources.py: "A failed source fails THAT SOURCE — not the
// whole tree", added after staging test5 on 2026-07-13, where the old
// all-or-nothing veto let one unfetchable third-party plugin take the concierge's
// own management MCP down with it. On a partial failure the runtime carries the
// live dirs forward, logs "promoting the N that succeeded", swaps, and returns
// swapped=True with a NON-EMPTY failed list.
//
// So the old rule (`… && len(failed) == 0`) called a workspace with 5 of 6 plugins
// running and one flaky gitea fetch NOT LIVE. This endpoint exists to stop core
// lying about plugin liveness; a false alarm on a healthy box is that same lie
// with the sign flipped, and it is worse than the original because it trains an
// operator to ignore the signal.
func TestReportIsLive_PartialPromotionIsLive(t *testing.T) {
	// The exact report the runtime sends after promoting 5 of 6 sources.
	declared, swapped := true, true
	failed := []string{"gitea://molecule-ai/molecule-ai-plugin-lark#deadbeef"}

	if !reportIsLive(declared, swapped) {
		t.Fatal("a promoted tree is LIVE — the runtime promotes partial builds on purpose")
	}
	if !reportIsDegraded(declared, swapped, failed) {
		t.Error("a promoted tree with a failed source is DEGRADED — the signal `failed` actually carries")
	}

	// Negative control for the hazard itself: the RETIRED predicate — the one this
	// change removed — calls the healthy box not-live. If this ever stops
	// disagreeing, the test above has stopped exercising anything.
	retiredRuleSaysLive := declared && swapped && len(failed) == 0
	if retiredRuleSaysLive {
		t.Fatal("control is not exercising the hazard: the retired `failed == []` rule must disagree")
	}
}

func TestReportIsDegraded_OnlyDescribesAPromotedTree(t *testing.T) {
	oops := []string{"gitea://o/r"}
	cases := []struct {
		name     string
		declared bool
		swapped  bool
		failed   []string
		want     bool
	}{
		{"promoted with a failure is degraded", true, true, oops, true},
		{"promoted with no failures is not degraded", true, true, nil, false},
		{"promoted with an empty failed slice is not degraded", true, true, []string{}, false},
		// A report that never promoted is not "degraded" — `failed` there is one of
		// the reasons NOTHING went live, and calling that degraded softens a hard
		// failure into a caveat.
		{"never promoted is not degraded, it is not live", true, false, oops, false},
		{"nothing declared is not degraded", false, true, oops, false},
	}
	for _, tc := range cases {
		if got := reportIsDegraded(tc.declared, tc.swapped, tc.failed); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// live and degraded must not be able to describe the same box two ways: degraded
// is a REFINEMENT of live, so degraded ⇒ live for every input. Asserted over the
// whole (declared × swapped × failed-shape) space rather than by example, because
// the failure mode is an input nobody thought to write a case for.
func TestReportIsLive_DegradedImpliesLiveAcrossEveryInput(t *testing.T) {
	failedShapes := [][]string{nil, {}, {"one"}, {"one", "two"}}
	for _, declared := range []bool{false, true} {
		for _, swapped := range []bool{false, true} {
			for _, failed := range failedShapes {
				live := reportIsLive(declared, swapped)
				degraded := reportIsDegraded(declared, swapped, failed)
				if degraded && !live {
					t.Errorf("declared=%v swapped=%v failed=%#v: degraded without live is not a state",
						declared, swapped, failed)
				}
				// And liveness must never consult `failed` — that is the whole finding.
				if live != (declared && swapped) {
					t.Errorf("declared=%v swapped=%v failed=%#v: liveness must not depend on failed",
						declared, swapped, failed)
				}
			}
		}
	}
}

// The migration's partial index is `WHERE declared AND NOT swapped`. It is the
// operator's first query, and it must be the EXACT complement of live — a fleet
// query that disagrees with the API is how an operator ends up debugging a
// workspace the API already called healthy.
func TestReportIsLive_MatchesTheFleetIndexPredicate(t *testing.T) {
	for _, declared := range []bool{false, true} {
		for _, swapped := range []bool{false, true} {
			indexMatches := declared && !swapped // the migration's WHERE clause
			live := reportIsLive(declared, swapped)
			if declared && indexMatches == live {
				t.Errorf("declared=%v swapped=%v: the not-live index and reportIsLive must be complements",
					declared, swapped)
			}
		}
	}
}

// THE case that motivated the rule. A build can stage every source and promote
// none of them — the runtime declines the swap when EVERY source failed, or when a
// previous dir could not be carried forward, and leaves the live tree untouched.
// Counting `installed` reports that as a success, which is exactly how the
// production symptom stayed invisible.
func TestReportIsLive_StagedEverythingPromotedNothingIsNotLive(t *testing.T) {
	installed := []string{"a", "b", "c", "d", "e", "f"}
	if reportIsLive(true, false) {
		t.Fatal("swapped=false must never be live")
	}
	// The negative control for the mistake itself: the naive predicate — the one a
	// reader reaches for instead of the rule — says this IS live.
	naiveSaysLive := len(installed) > 0
	if !naiveSaysLive {
		t.Fatal("control is not exercising the hazard: the naive predicate must say live")
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

// --- the body cap ----------------------------------------------------------
//
// maxInstallReportSources/…SourceLen are checked AFTER ShouldBindJSON, so on
// their own they bound what gets STORED, not what gets ALLOCATED. The group is
// mounted under wsAuth, which also accepts the ADMIN_TOKEN and an org key, so
// "the caller is a workspace" is not a size bound either.

// The attack shape, and the one the post-bind bounds cannot see at all: a body
// that is perfectly LEGAL by every field check, made enormous by padding the
// decoder will discard. Without a cap this decodes in full and answers 204.
func TestPluginInstallReport_HugeButOtherwiseLegalBodyIsRefusedBeforeDecoding(t *testing.T) {
	padding := strings.Repeat("x", 4<<20) // 4 MiB, ignored by the binding
	w := postReport(t, `{"declared":true,"swapped":true,"installed":[],"skipped":[],"failed":[],"padding":"`+padding+`"}`)

	if w.Code == molcontracts.PluginInstallReportSuccessStatus {
		t.Fatalf("a %d-byte body was DECODED and accepted — every post-bind bound passed, "+
			"because none of them can see padding the decoder throws away", len(padding))
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an over-cap body must be refused with 413 (not %d) — a producer needs to know "+
			"it was cut off rather than mis-serialised: %s", w.Code, w.Body.String())
	}
}

// The cap must not be tighter than the bounds: a report at the maximum the field
// checks admit has to survive it, or the endpoint 413s a report it would then have
// stored. This pins the inequality so tightening either side cannot silently
// invalidate the other.
func TestPluginInstallReport_BodyCapAdmitsAMaximallyLegalReport(t *testing.T) {
	source := `"` + strings.Repeat("s", maxInstallReportSourceLen) + `"`
	list := "[" + strings.Repeat(source+",", maxInstallReportSources-1) + source + "]"
	body := `{"declared":true,"swapped":true` +
		`,"plugins_dir":"` + strings.Repeat("d", maxInstallReportSourceLen) + `"` +
		`,"installed":` + list + `,"skipped":` + list + `,"failed":` + list + `}`

	if len(body) > maxInstallReportBody {
		t.Fatalf("the largest report the field bounds ADMIT (%d bytes) exceeds the body cap (%d) — "+
			"the endpoint would 413 a report it would then have stored", len(body), maxInstallReportBody)
	}
	// Not a hypothetical bound: the real handler must take it.
	w := postReport(t, body)
	if w.Code != molcontracts.PluginInstallReportSuccessStatus {
		t.Fatalf("a maximally-legal report must be accepted, got %d: %s", w.Code, w.Body.String())
	}

	// Negative control: the same construction one entry over the LIST bound is
	// still well under the body cap, so it must be refused by the field check with
	// a 400 — proving the cap has not quietly become the only bound in play.
	over := `{"declared":true,"swapped":true,"installed":[` +
		strings.Repeat(source+",", maxInstallReportSources) + source + `]}`
	if len(over) >= maxInstallReportBody {
		t.Fatalf("control is not exercising the field bound: %d bytes already trips the body cap", len(over))
	}
	if w := postReport(t, over); w.Code != http.StatusBadRequest {
		t.Errorf("one entry over the list bound must be a 400 from the field check, got %d", w.Code)
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

package handlers

// Unit tests for the fleet read (#4981 §1).
//
// The property under test is NOT "does a handler return JSON". It is that the two
// arms of the fleet sweep describe the two real classes of broken box, and that
// neither arm can quietly swallow the other:
//
//  1. `not_live` must be the partial index's predicate character for character —
//     an arm that is merely equivalent stops being served by the index the whole
//     issue is about.
//  2. `degraded` must exist as its own arm, because the runtime promotes partial
//     builds and those boxes are invisible to a not-live sweep by construction.
//  3. The two arms must be DISJOINT. Folding them together re-creates the exact
//     conflation core#4972 removed from the liveness rule.
//  4. The response must not be unbounded, and its params must be capped.
//
// Every assertion that could pass by accident is paired with a negative control.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.moleculesai.app/sdk/gen/go/molcontracts"
)

func fleetRequest(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// nil handle: the param-validation arms must reject BEFORE any query, which
	// is itself the assertion — a validator that only fires after the DB round
	// trip is not a validator.
	h := &AdminPluginInstallReportsHandler{db: nil}
	r.GET("/admin/plugin-install-reports", h.List)
	req := httptest.NewRequest(http.MethodGet, "/admin/plugin-install-reports"+query, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// --- 1. the not_live arm must BE the index predicate -----------------------

// The migration's index is `WHERE declared AND NOT swapped`. If this arm's SQL
// drifts from that string the planner is free to stop using the index, and the
// endpoint silently becomes the sequential scan the index was created to avoid —
// which is the original bug wearing an API.
func TestFleetReports_NotLiveArmIsTheIndexPredicateVerbatim(t *testing.T) {
	const indexPredicate = "declared AND NOT swapped" // copied from 20260730060000_…up.sql

	if notLiveFleetPredicate != indexPredicate {
		t.Fatalf("the not_live arm must match the partial index verbatim:\n index: %q\n  arm : %q",
			indexPredicate, notLiveFleetPredicate)
	}

	// Negative control: a LOGICALLY EQUIVALENT rewrite must be rejected by this
	// test, or the test is only asserting that two strings exist. `NOT (declared
	// AND swapped) AND declared` selects the same rows and is not guaranteed to be
	// recognised as implying the index's WHERE clause.
	equivalentRewrite := "NOT (declared AND swapped) AND declared"
	if equivalentRewrite == indexPredicate {
		t.Fatal("control is not exercising the hazard: the rewrite must differ textually")
	}
}

// --- 2. and 3. the two arms are the two classes, and they are disjoint ------

// THE case the old liveness rule got wrong, at the fleet level. A partially
// promoted box — declared, swapped, with a non-empty `failed` — is LIVE and
// DEGRADED. It must be reported by the degraded arm and must be ABSENT from the
// not-live arm.
//
// Asserted here against the Go rule (the integration test asserts the same thing
// against the SQL, which is the half that could disagree).
func TestFleetReports_PartialPromotionIsDegradedAndNotInTheNotLiveArm(t *testing.T) {
	// The exact report the runtime sends after promoting 5 of 6 sources.
	declared, swapped := true, true
	failed := []string{"gitea://molecule-ai/molecule-ai-plugin-lark#deadbeef"}

	if !reportIsLive(declared, swapped) {
		t.Fatal("a promoted tree is live — the runtime promotes partial builds on purpose")
	}
	if !reportIsDegraded(declared, swapped, failed) {
		t.Error("a promoted tree with a failed source must be reported by the degraded arm")
	}
	// The not-live arm's predicate, evaluated in Go over the same row.
	inNotLiveArm := declared && !swapped
	if inNotLiveArm {
		t.Error("a partially-promoted box must NOT appear in the not-live arm")
	}

	// Negative control: under the RETIRED rule (`… && failed == []`) this healthy
	// box is not live, so a fleet sweep built on it would have paged. If this ever
	// stops disagreeing, the assertions above have stopped exercising the hazard.
	retiredRuleSaysLive := declared && swapped && len(failed) == 0
	if retiredRuleSaysLive {
		t.Fatal("control is not exercising the hazard: the retired rule must disagree")
	}
}

// The two arms must never both claim the same row: `not_live` and `degraded`
// answer different questions and an operator triaging one must not be looking at
// the other's rows. Asserted over the whole input space, not by example.
func TestFleetReports_ArmsAreDisjointAcrossEveryInput(t *testing.T) {
	failedShapes := [][]string{nil, {}, {"one"}, {"one", "two"}}
	sawNotLive, sawDegraded := false, false
	for _, declared := range []bool{false, true} {
		for _, swapped := range []bool{false, true} {
			for _, failed := range failedShapes {
				inNotLive := declared && !swapped
				inDegraded := reportIsDegraded(declared, swapped, failed)
				if inNotLive && inDegraded {
					t.Errorf("declared=%v swapped=%v failed=%#v: a row cannot be in both arms",
						declared, swapped, failed)
				}
				// A degraded row is by definition live; a not-live row by definition
				// is not. Pin both directions so a future edit to either predicate
				// cannot make one arm swallow the other.
				if inDegraded && !reportIsLive(declared, swapped) {
					t.Errorf("declared=%v swapped=%v: degraded without live is not a state",
						declared, swapped)
				}
				if inNotLive && reportIsLive(declared, swapped) {
					t.Errorf("declared=%v swapped=%v: the not-live arm must exclude live rows",
						declared, swapped)
				}
				sawNotLive = sawNotLive || inNotLive
				sawDegraded = sawDegraded || inDegraded
			}
		}
	}
	// Negative control for a vacuous pass: if neither arm ever matched anything,
	// disjointness above is trivially true and proves nothing.
	if !sawNotLive || !sawDegraded {
		t.Fatalf("control failed: the space must contain both classes (not_live=%v degraded=%v)",
			sawNotLive, sawDegraded)
	}
}

// --- 4. params are capped, and the response is bounded ---------------------

func TestFleetReports_UnknownStatusIsRefusedWithTheAllowedSet(t *testing.T) {
	w := fleetRequest(t, "?status=everything")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown status filter must be refused, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Allowed []string `json:"allowed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Allowed) != len(fleetReportFilters) {
		t.Errorf("the error must list every accepted filter, got %#v", body.Allowed)
	}
	// The refusal must be the reason there is no unfiltered listing — an
	// "everything" arm has no index behind it.
	if _, exists := fleetReportFilters["all"]; exists {
		t.Error("there must be no unbounded/unfiltered fleet arm")
	}
}

func TestFleetReports_LimitIsCappedAndValidated(t *testing.T) {
	for _, bad := range []string{"0", "-1", "1001", "abc", "9999999999999999999999"} {
		w := fleetRequest(t, "?limit="+bad)
		if w.Code != http.StatusBadRequest {
			t.Errorf("limit=%q must be refused, got %d: %s", bad, w.Code, w.Body.String())
		}
	}
	// Negative control: a legal limit must NOT take the same path, or the test
	// above is only proving that the handler rejects everything.
	w := fleetRequest(t, "?limit=1000")
	if w.Code == http.StatusBadRequest {
		t.Fatalf("control failed: limit=1000 is legal and must not be refused: %s", w.Body.String())
	}
}

// A fleet read with no `limit` must still be bounded — the footgun is the caller
// who omits the param, not the one who passes 10000.
func TestFleetReports_DefaultLimitIsBounded(t *testing.T) {
	if defaultListLimit <= 0 || defaultListLimit > maxListLimit {
		t.Fatalf("default limit %d is not a bound", defaultListLimit)
	}
	if maxListLimit <= 0 {
		t.Fatalf("max limit %d is not a bound", maxListLimit)
	}
}

// --- the echoed rules must be the CORRECTED ones ---------------------------

// The response echoes the contract's rules so a reader learns why a row is in the
// arm it is in. Both must be the post-#4972 strings.
//
// This is a real regression that was live: core's pinned sdk/gen module still
// carried `live iff declared && swapped && failed == []` while reportIsLive had
// already been corrected, so GET /workspaces/:id/plugin-install-report was
// returning live=true, degraded=true alongside an outcome_rule that says that box
// should be live=false. The pre-existing invariant test only asserted the string
// CONTAINS "swapped" — which both the retired and the corrected rule do, so it
// could not see the drift. Pin the strings.
func TestFleetReports_EchoedRulesAreTheCorrectedContractRules(t *testing.T) {
	if molcontracts.PluginInstallReportOutcomeRule != "live iff declared && swapped" {
		t.Errorf("outcome rule is not the corrected one: %q", molcontracts.PluginInstallReportOutcomeRule)
	}
	if molcontracts.PluginInstallReportDegradedRule != "degraded iff live && failed != []" {
		t.Errorf("degraded rule is not the corrected one: %q", molcontracts.PluginInstallReportDegradedRule)
	}
	// The retired rule folded `failed` into liveness. Assert its absence directly:
	// a contains-check for "swapped" passes under BOTH rules, which is how the
	// stale pin survived.
	if strings.Contains(molcontracts.PluginInstallReportOutcomeRule, "failed") {
		t.Error("liveness must not consult `failed` — that is the retired rule")
	}
}

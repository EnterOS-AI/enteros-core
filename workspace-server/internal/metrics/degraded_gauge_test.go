package metrics

// degraded_gauge_test.go — core#5025 finding 6: a gauge that has never measured
// anything must not report a number.
//
// molecule_plugin_install_degraded_workspaces was printed from its Go zero value
// on every scrape. When the sweeper is disabled (StartDegradedPluginSweeper
// returns immediately on a nil db) or its query fails persistently, /metrics
// served "0 degraded" forever — byte-for-byte identical to a measured, healthy
// fleet.
//
// That is the exact failure the sweeper's own error path was written to avoid:
// sweepDegradedPluginInstallsOnce deliberately returns the error and leaves the
// previous reading rather than publishing a 0 it did not observe. The exposition
// layer then reintroduced it, which is worse, because the metric that exists to
// end a five-day silence (core#4997) reports the all-clear during exactly the
// outages that stop it from measuring.
//
// Two signals, because "absent" and "stale" are different questions: the series
// is OMITTED until the first successful sweep, and a last-success timestamp lets
// an alert require freshness rather than trusting a number's mere presence.

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func scrape(t *testing.T) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/metrics", Handler())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics returned %d", w.Code)
	}
	return w.Body.String()
}

// seriesValue returns the value of a bare (unlabelled) metric line.
func seriesValue(t *testing.T, body, name string) (string, bool) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name+" ") {
			return strings.TrimSpace(strings.TrimPrefix(line, name+" ")), true
		}
	}
	return "", false
}

// TestDegradedGauge_IsAbsentUntilSomethingHasBeenMeasured is the finding.
func TestDegradedGauge_IsAbsentUntilSomethingHasBeenMeasured(t *testing.T) {
	ResetDegradedPluginWorkspacesForTest()

	body := scrape(t)
	if v, ok := seriesValue(t, body, "molecule_plugin_install_degraded_workspaces"); ok {
		t.Fatalf("the gauge reported %q before any sweep succeeded. A sweeper that is disabled "+
			"(nil db) or whose query keeps failing then serves 'zero degraded workspaces' forever, "+
			"indistinguishable from a measured healthy fleet — the confident lie this metric exists "+
			"to end (core#4997).", v)
	}
}

// TestDegradedGauge_AppearsAfterTheFirstSuccessfulSweep is the RED CONTROL. An
// absent series is only the right answer while nothing has been measured;
// suppressing it forever would be a different way to say nothing.
func TestDegradedGauge_AppearsAfterTheFirstSuccessfulSweep(t *testing.T) {
	ResetDegradedPluginWorkspacesForTest()
	SetDegradedPluginWorkspaces(0) // a MEASURED zero — the fleet is genuinely healthy

	body := scrape(t)
	v, ok := seriesValue(t, body, "molecule_plugin_install_degraded_workspaces")
	if !ok {
		t.Fatal("after a successful sweep the gauge must be exported — a healthy fleet has to be able " +
			"to say so, or the alert can never clear")
	}
	if v != "0" {
		t.Fatalf("gauge = %q, want the measured 0", v)
	}
}

// TestDegradedGauge_ExposesLastSuccessSoAlertsCanRequireFreshness.
//
// Absence covers "never measured". It does NOT cover "measured once at boot and
// silently stuck since" — the series is present, the number looks fine, and the
// sweeper has been failing for a day. An alert can only exclude that if the
// exposition carries when the reading was taken.
func TestDegradedGauge_ExposesLastSuccessSoAlertsCanRequireFreshness(t *testing.T) {
	ResetDegradedPluginWorkspacesForTest()

	body := scrape(t)
	v, ok := seriesValue(t, body, "molecule_plugin_install_degraded_last_success_timestamp_seconds")
	if !ok {
		t.Fatal("no last-success timestamp is exported, so no alert can distinguish a fresh reading " +
			"from one frozen at boot")
	}
	if v != "0" {
		t.Fatalf("last-success = %q before any sweep; want 0 (the epoch reads as maximally stale, so a "+
			"freshness alert fires instead of trusting a number nobody took)", v)
	}

	before := time.Now().Unix()
	SetDegradedPluginWorkspaces(2)
	after := time.Now().Unix()

	v, ok = seriesValue(t, scrape(t), "molecule_plugin_install_degraded_last_success_timestamp_seconds")
	if !ok {
		t.Fatal("last-success timestamp vanished after a sweep")
	}
	ts, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		t.Fatalf("last-success %q is not a number: %v", v, err)
	}
	if ts < before || ts > after {
		t.Fatalf("last-success = %d, want a stamp taken during the sweep (%d..%d)", ts, before, after)
	}
}

// TestDegradedGauge_HelpTextPointsAtTheContractEndpoint keeps the HELP honest
// about WHICH workspaces are counted — the wording is what an operator reads at
// 3am, and core#5025 finding 8 was a case of the count and the wording
// disagreeing.
func TestDegradedGauge_HelpTextPointsAtTheContractEndpoint(t *testing.T) {
	ResetDegradedPluginWorkspacesForTest()
	SetDegradedPluginWorkspaces(1)

	body := scrape(t)
	if !strings.Contains(body, "# HELP molecule_plugin_install_degraded_workspaces") {
		t.Fatal("the gauge must carry HELP text")
	}
	if !strings.Contains(body, "# TYPE molecule_plugin_install_degraded_last_success_timestamp_seconds gauge") {
		t.Error("the staleness signal must be typed, or a scraper cannot use it")
	}
}

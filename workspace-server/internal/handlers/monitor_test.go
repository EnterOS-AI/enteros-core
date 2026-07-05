package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// Tests for the OSS Monitor API (monitor.go). The handler reads ONLY real
// rows from activity_logs / workspaces and never fabricates a series, so the
// two cases that matter most are:
//
//	(1) empty activity_logs → empty (all-zero) buckets + zero rates, and
//	(2) a few seeded a2a rows → the right per-bucket counts.

// callA2ATraffic builds a MonitorHandler over the current (mocked) db.DB and
// invokes A2ATraffic for the given window query, returning the recorder.
func callA2ATraffic(window string) *httptest.ResponseRecorder {
	handler := NewMonitorHandler(db.DB)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	url := "/monitor/a2a-traffic"
	if window != "" {
		url += "?window=" + window
	}
	c.Request = httptest.NewRequest("GET", url, nil)
	handler.A2ATraffic(c)
	return w
}

// a2aTrafficResp is the decoded shape of GET /monitor/a2a-traffic.
type a2aTrafficResp struct {
	Window        string `json:"window"`
	BucketSeconds int    `json:"bucket_seconds"`
	Buckets       []struct {
		Ts    string `json:"ts"`
		Count int    `json:"count"`
	} `json:"buckets"`
	RpsNow    float64 `json:"rps_now"`
	RpsPeak   float64 `json:"rps_peak"`
	RpsPeakAt *string `json:"rps_peak_at"`
	Total     int     `json:"total"`
}

// TestMonitorA2ATraffic_EmptyState verifies the honesty contract: when
// activity_logs has no a2a rows in the window, the response is a full bucket
// array of zeros with zero totals/rates — NOT a synthesised curve.
func TestMonitorA2ATraffic_EmptyState(t *testing.T) {
	mock := setupTestDB(t)

	// No rows returned — the GROUP BY found nothing.
	mock.ExpectQuery(`FROM activity_logs`).
		WithArgs(sqlmock.AnyArg(), 3600, 86400). // now, bucketSecs(24h), windowSecs(24h)
		WillReturnRows(sqlmock.NewRows([]string{"bucket_idx", "cnt"}))

	w := callA2ATraffic("24h")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp a2aTrafficResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Window != "24h" || resp.BucketSeconds != 3600 {
		t.Fatalf("unexpected window/bucket: %+v", resp)
	}
	if len(resp.Buckets) != 24 {
		t.Fatalf("expected 24 buckets, got %d", len(resp.Buckets))
	}
	for i, b := range resp.Buckets {
		if b.Count != 0 {
			t.Fatalf("bucket %d expected 0 count (empty state), got %d", i, b.Count)
		}
	}
	if resp.Total != 0 || resp.RpsNow != 0 || resp.RpsPeak != 0 {
		t.Fatalf("expected zero totals/rates, got total=%d rps_now=%v rps_peak=%v", resp.Total, resp.RpsNow, resp.RpsPeak)
	}
	if resp.RpsPeakAt != nil {
		t.Fatalf("expected rps_peak_at null in empty state, got %v", *resp.RpsPeakAt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestMonitorA2ATraffic_SeededRows verifies real bucket counts are mapped to
// the right positions: bucket_idx 0 = the newest bucket, increasing into the
// past, and the emitted array is oldest→newest.
func TestMonitorA2ATraffic_SeededRows(t *testing.T) {
	mock := setupTestDB(t)

	// 1h window → 60 one-minute buckets. Seed: 3 events in the newest bucket
	// (idx 0), 5 events two buckets back (idx 2).
	rows := sqlmock.NewRows([]string{"bucket_idx", "cnt"}).
		AddRow(0, 3).
		AddRow(2, 5)
	mock.ExpectQuery(`FROM activity_logs`).
		WithArgs(sqlmock.AnyArg(), 60, 3600).
		WillReturnRows(rows)

	w := callA2ATraffic("1h")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp a2aTrafficResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Buckets) != 60 {
		t.Fatalf("expected 60 buckets, got %d", len(resp.Buckets))
	}
	// Newest bucket (last in array) = idx 0 = 3 events.
	if got := resp.Buckets[59].Count; got != 3 {
		t.Fatalf("newest bucket: expected 3, got %d", got)
	}
	// idx 2 maps to position 60-1-2 = 57.
	if got := resp.Buckets[57].Count; got != 5 {
		t.Fatalf("bucket idx 2 (pos 57): expected 5, got %d", got)
	}
	if resp.Total != 8 {
		t.Fatalf("expected total 8, got %d", resp.Total)
	}
	// Peak bucket = 5 events / 60s.
	if resp.RpsPeak != 5.0/60.0 {
		t.Fatalf("expected rps_peak %v, got %v", 5.0/60.0, resp.RpsPeak)
	}
	// rps_now = newest bucket (3) / 60s.
	if resp.RpsNow != 3.0/60.0 {
		t.Fatalf("expected rps_now %v, got %v", 3.0/60.0, resp.RpsNow)
	}
	if resp.RpsPeakAt == nil {
		t.Fatalf("expected rps_peak_at to be set when total>0")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestMonitorA2ATraffic_DefaultWindow verifies the default window is 24h.
func TestMonitorA2ATraffic_DefaultWindow(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`FROM activity_logs`).
		WithArgs(sqlmock.AnyArg(), 3600, 86400).
		WillReturnRows(sqlmock.NewRows([]string{"bucket_idx", "cnt"}))

	w := callA2ATraffic("") // no window param
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp a2aTrafficResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Window != "24h" {
		t.Fatalf("expected default window 24h, got %q", resp.Window)
	}
}

// TestMonitorA2ATraffic_InvalidWindow verifies a bad window is rejected with
// 400 and never touches the DB.
func TestMonitorA2ATraffic_InvalidWindow(t *testing.T) {
	setupTestDB(t) // no ExpectQuery — the handler must not query on a 400.
	w := callA2ATraffic("99y")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// topologySummaryResp is the decoded shape of GET /monitor/topology-summary.
type topologySummaryResp struct {
	Total    int `json:"total"`
	Agents   int `json:"agents"`
	Teams    int `json:"teams"`
	Platform int `json:"platform"`
}

func callTopologySummary() *httptest.ResponseRecorder {
	handler := NewMonitorHandler(db.DB)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/monitor/topology-summary", nil)
	handler.TopologySummary(c)
	return w
}

// TestMonitorTopologySummary_Empty verifies an org with no workspaces returns
// all zeros (no fabricated counts).
func TestMonitorTopologySummary_Empty(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`FROM workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id", "kind"}))

	w := callTopologySummary()
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp topologySummaryResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 0 || resp.Agents != 0 || resp.Teams != 0 || resp.Platform != 0 {
		t.Fatalf("expected all zeros, got %+v", resp)
	}
}

// TestMonitorTopologySummary_RealCounts seeds a small org graph and asserts the
// agent/team split is computed from the REAL parent_id/kind data:
//
//	platform (root, kind=platform)         → team (and platform)
//	├─ team-a (kind=workspace, has child)  → team
//	│   └─ agent-1 (leaf)                  → agent
//	└─ agent-2 (leaf under platform)       → agent
func TestMonitorTopologySummary_RealCounts(t *testing.T) {
	mock := setupTestDB(t)
	rows := sqlmock.NewRows([]string{"id", "parent_id", "kind"}).
		AddRow("platform", nil, "platform").
		AddRow("team-a", "platform", "workspace").
		AddRow("agent-1", "team-a", "workspace").
		AddRow("agent-2", "platform", "workspace")
	mock.ExpectQuery(`FROM workspaces`).WillReturnRows(rows)

	w := callTopologySummary()
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp topologySummaryResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 4 {
		t.Fatalf("expected total 4, got %d", resp.Total)
	}
	// teams = platform + team-a = 2; agents = agent-1 + agent-2 = 2.
	if resp.Teams != 2 {
		t.Fatalf("expected 2 teams, got %d", resp.Teams)
	}
	if resp.Agents != 2 {
		t.Fatalf("expected 2 agents, got %d", resp.Agents)
	}
	if resp.Platform != 1 {
		t.Fatalf("expected 1 platform, got %d", resp.Platform)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

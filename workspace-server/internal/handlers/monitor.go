package handlers

import (
	"database/sql"
	"log"
	"net/http"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/gin-gonic/gin"
)

// MonitorHandler powers the OSS "Monitor" page (canvas/src/app/monitor) — the
// org-dashboard monitoring surface that lives in molecule-core (OSS) so every
// self-hoster sees it, served like Canvas, with the control plane / app only
// READING its API. It owns no synthetic data: every number it returns is read
// straight from the local tables (activity_logs, workspaces). Pre-customer
// volume is genuinely near-zero, and that is exactly what the endpoints
// report — empty series + zeros, never a fabricated curve.
//
// Both endpoints are admin-gated at the router (mirrors /requests/pending);
// the handler does no auth itself.
type MonitorHandler struct {
	db *sql.DB
}

// NewMonitorHandler wires the handler to the platform DB. The DB is captured at
// construction (same shape the router uses for AdminAuth(db.DB)); tests build
// the handler AFTER swapping db.DB for a sqlmock so the mock is captured.
func NewMonitorHandler(database *sql.DB) *MonitorHandler {
	return &MonitorHandler{db: database}
}

// a2aTrafficWindow describes one selectable time window: its total span, the
// per-bucket granularity, and the resulting bucket count (span / bucket).
type a2aTrafficWindow struct {
	windowSecs int
	bucketSecs int
	buckets    int
}

// a2aWindows maps the canvas window toggle (1H / 24H / 7D / 30D) to sane bucket
// granularities. Each window yields a moderate, chart-friendly number of
// buckets (24–60) so the series is readable without flooding the client:
//
//	1h  → 1-minute buckets  (60)
//	24h → 1-hour buckets    (24)
//	7d  → 6-hour buckets    (28)
//	30d → 1-day buckets     (30)
func a2aWindows(window string) (a2aTrafficWindow, bool) {
	switch window {
	case "1h":
		return a2aTrafficWindow{windowSecs: 3600, bucketSecs: 60, buckets: 60}, true
	case "24h":
		return a2aTrafficWindow{windowSecs: 86400, bucketSecs: 3600, buckets: 24}, true
	case "7d":
		return a2aTrafficWindow{windowSecs: 7 * 86400, bucketSecs: 6 * 3600, buckets: 28}, true
	case "30d":
		return a2aTrafficWindow{windowSecs: 30 * 86400, bucketSecs: 86400, buckets: 30}, true
	default:
		return a2aTrafficWindow{}, false
	}
}

// a2aTrafficBucket is one point on the traffic time-series. Ts is the START of
// the bucket interval (UTC); Count is the number of a2a_send + a2a_receive
// activity_logs rows that fall inside it.
type a2aTrafficBucket struct {
	Ts    time.Time `json:"ts"`
	Count int       `json:"count"`
}

// A2ATraffic handles GET /monitor/a2a-traffic?window=1h|24h|7d|30d
//
// It returns a time-bucketed count of agent-to-agent traffic from
// activity_logs (activity_type IN ('a2a_send','a2a_receive')), plus the
// current and peak request-per-second rate derived from those buckets.
//
// HONESTY CONTRACT: the buckets are the REAL counts. When activity_logs holds
// no a2a rows in the window (the pre-customer norm) every bucket is 0, total is
// 0, and the rate fields are 0 / null. The handler NEVER synthesises a curve.
//
// Bucketing is done in SQL via a single GROUP BY on a computed bucket index
// (floor((now - created_at) / bucketSecs)); the full bucket array — including
// the empty buckets the GROUP BY omits — is assembled in Go. `now` is captured
// once in Go and bound as a parameter so SQL and Go agree on the window edges
// (and so the query is deterministic for sqlmock).
func (h *MonitorHandler) A2ATraffic(c *gin.Context) {
	window := c.DefaultQuery("window", "24h")
	w, ok := a2aWindows(window)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "window must be one of 1h, 24h, 7d, 30d"})
		return
	}

	now := time.Now().UTC()

	// bucket_idx 0 = the most-recent bucket (now-bucketSecs .. now), increasing
	// into the past. Rows older than the window or newer than `now` are
	// excluded by the WHERE clause, so bucket_idx is always in [0, buckets).
	const query = `
		SELECT floor(extract(epoch FROM ($1::timestamptz - created_at)) / $2)::int AS bucket_idx,
		       COUNT(*) AS cnt
		FROM activity_logs
		WHERE activity_type IN ('a2a_send', 'a2a_receive')
		  AND created_at > $1::timestamptz - make_interval(secs => $3)
		  AND created_at <= $1::timestamptz
		GROUP BY bucket_idx`

	rows, err := h.db.QueryContext(c.Request.Context(), query, now, w.bucketSecs, w.windowSecs)
	if err != nil {
		log.Printf("Monitor a2a-traffic query error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	// counts indexed by emitted position: 0 = oldest bucket, buckets-1 = newest.
	counts := make([]int, w.buckets)
	for rows.Next() {
		var idx, cnt int
		if err := rows.Scan(&idx, &cnt); err != nil {
			log.Printf("Monitor a2a-traffic scan error: %v", err)
			continue
		}
		if idx < 0 || idx >= w.buckets {
			continue
		}
		counts[w.buckets-1-idx] += cnt
	}
	if err := rows.Err(); err != nil {
		log.Printf("Monitor a2a-traffic rows error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query iteration failed"})
		return
	}

	buckets := make([]a2aTrafficBucket, w.buckets)
	total := 0
	peak := 0
	var peakAt *time.Time
	for p := 0; p < w.buckets; p++ {
		// Bucket start = now - (buckets - p) * bucketSecs. p=buckets-1 (newest)
		// starts one bucket-width before now; p=0 (oldest) starts at the window
		// edge.
		startOffset := time.Duration(w.buckets-p) * time.Duration(w.bucketSecs) * time.Second
		ts := now.Add(-startOffset)
		cnt := counts[p]
		buckets[p] = a2aTrafficBucket{Ts: ts, Count: cnt}
		total += cnt
		if cnt > peak {
			peak = cnt
			at := ts
			peakAt = &at
		}
	}

	// rps = events in a bucket / bucket width. "now" = the most-recent bucket.
	rpsNow := float64(counts[w.buckets-1]) / float64(w.bucketSecs)
	rpsPeak := float64(peak) / float64(w.bucketSecs)

	c.JSON(http.StatusOK, gin.H{
		"window":         window,
		"bucket_seconds": w.bucketSecs,
		"buckets":        buckets,
		"rps_now":        rpsNow,
		"rps_peak":       rpsPeak,
		"rps_peak_at":    peakAt,
		"total":          total,
	})
}

// TopologySummary handles GET /monitor/topology-summary
//
// It returns REAL agent/team counts derived from the workspaces graph (the same
// data GET /workspaces serves), so the Monitor page never has to fabricate a
// "9 agents · 3 teams". This is the single source of truth for the headline
// counts the canvas renders and the CP / app can read.
//
// Definitions (mirrors canvas/src/store/canvas-topology.ts semantics):
//   - team  = a node that has at least one child OR is kind=platform (the org
//     concierge root is always a team).
//   - agent = a leaf node (no children) that is not kind=platform.
//
// Removed workspaces are excluded (status != 'removed'), matching workspaceListQuery.
func (h *MonitorHandler) TopologySummary(c *gin.Context) {
	const query = `
		SELECT id, parent_id, COALESCE(kind, 'workspace')
		FROM workspaces
		WHERE status != 'removed'`

	rows, err := h.db.QueryContext(c.Request.Context(), query)
	if err != nil {
		log.Printf("Monitor topology-summary query error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	type node struct {
		id   string
		kind string
	}
	var nodes []node
	childCount := make(map[string]int)
	for rows.Next() {
		var id, kind string
		var parentID *string
		if err := rows.Scan(&id, &parentID, &kind); err != nil {
			log.Printf("Monitor topology-summary scan error: %v", err)
			continue
		}
		nodes = append(nodes, node{id: id, kind: kind})
		if parentID != nil && *parentID != "" {
			childCount[*parentID]++
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("Monitor topology-summary rows error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query iteration failed"})
		return
	}

	agents := 0
	teams := 0
	platform := 0
	for _, n := range nodes {
		isPlatform := n.kind == models.KindPlatform
		if isPlatform {
			platform++
		}
		if isPlatform || childCount[n.id] > 0 {
			teams++
		} else {
			agents++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"total":    len(nodes),
		"agents":   agents,
		"teams":    teams,
		"platform": platform,
	})
}

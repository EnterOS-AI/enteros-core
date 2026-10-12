package handlers

import (
	"database/sql"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

// admin_delegations.go — RFC #2829 PR-4: operator dashboard endpoint
// over the durable delegations ledger (PR-1 schema, PR-3 sweeper).
//
// What this endpoint serves
// -------------------------
//
//   GET /admin/delegations[?status=in_flight|stuck|failed&limit=N]
//
// Returns the rows the operator needs to triage delegation health:
//   - in_flight : status IN (queued, dispatched, in_progress) — the
//                 things actively churning right now. Default view.
//   - stuck     : status='stuck' — sweeper found these wedged. Operator
//                 can investigate the callee + decide whether to retry
//                 (RFC #2829 PR-5 plan).
//   - failed    : status='failed' — terminal failures, recent. Useful
//                 for spotting trends like "callee X is failing 50% of
//                 delegations since 14:00".
//
// Why an admin endpoint at all
// ----------------------------
// Without this, post-incident investigation requires direct DB access —
// only the on-call SRE can answer "is workspace X delegating to a wedged
// callee?". The dashboard endpoint moves that visibility into the same
// surface as /admin/queue, /admin/schedules-health, /admin/memories etc.
//
// Out of scope (deferred to a follow-up PR per RFC #2829)
// -------------------------------------------------------
//   - "retry this stuck task" mutation: needs careful interaction with
//     the agent-side cutover (PR-5) before it can be safely re-fired
//   - p95 / p99 duration aggregates: separate metric exposure, not a
//     row-level read
//   - Canvas UI: this is the JSON contract; the canvas operator panel
//     consumes it in a follow-up canvas PR

// AdminDelegationsHandler serves the operator dashboard read endpoint.
type AdminDelegationsHandler struct {
	db *sql.DB
}

func NewAdminDelegationsHandler(handle *sql.DB) *AdminDelegationsHandler {
	if handle == nil {
		handle = db.DB
	}
	return &AdminDelegationsHandler{db: handle}
}

// delegationRow mirrors the row shape of the `delegations` table that the
// operator dashboard cares about. Order matches the SELECT below — keep
// the two in sync if you add a column.
type delegationRow struct {
	DelegationID  string     `json:"delegation_id"`
	CallerID      string     `json:"caller_id"`
	CalleeID      string     `json:"callee_id"`
	TaskPreview   string     `json:"task_preview"`
	Status        string     `json:"status"`
	LastHeartbeat *time.Time `json:"last_heartbeat,omitempty"`
	Deadline      time.Time  `json:"deadline"`
	ResultPreview *string    `json:"result_preview,omitempty"`
	ErrorDetail   *string    `json:"error_detail,omitempty"`
	RetryCount    int        `json:"retry_count"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// statusFilters maps the query-string `status` value to the SQL set.
// Keep tight — operators don't get to query arbitrary status — so a
// new status name added to the schema needs an explicit allowlist
// entry here. Caught when a future status name doesn't pin to a UI
// expectation (forward-defense).
var statusFilters = map[string][]string{
	// DERIVED — and this one mattered: `in_flight` is the DEFAULT view
	// (c.DefaultQuery("status", "in_flight")), and the hand-typed list omitted
	// `stuck`. So the operator opening the delegations dashboard to find out why an
	// agent was wedged saw everything EXCEPT the wedged delegations. That is #4314
	// with a UI instead of a digest.
	//
	// `stuck` rows appear in BOTH `in_flight` (they are awaiting an answer) and the
	// dedicated `stuck` tab (which narrows to just them). The overlap is intended.
	"in_flight": DelegationInFlightStates,
	"stuck":     {"stuck"},
	"failed":    {"failed"},
	"completed": {"completed"},
}

// statusFilterKeys returns the accepted `?status=` values, sorted for a stable
// error message.
func statusFilterKeys() []string {
	keys := make([]string, 0, len(statusFilters))
	for k := range statusFilters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

const defaultListLimit = 100
const maxListLimit = 1000

// List handles GET /admin/delegations
//
// Query params:
//   - status — one of `in_flight` (default) / `stuck` / `failed` / `completed`
//   - limit  — int, 1..1000 (default 100)
//
// Returns 200 with `{"delegations": [...], "count": N}`.
func (h *AdminDelegationsHandler) List(c *gin.Context) {
	statusKey := c.DefaultQuery("status", "in_flight")
	statuses, ok := statusFilters[statusKey]
	if !ok {
		// DERIVED from the map itself. Hand-listing the keys here meant the error
		// message could disagree with what the endpoint actually accepts — a small
		// instance of the same drift, and it lists status names that are really
		// FILTER keys, which is its own confusion.
		c.JSON(http.StatusBadRequest, gin.H{
			"error":            "unknown status filter",
			"allowed":          statusFilterKeys(),
			"requested_status": statusKey,
		})
		return
	}

	limit := defaultListLimit
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListLimit {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":     "limit must be 1..1000",
				"requested": v,
			})
			return
		}
		limit = n
	}

	// Build the IN list as a parameterized expression — never string-
	// concatenate user-controlled values into the SQL. statusKey came
	// from the allowlist above so the slice is fully bounded.
	args := make([]any, 0, len(statuses)+1)
	placeholders := ""
	for i, s := range statuses {
		if i > 0 {
			placeholders += ","
		}
		args = append(args, s)
		placeholders += "$" + strconv.Itoa(i+1)
	}
	args = append(args, limit)
	limitPlaceholder := "$" + strconv.Itoa(len(statuses)+1)

	rows, err := h.db.QueryContext(c.Request.Context(), `
		SELECT delegation_id, caller_id::text, callee_id::text, task_preview,
		       status, last_heartbeat, deadline, result_preview, error_detail,
		       retry_count, created_at, updated_at
		  FROM delegations
		 WHERE status IN (`+placeholders+`)
		 ORDER BY created_at DESC
		 LIMIT `+limitPlaceholder, args...)
	if err != nil {
		log.Printf("AdminDelegations.List: query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	out := make([]delegationRow, 0)
	for rows.Next() {
		var r delegationRow
		var lastBeat sql.NullTime
		var resultPreview, errorDetail sql.NullString
		if err := rows.Scan(
			&r.DelegationID, &r.CallerID, &r.CalleeID, &r.TaskPreview,
			&r.Status, &lastBeat, &r.Deadline, &resultPreview, &errorDetail,
			&r.RetryCount, &r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			log.Printf("AdminDelegations.List: scan failed: %v", err)
			continue
		}
		if lastBeat.Valid {
			t := lastBeat.Time
			r.LastHeartbeat = &t
		}
		if resultPreview.Valid {
			s := resultPreview.String
			r.ResultPreview = &s
		}
		if errorDetail.Valid {
			s := errorDetail.String
			r.ErrorDetail = &s
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		log.Printf("AdminDelegations.List: rows.Err: %v", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"delegations": out,
		"count":       len(out),
		"status":      statusKey,
		"limit":       limit,
	})
}

// Stats handles GET /admin/delegations/stats — at-a-glance counts per
// status. Useful for the dashboard summary card at the top of the
// operator panel without paying for a row-level fetch.
//
// Returns 200 with `{"queued": N, "dispatched": N, "in_progress": N,
// "completed": N, "failed": N, "stuck": N}`.
func (h *AdminDelegationsHandler) Stats(c *gin.Context) {
	rows, err := h.db.QueryContext(c.Request.Context(), `
		SELECT status, COUNT(*) FROM delegations GROUP BY status
	`)
	if err != nil {
		log.Printf("AdminDelegations.Stats: query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	// Initialise to zero so the response always has every known status key — the
	// dashboard card doesn't need to handle "missing key vs zero" branching.
	//
	// DERIVED: this list happens to be complete today, but a hand-typed one goes
	// stale silently — a new state would simply never appear on the dashboard, and
	// nobody would know to look for it.
	stats := make(map[string]int, len(DelegationAllStates))
	for _, st := range DelegationAllStates {
		stats[st] = 0
	}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			log.Printf("AdminDelegations.Stats: scan failed: %v", err)
			continue
		}
		stats[status] = count
	}
	if err := rows.Err(); err != nil {
		log.Printf("AdminDelegations.Stats: rows.Err: %v", err)
	}

	c.JSON(http.StatusOK, stats)
}

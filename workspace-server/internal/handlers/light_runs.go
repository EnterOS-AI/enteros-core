package handlers

// light_runs.go — the Enter OS Light run ledger (contract C3) and the owner's
// runs listing.
//
// POST /workspaces/:id/light/runs is called by a run pod with the agent's run
// token: once "started" (status running) before the turn, once "finished"
// after it. Core upserts on (workspace_id, run_id):
//
//   - started inserts a running row, or refreshes a running one; a started for
//     a run whose row is already terminal is refused 409 LIGHT_RUN_FINISHED
//     (the runtime then does not run — e.g. a run a pause cancelled);
//   - finished overwrites a running row or one core recorded from the jobs
//     listing (source 'job'), never a reported terminal row: the first
//     finished report wins and a retried one is a 200 no-op. A finished write
//     never goes back to running.
//
// Output is kept to its last 64 KiB and error to its first 4 KiB (the runtime
// already truncates; core enforces it rather than refusing the report), and
// U+0000 or invalid UTF-8 in them becomes U+FFFD (Postgres text cannot hold a
// NUL; refusing would lose the run's outcome).

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
)

const (
	lightOutputLimit       = 64 << 10
	lightErrorLimit        = 4 << 10
	lightReportBodyLimit   = 256 << 10
	lightScheduleNameLimit = 128
	lightRunsDefaultLimit  = 20
	lightRunsMaxLimit      = 100
)

// lightRunIDRe is a run id as the runtime sends it (MOLECULE_RUN_ID, the Job
// name): printable ASCII, no space, at most 128 bytes.
var lightRunIDRe = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)

// lightRunReport is the C3 body.
type lightRunReport struct {
	RunID      string  `json:"run_id"`
	Schedule   string  `json:"schedule"`
	Phase      string  `json:"phase"`
	Status     string  `json:"status"`
	StartedAt  *string `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
	ExitCode   *int    `json:"exit_code"`
	Output     *string `json:"output"`
	Error      *string `json:"error"`
}

// lightRunRow is one ledger row as core writes it.
type lightRunRow struct {
	RunID        string
	Schedule     string
	Status       string
	Source       string
	ScheduledFor *time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
	ExitCode     *int
	Output       string
	Error        string
}

// tailUTF8 keeps the last max bytes of s without splitting a character.
func tailUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[len(s)-max:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// headUTF8 keeps the first max bytes of s without splitting a character.
func headUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func validLightScheduleName(name string) bool {
	if name == "" || len(name) > lightScheduleNameLimit || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func parseLightTime(s *string) (*time.Time, bool) {
	if s == nil || *s == "" {
		return nil, true
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return nil, false
	}
	t = t.UTC()
	return &t, true
}

// validateLightRunReport checks a C3 body and returns the row to write, or the
// name of the first bad field.
func validateLightRunReport(r lightRunReport) (lightRunRow, string) {
	row := lightRunRow{RunID: r.RunID, Schedule: r.Schedule, Status: r.Status, Source: "report", ExitCode: r.ExitCode}
	if !lightRunIDRe.MatchString(r.RunID) {
		return row, "run_id"
	}
	if !validLightScheduleName(r.Schedule) {
		return row, "schedule"
	}
	switch r.Phase {
	case "started":
		if r.Status != models.LightRunRunning {
			return row, "status"
		}
	case "finished":
		switch r.Status {
		case models.LightRunSucceeded, models.LightRunFailed, models.LightRunTimedOut:
		default:
			return row, "status"
		}
	default:
		return row, "phase"
	}
	started, ok := parseLightTime(r.StartedAt)
	if !ok || started == nil {
		return row, "started_at"
	}
	row.StartedAt = started
	finished, ok := parseLightTime(r.FinishedAt)
	if !ok {
		return row, "finished_at"
	}
	row.FinishedAt = finished
	if r.ExitCode != nil && (*r.ExitCode < math.MinInt32 || *r.ExitCode > math.MaxInt32) {
		return row, "exit_code"
	}
	if r.Output != nil {
		row.Output = tailUTF8(lightStorableText(*r.Output), lightOutputLimit)
	}
	if r.Error != nil {
		row.Error = headUTF8(lightStorableText(*r.Error), lightErrorLimit)
	}
	return row, ""
}

// lightStorableText makes report text storable in a Postgres text column:
// invalid UTF-8 and U+0000 (which text refuses) become U+FFFD. Applied before
// the byte limits, so the stored text still fits them.
func lightStorableText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "\uFFFD")
}

// upsertLightRunReport writes a run pod's report. accepted is false for a
// started report on a run whose row is already terminal.
func upsertLightRunReport(ctx context.Context, workspaceID, phase string, row lightRunRow) (accepted bool, err error) {
	if phase == "started" {
		var status string
		err := db.DB.QueryRowContext(ctx, `
			INSERT INTO light_runs (workspace_id, run_id, schedule, status, source, started_at)
			VALUES ($1, $2, $3, 'running', 'report', $4)
			ON CONFLICT (workspace_id, run_id) DO UPDATE
				SET source = 'report',
				    started_at = COALESCE(light_runs.started_at, EXCLUDED.started_at),
				    updated_at = now()
				WHERE light_runs.status = 'running'
			RETURNING status`,
			workspaceID, row.RunID, row.Schedule, row.StartedAt,
		).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}
	_, err = db.DB.ExecContext(ctx, `
		INSERT INTO light_runs (workspace_id, run_id, schedule, status, source, started_at, finished_at, exit_code, output, error)
		VALUES ($1, $2, $3, $4, 'report', $5, COALESCE($6, now()), $7, $8, $9)
		ON CONFLICT (workspace_id, run_id) DO UPDATE
			SET status = EXCLUDED.status,
			    source = 'report',
			    started_at = COALESCE(light_runs.started_at, EXCLUDED.started_at),
			    finished_at = EXCLUDED.finished_at,
			    exit_code = EXCLUDED.exit_code,
			    output = EXCLUDED.output,
			    error = EXCLUDED.error,
			    updated_at = now()
			WHERE light_runs.status = 'running' OR light_runs.source = 'job'`,
		workspaceID, row.RunID, row.Schedule, row.Status, row.StartedAt, row.FinishedAt, row.ExitCode, row.Output, row.Error)
	return err == nil, err
}

// ReportRun handles POST /workspaces/:id/light/runs (contract C3).
//
//	@Summary	Record a Light run (called by the run pod)
//	@Description	The run pod's report, with the agent's run token: phase "started" (status running) and phase "finished" (succeeded|failed|timed_out). Upserts on (workspace_id, run_id); a finished run never goes back to running.
//	@Tags		light
//	@Accept		json
//	@Produce	json
//	@Param		id	path		string	true	"Workspace ID"
//	@Success	200	{object}	map[string]interface{}	"{\"ok\":true}"
//	@Failure	400	{object}	map[string]interface{}	"LIGHT_INVALID_REPORT"
//	@Failure	403	{object}	map[string]interface{}	"LIGHT_RUN_TOKEN_REQUIRED"
//	@Failure	409	{object}	map[string]interface{}	"LIGHT_NOT_ENABLED, LIGHT_RUN_FINISHED"
//	@Router		/workspaces/{id}/light/runs [post]
//	@Security	BearerAuth
func (h *LightHandler) ReportRun(c *gin.Context) {
	const op = "report"
	id, ok := lightPreamble(c, op, false)
	if !ok {
		return
	}
	// Only the agent's own token writes its ledger (C3). WorkspaceAuth records
	// the workspace a workspace token proved; owner credentials carry none.
	if c.GetString("authenticated_workspace_id") != id {
		lightCode(c, op, id, http.StatusForbidden, "LIGHT_RUN_TOKEN_REQUIRED",
			"the run ledger is written by the agent's own runs with its workspace token", nil)
		return
	}
	var report lightRunReport
	if err := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, lightReportBodyLimit)).Decode(&report); err != nil {
		lightCode(c, op, id, http.StatusBadRequest, "LIGHT_INVALID_REPORT", "the body must be one JSON object (C3)", gin.H{"field": "body"})
		return
	}
	row, bad := validateLightRunReport(report)
	if bad != "" {
		lightCode(c, op, id, http.StatusBadRequest, "LIGHT_INVALID_REPORT", "invalid "+bad, gin.H{"field": bad})
		return
	}
	ctx := c.Request.Context()
	state, err := readLightState(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		lightCode(c, op, id, http.StatusNotFound, "WORKSPACE_NOT_FOUND", "workspace not found", nil)
		return
	}
	if err != nil {
		log.Printf("light op=report ws=%s run=%s: %v", id, row.RunID, err)
		lightCode(c, op, id, http.StatusInternalServerError, "LIGHT_FAILED", "failed to record the run", nil)
		return
	}
	if state == "" {
		lightCode(c, op, id, http.StatusConflict, "LIGHT_NOT_ENABLED", "this workspace is not a Light agent", nil)
		return
	}
	accepted, err := upsertLightRunReport(ctx, id, report.Phase, row)
	if err != nil {
		log.Printf("light op=report ws=%s run=%s: %v", id, row.RunID, err)
		lightCode(c, op, id, http.StatusInternalServerError, "LIGHT_FAILED", "failed to record the run", nil)
		return
	}
	if !accepted {
		lightCode(c, op, id, http.StatusConflict, "LIGHT_RUN_FINISHED",
			"this run is already recorded as finished; it must not start", gin.H{"run_id": row.RunID})
		return
	}
	log.Printf("light op=report ws=%s run=%s schedule=%q phase=%s status=%s", id, row.RunID, row.Schedule, report.Phase, row.Status)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// lightRunView is one run as the listing returns it.
type lightRunView struct {
	RunID         string     `json:"run_id"`
	Schedule      string     `json:"schedule"`
	Status        string     `json:"status"`
	Source        string     `json:"source"`
	ScheduledFor  *time.Time `json:"scheduled_for"`
	StartedAt     *time.Time `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	ExitCode      *int       `json:"exit_code"`
	Output        string     `json:"output"`
	Error         string     `json:"error"`
	CreatedAt     time.Time  `json:"created_at"`
	ActivityCount int        `json:"activity_count"`
	ActivityPath  string     `json:"activity_path"`
}

func nullTimeUTC(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}

// lightRunsCursor is the listing's next_before: the last run's created_at and
// run_id (the second sort key breaks created_at ties, so no run is skipped or
// repeated across pages), opaque to the client.
func lightRunsCursor(createdAt time.Time, runID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt.UTC().Format(time.RFC3339Nano) + " " + runID))
}

// parseLightRunsBefore reads ?before=: a next_before cursor, or a plain
// RFC3339 time (runs recorded strictly earlier). runID is empty for a time.
func parseLightRunsBefore(v string) (at time.Time, runID string, ok bool) {
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, "", true
	}
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return time.Time{}, "", false
	}
	ts, id, found := strings.Cut(string(raw), " ")
	if !found || !lightRunIDRe.MatchString(id) {
		return time.Time{}, "", false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, "", false
	}
	return t, id, true
}

// ListRuns handles GET /workspaces/:id/light/runs.
//
//	@Summary	A Light agent's runs, newest first
//	@Description	Each run with its outcome, output (last 64 KiB) and error, the number of activity rows it wrote, and the activity path filtered to it. Page with ?before=<next_before> (null on the last page).
//	@Tags		light
//	@Produce	json
//	@Param		id			path		string	true	"Workspace ID"
//	@Param		limit		query		int		false	"1..100, default 20 (above 100 is capped)"
//	@Param		schedule	query		string	false	"only this schedule's runs"
//	@Param		before		query		string	false	"the previous page's next_before, or an RFC3339 time (runs recorded strictly earlier)"
//	@Success	200			{object}	map[string]interface{}
//	@Failure	400			{object}	map[string]interface{}	"LIGHT_BAD_REQUEST"
//	@Failure	404			{object}	map[string]interface{}	"WORKSPACE_NOT_FOUND"
//	@Router		/workspaces/{id}/light/runs [get]
//	@Security	BearerAuth
func (h *LightHandler) ListRuns(c *gin.Context) {
	const op = "runs"
	id, ok := lightPreamble(c, op, false)
	if !ok {
		return
	}
	limit := lightRunsDefaultLimit
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			lightCode(c, op, id, http.StatusBadRequest, "LIGHT_BAD_REQUEST", "limit must be a positive integer", nil)
			return
		}
		if n > lightRunsMaxLimit {
			n = lightRunsMaxLimit
		}
		limit = n
	}
	schedule := c.Query("schedule")
	if len(schedule) > lightScheduleNameLimit {
		lightCode(c, op, id, http.StatusBadRequest, "LIGHT_BAD_REQUEST", "schedule is longer than 128 bytes", nil)
		return
	}
	var before *time.Time
	var beforeRun string
	if v := c.Query("before"); v != "" {
		t, runID, ok := parseLightRunsBefore(v)
		if !ok {
			lightCode(c, op, id, http.StatusBadRequest, "LIGHT_BAD_REQUEST", "before must be a next_before value or an RFC3339 time", nil)
			return
		}
		before, beforeRun = &t, runID
	}

	ctx := c.Request.Context()
	var one int
	if err := db.DB.QueryRowContext(ctx, `SELECT 1 FROM workspaces WHERE id = $1 AND status != 'removed'`, id).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			lightCode(c, op, id, http.StatusNotFound, "WORKSPACE_NOT_FOUND", "workspace not found", nil)
			return
		}
		lightCode(c, op, id, http.StatusInternalServerError, "LIGHT_FAILED", "failed to list runs", nil)
		return
	}

	query := `
		SELECT run_id, schedule, status, source, scheduled_for, started_at, finished_at, exit_code, output, error, created_at
		FROM light_runs WHERE workspace_id = $1`
	args := []interface{}{id}
	if schedule != "" {
		args = append(args, schedule)
		query += fmt.Sprintf(" AND schedule = $%d", len(args))
	}
	switch {
	case before != nil && beforeRun != "":
		args = append(args, *before, beforeRun)
		query += fmt.Sprintf(" AND (created_at, run_id) < ($%d, $%d)", len(args)-1, len(args))
	case before != nil:
		args = append(args, *before)
		query += fmt.Sprintf(" AND created_at < $%d", len(args))
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY created_at DESC, run_id DESC LIMIT $%d", len(args))

	rows, err := db.DB.QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("light op=runs ws=%s: %v", id, err)
		lightCode(c, op, id, http.StatusInternalServerError, "LIGHT_FAILED", "failed to list runs", nil)
		return
	}
	runs := []lightRunView{}
	var ids []string
	for rows.Next() {
		var v lightRunView
		var scheduledFor, startedAt, finishedAt sql.NullTime
		var exitCode sql.NullInt64
		if err := rows.Scan(&v.RunID, &v.Schedule, &v.Status, &v.Source, &scheduledFor, &startedAt, &finishedAt, &exitCode, &v.Output, &v.Error, &v.CreatedAt); err != nil {
			rows.Close()
			lightCode(c, op, id, http.StatusInternalServerError, "LIGHT_FAILED", "failed to list runs", nil)
			return
		}
		v.ScheduledFor, v.StartedAt, v.FinishedAt = nullTimeUTC(scheduledFor), nullTimeUTC(startedAt), nullTimeUTC(finishedAt)
		if exitCode.Valid {
			n := int(exitCode.Int64)
			v.ExitCode = &n
		}
		v.CreatedAt = v.CreatedAt.UTC()
		v.ActivityPath = "/workspaces/" + id + "/activity?run_id=" + url.QueryEscape(v.RunID)
		runs = append(runs, v)
		ids = append(ids, v.RunID)
	}
	rows.Close()

	if len(ids) > 0 {
		counts, err := db.DB.QueryContext(ctx, `
			SELECT run_id, COUNT(*) FROM activity_logs
			WHERE workspace_id = $1 AND run_id = ANY($2)
			GROUP BY run_id`, id, pq.Array(ids))
		if err != nil {
			log.Printf("light op=runs ws=%s: activity counts: %v", id, err)
		} else {
			byRun := map[string]int{}
			for counts.Next() {
				var runID string
				var n int
				if counts.Scan(&runID, &n) == nil {
					byRun[runID] = n
				}
			}
			counts.Close()
			for i := range runs {
				runs[i].ActivityCount = byRun[runs[i].RunID]
			}
		}
	}

	var next interface{}
	if len(runs) == limit {
		last := runs[len(runs)-1]
		next = lightRunsCursor(last.CreatedAt, last.RunID)
	}
	c.JSON(http.StatusOK, gin.H{"runs": runs, "next_before": next})
}

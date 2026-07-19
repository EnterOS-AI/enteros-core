package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/cronspec"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/registry"
)

// ErrorResponse is returned for 4xx/5xx errors. (OpenAPI doc shape — used by swaggo.)
type ErrorResponse struct {
	Error string `json:"error"`
}

// StatusResponse is returned by mutating endpoints that only echo a status verb.
type StatusResponse struct {
	Status string `json:"status"`
}

// CreateScheduleResponse is returned by POST /workspaces/{id}/schedules.
type CreateScheduleResponse struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	NextRunAt time.Time `json:"next_run_at"`
}

// RunNowResponse is returned by POST /workspaces/{id}/schedules/{scheduleId}/run.
type RunNowResponse struct {
	Status      string `json:"status"`
	WorkspaceID string `json:"workspace_id"`
	Prompt      string `json:"prompt"`
}

// HistoryEntry is one row of /workspaces/{id}/schedules/{scheduleId}/history.
type HistoryEntry struct {
	Timestamp   time.Time       `json:"timestamp"`
	DurationMs  *int            `json:"duration_ms"`
	Status      *string         `json:"status"`
	ErrorDetail string          `json:"error_detail"`
	Request     json.RawMessage `json:"request" swaggertype:"object"`
}

type ScheduleHandler struct {
	// httpClient forwards volume-mode CRUD to the workspace runtime's
	// /internal/schedules API. Broken out so tests can inject an
	// httptest.Server-backed client. nil → lazily built with the SSRF-safe
	// dialer (same guard as the chat-file / A2A forwards).
	httpClient *http.Client
}

func NewScheduleHandler() *ScheduleHandler {
	return &ScheduleHandler{}
}

// forwardClient returns the SSRF-safe client used for volume-mode forwards,
// lazily constructing the default. The dialer re-applies isSafeURL at connect
// time (DNS-rebind defence), matching chat_files/a2a_proxy.
func (h *ScheduleHandler) forwardClient() *http.Client {
	if h.httpClient != nil {
		return h.httpClient
	}
	h.httpClient = &http.Client{
		Timeout: scheduleForwardTimeout,
		Transport: &http.Transport{
			DialContext:         safeDialer().DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
	return h.httpClient
}

// computeNextRunSafe wraps cronspec.ComputeNextRun with the current time in the
// schedule's timezone. Returned in UTC (cronspec's contract).
func computeNextRunSafe(cronExpr, tz string) (time.Time, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, err
	}
	return cronspec.ComputeNextRun(cronExpr, tz, time.Now().In(loc))
}

type ScheduleResponse struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	Name        string     `json:"name"`
	CronExpr    string     `json:"cron_expr"`
	Timezone    string     `json:"timezone"`
	Prompt      string     `json:"prompt"`
	Enabled     bool       `json:"enabled"`
	LastRunAt   *time.Time `json:"last_run_at"`
	NextRunAt   *time.Time `json:"next_run_at"`
	RunCount    int        `json:"run_count"`
	LastStatus  string     `json:"last_status"`
	LastError   string     `json:"last_error"`
	Source      string     `json:"source,omitempty"` // 'template' (seeded by org/import) | 'runtime' (created via Canvas/API). Issue #24.
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// List returns all schedules for a workspace.
//
//	@Summary	List schedules for a workspace
//	@Tags		schedules
//	@Produce	json
//	@Param		id	path		string	true	"Workspace ID"
//	@Success	200	{array}		ScheduleResponse
//	@Failure	500	{object}	ErrorResponse
//	@Router		/workspaces/{id}/schedules [get]
//	@Security	BearerAuth && OrgSlugAuth
func (h *ScheduleHandler) List(c *gin.Context) {
	h.listVolume(c, c.Param("id"))
}

type CreateScheduleRequest struct {
	Name     string `json:"name"`
	CronExpr string `json:"cron_expr" binding:"required"`
	Timezone string `json:"timezone"`
	Prompt   string `json:"prompt" binding:"required"`
	Enabled  *bool  `json:"enabled"`
}

// Create adds a new schedule for a workspace.
//
//	@Summary	Create a schedule
//	@Tags		schedules
//	@Accept		json
//	@Produce	json
//	@Param		id		path		string					true	"Workspace ID"
//	@Param		body	body		CreateScheduleRequest	true	"Schedule fields"
//	@Success	201		{object}	CreateScheduleResponse
//	@Failure	400		{object}	ErrorResponse
//	@Failure	500		{object}	ErrorResponse
//	@Router		/workspaces/{id}/schedules [post]
//	@Security	BearerAuth && OrgSlugAuth
func (h *ScheduleHandler) Create(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var body CreateScheduleRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cron_expr and prompt are required"})
		return
	}

	// Strip CRLF from prompts — org-template files committed on Windows
	// inject \r\n, causing empty agent responses (issue #958).
	body.Prompt = strings.ReplaceAll(body.Prompt, "\r", "")

	if body.Timezone == "" {
		body.Timezone = "UTC"
	}

	// Per-workspace scheduler delivery (scheduler-as-trigger-plugin): a
	// workspace that has a schedule must run the molecule-scheduler trigger
	// daemon. Declare the plugin (idempotent; installs on the next boot/reconcile
	// so scheduling survives a restart) and arm the running daemon SYNCHRONOUSLY.
	// Post-P4b the schedule store is volume-only, so the arm must land before
	// createVolume forwards — a 2xx reload proves /internal/schedules is serving,
	// which closes the first-schedule race that the retired DB path used to
	// absorb. Non-fatal: a declaration hiccup is logged (createVolume's retry+503
	// covers a still-starting daemon), never blocking creation beyond the arm.
	if _, err := ensureAndArmSchedulerPluginSync(ctx, workspaceID); err != nil {
		log.Printf("Schedules.Create: ensure scheduler plugin for %s (non-fatal): %v", workspaceID, err)
	}

	// The runtime store validates cron + timezone + caps itself.
	h.createVolume(c, workspaceID, body)
}

type UpdateScheduleRequest struct {
	Name     *string `json:"name"`
	CronExpr *string `json:"cron_expr"`
	Timezone *string `json:"timezone"`
	Prompt   *string `json:"prompt"`
	Enabled  *bool   `json:"enabled"`
}

// Update modifies a schedule. Uses a fixed UPDATE with COALESCE so only
// provided fields are changed — no dynamic SQL construction.
//
//	@Summary	Update a schedule
//	@Tags		schedules
//	@Accept		json
//	@Produce	json
//	@Param		id			path		string					true	"Workspace ID"
//	@Param		scheduleId	path		string					true	"Schedule ID"
//	@Param		body		body		UpdateScheduleRequest	true	"Partial schedule fields (only provided keys are updated)"
//	@Success	200			{object}	ScheduleResponse
//	@Failure	400			{object}	ErrorResponse
//	@Failure	404			{object}	ErrorResponse
//	@Failure	500			{object}	ErrorResponse
//	@Router		/workspaces/{id}/schedules/{scheduleId} [patch]
//	@Security	BearerAuth && OrgSlugAuth
func (h *ScheduleHandler) Update(c *gin.Context) {
	scheduleID := c.Param("scheduleId")
	workspaceID := c.Param("id") // #113: bind to owning workspace to prevent IDOR
	var body UpdateScheduleRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON"})
		return
	}

	// Strip CRLF from prompt if provided (issue #958).
	if body.Prompt != nil {
		clean := strings.ReplaceAll(*body.Prompt, "\r", "")
		body.Prompt = &clean
	}

	// scheduleID is the grid entry name in the volume model.
	h.updateVolume(c, workspaceID, scheduleID, body)
}

// Delete removes a schedule.
//
//	@Summary	Delete a schedule
//	@Tags		schedules
//	@Produce	json
//	@Param		id			path		string	true	"Workspace ID"
//	@Param		scheduleId	path		string	true	"Schedule ID"
//	@Success	200			{object}	StatusResponse
//	@Failure	404			{object}	ErrorResponse
//	@Failure	500			{object}	ErrorResponse
//	@Router		/workspaces/{id}/schedules/{scheduleId} [delete]
//	@Security	BearerAuth && OrgSlugAuth
func (h *ScheduleHandler) Delete(c *gin.Context) {
	scheduleID := c.Param("scheduleId")
	workspaceID := c.Param("id") // #113: bind to owning workspace to prevent IDOR
	h.deleteVolume(c, workspaceID, scheduleID)
}

// RunNow manually fires a schedule immediately.
//
//	@Summary	Fire a schedule manually
//	@Tags		schedules
//	@Produce	json
//	@Param		id			path		string	true	"Workspace ID"
//	@Param		scheduleId	path		string	true	"Schedule ID"
//	@Success	200			{object}	RunNowResponse
//	@Failure	404			{object}	ErrorResponse
//	@Failure	500			{object}	ErrorResponse
//	@Router		/workspaces/{id}/schedules/{scheduleId}/run [post]
//	@Security	BearerAuth && OrgSlugAuth
func (h *ScheduleHandler) RunNow(c *gin.Context) {
	scheduleID := c.Param("scheduleId")
	workspaceID := c.Param("id")
	// The trigger daemon fires the turn as a self-scheduler system turn
	// (RFC §1.1) — the platform does not hand a prompt back for the client
	// to fire. Response carries fired_by:"daemon" so Canvas skips /a2a.
	h.runNowVolume(c, workspaceID, scheduleID)
}

// History returns recent runs for a schedule from activity_logs.
//
//	@Summary	Get past runs of a schedule
//	@Tags		schedules
//	@Produce	json
//	@Param		id			path		string	true	"Workspace ID"
//	@Param		scheduleId	path		string	true	"Schedule ID"
//	@Success	200			{array}		HistoryEntry
//	@Failure	500			{object}	ErrorResponse
//	@Router		/workspaces/{id}/schedules/{scheduleId}/history [get]
//	@Security	BearerAuth && OrgSlugAuth
func (h *ScheduleHandler) History(c *gin.Context) {
	scheduleID := c.Param("scheduleId")
	workspaceID := c.Param("id")
	// scheduleID is the grid entry name in the volume model; the runtime's
	// bounded run log is the source (P4b re-point — post-#4399 no core code
	// writes cron_run rows for these workspaces).
	h.historyVolume(c, workspaceID, scheduleID)
}

// ScheduleHealthResponse is the read-only health view of a schedule.
// It deliberately omits prompt and cron_expr so sensitive task content is
// never exposed to peer workspaces — only execution-state fields needed to
// detect silent cron failures are returned (issue #249).
type ScheduleHealthResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Enabled    bool       `json:"enabled"`
	LastRunAt  *time.Time `json:"last_run_at"`
	NextRunAt  *time.Time `json:"next_run_at"`
	RunCount   int        `json:"run_count"`
	LastStatus string     `json:"last_status"`
	LastError  string     `json:"last_error"`
}

// Health returns schedule health fields (last_run_at, last_status, run_count,
// etc.) for all schedules belonging to a workspace.
//
// Unlike GET /workspaces/:id/schedules (which requires the workspace's own
// bearer token), this endpoint is accessible to CanCommunicate peers — i.e.,
// any workspace in the same org hierarchy — so peer agents can detect silent
// cron failures without needing admin auth (issue #249).
//
// Auth rules (mirrors the A2A proxy pattern):
//   - X-Workspace-ID header is required to identify the caller.
//   - The Authorization bearer must authenticate the caller workspace, or be
//     a verified human admin/session credential. There is no tokenless legacy
//     or self-call exception.
//   - registry.CanCommunicate(callerID, workspaceID) must return true.
//   - System caller prefixes supplied through HTTP are rejected.
//   - Self-calls skip hierarchy checks only after bearer authentication.
//
// Prompt and cron_expr are intentionally absent from the response.
func (h *ScheduleHandler) Health(c *gin.Context) {
	workspaceID := c.Param("id")
	callerID := c.GetHeader("X-Workspace-ID")
	ctx := c.Request.Context()

	// Caller identity is mandatory — anonymous reads are not permitted.
	if callerID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "X-Workspace-ID header required"})
		return
	}
	if isSystemCaller(callerID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid caller ID"})
		return
	}

	// Validate the caller's own bearer token (Phase 30.5 contract).
	// Post-RFC#637: canvas users may read schedule health too.
	isCanvasUser, err := validateCallerToken(ctx, c, callerID)
	if err != nil {
		return // response already written with 401
	}

	// CanCommunicate gate — only peers in the org hierarchy may read health.
	// Canvas users (human operators) bypass this gate.
	if callerID != workspaceID && !isCanvasUser {
		if !registry.CanCommunicate(callerID, workspaceID) {
			log.Printf("ScheduleHealth: access denied %s → %s", callerID, workspaceID)
			c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
			return
		}
	}

	// Serve health from the runtime grid + the trigger daemon's health file.
	// Deliberately AFTER every auth gate above — the data source is the volume;
	// the caller identity + CanCommunicate contract is unchanged.
	h.healthVolume(c, workspaceID)
}

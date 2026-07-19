package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// Volume-authoritative schedule storage.
//
// Schedules are owned by each workspace's kind:trigger scheduler plugin: the
// grid lives on the workspace's persisted volume behind the runtime's
// `/internal/schedules*` API, and the trigger daemon fires it. Core no longer
// stores or fires schedules — it only proxies the Canvas/admin CRUD surface to
// that volume-backed runtime API. Every workspace that has schedules advertises
// the `scheduler` capability (heartbeat → runtimeOverrides) and runs the daemon;
// the legacy dual-path core-DB schedule backend was retired in P4b.

// scheduleForwardTimeout bounds a single forward to the workspace runtime.
const scheduleForwardTimeout = 15 * time.Second

// volumeSchedulerWorkspaceIDs returns the workspaces whose schedule surface is
// served by the runtime volume backend — those advertising the native
// `scheduler` capability. Stable (sorted) order so fan-out surfaces (webhook
// cron-poke, admin health) behave deterministically.
func volumeSchedulerWorkspaceIDs() []string {
	return runtimeOverrides.WorkspacesWithCapability("scheduler")
}

// volumeEntry is one schedule as the runtime's SDK-owned grid contract names its
// fields (contracts/schedule): `cron` (not core's `cron_expr`), definition only.
type volumeEntry struct {
	Name     string `json:"name"`
	Cron     string `json:"cron"`
	Timezone string `json:"timezone"`
	Prompt   string `json:"prompt"`
	Enabled  bool   `json:"enabled"`
	Source   string `json:"source,omitempty"`
}

// toScheduleResponse maps a volume grid entry to the stable Canvas shape. In the
// volume model the grid is name-keyed, so `id == name`; the bookkeeping columns
// the core table carried (last_run_at, run_count, last_status) are not part of
// the definition grid and are surfaced separately via the health/history
// endpoints, so they are left zero here. next_run_at is computed from the cron
// so the UI's "next run" stays populated.
func toScheduleResponse(workspaceID string, e volumeEntry) ScheduleResponse {
	resp := ScheduleResponse{
		ID:          e.Name,
		WorkspaceID: workspaceID,
		Name:        e.Name,
		CronExpr:    e.Cron,
		Timezone:    e.Timezone,
		Prompt:      e.Prompt,
		Enabled:     e.Enabled,
		Source:      e.Source,
	}
	if tz := e.Timezone; tz != "" {
		if next, err := computeNextRunSafe(e.Cron, tz); err == nil {
			resp.NextRunAt = &next
		}
	}
	return resp
}

// forwardScheduleAPI proxies one request to the workspace runtime's
// `/internal/schedules<subpath>` endpoint, using the same SSRF-safe client +
// inbound-secret bearer the chat-file forwards use. `subpath` must start with
// "/" or be empty. On a resolve/transport failure it writes the gin response and
// returns ok=false; otherwise it returns the runtime's status + body verbatim.
func (h *ScheduleHandler) forwardScheduleAPI(c *gin.Context, workspaceID, method, subpath string, body []byte) (status int, respBody []byte, ok bool) {
	ctx := c.Request.Context()
	wsURL, secret, resolved := resolveWorkspaceForwardCreds(c, ctx, workspaceID, "schedules")
	if !resolved {
		return 0, nil, false // gin response already written
	}
	target := strings.TrimRight(wsURL, "/") + "/internal/schedules" + subpath

	fctx, cancel := context.WithTimeout(ctx, scheduleForwardTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(fctx, method, target, reader)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build schedule request"})
		return 0, nil, false
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.forwardClient().Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "schedule backend unreachable"})
		return 0, nil, false
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, rb, true
}

// resolveScheduleFanoutTarget resolves the workspace URL + inbound secret for a
// quiet (non-gin) schedule forward — the webhook poke fan-out and the admin
// health aggregate, where one unreachable workspace must never fail the whole
// surface. Mirrors armSchedulerPlugin's resolution; callers log-and-skip on a
// non-nil error.
func resolveScheduleFanoutTarget(ctx context.Context, workspaceID string) (string, string, error) {
	var wsURL string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(url, '') FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&wsURL); err != nil {
		return "", "", fmt.Errorf("workspace lookup: %w", err)
	}
	if wsURL == "" {
		return "", "", errors.New("no callback URL (poll-mode / not registered)")
	}
	if err := isSafeURL(wsURL); err != nil {
		return "", "", fmt.Errorf("unsafe workspace URL rejected: %w", err)
	}
	secret, _, err := readOrLazyHealInboundSecret(ctx, workspaceID, "schedules-fanout")
	if err != nil {
		return "", "", fmt.Errorf("inbound secret unavailable: %w", err)
	}
	return wsURL, secret, nil
}

// fetchScheduleAPI performs one quiet forward to a resolved workspace runtime
// target's `/internal/schedules<subpath>` endpoint. Unlike forwardScheduleAPI
// it never touches a gin response — fan-out callers aggregate results and
// decide the surface themselves. A fresh SSRF-safe client per call matches the
// armSchedulerPlugin idiom.
func fetchScheduleAPI(ctx context.Context, wsURL, secret, method, subpath string, body []byte) (int, []byte, error) {
	target := strings.TrimRight(wsURL, "/") + "/internal/schedules" + subpath
	fctx, cancel := context.WithTimeout(ctx, scheduleForwardTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(fctx, method, target, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{
		Timeout: scheduleForwardTimeout,
		Transport: &http.Transport{
			DialContext:         safeDialer().DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, rb, nil
}

// listVolume serves List from the runtime grid.
func (h *ScheduleHandler) listVolume(c *gin.Context, workspaceID string) {
	status, body, ok := h.forwardScheduleAPI(c, workspaceID, http.MethodGet, "", nil)
	if !ok {
		return
	}
	if status != http.StatusOK {
		relayScheduleError(c, status, body)
		return
	}
	var payload struct {
		Schedules []volumeEntry `json:"schedules"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "malformed schedule grid from runtime"})
		return
	}
	out := make([]ScheduleResponse, 0, len(payload.Schedules))
	for _, e := range payload.Schedules {
		out = append(out, toScheduleResponse(workspaceID, e))
	}
	c.JSON(http.StatusOK, out)
}

// createVolume serves Create by POSTing a definition to the runtime grid.
func (h *ScheduleHandler) createVolume(c *gin.Context, workspaceID string, body CreateScheduleRequest) {
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	entry := volumeEntry{
		Name: body.Name, Cron: body.CronExpr, Timezone: body.Timezone,
		Prompt: body.Prompt, Enabled: enabled, Source: "runtime",
	}
	raw, _ := json.Marshal(entry)
	status, respBody, ok := h.forwardScheduleAPI(c, workspaceID, http.MethodPost, "", raw)
	if !ok {
		return
	}
	if status != http.StatusCreated {
		relayScheduleError(c, status, respBody)
		return
	}
	var created volumeEntry
	_ = json.Unmarshal(respBody, &created)
	out := gin.H{"id": created.Name, "status": "created"}
	if created.Timezone != "" {
		if next, err := computeNextRunSafe(created.Cron, created.Timezone); err == nil {
			out["next_run_at"] = next
		}
	}
	c.JSON(http.StatusCreated, out)
}

// updateVolume serves Update by PATCHing the name-keyed grid entry. The Canvas
// `scheduleId` path param is the entry name in the volume model.
func (h *ScheduleHandler) updateVolume(c *gin.Context, workspaceID, name string, body UpdateScheduleRequest) {
	patch := map[string]any{}
	if body.Name != nil {
		patch["name"] = *body.Name
	}
	if body.CronExpr != nil {
		patch["cron"] = *body.CronExpr
	}
	if body.Timezone != nil {
		patch["timezone"] = *body.Timezone
	}
	if body.Prompt != nil {
		patch["prompt"] = *body.Prompt
	}
	if body.Enabled != nil {
		patch["enabled"] = *body.Enabled
	}
	raw, _ := json.Marshal(patch)
	status, respBody, ok := h.forwardScheduleAPI(c, workspaceID, http.MethodPatch, "/"+urlPathEscape(name), raw)
	if !ok {
		return
	}
	if status != http.StatusOK {
		relayScheduleError(c, status, respBody)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "updated"})
}

// deleteVolume serves Delete against the name-keyed grid entry.
func (h *ScheduleHandler) deleteVolume(c *gin.Context, workspaceID, name string) {
	status, respBody, ok := h.forwardScheduleAPI(c, workspaceID, http.MethodDelete, "/"+urlPathEscape(name), nil)
	if !ok {
		return
	}
	if status != http.StatusOK {
		relayScheduleError(c, status, respBody)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// runNowVolume serves RunNow by enqueuing a poke on the runtime; the trigger
// daemon fires the turn as an autonomous `self-scheduler` self-turn (the RFC
// §1.1 correctness fix — NOT a frontend-fired `role:user` /a2a turn). The
// response carries `fired_by:"daemon"` so the Canvas client knows the platform
// already fired and must NOT double-fire via /a2a.
func (h *ScheduleHandler) runNowVolume(c *gin.Context, workspaceID, name string) {
	status, respBody, ok := h.forwardScheduleAPI(c, workspaceID, http.MethodPost, "/"+urlPathEscape(name)+"/run", nil)
	if !ok {
		return
	}
	if status != http.StatusAccepted {
		relayScheduleError(c, status, respBody)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":       "fired",
		"workspace_id": workspaceID,
		"fired_by":     "daemon",
	})
}

// scheduleHistoryLimit mirrors the legacy History query's `LIMIT 20` so the
// volume-proxied history surface returns the same window as the DB one.
const scheduleHistoryLimit = 20

// volumeHistoryRow is one run-log row exactly as the trigger daemon appends it
// (SDK templates/trigger/scheduler.py run_once → schedule-history.json).
type volumeHistoryRow struct {
	Name         string `json:"name"`
	At           string `json:"at"`
	ScheduledFor string `json:"scheduled_for"`
	Poked        bool   `json:"poked"`
	Status       string `json:"status"` // fired | deferred | unknown
}

// historyVolume serves History from the runtime's bounded run log
// (GET /internal/schedules/{name}/history — the per-name filter is a PATH
// segment; the runtime ignores query params). The daemon appends
// chronologically, so the Canvas newest-first contract means walking the log
// backwards, capped at scheduleHistoryLimit (parity with the legacy
// activity_logs query). The legacy HistoryEntry JSON shape is preserved
// field-for-field; the daemon's success value "fired" maps to the legacy
// success value "ok", while the newer daemon states (deferred/unknown) pass
// through — masking them as ok or error would misreport.
func (h *ScheduleHandler) historyVolume(c *gin.Context, workspaceID, name string) {
	status, body, ok := h.forwardScheduleAPI(c, workspaceID, http.MethodGet, "/"+urlPathEscape(name)+"/history", nil)
	if !ok {
		return
	}
	if status != http.StatusOK {
		relayScheduleError(c, status, body)
		return
	}
	var payload struct {
		History []volumeHistoryRow `json:"history"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "malformed schedule history from runtime"})
		return
	}
	entries := make([]HistoryEntry, 0, len(payload.History))
	for i := len(payload.History) - 1; i >= 0 && len(entries) < scheduleHistoryLimit; i-- {
		row := payload.History[i]
		ts, err := time.Parse(time.RFC3339, row.At)
		if err != nil {
			continue // unparseable row — skip rather than fabricate a timestamp
		}
		st := row.Status
		if st == "fired" {
			st = "ok"
		}
		req, _ := json.Marshal(map[string]any{
			"schedule_id":   name,
			"scheduled_for": row.ScheduledFor,
			"poked":         row.Poked,
		})
		entries = append(entries, HistoryEntry{
			Timestamp:   ts,
			DurationMs:  nil, // the daemon's run log does not measure turn duration
			Status:      &st,
			ErrorDetail: "",
			Request:     req,
		})
	}
	c.JSON(http.StatusOK, entries)
}

// volumeHealthPayload is the daemon-health surface the runtime serves
// (GET /internal/schedules/health): the trigger daemon's heartbeat file, or the
// pre-first-tick fallback {"last_tick":null,"armed":N,"errors":{}}.
type volumeHealthPayload struct {
	LastTick *string           `json:"last_tick"`
	Armed    int               `json:"armed"`
	Errors   map[string]string `json:"errors"`
}

// healthVolume serves the peer Health view from the runtime grid + the trigger
// daemon's health file, mapped into the legacy ScheduleHealthResponse shape so
// the JSON contract — field names AND the no-prompt/no-cron_expr redaction
// (issue #249) — is unchanged. Callers MUST have already passed the caller/
// CanCommunicate gates; this function only changes the data source, never the
// access contract. Bookkeeping the volume model does not surface per schedule
// (last_run_at, run_count) is left zero — the same convention as
// toScheduleResponse. last_status carries what a peer needs to detect a silent
// failure: "error" (+ last_error) when the daemon reports the schedule broken,
// "ok" while the daemon is ticking, "" before its first tick.
func (h *ScheduleHandler) healthVolume(c *gin.Context, workspaceID string) {
	status, body, ok := h.forwardScheduleAPI(c, workspaceID, http.MethodGet, "", nil)
	if !ok {
		return
	}
	if status != http.StatusOK {
		relayScheduleError(c, status, body)
		return
	}
	var grid struct {
		Schedules []volumeEntry `json:"schedules"`
	}
	if err := json.Unmarshal(body, &grid); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "malformed schedule grid from runtime"})
		return
	}
	hstatus, hbody, ok := h.forwardScheduleAPI(c, workspaceID, http.MethodGet, "/health", nil)
	if !ok {
		return
	}
	if hstatus != http.StatusOK {
		relayScheduleError(c, hstatus, hbody)
		return
	}
	var health volumeHealthPayload
	if err := json.Unmarshal(hbody, &health); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "malformed schedule health from runtime"})
		return
	}
	out := make([]ScheduleHealthResponse, 0, len(grid.Schedules))
	for _, e := range grid.Schedules {
		s := ScheduleHealthResponse{ID: e.Name, Name: e.Name, Enabled: e.Enabled}
		if next, err := computeNextRunSafe(e.Cron, e.Timezone); err == nil {
			s.NextRunAt = &next
		}
		if reason, broken := health.Errors[e.Name]; broken {
			s.LastStatus, s.LastError = "error", reason
		} else if health.LastTick != nil {
			s.LastStatus = "ok"
		}
		out = append(out, s)
	}
	c.JSON(http.StatusOK, out)
}

// relayScheduleError forwards a non-2xx runtime response, mapping the runtime's
// JSON `{"error":...}` to the core error shape and preserving the status code.
func relayScheduleError(c *gin.Context, status int, body []byte) {
	msg := "schedule backend error"
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error != "" {
		msg = parsed.Error
	}
	// Normalize to statuses the Canvas client already handles.
	switch status {
	case http.StatusNotFound:
		c.JSON(http.StatusNotFound, gin.H{"error": msg})
	case http.StatusBadRequest, http.StatusConflict:
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
	case http.StatusUnauthorized, http.StatusForbidden:
		c.JSON(http.StatusBadGateway, gin.H{"error": "schedule backend auth failed"})
	default:
		c.JSON(http.StatusBadGateway, gin.H{"error": msg})
	}
}

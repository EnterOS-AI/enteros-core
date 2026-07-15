package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// P3-live: volume-authoritative schedule storage.
//
// When a workspace runs the kind:trigger scheduler plugin it advertises the
// `scheduler` capability (heartbeat → runtimeOverrides). For such a workspace
// the schedule *grid* lives on its persisted volume and is owned by the trigger
// daemon; the core `workspace_schedules` table is NOT the source of truth. Canvas
// CRUD must therefore be served by the workspace runtime's volume-backed
// `/internal/schedules*` API rather than the core DB — otherwise a Canvas edit
// writes a row the daemon never reads (a silent no-op).
//
// This is the destination shape of RFC Option A. It is intentionally SELF-DARK:
// `scheduleBackendIsVolume` is false for every workspace that does not report the
// `scheduler` capability, so until the trigger-plugin runtime is actually
// deployed the entire proxy path is unreachable and core keeps serving the DB.
// The kill-switch env forces the legacy DB path even for native workspaces, an
// operational escape hatch during the staged cutover.

// scheduleProxyKillEnv, when set truthy, forces the legacy core-DB schedule path
// even for workspaces that advertise a native scheduler.
const scheduleProxyKillEnv = "SCHEDULE_VOLUME_PROXY_DISABLED"

// scheduleForwardTimeout bounds a single forward to the workspace runtime.
const scheduleForwardTimeout = 15 * time.Second

// scheduleBackendIsVolume reports whether schedule CRUD for this workspace must
// be proxied to the runtime's volume-backed API rather than served from the core
// workspace_schedules table. True exactly when the workspace advertises the
// native scheduler capability AND the kill-switch is off.
func scheduleBackendIsVolume(workspaceID string) bool {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv(scheduleProxyKillEnv))); v == "1" || v == "true" || v == "yes" {
		return false
	}
	return ProvidesNativeScheduler(workspaceID)
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

// MigrateToVolume copies a workspace's user-created (source='runtime') schedules
// from the core workspace_schedules table into its volume grid via the runtime
// API. Template-source schedules are NOT copied — they are re-seeded on the
// volume by the org-template reconcile channel, so copying them here would
// duplicate. Idempotent: entries whose name already exists on the volume are
// skipped, so re-running (or running before every workspace is cut over) never
// double-writes or errors. This is the DB→volume data step of the Option-A
// cutover; run it per workspace once its trigger plugin is live, before core
// stops writing the table (P4).
//
//	@Summary	Migrate a workspace's schedules from the core DB to its volume
//	@Tags		schedules
//	@Produce	json
//	@Param		id	path	string	true	"Workspace ID"
//	@Success	200	{object}	map[string]int
//	@Router		/admin/workspaces/{id}/schedules/migrate-to-volume [post]
func (h *ScheduleHandler) MigrateToVolume(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	if !ProvidesNativeScheduler(workspaceID) {
		c.JSON(http.StatusConflict, gin.H{
			"error": "workspace does not advertise a native scheduler; nothing to migrate to (install the trigger plugin first)",
		})
		return
	}

	// Existing volume names — for idempotency, we never re-create these.
	status, body, ok := h.forwardScheduleAPI(c, workspaceID, http.MethodGet, "", nil)
	if !ok {
		return
	}
	if status != http.StatusOK {
		relayScheduleError(c, status, body)
		return
	}
	var existing struct {
		Schedules []volumeEntry `json:"schedules"`
	}
	_ = json.Unmarshal(body, &existing)
	onVolume := make(map[string]bool, len(existing.Schedules))
	for _, e := range existing.Schedules {
		onVolume[e.Name] = true
	}

	rows, err := db.DB.QueryContext(ctx, `
		SELECT name, cron_expr, timezone, prompt, enabled
		FROM workspace_schedules
		WHERE workspace_id = $1 AND source = 'runtime'
		ORDER BY created_at ASC
	`, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read schedules"})
		return
	}
	defer rows.Close()

	migrated, skipped, failed := 0, 0, 0
	for rows.Next() {
		var e volumeEntry
		if err := rows.Scan(&e.Name, &e.Cron, &e.Timezone, &e.Prompt, &e.Enabled); err != nil {
			failed++
			continue
		}
		if onVolume[e.Name] {
			skipped++
			continue
		}
		e.Source = "runtime"
		raw, _ := json.Marshal(e)
		st, _, fok := h.forwardScheduleAPI(c, workspaceID, http.MethodPost, "", raw)
		if !fok {
			return // forward wrote the error response
		}
		if st == http.StatusCreated {
			migrated++
			onVolume[e.Name] = true
		} else {
			failed++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"workspace_id": workspaceID,
		"migrated":     migrated,
		"skipped":      skipped, // already present on the volume (idempotent)
		"failed":       failed,
	})
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

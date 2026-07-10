package handlers

// boot_event.go — POST /workspaces/:id/boot-event
//
// Ingestion path for the "Enter OS" workspace boot sequence. The runtime
// walks its cold-boot checklist while a workspace is `provisioning`
// (provision compute → start runtime → wire transport → install plugins →
// load identity → connect management MCP → enumerate tools → go online) and
// POSTs one BOOT_STEP per phase transition here. The server does no domain
// work of its own — it validates the payload and fans it out to the canvas
// over the EXISTING broadcaster → WS hub, where BootSequenceScreen renders
// the per-step keycap animation.
//
// Presentation-only + broadcast-only: BOOT_STEP is NOT recorded in
// structure_events (BroadcastOnly, not RecordAndBroadcast). A mid-boot page
// reload re-derives the boot screen from the workspace's `provisioning`
// status; there is no durable boot-step history to persist. The terminal
// signal that flips the canvas out of the boot screen is the existing
// WORKSPACE_ONLINE event (emitted when the workspace row goes `online`),
// NOT a BOOT_STEP — this handler never mutates workspace status.
//
// Wire payload — the shape the runtime emitter MUST send (mirrors the
// EventBootStep doc comment in events/types.go and the canvas store's
// BOOT_STEP handler):
//
//	{
//	  "step":    3,          // 1-based index of this step (>= 1)
//	  "total":   8,          // total steps in the boot plan (>= step)
//	  "key":     "MCP",      // short keycap legend, e.g. PWR / RT / MCP
//	  "label":   "Connect management MCP", // human step name
//	  "status":  "running",  // one of: running | ok | failed
//	  "message": "launching npx @molecule-ai/mcp-server…" // optional log line;
//	                         // on status=failed this is the red failure reason
//	}
//
// Auth: mounted under the wsAuth group (WorkspaceAuth middleware), the same
// bearer-token trust boundary the other runtime callbacks use
// (POST /activity, POST /notify). The runtime holds the workspace's own
// scoped boot/bearer token; WorkspaceAuth binds it to :id, so a workspace
// can only post boot steps for itself.

import (
	"net/http"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// bootStepStatuses is the closed set of BOOT_STEP.status values. Keeping it
// a map (not a slice scan) makes the validation O(1) and the allowed set
// self-documenting. Mirrors the canvas store's accepted values.
var bootStepStatuses = map[string]struct{}{
	"running": {},
	"ok":      {},
	"failed":  {},
}

// maxBootKeyLen caps the keycap legend so a runaway runtime string can't
// blow up the keycap layout on the canvas. 8 chars fits the widest planned
// legend ("TOOL") with headroom.
const maxBootKeyLen = 8

// BootEventHandler ingests runtime-reported boot steps and rebroadcasts
// them to the canvas. Stateless apart from the broadcaster — same shape as
// ChatSessionHandler.
type BootEventHandler struct {
	broadcaster events.EventEmitter
}

// NewBootEventHandler wires the broadcaster used to fan BOOT_STEP out to the
// canvas. Tests inject a capture-only stub via the same constructor.
func NewBootEventHandler(broadcaster events.EventEmitter) *BootEventHandler {
	return &BootEventHandler{broadcaster: broadcaster}
}

// bootStepBody is the request/broadcast wire shape. Step/Total are pointers
// so we can distinguish an omitted field (nil → 400) from a legitimate
// zero — a 0-based or missing index is a runtime bug we want to reject
// loudly rather than render as a broken "0/8".
type bootStepBody struct {
	Step    *int   `json:"step"`
	Total   *int   `json:"total"`
	Key     string `json:"key" binding:"required"`
	Label   string `json:"label" binding:"required"`
	Status  string `json:"status" binding:"required"`
	Message string `json:"message,omitempty"`
}

// Report handles POST /workspaces/:id/boot-event — the runtime reports one
// boot step; the server validates it and BroadcastOnly's a BOOT_STEP event
// to the canvas. Returns 200 {"status":"broadcast"} on success.
//
// Error surface (all fail loud — a silently-dropped boot step leaves the
// canvas keycap stuck mid-press):
//   - 400 if :id isn't a UUID (trust boundary)
//   - 400 if the body is malformed / a required field is missing
//   - 400 if status isn't one of running|ok|failed
//   - 400 if step/total are out of range (step >= 1, total >= step)
func (h *BootEventHandler) Report(c *gin.Context) {
	workspaceID := c.Param("id")
	if _, err := uuid.Parse(workspaceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace id must be a UUID"})
		return
	}

	var body bootStepBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid boot-event body: key, label and status are required"})
		return
	}

	if _, ok := bootStepStatuses[body.Status]; !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be one of: running, ok, failed"})
		return
	}
	if body.Step == nil || body.Total == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "step and total are required"})
		return
	}
	if *body.Step < 1 || *body.Total < *body.Step {
		c.JSON(http.StatusBadRequest, gin.H{"error": "step must be >= 1 and total must be >= step"})
		return
	}
	if len(body.Key) > maxBootKeyLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key must be <= 8 characters"})
		return
	}

	// Broadcast-only: presentation event, no structure_events row. The
	// nil guard mirrors ChatSessionHandler — a handler constructed without
	// a broadcaster (unlikely in production wiring) is a no-op rather than
	// a panic.
	if h.broadcaster != nil {
		h.broadcaster.BroadcastOnly(workspaceID, string(events.EventBootStep), map[string]interface{}{
			"workspace_id": workspaceID,
			"step":         *body.Step,
			"total":        *body.Total,
			"key":          body.Key,
			"label":        body.Label,
			"status":       body.Status,
			"message":      body.Message,
		})
	}

	c.JSON(http.StatusOK, gin.H{"status": "broadcast"})
}

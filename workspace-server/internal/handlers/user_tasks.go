package handlers

import (
	"log"
	"net/http"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"github.com/gin-gonic/gin"
)

// UserTasksHandler serves the "user tasks" primitive — structured asks an
// agent raises for the human user (e.g. "Review the draft", "Provide the API
// key"). It mirrors ApprovalsHandler but resolving a task has no enforcement
// effect; it is a worklist signal. See docs/design/rfc-user-tasks.md.
type UserTasksHandler struct {
	broadcaster *events.Broadcaster
}

func NewUserTasksHandler(b *events.Broadcaster) *UserTasksHandler {
	return &UserTasksHandler{broadcaster: b}
}

// Create handles POST /workspaces/:id/user-tasks — an agent raises an ask.
func (h *UserTasksHandler) Create(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var body struct {
		Title  string `json:"title" binding:"required"`
		Detail string `json:"detail"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	var detail interface{}
	if body.Detail != "" {
		detail = body.Detail
	}

	var taskID string
	err := db.DB.QueryRowContext(ctx, `
		INSERT INTO user_tasks (workspace_id, title, detail)
		VALUES ($1, $2, $3)
		RETURNING id
	`, workspaceID, body.Title, detail).Scan(&taskID)
	if err != nil {
		log.Printf("Create user task error workspace=%s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create user task"})
		return
	}

	if err := h.broadcaster.RecordAndBroadcast(ctx, string(events.EventUserTaskRequested), workspaceID, map[string]interface{}{
		"user_task_id": taskID,
		"title":        body.Title,
	}); err != nil {
		log.Printf("user_tasks: failed to broadcast user task requested: %v", err)
	}

	c.JSON(http.StatusCreated, gin.H{"user_task_id": taskID, "status": "pending"})
}

// ListAll handles GET /user-tasks/pending — all pending asks across the org
// (for the concierge Tasks tab). Cross-workspace, so AdminAuth-gated.
func (h *UserTasksHandler) ListAll(c *gin.Context) {
	ctx := c.Request.Context()

	rows, err := db.DB.QueryContext(ctx, `
		SELECT t.id, t.workspace_id, w.name, t.title, t.detail, t.status, t.created_at
		FROM user_tasks t
		JOIN workspaces w ON w.id = t.workspace_id
		WHERE t.status = 'pending'
		ORDER BY t.created_at DESC
		LIMIT 50
	`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	tasks := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, wsID, wsName, title, status, createdAt string
		var detail *string
		if rows.Scan(&id, &wsID, &wsName, &title, &detail, &status, &createdAt) != nil {
			continue
		}
		tasks = append(tasks, map[string]interface{}{
			"id":             id,
			"workspace_id":   wsID,
			"workspace_name": wsName,
			"title":          title,
			"detail":         detail,
			"status":         status,
			"created_at":     createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		log.Printf("ListAll user tasks rows.Err: %v", err)
	}

	c.JSON(http.StatusOK, tasks)
}

// Resolve handles POST /workspaces/:id/user-tasks/:taskId/resolve — the user
// marks an ask done or dismissed.
func (h *UserTasksHandler) Resolve(c *gin.Context) {
	workspaceID := c.Param("id")
	taskID := c.Param("taskId")
	ctx := c.Request.Context()

	var body struct {
		Status     string `json:"status" binding:"required"`
		ResolvedBy string `json:"resolved_by"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if body.Status != "done" && body.Status != "dismissed" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be 'done' or 'dismissed'"})
		return
	}

	resolvedBy := body.ResolvedBy
	if resolvedBy == "" {
		resolvedBy = "human"
	}

	result, err := db.DB.ExecContext(ctx, `
		UPDATE user_tasks
		SET status = $1, resolved_at = now(), resolved_by = $2
		WHERE id = $3 AND workspace_id = $4 AND status = 'pending'
	`, body.Status, resolvedBy, taskID, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update"})
		return
	}

	rows, err := result.RowsAffected()
	if err != nil {
		log.Printf("User task resolve RowsAffected error task=%s workspace=%s: %v", taskID, workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update"})
		return
	}
	if rows == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "user task not found or already resolved"})
		return
	}

	if err := h.broadcaster.RecordAndBroadcast(ctx, string(events.EventUserTaskResolved), workspaceID, map[string]interface{}{
		"user_task_id": taskID,
		"status":       body.Status,
		"resolved_by":  resolvedBy,
	}); err != nil {
		log.Printf("user_tasks: failed to broadcast user task resolved: %v", err)
	}

	c.JSON(http.StatusOK, gin.H{"status": body.Status, "user_task_id": taskID})
}

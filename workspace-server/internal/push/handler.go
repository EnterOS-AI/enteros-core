package push

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Handler exposes HTTP endpoints for push-token management.
type Handler struct {
	repo *Repo
}

// NewHandler creates a push-token HTTP handler.
func NewHandler(repo *Repo) *Handler {
	return &Handler{repo: repo}
}

// RegisterRoutes mounts push-token routes on the given router group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/push-tokens", h.Create)
	rg.DELETE("/push-tokens", h.Delete)
}

// Create handles POST /push-tokens.
// Body: { "token": "ExponentPushToken[xxx]", "platform": "ios" | "android" }
func (h *Handler) Create(c *gin.Context) {
	workspaceID := c.Param("id")
	if _, err := uuid.Parse(workspaceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace id"})
		return
	}

	var body struct {
		Token    string `json:"token" binding:"required"`
		Platform string `json:"platform" binding:"required,oneof=ios android"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.repo.SaveToken(c.Request.Context(), workspaceID, body.Token, body.Platform); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save token"})
		return
	}

	c.Status(http.StatusNoContent)
}

// Delete handles DELETE /push-tokens.
// Body: { "token": "ExponentPushToken[xxx]" }
func (h *Handler) Delete(c *gin.Context) {
	workspaceID := c.Param("id")
	if _, err := uuid.Parse(workspaceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace id"})
		return
	}

	var body struct {
		Token string `json:"token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.repo.DeleteToken(c.Request.Context(), workspaceID, body.Token); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete token"})
		return
	}

	c.Status(http.StatusNoContent)
}

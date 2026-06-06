package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestInstallPlatformAgent_BadJSON rejects a payload missing the required id
// before touching the DB (binding:"required" on ID).
func TestInstallPlatformAgent_BadJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/org/platform-agent",
		bytes.NewBufferString(`{"name":"Org Concierge"}`)) // no id
	c.Request.Header.Set("Content-Type", "application/json")

	InstallPlatformAgent(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("missing id: expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The schedules/chat-files 422 is decided by the TARGET workspace's own row.
// On 2026-11-13 (reno-stars) the customer read "poll mode has no callback URL"
// as a judgement about the CALLER's (workspace-token) registration, because
// the message named neither the workspace nor the delivery mode it judged.
// The 422 must say which workspace, which delivery_mode, and that it is the
// target's own registration that decides.
func TestResolveWorkspaceForwardCreds_422NamesTargetAndMode(t *testing.T) {
	mock := setupTestDB(t)
	const wsID = "ca17a092-fecb-4533-bbce-48055a757d13"
	expectURLAndMode(mock, wsID, "", "poll")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/schedules", nil)

	if _, _, ok := resolveWorkspaceForwardCreds(c, c.Request.Context(), wsID, "schedules"); ok {
		t.Fatal("empty url + poll must not resolve")
	}
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	msg, _ := resp["error"].(string)
	for _, want := range []string{wsID, "delivery_mode=poll", "target workspace"} {
		if !strings.Contains(msg, want) {
			t.Errorf("422 error must name %q, got: %s", want, msg)
		}
	}
	if !strings.HasPrefix(msg, "workspace has no callback URL — schedules requires push-mode") {
		t.Errorf("422 error must keep its existing prefix for callers that match on it, got: %s", msg)
	}
	if resp["workspace_id"] != wsID || resp["delivery_mode"] != "poll" {
		t.Errorf("workspace_id/delivery_mode fields = %v/%v, want %s/poll", resp["workspace_id"], resp["delivery_mode"], wsID)
	}
	if detail, _ := resp["detail"].(string); !strings.Contains(detail, "not by the caller's credential") {
		t.Errorf("detail must say the caller's credential is not what decides, got: %s", detail)
	}
}

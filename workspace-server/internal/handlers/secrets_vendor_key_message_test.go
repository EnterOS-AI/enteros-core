package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// The vendor-key guard (platformManagedLLMModeForWorkspace) is correct to refuse
// a vendor key on a platform-billed model. What it did NOT do was say WHY in
// terms a customer can act on. Live 2026-11-13 (reno-stars): a claude-code
// workspace on MODEL=anthropic/claude-sonnet-4-6 had CLAUDE_CODE_OAUTH_TOKEN
// refused with "direct vendor key writes are blocked for platform-managed
// workspaces …", and the customer found by trial and error that the
// subscription form (sonnet/opus/haiku) was what the key needs. The refusal
// must name the MODEL value it judged, say the slash form means platform
// billing, and name the subscription form + key that would be accepted.

func setVendorKeyForModel(t *testing.T, runtime, model, key string) (int, map[string]interface{}) {
	t.Helper()
	mock := setupTestDB(t)
	setupTestRedis(t)
	const wsID = "33333333-3333-3333-3333-333333333333"

	mock.ExpectQuery(`SELECT runtime FROM workspaces WHERE id = \$1`).WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow(runtime))
	rows := sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"})
	if model != "" {
		rows.AddRow("MODEL", []byte(model), 0)
	}
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1`).
		WithArgs(wsID).WillReturnRows(rows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	body := `{"key":"` + key + `","value":"sk-test"}`
	c.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/secrets", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	NewSecretsHandler(nil).Set(c)

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, w.Body.String())
	}
	return w.Code, resp
}

func TestSecretsSet_VendorKeyOnSlashModel_ExplainsBillingAndSubscriptionForm(t *testing.T) {
	code, resp := setVendorKeyForModel(t, "claude-code", "anthropic/claude-sonnet-4-6", "CLAUDE_CODE_OAUTH_TOKEN")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (the guard itself is unchanged): %v", code, resp)
	}
	msg, _ := resp["error"].(string)
	for _, want := range []string{
		`MODEL="anthropic/claude-sonnet-4-6"`, // names the value it judged
		"platform billing",                    // says what the slash form means
		"sonnet", "opus", "haiku",             // the subscription form
		"CLAUDE_CODE_OAUTH_TOKEN", // the key that form takes
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message is missing %q:\n%s", want, msg)
		}
	}
	if resp["model"] != "anthropic/claude-sonnet-4-6" {
		t.Errorf("model = %v, want the judged MODEL value", resp["model"])
	}
	if resp["code"] != "VENDOR_KEY_ON_PLATFORM_MODEL" {
		t.Errorf("code = %v, want VENDOR_KEY_ON_PLATFORM_MODEL", resp["code"])
	}
}

func TestSecretsSet_VendorKeyWithNoModel_SaysModelIsUnset(t *testing.T) {
	code, resp := setVendorKeyForModel(t, "claude-code", "", "CLAUDE_CODE_OAUTH_TOKEN")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %v", code, resp)
	}
	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, "MODEL is not set") {
		t.Errorf("an unset MODEL must be named as the reason, got:\n%s", msg)
	}
	if !strings.Contains(msg, "sonnet") || !strings.Contains(msg, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("claude-code refusal must still name the subscription form, got:\n%s", msg)
	}
}

func TestSecretsSet_VendorKeyOnSubscriptionModel_StillAllowedPastGuard(t *testing.T) {
	// Guard behaviour is unchanged: sonnet + CLAUDE_CODE_OAUTH_TOKEN derives to
	// anthropic-oauth (BYOK), so the write passes the vendor-key guard.
	m, err := providerRegistry()
	if err != nil {
		t.Fatal(err)
	}
	p, err := m.DeriveProvider("claude-code", "sonnet", []string{"CLAUDE_CODE_OAUTH_TOKEN"})
	if err != nil || p.IsPlatform() {
		t.Fatalf("sonnet must derive to a non-platform provider, got %q platform=%v err=%v", p.Name, p.IsPlatform(), err)
	}
}

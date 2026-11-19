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
	if resp["code"] != "VENDOR_KEY_MODEL_UNSET" {
		t.Errorf("code = %v, want VENDOR_KEY_MODEL_UNSET (an unset MODEL is not a platform model)", resp["code"])
	}
}

// On hermes the provider/model slash form is NOT the platform form in general:
// anthropic/claude-sonnet-4-6 derives to byok-anthropic there, and the bare
// claude-sonnet-4-6 is unregistered. The refusal for a hermes workspace on a
// platform model must therefore neither claim "slash form = platform billing"
// nor steer to claude-code's bare ids; it must name what the registry accepts
// for the supplied key ON HERMES.
func TestSecretsSet_VendorKeyOnHermesPlatformModel_AdviceComesFromRegistry(t *testing.T) {
	code, resp := setVendorKeyForModel(t, "hermes", "minimax/MiniMax-M2.7", "ANTHROPIC_API_KEY")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %v", code, resp)
	}
	msg, _ := resp["error"].(string)
	for _, bad := range []string{"slash form means platform billing", "claude-sonnet-4-6", "sonnet, opus or haiku"} {
		if strings.Contains(msg, bad) {
			t.Errorf("hermes refusal must not contain claude-code-only advice %q:\n%s", bad, msg)
		}
	}
	for _, want := range []string{`MODEL="minimax/MiniMax-M2.7"`, `runtime "hermes"`, "^anthropic/", "ANTHROPIC_API_KEY"} {
		if !strings.Contains(msg, want) {
			t.Errorf("hermes refusal is missing %q:\n%s", want, msg)
		}
	}
	// The advice must be TRUE: a model matching the named pattern has to derive
	// to a non-platform provider on hermes for the supplied key.
	m, err := providerRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if p, err := m.DeriveProvider("hermes", "anthropic/claude-sonnet-4-6", []string{"ANTHROPIC_API_KEY"}); err != nil || p.IsPlatform() {
		t.Fatalf("advice premise broken: anthropic/claude-sonnet-4-6 on hermes -> %q platform=%v err=%v", p.Name, p.IsPlatform(), err)
	}
	if resp["code"] != "VENDOR_KEY_ON_PLATFORM_MODEL" {
		t.Errorf("code = %v, want VENDOR_KEY_ON_PLATFORM_MODEL", resp["code"])
	}
}

func TestSecretsSet_VendorKeyNoProviderOnRuntime_SaysSo(t *testing.T) {
	// hermes has no provider that takes CLAUDE_CODE_OAUTH_TOKEN.
	_, resp := setVendorKeyForModel(t, "hermes", "minimax/MiniMax-M2.7", "CLAUDE_CODE_OAUTH_TOKEN")
	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, "no bring-your-own-key provider takes CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("must say hermes has no provider for that key, got:\n%s", msg)
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

// The CREATE path cannot read the MODEL from storage (it persists after
// commit), so its refusal must be built from the create payload — otherwise a
// create naming a platform model would be told "MODEL is not set".
func TestWorkspaceCreate_VendorKeyOnPlatformModel_MessageNamesPayloadModel(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO workspaces").WillReturnResult(sqlmock.NewResult(0, 1))
	// The guard's verdict still comes from the stored-state read (unchanged);
	// inside the uncommitted create it sees no runtime row and no MODEL.
	mock.ExpectQuery(`SELECT runtime FROM workspaces WHERE id = \$1`).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	mock.ExpectRollback()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"name":"Platform Model Agent","runtime":"claude-code","model":"anthropic/claude-sonnet-4-6","secrets":{"CLAUDE_CODE_OAUTH_TOKEN":"sk-test"}}`
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	msg, _ := resp["error"].(string)
	if strings.Contains(msg, "MODEL is not set") {
		t.Errorf("create refusal must not claim MODEL is unset when the payload named one:\n%s", msg)
	}
	for _, want := range []string{`MODEL="anthropic/claude-sonnet-4-6"`, "platform billing", "sonnet", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if !strings.Contains(msg, want) {
			t.Errorf("create refusal is missing %q:\n%s", want, msg)
		}
	}
	if resp["code"] != "VENDOR_KEY_ON_PLATFORM_MODEL" || resp["model"] != "anthropic/claude-sonnet-4-6" {
		t.Errorf("code/model = %v/%v, want VENDOR_KEY_ON_PLATFORM_MODEL/anthropic/claude-sonnet-4-6", resp["code"], resp["model"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// Every MODEL id the refusal advises must actually be accepted for the key:
// run each advised id, on every registered runtime and for every vendor key the
// guard blocks, back through DeriveProvider and require a NON-platform
// provider. (Pattern-only options name a regex, not an id, and are checked by
// the hermes test above.) A registry change that makes the advice false fails
// here instead of misleading a customer.
func TestByokAdvice_EveryAdvisedModelDerivesToANonPlatformProvider(t *testing.T) {
	m, err := providerRegistry()
	if err != nil || m == nil {
		t.Fatalf("provider registry: %v", err)
	}
	checked := 0
	for runtime := range m.Runtimes {
		for key := range platformManagedDirectLLMBypassKeys {
			opts, _ := byokOptionsForKey(m, runtime, key)
			for _, o := range opts {
				for _, model := range o.models {
					p, err := m.DeriveProvider(runtime, model, []string{key})
					if err != nil || p.IsPlatform() {
						t.Errorf("advice is false: runtime=%s key=%s advises MODEL=%q (%s) but DeriveProvider -> %q platform=%v err=%v",
							runtime, key, model, o.provider, p.Name, p.IsPlatform(), err)
					}
					checked++
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no advised model ids were checked — the table is vacuous")
	}
	t.Logf("checked %d advised (runtime, key, model) triples", checked)
}

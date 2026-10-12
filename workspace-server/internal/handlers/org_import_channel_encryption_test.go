package handlers

import (
	"database/sql/driver"
	"encoding/json"
	"strings"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/channels"
	channelscrypto "git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/crypto"
	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

type capturedImportedChannelConfig struct {
	config map[string]interface{}
}

func (c *capturedImportedChannelConfig) Match(value driver.Value) bool {
	var raw []byte
	switch value := value.(type) {
	case string:
		raw = []byte(value)
	case []byte:
		raw = value
	default:
		return false
	}
	var config map[string]interface{}
	if err := json.Unmarshal(raw, &config); err != nil {
		return false
	}
	c.config = config
	return true
}

func TestCreateWorkspaceTree_EncryptsImportedChannelSecrets(t *testing.T) {
	t.Setenv("SECRETS_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	channelscrypto.ResetForTesting()
	channelscrypto.Init()
	t.Cleanup(channelscrypto.ResetForTesting)

	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	workspace := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	handler := &OrgHandler{workspace: workspace, broadcaster: broadcaster}

	mock.ExpectQuery(`INSERT INTO workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-import-channel"))

	captured := &capturedImportedChannelConfig{}
	mock.ExpectExec(`INSERT INTO workspace_channels`).
		WithArgs(sqlmock.AnyArg(), "slack", captured, true, `["U123"]`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	plain := map[string]string{
		"bot_token":      "xoxb-imported-bot-secret",
		"webhook_secret": "imported-webhook-secret",
		"webhook_url":    "https://hooks.slack.com/services/T/B/imported-secret",
		"verify_token":   "imported-verify-token",
		"signing_secret": "imported-signing-secret",
	}
	config := map[string]string{
		"bot_token":      plain["bot_token"],
		"webhook_secret": plain["webhook_secret"],
		"webhook_url":    plain["webhook_url"],
		"verify_token":   plain["verify_token"],
		"signing_secret": plain["signing_secret"],
		"channel_id":     "C-IMPORTED",
		"username":       "Imported Agent",
	}
	ws := OrgWorkspace{
		Name:    "Imported Agent",
		Runtime: "claude-code",
		Model:   "anthropic:claude-sonnet-4-6",
		Channels: []OrgChannel{{
			Type:         "slack",
			Config:       config,
			AllowedUsers: []string{"U123"},
		}},
	}
	results := []map[string]interface{}{}
	if err := handler.createWorkspaceTree(ws, nil, 0, 0, 0, 0, OrgDefaults{}, "", &results, make(chan struct{}, 1)); err != nil {
		t.Fatalf("createWorkspaceTree: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("org import SQL expectations: %v", err)
	}
	if captured.config == nil {
		t.Fatal("workspace_channels insert did not capture channel_config")
	}

	for field, plaintext := range plain {
		stored, _ := captured.config[field].(string)
		if !strings.HasPrefix(stored, "ec1:") {
			t.Errorf("imported %s stored without field encryption: %q", field, stored)
		}
		if strings.Contains(stored, plaintext) {
			t.Errorf("imported %s ciphertext contains plaintext secret", field)
		}
	}
	if got := captured.config["channel_id"]; got != "C-IMPORTED" {
		t.Errorf("routing channel_id was modified: got %v", got)
	}
	if got := captured.config["username"]; got != "Imported Agent" {
		t.Errorf("public username was modified: got %v", got)
	}

	if err := channels.DecryptSensitiveFields(captured.config); err != nil {
		t.Fatalf("decrypt imported channel config: %v", err)
	}
	for field, want := range plain {
		if got := captured.config[field]; got != want {
			t.Errorf("imported %s round-trip = %v, want %q", field, got, want)
		}
	}
}

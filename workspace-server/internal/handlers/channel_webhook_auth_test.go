package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/channels"
	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func handlerSlackSignature(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("v0:" + timestamp + ":"))
	_, _ = mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func TestAuthenticateWebhookCandidates_BindsCredentialToConfiguredRow(t *testing.T) {
	now := time.Unix(1_750_000_000, 0)

	t.Run("Slack HMAC", func(t *testing.T) {
		body := []byte("command=%2Fask&text=hello&channel_id=C-B&user_id=U1")
		ts := strconv.FormatInt(now.Unix(), 10)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/slack", strings.NewReader(string(body)))
		req.Header.Set("X-Slack-Request-Timestamp", ts)
		req.Header.Set("X-Slack-Signature", handlerSlackSignature("secret-a", ts, body))
		rows := []channels.ChannelRow{
			{ID: "slack-a", Config: map[string]interface{}{"channel_id": "C-A", "signing_secret": "secret-a"}},
			{ID: "slack-b", Config: map[string]interface{}{"channel_id": "C-B", "signing_secret": "secret-b"}},
			{ID: "slack-missing", Config: map[string]interface{}{"channel_id": "C-B"}},
		}

		got := authenticateWebhookCandidates("slack", req, body, rows, now)
		if len(got) != 1 || got[0].ID != "slack-a" {
			t.Fatalf("authenticated Slack rows = %+v, want only slack-a", got)
		}
	})

	t.Run("Lark verify token", func(t *testing.T) {
		body := []byte(`{"schema":"2.0","header":{"event_type":"im.message.receive_v1","token":"token-b"}}`)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/lark", strings.NewReader(string(body)))
		rows := []channels.ChannelRow{
			{ID: "lark-a", Config: map[string]interface{}{"verify_token": "token-a"}},
			{ID: "lark-b", Config: map[string]interface{}{"verify_token": "token-b"}},
			{ID: "lark-missing", Config: map[string]interface{}{}},
		}

		got := authenticateWebhookCandidates("lark", req, body, rows, now)
		if len(got) != 1 || got[0].ID != "lark-b" {
			t.Fatalf("authenticated Lark rows = %+v, want only lark-b", got)
		}
	})

	t.Run("Telegram secret header", func(t *testing.T) {
		body := []byte(`{"update_id":1}`)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/telegram", strings.NewReader(string(body)))
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "secret-b")
		rows := []channels.ChannelRow{
			{ID: "telegram-a", Config: map[string]interface{}{"webhook_secret": "secret-a"}},
			{ID: "telegram-b", Config: map[string]interface{}{"webhook_secret": "secret-b"}},
			{ID: "telegram-missing", Config: map[string]interface{}{}},
		}

		got := authenticateWebhookCandidates("telegram", req, body, rows, now)
		if len(got) != 1 || got[0].ID != "telegram-b" {
			t.Fatalf("authenticated Telegram rows = %+v, want only telegram-b", got)
		}
	})

	t.Run("Discord Ed25519", func(t *testing.T) {
		pubA, privA := genDiscordKey(t)
		pubB, _ := genDiscordKey(t)
		body := []byte(`{"type":2,"channel_id":"discord-b","data":{"name":"ask"}}`)
		req := discordSignedRequest(t, string(body), "1750000000", privA)
		rows := []channels.ChannelRow{
			{ID: "discord-a", Config: map[string]interface{}{"chat_id": "discord-a", "public_key": pubA}},
			{ID: "discord-b", Config: map[string]interface{}{"chat_id": "discord-b", "public_key": pubB}},
			{ID: "discord-missing", Config: map[string]interface{}{"chat_id": "discord-b"}},
		}
		t.Setenv("DISCORD_APP_PUBLIC_KEY", "")

		got := authenticateWebhookCandidates("discord", req, body, rows, now)
		if len(got) != 1 || got[0].ID != "discord-a" {
			t.Fatalf("authenticated Discord rows = %+v, want only discord-a", got)
		}
		if matchesChatID(got[0].Config, "discord-b") {
			t.Fatal("request signed by Discord app A became eligible for app B's configured channel")
		}
	})
}

func TestMatchesChatID_SlackChannelIDFallback(t *testing.T) {
	if !matchesChatID(map[string]interface{}{"channel_id": "C123"}, "C123") {
		t.Fatal("Slack channel_id did not bind the parsed webhook channel")
	}
	if matchesChatID(map[string]interface{}{"channel_id": "C123"}, "C999") {
		t.Fatal("Slack channel_id matched a different channel")
	}
}

func webhookDBRows(configs ...struct {
	id, workspaceID, channelType, config string
}) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{
		"id", "workspace_id", "channel_type", "channel_config", "enabled", "allowed_users",
	})
	for _, item := range configs {
		rows.AddRow(item.id, item.workspaceID, item.channelType, []byte(item.config), true, []byte(`[]`))
	}
	return rows
}

func expectWebhookRows(mock sqlmock.Sqlmock, channelType string, rows *sqlmock.Rows) {
	mock.ExpectQuery(`SELECT id, workspace_id, channel_type, channel_config, enabled, allowed_users\s+FROM workspace_channels\s+WHERE channel_type = \$1 AND enabled = true`).
		WithArgs(channelType).
		WillReturnRows(rows)
}

func runWebhook(handler *ChannelHandler, channelType string, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Params = gin.Params{{Key: "type", Value: channelType}}
	handler.Webhook(c)
	return w
}

func TestChannelHandler_Webhook_ProviderAuthenticationFailsClosed(t *testing.T) {
	t.Run("Slack missing signing secret", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		handler := NewChannelHandler(newTestChannelManager())
		expectWebhookRows(mock, "slack", webhookDBRows(struct {
			id, workspaceID, channelType, config string
		}{"slack-1", "ws-1", "slack", `{"channel_id":"C123"}`}))

		body := "command=%2Fask&text=hello&channel_id=C123&user_id=U1"
		now := time.Now()
		ts := strconv.FormatInt(now.Unix(), 10)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/slack", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Slack-Request-Timestamp", ts)
		req.Header.Set("X-Slack-Signature", handlerSlackSignature("some-secret", ts, []byte(body)))

		if w := runWebhook(handler, "slack", req); w.Code != http.StatusUnauthorized {
			t.Fatalf("Slack row without signing_secret returned %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("Lark missing verify token", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		handler := NewChannelHandler(newTestChannelManager())
		expectWebhookRows(mock, "lark", webhookDBRows(struct {
			id, workspaceID, channelType, config string
		}{"lark-1", "ws-1", "lark", `{"chat_id":"oc_chat"}`}))

		body := `{"schema":"2.0","header":{"event_type":"im.message.receive_v1","token":"some-token"},"event":{"sender":{"sender_id":{"open_id":"ou_x"}},"message":{"message_id":"m","chat_id":"oc_chat","message_type":"text","content":"{\"text\":\"hi\"}"}}}`
		req := httptest.NewRequest(http.MethodPost, "/webhooks/lark", strings.NewReader(body))

		if w := runWebhook(handler, "lark", req); w.Code != http.StatusUnauthorized {
			t.Fatalf("Lark row without verify_token returned %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("Telegram missing webhook secret", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		handler := NewChannelHandler(newTestChannelManager())
		expectWebhookRows(mock, "telegram", webhookDBRows(struct {
			id, workspaceID, channelType, config string
		}{"telegram-1", "ws-1", "telegram", `{"chat_id":"-100123"}`}))

		body := `{"update_id":1,"message":{"message_id":1,"from":{"id":111,"is_bot":false,"first_name":"Test"},"chat":{"id":-100123,"title":"Test Group","type":"supergroup"},"date":1700000000,"text":"hello"}}`
		req := httptest.NewRequest(http.MethodPost, "/webhooks/telegram", strings.NewReader(body))
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "some-secret")

		if w := runWebhook(handler, "telegram", req); w.Code != http.StatusUnauthorized {
			t.Fatalf("Telegram row without webhook_secret returned %d: %s", w.Code, w.Body.String())
		}
	})
}

func TestChannelHandler_Webhook_AuthenticatedRequestsBindAndRespond(t *testing.T) {
	t.Run("Slack authentic request uses channel_id", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		handler := NewChannelHandler(newTestChannelManager())
		expectWebhookRows(mock, "slack", webhookDBRows(struct {
			id, workspaceID, channelType, config string
		}{"slack-1", "ws-1", "slack", `{"channel_id":"C-configured","signing_secret":"slack-secret"}`}))

		body := "command=%2Fask&text=hello&channel_id=C-other&user_id=U1"
		now := time.Now()
		ts := strconv.FormatInt(now.Unix(), 10)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/slack", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Slack-Request-Timestamp", ts)
		req.Header.Set("X-Slack-Signature", handlerSlackSignature("slack-secret", ts, []byte(body)))

		w := runWebhook(handler, "slack", req)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"no_channel"`) {
			t.Fatalf("authenticated Slack request returned %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("Lark authentic request binds chat", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		handler := NewChannelHandler(newTestChannelManager())
		expectWebhookRows(mock, "lark", webhookDBRows(struct {
			id, workspaceID, channelType, config string
		}{"lark-1", "ws-1", "lark", `{"chat_id":"oc_configured","verify_token":"lark-token"}`}))

		body := `{"schema":"2.0","header":{"event_type":"im.message.receive_v1","token":"lark-token"},"event":{"sender":{"sender_id":{"open_id":"ou_x"}},"message":{"message_id":"m","chat_id":"oc_other","message_type":"text","content":"{\"text\":\"hi\"}"}}}`
		req := httptest.NewRequest(http.MethodPost, "/webhooks/lark", strings.NewReader(body))

		w := runWebhook(handler, "lark", req)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"no_channel"`) {
			t.Fatalf("authenticated Lark request returned %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("Telegram authentic request binds chat", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		handler := NewChannelHandler(newTestChannelManager())
		expectWebhookRows(mock, "telegram", webhookDBRows(struct {
			id, workspaceID, channelType, config string
		}{"telegram-1", "ws-1", "telegram", `{"chat_id":"-100999","webhook_secret":"telegram-secret"}`}))

		body := `{"update_id":1,"message":{"message_id":1,"from":{"id":111,"is_bot":false,"first_name":"Test"},"chat":{"id":-100123,"title":"Test Group","type":"supergroup"},"date":1700000000,"text":"hello"}}`
		req := httptest.NewRequest(http.MethodPost, "/webhooks/telegram", strings.NewReader(body))
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "telegram-secret")

		w := runWebhook(handler, "telegram", req)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"no_channel"`) {
			t.Fatalf("authenticated Telegram request returned %d: %s", w.Code, w.Body.String())
		}
	})
}

func TestChannelHandler_Webhook_VerificationChallengesAreNotOverwritten(t *testing.T) {
	t.Run("Slack URL verification", func(t *testing.T) {
		mock := setupTestDB(t)
		handler := NewChannelHandler(newTestChannelManager())
		expectWebhookRows(mock, "slack", webhookDBRows(struct {
			id, workspaceID, channelType, config string
		}{"slack-1", "ws-1", "slack", `{"channel_id":"C123","signing_secret":"slack-secret"}`}))

		body := `{"type":"url_verification","challenge":"slack-challenge"}`
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/slack", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Slack-Request-Timestamp", ts)
		req.Header.Set("X-Slack-Signature", handlerSlackSignature("slack-secret", ts, []byte(body)))

		w := runWebhook(handler, "slack", req)
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"challenge":"slack-challenge"}` {
			t.Fatalf("Slack challenge response = %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("Lark URL verification", func(t *testing.T) {
		mock := setupTestDB(t)
		handler := NewChannelHandler(newTestChannelManager())
		expectWebhookRows(mock, "lark", webhookDBRows(struct {
			id, workspaceID, channelType, config string
		}{"lark-1", "ws-1", "lark", `{"chat_id":"oc_chat","verify_token":"lark-token"}`}))

		body := `{"type":"url_verification","challenge":"lark-challenge","token":"lark-token"}`
		req := httptest.NewRequest(http.MethodPost, "/webhooks/lark", strings.NewReader(body))

		w := runWebhook(handler, "lark", req)
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"challenge":"lark-challenge"}` {
			t.Fatalf("Lark challenge response = %d %s", w.Code, w.Body.String())
		}
	})
}

func TestChannelHandler_Webhook_DiscordCredentialBindingAndProtocolResponses(t *testing.T) {
	pubA, privA := genDiscordKey(t)
	pubB, privB := genDiscordKey(t)
	t.Setenv("DISCORD_APP_PUBLIC_KEY", "")

	t.Run("PING returns PONG for its configured application key", func(t *testing.T) {
		mock := setupTestDB(t)
		handler := NewChannelHandler(newTestChannelManager())
		expectWebhookRows(mock, "discord", webhookDBRows(
			struct{ id, workspaceID, channelType, config string }{"discord-a", "ws-a", "discord", `{"chat_id":"A","public_key":"` + pubA + `"}`},
			struct{ id, workspaceID, channelType, config string }{"discord-b", "ws-b", "discord", `{"chat_id":"B","public_key":"` + pubB + `"}`},
		))

		w := runWebhook(handler, "discord", discordSignedRequest(t, `{"type":1}`, "1750000000", privB))
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"type":1}` {
			t.Fatalf("Discord PING response = %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("app A signature cannot route app B channel", func(t *testing.T) {
		mock := setupTestDB(t)
		handler := NewChannelHandler(newTestChannelManager())
		expectWebhookRows(mock, "discord", webhookDBRows(
			struct{ id, workspaceID, channelType, config string }{"discord-a", "ws-a", "discord", `{"chat_id":"A","public_key":"` + pubA + `"}`},
			struct{ id, workspaceID, channelType, config string }{"discord-b", "ws-b", "discord", `{"chat_id":"B","public_key":"` + pubB + `"}`},
		))

		body := `{"type":2,"id":"i-1","channel_id":"B","token":"interaction-token","data":{"name":"ask","options":[{"name":"prompt","value":"hello"}]},"member":{"user":{"id":"u-1","username":"alice"}}}`
		w := runWebhook(handler, "discord", discordSignedRequest(t, body, "1750000000", privA))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "No Molecule AI channel is configured") {
			t.Fatalf("cross-app Discord request returned %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("valid command returns Discord interaction response", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		handler := NewChannelHandler(newTestChannelManager())
		expectWebhookRows(mock, "discord", webhookDBRows(
			struct{ id, workspaceID, channelType, config string }{"discord-b", "ws-b", "discord", `{"chat_id":"B","public_key":"` + pubB + `"}`},
		))

		body := `{"type":2,"id":"i-1","channel_id":"B","token":"interaction-token","data":{"name":"ask","options":[{"name":"prompt","value":"hello"}]},"member":{"user":{"id":"u-1","username":"alice"}}}`
		w := runWebhook(handler, "discord", discordSignedRequest(t, body, "1750000000", privB))
		if w.Code != http.StatusOK {
			t.Fatalf("valid Discord command returned %d: %s", w.Code, w.Body.String())
		}
		var response struct {
			Type int `json:"type"`
			Data struct {
				Content string `json:"content"`
				Flags   int    `json:"flags"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode Discord command response: %v", err)
		}
		if response.Type != 4 || response.Data.Content == "" || response.Data.Flags != 64 {
			t.Fatalf("invalid Discord interaction response: %s", w.Body.String())
		}
	})
}

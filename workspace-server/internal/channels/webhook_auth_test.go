package channels

import (
	"context"
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

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func slackTestSignature(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("v0:" + timestamp + ":"))
	_, _ = mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func signedSlackContext(t *testing.T, secret string, now time.Time, body string) *gin.Context {
	t.Helper()
	timestamp := strconv.FormatInt(now.Unix(), 10)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/webhooks/slack", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.Request.Header.Set("X-Slack-Request-Timestamp", timestamp)
	c.Request.Header.Set("X-Slack-Signature", slackTestSignature(secret, timestamp, []byte(body)))
	return c
}

func TestVerifySlackSignature_RealHMACAndReplayWindow(t *testing.T) {
	secret := "slack-signing-secret"
	body := []byte("command=%2Fask&text=hello&channel_id=C123&user_id=U123")
	now := time.Unix(1_750_000_000, 0)
	timestamp := strconv.FormatInt(now.Unix(), 10)
	signature := slackTestSignature(secret, timestamp, body)

	if !VerifySlackSignature(body, timestamp, signature, secret, now) {
		t.Fatal("valid Slack HMAC signature was rejected")
	}
	if VerifySlackSignature(append([]byte(nil), append(body, 'x')...), timestamp, signature, secret, now) {
		t.Fatal("tampered Slack body passed signature verification")
	}
	if VerifySlackSignature(body, timestamp, signature, "wrong-secret", now) {
		t.Fatal("wrong Slack signing secret passed verification")
	}
	if VerifySlackSignature(body, timestamp, signature, secret, now.Add(5*time.Minute+time.Second)) {
		t.Fatal("stale Slack request passed the five-minute replay window")
	}
	if VerifySlackSignature(body, timestamp, signature, "", now) {
		t.Fatal("missing Slack signing secret must fail closed")
	}
}

func TestSlackAdapter_ParseWebhook_RequiresAndVerifiesSigningSecret(t *testing.T) {
	const secret = "slack-signing-secret"
	now := time.Now()
	body := "command=%2Fask&text=hello&channel_id=C123&user_id=U123&user_name=alice&trigger_id=T123"

	valid := signedSlackContext(t, secret, now, body)
	msg, err := (&SlackAdapter{}).ParseWebhook(valid, map[string]interface{}{"signing_secret": secret})
	if err != nil {
		t.Fatalf("valid signed Slack request: %v", err)
	}
	if msg == nil || msg.ChatID != "C123" || msg.Text != "/ask hello" {
		t.Fatalf("unexpected parsed Slack message: %+v", msg)
	}

	missing := signedSlackContext(t, secret, now, body)
	if _, err := (&SlackAdapter{}).ParseWebhook(missing, map[string]interface{}{}); err == nil {
		t.Fatal("Slack ParseWebhook accepted a request without a configured signing_secret")
	}

	wrong := signedSlackContext(t, secret, now, body)
	if _, err := (&SlackAdapter{}).ParseWebhook(wrong, map[string]interface{}{"signing_secret": "wrong"}); err == nil {
		t.Fatal("Slack ParseWebhook accepted a request signed by another app")
	}
}

func TestLarkAdapter_ParseWebhook_RequiresVerifyToken(t *testing.T) {
	body := `{"schema":"2.0","header":{"event_type":"im.message.receive_v1","token":"right"},"event":{"sender":{"sender_id":{"open_id":"ou_x"}},"message":{"message_id":"m","chat_id":"oc_chat","message_type":"text","content":"{\"text\":\"hi\"}"}}}`

	if _, err := (&LarkAdapter{}).ParseWebhook(newLarkRequest(body), map[string]interface{}{}); err == nil {
		t.Fatal("Lark ParseWebhook accepted an event without a configured verify_token")
	}
	if !VerifyLarkWebhookToken([]byte(body), "right") {
		t.Fatal("valid Lark event token was rejected")
	}
	if VerifyLarkWebhookToken([]byte(body), "wrong") {
		t.Fatal("wrong Lark event token was accepted")
	}
	if VerifyLarkWebhookToken([]byte(body), "") {
		t.Fatal("missing Lark verify token must fail closed")
	}
}

func TestTelegramAdapter_ParseWebhook_RequiresWebhookSecret(t *testing.T) {
	body := `{"update_id":1,"message":{"message_id":1,"from":{"id":111,"is_bot":false,"first_name":"Test"},"chat":{"id":-100123,"title":"Test Group","type":"supergroup"},"date":1700000000,"text":"hello"}}`
	newContext := func(header string) *gin.Context {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/webhooks/telegram", strings.NewReader(body))
		c.Request.Header.Set("X-Telegram-Bot-Api-Secret-Token", header)
		return c
	}

	msg, err := (&TelegramAdapter{}).ParseWebhook(newContext("telegram-secret"), map[string]interface{}{"webhook_secret": "telegram-secret"})
	if err != nil || msg == nil || msg.ChatID != "-100123" {
		t.Fatalf("valid Telegram webhook: msg=%+v err=%v", msg, err)
	}
	if _, err := (&TelegramAdapter{}).ParseWebhook(newContext("telegram-secret"), map[string]interface{}{}); err == nil {
		t.Fatal("Telegram ParseWebhook accepted a row without webhook_secret")
	}
	if _, err := (&TelegramAdapter{}).ParseWebhook(newContext("wrong"), map[string]interface{}{"webhook_secret": "telegram-secret"}); err == nil {
		t.Fatal("Telegram ParseWebhook accepted a mismatched secret header")
	}
}

func TestWebhookParsersRejectOversizedBodies(t *testing.T) {
	const oversizedBodyLen = (1 << 20) + 1
	body := strings.Repeat("x", oversizedBodyLen)

	t.Run("Lark", func(t *testing.T) {
		_, err := (&LarkAdapter{}).ParseWebhook(
			newLarkRequest(body),
			map[string]interface{}{"verify_token": "right"},
		)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversized Lark body error = %v, want explicit size rejection", err)
		}
	})

	t.Run("Telegram", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/webhooks/telegram", strings.NewReader(body))
		c.Request.Header.Set("X-Telegram-Bot-Api-Secret-Token", "telegram-secret")

		_, err := (&TelegramAdapter{}).ParseWebhook(
			c,
			map[string]interface{}{"webhook_secret": "telegram-secret"},
		)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversized Telegram body error = %v, want explicit size rejection", err)
		}
	})
}

func TestTelegramAdapter_StartPolling_WebhookModeDoesNotDeleteWebhook(t *testing.T) {
	config := map[string]interface{}{
		"bot_token":      "123456789:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"chat_id":        "-100123",
		"webhook_secret": "telegram-secret",
	}
	if err := (&TelegramAdapter{}).StartPolling(context.Background(), config, nil); err != nil {
		t.Fatalf("webhook-configured Telegram row must not start long polling: %v", err)
	}
}

func TestInboundWebhookCredentialsAreExposedByAdapterSchemas(t *testing.T) {
	tests := []struct {
		name    string
		fields  []ConfigField
		wantKey string
	}{
		{name: "slack signing secret", fields: (&SlackAdapter{}).ConfigSchema(), wantKey: "signing_secret"},
		{name: "telegram webhook secret", fields: (&TelegramAdapter{}).ConfigSchema(), wantKey: "webhook_secret"},
		{name: "lark inbound chat id", fields: (&LarkAdapter{}).ConfigSchema(), wantKey: "chat_id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, field := range tt.fields {
				if field.Key != tt.wantKey {
					continue
				}
				if tt.wantKey != "chat_id" && !field.Sensitive {
					t.Fatalf("%s must be marked sensitive", tt.wantKey)
				}
				if field.Required {
					t.Fatalf("%s must remain optional for outbound-only configs", tt.wantKey)
				}
				return
			}
			t.Fatalf("adapter schema does not expose %s", tt.wantKey)
		})
	}
}

func TestManagerSendOutbound_UsesProviderDestinationContract(t *testing.T) {
	tests := []struct {
		name        string
		channelType string
		config      map[string]interface{}
		wantChats   []string
		wantErr     bool
	}{
		{
			name:        "Discord webhook URL encodes destination",
			channelType: "discord",
			config:      map[string]interface{}{"webhook_url": "https://discord.com/api/webhooks/id/token"},
			wantChats:   []string{""},
		},
		{
			name:        "Lark webhook URL encodes destination",
			channelType: "lark",
			config:      map[string]interface{}{"webhook_url": "https://open.larksuite.com/open-apis/bot/v2/hook/token"},
			wantChats:   []string{""},
		},
		{
			name:        "Slack incoming webhook encodes destination",
			channelType: "slack",
			config:      map[string]interface{}{"webhook_url": "https://hooks.slack.com/services/T/B/token"},
			wantChats:   []string{""},
		},
		{
			name:        "Slack bot API uses channel_id",
			channelType: "slack",
			config:      map[string]interface{}{"bot_token": "xoxb-token", "channel_id": "C123"},
			wantChats:   []string{"C123"},
		},
		{
			name:        "Telegram fans out chat_id",
			channelType: "telegram",
			config:      map[string]interface{}{"bot_token": "123:token", "chat_id": "-100,-200"},
			wantChats:   []string{"-100", "-200"},
		},
		{
			name:        "Telegram still requires chat_id",
			channelType: "telegram",
			config:      map[string]interface{}{"bot_token": "123:token"},
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB, sqlMock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			previousDB := db.DB
			db.DB = mockDB
			t.Cleanup(func() {
				db.DB = previousDB
				_ = mockDB.Close()
			})

			configJSON, err := json.Marshal(tt.config)
			if err != nil {
				t.Fatalf("marshal config: %v", err)
			}
			sqlMock.ExpectQuery(`SELECT id, workspace_id, channel_type, channel_config, enabled, allowed_users`).
				WithArgs("channel-1").
				WillReturnRows(sqlmock.NewRows([]string{
					"id", "workspace_id", "channel_type", "channel_config", "enabled", "allowed_users",
				}).AddRow("channel-1", "workspace-1", tt.channelType, configJSON, true, []byte(`[]`)))

			var sentChats []string
			SetGetSendAdapter(func(string) (SendAdapter, bool) {
				return sendAdapterFunc(func(_ context.Context, _ map[string]interface{}, chatID, _ string) error {
					sentChats = append(sentChats, chatID)
					return nil
				}), true
			})
			t.Cleanup(ResetSendAdapters)

			if !tt.wantErr {
				sqlMock.ExpectExec(`UPDATE workspace_channels`).
					WithArgs("channel-1").
					WillReturnResult(sqlmock.NewResult(0, 1))
			}

			err = (&Manager{}).SendOutbound(context.Background(), "channel-1", "hello")
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected missing destination error")
				}
			} else if err != nil {
				t.Fatalf("SendOutbound: %v", err)
			}
			if strings.Join(sentChats, ",") != strings.Join(tt.wantChats, ",") {
				t.Fatalf("sent chats = %q, want %q", sentChats, tt.wantChats)
			}
			if err := sqlMock.ExpectationsWereMet(); err != nil {
				t.Fatalf("sqlmock expectations: %v", err)
			}
		})
	}
}

type sendAdapterFunc func(context.Context, map[string]interface{}, string, string) error

func (fn sendAdapterFunc) SendMessage(ctx context.Context, config map[string]interface{}, chatID, text string) error {
	return fn(ctx, config, chatID, text)
}

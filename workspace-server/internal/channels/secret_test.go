package channels

import (
	"strings"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/crypto"
)

// withTestEncryptionKey installs a deterministic 32-byte key for the
// duration of a test, then restores the previous state. Without this
// the tests would depend on ambient SECRETS_ENCRYPTION_KEY.
func withTestEncryptionKey(t *testing.T) {
	t.Helper()
	// Base64 of 32 zero bytes = "AAAA..." (44 chars). Matches the loader's
	// base64 path — the raw 32-byte path requires a string that is not
	// decodable as base64, which is surprisingly hard to construct.
	t.Setenv("SECRETS_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	crypto.ResetForTesting()
	crypto.Init()
	t.Cleanup(func() {
		crypto.ResetForTesting()
	})
}

func TestEncryptSensitiveFields_RoundTrip(t *testing.T) {
	withTestEncryptionKey(t)

	cfg := map[string]interface{}{
		"bot_token":      "123456:telegram-bot-token",
		"chat_id":        "-100999",         // non-sensitive, untouched
		"webhook_secret": "hmac-shared-key", // second known-sensitive field
		"webhook_url":    "https://discord.com/api/webhooks/id/credential",
		"verify_token":   "lark-verification-token",
	}

	if err := EncryptSensitiveFields(cfg); err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	if tok, _ := cfg["bot_token"].(string); !strings.HasPrefix(tok, ciphertextPrefix) {
		t.Errorf("bot_token not encrypted: got %q", tok)
	}
	if sec, _ := cfg["webhook_secret"].(string); !strings.HasPrefix(sec, ciphertextPrefix) {
		t.Errorf("webhook_secret not encrypted: got %q", sec)
	}
	if url, _ := cfg["webhook_url"].(string); !strings.HasPrefix(url, ciphertextPrefix) {
		t.Errorf("webhook_url not encrypted: got %q", url)
	}
	if token, _ := cfg["verify_token"].(string); !strings.HasPrefix(token, ciphertextPrefix) {
		t.Errorf("verify_token not encrypted: got %q", token)
	}
	if chat, _ := cfg["chat_id"].(string); chat != "-100999" {
		t.Errorf("chat_id modified: got %q", chat)
	}

	if err := DecryptSensitiveFields(cfg); err != nil {
		t.Fatalf("decrypt: %v", err)
	}

	if got, _ := cfg["bot_token"].(string); got != "123456:telegram-bot-token" {
		t.Errorf("bot_token round-trip mismatch: got %q", got)
	}
	if got, _ := cfg["webhook_secret"].(string); got != "hmac-shared-key" {
		t.Errorf("webhook_secret round-trip mismatch: got %q", got)
	}
	if got, _ := cfg["webhook_url"].(string); got != "https://discord.com/api/webhooks/id/credential" {
		t.Errorf("webhook_url round-trip mismatch: got %q", got)
	}
	if got, _ := cfg["verify_token"].(string); got != "lark-verification-token" {
		t.Errorf("verify_token round-trip mismatch: got %q", got)
	}
}

func TestEncryptSensitiveFields_Idempotent(t *testing.T) {
	withTestEncryptionKey(t)

	cfg := map[string]interface{}{"bot_token": "abc"}
	if err := EncryptSensitiveFields(cfg); err != nil {
		t.Fatalf("first encrypt: %v", err)
	}
	first, _ := cfg["bot_token"].(string)

	if err := EncryptSensitiveFields(cfg); err != nil {
		t.Fatalf("second encrypt: %v", err)
	}
	second, _ := cfg["bot_token"].(string)

	if first != second {
		t.Errorf("idempotent encrypt should not re-wrap: first=%q second=%q", first, second)
	}
}

func TestDecryptSensitiveFields_LegacyPlaintextPassesThrough(t *testing.T) {
	// Legacy row predates #319 — no ciphertext prefix.
	withTestEncryptionKey(t)

	cfg := map[string]interface{}{"bot_token": "legacy-plaintext-value"}
	if err := DecryptSensitiveFields(cfg); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got, _ := cfg["bot_token"].(string); got != "legacy-plaintext-value" {
		t.Errorf("legacy plaintext was mangled: got %q", got)
	}
}

func TestEncryptSensitiveFields_DevFallback_NoKey(t *testing.T) {
	// No key set — dev behaviour matches workspace_secrets: store plaintext.
	t.Setenv("SECRETS_ENCRYPTION_KEY", "")
	crypto.ResetForTesting()
	crypto.Init()
	t.Cleanup(crypto.ResetForTesting)

	cfg := map[string]interface{}{"bot_token": "dev-token"}
	if err := EncryptSensitiveFields(cfg); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if got, _ := cfg["bot_token"].(string); got != "dev-token" {
		t.Errorf("dev fallback should leave plaintext: got %q", got)
	}
}

func TestEncryptSensitiveFields_SkipsEmptyAndNonString(t *testing.T) {
	withTestEncryptionKey(t)

	cfg := map[string]interface{}{
		"bot_token":      "",       // empty
		"webhook_secret": 12345,    // non-string
		"unrelated":      "ignore", // not in sensitiveFields
	}
	if err := EncryptSensitiveFields(cfg); err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if got, _ := cfg["bot_token"].(string); got != "" {
		t.Errorf("empty bot_token should stay empty: got %q", got)
	}
	if got, _ := cfg["webhook_secret"].(int); got != 12345 {
		t.Errorf("non-string webhook_secret should be untouched: got %v", cfg["webhook_secret"])
	}
	if got, _ := cfg["unrelated"].(string); got != "ignore" {
		t.Errorf("unrelated field should be untouched: got %q", got)
	}
}

func TestEncryptSensitiveFields_NilConfig(t *testing.T) {
	if err := EncryptSensitiveFields(nil); err != nil {
		t.Errorf("nil config: expected no error, got %v", err)
	}
	if err := DecryptSensitiveFields(nil); err != nil {
		t.Errorf("nil config: expected no error, got %v", err)
	}
}

func TestRedactSensitiveFields_HidesCredentialURLsAndSecrets(t *testing.T) {
	cfg := map[string]interface{}{
		"bot_token":      "123:ABCDEFGHIJ",
		"webhook_url":    "https://discord.com/api/webhooks/id/super-secret",
		"verify_token":   "verification-secret",
		"signing_secret": "slack-signing-secret",
		"public_key":     "not-a-secret",
		"chat_id":        "channel-123",
	}

	RedactSensitiveFields(cfg)

	if got := cfg["bot_token"]; got != "123:...GHIJ" {
		t.Fatalf("bot_token redaction = %q", got)
	}
	for _, field := range []string{"webhook_url", "verify_token", "signing_secret"} {
		if got := cfg[field]; got != "***" {
			t.Fatalf("%s redaction = %q, want ***", field, got)
		}
	}
	if got := cfg["public_key"]; got != "not-a-secret" {
		t.Fatalf("public_key must remain visible, got %q", got)
	}
	if got := cfg["chat_id"]; got != "channel-123" {
		t.Fatalf("chat_id must remain visible, got %q", got)
	}
}

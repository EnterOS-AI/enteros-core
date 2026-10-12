# Social Channels

Connect AI agent workspaces to Telegram, Slack, Discord, and Lark/Feishu.
Each workspace can have one channel row per platform type; Telegram rows can
serve multiple comma-separated chat IDs.

## Architecture

```
Telegram/Slack/Discord/Lark
    ↓ webhook or long-polling
Public receiver: authenticate request against the candidate channel row
    ↓ authenticated payload only
ChannelAdapter.ParseWebhook() / StartPolling()
    ↓ allowlist check + Redis history lookup
ProxyA2ARequest(ctx, workspaceID, body, "channel:<type>", true)
    ↓ agent processes (existing A2A flow)
Reply text extracted from response
    ↓ ChannelAdapter.SendMessage()
Social chat ← reply (with typing indicator while waiting)
```

The `channel:<type>` caller prefix bypasses workspace hierarchy access checks (same pattern as `webhook:` and `system:`).

## Adapters

| Type | Status | Library |
|------|--------|---------|
| `telegram` | Implemented: long polling or authenticated webhook | `go-telegram-bot-api/v5` |
| `slack` | Implemented: Events API/slash-command input and two outbound modes | native `net/http` |
| `discord` | Implemented: signed Interactions input and Incoming Webhook output | native `net/http`, `crypto/ed25519` |
| `lark` | Implemented: Event Subscription input and Custom Bot output | native `net/http` |

To add an adapter, implement `ChannelAdapter` in
`workspace-server/internal/channels/` and register it in `registry.go`. The
CRUD API and Canvas form consume the adapter schema. A new public-webhook
provider must also add an explicit fail-closed authentication case in
`handlers/channels.go`; registration alone does not make inbound requests safe.

## Telegram Setup

### 1. Create the bot
1. Talk to [@BotFather](https://t.me/BotFather) on Telegram → `/newbot`
2. Save the token (looks like `1234567890:ABCdefGHIjklMNOpqrSTUvwxYZ`)

### 2. Disable group privacy (recommended)
By default, Telegram bots in groups only see commands and @mentions. To let your bot see all group messages:
- @BotFather → `/mybots` → select your bot → **Bot Settings** → **Group Privacy** → **Turn off**
- Then **re-add the bot to the group** (privacy changes don't apply to existing memberships)

The Discover endpoint reports `can_read_all_group_messages` and surfaces a warning if privacy is on.

### 3. Connect via Canvas
1. Open the workspace in Canvas → **Channels** tab → **+ Connect**
2. Paste the bot token
3. Add the bot to your group(s) and send a message, OR send `/start` to it in DMs
4. Click **Detect Chats** → select the chats from the checklist
5. (Optional) Add **Allowed Users** for an allowlist
6. **Connect Channel**

### 4. Or connect via API
```bash
curl -X POST "${PLATFORM_URL}/workspaces/${WORKSPACE_ID}/channels" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{
    "channel_type": "telegram",
    "config": {
      "bot_token": "1234567890:ABC...",
      "chat_id": "-100123, -100456"
    },
    "allowed_users": ["telegram_user_id_1"]
  }'
```

## Multi-chat IDs

A single channel entry serves multiple chats — `chat_id` is comma-separated:
```yaml
config:
  chat_id: "-100123, -100456, -100789"
```

The bot listens for messages from any of these chats and uses the same workspace agent for all of them. Outbound messages (e.g. agent-initiated notifications) are sent to all configured chats.

## Allowlist

Per-channel allowlist of user IDs (or chat IDs for groups). Empty = allow everyone.

```json
{ "allowed_users": ["123456789", "987654321"] }
```

When non-empty, messages from users not in the list are silently dropped (logged but no error).

## Bot Commands

The bot registers these commands via `setMyCommands`, so they appear in Telegram's command autocomplete:

| Command | Behavior |
|---------|----------|
| `/start` | Reply "Connected to Molecule AI agent". Skipped if forwarded to agent. |
| `/help` | List all commands. |
| `/reset` | Clear conversation history (Redis key). |
| `/cancel` | Best-effort acknowledgment (no actual cancel plumbing yet). |

## Conversation History

Last 10 messages per chat stored in Redis at `channel:telegram:{chat_id}:history` with 24h TTL. Sent in A2A `metadata.history` so the agent has context. Same shape as Canvas chat history.

## Webhook Mode

Telegram rows use long polling by default. Adding `webhook_secret` selects
webhook mode and prevents the poller from deleting the configured webhook.
Webhook mode requires:

1. Public URL pointing to `POST /webhooks/telegram` on your platform
2. Manual `setWebhook` call to Telegram with the URL + a `secret_token`
3. Storing the same `secret_token` in `channel_config.webhook_secret`

The receiver requires an exact, constant-time match of
`X-Telegram-Bot-Api-Secret-Token` against that row's non-empty secret. Rows
without `webhook_secret` cannot receive public webhook requests.

## Org Template Auto-Link

Channels can be defined in `org.yaml` so they're auto-created when the org is deployed. Config values support `${VAR}` expansion from `.env` files.

```yaml
workspaces:
  - name: PM
    files_dir: pm
    channels:
      - type: telegram
        config:
          bot_token: ${TELEGRAM_BOT_TOKEN}
          chat_id: ${TELEGRAM_CHAT_ID}
        allowed_users: []
        enabled: true
```

The vars are resolved from (in order): `pm/.env` → org root `.env` → platform process env. If any required var is unresolved, the channel is skipped with a clear log message and the skip reason is surfaced in the import response (`channels_skipped` field).

The platform calls `adapter.ValidateConfig()` upfront so unknown channel types or invalid configs fail fast. Insert is idempotent (`ON CONFLICT DO UPDATE`) so re-importing the same org refreshes the channel config.

## Slack Setup

Slack supports two outbound configurations:

- Bot API: `bot_token` plus `channel_id`
- Incoming Webhook: `webhook_url`

To receive Events API callbacks or slash commands, also set the Slack App's
`signing_secret`. Point the Slack request URL at `POST /webhooks/slack`.
The receiver computes Slack's `v0:{timestamp}:{raw_body}` HMAC-SHA256 signature,
uses a constant-time comparison, and rejects timestamps more than five minutes
from server time. The signature must authenticate the same row whose
`channel_id` matches the parsed event.

```json
{
  "channel_type": "slack",
  "config": {
    "bot_token": "xoxb-...",
    "channel_id": "C01234ABCDE",
    "signing_secret": "..."
  }
}
```

Slack URL-verification challenges are echoed only after signature verification.
Molecule AI parses registered slash commands, but it does not create or
register commands in your Slack App.

## Lark / Feishu Setup

Outbound delivery uses a Lark or Feishu Custom Bot `webhook_url`. Inbound
delivery is a separate Lark App Event Subscription pointed at
`POST /webhooks/lark`. An inbound-capable row also needs:

- `chat_id`: the chat to bind to this workspace row
- `verify_token`: the Event Subscription Verification Token

The receiver requires the token for URL-verification and
`im.message.receive_v1` callbacks, compares it in constant time, and only
routes events whose chat ID matches the authenticated row. See
[Connecting an AI Agent to Lark / Feishu](../tutorials/lark-feishu-channel.md).

## Discord Setup

### 1. Create a Discord Webhook
1. Open your Discord server → **Edit Channel** (or create a new one) → **Integrations** → **Webhooks**
2. Click **New Webhook** → name it → **Copy Webhook URL**
3. The URL looks like: `https://discord.com/api/webhooks/<id>/<token>`

### 2. Connect via Canvas
1. Open the workspace in Canvas → **Channels** tab → **+ Connect**
2. Paste the webhook URL
3. **Connect Channel**

### 3. Or connect via API
```bash
curl -X POST "${PLATFORM_URL}/workspaces/${WORKSPACE_ID}/channels" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{
    "channel_type": "discord",
    "config": {
      "webhook_url": "https://discord.com/api/webhooks/YOUR_WEBHOOK_ID/YOUR_WEBHOOK_TOKEN",
      "chat_id": "YOUR_DISCORD_CHANNEL_ID",
      "public_key": "YOUR_APPLICATION_ED25519_PUBLIC_KEY_HEX"
    }
  }'
```

### 4. Register application commands (for inbound)

Discord inbound requires a Discord Application. Copy its Ed25519 Public Key
into `public_key`, copy the target channel ID into `chat_id`, and point the
Application's Interactions Endpoint URL at `POST /webhooks/discord`. Register
the commands you want in Discord; Molecule AI parses the received command name
and options but does not register commands for you.

Every request is verified against the candidate row's `public_key` before its
untrusted `channel_id` can select a destination. The legacy
`app_public_key` config name is accepted only for existing rows. A self-hosted
deployment can use `DISCORD_APP_PUBLIC_KEY` for rows with no per-row key.

### Inbound / Outbound
| Direction | Mechanism |
|---|---|
| **Inbound** | Signed Discord Interactions endpoint → `ParseWebhook()` |
| **Outbound** | Discord Incoming Webhooks → `SendMessage()` (2000-char chunking built in) |

Discord endpoint PINGs receive the required type-1 PONG. Accepted application
commands receive an immediate ephemeral type-4 interaction response while the
agent runs asynchronously; the agent's reply is sent through the configured
Incoming Webhook. No Discord bot token is required for outbound-only use.

See `workspace-server/internal/channels/discord.go` for the full adapter implementation.

## Credential Storage and Public Receiver Rules

In production, `bot_token`, `webhook_secret`, `webhook_url`, `verify_token`,
and `signing_secret` are field-encrypted with AES-256-GCM in
`channel_config`. Read APIs redact these values. Routing identifiers and
Discord public keys remain plaintext because they are not secrets. Legacy
plaintext rows are read for compatibility and are upgraded when re-saved.

The public receiver caps bodies at 1 MiB and rejects a request unless at least
one enabled row of that provider authenticates it. Chat matching happens only
inside that authenticated row subset, preventing one app or bot credential
from selecting another row by supplying its chat ID.

## Hot Reload

CRUD operations on `/workspaces/:id/channels` (POST, PATCH, DELETE) trigger `manager.Reload()`. Active polling goroutines are diffed against the desired DB state — new channels start, removed/disabled ones stop. No platform restart required.

The Discover endpoint also pauses any pollers using the same bot token to avoid Telegram's "only one `getUpdates` per bot" 409 Conflict, then resumes them after.

## Database

Migration `016_workspace_channels.sql`:
```sql
CREATE TABLE workspace_channels (
    id              UUID PRIMARY KEY,
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    channel_type    TEXT NOT NULL,          -- 'telegram', 'slack', etc.
    channel_config  JSONB NOT NULL,         -- adapter-specific (bot_token, chat_id, ...)
    enabled         BOOLEAN DEFAULT true,
    allowed_users   JSONB DEFAULT '[]',
    last_message_at TIMESTAMPTZ,
    message_count   INTEGER DEFAULT 0,
    ...
);
```

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/channels/adapters` | List available platforms |
| POST | `/channels/discover` | Detect chats for a bot token |
| GET | `/workspaces/:id/channels` | List channels (sensitive config redacted) |
| POST | `/workspaces/:id/channels` | Create channel (validates config) |
| PATCH | `/workspaces/:id/channels/:channelId` | Update config/enabled/allowlist |
| DELETE | `/workspaces/:id/channels/:channelId` | Remove channel |
| POST | `/workspaces/:id/channels/:channelId/send` | Outbound message |
| POST | `/workspaces/:id/channels/:channelId/test` | Send test message |
| POST | `/webhooks/:type` | Incoming webhook receiver |

## Telegram-Specific Implementation Notes

- **Bot instance cache** (`sync.RWMutex`) avoids `getMe` API call on every send.
- **4096-char message splitting** at paragraph/line/word boundaries (Telegram's hard limit).
- **`sendChatAction("typing")`** goroutine re-sends every 4s during agent calls so the user sees "typing..." for the entire wait.
- **Markdown → plain text fallback** if the formatting fails (`ParseMode = "Markdown"` then retry without).
- **`my_chat_member` event handling** — when the bot is added to a chat, it auto-greets with the chat ID (no `/start` required).
- **Typed error handling**: 401 invalidates the bot cache; 403 returns a forbidden error; 429 honors `RetryAfter`.
- **Token format validation** via regex (`^\d+:[A-Za-z0-9_-]{30,}$`) before any API call.

## Files

| File | Purpose |
|------|---------|
| `workspace-server/internal/channels/adapter.go` | `ChannelAdapter` interface |
| `workspace-server/internal/channels/registry.go` | Adapter registry |
| `workspace-server/internal/channels/telegram.go` | Telegram implementation |
| `workspace-server/internal/channels/slack.go` | Slack implementation + v0 HMAC verification |
| `workspace-server/internal/channels/lark.go` | Lark/Feishu implementation + token verification |
| `workspace-server/internal/channels/discord.go` | Discord adapter and interaction parsing |
| `workspace-server/internal/channels/secret.go` | Sensitive-field encryption and redaction |
| `workspace-server/internal/channels/manager.go` | Orchestrator with hot reload |
| `workspace-server/internal/handlers/channels.go` | REST API + webhook |
| `workspace-server/migrations/016_workspace_channels.sql` | DB schema |
| `canvas/src/components/tabs/ChannelsTab.tsx` | Canvas UI |

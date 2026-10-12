# Social Channels Quickstart — Discord and Telegram

Connect a workspace to Discord or Telegram using the same A2A path as Canvas.
Channel changes hot-reload; no platform restart is required. A workspace can
have one channel row per platform type, and a Telegram row can cover multiple
chat IDs.

| Platform | Inbound | Outbound |
|---|---|---|
| Discord | Ed25519-signed HTTP Interactions | Discord Incoming Webhook |
| Telegram | Bot API long polling (default) or secret-token webhook | Bot API |

## Discord

### 1. Create the outbound webhook

In the target Discord channel, open **Channel Settings → Integrations →
Webhooks**, create a webhook, and copy its URL:

```text
https://discord.com/api/webhooks/123456789/abcdefghijklmnop
```

The URL contains a credential. Do not paste it into logs or issues.

### 2. Connect the workspace

For outbound-only use, select **Discord** in Canvas → **Channels** and enter
the webhook URL. For inbound commands, also enter:

- **Discord Channel ID** (`chat_id`)
- the Discord Application's hex-encoded **Interactions Public Key**
  (`public_key`)

The equivalent API request is:

```bash
curl -X POST "${PLATFORM_URL}/workspaces/${WORKSPACE_ID}/channels" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "channel_type": "discord",
    "config": {
      "webhook_url": "https://discord.com/api/webhooks/123456789/abcdefghijklmnop",
      "chat_id": "123456789012345678",
      "public_key": "YOUR_APPLICATION_ED25519_PUBLIC_KEY_HEX"
    },
    "allowed_users": []
  }'
```

`webhook_url` is the only field required for outbound-only use.

### 3. Configure inbound Interactions

Create a Discord Application, point its **Interactions Endpoint URL** at:

```text
https://your-platform.example/webhooks/discord
```

Register the application commands you want through Discord's Developer Portal
or API. Molecule AI does not create `/ask`, `/status`, or any other command.
It forwards the name and options of a command that you registered.

The receiver verifies Discord's Ed25519 signature against the same channel row
that later matches the payload's `channel_id`. Discord PING receives a type-1
PONG. A valid command receives an immediate ephemeral type-4 acknowledgement;
the agent's eventual reply is posted through the configured Incoming Webhook.

To verify, invoke a command you actually registered. You should first see the
acknowledgement and then the agent reply in that channel.

## Telegram

### 1. Create the bot

Open [@BotFather](https://t.me/BotFather), run `/newbot`, and save the bot
token. In groups, disable Group Privacy if the bot should receive ordinary
messages, then remove and re-add it so the setting takes effect.

### 2. Detect and connect chats

In Canvas → **Channels**, select **Telegram**, enter the token, add the bot to
the desired chats, and click **Detect Chats**. Select one or more chat IDs and
connect the channel.

```bash
curl -X POST "${PLATFORM_URL}/workspaces/${WORKSPACE_ID}/channels" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "channel_type": "telegram",
    "config": {
      "bot_token": "123456789:ABCdefGHI...",
      "chat_id": "-100123456789"
    },
    "allowed_users": []
  }'
```

Send an ordinary text message to verify the agent round trip. The adapter also
registers four local bot commands:

| Command | Implemented behavior |
|---|---|
| `/start` | Confirm that the bot is connected |
| `/help` | Show the command list |
| `/reset` | Clear this chat's Redis conversation history |
| `/cancel` | Acknowledge a best-effort cancellation request; no hard-cancel plumbing exists |

Other slash-prefixed text is not a built-in command; it is forwarded like any
other message if Telegram delivers it.

### Optional webhook mode

Long polling is the default. To use Telegram webhooks, generate a `secret_token`,
pass it to Telegram's `setWebhook`, and store the identical value as
`config.webhook_secret`. Point Telegram at:

```text
https://your-platform.example/webhooks/telegram
```

The presence of `webhook_secret` disables long polling for that row. The
receiver requires an exact constant-time match of the
`X-Telegram-Bot-Api-Secret-Token` header. A row with no secret cannot receive
requests through the public Telegram webhook route.

## Multi-chat Telegram rows

`chat_id` accepts a comma-separated list:

```json
{
  "bot_token": "123456789:ABCdefGHI...",
  "chat_id": "-100123456789, -100987654321"
}
```

Inbound messages from any listed chat route to the same workspace. Outbound
messages fan out to every configured chat ID.

## Sending outbound messages

Use the channel API with a real channel row ID:

```bash
curl -X POST "${PLATFORM_URL}/workspaces/${WORKSPACE_ID}/channels/${CHANNEL_ID}/send" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"text":"Deployment complete."}'
```

## Security behavior

- Discord Interactions use per-row Ed25519 verification; Discord webhook URLs
  are never included in returned HTTP-client errors.
- Telegram webhook mode uses Telegram's exact secret-token header; token-format
  validation alone is not webhook authentication.
- In production, `bot_token`, `webhook_secret`, and credential-bearing
  `webhook_url` values are AES-256-GCM field-encrypted in `channel_config` and
  redacted from list responses. Recoverable encryption is required because the
  adapters use these credentials for outbound API calls.
- `allowed_users` can restrict a channel to selected platform user or chat IDs.

## Related

- [Social channels reference](../agent-runtime/social-channels.md)
- [Discord adapter implementation note](../blog/2026-04-21-discord-adapter/index.md)
- [Lark / Feishu tutorial](lark-feishu-channel.md)
- [Remote agent tutorial](register-remote-agent.md)

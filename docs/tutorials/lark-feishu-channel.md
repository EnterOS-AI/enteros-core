# Connecting an AI Agent to Lark / Feishu

The Lark adapter combines two Lark primitives:

- a **Custom Bot webhook** for outbound messages
- a **Lark App Event Subscription** for optional inbound messages

Lark (international) and Feishu (China) use the same payload shape. Custom Bot
URLs on both `open.larksuite.com` and `open.feishu.cn` are accepted.

## Prerequisites

- a running Molecule AI workspace
- a Lark or Feishu Custom Bot webhook URL
- for inbound messages, a Lark App with Event Subscriptions access, its
  Verification Token, and the target chat ID

## 1. Create the outbound Custom Bot

In the target group, open **Settings → Bots → Add Bot → Custom Bot** and copy
the webhook URL:

```text
https://open.larksuite.com/open-apis/bot/v2/hook/YOUR_TOKEN
```

That URL contains a credential and is field-encrypted by Molecule AI in
production.

## 2. Add the channel row

For outbound-only use, `webhook_url` is sufficient. For inbound use, add:

- `chat_id`: the Lark chat that should route to this workspace
- `verify_token`: the Verification Token from the Lark App's Event
  Subscriptions settings

```bash
curl -X POST "${PLATFORM_URL}/workspaces/${WORKSPACE_ID}/channels" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "channel_type": "lark",
    "config": {
      "webhook_url": "https://open.larksuite.com/open-apis/bot/v2/hook/YOUR_TOKEN",
      "chat_id": "oc_TARGET_CHAT_ID",
      "verify_token": "YOUR_EVENT_SUBSCRIPTION_VERIFY_TOKEN"
    },
    "allowed_users": []
  }'
```

The Custom Bot URL already selects the outbound group, so `chat_id` is not
used by `SendMessage`. It is required for inbound routing: the public receiver
will not route an event to a row whose configured chat ID differs from the
event's `event.message.chat_id`.

## 3. Configure Event Subscriptions

In the Lark Developer Console for your App:

1. Set the Event Subscription Request URL to:

   ```text
   https://your-platform.example/webhooks/lark
   ```

2. Subscribe to `im.message.receive_v1`.
3. Ensure the App is installed in, and permitted to receive messages from, the
   target chat.

During URL verification, Molecule AI verifies the request token and returns
Lark's challenge. Event callbacks also require the configured token. Missing
or mismatched tokens fail closed; comparisons are constant-time. If multiple
Lark rows exist, only rows authenticated by that token are eligible for chat
matching.

## 4. Verify both directions

Send an ordinary text message in the configured Lark chat. A valid
`im.message.receive_v1` event is converted to an A2A user message. The agent's
reply is then posted through the configured Custom Bot webhook.

The outbound API can return HTTP 200 with an application-level error. Molecule
AI treats a response body with `code != 0` as a delivery failure; only
`{"code":0,...}` is accepted as successful delivery.

## Important boundaries

- A Custom Bot webhook alone is outbound-only; it does not deliver inbound
  Event Subscription callbacks.
- `verify_token` is optional only for outbound-only rows. It is mandatory at
  the public receiver.
- Only text `im.message.receive_v1` events are routed. Other event and message
  types are acknowledged and ignored after authentication.
- Outbound messages go to the group encoded in `webhook_url`; the adapter does
  not reply through a per-message thread or DM API.
- `webhook_url` and `verify_token` are AES-256-GCM field-encrypted in
  production and redacted from channel list responses.

## Related

- [Social channels reference](../agent-runtime/social-channels.md)
- [API routes](../api-reference.md#routes)

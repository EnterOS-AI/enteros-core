---
title: "Discord Channels: Outbound Webhooks and Inbound Interactions"
date: 2026-04-21
slug: discord-adapter-launch
description: "Current Discord outbound-webhook support and row-bound, Ed25519-signed inbound Interactions."
tags: [launch, discord, social-channels, platform]
---

# Discord Channels: Outbound Webhooks and Inbound Interactions

> **Current status (July 14, 2026):** Outbound delivery through a Discord
> Incoming Webhook is implemented. A signed inbound-interaction handler also
> exists and verifies each request against the selected row's `public_key`
> before chat routing. Existing rows using `app_public_key` remain readable,
> and self-hosted rows without a per-row key can use
> `DISCORD_APP_PUBLIC_KEY`. Neither path uses a Discord Gateway connection.

This article describes the Discord channel adapter that is present in the
current Core source tree.

---

## The Problem with Traditional Discord Bot Setup

Most Discord bot integrations follow the same pattern: create an app in the Developer Portal, set up OAuth2, handle the Gateway connection, configure intents and permissions, manage rate limits. That's a significant chunk of work before your agent can say hello in a channel.

For outbound-only notifications, that overhead rarely pays for itself. The
Molecule AI adapter can send through an Incoming Webhook without a bot token.
Inbound commands still require a Discord Application because Discord signs and
delivers interactions through that application.

---

## What the Adapter Does

**Outbound: your agent sends to Discord**

You create a Discord Incoming Webhook — one URL, generated from any channel's Integrations settings. That URL encodes the channel and the bot credentials. You paste it into your Molecule AI workspace config.

That's the only credential required for **outbound-only** use. Your workspace
agent can send messages to that Discord channel. Long responses are
automatically split into Discord-safe chunks (2,000-character limit).

**Inbound: slash commands route to your agent**

The inbound handler accepts commands registered on a Discord Application.
Discord POSTs a signed JSON payload to the platform's Interactions endpoint;
the platform verifies the Ed25519 signature before the adapter parses and
routes it. The signature must authenticate the same row whose `chat_id`
matches the interaction's `channel_id`. Replies use the configured Incoming
Webhook.

No polling or Gateway connection is involved. The webhook alone is sufficient
only for outbound delivery; inbound delivery additionally needs the Discord
Application setup described below.

---

## Setup

1. Create a Discord Incoming Webhook — Channel Settings → Integrations → Webhooks → New Webhook
2. Copy the webhook URL
3. In Molecule AI Canvas: open your workspace → **Channels** tab → **+ Connect** → **Discord** → paste the URL

Or via API:

```bash
curl -X POST https://your-platform.com/workspaces/${WORKSPACE_ID}/channels \
  -H 'Authorization: Bearer ${TOKEN}' \
  -H 'Content-Type: application/json' \
  -d '{
    "channel_type": "discord",
    "config": {
      "webhook_url": "https://discord.com/api/webhooks/123456789/abcdefghijklmnop",
      "chat_id": "123456789012345678",
      "public_key": "YOUR_APPLICATION_ED25519_PUBLIC_KEY_HEX"
    }
  }'
```

For inbound commands, create a Discord Application, put its Ed25519 Public Key
in `public_key`, put the destination channel ID in `chat_id`, and point the
Application's **Interactions Endpoint URL** at
`https://<tenant>.moleculesai.app/webhooks/discord`. Register the commands you
want through Discord; Molecule AI does not create `/ask`, `/status`, or any
other command. A PING receives Discord's type-1 PONG. A valid application
command receives an immediate ephemeral type-4 acknowledgement while the
agent runs, then the agent reply is posted through the Incoming Webhook.

---

## Security: Webhook Tokens Don't Appear in Logs

Webhook URLs contain a token (`/webhooks/{id}/{token}`). If that token leaks
into server logs, it's a rotation event. The current adapter deliberately does
not wrap the HTTP client's URL-bearing network error and returns a generic
error instead.

In production, the credential-bearing `webhook_url` is AES-256-GCM
field-encrypted in `channel_config` and redacted from list responses. The
Ed25519 `public_key` and routing `chat_id` remain plaintext because they are
not secrets.

---

## What to Actually Use It For

The adapter fits naturally into workflows your team already runs in Discord:

- **Incident triage** — after you register an `/incident` command, its name and options can be forwarded to a triage agent
- **Deployment coordination** — a CI/CD agent posts build results, rollback recommendations, and health checks to a DevOps Discord channel
- **Community management** — after you register `/support`, a Community Manager agent can route its options to a sub-agent and return the answer to Discord
- **Scheduled summaries** — agents post periodic status updates, log digests, or metric snapshots to a channel on a schedule

Registered Discord Application Commands are the inbound interface. Their
behavior comes from your agent; the adapter only authenticates, parses, and
routes them.

---

## Implementation

The Discord adapter implements the same `ChannelAdapter` interface used by the
other channel integrations. The current source of truth is
`workspace-server/internal/channels/discord.go`; the route and signature check
are in `workspace-server/internal/handlers/channels.go`.

Documentation: [Social Channels guide](../../agent-runtime/social-channels.md#discord-setup)

→ [Connect a Discord channel →](../../agent-runtime/social-channels.md#discord-setup)

---

*Current behavior was verified against the Core channel adapter and webhook
handler on July 14, 2026.*

---
title: "[Historical] Phase 33: Cloudflare Tunnel to Public-IP Workspaces"
date: 2026-04-22
slug: cloudflare-tunnel-migration
description: "Historical record of the retired Phase 33 AWS direct-connect design. This is not current deployment guidance."
tags: [platform, infrastructure, cloud, deployment]
---

# [Historical] Phase 33: From Cloudflare Tunnel to Public-IP Workspaces

> **Historical architecture only — do not follow these deployment
> instructions.** This April 2026 article records an intermediate AWS design
> that was later retired. The current platform uses domain-only access,
> CI-on-merge deployments, and the active control-plane provisioning backends;
> operators must not infer a public-IP, VPC, security-group, or operator-host
> workflow from this post. See the current
> [provisioner](../../architecture/provisioner.md) and
> [workspace placement](../../architecture/workspace-placement.md)
> documentation.

In Phase 33, Molecule AI changed how the then-current cloud-hosted agent
workspaces connected to the platform. The design moved from outbound
Cloudflare Tunnels to public IP addresses in the operator's AWS account.

This post covers what changed architecturally, why we made the change, and what operators and developers need to know.

## What was there before: the Cloudflare Tunnel model

Cloudflare Tunnel (formerly `cloudflared`) worked like this:

1. A lightweight daemon ran inside each agent workspace container
2. It maintained an outbound-only WebSocket connection to a Cloudflare edge node
3. External traffic (your browser, API calls, CLI commands) hit a Cloudflare-assigned hostname (`*.trydirect.io` or a custom domain via Cloudflare)
4. Cloudflare routed that traffic through the tunnel WebSocket to the workspace

This was elegant for one specific constraint: **no inbound firewall rules required**. The workspace container opened only an outbound connection. Everything else was handled at Cloudflare's edge. For development environments and scenarios where you can't modify network security groups, this was a valid tradeoff.

The tradeoff became less acceptable at scale:

- **Latency**: every request from the platform to the workspace traveled through Cloudflare's network — extra hops, extra latency
- **Bandwidth costs**: Cloudflare metered tunnel egress; at agent-fleet scale this compounded
- **Single dependency**: if Cloudflare had an outage, every agent workspace lost its connection path simultaneously
- **No direct diagnostics**: you couldn't `curl` a workspace's IP directly or run network checks without the tunnel path

For teams running production agent fleets, these weren't hypothetical concerns.

## What's different now: public IP per workspace

Phase 33 provisions each workspace with its own public IP address from the VPC's public subnet. The connection model:

```
Your browser / API client
        │
        ▼
   Platform API (api.moleculesai.app)
        │  platform knows workspace IP from provisioning
        ▼
   AWS security group: platform-controlled inbound rules
        │  port 443 (WebSocket), authenticated by platform JWT
        ▼
   Agent workspace — public IP, direct WebSocket
```

The platform still handles auth and routing. But the data path no longer goes through Cloudflare's tunnel network — it's a direct TCP connection from client to workspace.

What changed in that retired design:

| | Cloudflare Tunnel (pre-Phase 33) | AWS direct connect (Phase 33 snapshot) |
|---|---|---|
| Workspace gets | Cloudflare-assigned hostname | Public IP from your VPC |
| Inbound connection | Outbound tunnel WebSocket only | Direct WebSocket on :443 |
| Firewall config | None required | Security group rules managed by platform |
| Latency | Extra Cloudflare hop | Direct — ~20–40ms reduction depending on region |
| Platform dependency | Cloudflare required for connectivity | Platform API still required for auth/routing; workspace IP works for direct curl |
| Debugging | Must go through tunnel | `curl https://<workspace-ip>` works directly |

## What operators needed to do (historical)

At the time, existing CP-managed workspaces in an operator's AWS account were
expected to transition automatically, with the platform managing the security
group rules.

**New provisioners:** the Phase 33 design assigned a public IP from the
workspace subnet while preserving the existing provisioning API.

**Existing self-hosted or Fly.io workspaces:** these were outside the Phase 33
control-plane provisioner path.

**If you have a custom VPC configuration:** the Phase 33 design expected a
workspace subnet with outbound internet access and a platform-managed security
group. This paragraph is retained as historical context, not as a current
network allowlist or operator procedure.

## What developers needed to know (historical)

From the agent runtime's perspective, the design intended to preserve the
platform API contract while changing the transport layer.

Changes described by that snapshot included:

- **Direct workspace access**: tools were expected to reach a workspace's
  public IP instead of the platform proxy.
- **WebSocket path**: the design retained an outbound workspace-to-platform
  WebSocket over the new network path.
- **CI/CD and health checks**: scripts were expected to address the public IP
  instead of a tunnel hostname.

## Security model at the time

The design assigned workspace security-group management to the platform and
intended to enforce:

- Port 443 only (no other inbound ports)
- TLS required on all connections
- JWT validation before any workspace data is served

The design did not manage VPC-level security groups beyond the
workspace-specific one. It expected inbound TCP 443 from the platform and
outbound TCP 443 to model-provider endpoints.

## Original rollout timing

The original plan called for new CP-managed provisions to use this topology
starting 2026-04-22 and for existing workspaces to migrate on restart.

That migration window and its runbook are closed. Use the current architecture
documents linked in the banner above.

---

*Archived Phase 33 design record; superseded and not current operator guidance.*

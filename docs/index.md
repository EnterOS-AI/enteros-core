---
layout: home

hero:
  name: Molecule AI
  text: Org-native operations for heterogeneous AI-agent workspaces
  tagline: A visual Canvas and authenticated control plane for durable roles, hierarchy, runtime integration, memory boundaries, and lifecycle operations.
  image:
    src: /assets/branding/molecule-icon.png
    alt: Molecule AI
  actions:
    - theme: brand
      text: Quick start
      link: /quickstart
    - theme: alt
      text: Architecture
      link: /architecture/architecture
    - theme: alt
      text: Platform API
      link: /api-protocol/platform-api

features:
  - title: Visual organization hierarchy
    details: Compose workspace roles through parent_id and inspect current state on Canvas. Team-view controls are non-destructive.
    icon: "🗺️"
  - title: Runtime boundary
    details: Pinned workspace templates integrate independently maintained runtimes behind one authenticated workspace contract.
    icon: "⚙️"
  - title: Scoped memory surfaces
    details: Distinct scoped-agent, key/value, activity-recall, and optional Memory v2 plugin surfaces with explicit ownership.
    icon: "🧠"
  - title: Operational control plane
    details: Registry, heartbeat, lifecycle, approvals, activity, secrets, files, terminal, bundles, and WebSocket fanout.
    icon: "🛡️"
  - title: External agent connections
    details: Server-stamped runtime-specific setup snippets support authenticated external registration and delivery without a shared operator host.
    icon: "🌐"
  - title: Reproducible catalog
    details: Template and plugin sources are pinned to immutable commits in manifest.json.
    icon: "📌"
---

## Current-state rules

- `manifest.json`, not a copied count, defines the current template/plugin
  catalog.
- `workspaces.parent_id` defines hierarchy. Visual collapse only hides or shows
  existing descendants.
- Postgres domain tables own durable current state; `structure_events` is
  selected history, not complete event sourcing.
- Runtime parsing and prompt assembly belong to the workspace-runtime package.
- Deployment topology is environment-specific and must be verified through the
  active Gitea workflow and runtime health surfaces.

## Recommended reading

- [Quick start](/quickstart)
- [System architecture](/architecture/architecture)
- [Core technical reference](/architecture/molecule-technical-doc)
- [Runtime / platform / plugin responsibilities](/architecture/runtime-platform-plugin-responsibilities)
- [Memory architecture](/architecture/memory)
- [Communication rules](/api-protocol/communication-rules)
- [Platform API](/api-protocol/platform-api)
- [Canvas](/frontend/canvas)
- [Local development](/development/local-development)

## Historical content

Dated blog posts and postmortems describe the system at the time they were
written. They are not current architecture or deployment references; use the
pages above and checked-in code for current behavior.

---
title: "Molecule AI Quick Start — Audio Guide"
description: "Audio walkthrough of the current Molecule AI local quick start, workspace creation, chat, and hierarchy controls."
tags: [onboarding, quickstart, audio]
---

## TTS Script

*Target: 65–75 seconds, en-US-AriaNeural*

---

Getting started with Molecule AI locally takes about five minutes.

Clone the core repository and run the development start script. It creates the
local environment file when needed and brings up the workspace server, Canvas,
and supporting services. Open the Canvas URL printed by the script; it chooses
another available port when port three thousand is already in use.

On first run, choose the server configuration appropriate for your environment.
Then create a workspace from Canvas. Pick one of the runtime templates that is
actually enabled in your deployment, give the workspace a role, and wait for it
to register.

Open chat and send the workspace a small task. A successful response proves the
UI, platform API, registry, and selected runtime can communicate end to end.

To organize more workspaces, create or drag them under a parent. The stored
parent I D defines both the org-chart hierarchy and peer-communication policy.
Expand Team View and Collapse Team View only change what Canvas displays; they
do not create, delete, or move workspaces.

That is Molecule AI: provider-independent agent runtimes, one live Canvas, and
an authenticated workspace hierarchy. Use the documentation in the source
repository for the current operational reference.

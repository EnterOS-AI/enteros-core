# Technology-choice principles

This repository intentionally avoids a fixed runtime/vendor scorecard. Runtime
support and infrastructure providers change independently; current choices must
be read from executable sources.

## Stable choices

- **Go/Gin workspace server:** explicit authenticated lifecycle, registry, A2A,
  storage, and backend-dispatch boundaries.
- **Next.js Canvas:** browser operational UI with server-backed current state and
  WebSocket fanout.
- **Postgres:** authoritative durable domain state.
- **Redis where configured:** liveness, cache, and fanout—not durable replay.
- **A2A JSON-RPC:** runtime-neutral message boundary.
- **Pinned template/runtime artifacts:** reproducible workspace boot instead of
  mutable branch installs.
- **Provider abstraction:** local and control-plane lifecycle implementations
  behind shared dispatchers.

## Sources of truth

- runtime/template catalog: `manifest.json`;
- runtime implementation: `molecule-ai-workspace-runtime` and exact template
  pins;
- API/auth: `workspace-server/internal/router/router.go`;
- deployment: active `.gitea/workflows/` and environment configuration; and
- optional tracing/exporters: exact runtime/template code.

Older LangGraph, DeepAgents, EC2, Railway, Langfuse, or vendor comparison text
is historical research, not the current platform contract.

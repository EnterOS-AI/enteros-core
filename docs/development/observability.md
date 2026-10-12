# Observability boundary

Core provides operational state through health/metrics, activity, current-task,
event, audit-read, and Canvas surfaces. Runtime-level LLM tracing is adapter and
environment dependent; this repository must not claim that every runtime emits
to a particular vendor automatically.

## Core-owned surfaces

- health and Prometheus metrics routes wired in
  `workspace-server/internal/router/router.go`;
- registry heartbeat and current-task state;
- `activity_logs` and the activity APIs;
- selected `structure_events` plus live WebSocket fanout; and
- Canvas operational views backed by those APIs.

`structure_events` is not a complete replay log. The audit-ledger endpoint is a
read surface with no current runtime producer, so its presence is not evidence
that agent actions are being recorded there.

## Runtime-owned tracing

Tracing exporters, SDK hooks, and provider credentials belong to the selected
runtime/template and its deployed environment. Verify support against the
runtime's current code and template pin before documenting an exporter as
enabled. Never infer universal LangGraph, Langfuse, LangSmith, or OpenTelemetry
behavior from an optional environment variable.

For debugging, correlate the exact workspace ID, activity/delegation ID, runtime
pin, Core commit, and terminal workflow run. Avoid joining unrelated 401s or
startup failures only because they occurred near the same time.

See [Event Log](../architecture/event-log.md), [Registry &
Heartbeat](../api-protocol/registry-and-heartbeat.md), and [Platform
API](../api-protocol/platform-api.md).

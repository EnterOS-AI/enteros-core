// Code generated — DO NOT EDIT.
//
// Vendored mirror of the SDK SSOT for the self-source `source_type` markers
// (RFC follow-up #29). Defined ONCE in molecule-ai-sdk at
// contracts/workspace-comms/self-source-types.schema.json and codegen'd into
// `@enteros/contracts` (gen/ts/workspace_comms_gen.ts `SELF_SOURCE_TYPES`) +
// the Go `molcontracts.SelfSourceTypes`.
//
// molecule-core's canvas has NO npm dependency on the SDK, so the generated
// list is vendored here as a values mirror. The SDK's contracts-codegen-drift
// gate guarantees the upstream list is derived from the schema and cannot
// drift; this vendored copy MUST carry the same values in the same order.
// The Go consumer (workspace-server/internal/messagestore) imports the same
// SSOT via the SDK Go module, so the two consumers cannot diverge.
//
// A message carrying one of these markers is a routine/internal SELF wake, not
// a human turn: it is classified as a system notice and must NEVER render as a
// blue user bubble. This CLASSIFICATION set is deliberately broader than the
// runtime's `_ROUTINE_SELF_SOURCE_TYPES` drop-not-queue governance subset
// (molecule_runtime/a2a_executor.py) — do not conflate them.
export const SELF_SOURCE_TYPES = [
  "self-cron",
  "self-harvester",
  "self-idle",
  "self-scheduler",
  "self-goal-nudge",
  "self-delegation-result",
  "self-warmup",
  "self-restart-context",
  "self-first-boot-greet",
  "self-lifecycle",
  "self-stall",
  "self-nudge",
] as const;

export type SelfSourceType = (typeof SELF_SOURCE_TYPES)[number];

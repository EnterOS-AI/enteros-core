package models

// runtime_defaults.go — single source of truth for per-runtime defaults
// the platform applies when the operator/agent didn't supply a value.
//
// Why this lives in models/ (not handlers/): default selection is a
// pure data fact about the runtime, not handler logic. Multiple
// callers (Create-workspace handler, org-import handler, future
// auto-provision paths) need the same answer; concentrating the
// rule here means one edit when a runtime's default changes.
//
// Related work (RFC #2873): this is the seed for a future
// `RuntimeConfig` interface that will also expose `ProvisioningTimeout()`,
// `CapabilitiesSupported()`, and other per-runtime facts. For now the
// surface is one helper — extracted from the duplicate branch in
// workspace_provision.go:537 and org_import.go:54 that diverged silently
// during refactors before this consolidation.

// DefaultModel returns the model slug to use when a workspace is
// created without an explicit model and the runtime can't infer one
// from its own config.
//
//   - claude-code: "sonnet" — Anthropic's CLI accepts the short
//     name and resolves it via the operator's anthropic-oauth or
//     ANTHROPIC_API_KEY chain.
//   - everything else (hermes, langgraph, crewai, autogen, deepagents,
//     codex, openclaw, gemini-cli, external, ""): a fully-qualified
//     vendor:model slug that the universal MODEL_PROVIDER chain in
//     molecule-core PR #247 can route via per-vendor required_env.
//
// The function never returns an empty string; an unknown runtime
// gets the universal default rather than failing closed (matches the
// pre-refactor behavior — both call sites used the same fallback).
func DefaultModel(runtime string) string {
	if runtime == "claude-code" {
		return "sonnet"
	}
	return "anthropic:claude-opus-4-7"
}

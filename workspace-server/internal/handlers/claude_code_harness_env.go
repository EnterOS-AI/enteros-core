package handlers

import (
	"log"
	"strings"
)

// Claude Code picks the model of every call that is not a main-loop turn from
// these variables: the small/fast tier (background calls such as titles and
// summaries, and WebFetch's page digest), the haiku/sonnet/opus aliases (the
// Explore subagent runs on haiku; a subagent or command may name any alias) and
// the subagent tier. Unset, each falls back to a built-in claude-* id. On a
// platform-managed workspace that id reaches the platform LLM proxy, which
// routes every claude-* id to Anthropic: those calls fail whenever the platform
// is not running on Anthropic, in the middle of a turn that started fine.
//
// The control plane computes the same set for the tenant's own default
// (platformManagedHarnessModelEnv in molecule-controlplane tenant_config.go)
// and the tenant server applies it to its own process (cmd/server/cp_config.go),
// but that value describes the TENANT default, not a given workspace: a
// workspace created on google/gemini-3.1-flash-lite must keep its background
// calls on google/gemini-3.1-flash-lite. So the provisioner derives the set from
// each workspace's own model instead (applyClaudeCodeHarnessModelEnv).
var claudeCodeHarnessModelEnvKeys = [...]string{
	"ANTHROPIC_SMALL_FAST_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL",
	"CLAUDE_CODE_SUBAGENT_MODEL",
}

// claudeCodeToolSearchEnvKey turns Claude Code's MCP tool search off. Tool
// search relies on Anthropic-only request features, so it is off whenever the
// model is not served by Anthropic (the control plane sets it the same way).
const claudeCodeToolSearchEnvKey = "ENABLE_TOOL_SEARCH"

// claudeCodeHarnessModelEnv returns the harness tier env for a claude-code
// workspace whose model is model, as the platform proxy serves it: every tier
// on model itself, in the exact form the main loop sends (so the proxy routes
// and bills the tier calls exactly like the main loop's), and tool search off.
// It returns nil for an empty model and for a model the proxy sends to
// Anthropic, whose built-in claude-* tier defaults already go to the same
// vendor.
func claudeCodeHarnessModelEnv(model string) map[string]string {
	model = strings.TrimSpace(model)
	if model == "" || claudeCodeModelServedByAnthropic(model) {
		return nil
	}
	env := make(map[string]string, len(claudeCodeHarnessModelEnvKeys)+1)
	for _, k := range claudeCodeHarnessModelEnvKeys {
		env[k] = model
	}
	env[claudeCodeToolSearchEnvKey] = "false"
	return env
}

// claudeCodeModelServedByAnthropic reports whether the platform LLM proxy sends
// model to Anthropic: an anthropic (or claude) namespace before the first "/"
// or ":", or a bare Claude id (claude-*, or one of the subscription aliases
// sonnet, opus and haiku). Every other id — google/*, minimax/*, platform/auto,
// a bare MiniMax or Kimi id — is served by another vendor or by Auto routing.
// This mirrors the proxy's routing of namespaced ids and its retained bare-id
// rule (inferLLMProvider in molecule-controlplane llm_proxy.go).
func claudeCodeModelServedByAnthropic(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.IndexAny(m, "/:"); i >= 0 {
		ns := m[:i]
		return ns == "anthropic" || ns == "claude"
	}
	switch m {
	case "sonnet", "opus", "haiku":
		return true
	}
	return strings.HasPrefix(m, "claude")
}

// applyClaudeCodeHarnessModelEnv sets the harness tier env of a claude-code
// workspace routed to the platform proxy, derived from that workspace's own
// final model (call it after every model-setting step of the provision). A
// tier variable already present in envVars — a workspace or org secret of that
// name — is kept: it is a deliberate choice. A no-op for every other runtime.
func applyClaudeCodeHarnessModelEnv(envVars map[string]string, workspaceID, runtime, model string) {
	if !runtimeUsesAnthropicNativeProxy(runtime) {
		return
	}
	set := 0
	for k, v := range claudeCodeHarnessModelEnv(model) {
		if _, present := envVars[k]; present {
			continue
		}
		envVars[k] = v
		set++
	}
	if set > 0 {
		log.Printf("workspace_provision: claude-code harness model tiers workspace=%s follow model %q (%d variable(s) set)", workspaceID, strings.TrimSpace(model), set)
	}
}

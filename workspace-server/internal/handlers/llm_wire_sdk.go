package handlers

import "go.moleculesai.app/sdk/llmwire"

// Re-export of the LLM billing-mode wire constants from the shared SSOT module
// go.moleculesai.app/sdk/llmwire. These are ALIASES, not copies: the values are
// defined once in the SDK and cannot drift from molecule-controlplane's copy
// (which imports the same SDK). This is the sanctioned per-repo "seam" — a thin
// re-export shim — so the ~100 existing references in package handlers keep
// working unqualified with zero call-site churn.
//
// nodup-lint (go.moleculesai.app/sdk/tools/cmd/nodup-lint) permits this file: a
// re-export's value is an import selector (llmwire.X), not a re-pasted string
// literal, and the file is also listed in .nodup-lint-allow.
const (
	LLMBillingModePlatformManaged = llmwire.LLMBillingModePlatformManaged
	LLMBillingModeBYOK            = llmwire.LLMBillingModeBYOK
	LLMBillingModeDisabled        = llmwire.LLMBillingModeDisabled
)

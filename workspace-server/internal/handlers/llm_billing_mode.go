package handlers

// llm_billing_mode.go — per-workspace LLM billing mode resolution (internal#691).
//
// The resolver answers a single question at provision time:
//   "Should we strip CLAUDE_CODE_OAUTH_TOKEN + every vendor key from this
//    workspace's env, force-route to the CP proxy, and bill org credits?"
//
// That question used to be a single env-var read inside applyPlatformManagedLLMEnv:
//
//   os.Getenv("MOLECULE_LLM_BILLING_MODE") == "platform_managed"  → strip
//
// where MOLECULE_LLM_BILLING_MODE was an ORG-level value, fetched from CP's
// tenant_config and exported into the workspace-server process at boot. That
// shape made it impossible to mix billing modes across workspaces in the same
// org: turning the org dial to `byok` so one workspace could keep its OAuth
// stops the strip for EVERY workspace in the org. Turning it to `platform_managed`
// blocks every workspace's own OAuth/vendor keys.
//
// The resolver replaces the env-var read with a per-workspace lookup:
//
//   workspaces.llm_billing_mode (per-workspace override, NULLABLE)
//     ?? organizations.llm_billing_mode (org default, fetched via tenant_config)
//     ?? "platform_managed" (closed default — the existing implicit default)
//
// Default-closed contract — non-negotiable per the RFC Safety axis:
//
//   - workspace row missing (sql.ErrNoRows)         → fall through to org default
//   - DB error on the lookup                         → "platform_managed" + propagated error
//   - workspace override = NULL                      → fall through to org default
//   - workspace override = unknown string            → "platform_managed" (default-closed)
//   - org default = NULL / empty / unknown string    → "platform_managed" (closed default)
//   - org default = recognized non-pm string + ws null → org default (byok/disabled honored)
//
// The ONLY way to resolve to "byok" or "disabled" is an explicit, recognized
// string in the workspace override OR the org default. A NULL JOIN, transient
// resolver error, or garbled enum value MUST NOT silently flip a workspace
// off of platform_managed — that would shadow the org's billing policy and
// is the exact failure mode the RFC's Safety hot-spot calls out.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// Constants mirror molecule-controlplane/internal/credits/llm_billing.go.
// Kept as string literals (not imports) because workspace-server has no
// build-time dependency on the CP module; the values are stable wire
// strings used in the tenant_config response, the workspaces.llm_billing_mode
// column check constraint, and the CP route bodies.
const (
	LLMBillingModePlatformManaged = "platform_managed"
	LLMBillingModeBYOK            = "byok"
	LLMBillingModeDisabled        = "disabled"
)

// BillingModeSource describes which layer of the resolution stack supplied
// the final mode. Surfaced via the admin route for operator debug
// ("why is this workspace being stripped?") per the RFC Observability axis.
type BillingModeSource string

const (
	BillingModeSourceWorkspaceOverride BillingModeSource = "workspace_override"
	BillingModeSourceOrgDefault        BillingModeSource = "org_default"
	BillingModeSourceConstantFallback  BillingModeSource = "constant_fallback"
)

// BillingModeResolution is the structured answer the admin GET route returns
// and the strip gate logs at INFO. The same struct is the unit-test fixture
// shape, so the resolver test asserts both the mode AND the source per case
// (catches a bug where the right mode is returned via the wrong layer).
type BillingModeResolution struct {
	WorkspaceID       string             `json:"workspace_id"`
	ResolvedMode      string             `json:"resolved_mode"`
	WorkspaceOverride *string            `json:"workspace_override"` // nil = inherit
	OrgDefault        string             `json:"org_default"`        // already default-closed by CP
	Source            BillingModeSource  `json:"source"`
}

// isKnownBillingMode is the enum-recognizer for the resolver's default-closed
// branch. Returning false for an unknown string forces the resolver to fall
// through to the next layer (or the constant fallback) — NEVER to honor a
// garbled value as if it were valid. This is what makes a row with mode='byokk'
// (typo) resolve to platform_managed instead of accidentally to byok.
func isKnownBillingMode(s string) bool {
	switch s {
	case LLMBillingModePlatformManaged, LLMBillingModeBYOK, LLMBillingModeDisabled:
		return true
	default:
		return false
	}
}

// normalizeOrgDefault applies the same default-closed contract to the
// org-level input as the workspace override gets. The org_default arrives
// from tenant_config which already COALESCEs NULL → platform_managed at the
// CP SQL layer, but we DO NOT trust that contract here — if CP regresses or
// the tenant_config env wasn't populated (race on boot), we still default-
// close. Same principle: never honor a garbled value.
func normalizeOrgDefault(orgMode string) string {
	if isKnownBillingMode(orgMode) {
		return orgMode
	}
	return LLMBillingModePlatformManaged
}

// ResolveLLMBillingMode is the canonical resolver. Every code path that
// previously gated on `os.Getenv("MOLECULE_LLM_BILLING_MODE") == "platform_managed"`
// must call this instead and gate on the returned mode. The architectural
// test (resolver_ast_test.go) asserts there is no remaining call site of
// the old shape outside the resolver-input wiring.
//
// Returning an error does NOT prevent the caller from making a decision —
// the returned mode is always a valid enum value (default-closed to
// platform_managed) so the caller can proceed without a separate fail-closed
// branch. The error is informational: log it, surface it to operators, but
// the strip-gate decision is already safe.
func ResolveLLMBillingMode(ctx context.Context, workspaceID, orgMode string) (BillingModeResolution, error) {
	res := BillingModeResolution{
		WorkspaceID: workspaceID,
		OrgDefault:  normalizeOrgDefault(orgMode),
	}

	if workspaceID == "" {
		// No workspace ID = pre-provision context (templating, validation).
		// Resolve against the org default only, no DB read.
		res.ResolvedMode = res.OrgDefault
		res.Source = BillingModeSourceOrgDefault
		if !isKnownBillingMode(orgMode) {
			// Org default was garbled/NULL and we clamped to platform_managed.
			// Mark the source as constant_fallback so the operator can see
			// the clamp happened, not that the org "really" said platform_managed.
			res.Source = BillingModeSourceConstantFallback
		}
		return res, nil
	}

	var wsOverride sql.NullString
	err := db.DB.QueryRowContext(ctx,
		`SELECT llm_billing_mode FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&wsOverride)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Workspace row missing — concurrent delete, or pre-create call. Don't
		// silently flip; fall through to org default. Source stays org_default
		// so operators can see the row-missing case is being handled as a
		// fallback, not a workspace-explicit decision.
		res.ResolvedMode = res.OrgDefault
		res.Source = BillingModeSourceOrgDefault
		if !isKnownBillingMode(orgMode) {
			res.Source = BillingModeSourceConstantFallback
		}
		return res, nil
	case err != nil:
		// DB error — default-closed to platform_managed AND propagate the
		// error so operators get a structured log line. The caller is
		// expected to log and continue with the safe default.
		res.ResolvedMode = LLMBillingModePlatformManaged
		res.Source = BillingModeSourceConstantFallback
		return res, fmt.Errorf("resolve workspace llm_billing_mode for %s: %w", workspaceID, err)
	}

	if wsOverride.Valid && isKnownBillingMode(wsOverride.String) {
		mode := wsOverride.String
		res.WorkspaceOverride = &mode
		res.ResolvedMode = mode
		res.Source = BillingModeSourceWorkspaceOverride
		return res, nil
	}

	// Override row present but the value is NULL or garbled. Fall through.
	// If the value was non-NULL but garbled (CHECK constraint should prevent
	// this, but defense in depth — a future migration could relax the check
	// or another path could write the column directly), surface the raw
	// override value so operators can spot the corrupt row.
	if wsOverride.Valid {
		raw := wsOverride.String
		res.WorkspaceOverride = &raw
	}
	res.ResolvedMode = res.OrgDefault
	res.Source = BillingModeSourceOrgDefault
	if !isKnownBillingMode(orgMode) {
		res.Source = BillingModeSourceConstantFallback
	}
	return res, nil
}

// SetWorkspaceLLMBillingMode writes the override column. Pass mode=="" to
// clear (set to NULL = inherit). Validates the mode against the enum set
// so the route handler doesn't have to duplicate validation; a garbled
// mode round-trips as an explicit 400 from the caller, not a CHECK-
// constraint error from the DB driver.
func SetWorkspaceLLMBillingMode(ctx context.Context, workspaceID, mode string) error {
	if workspaceID == "" {
		return errors.New("SetWorkspaceLLMBillingMode: workspace id required")
	}
	if mode == "" {
		// NULL = inherit. Caller asked to clear the override.
		res, err := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET llm_billing_mode = NULL WHERE id = $1`,
			workspaceID,
		)
		if err != nil {
			return fmt.Errorf("clear workspace llm_billing_mode for %s: %w", workspaceID, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return sql.ErrNoRows
		}
		return nil
	}
	if !isKnownBillingMode(mode) {
		return fmt.Errorf("unknown billing mode %q (allowed: %s, %s, %s)",
			mode, LLMBillingModePlatformManaged, LLMBillingModeBYOK, LLMBillingModeDisabled)
	}
	res, err := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET llm_billing_mode = $1 WHERE id = $2`,
		mode, workspaceID,
	)
	if err != nil {
		return fmt.Errorf("set workspace llm_billing_mode for %s: %w", workspaceID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

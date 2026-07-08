package handlers

// platform_agent_flow.go — the ONE canonical platform-agent (concierge)
// lifecycle flow (core#3496, operator ruling 2026-07-07).
//
// Before this file, the shared logic was only the bottom half
// (installPlatformAgent — the transactional row upsert + re-parent + anchor
// migration). The top halves had drifted into three implementations: the
// ensure handler carried decide/revive/provision, the self-host boot seed
// re-implemented exists-checking, and the CP install endpoint was a third thin
// wrapper. ensurePlatformAgentFlow is now the single decide → install →
// name/model → revive → provision pipeline; every entrypoint is a thin
// adapter:
//
//   - POST /admin/org/platform-agent/ensure  (EnsurePlatformAgent — canvas +
//     the self-host onboarding scene; provision on)
//   - boot seed                              (EnsureSelfHostedPlatformAgent —
//     row-only; the boot provision is phase 2, MaybeProvisionPlatformAgentOnBoot)
//   - POST /admin/org/platform-agent         (InstallPlatformAgent — the CP's
//     contract-frozen row-only shim; does NOT use the decide step on purpose,
//     see its doc comment)
//
// The flow is gin-free and returns typed errors so adapters can map them to
// their own wire contracts without this file knowing about HTTP.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
)

// platformRuntimeAllowed reports whether runtime may back the kind='platform'
// org root, with a human-readable reason when it may not.
//
// Why this guard exists (SSOT audit, 2026-07-07): the AdminAuth install/ensure
// endpoints used to stamp the payload runtime VERBATIM. POST ensure
// {runtime:"external"} "succeeded" — row upserted kind='platform'
// runtime='external', response claimed provisioning:true — but the provision
// path silently no-ops for external-like runtimes and external-only machinery
// (healthsweep's awaiting_agent flip, a2a-proxy branches) adopted the row: a
// permanently wedged concierge behind a lying API response. The public
// /registry/register path already 403s kind='platform'; these endpoints were
// the unguarded hole.
//
// Empty runtime is allowed — it means "use the platform default"
// (conciergeDefaultRuntime), which validates itself against isKnownRuntime.
func platformRuntimeAllowed(runtime string) (bool, string) {
	rt := strings.TrimSpace(runtime)
	if rt == "" {
		return true, ""
	}
	if !isKnownRuntime(rt) {
		return false, fmt.Sprintf("unknown runtime %q — not in the runtime registry", rt)
	}
	if isExternalLikeRuntime(rt) {
		return false, fmt.Sprintf("runtime %q is a BYO-compute meta-runtime with no container for the platform provision path — the org root must be a container-backed runtime", rt)
	}
	if rt == "mock" {
		return false, `runtime "mock" is the canned demo runtime — not allowed for the org root`
	}
	return true, ""
}

// flowReject is the 422-class validation rejection: the request is well-formed
// but names something the registry/guard refuses. Adapters map it to their
// wire shape (the HTTP handlers use 422 {error, code}).
type flowReject struct {
	Code    string // stable machine code, e.g. RUNTIME_UNSUPPORTED
	Message string
}

func (e *flowReject) Error() string { return e.Code + ": " + e.Message }

// flowStageError wraps an infrastructure failure with the pipeline stage that
// produced it ("lookup" | "install" | "model" | "revive") so adapters can keep
// their historical per-stage error responses byte-identical.
type flowStageError struct {
	Stage string
	Err   error
}

func (e *flowStageError) Error() string { return e.Stage + " failed: " + e.Err.Error() }
func (e *flowStageError) Unwrap() error { return e.Err }

// ensureSetModelFn writes the concierge's MODEL workspace_secret. Defaults to
// the real setModelSecret; tests override it to assert the model write happens
// (and happens BEFORE the provision trigger — the race the onboarding scene
// design closes) without a real Postgres.
var ensureSetModelFn = setModelSecret

// ensureFlowOptions parameterizes one ensurePlatformAgentFlow run.
type ensureFlowOptions struct {
	// Name is the concierge display name; empty defaults to
	// defaultPlatformAgentName().
	Name string
	// Runtime is used ONLY when the row is freshly inserted —
	// installPlatformAgent deliberately preserves an existing root's runtime on
	// conflict (a runtime change goes through the standard PATCH path). Empty
	// defaults to conciergeDefaultRuntime(). Must pass platformRuntimeAllowed.
	Runtime string
	// Model, when set, is validated against the EFFECTIVE runtime (the existing
	// row's runtime when repairing, else Runtime/default) and persisted as the
	// MODEL workspace_secret BEFORE any provision trigger fires. Seed-only
	// downstream: ensureConciergeModel respects an existing MODEL secret, so
	// this pick sticks. Empty = leave the model channel untouched.
	Model string
	// Force repairs even a healthy 'online' concierge (the repair-tool path).
	Force bool
	// SkipTombstoned turns a repair-of-removed decision into a no-op instead of
	// a revive. The BOOT SEED sets this: a tombstoned concierge was DELIBERATELY
	// deleted, and an unattended boot must never silently un-delete it (the
	// long-standing install-path contract). Reviving stays an explicit caller
	// action — the ensure endpoint (canvas button / onboarding scene) leaves
	// this false.
	SkipTombstoned bool
	// HasProvisioner gates whether the decision may call for a provision.
	HasProvisioner bool
	// ProvisionTrigger fires the async provision when the decision calls for
	// one. Nil = row-only run (the boot seed's phase 1; the CP shim).
	ProvisionTrigger func(id string)
}

// ensureFlowOutcome reports what the flow decided and did.
type ensureFlowOutcome struct {
	Action      ensureAction
	PriorStatus string // the pre-existing root's status ("" when none)
	// Skipped is non-empty when the flow deliberately stopped before side
	// effects ("tombstoned" — SkipTombstoned met a removed root).
	Skipped string
}

// ensurePlatformAgentFlow is the canonical create/repair pipeline:
//
//	guard runtime → lookup root → decide → validate model → install →
//	write model → revive tombstone → trigger provision
//
// Ordering is load-bearing: the MODEL secret is committed BEFORE the provision
// trigger so the very first provision resolves the caller's model instead of
// racing it (the self-host default fallback is a platform-proxy arm that
// aborts MISSING_PLATFORM_PROXY on self-host — the exact first-run failure the
// onboarding scene exists to prevent).
func ensurePlatformAgentFlow(ctx context.Context, database *sql.DB, opts ensureFlowOptions) (ensureFlowOutcome, error) {
	var out ensureFlowOutcome

	if ok, why := platformRuntimeAllowed(opts.Runtime); !ok {
		return out, &flowReject{Code: "RUNTIME_UNSUPPORTED", Message: why}
	}

	derivedID := PlatformAgentID()

	// Root lookup — INCLUDING tombstones; see platformRootLookupQuery's doc
	// comment for both the tombstone contract and the enum-scan hazard.
	var existingID, existingStatus string
	found := false
	err := database.QueryRowContext(ctx, platformRootLookupQuery).
		Scan(&existingID, &existingStatus)
	switch {
	case err == nil:
		found = true
	case errors.Is(err, sql.ErrNoRows):
		found = false
	default:
		return out, &flowStageError{Stage: "lookup", Err: err}
	}
	out.PriorStatus = existingStatus

	decision := decideEnsureAction(derivedID, existingID, existingStatus, found, opts.Force, opts.HasProvisioner)
	out.Action = decision
	if decision.status == "exists" {
		return out, nil // healthy + not forced: idempotent no-op
	}
	if decision.revive && opts.SkipTombstoned {
		out.Skipped = "tombstoned"
		return out, nil // deliberate deletion respected — no unattended revive
	}

	// Validate the model BEFORE any side effect, against the runtime the row
	// will actually have: an existing root keeps its own runtime (the install
	// upsert preserves it), a fresh insert gets opts.Runtime/default.
	model := strings.TrimSpace(opts.Model)
	if model != "" {
		effRuntime := strings.TrimSpace(opts.Runtime)
		if found {
			if rt, rtErr := platformRootRuntime(ctx, database, existingID); rtErr != nil {
				return out, &flowStageError{Stage: "lookup", Err: rtErr}
			} else if rt != "" {
				effRuntime = rt
			}
		}
		if effRuntime == "" {
			effRuntime = conciergeDefaultRuntime()
		}
		if ok, why := validateRegisteredModelForRuntime(effRuntime, model); !ok {
			return out, &flowReject{Code: "UNREGISTERED_MODEL_FOR_RUNTIME", Message: why}
		}
	}

	callerName := strings.TrimSpace(opts.Name)
	name := callerName
	if name == "" {
		name = defaultPlatformAgentName()
	}
	if err := ensureInstallFn(ctx, database, decision.targetID, name, opts.Runtime); err != nil {
		return out, &flowStageError{Stage: "install", Err: err}
	}

	// An EXPLICIT caller name must stick on an EXISTING root too: the install
	// upsert deliberately preserves name on conflict (correct for the CP shim
	// and the defaulted boot seed), so without this step the onboarding
	// scene's fixed brand name would silently never apply to the
	// always-seeded root. Must land BEFORE the provision trigger — the
	// {{CONCIERGE_NAME}} persona substitution reads the row name at provision.
	// Defaulted (empty opts.Name) callers never rename.
	if callerName != "" && found {
		if _, err := database.ExecContext(ctx,
			`UPDATE workspaces SET name = $2, updated_at = now() WHERE id = $1 AND name IS DISTINCT FROM $2`,
			decision.targetID, callerName); err != nil {
			return out, &flowStageError{Stage: "name", Err: err}
		}
	}

	// MODEL secret lands before the provision trigger — see the flow doc.
	if model != "" {
		if err := ensureSetModelFn(ctx, decision.targetID, model); err != nil {
			return out, &flowStageError{Stage: "model", Err: err}
		}
	}

	// Explicit revive of a tombstoned root — the ONE path allowed to
	// un-tombstone a concierge (see reviveRemovedPlatformAgent).
	if decision.revive {
		if err := reviveRemovedPlatformAgent(ctx, database, decision.targetID); err != nil {
			return out, &flowStageError{Stage: "revive", Err: err}
		}
	}

	if decision.provision && opts.ProvisionTrigger != nil {
		opts.ProvisionTrigger(decision.targetID)
	}
	return out, nil
}

// platformRootRuntime reads the existing platform root's runtime column (the
// value the install upsert will preserve) so the flow can validate a model
// pick against the runtime the concierge will actually run.
func platformRootRuntime(ctx context.Context, database *sql.DB, id string) (string, error) {
	var rt string
	err := database.QueryRowContext(ctx,
		`SELECT COALESCE(runtime, '') FROM workspaces WHERE id = $1`, id).Scan(&rt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return strings.TrimSpace(rt), err
}

// SelfHostPlatformSeedEnabled reports whether this deployment self-seeds the
// platform agent at boot: true exactly when MOLECULE_ORG_ID is unset (self-host
// / local — no control plane exists to install the concierge).
//
// This replaces the removed MOLECULE_SEED_PLATFORM_AGENT opt-in flag
// (core#3496, operator ruling 2026-07-07: "we should always have a tenant
// agent... the first agent and manager agent should always be the concierge").
// On SaaS/CP tenants and CI harnesses MOLECULE_ORG_ID is always set, so they
// never self-seed — byte-identical to the old flag-unset behavior there.
func SelfHostPlatformSeedEnabled() bool {
	return strings.TrimSpace(os.Getenv("MOLECULE_ORG_ID")) == ""
}

// platformAgentModelConfigured reports whether the platform root has ANY model
// signal a boot provision could resolve: a MODEL workspace_secret (a prior
// user/scene pick — seed-only, never clobbered) or the MOLECULE_LLM_DEFAULT_MODEL
// env (the headless setup path).
//
// Used by the boot provision's unconfigured-skip (spec D2): a keyless,
// model-less fresh self-host would provision straight into a guaranteed
// MISSING_MODEL / MISSING_PLATFORM_PROXY abort on every boot. Skipping keeps
// the root parked at 'offline' for the onboarding scene to configure instead
// of burning a failed provision. Fail-open: on a lookup error we ATTEMPT the
// provision (the old behavior) — the provision path fail-closes properly on
// its own.
func platformAgentModelConfigured(ctx context.Context, database *sql.DB, id string) bool {
	if strings.TrimSpace(os.Getenv("MOLECULE_LLM_DEFAULT_MODEL")) != "" {
		return true
	}
	var one int
	err := database.QueryRowContext(ctx,
		`SELECT 1 FROM workspace_secrets WHERE workspace_id = $1 AND key = 'MODEL' LIMIT 1`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	return true // found, or fail-open on error
}

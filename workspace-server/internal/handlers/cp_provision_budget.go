package handlers

// cp_provision_budget.go — the CP-mode provision budget, the create-path image
// pre-flight, and the failure reason that is allowed to reach a customer.
//
// All three exist because of one incident class: a workspace provisioned within
// minutes of a runtime-image promote. The control plane resolves the pin at
// PROVISION time, so that workspace is the one host in the fleet that has to
// obtain a freshly promoted 6.89GB image before it can come up — and the tenant
// was giving it three minutes.

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// ---------------------------------------------------------------------------
// the budget
// ---------------------------------------------------------------------------

// cpProvisionLegReserve is the slice of the provision context that
// cpCreatePrewarmBudget must leave UNSPENT for the provision POST itself.
//
// The pre-warm and the provision run in series on one deadline, so a pre-flight
// permitted to spend the whole budget would starve the call it exists to make
// succeed — the same shape of error as the inversion this file fixes, one level
// in. 5 minutes is generous against the measured design point: once the
// pre-warm has answered, the image is present on the daemon that will run the
// workspace and the provision that follows is a cache hit (measured at 13s in
// core#5019).
var cpProvisionLegReserve = 5 * time.Minute

// cpProvisionTimeout resolves the absolute deadline for the CP-mode provision
// context.
//
// It is the MAX of provisioner.CPProvisionCeiling() — the provision HTTP
// client's OWN budget — and any per-runtime provision_timeout_seconds the
// template manifest declares (the same value the sweep and the canvas spinner
// read via ProvisionTimeoutSecondsForRuntime).
//
// The floor is the load-bearing half and it is a FLOOR, not a target: a context
// shorter than the client budget means the context, never the client, is what
// fires. The client's timeout becomes unreachable, core#5019's widening becomes
// decorative, and the failure lands on the control plane as `context canceled`
// mid-pull rather than as a legible client timeout here.
//
// This is the same migration the Docker-mode sibling already made:
// dockerProvisionTimeout replaced the fixed 3-minute provisioner.ProvisionTimeout
// with max(per-runtime, 12m) after "capping real builds at 3 min here bricked a
// hermes concierge provision". CP mode was simply missed at the time, and it is
// the mode production runs.
func (h *WorkspaceHandler) cpProvisionTimeout(runtime string) time.Duration {
	d := provisioner.CPProvisionCeiling()
	if secs := h.ProvisionTimeoutSecondsForRuntime(runtime); secs > 0 {
		if perRuntime := time.Duration(secs) * time.Second; perRuntime > d {
			d = perRuntime
		}
	}
	return d
}

// cpCreatePrewarmBudget carves the create pre-flight's budget out of the total
// provision budget, always leaving cpProvisionLegReserve for the provision POST.
//
// The ceiling is read from restartPrewarmBudget rather than re-declared, so the
// create and restart pre-flights cannot drift into two different numbers that
// both look deliberate. Exceeding it does NOT fail the create — see
// ensurePinnedImageBeforeProvision for why the two paths diverge there.
func cpCreatePrewarmBudget(total time.Duration) time.Duration {
	b := restartPrewarmBudget
	if lim := total - cpProvisionLegReserve; b > lim {
		b = lim
	}
	if b < 0 {
		return 0
	}
	return b
}

// ---------------------------------------------------------------------------
// the create-path pre-flight
// ---------------------------------------------------------------------------

// ensurePinnedImageBeforeProvision asks the control plane to make this
// workspace's pinned image obtainable BEFORE the provision that needs it.
// It returns "" to proceed, or the sanitised reason to fail with.
//
// CPProvisioner.EnsureImage was built for exactly this and was wired only on
// the RESTART path (ensurePinnedImageBeforeStop). The create path — a fresh org,
// a new customer's first workspace — never asked. That is the path the control
// plane names `stage=create_workspace` in the failures this fixes.
//
// It is NOT ensurePinnedImageBeforeStop, and the difference is the whole point:
// that helper protects a RUNNING CONTAINER. Its decision rule ("anything other
// than success declines") and its wire log ("the workspace was NOT stopped; it
// keeps its running container") are both statements about something a create has
// not got. Reusing it here would have declined creates over a transient CP blip
// and logged a reassuring lie about a container that never existed.
//
// The create-path rule, and which way each branch fails:
//
//	CP confirms                    -> PROCEED, and the provision is a cache hit
//	CP has no such endpoint        -> PROCEED (deliberate fail-OPEN, version skew)
//	CP UNDERSTOOD and refused      -> FAIL, with the image-specific reason
//	anything else (transport, 5xx) -> PROCEED, logged
//
// The last branch is where this inverts relative to the restart path. A
// momentarily unreachable control plane says nothing about the image; on restart
// that ambiguity is resolved in favour of the container the user still has,
// while on create there is nothing to protect and refusing would turn a
// 60-second CP redeploy into a fleet-wide create outage. The provision leg is
// itself a real attempt, on a budget that now actually covers a pull, with its
// own bounded ctx-aware retry (core#5057).
//
// A permanent refusal fails CLOSED and FAST. Retrying it returns the same answer
// — and, more to the point, re-pulling 6.89GB into the same wall is not a fix.
//
// SINGLE ATTEMPT by design. CPProvisioner.EnsureImage carries no retry loop of
// its own, and adding one here would spend the pre-flight budget on a question
// whose answer the provision leg is about to ask again anyway.
//
// On the RESTART path this runs a second time, after ensurePinnedImageBeforeStop
// already asked. That is deliberate, not an oversight. EnsureImage is documented
// idempotent — an image already present short-circuits on the local digest check
// without touching the registry — so the repeat costs one cheap round trip, and
// it is asked at a strictly better moment: the first call answered BEFORE the
// container was destroyed, this one answers immediately before the provision
// that needs it. Suppressing it would mean threading "already asked" state
// through every path into provisionWorkspaceCP, which is the shape that let the
// create path go unguarded in the first place.
func (h *WorkspaceHandler) ensurePinnedImageBeforeProvision(ctx context.Context, workspaceID string, payload models.CreateWorkspacePayload, budget time.Duration) string {
	if h.cpProv == nil {
		return ""
	}
	prewarm := cpCreatePrewarmBudget(budget)
	if prewarm <= 0 {
		// A budget too small to leave the provision leg its reserve: skip the
		// pre-flight rather than starve the call that actually creates the box.
		log.Printf("CPProvisioner: %s create pre-warm skipped — provision budget %s leaves no room above the %s provision-leg reserve",
			workspaceID, budget, cpProvisionLegReserve)
		return ""
	}
	// Its OWN deadline, derived from the provision ctx. Without this the
	// pre-flight would inherit the full provision budget and could consume all
	// of it, leaving the provision POST to be cancelled by the same context —
	// which is the bug this file exists to remove, reintroduced from inside.
	prewarmCtx, cancel := context.WithTimeout(ctx, prewarm)
	defer cancel()

	res, err := h.cpProv.EnsureImage(prewarmCtx, provisioner.EnsureImageRequest{
		WorkspaceID: workspaceID,
		Runtime:     payload.Runtime,
		Template:    payload.Template,
		// core#5025: the provider the PROVISION will use, read off the same
		// payload the provision receives. Left unset, the wire names no backend,
		// the control plane resolves its SSOT default, and it answers
		// "not_applicable" for a workspace on another substrate — a 200 from a
		// guard that guarded nothing.
		Provider: payload.Compute.Provider,
	})
	switch {
	case err == nil:
		log.Printf("CPProvisioner: %s pinned image ready before provision (runtime=%q template=%q status=%q ref=%q budget=%s) — the provision that follows should be a cache hit",
			workspaceID, payload.Runtime, payload.Template, res.Status, res.ImageRef, prewarm)
		return ""
	case errors.Is(err, provisioner.ErrEnsureImageUnsupported):
		log.Printf("CPProvisioner: %s control plane has no ensure-image endpoint — creating without a pre-warm (runtime=%q). Upgrade the control plane to close the cold-adoption window.",
			workspaceID, payload.Runtime)
		return ""
	case errors.Is(err, provisioner.ErrEnsureImagePermanent):
		// LOUD and greppable. The control plane understood the question and said
		// no; the provision would fail identically, minutes later, with less
		// information.
		log.Printf("CREATE-IMAGE-UNOBTAINABLE workspace_id=%s runtime=%q template=%q err=%q — the provision was NOT attempted; the control plane refused the pinned image (core#5019)",
			workspaceID, payload.Runtime, payload.Template, err.Error())
		return provisionFailedImageUnavailable
	default:
		log.Printf("CPProvisioner: %s pre-warm did not complete (%v) — proceeding with the provision anyway; an unreachable control plane says nothing about the image, and on the create path there is no running container to protect",
			workspaceID, err)
		return ""
	}
}

// ---------------------------------------------------------------------------
// the failure reason
// ---------------------------------------------------------------------------

// The reasons a CP-mode provision failure may publish. Every one of them is a
// compile-time constant, and that is the entire sanitisation mechanism — see
// cpProvisionFailureReason.
const (
	// provisionFailedGeneric is the pre-existing catch-all. It stays, as the
	// default for failures nothing has classified yet; what changes is that it
	// is no longer the ONLY thing this path can say.
	provisionFailedGeneric = "provisioning failed"

	// provisionFailedImageUnavailable covers the incident this change was
	// written for: the control plane could not obtain the workspace's pinned
	// runtime image. Worded for the person reading a canvas card — it names the
	// thing that did not happen and implies the wait, without naming a registry,
	// a digest or a host.
	provisionFailedImageUnavailable = "image_unavailable: the workspace runtime image could not be obtained — this is expected briefly after a runtime-image promote; retry the workspace shortly"

	// provisionFailedRuntimePinMissing is ACTIONABLE IN A DIFFERENT WAY from a
	// slow pull — the operator has to promote a runtime image, not wait — so it
	// is its own bucket. The control plane already separates them (422
	// RUNTIME_PIN_MISSING); collapsing them here would hand an operator the
	// wrong runbook.
	provisionFailedRuntimePinMissing = "runtime_pin_missing: no runtime image is pinned for this workspace's runtime — promote a runtime image, then retry"

	// provisionFailedBudgetExhausted is what the ceiling looks like from the
	// outside. A budget with no legible boundary behaviour is just a longer
	// silence; this is the boundary, said out loud.
	provisionFailedBudgetExhausted = "provision_timeout: the control plane did not finish provisioning within the provision budget"
)

// cpProvisionFailureReasons returns the CLOSED SET of values
// cpProvisionFailureReason can return. Tests assert the range against it, which
// is what keeps the guarantee below structural rather than aspirational.
func cpProvisionFailureReasons() []string {
	return []string{
		provisionFailedGeneric,
		provisionFailedImageUnavailable,
		provisionFailedRuntimePinMissing,
		provisionFailedBudgetExhausted,
	}
}

// cpProvisionFailureReason maps a control-plane provision error to the short,
// specific, machine-agnostic sentence that goes to workspaces.last_sample_error
// and to the WORKSPACE_PROVISION_FAILED broadcast.
//
// WHY THIS REPLACES A CONSTANT. The path published "provisioning failed" for
// every failure. F1086 / #1206 is the reason it published something canned —
// CP errors can carry machine types, AMI ids, VPC and subnet ids, host paths —
// and that requirement is not in question here. But it was implemented by
// DELETING the reason rather than by BOUNDING it, and the cost was five weeks in
// which the single most common provision failure in production, a runtime image
// that could not be pulled after a promote, was indistinguishable — to the gate
// AND to the customer's canvas — from a bad model id or a missing backend. The
// leak guard exists to keep instance metadata out. It was never meant to
// suppress "the image did not arrive".
//
// HOW THE SANITISATION IS PROVEN. It is STRUCTURAL, not textual. The error is
// only ever CLASSIFIED — matched against fixed, non-parameterised phrases the
// control plane emits — and the return value is always one of the constants
// above. No part of the error is copied, interpolated, truncated or reflected,
// so there is no marker to remember and no denylist to keep current. The full
// error is logged server-side by the caller, exactly as before.
//
// Ordering is deliberate: the most specific cause wins. An image failure whose
// text also contains "context canceled" (the production signature) is an IMAGE
// failure, not a timeout.
func cpProvisionFailureReason(err error) string {
	if err == nil {
		return provisionFailedGeneric
	}
	if errors.Is(err, provisioner.ErrEnsureImagePermanent) {
		return provisionFailedImageUnavailable
	}
	s := strings.ToLower(err.Error())
	contains := func(needles ...string) bool {
		for _, n := range needles {
			if strings.Contains(s, n) {
				return true
			}
		}
		return false
	}
	switch {
	// The CP's own 422 code, and the label its metrics bucket it under.
	case contains("runtime_pin_missing"):
		return provisionFailedRuntimePinMissing
	// The CP's image-resolution family: its metrics reason (image_resolution),
	// the local-docker prose ("is not available locally and pull failed"), the
	// stall-runner's verdict, and the digest-mismatch tail.
	case contains(
		"image resolution",
		"image_resolution",
		"pull failed",
		"pull stalled",
		"not available locally",
		"did not resolve to expected digest",
	):
		return provisionFailedImageUnavailable
	// The ceiling, seen from the caller: our own context gave up, or the HTTP
	// client did.
	case contains(
		"context deadline exceeded",
		"client.timeout exceeded",
		"context canceled",
		"context cancelled",
	):
		return provisionFailedBudgetExhausted
	default:
		return provisionFailedGeneric
	}
}

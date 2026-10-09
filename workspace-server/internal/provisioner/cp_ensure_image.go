package provisioner

// cp_ensure_image.go — core#5019, the "pull before stopping" seam.
//
// The control plane resolves a workspace's image from runtime_image_pins at
// PROVISION time, so adopting a newly promoted pin requires obtaining that
// image (~7GB). Pre-#5019 the tenant destroyed the running container first and
// only then asked CP to provision, which is where the image was needed. When
// the image could not be obtained, the workspace was left with nothing.
//
// EnsureImage is the pre-flight that closes that: it asks the control plane to
// make the workspace's pinned image present on whichever backend will run it,
// BEFORE the tenant stops anything. The pull happens CP-side (the tenant has no
// docker.sock into the provisioner daemon, and core must not take a dependency
// on the proprietary control plane — so this is an ordinary HTTP call on the
// existing /cp/workspaces/* provisioner seam, not an import).
//
// The tenant's decision rule is deliberately tiny; the control plane owns the
// semantics:
//
//	2xx                -> the CP has answered "you may stop"
//	404 / 501          -> this CP predates the endpoint; behave as before
//	anything else, or
//	a transport error  -> DO NOT STOP
//
// Failing open on 404/501 is the one deliberate exception. Failing closed there
// would wedge every restart on the fleet the moment a tenant ran ahead of the
// control plane during a rollout — a far larger outage than the one being
// fixed. Every other outcome fails CLOSED, because the only thing the user
// still has at that point is the running container.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// ErrEnsureImageUnsupported reports a control plane that does not implement
// POST /cp/workspaces/ensure-image (it answered 404 or 501). Callers treat it
// as "proceed as before" — it is a version skew, not an image problem.
var ErrEnsureImageUnsupported = errors.New("cp provisioner: ensure-image not supported by this control plane")

// ErrEnsureImagePermanent marks a refusal that RETRYING CANNOT FIX: the control
// plane understood the question and answered no. A digest that is not in the
// registry, a workspace the CP will not resolve, a credential it rejects — three
// more round trips return the same answer, and each one is spent holding the
// per-workspace restart gate.
//
// Errors WITHOUT this marker (transport failures, 5xx, 429, an undecodable body)
// say nothing about the image; they say the control plane is momentarily
// unavailable. Callers retry those. See ensurePinnedImageBeforeStop.
var ErrEnsureImagePermanent = errors.New("cp provisioner: ensure-image refused")

// EnsureImageClientTimeout exposes the ensure-image HTTP budget so callers can
// prove their own deadline actually bounds something. Without it, a caller
// "budget" larger than this constant bounds nothing at all — the client gives up
// first — and the test asserting the budget would pass while the gate was still
// held for the full client timeout.
func EnsureImageClientTimeout() time.Duration { return cpEnsureImageTimeout }

// isCPRouteFamilyLive probes a route the control plane has served since long
// before ensure-image existed, to distinguish "this CP predates the endpoint"
// from "this URL does not reach the control plane at all".
//
// core#5025 finding 5: any 404 used to be read as version skew and FAIL OPEN. A
// misconfigured base URL, a stale ingress rule or a proxy that lost the route
// therefore degraded the whole fleet back to the pre-core#5019 destroy-then-pull
// ordering — silently, while logging a reassuring compatibility skip. That is
// the single most expensive way for this guard to be wrong, because it is
// invisible.
//
// The detection is POSITIVE: a 404 counts as version skew only when the control
// plane demonstrably serves /cp/workspaces/* and merely lacks this one route. A
// CP too old to have ensure-image cannot emit a marker for a route it does not
// have, so the signal has to come from a route it DOES have. Any non-404 answer
// (including 4xx/5xx — those prove something is listening and routing) counts as
// live; a second 404, or an unreachable host, does not.
func (p *CPProvisioner) isCPRouteFamilyLive(ctx context.Context, workspaceID string) bool {
	client := p.httpClient
	if client == nil {
		return false
	}
	u := fmt.Sprintf("%s/cp/workspaces/%s/status", p.baseURL, workspaceID)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return false
	}
	p.provisionAuthHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode != http.StatusNotFound
}

// cpEnsureImageTimeout bounds the ensure-image POST.
//
// Its own constant, NOT a reuse of cpProvisionTimeout, even though the two
// currently carry the same value: they bound different operations and the
// lesson of #5019/#5020 is precisely that one budget covering two different
// jobs hides the failure of the slower one. This call may have to pull a ~7GB
// image on a cold host; the provision that follows it should then be fast
// (measured: 13s once the image was local).
const cpEnsureImageTimeout = 20 * time.Minute

// EnsureImageRequest asks the control plane to make a workspace's pinned image
// obtainable. It deliberately carries the same identity fields the provision
// request does, so CP resolves EXACTLY the pin that the subsequent provision
// will use — asking about a different resolution than the one that will run is
// a vacuous guard.
type EnsureImageRequest struct {
	OrgID       string `json:"org_id"`
	WorkspaceID string `json:"workspace_id"`
	Runtime     string `json:"runtime"`
	Template    string `json:"template,omitempty"`
	Provider    string `json:"provider,omitempty"`
}

// EnsureImageResult is the control plane's answer. Status is CP-owned prose for
// the tenant's log — the tenant branches on the HTTP status, never on this
// string, so CP can add statuses without a coordinated tenant release.
type EnsureImageResult struct {
	// Status is "ready" when the image is present on the daemon that will run
	// the workspace, or a CP-defined value such as "not_applicable" for a
	// backend that pulls at box boot and therefore cannot be pre-warmed by CP.
	Status string `json:"status"`
	// ImageRef is the exact digest-pinned ref CP made ready, when it has one.
	ImageRef string `json:"image_ref,omitempty"`
	// Error carries CP's structured reason on a refusal.
	Error string `json:"error,omitempty"`
}

// providerForWorkspace resolves the compute backend a workspace's box runs on.
//
// An explicit caller choice always wins: the provider-switch flow stops a box on
// one backend and provisions the next on ANOTHER, so preferring the persisted
// value there would act on the backend being abandoned. Otherwise the value is
// read off the workspace row through resolveProvider — the SAME seam Stop
// already uses, so the three legs of one restart (pre-warm, stop, provision)
// cannot resolve three different boxes.
//
// Fail-SOFT: a DB hiccup returns the explicit value (usually "") rather than an
// error. The alternative — failing the call — would make the pre-flight decline
// the restart, so a transient DB blip would wedge restarts fleet-wide. An
// unresolved provider is no worse than the pre-core#5025 wire.
func (p *CPProvisioner) providerForWorkspace(ctx context.Context, workspaceID, explicit string) string {
	if explicit != "" || workspaceID == "" {
		return explicit
	}
	provider, err := resolveProvider(ctx, workspaceID)
	if err != nil {
		log.Printf("CP provisioner: could not resolve the compute provider for %s: %v — proceeding without it (the control plane will fall back to its own default)", workspaceID, err)
		return explicit
	}
	return provider
}

// ensureImageStatusIsPermanent reports whether a non-2xx status is the control
// plane ANSWERING (retrying returns the same answer) rather than the control
// plane being UNAVAILABLE (retrying is the whole point).
//
// 4xx means the request was understood and refused — the core#5019 unobtainable
// digest arrives as 422 and must fail closed immediately rather than spend three
// backoffs holding the restart gate. The exceptions are the two 4xx codes that
// are explicitly "try again": 408 and 429.
func ensureImageStatusIsPermanent(status int) bool {
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
		return false
	}
	return status >= 400 && status < 500
}

// EnsureImage asks the control plane to make this workspace's pinned image
// obtainable before the caller stops anything.
//
// Returns ErrEnsureImageUnsupported for a control plane without the endpoint.
// Any other non-nil error means the caller MUST NOT stop the workspace.
func (p *CPProvisioner) EnsureImage(ctx context.Context, req EnsureImageRequest) (EnsureImageResult, error) {
	if p == nil {
		return EnsureImageResult{}, ErrNoBackend
	}
	if req.OrgID == "" {
		req.OrgID = p.orgID
	}
	// core#5025: without this the `provider` field was ALWAYS absent on the
	// ensure-image wire, and an absent provider is the SSOT default (AWS). The
	// control plane therefore pre-warmed — or rather declined to pre-warm — an
	// AWS box for a workspace that runs on the local-docker substrate, answered
	// "not_applicable" with 200, and the tenant read that 2xx as permission to
	// destroy the container. The guard reported success on every restart while
	// guarding nothing.
	req.Provider = p.providerForWorkspace(ctx, req.WorkspaceID, req.Provider)

	body, err := json.Marshal(req)
	if err != nil {
		return EnsureImageResult{}, fmt.Errorf("cp provisioner: ensure-image marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/cp/workspaces/ensure-image", bytes.NewReader(body))
	if err != nil {
		return EnsureImageResult{}, fmt.Errorf("cp provisioner: ensure-image create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	p.provisionAuthHeaders(httpReq)

	client := p.ensureImageHTTPClient
	if client == nil {
		// Never silently fall back to the 120s small-JSON budget: this call may
		// legitimately spend minutes pulling. An unwired client is a wiring bug,
		// and answering it with a deadline that is certain to fire on a cold
		// pull would recreate #5019 through the back door.
		client = &http.Client{Timeout: cpEnsureImageTimeout}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return EnsureImageResult{}, fmt.Errorf("cp provisioner: ensure-image send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 64 KiB cap, same rationale as Start: the CP returns small JSON and an
	// unbounded read off a compromised upstream is a log-flood DoS.
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil {
		return EnsureImageResult{}, fmt.Errorf("cp provisioner: ensure-image read response: %w", readErr)
	}
	var result EnsureImageResult
	unmarshalErr := json.Unmarshal(respBody, &result)

	// 501 is a POSITIVE signal on its own: the control plane routed the request
	// and told us it does not implement it. 404 is not — see
	// isCPRouteFamilyLive.
	if resp.StatusCode == http.StatusNotImplemented {
		return result, ErrEnsureImageUnsupported
	}
	if resp.StatusCode == http.StatusNotFound {
		if p.isCPRouteFamilyLive(ctx, req.WorkspaceID) {
			return result, ErrEnsureImageUnsupported
		}
		// The control plane does not answer on /cp/workspaces/* at all, so this
		// 404 came from a proxy, a stale route or a wrong base URL — not from a
		// CP that predates the endpoint. Fail CLOSED. Treating it as version
		// skew would put the entire fleet back on the core#5019 ordering while
		// the log said "compatibility skip".
		return result, fmt.Errorf(
			"%w: ensure-image answered 404 and %s/cp/workspaces/* does not route either — "+
				"this is a misrouted or misconfigured control-plane URL, not a control plane that "+
				"predates the endpoint", ErrEnsureImagePermanent, p.baseURL)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Prefer the structured {"error": …}. Never echo the raw body: an
		// upstream misconfiguration that reflected the request could otherwise
		// put the provision bearer into our logs (same guard as Start).
		msg := result.Error
		if msg == "" {
			msg = fmt.Sprintf("<unstructured body, %d bytes>", len(respBody))
		}
		if ensureImageStatusIsPermanent(resp.StatusCode) {
			return result, fmt.Errorf("%w (%d): %s", ErrEnsureImagePermanent, resp.StatusCode, msg)
		}
		return result, fmt.Errorf("cp provisioner: ensure-image failed (%d): %s", resp.StatusCode, msg)
	}
	if unmarshalErr != nil {
		return EnsureImageResult{}, fmt.Errorf("cp provisioner: ensure-image decode %d response: %w", resp.StatusCode, unmarshalErr)
	}
	return result, nil
}

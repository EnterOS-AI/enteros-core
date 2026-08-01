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
	"net/http"
	"time"
)

// ErrEnsureImageUnsupported reports a control plane that does not implement
// POST /cp/workspaces/ensure-image (it answered 404 or 501). Callers treat it
// as "proceed as before" — it is a version skew, not an image problem.
var ErrEnsureImageUnsupported = errors.New("cp provisioner: ensure-image not supported by this control plane")

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

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented {
		return result, ErrEnsureImageUnsupported
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Prefer the structured {"error": …}. Never echo the raw body: an
		// upstream misconfiguration that reflected the request could otherwise
		// put the provision bearer into our logs (same guard as Start).
		msg := result.Error
		if msg == "" {
			msg = fmt.Sprintf("<unstructured body, %d bytes>", len(respBody))
		}
		return result, fmt.Errorf("cp provisioner: ensure-image failed (%d): %s", resp.StatusCode, msg)
	}
	if unmarshalErr != nil {
		return EnsureImageResult{}, fmt.Errorf("cp provisioner: ensure-image decode %d response: %w", resp.StatusCode, unmarshalErr)
	}
	return result, nil
}

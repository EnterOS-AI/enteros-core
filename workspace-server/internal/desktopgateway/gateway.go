// Package desktopgateway is the platform-side ENFORCEMENT layer of the
// computer-use 3-layer split (design RFC §9): auth, control-lock arbitration,
// and scale-from-zero. It sits between the agent-facing tool and the sidecar's
// control server. It is NOT an agent tool itself — it is the gateway the tool
// (and the re-pointed runtime desktop tool) call.
package desktopgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// LockChecker arbitrates the display-control lock (design §8). INPUT goes through
// AcquireAgentControl; screenshots are not gated (sight is never arbitrated).
type LockChecker interface {
	// AcquireAgentControl grants (or refreshes) the AGENT's control lease unless
	// a HUMAN currently holds control — capability-IS-authorization: the agent
	// drives the desktop by default, and a human takes over by preempting
	// (AcquireDisplayControl). Returns true if the agent holds control after the
	// call, false if a human does. This is what makes /input actually work: a
	// passive "does the agent hold control?" check is always false because
	// nothing else grants the agent a lease (reviewer B2).
	AcquireAgentControl(ctx context.Context, workspaceID string) (bool, error)
}

// ActivityRecorder bumps last_agent_activity_at — the authoritative liveness
// signal that keeps the desktop from being reaped while the agent works (§10).
type ActivityRecorder interface {
	RecordAgentActivity(ctx context.Context, workspaceID string) error
}

// TokenResolver returns the per-sidecar inbound bearer for a workspace (§6.5).
type TokenResolver func(ctx context.Context, workspaceID string) (string, error)

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// ErrHumanInControl is returned from Input when the agent does not hold the
// control lock. The agent must PAUSE (§8), not fight the human for the one
// cursor. Fail-closed: any lock ambiguity denies input.
var ErrHumanInControl = errors.New("desktop input refused: agent does not hold control")

// Gateway proxies computer-use operations to the sidecar control server.
type Gateway struct {
	prov     provisioner.SidecarProvisioner
	locks    LockChecker
	activity ActivityRecorder
	http     httpDoer
	token    TokenResolver
}

// New builds the gateway. doer defaults to http.DefaultClient when nil.
func New(prov provisioner.SidecarProvisioner, locks LockChecker, activity ActivityRecorder, token TokenResolver, doer httpDoer) *Gateway {
	if doer == nil {
		doer = http.DefaultClient
	}
	return &Gateway{prov: prov, locks: locks, activity: activity, http: doer, token: token}
}

// Screenshot proxies GET /screenshot. Sight is NEVER arbitrated (§8): the agent
// can always see, even while a human drives. Scale-from-zero on first use;
// records activity (screenshots count, §10).
func (g *Gateway) Screenshot(ctx context.Context, workspaceID string) ([]byte, error) {
	addr, err := g.ensureRunning(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	g.recordActivity(ctx, workspaceID)
	req, err := g.newReq(ctx, workspaceID, http.MethodGet, addr, "/screenshot", nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sidecar screenshot: status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// Input proxies POST /input, FAIL-CLOSED on the control lock (§8): the agent
// acquires/refreshes its control lease (yielding to a human who has preempted),
// or the input is refused with ErrHumanInControl.
func (g *Gateway) Input(ctx context.Context, workspaceID string, action json.RawMessage) error {
	held, err := g.locks.AcquireAgentControl(ctx, workspaceID)
	if err != nil {
		return err // ambiguity -> fail closed (deny)
	}
	if !held {
		return ErrHumanInControl
	}
	addr, err := g.ensureRunning(ctx, workspaceID)
	if err != nil {
		return err
	}
	g.recordActivity(ctx, workspaceID)
	req, err := g.newReq(ctx, workspaceID, http.MethodPost, addr, "/input", bytes.NewReader(action))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("sidecar input: status %d", resp.StatusCode)
	}
	return nil
}

// ensureRunning scales the desktop from zero if needed and returns the sidecar
// address. StartDesktop is idempotent, so this is safe whether or not the
// desktop is already up. On an unwired backend it returns
// provisioner.ErrDesktopBackendUnavailable, which the caller surfaces as a
// per-tier availability gate (decision 4).
func (g *Gateway) ensureRunning(ctx context.Context, workspaceID string) (string, error) {
	h, err := g.prov.StartDesktop(ctx, provisioner.WorkspaceConfig{WorkspaceID: workspaceID})
	if err != nil {
		return "", err
	}
	if h.Address == "" {
		return "", fmt.Errorf("desktop started but no reachable address")
	}
	// SSRF allowlist (§13): confirm the backend-resolved address is EXACTLY a
	// desktop-sidecar target before any authed request is forwarded to it, so a
	// compromised/misconfigured provisioner address can never point the gateway
	// at the cloud metadata IP, a backend service, or an arbitrary host.
	if err := provisioner.ValidateSidecarTarget(h.Address); err != nil {
		return "", fmt.Errorf("refusing to forward to an invalid desktop target: %w", err)
	}
	return h.Address, nil
}

func (g *Gateway) recordActivity(ctx context.Context, workspaceID string) {
	if g.activity != nil {
		_ = g.activity.RecordAgentActivity(ctx, workspaceID) // best-effort; never blocks the op
	}
}

func (g *Gateway) newReq(ctx context.Context, workspaceID, method, addr, path string, body io.Reader) (*http.Request, error) {
	tok, err := g.token(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return req, nil
}

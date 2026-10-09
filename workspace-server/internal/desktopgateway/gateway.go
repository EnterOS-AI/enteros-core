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
	"net/url"
	"sync"
	"time"

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

// StateRecorder persists the desktop lifecycle state so scale-to-zero can FIND
// running desktops to evaluate for teardown (§10). Optional / nil-safe: when
// unset, ensureRunning simply never records 'running', and the idle sweeper has
// nothing to reap. Wired via SetStateRecorder rather than the constructor so
// existing callers stay source-compatible.
type StateRecorder interface {
	SetState(ctx context.Context, workspaceID, state, sidecarAddress string) error
}

// VNCPresenceRecorder persists the live human-viewer count so the idle sweeper
// does not reap a desktop that a human is actively watching (DesktopIsIdle
// treats VNCConnections>0 as live). Optional / nil-safe, wired via
// SetVNCPresenceRecorder. Without it, a human opening the viewer records NO
// liveness — and because the idle timer runs off the last AGENT action (often
// already stale when a human takes over), the desktop is reaped out from under
// the viewer on the next sweep (~1 min). That is the "take over the display and
// lose it" bug.
type VNCPresenceRecorder interface {
	SetVNCPresence(ctx context.Context, workspaceID string, connections int, lastInput time.Time) error
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
	state    StateRecorder // optional; nil disables 'running' state recording
	http     httpDoer
	token    TokenResolver

	// recordedMu guards recordedRunning: the set of workspaces whose 'running'
	// lifecycle state we have CONFIRMED-persisted this process. It lets
	// ensureRunning skip the DB write on steady-state forwards (already recorded)
	// while still retrying until the write succeeds — so a single failed
	// best-effort write on the scale-up edge can't permanently hide the desktop
	// from the idle sweeper (it would never be reaped otherwise).
	recordedMu      sync.Mutex
	recordedRunning map[string]struct{}

	// vncPresence + viewers track live human viewers so the idle sweeper never
	// reaps a desktop a human is watching. viewers is the in-process count per
	// workspace (the authoritative live count for this process); each change is
	// mirrored to the store via vncPresence. Optional; nil vncPresence disables
	// the DB mirror (the counter still runs, harmlessly).
	viewersMu   sync.Mutex
	viewers     map[string]int
	vncPresence VNCPresenceRecorder
}

// New builds the gateway. doer defaults to http.DefaultClient when nil.
func New(prov provisioner.SidecarProvisioner, locks LockChecker, activity ActivityRecorder, token TokenResolver, doer httpDoer) *Gateway {
	if doer == nil {
		doer = http.DefaultClient
	}
	return &Gateway{prov: prov, locks: locks, activity: activity, http: doer, token: token, recordedRunning: make(map[string]struct{}), viewers: make(map[string]int)}
}

// SetStateRecorder wires the optional lifecycle-state setter used to mark a
// desktop 'running' on scale-up, so the idle sweeper can later find and reap it
// (§10). Nil-safe: leaving it unset just disables state recording.
func (g *Gateway) SetStateRecorder(s StateRecorder) { g.state = s }

// SetVNCPresenceRecorder wires the optional human-viewer presence sink so a live
// display session suppresses idle teardown. Nil-safe.
func (g *Gateway) SetVNCPresenceRecorder(v VNCPresenceRecorder) { g.vncPresence = v }

// ViewerConnected records that a human display viewer has connected, keeping the
// desktop alive for as long as anyone is watching. Call once per websockify
// session, balanced by ViewerDisconnected. Best-effort mirror to the store.
func (g *Gateway) ViewerConnected(ctx context.Context, workspaceID string) {
	g.viewersMu.Lock()
	g.viewers[workspaceID]++
	n := g.viewers[workspaceID]
	g.viewersMu.Unlock()
	g.mirrorViewers(ctx, workspaceID, n)
}

// ViewerDisconnected records that a human viewer's session ended. When the last
// viewer leaves, presence drops to zero and the desktop becomes eligible for the
// normal idle teardown again.
func (g *Gateway) ViewerDisconnected(ctx context.Context, workspaceID string) {
	g.viewersMu.Lock()
	if g.viewers[workspaceID] > 0 {
		g.viewers[workspaceID]--
	}
	n := g.viewers[workspaceID]
	if n == 0 {
		delete(g.viewers, workspaceID)
	}
	g.viewersMu.Unlock()
	g.mirrorViewers(ctx, workspaceID, n)
}

func (g *Gateway) mirrorViewers(ctx context.Context, workspaceID string, n int) {
	if g.vncPresence == nil {
		return
	}
	// Detach from the request context: ViewerDisconnected runs as the websocket
	// tears down, when the request ctx is already cancelled — a cancelled ctx must
	// not skip the "drop to zero" write, or a stale positive count would pin the
	// desktop alive forever. Bounded so a stuck DB can't leak the goroutine.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = g.vncPresence.SetVNCPresence(wctx, workspaceID, n, time.Now()) // best-effort
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
	// Check the control lock FIRST, before any scale-up. AcquireAgentControl
	// returns held=false when a human holds control; in that case we must refuse
	// with ErrHumanInControl (-> 409) WITHOUT booting the desktop. Doing
	// ensureRunning first would scale a reaped 2GB sidecar back from zero just to
	// refuse the input, and would surface a scale-up/egress error as a 5xx
	// "input failed" instead of the correct 409 "human in control" — misleading a
	// client that pauses on 409 but retries on 5xx (verified 2026-07-28). When the
	// agent legitimately holds control (held=true) the lease write is warranted,
	// and only then do we ensure the desktop is up.
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

// ErrInvalidNavigateURL is returned by HumanNavigate for a non-http(s) or
// hostless URL — surfaced as a 400 by the handler.
var ErrInvalidNavigateURL = errors.New("navigate requires an http(s) URL with a host")

// HumanNavigate points the desktop browser at url on behalf of a HUMAN who holds
// display control. Unlike Input it does NOT arbitrate the agent lock — the human
// IS the controller, and the platform handler authorizes them via their display
// control token before calling this. Chromium runs --kiosk (no address bar), so
// a URL bar in the human view is the only way for a person taking over to
// browse; the control server actuates it by launching `chromium <url>` on the
// running instance's profile, which has NO effect on the agent coordinate
// contract. The scheme is re-checked here (defense in depth) even though the
// control server also restricts navigation to http(s).
func (g *Gateway) HumanNavigate(ctx context.Context, workspaceID, rawURL string) error {
	if u, err := url.Parse(rawURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ErrInvalidNavigateURL
	}
	addr, err := g.ensureRunning(ctx, workspaceID)
	if err != nil {
		return err
	}
	g.recordActivity(ctx, workspaceID)
	action, err := json.Marshal(struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}{Type: "navigate", URL: rawURL})
	if err != nil {
		return err
	}
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
		return fmt.Errorf("sidecar navigate: status %d", resp.StatusCode)
	}
	return nil
}

// EnsureRunning is the exported entry point for callers OUTSIDE the agent
// screenshot/input path — specifically the human display path, which must bring
// the desktop up when a human opens the viewer even if no agent has started it.
// It goes through the same ensureRunning as the agent path, so scale-up records
// the 'running' lifecycle state (the idle sweeper can still reap it) rather than
// starting an unrecorded sidecar that would leak.
func (g *Gateway) EnsureRunning(ctx context.Context, workspaceID string) (string, error) {
	return g.ensureRunning(ctx, workspaceID)
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
	// Record 'running' so scale-to-zero can find this desktop to evaluate for
	// teardown (§10) — but avoid the per-forward DB write that re-upserted the
	// unchanged state on every screenshot/input. Write when EITHER this call scaled
	// the desktop up (h.ScaledUp) OR we have not yet confirmed the 'running' state
	// is persisted (a prior best-effort write failed, or this is the first forward
	// after a process restart). In steady state (already recorded) this is a no-op.
	// Best-effort: a write failure must not fail the op the caller asked for and is
	// simply retried on the next forward — crucially, a single failed transition
	// write can no longer permanently hide the desktop from the sweeper (which
	// would otherwise never reap the 2GB sidecar). The setter is optional (nil-safe).
	if g.state != nil {
		g.recordedMu.Lock()
		_, recorded := g.recordedRunning[workspaceID]
		g.recordedMu.Unlock()
		if h.ScaledUp || !recorded {
			if err := g.state.SetState(ctx, workspaceID, "running", h.Address); err == nil {
				g.recordedMu.Lock()
				g.recordedRunning[workspaceID] = struct{}{}
				g.recordedMu.Unlock()
			}
		}
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

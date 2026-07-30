package provisioner

import "time"

// DesktopActivity is the observed liveness signal for a workspace's desktop,
// used to decide scale-to-zero teardown.
//
// CRITICAL correctness note (design §10, from the adversarial review): the
// AGENT drives the desktop over the HTTP control server and grabs the
// framebuffer directly — it opens ZERO VNC connections and injects input only
// intermittently (a computer-use loop is mostly screenshot -> reason -> wait).
// So a VNC-connection-count or X-idle signal ALONE would tear the desktop out
// from under a hard-working agent. The authoritative liveness signal is agent
// control-server activity (screenshots count — an agent that is only looking
// is still using the desktop), OR an explicit computer-use lease, OR any live
// human VNC presence.
type DesktopActivity struct {
	// LastAgentActivity is the last time the computer-use tool hit the control
	// server (a screenshot OR an input). Screenshots count.
	LastAgentActivity time.Time
	// LastVNCInput is the last time a human injected input over VNC.
	LastVNCInput time.Time
	// VNCConnections is the number of live human VNC viewers.
	VNCConnections int
	// AgentLeaseHeld is true while the computer-use tool loop holds an explicit
	// lease — suppresses teardown regardless of input cadence (covers long
	// reasoning / page-load gaps, and the §6 pause where the agent stops
	// sending input while a human drives).
	AgentLeaseHeld bool
}

// DesktopIsIdle reports whether a workspace's desktop should be scaled to zero.
// It is idle only when EVERY liveness signal is cold: no held agent lease, no
// live VNC viewer, and both the last agent activity and last human VNC input
// older than idleTimeout. A held lease or any live viewer keeps it up
// unconditionally. idleTimeout <= 0 disables teardown (never idle).
//
// now is passed in (not time.Now) so the decision is pure and unit-testable.
func DesktopIsIdle(now time.Time, a DesktopActivity, idleTimeout time.Duration) bool {
	if idleTimeout <= 0 {
		return false // teardown disabled
	}
	if a.AgentLeaseHeld {
		return false // the agent loop explicitly holds the desktop
	}
	if a.VNCConnections > 0 {
		return false // a human is watching or driving
	}
	cutoff := now.Add(-idleTimeout)
	if a.LastAgentActivity.After(cutoff) {
		return false // agent touched the control server within the window
	}
	if a.LastVNCInput.After(cutoff) {
		return false // recent human input
	}
	return true
}

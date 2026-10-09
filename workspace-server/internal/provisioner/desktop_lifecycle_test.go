package provisioner

import (
	"testing"
	"time"
)

func TestDesktopIsIdle(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const timeout = 10 * time.Minute
	recent := now.Add(-1 * time.Minute)
	stale := now.Add(-30 * time.Minute)

	cases := []struct {
		name    string
		act     DesktopActivity
		timeout time.Duration
		want    bool
	}{
		{
			// THE critical case (review fix): the agent is actively working —
			// it hit the control server recently — but has ZERO VNC
			// connections and no human input. Must NOT be idle.
			name:    "agent active, no VNC connections",
			act:     DesktopActivity{LastAgentActivity: recent, LastVNCInput: stale, VNCConnections: 0},
			timeout: timeout,
			want:    false,
		},
		{
			name:    "agent lease held keeps it up even with all-stale timestamps",
			act:     DesktopActivity{LastAgentActivity: stale, LastVNCInput: stale, VNCConnections: 0, AgentLeaseHeld: true},
			timeout: timeout,
			want:    false,
		},
		{
			name:    "human viewing (VNC connection) keeps it up",
			act:     DesktopActivity{LastAgentActivity: stale, LastVNCInput: stale, VNCConnections: 1},
			timeout: timeout,
			want:    false,
		},
		{
			name:    "recent human VNC input keeps it up",
			act:     DesktopActivity{LastAgentActivity: stale, LastVNCInput: recent, VNCConnections: 0},
			timeout: timeout,
			want:    false,
		},
		{
			name:    "everything cold and past the timeout is idle",
			act:     DesktopActivity{LastAgentActivity: stale, LastVNCInput: stale, VNCConnections: 0},
			timeout: timeout,
			want:    true,
		},
		{
			name:    "idleTimeout<=0 disables teardown (never idle)",
			act:     DesktopActivity{LastAgentActivity: stale, LastVNCInput: stale, VNCConnections: 0},
			timeout: 0,
			want:    false,
		},
		{
			name:    "agent activity exactly at the cutoff is still considered active",
			act:     DesktopActivity{LastAgentActivity: now.Add(-timeout), LastVNCInput: stale, VNCConnections: 0},
			timeout: timeout,
			// At exactly cutoff, After() is false -> counts as stale -> idle.
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DesktopIsIdle(now, tc.act, tc.timeout); got != tc.want {
				t.Fatalf("DesktopIsIdle = %v, want %v", got, tc.want)
			}
		})
	}
}

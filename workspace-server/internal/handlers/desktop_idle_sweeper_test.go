package handlers

import (
	"context"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

type fakeIdleSource struct {
	running []string
	acts    map[string]provisioner.DesktopActivity
}

func (f *fakeIdleSource) RunningDesktopWorkspaceIDs(context.Context) ([]string, error) {
	return f.running, nil
}
func (f *fakeIdleSource) LoadActivity(_ context.Context, id string) (provisioner.DesktopActivity, error) {
	return f.acts[id], nil
}

func TestDesktopIdleSweeper_ReapsOnlyIdle(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const timeout = 10 * time.Minute

	src := &fakeIdleSource{
		running: []string{"idle-ws", "busy-ws", "watched-ws"},
		acts: map[string]provisioner.DesktopActivity{
			// idle: agent + human input both stale, no viewers -> reap.
			"idle-ws": {LastAgentActivity: now.Add(-30 * time.Minute), LastVNCInput: now.Add(-30 * time.Minute)},
			// busy: agent hit the control server recently (zero VNC conns) -> keep.
			"busy-ws": {LastAgentActivity: now.Add(-1 * time.Minute)},
			// watched: a human is viewing -> keep.
			"watched-ws": {LastAgentActivity: now.Add(-30 * time.Minute), VNCConnections: 1},
		},
	}

	var torn []string
	sw := NewDesktopIdleSweeper(src, func(_ context.Context, id string) error {
		torn = append(torn, id)
		return nil
	}, timeout)
	sw.now = func() time.Time { return now }

	if err := sw.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(torn) != 1 || torn[0] != "idle-ws" {
		t.Fatalf("expected only idle-ws torn down, got %v", torn)
	}
}

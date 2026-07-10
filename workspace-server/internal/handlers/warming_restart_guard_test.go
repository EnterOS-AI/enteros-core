package handlers

import (
	"context"
	"errors"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
)

// The core#3082 warming guard: refuse to auto-restart a LIVE warming platform
// concierge (the self-perpetuating restart loop that kept it from ever reaching
// online), while still allowing the first provision + all non-warming restarts.
func TestIsLiveWarmingPlatformConcierge(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		status  string
		running bool
		err     error
		want    bool
	}{
		{"live warming platform -> SKIP restart", models.KindPlatform, string(models.StatusProvisioning), true, nil, true},
		{"first provision (no container yet) -> allow", models.KindPlatform, string(models.StatusProvisioning), false, nil, false},
		{"non-platform provisioning -> allow", models.KindWorkspace, string(models.StatusProvisioning), true, nil, false},
		{"platform online -> allow (not warming)", models.KindPlatform, string(models.StatusOnline), true, nil, false},
		{"platform failed -> allow (recover path)", models.KindPlatform, string(models.StatusFailed), true, nil, false},
		{"probe error -> fail-open (allow)", models.KindPlatform, string(models.StatusProvisioning), true, errors.New("backend blip"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isLiveWarmingPlatformConcierge(c.kind, c.status, c.running, c.err); got != c.want {
				t.Errorf("isLiveWarmingPlatformConcierge(%q,%q,%v,%v) = %v; want %v", c.kind, c.status, c.running, c.err, got, c.want)
			}
		})
	}
}

func TestWorkspaceIsRunningUsesCPProvisioner(t *testing.T) {
	cp := &fakeCPProv{running: true}
	h := &WorkspaceHandler{cpProv: cp}

	running, err := h.workspaceIsRunning(context.Background(), "ws-cp-concierge")
	if err != nil {
		t.Fatalf("workspaceIsRunning returned error: %v", err)
	}
	if !running {
		t.Fatal("workspaceIsRunning = false, want true from CP provisioner")
	}
	if cp.Calls() != 1 {
		t.Fatalf("CP IsRunning calls = %d, want 1", cp.Calls())
	}
}

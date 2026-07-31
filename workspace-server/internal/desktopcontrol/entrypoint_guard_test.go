package desktopcontrol

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestDesktopEntrypoint_LoadBearingInvariants is a cheap, per-PR guard on the
// desktop sidecar entrypoint (scripts/desktop-sidecar-entrypoint.sh). That file
// is a shell script — no `go test ./...` coverage — yet it carries invariants
// whose removal SILENTLY reintroduces production bugs we already hit and fixed:
//
//   - SingletonLock cleanup before Chromium launch: the persistent profile
//     survives scale-to-zero, so a recycled sidecar re-mounts a profile still
//     holding the previous container's Chromium singleton guard; Chromium then
//     REFUSES TO START ("profile in use by another Chromium on another
//     computer") and the browser is dead (black screen, reads as "no internet").
//   - websockify --heartbeat: without periodic WS pings an IDLE noVNC connection
//     (static screen -> no framebuffer traffic) is dropped by an upstream idle
//     timeout -> "take over the display, idle, lose the connection".
//   - Chromium --proxy-server=$DESKTOP_PROXY: the sidecar's ONLY route off its
//     internal network; dropping it either kills egress or (worse) leaks it.
//   - Chromium --kiosk: the coordinate contract assumes a full-screen window
//     with no toolbar; a windowed/toolbar'd Chromium mis-offsets every agent
//     click.
//
// If you intentionally change any of these, update BOTH the entrypoint and this
// guard — that is the point: the change becomes a reviewed decision, not a
// silent regression.
func TestDesktopEntrypoint_LoadBearingInvariants(t *testing.T) {
	path := filepath.Join("..", "..", "scripts", "desktop-sidecar-entrypoint.sh")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read entrypoint %s: %v", path, err)
	}
	script := string(raw)

	// The SingletonLock cleanup must appear BEFORE the chromium launch, else the
	// running instance's own fresh lock would be deleted out from under it.
	lockIdx := regexp.MustCompile(`rm -f[^\n]*SingletonLock`).FindStringIndex(script)
	if lockIdx == nil {
		t.Fatal("entrypoint no longer clears the stale Chromium SingletonLock — a recycled sidecar's browser will refuse to start (the 'no internet'/black-screen bug). See exec_actuator.go / RFC §8.")
	}
	chromiumIdx := regexp.MustCompile(`(?m)^\s*chromium\b`).FindStringIndex(script)
	if chromiumIdx == nil {
		t.Fatal("entrypoint no longer launches chromium — cannot verify launch-order invariants")
	}
	if lockIdx[0] > chromiumIdx[0] {
		t.Fatal("SingletonLock cleanup must run BEFORE the chromium launch, not after")
	}

	for _, want := range []struct {
		re, why string
	}{
		{`websockify[^\n]*--heartbeat=`, "websockify lost --heartbeat — idle human display sessions will be dropped (~34s) by the upstream idle timeout"},
		{`--proxy-server=\$\{?DESKTOP_PROXY`, "chromium no longer routes egress through $DESKTOP_PROXY — the sidecar's only safe route off its internal network"},
		{`(?m)^\s*chromium[\s\S]*?--kiosk`, "chromium is no longer --kiosk — the agent coordinate contract (full-screen, no toolbar offset) is broken"},
	} {
		if !regexp.MustCompile(want.re).MatchString(script) {
			t.Fatalf("entrypoint invariant lost: %s (pattern %q)", want.why, want.re)
		}
	}
}

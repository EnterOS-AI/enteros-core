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
// whose removal SILENTLY reintroduces production bugs we already hit and fixed,
// or reverts the v3 full-desktop design (§25):
//
//   - Full XFCE session (startxfce4): §25.2 — the desktop must launch a real
//     desktop, not the old single kiosk browser. Losing this reverts to a
//     browser-only experience.
//   - Stale X-lock cleanup: under the persistent-container model (§25.1) /tmp
//     survives scale-to-zero, so the previous boot's Xvfb lock + socket
//     (/tmp/.X<n>-lock, /tmp/.X11-unix/X<n>) persist; the new Xvfb then REFUSES
//     TO START ("X server already running") and the WHOLE desktop is dead after
//     one recycle. Verified end-to-end 2026-07-31. Same class as #4989.
//   - SingletonLock cleanup: the persistent profile survives scale-to-zero, so a
//     recycled sidecar re-mounts a profile still holding the previous
//     container's Chromium singleton guard; Chromium then REFUSES TO START
//     ("profile in use by another Chromium on another computer") and the browser
//     is dead (reads as "no internet"). #4989.
//   - websockify --heartbeat: without periodic WS pings an IDLE noVNC connection
//     (static screen -> no framebuffer traffic) is dropped by an upstream idle
//     timeout -> "take over the display, idle, lose the connection". #4989.
//   - Egress proxy for BOTH apt and the browser (§25.3 / §6.1): the sidecar has
//     NO direct route out, so apt (self-install) and Chromium must be pointed at
//     $DESKTOP_PROXY or they have no network at all. Dropping either the apt
//     proxy or the Chromium proxy policy breaks self-install / browsing.
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

	for _, want := range []struct {
		re, why string
	}{
		{`(?m)^\s*startxfce4\b`, "entrypoint no longer launches the XFCE desktop session — reverts §25.2 to a kiosk/browser-only desktop"},
		{`\.X\$\{?_dpynum\}?-lock`, "entrypoint no longer clears the stale Xvfb lock (/tmp/.X<n>-lock) — a recycled sidecar's X server refuses to start and the whole desktop is dead after one scale-to-zero (§25.1, verified 2026-07-31)"},
		{`rm -f[\s\S]*?Singleton`, "entrypoint no longer clears the stale Chromium Singleton guards — a recycled sidecar's browser will refuse to start (the 'no internet'/black-screen bug, #4989)"},
		{`websockify[^\n]*--heartbeat=`, "websockify lost --heartbeat — idle human display sessions will be dropped (~34s) by the upstream idle timeout (#4989)"},
		{`Acquire::http(s)?::Proxy`, "entrypoint no longer points apt at $DESKTOP_PROXY — self-install (§25.3) can't reach mirrors (the sidecar has no direct egress)"},
		{`ProxyServer`, "entrypoint no longer pins Chromium's proxy to $DESKTOP_PROXY — the browser has no egress route (sidecar is on an internal-only network, §6.1)"},
	} {
		if !regexp.MustCompile(want.re).MatchString(script) {
			t.Fatalf("entrypoint invariant lost: %s (pattern %q)", want.why, want.re)
		}
	}

	// The egress-proxy wiring MUST be gated on DESKTOP_PROXY being set (a dev run
	// with no proxy still boots) — assert the proxy block references it.
	if !regexp.MustCompile(`DESKTOP_PROXY`).MatchString(script) {
		t.Fatal("entrypoint no longer references DESKTOP_PROXY — the egress-proxy wiring is gone")
	}
}

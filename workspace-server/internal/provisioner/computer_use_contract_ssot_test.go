package provisioner

import (
	"testing"

	molcontracts "go.moleculesai.app/sdk/gen/go/molcontracts"
)

// TestComputerUseContract_MatchesSSOT value-pins the desktop constants this repo
// hardcodes against the generated SSOT binding (molcontracts.ComputerUse*,
// derived from contracts/tool/computer-use.contract.json). Mirrors the
// MatchesSSOT precedent in internal/handlers/mcp_plugin_delivery_contract.go: if
// the contract and the code drift, this test fails at build/test time instead of
// silently shipping a mismatch (e.g. a renamed role label that makes the reap
// path miss every sidecar, or a geometry change that reintroduces the wrong-pixel
// class bug).
func TestComputerUseContract_MatchesSSOT(t *testing.T) {
	// --- reap-safety naming (real code constants ↔ SSOT) ---------------------
	if molcontracts.ComputerUseSidecarNamePrefix != desktopContainerNamePrefix {
		t.Errorf("sidecar name prefix: SSOT %q != code %q", molcontracts.ComputerUseSidecarNamePrefix, desktopContainerNamePrefix)
	}
	if molcontracts.ComputerUseRoleLabel != LabelRole {
		t.Errorf("role label: SSOT %q != code %q", molcontracts.ComputerUseRoleLabel, LabelRole)
	}
	if molcontracts.ComputerUseRoleValue != RoleDesktop {
		t.Errorf("role value: SSOT %q != code %q", molcontracts.ComputerUseRoleValue, RoleDesktop)
	}
	// DesktopProfileVolumeName builds "<prefix><id><suffix>"; assert the suffix
	// the SSOT declares is exactly the one the volume name uses.
	const ssotWS = "abc"
	wantVolume := desktopContainerNamePrefix + ssotWS + molcontracts.ComputerUseProfileVolumeSuffix
	if got := DesktopProfileVolumeName(ssotWS); got != wantVolume {
		t.Errorf("profile volume suffix drift: DesktopProfileVolumeName=%q, SSOT-composed=%q", got, wantVolume)
	}

	// --- coordinate contract + surface (drift gate) --------------------------
	// These live as env/params/literals across the stack (Dockerfile geometry,
	// control-server mux paths, cmd/server ports) rather than one Go constant, so
	// this gate pins the SSOT to the values the code is built around. Any change
	// here must be made in lockstep with the contract.
	if molcontracts.ComputerUseDisplayWidth != 1280 || molcontracts.ComputerUseDisplayHeight != 800 {
		t.Errorf("display geometry drift: SSOT %dx%d, want 1280x800 (the fixed coordinate contract)", molcontracts.ComputerUseDisplayWidth, molcontracts.ComputerUseDisplayHeight)
	}
	if molcontracts.ComputerUseControlDefaultPort != 6070 {
		t.Errorf("control port drift: SSOT %d, want 6070", molcontracts.ComputerUseControlDefaultPort)
	}
	if molcontracts.ComputerUseVNCDefaultPort != 6080 {
		t.Errorf("vnc port drift: SSOT %d, want 6080", molcontracts.ComputerUseVNCDefaultPort)
	}
	if molcontracts.ComputerUseScreenshotPath != "/screenshot" || molcontracts.ComputerUseInputPath != "/input" || molcontracts.ComputerUseHealthPath != "/healthz" {
		t.Errorf("control-server path drift: SSOT screenshot=%q input=%q health=%q", molcontracts.ComputerUseScreenshotPath, molcontracts.ComputerUseInputPath, molcontracts.ComputerUseHealthPath)
	}
	if molcontracts.ComputerUseInputSuccessStatus != 204 {
		t.Errorf("input success status drift: SSOT %d, want 204", molcontracts.ComputerUseInputSuccessStatus)
	}
}

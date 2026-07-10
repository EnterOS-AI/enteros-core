//go:build !unix

package provisioner

import "os/exec"

// setProcessGroup is a no-op on non-unix platforms (no pgid concept). The
// workspace-server provisioner only runs on Linux (prod) + macOS (dev); this
// stub keeps the package buildable elsewhere.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup falls back to killing just the direct child on non-unix
// platforms.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

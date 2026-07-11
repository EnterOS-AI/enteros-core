//go:build unix

package provisioner

import (
	"os/exec"
	"syscall"
)

// setProcessGroup makes the child the leader of a new process group so the
// whole subtree can be signalled at once. Called before cmd.Start().
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup sends SIGKILL to the child's whole process group (negative
// pid) so grandchildren die too. Falls back to killing just the child if the
// group signal fails (e.g. the child already exited, or Setpgid didn't take).
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	// Negative pid targets the process group whose id == pid (the child is
	// its own group leader because of Setpgid above).
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

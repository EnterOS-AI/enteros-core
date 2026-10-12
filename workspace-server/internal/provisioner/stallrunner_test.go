package provisioner

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The stall-runner tests drive real child processes (via `sh -c`) so they
// exercise the actual Start/Wait/Kill/reap path, not a mock. They use tiny
// grace/ceiling values so the whole suite runs in a couple of seconds. They
// are hermetic — no docker/git/network — because runStreamingCommand is a
// pure process runner.

func requireSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stall-runner tests use a POSIX shell; skipping on windows")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH; skipping stall-runner process tests")
	}
}

// (a) A command that streams output for LONGER than the stall grace but
// never goes quiet longer than the grace must NOT be killed — it succeeds.
// This is the "slow but healthy cold build" case the whole change exists to
// protect.
func TestRunStreamingCommand_ChattyLongRunning_NotKilled(t *testing.T) {
	requireSh(t)
	// Emit a line every 100ms for ~1.5s — total runtime (1.5s) far exceeds
	// the 300ms stall grace, but no single gap does, so it must complete.
	script := `i=0; while [ $i -lt 15 ]; do echo "layer $i"; i=$((i+1)); sleep 0.1; done; echo done`
	cmd := exec.CommandContext(context.Background(), "sh", "-c", script)

	start := time.Now()
	out, err := runStreamingCommand(context.Background(), cmd, 300*time.Millisecond, 30*time.Second)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("chatty long-running command was killed: %v (out=%q)", err, out)
	}
	if elapsed < 1*time.Second {
		t.Errorf("command returned in %s — expected it to run its full ~1.5s (did the runner cut it short?)", elapsed)
	}
	if !strings.Contains(string(out), "done") {
		t.Errorf("output missing final line; got %q", out)
	}
	if !strings.Contains(string(out), "layer 0") {
		t.Errorf("output missing streamed lines; got %q", out)
	}
}

// (b) A command that emits one line then goes silent past the stall grace
// must be killed with errBuildStalled — AFTER roughly the grace, not before.
func TestRunStreamingCommand_GoesSilent_KilledWithStallError(t *testing.T) {
	requireSh(t)
	// One line, then sleep well past the grace with no output.
	script := `echo "starting"; sleep 10`
	cmd := exec.CommandContext(context.Background(), "sh", "-c", script)

	grace := 400 * time.Millisecond
	start := time.Now()
	out, err := runStreamingCommand(context.Background(), cmd, grace, 30*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected stall kill, got nil error (out=%q)", out)
	}
	if !errors.Is(err, errBuildStalled) {
		t.Fatalf("expected errBuildStalled, got %v", err)
	}
	// Must not fire BEFORE the grace (the "starting" line resets the clock,
	// so the kill is ~grace after that line). Allow slack for poll interval.
	if elapsed < grace {
		t.Errorf("killed after %s — before the %s grace elapsed", elapsed, grace)
	}
	// Must fire reasonably promptly after the grace (grace + a couple poll
	// intervals), not at the 10s sleep boundary.
	if elapsed > grace+2*stallPollInterval+2*time.Second {
		t.Errorf("stall kill took %s — far longer than grace(%s)+slack; monitor not prompt", elapsed, grace)
	}
	// The captured pre-stall output must be preserved for the error message.
	if !strings.Contains(string(out), "starting") {
		t.Errorf("pre-stall output not captured; got %q", out)
	}
}

// (c) The absolute ceiling must kill a command that stays chatty forever
// (never stalls) — the stall gate alone would never fire.
func TestRunStreamingCommand_Ceiling_KillsEndlessChattyCommand(t *testing.T) {
	requireSh(t)
	// Emits constantly (every 50ms) so the stall gate NEVER trips; only the
	// ceiling can stop it.
	script := `while true; do echo tick; sleep 0.05; done`
	cmd := exec.CommandContext(context.Background(), "sh", "-c", script)

	ceiling := 600 * time.Millisecond
	// Stall grace generously larger than the ceiling so it can't be the one
	// that fires.
	start := time.Now()
	out, err := runStreamingCommand(context.Background(), cmd, 30*time.Second, ceiling)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected ceiling kill, got nil error")
	}
	if !errors.Is(err, errBuildCeiling) {
		t.Fatalf("expected errBuildCeiling, got %v", err)
	}
	if elapsed < ceiling {
		t.Errorf("killed after %s — before the %s ceiling", elapsed, ceiling)
	}
	if elapsed > ceiling+2*stallPollInterval+2*time.Second {
		t.Errorf("ceiling kill took %s — far longer than ceiling(%s)+slack", elapsed, ceiling)
	}
	// Sanity: it did stream before being ceiling-killed.
	if !strings.Contains(string(out), "tick") {
		t.Errorf("expected streamed output before ceiling kill; got %q", out)
	}
}

// (d) The child process must be reaped (no zombie) on a stall kill. We assert
// this by confirming ProcessState is set after runStreamingCommand returns —
// which is only true once Wait() has completed, i.e. the process was reaped.
func TestRunStreamingCommand_ProcessReapedOnStall(t *testing.T) {
	requireSh(t)
	script := `echo hi; sleep 10`
	cmd := exec.CommandContext(context.Background(), "sh", "-c", script)

	_, err := runStreamingCommand(context.Background(), cmd, 300*time.Millisecond, 30*time.Second)
	if err == nil {
		t.Fatalf("expected stall kill")
	}
	// After the runner returns, Wait() has been called, so ProcessState is
	// populated. A nil ProcessState would mean the child was never reaped.
	if cmd.ProcessState == nil {
		t.Fatalf("ProcessState is nil — child was not reaped (zombie leak)")
	}
	if cmd.ProcessState.ExitCode() >= 0 {
		// A killed process exits via signal → ExitCode() is -1. A clean exit
		// (>=0) would mean we didn't actually kill it. Either way, reaped.
		t.Logf("note: killed process exit code = %d", cmd.ProcessState.ExitCode())
	}
}

// A command that exits cleanly on its own returns its full output and no
// error — the happy path stays identical to the old CombinedOutput path.
func TestRunStreamingCommand_CleanExit(t *testing.T) {
	requireSh(t)
	cmd := exec.CommandContext(context.Background(), "sh", "-c", `echo one; echo two`)
	out, err := runStreamingCommand(context.Background(), cmd, time.Second, 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), "one") || !strings.Contains(string(out), "two") {
		t.Errorf("output = %q, want both lines", out)
	}
}

// A command that exits NON-zero returns the exit error AND its output (so the
// caller can surface the build failure reason) — not a stall/ceiling error.
func TestRunStreamingCommand_NonZeroExit(t *testing.T) {
	requireSh(t)
	cmd := exec.CommandContext(context.Background(), "sh", "-c", `echo boom >&2; exit 7`)
	out, err := runStreamingCommand(context.Background(), cmd, time.Second, 30*time.Second)
	if err == nil {
		t.Fatalf("expected non-zero exit error")
	}
	if errors.Is(err, errBuildStalled) || errors.Is(err, errBuildCeiling) {
		t.Fatalf("non-zero exit misclassified as stall/ceiling: %v", err)
	}
	if !strings.Contains(string(out), "boom") {
		t.Errorf("stderr output not captured; got %q", out)
	}
}

// Parent-ctx cancellation kills the process and returns the ctx error, and
// the process is reaped.
func TestRunStreamingCommand_ParentCtxCancel(t *testing.T) {
	requireSh(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", `echo go; sleep 10`)

	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := runStreamingCommand(ctx, cmd, 30*time.Second, 30*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected ctx-cancel error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("ctx cancel took %s — the sleep(10) should have been killed promptly", elapsed)
	}
	if cmd.ProcessState == nil {
		t.Fatalf("ProcessState nil after ctx cancel — child not reaped")
	}
}

// stallGrace<=0 disables the stall gate; a long-silent-then-exit command runs
// to completion (used to prove the gate is opt-outable).
func TestRunStreamingCommand_ZeroGraceDisablesStallGate(t *testing.T) {
	requireSh(t)
	cmd := exec.CommandContext(context.Background(), "sh", "-c", `echo hi; sleep 0.5; echo bye`)
	out, err := runStreamingCommand(context.Background(), cmd, 0, 30*time.Second)
	if err != nil {
		t.Fatalf("zero grace should disable stall gate, but command errored: %v", err)
	}
	if !strings.Contains(string(out), "bye") {
		t.Errorf("command cut short; got %q", out)
	}
}

// A silence that the caller's exemption recognizes (e.g. BuildKit's final
// "unpacking to image" phase, which legitimately emits nothing for minutes on
// a large image) must NOT trip the stall gate — the command runs to
// completion. The absolute ceiling still applies (last sub-test). This is the
// 2026-07-18 first-boot regression: the ~7GB hermes image's silent unpack
// exceeded the 4m grace and EVERY fresh self-host onboarding died at
// "no output within stall grace".
func TestRunStreamingCommandExempt_QuietPhaseSurvivesStallGate(t *testing.T) {
	requireSh(t)
	// Emits the unpack marker, goes silent well past the grace, then finishes.
	script := `echo "#26 unpacking to docker.io/molecule-local/workspace-template-hermes:x-amd64"; sleep 1; echo "#26 DONE 0.6s"`
	cmd := exec.CommandContext(context.Background(), "sh", "-c", script)

	out, err := runStreamingCommandExempt(
		context.Background(), cmd, 300*time.Millisecond, 30*time.Second, buildkitQuietPhaseExempt)
	if err != nil {
		t.Fatalf("silent exempt phase was killed: %v (out=%q)", err, out)
	}
	if !strings.Contains(string(out), "#26 DONE") {
		t.Errorf("command cut short; got %q", out)
	}

	// Same silence WITHOUT an exempt marker in the tail must still be killed.
	cmd2 := exec.CommandContext(context.Background(), "sh", "-c", `echo "#24 RUN pip install"; sleep 10`)
	_, err = runStreamingCommandExempt(
		context.Background(), cmd2, 300*time.Millisecond, 30*time.Second, buildkitQuietPhaseExempt)
	if !errors.Is(err, errBuildStalled) {
		t.Fatalf("non-exempt silence must still stall-kill, got %v", err)
	}

	// An exempt phase that NEVER ends is still stall-killed — at the
	// extended (stallExemptFactor×) grace, not deferred all the way to the
	// absolute ceiling — so a daemon wedged mid-unpack keeps the diagnostic
	// stall error and dies promptly.
	cmd3 := exec.CommandContext(context.Background(), "sh", "-c", `echo "#26 unpacking to docker.io/x"; sleep 30`)
	grace := 300 * time.Millisecond
	start := time.Now()
	_, err = runStreamingCommandExempt(
		context.Background(), cmd3, grace, 30*time.Second, buildkitQuietPhaseExempt)
	elapsed := time.Since(start)
	if !errors.Is(err, errBuildStalled) {
		t.Fatalf("endless exempt phase must be stall-killed at the extended grace, got %v", err)
	}
	if elapsed < grace*stallExemptFactor {
		t.Errorf("exempt kill after %s — before the extended grace %s", elapsed, grace*stallExemptFactor)
	}
	if elapsed > grace*stallExemptFactor+2*stallPollInterval+2*time.Second {
		t.Errorf("exempt kill took %s — far past extended grace %s + slack", elapsed, grace*stallExemptFactor)
	}
}

package provisioner

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These tests cover the defects the #3822 stall-runner review surfaced. Each
// guards a regression that, if reintroduced, HANGS or MISLABELS the provision
// path rather than merely returning a wrong value — so they use a hard timeout
// wrapper and fail (not hang) on regression.

// Finding #1 — deadlock on an over-cap single line.
//
// The old reader was a bufio.Scanner with a 1 MiB cap; a single output line
// longer than that returned ErrTooLong and the reader STOPPED draining `pr`.
// The os/exec-internal stdout copier (present because cmd.Stdout is an
// *io.PipeWriter) then blocked forever on its `pw.Write`, so cmd.Wait() never
// returned and no stall/ceiling/ctx kill could rescue it. The chunked reader
// drains regardless of line length. If this regresses, the goroutine below
// never sends and the test fails via the timeout guard.
func TestRunStreamingCommand_HugeSingleLine_NoDeadlock(t *testing.T) {
	requireSh(t)
	// ~2 MiB on ONE line (no newline until the end) — comfortably over the old
	// 1 MiB scanner cap — then a final line and a clean exit.
	script := `head -c 2097152 /dev/zero | tr '\0' 'x'; echo; echo END`
	cmd := exec.CommandContext(context.Background(), "sh", "-c", script)

	type res struct {
		out []byte
		err error
	}
	done := make(chan res, 1)
	go func() {
		out, err := runStreamingCommand(context.Background(), cmd, 10*time.Second, 60*time.Second)
		done <- res{out, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("huge-single-line command should exit 0, got err=%v", r.err)
		}
		if !strings.Contains(string(r.out), "END") {
			t.Errorf("captured output missing the final line after the big blob (%d bytes captured)", len(r.out))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runStreamingCommand DEADLOCKED on a >1 MiB single line — finding #1 has regressed")
	}
}

// Finding #1 (companion) — a grandchild that outlives the direct child and
// holds the pipe's write end must not wedge Wait(). The runner puts the child
// in its own process group and kills the GROUP; without that, killing only the
// direct child leaves the grandchild holding the OS-pipe write end, the copier
// never sees EOF, and Wait() blocks until the grandchild exits on its own. Here
// the direct child spawns a silent 60s grandchild then goes quiet, so the stall
// gate must fire AND the group kill must reap the grandchild promptly.
func TestRunStreamingCommand_GrandchildHoldingPipe_StillReaped(t *testing.T) {
	requireSh(t)
	script := `sleep 60 & echo started; sleep 60`
	cmd := exec.CommandContext(context.Background(), "sh", "-c", script)

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := runStreamingCommand(context.Background(), cmd, 400*time.Millisecond, 60*time.Second)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, errBuildStalled) {
			t.Fatalf("expected errBuildStalled, got %v", err)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("runner took %s — Wait() likely blocked on the grandchild holding the pipe (group kill failed)", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runner never returned — grandchild held the pipe and group-kill/reaping failed")
	}
}

// Finding #4 — a clean (exit-0) build whose final step is quiet for LESS than
// the grace, then exits, must not be mislabeled as stalled. Exercises the
// non-blocking waitCh peek that prefers a real exit over a stall verdict.
func TestRunStreamingCommand_QuietFinalStepThenCleanExit_NotStalled(t *testing.T) {
	requireSh(t)
	// Emit a line, stay quiet ~600ms (< the 2s grace), then exit 0.
	cmd := exec.CommandContext(context.Background(), "sh", "-c", `echo building; sleep 0.6; exit 0`)
	out, err := runStreamingCommand(context.Background(), cmd, 2*time.Second, 30*time.Second)
	if err != nil {
		t.Fatalf("clean exit after a sub-grace quiet gap must succeed, got %v (out=%q)", err, out)
	}
	if !strings.Contains(string(out), "building") {
		t.Errorf("output missing streamed line; got %q", out)
	}
}

// Finding #6 — captured output is tail-capped so a pathologically chatty build
// can't grow the buffer without bound. Emit well past the cap and assert the
// retained output is bounded near maxCapturedOutput and keeps the TAIL (where a
// real build's error lives), not the head.
func TestRunStreamingCommand_OutputTailCapped(t *testing.T) {
	requireSh(t)
	// Emit ~6 MiB (> maxCapturedOutput+slack = 5 MiB) as one blob, then a
	// unique final marker.
	script := `head -c 6291456 /dev/zero | tr '\0' 'x'; echo; echo TAILMARKER`
	cmd := exec.CommandContext(context.Background(), "sh", "-c", script)

	out, err := runStreamingCommand(context.Background(), cmd, 10*time.Second, 60*time.Second)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out) > maxCapturedOutput+capturedOutputSlack {
		t.Errorf("captured output %d bytes exceeds cap %d — tail-cap not applied", len(out), maxCapturedOutput+capturedOutputSlack)
	}
	if !strings.Contains(string(out), "TAILMARKER") {
		t.Errorf("tail-cap dropped the TAIL instead of the head — final marker missing")
	}
}

// Finding #2 — ceilingFromCtxDeadline threads the provision ctx's per-runtime
// deadline into the stall-runner ceiling (so hermes's 30m reaches the build),
// and returns 0 for a deadline-less ctx (bundle-importer path keeps the
// buildCeiling() default).
func TestCeilingFromCtxDeadline(t *testing.T) {
	t.Run("deadline present → remaining time", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
		defer cancel()
		got := ceilingFromCtxDeadline(ctx)
		// Allow a little slack for the time between WithTimeout and the read.
		if got < 24*time.Minute || got > 25*time.Minute {
			t.Fatalf("expected ~25m remaining, got %s", got)
		}
	})

	t.Run("no deadline → 0 (keep option default)", func(t *testing.T) {
		if got := ceilingFromCtxDeadline(context.Background()); got != 0 {
			t.Fatalf("expected 0 for a deadline-less ctx, got %s", got)
		}
	})

	t.Run("already expired → 0", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
		defer cancel()
		if got := ceilingFromCtxDeadline(ctx); got != 0 {
			t.Fatalf("expected 0 for an already-expired ctx, got %s", got)
		}
	})
}

// Finding #2 (wiring) — a zero-value option resolves the ceiling to the package
// default, but once EnsureLocalImage sets opts.Ceiling from the ctx deadline,
// opts.ceiling() reflects that per-runtime value (not the 12m default). Proves
// the derived ceiling actually reaches the runner via the option accessor the
// build/clone sites call.
func TestLocalBuildOptions_CeilingReflectsPerRuntimeValue(t *testing.T) {
	def := (&LocalBuildOptions{}).ceiling()
	if def != buildCeiling() {
		t.Fatalf("zero-value ceiling() = %s, want package default %s", def, buildCeiling())
	}
	opts := newDefaultLocalBuildOptions()
	if c := ceilingFromCtxDeadline(mustDeadlineCtx(t, 30*time.Minute)); c > 0 {
		opts.Ceiling = c
	}
	if got := opts.ceiling(); got < 29*time.Minute || got > 30*time.Minute {
		t.Fatalf("per-runtime ceiling did not reach opts.ceiling(): got %s, want ~30m", got)
	}
}

func mustDeadlineCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

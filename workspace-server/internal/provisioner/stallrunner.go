package provisioner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// stallrunner.go — a progress-driven command runner for the local-build
// path (`docker build` + the shallow `git clone`).
//
// Why this exists
// ---------------
// The whole local-build path used to run inside a fixed
// `context.WithTimeout(ctx, provisioner.ProvisionTimeout)` = 3 minutes
// (provisioner.ProvisionTimeout, applied at every prov.Start call site).
// That ctx flows all the way into `exec.CommandContext(ctx,"docker","build",…)`,
// so a legitimately-slow cold build (first-ever image on a fresh host, or
// a QEMU-emulated cross-arch build) that runs past 3 min gets its process
// killed with a bare `context canceled` — the exact failure that bricked a
// hermes concierge provision even though the hermes template declares a
// 30-minute provision_timeout_seconds.
//
// A fixed wall-clock is the wrong gate: a build that is streaming layer
// output every few seconds is HEALTHY no matter how long it has been
// running; a build that has emitted nothing for minutes is the real stall.
// This runner mirrors the in-repo stall-watchdog precedent
// (handlers/stall_watchdog.go): watch for PROGRESS, not the clock. It
// cancels the process only when either
//
//	(a) no output line for `stallGrace` (the primary gate), OR
//	(b) an ABSOLUTE ceiling elapses (backstop against a chatty-but-endless
//	    build), OR
//	(c) the parent ctx is cancelled (caller gave up / process shutdown).
//
// On a stall/ceiling kill it returns a CLEAR typed error naming the
// mechanism — not an opaque context-cancel — so last_sample_error surfaces
// something actionable.

const (
	// defaultBuildStallGrace is how long the runner tolerates ZERO output
	// from `docker build` (or the clone) before treating the process as
	// wedged. Sized to comfortably clear the slowest single quiet step of a
	// real build — a large `pip install` / `apt-get` layer or a slow base
	// image pull can legitimately run for a few minutes emitting nothing
	// under BuildKit's collapsed progress — while still catching a genuine
	// hang (network black hole, deadlocked child) well before the absolute
	// ceiling. Overridable via MOLECULE_BUILD_STALL_GRACE_S.
	defaultBuildStallGrace = 4 * time.Minute

	// defaultBuildCeiling is the absolute wall-clock backstop for the whole
	// build when no per-runtime provision timeout is threaded in (the
	// bundle-importer path, or a runtime that declares nothing). It is the
	// hard "no build may run longer than this regardless of chatter" cap.
	// 12 min matches the DefaultProvisioningTimeout sweep window
	// (registry.DefaultProvisioningTimeout) so a build can never outlive the
	// row-level sweep that would flip it to failed anyway. Deliberately NOT
	// the old 3-min ProvisionTimeout — that value hard-capped real builds.
	// Overridable via MOLECULE_BUILD_CEILING_S.
	defaultBuildCeiling = 12 * time.Minute

	// stallPollInterval is how often the monitor re-checks the
	// last-progress timestamp. Small enough that the kill fires promptly
	// after the grace elapses; large enough to be free.
	stallPollInterval = 2 * time.Second
)

// errBuildStalled / errBuildCeiling are the sentinel errors the runner
// returns so callers (and tests) can distinguish a no-progress kill from a
// ceiling kill from an ordinary non-zero exit. Wrapped with %w by
// runStreamingCommand.
var (
	errBuildStalled = errors.New("no output within stall grace")
	errBuildCeiling = errors.New("exceeded absolute ceiling")
)

// envDurationSeconds reads an integer-seconds env override, falling back to
// def on missing/invalid input (never fails the build on a typo — mirrors
// handlers.envDuration). Exposed at package scope so both grace + ceiling
// defaults resolve identically.
func envDurationSeconds(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return time.Duration(n) * time.Second
}

// buildStallGrace / buildCeiling resolve the effective values, honoring the
// env overrides. Called once per build from the option defaults.
func buildStallGrace() time.Duration {
	return envDurationSeconds("MOLECULE_BUILD_STALL_GRACE_S", defaultBuildStallGrace)
}

func buildCeiling() time.Duration {
	return envDurationSeconds("MOLECULE_BUILD_CEILING_S", defaultBuildCeiling)
}

// DefaultProvisionCeiling is the absolute provision-context deadline for
// callers that bound prov.Start → the local build but have no per-runtime
// provision_timeout_seconds to consult (e.g. the bundle importer, which runs
// below the handlers package). It equals the build ceiling (env-overridable
// via MOLECULE_BUILD_CEILING_S, default 12m) so such a caller's ctx never
// caps a real build below the stall-runner's own backstop — replacing the
// old fixed 3-min ProvisionTimeout on those paths.
func DefaultProvisionCeiling() time.Duration {
	return buildCeiling()
}

// runStreamingCommand runs cmd, streaming its merged stdout+stderr
// line-by-line, and returns the FULL captured output plus an error.
//
// Gates (whichever fires first):
//   - stallGrace: no output line for this long → kill, return errBuildStalled.
//   - ceiling:    total elapsed exceeds this   → kill, return errBuildCeiling.
//   - ctx:        parent ctx cancelled          → kill, return ctx.Err().
//   - normal:     process exits on its own      → return its exit error (or nil).
//
// stallGrace<=0 disables the stall gate; ceiling<=0 disables the ceiling
// gate (ctx and normal exit still apply). The returned output is always the
// bytes captured so far, so callers can mask + surface it in the error
// message exactly as the old cmd.CombinedOutput() path did.
//
// Reaping: the command is Start()ed (not Run()) and always Wait()ed for in
// this function — even on a kill — so no zombie/defunct child is left. The
// single reader goroutine drains the pipe to EOF (which happens once the
// process dies and its stdout/stderr fds close), and we join it before
// returning, so there is no goroutine leak and the returned buffer is
// complete + race-free.
func runStreamingCommand(ctx context.Context, cmd *exec.Cmd, stallGrace, ceiling time.Duration) ([]byte, error) {
	// Merge stdout+stderr into one pipe so ordering is preserved and a
	// single reader drains both — matching CombinedOutput's semantics.
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	// Put the child in its OWN process group so a kill reaches the whole
	// tree (e.g. `docker build` shelling out, or `sh -c '… sleep …'`). Killing
	// only the direct child would leave grandchildren holding the pipe's
	// write end open — Wait() would then block on the internal stdout copier
	// until the grandchild exits on its own, defeating the stall/ceiling kill.
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return nil, err
	}

	// progress tracks the last time we saw an output line, and the buffer
	// accumulates the full output. Both are touched by the reader goroutine
	// and read by the monitor / main goroutine, so guard with a mutex.
	var (
		mu       sync.Mutex
		buf      bytes.Buffer
		lastSeen = time.Now()
	)
	sinceProgress := func() time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return time.Since(lastSeen)
	}

	// Reader goroutine: drain the merged pipe to EOF, appending to buf and
	// resetting the progress clock on every line. EOF arrives once the
	// process exits and the write end closes (we close pw right after Wait).
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		sc := bufio.NewScanner(pr)
		// Allow long single lines (BuildKit can emit wide progress lines);
		// 1 MiB is far above any realistic build line.
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			mu.Lock()
			buf.Write(line)
			buf.WriteByte('\n')
			lastSeen = time.Now()
			mu.Unlock()
		}
		// A scanner error (e.g. line-too-long) is not fatal to the build
		// verdict — the process's own exit status is the source of truth.
		// We simply stop draining; the io.Pipe read end is closed below.
	}()

	// killReason is set by the monitor before it kills, so the main
	// goroutine can convert the resulting Wait error into the RIGHT typed
	// error rather than a bare "signal: killed".
	var (
		killMu     sync.Mutex
		killReason error
	)
	setKillReason := func(err error) {
		killMu.Lock()
		if killReason == nil {
			killReason = err
		}
		killMu.Unlock()
	}
	getKillReason := func() error {
		killMu.Lock()
		defer killMu.Unlock()
		return killReason
	}

	// waitErr carries the process's exit status back from a dedicated
	// goroutine so the monitor's select can race Wait against the timers.
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	ticker := time.NewTicker(stallPollInterval)
	defer ticker.Stop()

	// deadline is the absolute-ceiling wall clock (zero time = disabled).
	var deadline time.Time
	if ceiling > 0 {
		deadline = time.Now().Add(ceiling)
	}

	kill := func(reason error) {
		setKillReason(reason)
		// Kill the whole process GROUP (see setProcessGroup) so grandchildren
		// die too and release the pipe's write end — otherwise Wait()'s
		// internal stdout copier blocks until they exit on their own. Falls
		// back to a single-process kill on platforms without pgid support.
		killProcessGroup(cmd)
	}

	// ctxDone is nil-ed after the first cancel so the (now-permanently-ready)
	// ctx.Done() channel can't be re-selected on every loop iteration while
	// we wait for waitCh to fire — a nil channel blocks forever in select.
	ctxDone := ctx.Done()

	// Monitor loop: races normal exit against the stall/ceiling/ctx gates.
	for {
		select {
		case werr := <-waitCh:
			// Process exited (naturally or because we killed it). Close the
			// write end so the reader sees EOF, then join it so buf is
			// complete before we read it.
			_ = pw.Close()
			<-readerDone
			_ = pr.Close()

			mu.Lock()
			out := append([]byte(nil), buf.Bytes()...)
			mu.Unlock()

			if reason := getKillReason(); reason != nil {
				// We killed it deliberately — surface the mechanism, not
				// the resulting "signal: killed".
				return out, reason
			}
			return out, werr

		case <-ctxDone:
			kill(ctx.Err())
			ctxDone = nil // don't re-fire; wait for waitCh to reap
			// The waitCh case fires once the kill lands and reaps the process.

		case <-ticker.C:
			if ceiling > 0 && time.Now().After(deadline) {
				kill(fmt.Errorf("%w %s", errBuildCeiling, ceiling))
				continue
			}
			if stallGrace > 0 && sinceProgress() > stallGrace {
				kill(fmt.Errorf("%w %s", errBuildStalled, stallGrace))
				continue
			}
		}
	}
}

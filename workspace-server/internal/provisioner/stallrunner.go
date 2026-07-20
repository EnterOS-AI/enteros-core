package provisioner

import (
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

const (
	// maxCapturedOutput bounds the retained build/clone output so a
	// pathologically chatty build can't grow the capture buffer without
	// limit. We keep the TAIL — a failing `docker build` / `git clone` prints
	// the actionable error at the END — dropping the oldest bytes once over
	// the cap. (The old cmd.CombinedOutput path was itself unbounded, so this
	// is a robustness improvement, not a regression.)
	maxCapturedOutput = 4 * 1024 * 1024
	// capturedOutputSlack lets the buffer run this far past the cap before a
	// trim, so the O(n) tail-copy amortizes over ~1 MiB of new output rather
	// than firing on every 32 KiB chunk once over the cap.
	capturedOutputSlack = 1 * 1024 * 1024
)

// appendCapped appends p to buf, then trims buf from the FRONT if it has grown
// past maxCapturedOutput+capturedOutputSlack, leaving the last maxCapturedOutput
// bytes. The caller holds the buffer's mutex.
func appendCapped(buf *bytes.Buffer, p []byte) {
	buf.Write(p)
	if buf.Len() <= maxCapturedOutput+capturedOutputSlack {
		return
	}
	b := buf.Bytes()
	tail := append([]byte(nil), b[len(b)-maxCapturedOutput:]...)
	buf.Reset()
	buf.Write(tail)
}

// stallExemptFunc lets a caller declare that a particular silence is
// LEGITIMATE: it receives the tail of the output captured so far and returns
// true when the process is known to be in a quiet-but-healthy phase (e.g.
// BuildKit's final image unpack, which emits nothing for minutes on a
// multi-GB image). An exempt silence gets an EXTENDED grace
// (stallExemptFactor × stallGrace) rather than an unlimited one — a daemon
// that wedges mid-unpack still dies with the diagnostic stall error instead
// of silently running to the (much larger, per-runtime) absolute ceiling.
type stallExemptFunc func(tail []byte) bool

// stallExemptFactor multiplies the stall grace for a recognized quiet phase.
// 4× the 4m default = 16 minutes of tolerated silent unpack — comfortably
// clears a multi-GB image on slow disk while still bounding a genuine
// mid-phase hang well below a 30-minute runtime ceiling.
const stallExemptFactor = 4

// runStreamingCommand runs cmd, streaming its merged stdout+stderr in chunks,
// and returns the (tail-capped) captured output plus an error.
//
// Gates (whichever fires first):
//   - stallGrace: no output for this long      → kill, return errBuildStalled.
//   - ceiling:    total elapsed exceeds this   → kill, return errBuildCeiling.
//   - ctx:        parent ctx cancelled          → kill, return ctx.Err().
//   - normal:     process exits on its own      → return its exit error (or nil).
//
// stallGrace<=0 disables the stall gate; ceiling<=0 disables the ceiling
// gate (ctx and normal exit still apply). The returned output is the bytes
// captured so far (last maxCapturedOutput bytes), so callers can mask +
// surface it in the error message like the old cmd.CombinedOutput() path did.
//
// Reaping: the command is Start()ed (not Run()) and always Wait()ed for in
// this function — even on a kill — so no zombie/defunct child is left. The
// single reader goroutine drains the pipe to EOF (which happens once the
// process dies and its stdout/stderr fds close), and we join it before
// returning, so there is no goroutine leak and the returned buffer is
// complete + race-free. The reader reads raw CHUNKS (not lines) so an
// over-long single line can never stop it draining and wedge Wait() — see the
// reader goroutine below.
func runStreamingCommand(ctx context.Context, cmd *exec.Cmd, stallGrace, ceiling time.Duration) ([]byte, error) {
	return runStreamingCommandExempt(ctx, cmd, stallGrace, ceiling, nil)
}

// runStreamingCommandExempt is runStreamingCommand with an optional
// stall-exemption hook (nil = never exempt, identical behavior). See
// stallExemptFunc for the contract.
func runStreamingCommandExempt(ctx context.Context, cmd *exec.Cmd, stallGrace, ceiling time.Duration, stallExempt stallExemptFunc) ([]byte, error) {
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
	// outputTail snapshots the last few KB of captured output for the
	// stall-exemption check — enough to hold the final progress lines
	// without copying the whole buffer every poll tick.
	outputTail := func() []byte {
		mu.Lock()
		defer mu.Unlock()
		b := buf.Bytes()
		if len(b) > 4096 {
			b = b[len(b)-4096:]
		}
		return append([]byte(nil), b...)
	}

	// Reader goroutine: drain the merged pipe to EOF in fixed-size CHUNKS,
	// appending to buf and resetting the progress clock on every read. EOF
	// arrives once the process exits and the write end closes (we close pw
	// right after Wait).
	//
	// Chunked (not line-scanned) on purpose. A bufio.Scanner stops — with
	// ErrTooLong — on a single line longer than its buffer cap, and once it
	// stops draining `pr` the os/exec-internal stdout copier (present because
	// cmd.Stdout/Stderr is an *io.PipeWriter, not an *os.File) blocks FOREVER
	// on its `pw.Write` (an io.Pipe write blocks until a reader consumes it).
	// cmd.Wait() waits on that copier, so Wait never returns — and NO kill can
	// rescue it: SIGKILL closes the child's fd, but the copier is stuck on the
	// WRITE side holding bytes it already read, so it never observes the close.
	// A single build/clone line over the old 1 MiB cap (a base64 blob in a RUN
	// echo, a minified asset, `--progress=plain`'s wide lines, git's
	// \r-delimited meter) would therefore wedge the whole provision goroutine
	// permanently — the exact hang this runner exists to prevent. Reading raw
	// chunks never stops until EOF, so the pipe is always drained and Wait()
	// can always complete. Any byte activity counts as progress, which is the
	// correct stall signal independent of line structure.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		chunk := make([]byte, 32*1024)
		for {
			n, rerr := pr.Read(chunk)
			if n > 0 {
				mu.Lock()
				appendCapped(&buf, chunk[:n])
				lastSeen = time.Now()
				mu.Unlock()
			}
			if rerr != nil {
				return // io.EOF (write end closed + drained) or pipe closed
			}
		}
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

	// finish closes the write end (so the reader sees EOF), joins the reader so
	// buf is complete, and returns the captured output with either the
	// deliberate kill reason (if the monitor killed the process) or the
	// process's own exit error.
	finish := func(werr error) ([]byte, error) {
		_ = pw.Close()
		<-readerDone
		_ = pr.Close()

		mu.Lock()
		out := append([]byte(nil), buf.Bytes()...)
		mu.Unlock()

		if reason := getKillReason(); reason != nil {
			// We killed it deliberately — surface the mechanism, not the
			// resulting "signal: killed".
			return out, reason
		}
		return out, werr
	}

	// Monitor loop: races normal exit against the stall/ceiling/ctx gates.
	for {
		select {
		case werr := <-waitCh:
			return finish(werr)

		case <-ctxDone:
			kill(ctx.Err())
			ctxDone = nil // don't re-fire; wait for waitCh to reap
			// The waitCh case fires once the kill lands and reaps the process.

		case <-ticker.C:
			// Prefer a real exit over a stall/ceiling verdict: if the process
			// has ALREADY exited, take that path instead of SIGKILLing a
			// corpse and mislabeling a clean (exit-0) build whose final step
			// happened to be quiet for longer than the grace. Non-blocking, so
			// a still-running process falls through to the gate checks below.
			select {
			case werr := <-waitCh:
				return finish(werr)
			default:
			}
			if ceiling > 0 && time.Now().After(deadline) {
				kill(fmt.Errorf("%w %s", errBuildCeiling, ceiling))
				continue
			}
			if silence := sinceProgress(); stallGrace > 0 && silence > stallGrace {
				// A recognized quiet-but-healthy phase (see stallExemptFunc)
				// earns an extended — but still bounded — grace.
				if stallExempt != nil && silence <= stallGrace*stallExemptFactor && stallExempt(outputTail()) {
					continue
				}
				kill(fmt.Errorf("%w %s", errBuildStalled, stallGrace))
				continue
			}
		}
	}
}

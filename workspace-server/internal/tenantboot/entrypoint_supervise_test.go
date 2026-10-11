package tenantboot

// Regression tests for the tenant entrypoint's "first process to die takes the
// container down" supervisor.
//
// The entrypoint promises, in its own header comment: "If either process dies,
// we kill the other and exit non-zero so the container supervisor restarts the
// service." It implemented that with `wait -n <pids>`.
//
// `-n` is a BASHISM. The tenant runtime stage is node:*-alpine, so /bin/sh is
// BusyBox ash, and BusyBox's `wait` SILENTLY IGNORES `-n`: it blocks until EVERY
// listed pid has exited, with no error, no stderr and a zero status. It fails
// OPEN, which is exactly why it survived review and CI for so long — nothing
// anywhere was shaped like a failure. Measured inside the shipped tenant image
// (BusyBox v1.37.0) with children exiting at 1s and 12s, `wait -n $A $B`
// returned rc=0 after 12s: the LAST child, not the first.
//
// What that cost: when /platform dies during boot (`Redis init failed: ping
// redis: context deadline exceeded` -> log.Fatalf), Canvas and the memory
// sidecar are still alive, so `wait` never returned, cleanup never ran, and the
// container did not exit. :8080 was never bound and NOTHING restarted the
// tenant until the kubelet startupProbe budget (failureThreshold 60 x
// periodSeconds 5 = exactly 300s) expired and SIGTERMed it. The replacement
// container went Ready in six seconds.
//
// Staging 2026-08-11, org cp455-...-0a4f85db: tenant Ready 349s after the admin
// create instead of the usual 75-115s, overrunning the 300s provision budget in
// boot-to-registration-e2e and leaking the org.
//
// Like entrypoint_memory_test.go, these tests extract the marked region of the
// REAL entrypoint-tenant.sh and run it under /bin/sh, so they cannot drift from
// a hand-kept copy.

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	superviseStart = "# >>> first-death-supervise"
	superviseEnd   = "# <<< first-death-supervise"
)

// superviseBlock returns the marked supervisor region of the real entrypoint.
func superviseBlock(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(entrypointPath)
	if err != nil {
		t.Fatalf("read %s: %v", entrypointPath, err)
	}
	s := string(raw)
	i := strings.Index(s, superviseStart)
	j := strings.Index(s, superviseEnd)
	if i < 0 || j < 0 || j <= i {
		t.Fatalf("markers %q/%q not found in %s — did someone remove them? The test drives the REAL script through these.",
			superviseStart, superviseEnd, entrypointPath)
	}
	return s[i:j]
}

// TestSupervise_NeverUsesWaitDashN is the cheap, shell-independent guard.
//
// It is deliberately a STATIC assertion rather than a behavioural one, because
// the behaviour that matters only diverges on BusyBox — the shell CI does not
// run. A test that merely ran the block under the runner's /bin/sh would go
// green on a `wait -n` regression while production silently hung for 300s.
func TestSupervise_NeverUsesWaitDashN(t *testing.T) {
	raw, err := os.ReadFile(entrypointPath)
	if err != nil {
		t.Fatalf("read %s: %v", entrypointPath, err)
	}
	for i, line := range strings.Split(string(raw), "\n") {
		code := line
		if idx := strings.Index(code, "#"); idx >= 0 {
			code = code[:idx] // ignore the explanatory comments, which name it on purpose
		}
		if strings.Contains(code, "wait -n") {
			t.Fatalf("%s:%d uses `wait -n`, a bashism. This image's /bin/sh is BusyBox ash, which SILENTLY IGNORES -n and blocks until EVERY child exits — so a dead /platform would not bring the container down and the tenant would hang unhealthy for the full 300s startupProbe budget. Poll for the first death instead.\n  %s",
				entrypointPath, i+1, strings.TrimSpace(line))
		}
	}
}

// runSupervise drives the extracted block with stub children and returns the
// EXIT_CODE it computed plus how long it took to notice the first death.
//
// dieAfter/dieCode describe the child that dies FIRST; the other two outlive the
// test. If the block waits for the last child instead of the first (the `wait -n`
// bug), it will still be blocked when the deadline below fires.
func runSupervise(t *testing.T, dieAfter time.Duration, dieCode int) (int, time.Duration) {
	t.Helper()

	// The block reads these three pids and writes EXIT_CODE. Long-lived children
	// must outlive the whole test so "returned early" can only mean "noticed the
	// first death", never "everything happened to finish".
	// The subshells are explicit: `sleep 2; exit 7 &` parses as a foreground
	// `sleep` followed by a background `exit`, which would make $! the wrong pid.
	driver := fmt.Sprintf(`
( sleep %.0f; exit %d ) & PLATFORM_PID=$!
( sleep 600 ) & CANVAS_PID=$!
( sleep 600 ) & MEMORY_PLUGIN_PID=$!
%s
echo "EXIT_CODE=$EXIT_CODE"
kill $CANVAS_PID $MEMORY_PLUGIN_PID 2>/dev/null || true
`, dieAfter.Seconds(), dieCode, superviseBlock(t))

	cmd := exec.Command("/bin/sh", "-c", driver)
	done := make(chan struct{})
	var out []byte
	var err error
	start := time.Now()
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()

	// Generous relative to the ~1s poll tick, but far below the 600s the
	// surviving children live for: only first-death detection can beat it.
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("supervisor never returned after the first child died — this is the `wait -n` bug: it is waiting for the LAST child instead of the first. Output so far:\n%s", out)
	}
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("driver failed: %v\n%s", err, out)
	}

	const marker = "EXIT_CODE="
	idx := strings.LastIndex(string(out), marker)
	if idx < 0 {
		t.Fatalf("block never set EXIT_CODE; output:\n%s", out)
	}
	field := strings.TrimSpace(strings.SplitN(string(out)[idx+len(marker):], "\n", 2)[0])
	code, convErr := strconv.Atoi(field)
	if convErr != nil {
		t.Fatalf("EXIT_CODE %q is not a number; output:\n%s", field, out)
	}
	return code, elapsed
}

// TestSupervise_ReturnsOnFirstDeathNotLastDeath is the behavioural half: it
// proves the block returns while two other children are still running.
func TestSupervise_ReturnsOnFirstDeathNotLastDeath(t *testing.T) {
	code, elapsed := runSupervise(t, 2*time.Second, 7)
	if code != 7 {
		t.Fatalf("EXIT_CODE=%d, want 7 — the dead child's status must propagate so the container supervisor sees a non-zero exit and restarts the tenant. A 0 here is the false-green that lets a dead /platform look like a clean shutdown.", code)
	}
	// Two children are still sleeping for 600s. Anything near that means the
	// block waited on them too.
	if elapsed > 30*time.Second {
		t.Fatalf("took %s to notice the first death while two children were still alive — the supervisor is not returning on FIRST death", elapsed)
	}
}

// A zero-exit death must ALSO bring the container down. `wait -n` returning 0 was
// indistinguishable from "nothing died"; the poll must not reintroduce that by
// treating rc=0 as "keep waiting".
func TestSupervise_ZeroExitStillEndsTheWait(t *testing.T) {
	code, elapsed := runSupervise(t, 2*time.Second, 0)
	if code != 0 {
		t.Fatalf("EXIT_CODE=%d, want 0", code)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("a clean child exit did not end the wait (%s) — the supervisor is keyed on failure rather than on death", elapsed)
	}
}

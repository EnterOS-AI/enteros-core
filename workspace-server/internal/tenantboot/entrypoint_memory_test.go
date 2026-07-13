package tenantboot

// Regression test for the tenant entrypoint's memory-sidecar boot decision
// (task #4114).
//
// Why this exists: MEMORY_PLUGIN_URL is a CONSTANT (loopback :9100) that used to
// have to be re-declared by every provisioner. The cloud user-data paths set it;
// the local-docker provisioner never did, so those tenants booted with no memory
// sidecar and every memory call answered 503 "memory plugin is not configured".
// The entrypoint now defaults it, so no provisioner and no environment has to
// know the constant.
//
// Defaulting it on is only SAFE because a sidecar that never gets healthy now
// DEGRADES (tenant boots without memory) instead of aborting boot. Requiring
// memory is a SEPARATE, explicit flag — MEMORY_PLUGIN_REQUIRED — and that
// separation is the whole ballgame: the first cut of this change inferred
// "required" from MEMORY_PLUGIN_URL being SET, which looks right until you
// notice the control plane sets that URL on every cloud tenant already. The
// degrade arm would then have been dead code on EC2/Hetzner/GCP — exactly the
// fleet where the 2026-05-05 `extension "vector" is not available` crash-loop
// happened. TestMemorySidecar_CPInjectedURLAndUnhealthy_StillDegrades pins that.
//
// The test extracts the marked block from the REAL entrypoint-tenant.sh and runs
// it under /bin/sh with stubs, so it can never drift from a hand-kept copy.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	entrypointPath = "../../entrypoint-tenant.sh"
	blockStart     = "# >>> memory-sidecar-boot"
	blockEnd       = "# <<< memory-sidecar-boot"

	// Printed by the driver AFTER the block. Its absence is how we prove the
	// block really aborted the boot rather than merely printing about it.
	bootContinued = "BOOT_CONTINUED"

	// What controlplane tenant_container_env.go / ec2.go inject into EVERY cloud
	// tenant. Not operator intent — the same constant, restated.
	cpInjectedURL = "http://localhost:9100"
)

// memorySidecarBlock returns the marked region of the real entrypoint.
func memorySidecarBlock(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(entrypointPath)
	if err != nil {
		t.Fatalf("read %s: %v", entrypointPath, err)
	}
	s := string(raw)
	i := strings.Index(s, blockStart)
	j := strings.Index(s, blockEnd)
	if i < 0 || j < 0 || j <= i {
		t.Fatalf("markers %q/%q not found in %s — did someone remove them? The test drives the REAL script through these.", blockStart, blockEnd, entrypointPath)
	}
	return s[i:j]
}

// runMemoryBoot executes the block with stubbed sidecar + health probe.
// healthOK=false simulates a plugin that never answers /v1/health 200 (e.g. the
// Postgres has no pgvector). Returns stderr and the shell's exit code.
func runMemoryBoot(t *testing.T, env map[string]string, healthOK bool) (string, int) {
	t.Helper()
	dir := t.TempDir()

	// Stub sidecar binary: just stays alive so $! is a real pid.
	bin := filepath.Join(dir, "memory-plugin-stub")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Stub canvas process. The abort arm does `kill "$CANVAS_PID"`, and CANVAS_PID
	// MUST be a real OTHER process. An earlier version of this test set
	// CANVAS_PID=$$ — the driver's own pid — which made the driver SIGTERM ITSELF.
	// The test then "passed" on exit-code alone even with the `exit 1` deleted: a
	// tautology. Now CANVAS_PID is a separate child, the abort arm can only be
	// proven by BOOT_CONTINUED not being reached.
	//
	// `exec` so CANVAS_PID is the sleep ITSELF, not a shell wrapping it — killing
	// the wrapper would leave the sleep alive, holding the inherited stderr pipe
	// open and stalling every test for the full sleep.
	canvas := filepath.Join(dir, "canvas-stub")
	if err := os.WriteFile(canvas, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// node is the health probe. Exit 0 = healthy, 1 = never healthy.
	nodeRC := 0
	if !healthOK {
		nodeRC = 1
	}
	node := filepath.Join(dir, "node")
	if err := os.WriteFile(node, []byte(fmt.Sprintf("#!/bin/sh\nexit %d\n", nodeRC)), 0o755); err != nil {
		t.Fatal(err)
	}

	// Drive the real block. The canvas stub is launched BEFORE `sleep` is
	// neutered, so it is a genuine background process; the override then makes
	// the block's own 30x retry loop instant.
	script := strings.Join([]string{
		"set -e",
		// >/dev/null 2>&1 so the stub never inherits our stderr pipe: a surviving
		// grandchild holding it open would make exec.Command wait out its lifetime.
		canvas + " >/dev/null 2>&1 &",
		"CANVAS_PID=$!",
		"sleep() { command true; }",
		memorySidecarBlock(t),
		`echo "FINAL_URL=[${MEMORY_PLUGIN_URL}]" >&2`,
		`echo "FINAL_PID=[${MEMORY_PLUGIN_PID}]" >&2`,
		`echo "` + bootContinued + `" >&2`,
		`kill "$CANVAS_PID" 2>/dev/null || true`,
	}, "\n")

	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"MEMORY_PLUGIN_BIN="+bin,
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = &strings.Builder{}
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run: %v", err)
	}
	return stderr.String(), code
}

// baseEnv is a tenant with nothing memory-related configured — the staging shape.
func baseEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":               "postgres://u:p@db/x",
		"MEMORY_PLUGIN_URL":          "",
		"MEMORY_PLUGIN_DISABLE":      "",
		"MEMORY_PLUGIN_REQUIRED":     "",
		"MEMORY_PLUGIN_DATABASE_URL": "",
		"MEMORY_PLUGIN_LISTEN_ADDR":  "",
	}
}

func with(overrides map[string]string) map[string]string {
	e := baseEnv()
	for k, v := range overrides {
		e[k] = v
	}
	return e
}

// The staging case: a tenant whose provisioner sets NOTHING still gets memory.
// This is the whole point — no environment should have to know the constant.
func TestMemorySidecar_DefaultsOnWithNoEnvAtAll(t *testing.T) {
	out, code := runMemoryBoot(t, baseEnv(), true)
	if code != 0 {
		t.Fatalf("tenant must boot; got exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "FINAL_URL=[http://localhost:9100]") {
		t.Errorf("MEMORY_PLUGIN_URL must default to the bundled sidecar; got:\n%s", out)
	}
	if !strings.Contains(out, "sidecar healthy") {
		t.Errorf("sidecar should have been started and gone healthy; got:\n%s", out)
	}
}

// The arm that makes default-on safe: we turned it on, so a sick sidecar must
// NOT brick a tenant that never asked for memory. Without this, a Postgres with
// no pgvector aborts boot for every tenant.
func TestMemorySidecar_DefaultedAndUnhealthy_DegradesInsteadOfAborting(t *testing.T) {
	out, code := runMemoryBoot(t, baseEnv(), false)
	if code != 0 {
		t.Fatalf("a defaulted-on sidecar that never gets healthy must DEGRADE, not abort boot; got exit %d\n%s", code, out)
	}
	if !strings.Contains(out, bootContinued) {
		t.Errorf("boot must continue past the memory block; got:\n%s", out)
	}
	if !strings.Contains(out, "DEGRADING") {
		t.Errorf("expected a loud DEGRADING warning; got:\n%s", out)
	}
	// Blanked so /platform's wiring.Build returns nil => memory endpoints answer a
	// clean 503 instead of dialing a sidecar that is not listening.
	if !strings.Contains(out, "FINAL_URL=[]") {
		t.Errorf("MEMORY_PLUGIN_URL must be blanked on degrade so the platform reports 'not configured'; got:\n%s", out)
	}
}

// THE ONE THAT WOULD HAVE CAUGHT THE FIRST CUT OF THIS PR.
//
// Every cloud tenant is born with MEMORY_PLUGIN_URL='http://localhost:9100'
// already in its env, because the CP injects it (tenant_container_env.go:46,
// ec2.go:2811). If the entrypoint reads that as "an operator explicitly asked
// for memory", the degrade arm above is unreachable on EC2/Hetzner/GCP and an
// unhealthy sidecar crash-loops the whole tenant — which is precisely the
// 2026-05-05 pgvector incident, on precisely the fleet where it happened. The
// degrade arm would have been dead code exactly where it was needed.
//
// A loopback URL names the BUNDLED sidecar. It is not intent. It must degrade.
func TestMemorySidecar_CPInjectedURLAndUnhealthy_StillDegrades(t *testing.T) {
	out, code := runMemoryBoot(t, with(map[string]string{
		"MEMORY_PLUGIN_URL": cpInjectedURL,
	}), false)
	if code != 0 {
		t.Fatalf("a CP-injected loopback URL is the bundled constant restated, NOT a declaration that memory is required — an unhealthy sidecar must DEGRADE. Got exit %d, i.e. every EC2/Hetzner/GCP tenant crash-loops on a Postgres without pgvector:\n%s", code, out)
	}
	if !strings.Contains(out, bootContinued) {
		t.Errorf("boot must continue past the memory block; got:\n%s", out)
	}
	if !strings.Contains(out, "DEGRADING") {
		t.Errorf("expected the DEGRADING warning; got:\n%s", out)
	}
}

// The crash-loop contract, now behind an explicit flag: an operator who declares
// memory REQUIRED gets a tenant that refuses to serve without it.
func TestMemorySidecar_RequiredAndUnhealthy_AbortsBoot(t *testing.T) {
	out, code := runMemoryBoot(t, with(map[string]string{
		"MEMORY_PLUGIN_REQUIRED": "1",
	}), false)
	if code == 0 {
		t.Fatalf("MEMORY_PLUGIN_REQUIRED=1 with a sidecar that never gets healthy must abort boot (crash-loop), not degrade silently\n%s", out)
	}
	// The load-bearing assertion. Exit code alone is NOT enough: the abort arm
	// kills CANVAS_PID, so a driver that killed its own shell would exit non-zero
	// even with the `exit 1` deleted. Reaching BOOT_CONTINUED means the entrypoint
	// carried on booting a tenant whose operator said memory is mandatory.
	if strings.Contains(out, bootContinued) {
		t.Fatalf("boot CONTINUED past the memory block despite MEMORY_PLUGIN_REQUIRED=1 and an unhealthy sidecar — the tenant would serve without the memory its operator declared mandatory:\n%s", out)
	}
	if !strings.Contains(out, "aborting boot") {
		t.Errorf("expected the abort message; got:\n%s", out)
	}
}

// An EXTERNAL plugin (non-loopback URL) is someone else's process on someone
// else's host. We must not start a bundled sidecar nobody dials — and above all
// must not BLANK their URL, which would silently switch their memory off. The
// external-plugin deployment is a documented config (workspace-server/Dockerfile).
func TestMemorySidecar_ExternalURL_NoSidecarAndURLPreserved(t *testing.T) {
	const external = "http://memory.internal:9100"
	out, code := runMemoryBoot(t, with(map[string]string{
		"MEMORY_PLUGIN_URL": external,
	}), true)
	if code != 0 {
		t.Fatalf("an external plugin URL must not fail boot; got exit %d\n%s", code, out)
	}
	if strings.Contains(out, "starting sidecar") {
		t.Errorf("the bundled sidecar must NOT start when the URL points at an external plugin; got:\n%s", out)
	}
	if !strings.Contains(out, "FINAL_URL=["+external+"]") {
		t.Errorf("an operator's external MEMORY_PLUGIN_URL must survive the entrypoint untouched — blanking it silently disables their memory service; got:\n%s", out)
	}
}

// MEMORY_PLUGIN_DISABLE=1 *together with* an external URL. This is not an exotic
// combination — it is the DOCUMENTED way to run an external plugin (Dockerfile:
// "Set MEMORY_PLUGIN_DISABLE=1 to force-skip the sidecar even with cutover env
// set (e.g. running the plugin externally on a separate host)"). DISABLE means
// "do not start the BUNDLED sidecar"; there is no sidecar to skip here, so it
// must NOT blank the operator's URL and silently kill their memory service.
//
// The first cut of this change tested DISABLE before the bundled/external
// classification and did exactly that: FINAL_URL came back empty. The
// external-URL test above did not set DISABLE, which is why the hole shipped.
func TestMemorySidecar_DisableWithExternalURL_MustNotBlankIt(t *testing.T) {
	const external = "http://memory.internal:9100"
	out, code := runMemoryBoot(t, with(map[string]string{
		"MEMORY_PLUGIN_DISABLE": "1",
		"MEMORY_PLUGIN_URL":     external,
	}), true)
	if code != 0 {
		t.Fatalf("DISABLE + an external plugin URL must not fail boot; got exit %d\n%s", code, out)
	}
	if strings.Contains(out, "starting sidecar") {
		t.Errorf("no bundled sidecar may start for an external URL; got:\n%s", out)
	}
	if !strings.Contains(out, "FINAL_URL=["+external+"]") {
		t.Errorf("MEMORY_PLUGIN_DISABLE must not blank an EXTERNAL MEMORY_PLUGIN_URL — that silently "+
			"disables the operator's own memory service, and DISABLE+external is the documented way to "+
			"run one; got:\n%s", out)
	}
}

// The opt-out still works, and it leaves the platform reporting "not configured"
// rather than dialing a sidecar that was never started.
func TestMemorySidecar_DisableSkipsAndBlanksURL(t *testing.T) {
	out, code := runMemoryBoot(t, with(map[string]string{
		"MEMORY_PLUGIN_DISABLE": "1",
	}), true)
	if code != 0 {
		t.Fatalf("MEMORY_PLUGIN_DISABLE must not fail boot; got exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "FINAL_URL=[]") {
		t.Errorf("disable must blank the URL so the platform 503s cleanly; got:\n%s", out)
	}
	if strings.Contains(out, "starting sidecar") {
		t.Errorf("sidecar must not start when disabled; got:\n%s", out)
	}
}

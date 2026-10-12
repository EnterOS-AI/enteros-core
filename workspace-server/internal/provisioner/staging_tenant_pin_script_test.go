package provisioner_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Regression guard for scripts/deploy/advance-staging-tenant-pin.sh.
//
// The staging tenant-image CD advances TWO sources of truth that must always move
// together:
//
//  1. the control-plane runtime_image_pins DB row (dynamic; fresh provisions pick
//     it up with no CP restart), promoted via /cp/admin/runtime-image/promote, and
//  2. LOCAL_TENANT_IMAGE in Infisical /shared/controlplane (the image a rebooted
//     or freshly-provisioned CP reads at BOOT as the tenant-app default).
//
// Rolling the DB pin while leaving LOCAL_TENANT_IMAGE stale is the EXACT drift that
// broke prod fresh-org signup (the pin was patched on running containers but never
// written back to the boot SSOT, so every fresh org provisioned onto the old image
// and its concierge failed). These assertions read the script SOURCE so they go RED
// if the SSOT write is removed, points at the wrong Infisical env/path, or stops
// verifying that the write actually landed — without needing a live Infisical.
func readStagingPinScript(t *testing.T) string {
	t.Helper()
	repoRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	path := filepath.Join(repoRoot, "scripts", "deploy", "advance-staging-tenant-pin.sh")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestStagingTenantPin_WritesTheBootDefaultSSOT(t *testing.T) {
	src := readStagingPinScript(t)

	// The DB-pin promote must still be present (this is the pre-existing behaviour).
	if !strings.Contains(src, "/cp/admin/runtime-image/promote") {
		t.Error("script no longer promotes the runtime_image_pins DB row — the CP pin write is gone")
	}

	// It must ALSO write the boot-default SSOT. write_ssot_pin is the function that
	// does the Infisical LOCAL_TENANT_IMAGE write; require both its definition and
	// at least one invocation, so a defined-but-never-called dead function fails too.
	if !strings.Contains(src, "write_ssot_pin()") {
		t.Fatal("write_ssot_pin() is not defined — the LOCAL_TENANT_IMAGE (boot-default) SSOT write is missing")
	}
	calls := strings.Count(src, "write_ssot_pin ")
	if calls < 1 {
		t.Error("write_ssot_pin is defined but never called — the SSOT write never runs")
	}

	// The write must target the tenant-app boot secret, not something else.
	//
	// Anchored on the ASSIGNMENT, not a bare substring. `strings.Contains(src,
	// "LOCAL_TENANT_IMAGE")` also matches the comments in this script, so renaming
	// the actual default to anything at all still satisfied it — the assertion was
	// green with the behaviour it guards removed.
	if !regexp.MustCompile(`SSOT_SECRET_NAME="\$\{SSOT_SECRET_NAME:-LOCAL_TENANT_IMAGE\}"`).MatchString(src) {
		t.Error("script does not default SSOT_SECRET_NAME to LOCAL_TENANT_IMAGE — the boot-default SSOT is not being written (comments mentioning the name do not count)")
	}
}

func TestStagingTenantPin_SSOTWriteIsVerifiedNotAssumed(t *testing.T) {
	src := readStagingPinScript(t)

	// A write that trusts an HTTP 2xx without reading the value back is exactly the
	// failure mode that hid the stale prod pin. The write function must read the
	// value back and FATAL on mismatch.
	fn := extractShellFunc(t, src, "write_ssot_pin")
	if !strings.Contains(fn, "inf_get_raw") {
		t.Error("write_ssot_pin does not read the secret back (no inf_get_raw call) — it trusts the HTTP status")
	}
	if !regexp.MustCompile(`FATAL:.*did not take`).MatchString(fn) {
		t.Error("write_ssot_pin does not FATAL when the read-back value mismatches the intended ref")
	}
}

func TestStagingTenantPin_UsesStagingEnvNotProduction(t *testing.T) {
	src := readStagingPinScript(t)

	// Infisical's production env slug is `prod`, never `production`; and this is the
	// STAGING script, so the env must default to `staging`. A literal `production`
	// slug anywhere is the silent-no-op bug that stale-pinned prod.
	if regexp.MustCompile(`environment=production\b`).MatchString(src) ||
		strings.Contains(src, `INFISICAL_ENV:-production`) {
		t.Error("script uses the nonexistent `production` Infisical slug — reads/writes there 404 silently")
	}
	if !strings.Contains(src, "INFISICAL_ENV:-staging") {
		t.Error("script does not default INFISICAL_ENV to `staging`")
	}

	// The boot default lives at /shared/controlplane (NOT the -admin token path).
	//
	// Anchored on the ASSIGNMENT and terminated, because the obvious substring check
	// —`strings.Contains(src, "CP_SSOT_PATH:-/shared/controlplane")` — PREFIX-MATCHES
	// `/shared/controlplane-admin`. Repointing this write into the CP-admin-token
	// folder, the one thing this test names as forbidden, sailed straight through it.
	// A guard that passes on the case it exists to forbid is not a guard.
	if !regexp.MustCompile(`CP_SSOT_PATH="\$\{CP_SSOT_PATH:-/shared/controlplane\}"`).MatchString(src) {
		t.Error("script does not default the boot-default SSOT path to exactly /shared/controlplane (note: /shared/controlplane-admin is the CP ADMIN TOKEN path — the boot default must not be written there)")
	}
}

func TestStagingTenantPin_InfisicalLoginIsEchoFree(t *testing.T) {
	src := readStagingPinScript(t)

	// infisical_login runs inside the `CP_TOKEN="$(fetch_cp_token)"` command
	// substitution, so ANY stdout it emits is captured into CP_TOKEN and corrupts
	// the bearer header. It must not echo ::add-mask:: (or anything) — masking is
	// done by the caller in the parent shell.
	fn := extractShellFunc(t, src, "infisical_login")
	if strings.Contains(fn, "::add-mask::") {
		t.Error("infisical_login echoes ::add-mask:: — captured into CP_TOKEN inside the fetch_cp_token substitution, corrupting the CP bearer token")
	}
}

func TestStagingTenantPin_SSOTWriteRequiredUpFront(t *testing.T) {
	src := readStagingPinScript(t)

	// The SSOT write must not fatal AFTER the DB pin is mutated (half-apply). The
	// script asserts the need for Infisical creds up front, before the pin read/
	// promote, and honors SKIP_SSOT_WRITE as the conscious opt-out.
	promoteAt := strings.Index(src, "/cp/admin/runtime-image/promote")
	needAt := strings.Index(src, "need_infisical")
	if needAt < 0 {
		t.Fatal("no up-front `need_infisical` auth gate — a creds-less run can half-apply (DB pin moved, SSOT stale)")
	}
	if promoteAt >= 0 && needAt > promoteAt {
		t.Error("the Infisical-creds gate runs AFTER the promote — a missing-creds failure would leave a half-applied state")
	}
	if !strings.Contains(src, "SKIP_SSOT_WRITE") {
		t.Error("no SKIP_SSOT_WRITE opt-out — a CP_ADMIN_API_TOKEN-only caller cannot run without Infisical creds")
	}
}

func TestStagingTenantPin_DryRunPerformsNoWrite(t *testing.T) {
	src := readStagingPinScript(t)

	// write_ssot_pin is reached on the pin-already-at-target path, which runs before
	// the top-level DRY_RUN guard — so write_ssot_pin itself must honor DRY_RUN and
	// perform no real write, or `--dry-run` silently mutates the SSOT.
	fn := extractShellFunc(t, src, "write_ssot_pin")
	if !regexp.MustCompile(`DRY_RUN.*=.*"?1"?`).MatchString(fn) {
		t.Error("write_ssot_pin does not guard on DRY_RUN — --dry-run can perform a real Infisical write on the already-at-target path")
	}
	if !strings.Contains(fn, "DRY-RUN: would set") {
		t.Error("write_ssot_pin has no dry-run preview log")
	}
}

func TestStagingTenantPin_ReconcilesSSOTEvenWhenDBPinUnchanged(t *testing.T) {
	src := readStagingPinScript(t)

	// The boot-default SSOT drifts INDEPENDENTLY of the DB pin (staging today: DB
	// pin current, LOCAL_TENANT_IMAGE stale). The "pin already at target" early-exit
	// must still reconcile the SSOT rather than short-circuiting before the write.
	idx := strings.Index(src, "DB pin already at target")
	if idx < 0 {
		t.Fatal("no `DB pin already at target` branch found — cannot confirm it reconciles the SSOT")
	}
	// Between that branch and its `exit 0`, write_ssot_pin must be called.
	rest := src[idx:]
	exitAt := strings.Index(rest, "exit 0")
	if exitAt < 0 || !strings.Contains(rest[:exitAt], "write_ssot_pin") {
		t.Error("the pin-already-at-target branch exits without reconciling LOCAL_TENANT_IMAGE — drift survives")
	}
}

// extractShellFunc returns the body of a `name() { ... }` shell function using
// brace-depth matching. Good enough for these well-formed deploy scripts.
func extractShellFunc(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, name+"() {")
	if start < 0 {
		t.Fatalf("shell function %s() not found", name)
	}
	body := src[start:]
	depth := 0
	for i, r := range body {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return body[:i+1]
			}
		}
	}
	t.Fatalf("unbalanced braces extracting %s()", name)
	return ""
}

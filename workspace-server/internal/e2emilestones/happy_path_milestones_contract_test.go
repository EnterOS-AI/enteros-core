package e2emilestones

// Happy-path milestone derive-gate (RFC molecule-core#4428, Phase 1 — SHADOW / non-blocking).
//
// Asserts that the staging-full-saas E2E runner's hardcoded happy-path
// `required=` milestone set (tests/e2e/test_staging_full_saas.sh,
// require_live_or_die) is exactly the id set declared by the vendored contract
// binding molcontracts.HappyPathMilestones (the SSOT). This is the core-side
// derive-gate for issue #88: it LOCKS the runner's false-green-on-skip guard
// to the SDK milestone SSOT so Phase 2's extension of the milestone set cannot
// silently drift the two apart.
//
// Phase 1 runs this ONLY as a post-merge / scheduled shadow signal
// (.gitea/workflows/sdk-route-milestone-contract-drift.yml, push[main]/schedule,
// NOT pull_request) — per task #113 a pull_request-triggered status would post
// a commit status that core main's branch-protection required_contexts=['*']
// counts, jamming the merge queue even with continue-on-error. It is therefore
// NOT wired to pull_request and NOT in .gitea/required-contexts.txt.
//
// Phase 1 scope note: this gate deliberately does NOT extend or re-order the
// milestone set — it only proves the runner and the SSOT declare the same ids.
// Both sides are the same 4 today (provisioned, tenant_online, workspace_online,
// a2a_roundtrip), so this passes; the lock is what matters.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"go.moleculesai.app/sdk/gen/go/molcontracts"
)

// Matches: local required="provisioned tenant_online workspace_online a2a_roundtrip"
var requiredMilestonesRE = regexp.MustCompile(`local\s+required="([^"]*)"`)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate repo root")
	}
	// .../workspace-server/internal/e2emilestones/<file> -> repo root is three up.
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}

// runnerRequiredMilestones extracts the space-delimited `required=` set from the
// staging-full-saas E2E runner.
func runnerRequiredMilestones(t *testing.T) []string {
	t.Helper()
	runner := filepath.Join(repoRoot(t), "tests", "e2e", "test_staging_full_saas.sh")
	b, err := os.ReadFile(runner)
	if err != nil {
		t.Fatalf("read E2E runner (%s): %v", runner, err)
	}
	m := requiredMilestonesRE.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("could not find `local required=\"...\"` in %s — the runner's "+
			"happy-path guard moved or was renamed; update this derive-gate", runner)
	}
	return strings.Fields(m[1])
}

// contractDriftGateEnv, when unset, makes this shadow derive-gate SKIP. See the
// long note in internal/router/sdk_routes_contract_test.go: ci.yml runs
// `go test ./...` on every pull_request inside the required `CI / all-required`
// job, so an unguarded drift here would block the PR. The gate executes for real
// ONLY in .gitea/workflows/sdk-route-milestone-contract-drift.yml (push[main]/
// dispatch), which sets this env var. Locally:
// MOLECULE_RUN_CONTRACT_DRIFT_GATES=1 go test ...
const contractDriftGateEnv = "MOLECULE_RUN_CONTRACT_DRIFT_GATES"

// TestHappyPathMilestonesMatchRunner is the shadow milestone derive-gate: the
// runner's `required=` id set must equal molcontracts.HappyPathMilestones ids.
func TestHappyPathMilestonesMatchRunner(t *testing.T) {
	if os.Getenv(contractDriftGateEnv) == "" {
		t.Skipf("shadow contract-drift gate (RFC #4428 Phase 1, issue #88) — set %s=1 to run. "+
			"Skipped by default so it never blocks a PR via ci.yml's `go test ./...`; it executes "+
			"post-merge in sdk-route-milestone-contract-drift.yml.", contractDriftGateEnv)
	}
	runnerIDs := runnerRequiredMilestones(t)
	if len(runnerIDs) == 0 {
		t.Fatal("parsed zero milestone ids from the E2E runner")
	}
	if len(molcontracts.HappyPathMilestones) == 0 {
		t.Fatal("molcontracts.HappyPathMilestones is empty — vendored binding is wrong")
	}

	var sdkIDs []string
	for _, ms := range molcontracts.HappyPathMilestones {
		sdkIDs = append(sdkIDs, ms.ID)
	}

	runnerSet := toSet(runnerIDs)
	sdkSet := toSet(sdkIDs)

	var missingInRunner, extraInRunner []string
	for id := range sdkSet {
		if !runnerSet[id] {
			missingInRunner = append(missingInRunner, id)
		}
	}
	for id := range runnerSet {
		if !sdkSet[id] {
			extraInRunner = append(extraInRunner, id)
		}
	}
	sort.Strings(missingInRunner)
	sort.Strings(extraInRunner)

	if len(missingInRunner) > 0 {
		t.Errorf("E2E runner `required=` is MISSING milestone(s) declared by the SSOT "+
			"molcontracts.HappyPathMilestones: %v — the runner drifted BEHIND the SDK "+
			"milestone set (RFC #4428, issue #88). SDK=%v runner=%v",
			missingInRunner, sortedKeys(sdkSet), sortedKeys(runnerSet))
	}
	if len(extraInRunner) > 0 {
		t.Errorf("E2E runner `required=` declares milestone(s) NOT in the SSOT "+
			"molcontracts.HappyPathMilestones: %v — the runner drifted AHEAD of the SDK "+
			"milestone set (RFC #4428, issue #88). SDK=%v runner=%v",
			extraInRunner, sortedKeys(sdkSet), sortedKeys(runnerSet))
	}
}

func toSet(xs []string) map[string]bool {
	s := make(map[string]bool, len(xs))
	for _, x := range xs {
		s[x] = true
	}
	return s
}

func sortedKeys(s map[string]bool) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

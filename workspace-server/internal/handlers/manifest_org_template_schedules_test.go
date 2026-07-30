// manifest_org_template_schedules_test.go — the pinned-ref half of M3.
//
// WHAT THIS EXISTS TO CATCH
//
// M3 moves a workspace's schedules off a top-level `schedules:` key and onto
// the owning plugin's own config (`plugins[].config.schedules`), then DELETES
// core's legacy renderer (renderTemplateSchedulesYAML). The deletion was
// cleared as "zero producers fleet-wide" — a survey of every org-template
// repo's `main`. That survey looked at the wrong refs.
//
// manifest.json pins each org template to an IMMUTABLE SHA, and the tenant
// image ships THOSE refs, not `main`. On 2026-07-30 the pins were:
//
//	molecule-dev  990d7b23  (pre-M3)  →  9 files with top-level `schedules:`
//	              51e20676  (main)    →  0
//
// So the production image was still a producer of the legacy shape while the
// repo's main branch had been migrated for two days, and deleting the renderer
// would have silently stopped 9 schedules — the exact failure class M3 exists to
// remove. A repo-main survey cannot see this; only the pinned ref can.
//
// THE INVARIANT, and why it is strict
//
// No pinned org template may declare a COLUMN-0 `schedules:` key. That is
// deliberately stricter than "the renderer still exists": once M3 lands, the
// legacy location is dead config, and a pin bump that reintroduces it would be
// a silent regression discovered in production rather than in review. Making
// the flag day a gate is cheaper than making it a postmortem.
//
// Nested `schedules:` (under `plugins[].config:`) is the CURRENT location and is
// what this test wants to see, so it is matched only at column 0 — a purely
// textual check, because these files carry `!include` / `!external` custom tags
// that a YAML round-trip cannot load (the same reason the migration itself was a
// text transform).
//
// Network-dependent, following manifest_pinning_test.go exactly: skips when
// Gitea is unreachable or manifest.json is not readable.

package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// topLevelSchedulesKey matches a `schedules:` mapping key at column 0 — the
// LEGACY location. Anchored with (?m) per line; leading whitespace is what
// distinguishes the current nested location, so it must not be allowed.
var topLevelSchedulesKey = regexp.MustCompile(`(?m)^schedules:\s*$`)

// orgTemplateYAMLPath reports whether a tree path is a template document this
// test should read. Workflow definitions and repo metadata are not templates and
// legitimately contain unrelated keys.
func orgTemplateYAMLPath(p string) bool {
	if !strings.HasSuffix(p, ".yaml") && !strings.HasSuffix(p, ".yml") {
		return false
	}
	if strings.HasPrefix(p, ".gitea/") || strings.HasPrefix(p, ".github/") {
		return false
	}
	return p != "repo-meta.yaml"
}

// TestManifest_PinnedOrgTemplates_UseCurrentSchedulesLocation asserts that every
// org template the image SHIPS (i.e. at its pinned SHA, not at repo main) has
// been migrated off the legacy top-level `schedules:` key.
func TestManifest_PinnedOrgTemplates_UseCurrentSchedulesLocation(t *testing.T) {
	if !giteaReachableForTest() {
		t.Skip("Gitea unreachable (offline CI lane); skipping pinned org-template schedules-location check")
	}
	data, err := readRealManifestForPinningTest(t)
	if err != nil {
		t.Skipf("manifest.json not readable: %v", err)
	}
	var m struct {
		OrgTemplates []manifestEntry `json:"org_templates"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	if len(m.OrgTemplates) == 0 {
		t.Fatal("no org_templates entries (test invariant broken)")
	}

	client := &http.Client{Timeout: 20 * time.Second}
	auth := giteaBasicAuthForTest(t)

	for _, e := range m.OrgTemplates {
		paths, err := orgTemplateTreeYAMLs(client, auth, e.Repo, e.Ref)
		if err != nil {
			t.Errorf("entry %q (%s@%s): %v", e.Name, e.Repo, e.Ref, err)
			continue
		}
		if len(paths) == 0 {
			t.Errorf("entry %q (%s@%s): pinned ref exposes NO template YAML — a pin this empty would provision nothing", e.Name, e.Repo, e.Ref)
			continue
		}
		for _, p := range paths {
			body, err := orgTemplateFileAtRef(client, auth, e.Repo, e.Ref, p)
			if err != nil {
				t.Errorf("entry %q (%s@%s): reading %s: %v", e.Name, e.Repo, e.Ref, p, err)
				continue
			}
			if topLevelSchedulesKey.MatchString(body) {
				t.Errorf(`entry %q (%s@%s): %s declares a top-level `+"`schedules:`"+` key — the LEGACY location.

The image ships the PINNED ref, so this is a live producer of the shape M3
removes: once renderTemplateSchedulesYAML is deleted, every schedule in this
file silently stops firing. A survey of the repo's main branch does not see
this — only the pin does.

Fix: migrate the file to plugins[].config.schedules in the template repo, then
bump this entry's ref in manifest.json (one reviewed PR, per the pinning
contract).`, e.Name, e.Repo, e.Ref, p)
			}
		}
	}
}

// orgTemplateTreeYAMLs lists the template YAML paths at a pinned ref.
func orgTemplateTreeYAMLs(client *http.Client, auth, repo, ref string) ([]string, error) {
	url := "https://git.moleculesai.app/api/v1/repos/" + repo + "/git/trees/" + ref + "?recursive=1&per_page=1000"
	req, _ := http.NewRequest("GET", url, nil)
	setGiteaTestHeaders(req, auth)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tree lookup failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("tree lookup returned HTTP %d", resp.StatusCode)
	}
	var treeResp struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&treeResp); err != nil {
		return nil, fmt.Errorf("tree JSON parse failed: %w", err)
	}
	// A truncated tree would silently narrow the scan — the gate would pass by
	// not looking, which is the failure mode it exists to prevent.
	if treeResp.Truncated {
		return nil, fmt.Errorf("tree listing was TRUNCATED; the scan would be incomplete")
	}
	out := []string{}
	for _, n := range treeResp.Tree {
		if n.Type == "blob" && orgTemplateYAMLPath(n.Path) {
			out = append(out, n.Path)
		}
	}
	return out, nil
}

// orgTemplateFileAtRef fetches one file's raw bytes at a pinned ref.
func orgTemplateFileAtRef(client *http.Client, auth, repo, ref, path string) (string, error) {
	url := "https://git.moleculesai.app/api/v1/repos/" + repo + "/raw/" + path + "?ref=" + ref
	req, _ := http.NewRequest("GET", url, nil)
	setGiteaTestHeaders(req, auth)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("raw fetch returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// TestTopLevelSchedulesKey_MatchesOnlyColumnZero pins the discriminator the gate
// hangs on. Without it the test would either miss the legacy shape or condemn
// the current one — both silent, in opposite directions.
func TestTopLevelSchedulesKey_MatchesOnlyColumnZero(t *testing.T) {
	legacy := "name: PM\nschedules:\n  - name: pulse\n    cron_expr: \"0 * * * *\"\n"
	current := "name: PM\nplugins:\n  - source: s\n    config:\n      schedules:\n        - name: pulse\n"
	if !topLevelSchedulesKey.MatchString(legacy) {
		t.Error("legacy top-level schedules: must match")
	}
	if topLevelSchedulesKey.MatchString(current) {
		t.Error("nested plugins[].config.schedules must NOT match — that is the current location")
	}
	// A `schedules:` with a same-line value is not a block and not the shape the
	// renderer read; excluding it keeps the gate honest about what it claims.
	if topLevelSchedulesKey.MatchString("schedules: []\n") {
		t.Error("`schedules: []` is not a block key and must not match")
	}
}

func TestOrgTemplateYAMLPath_SkipsNonTemplates(t *testing.T) {
	for _, p := range []string{".gitea/workflows/ci.yml", ".github/workflows/ci.yml", "repo-meta.yaml", "README.md", "coordinator/skills/x.md"} {
		if orgTemplateYAMLPath(p) {
			t.Errorf("%q must not be scanned as a template document", p)
		}
	}
	for _, p := range []string{"org.yaml", "teams/pm.yaml", "community-manager/workspace.yaml"} {
		if !orgTemplateYAMLPath(p) {
			t.Errorf("%q must be scanned", p)
		}
	}
}

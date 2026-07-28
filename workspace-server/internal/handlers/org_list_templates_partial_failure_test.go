package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

// GET /org/templates used to return 200 with a broken template silently ABSENT:
// each load failure logged and `continue`d, so the caller could not tell "this
// org has 3 templates" from "it has 4 and one is broken".
//
// That loud-log/silent-wire shape is how molecule-core#4889 hid — molecule-dev
// was unimportable on EVERY image while /org/templates cheerfully returned the
// other two and the Canvas palette merely looked short. The failure was in the
// logs the whole time and on the wire nowhere.
//
// These tests pin the corrected contract: a template that cannot be loaded is
// REPORTED as present-but-unavailable, never omitted.

func newOrgTemplatesRequest(t *testing.T, orgDir string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := &OrgHandler{orgDir: orgDir}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/org/templates", nil)
	h.ListTemplates(c)
	return w
}

func decodeTemplates(t *testing.T, w *httptest.ResponseRecorder) []map[string]interface{} {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return got
}

func writeOrgTemplateDir(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "org.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const healthyOrgYAML = `name: Healthy Org
description: fine
workspaces:
  - name: Root
    runtime: claude-code
`

// TestListTemplates_BrokenTemplateIsReportedNotOmitted is the core assertion.
func TestListTemplates_BrokenTemplateIsReportedNotOmitted(t *testing.T) {
	root := t.TempDir()
	writeOrgTemplateDir(t, root, "healthy", healthyOrgYAML)
	// Well-formed YAML whose TYPES do not fit OrgTemplate — this is the case
	// that genuinely reaches yaml.Unmarshal. Syntactically-invalid YAML never
	// gets that far: resolveYAMLIncludes parses into a node tree first, so it
	// surfaces as include_expansion_failed (covered separately below).
	writeOrgTemplateDir(t, root, "broken-yaml", "name: Bad Types\nworkspaces: not-a-list\n")

	got := decodeTemplates(t, newOrgTemplatesRequest(t, root))

	byDir := map[string]map[string]interface{}{}
	for _, e := range got {
		byDir[e["dir"].(string)] = e
	}

	if len(got) != 2 {
		t.Fatalf("listing returned %d entries, want 2 — a broken template must be REPORTED, not dropped (got %v)", len(got), byDir)
	}
	if _, ok := byDir["healthy"]; !ok {
		t.Errorf("healthy template missing from listing: %v", byDir)
	}
	bad, ok := byDir["broken-yaml"]
	if !ok {
		t.Fatalf("broken template was OMITTED — this is the #4889 silent-wire regression; entries: %v", byDir)
	}
	if bad["error"] == nil || bad["error"] == "" {
		t.Errorf("broken entry carries no error message: %v", bad)
	}
	if bad["reason"] != "yaml_invalid" {
		t.Errorf("reason = %v, want yaml_invalid", bad["reason"])
	}
	if n, _ := bad["workspaces"].(float64); n != 0 {
		t.Errorf("broken entry reports workspaces=%v, want 0 — a template we could not parse must not look importable", bad["workspaces"])
	}
}

// TestListTemplates_BrokenIncludeIsReported covers the other failure path: the
// tree parses but an !include target is missing. This is the per-agent layout's
// most likely real breakage (a renamed or deleted file).
func TestListTemplates_BrokenIncludeIsReported(t *testing.T) {
	root := t.TempDir()
	writeOrgTemplateDir(t, root, "healthy", healthyOrgYAML)
	writeOrgTemplateDir(t, root, "broken-include", "name: Broken\nworkspaces:\n  - !include ./missing/workspace.yaml\n")

	got := decodeTemplates(t, newOrgTemplatesRequest(t, root))

	var bad map[string]interface{}
	for _, e := range got {
		if e["dir"] == "broken-include" {
			bad = e
		}
	}
	if bad == nil {
		t.Fatalf("template with a missing !include target was OMITTED rather than reported; entries: %v", got)
	}
	if bad["reason"] != "include_expansion_failed" {
		t.Errorf("reason = %v, want include_expansion_failed", bad["reason"])
	}
	if msg, _ := bad["error"].(string); msg == "" {
		t.Errorf("broken-include entry carries no error detail: %v", bad)
	}
}

// TestListTemplates_HealthyListingUnchanged guards the other direction: adding
// failure reporting must not alter the shape of a healthy entry, or every
// existing consumer breaks.
func TestListTemplates_HealthyListingUnchanged(t *testing.T) {
	root := t.TempDir()
	writeOrgTemplateDir(t, root, "healthy", healthyOrgYAML)

	got := decodeTemplates(t, newOrgTemplatesRequest(t, root))
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1: %v", len(got), got)
	}
	e := got[0]
	if e["name"] != "Healthy Org" {
		t.Errorf("name = %v, want %q", e["name"], "Healthy Org")
	}
	if n, _ := e["workspaces"].(float64); n != 1 {
		t.Errorf("workspaces = %v, want 1", e["workspaces"])
	}
	if _, present := e["error"]; present {
		t.Errorf("healthy entry must NOT carry an error key: %v", e)
	}
}

// TestListTemplates_MalformedYAMLIsReported covers syntactically-invalid YAML.
// It surfaces as include_expansion_failed rather than yaml_invalid because
// resolveYAMLIncludes parses the document into a node tree BEFORE unmarshal —
// the reason string names the stage that actually rejected it. Asserted
// explicitly so the distinction is deliberate rather than incidental.
func TestListTemplates_MalformedYAMLIsReported(t *testing.T) {
	root := t.TempDir()
	writeOrgTemplateDir(t, root, "malformed", "name: [unclosed\n  bad: : :\n")

	got := decodeTemplates(t, newOrgTemplatesRequest(t, root))
	if len(got) != 1 {
		t.Fatalf("malformed template was OMITTED rather than reported: %v", got)
	}
	if got[0]["reason"] != "include_expansion_failed" {
		t.Errorf("reason = %v, want include_expansion_failed (expansion parses first)", got[0]["reason"])
	}
}

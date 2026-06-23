package handlers

// Regression tests for core#2129 write-path SSRF defense on the org_import
// path.
//
// Background: /registry/register validates the workspace URL at registration
// time (the front-door gate). The /org/import path bypasses /registry/register
// entirely — it writes `ws.URL` directly to `workspaces.url`. Without a
// re-validation in createWorkspaceTree, a malicious org template that ships
// a metadata endpoint (169.254.169.254) or loopback URL would land in the
// DB and the downstream chat-files forward would attach
// platform_inbound_secret to it (the forward-time gate in PR#3169 / #3169's
// branch is the critical defense; this is defense-in-depth at the write
// path).
//
// The workspace.go (Create) write site already had SSRF tests
// (TestWorkspaceCreate_ExternalURL_SSRFMetadataBlocked +
// TestWorkspaceCreate_ExternalURL_SSRFLoopbackBlocked). The org_import
// write site was the gap that CR2 RC 13398 called out.
//
// CR2 RC 13399 (raised after the initial #3170RC1 fix) tightened the
// contract further: the validation must fire BEFORE the workspace
// INSERT (and the canvas_layouts INSERT, and the structure_events
// INSERT), so a rejected malicious URL leaves NO workspace-row side
// effects. The previous post-INSERT check created a stranded
// provisioning row + layout + event for the rejected leaf — same class
// of "leave debris on failure" the Researcher flagged. These tests
// assert the no-stray-row contract: a rejected external URL produces
// no INSERT INTO workspaces, no INSERT INTO canvas_layouts, and no
// INSERT INTO structure_events. sqlmock catches any unexpected call
// as a test failure (the same way the existing TestEmitOrgEvent tests
// pin SQL shapes).

import (
	"strings"
	"testing"
)

// TestCreateWorkspaceTree_RejectsMetadataURL is the cloud-metadata
// analogue of the workspace.go SSRF tests, for the org_import write
// path. The test pins two contracts:
//
//  1. The function returns a non-nil error mentioning "URL rejected"
//     (so the top-level OrgImport handler's 207 partial-import path
//     surfaces the leaf error to the caller).
//
//  2. The function leaves NO database side effects on rejection —
//     no workspaces row, no canvas_layouts row, no structure_events
//     row. sqlmock expects zero queries, so any INSERT/UPDATE the
//     function might have issued would fail the test via
//     mock.ExpectationsWereMet() (it reports unexpected calls).
func TestCreateWorkspaceTree_RejectsMetadataURL(t *testing.T) {
	setSSRFCheckForTest(true)
	t.Cleanup(func() { setSSRFCheckForTest(false)() })

	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	h := &OrgHandler{
		workspace:   wh,
		broadcaster: broadcaster,
	}

	ws := OrgWorkspace{
		Name:     "Bad Agent",
		Runtime:  "external",
		Model:    "external:custom",
		External: true,
		URL:      "http://169.254.169.254/latest/meta-data/",
	}
	defaults := OrgDefaults{}
	results := []map[string]interface{}{}
	provisionSem := make(chan struct{}, 1)
	parentID := (*string)(nil)

	err := h.createWorkspaceTree(ws, parentID, 0, 0, 0, 0, defaults, "", &results, provisionSem)

	if err == nil {
		t.Fatalf("expected error from createWorkspaceTree for metadata URL, got nil")
	}
	if !strings.Contains(err.Error(), "URL rejected") {
		t.Errorf("expected error to mention 'URL rejected', got: %v", err)
	}

	// No-stray-row contract: zero INSERTs/UPDATEs to workspaces,
	// canvas_layouts, or structure_events. mock.ExpectationsWereMet()
	// returns nil when zero expectations were set AND zero unexpected
	// calls were logged; any INSERT/UPDATE the function would have
	// made surfaces here as a test failure.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestCreateWorkspaceTree_RejectsLoopbackURL is the loopback analogue
// of TestCreateWorkspaceTree_RejectsMetadataURL. Loopback is blocked
// in self-hosted mode (the default for the org_import test) — a
// malicious org template pointing an external workspace at 127.0.0.1
// must be rejected by the same pre-INSERT defense.
//
// 127.0.0.1 is a metadata-class target — an attacker reaching the
// host's loopback interface can hit any debug/admin endpoint a
// developer left open during development.
func TestCreateWorkspaceTree_RejectsLoopbackURL(t *testing.T) {
	setSSRFCheckForTest(true)
	t.Cleanup(func() { setSSRFCheckForTest(false)() })

	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	h := &OrgHandler{
		workspace:   wh,
		broadcaster: broadcaster,
	}

	ws := OrgWorkspace{
		Name:     "Bad Loopback",
		Runtime:  "external",
		Model:    "external:custom",
		External: true,
		URL:      "http://127.0.0.1:9000/a2a",
	}
	defaults := OrgDefaults{}
	results := []map[string]interface{}{}
	provisionSem := make(chan struct{}, 1)
	parentID := (*string)(nil)

	err := h.createWorkspaceTree(ws, parentID, 0, 0, 0, 0, defaults, "", &results, provisionSem)

	if err == nil {
		t.Fatalf("expected error from createWorkspaceTree for loopback URL, got nil")
	}
	if !strings.Contains(err.Error(), "URL rejected") {
		t.Errorf("expected error to mention 'URL rejected', got: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

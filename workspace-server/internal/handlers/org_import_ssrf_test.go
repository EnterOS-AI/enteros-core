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
// write site was the gap that CR2 RC 13398 called out — the fix is in
// createWorkspaceTree (org_import.go:266) but the test coverage was
// missing. This file closes the gap.

import (
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// TestCreateWorkspaceTree_RejectsMetadataURL exercises the org_import
// write-path SSRF defense for an external workspace whose URL points at
// the cloud-metadata endpoint (169.254.169.254). The block must fire
// AFTER the workspaces INSERT (the function inserts the row first, then
// validates the URL before the status/url UPDATE) so the rejection
// surfaces as a per-leaf error to the caller (the top-level OrgImport
// handler returns 207 partial-import on a per-workspace error, so a
// per-leaf SSRF rejection doesn't abort the rest of the tree).
//
// Key assertions:
//   * createWorkspaceTree returns a non-nil error mentioning "URL rejected"
//   * the workspaces INSERT happened (the row exists; otherwise the URL
//     UPDATE would not even be reachable — the test is exercising the
//     after-INSERT validation, not the pre-INSERT refusal)
//   * the URL UPDATE for the metadata URL was NOT issued (sqlmock fails
//     the test if any unmatched UPDATE with a metadata URL is posted).
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

	// 1. INSERT workspaces (RETURNING id) — the row IS created; the URL
	// check fires AFTER the INSERT. The test exercises the post-INSERT
	// defense; pre-INSERT refusal is covered by the workspace.go tests.
	mock.ExpectQuery(`INSERT INTO workspaces`).
		WithArgs(
			sqlmock.AnyArg(), // id
			"Bad Agent",      // name
			sqlmock.AnyArg(), // role
			sqlmock.AnyArg(), // tier
			sqlmock.AnyArg(), // runtime
			"provisioning",   // status
			sqlmock.AnyArg(), // parent_id
			sqlmock.AnyArg(), // workspace_dir
			sqlmock.AnyArg(), // workspace_access
			sqlmock.AnyArg(), // max_concurrent_tasks
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uuid.New().String()))

	// 2. INSERT canvas_layouts (for the canvas placement).
	mock.ExpectExec(`INSERT INTO canvas_layouts`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	// 3. INSERT structure_events (RecordAndBroadcast for EventWorkspaceProvisioning).
	mock.ExpectExec(`INSERT INTO structure_events`).
		WillReturnResult(sqlmock.NewResult(1, 1))

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

	// The URL UPDATE for the metadata URL must NOT have been issued.
	// sqlmock.ExpectationsWereMet() fails the test if any expected exec
	// wasn't called AND if any unexpected call was logged. The latter
	// is what catches an UPDATE for the metadata URL — we never set up
	// that expectation, so any UPDATE call would be flagged.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestCreateWorkspaceTree_RejectsLoopbackURL is the loopback analogue of
// TestCreateWorkspaceTree_RejectsMetadataURL. Loopback is blocked in
// self-hosted mode (the default for the org_import test) — a malicious
// org template pointing an external workspace at 127.0.0.1 must be
// rejected by the same defense-in-depth gate.
//
// 127.0.0.1 is a metadata-class target (same class as the AWS / GCP /
// Azure IMDS endpoints — an attacker reaching the loopback interface of
// the host running the platform can hit services that listen on
// 127.0.0.1 only, including any debug/admin endpoints a developer left
// open during development).
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

	mock.ExpectQuery(`INSERT INTO workspaces`).
		WithArgs(
			sqlmock.AnyArg(),
			"Bad Loopback",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"provisioning",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uuid.New().String()))

	mock.ExpectExec(`INSERT INTO canvas_layouts`).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO structure_events`).
		WillReturnResult(sqlmock.NewResult(1, 1))

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

// errSentinel is referenced in package tests that need a stable error
// for sqlmock expectation error returns. Declared here to keep the SSRF
// test self-contained.
var errSentinel = errors.New("sentinel")

// errSentinelTest aliases the package-level test helper when present
// (some test files already declare their own; this keeps the SSRF file
// importable in isolation).
var _ = errSentinelTest

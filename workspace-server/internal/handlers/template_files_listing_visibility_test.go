package handlers

// Files-API listing visibility + containment (core#4341).
//
// An operator listing a live molecules-server tenant's /configs saw
//
//	path=''        -> [{"path":"config.yaml",...}]
//	path='plugins' -> []
//
// and concluded the workspace had no plugins. The container in fact had 8.
// The listing was served from the docker-less HOST-SIDE MIRROR, which only
// ever carries the CP-delivered template bundle (config.yaml + prompts/*) —
// `plugins/` is created by the runtime INSIDE the container and is not in the
// mirror at all. The handler reported that as `200 []`, which is
// indistinguishable from "this directory exists and is empty".
//
// These tests pin the three behaviours that make that failure legible:
//   - directory entries ARE emitted (dir:true) — proving there is no dir filter
//   - an ABSENT subpath is 404, a genuinely EMPTY one is 200 [] (the fix)
//   - an absolute ?path= is rejected with a message naming the root
//
// plus the containment guarantees the traversal fix must not regress.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// listEntry is the Files-API wire shape.
type listEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Dir  bool   `json:"dir"`
}

// doListFiles runs ListFiles against a handler with the given query string.
func doListFiles(t *testing.T, h *TemplatesHandler, wsID, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/files?"+query, nil)
	h.ListFiles(c)
	return w
}

// decodeList decodes a 200 listing body, failing the test when the body is not
// a JSON array. Callers MUST check emptiness themselves before asserting
// membership — an assertion over an empty slice passes vacuously, and a
// vacuous pass is exactly the bug under repair here.
func decodeList(t *testing.T, w *httptest.ResponseRecorder) []listEntry {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got []listEntry
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad JSON: %v (body=%s)", err, w.Body.String())
	}
	return got
}

func findEntry(entries []listEntry, relPath string) (listEntry, bool) {
	want := filepath.FromSlash(relPath)
	for _, e := range entries {
		if e.Path == want || e.Path == relPath {
			return e, true
		}
	}
	return listEntry{}, false
}

// ---------------------------------------------------------------------------
// Defect 1 — directory entries must appear in a listing
// ---------------------------------------------------------------------------

// TestListFiles_Mirror_EmitsDirectoryEntries: a directory that exists in the
// served root appears in the listing with dir:true. The pre-existing mirror
// test only asserted the nested FILE (prompts/concierge.md) was present, never
// the `prompts` DIRECTORY row, so a dir-dropping regression would not have
// been caught by it.
func TestListFiles_Mirror_EmitsDirectoryEntries(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	const wsID = "ws-dir-visible"
	expectWorkspaceRow(mock, wsID, "Agent", "mol-ws-dv", "openclaw")

	base := t.TempDir()
	seedHostSideMirror(t, base, wsID, map[string]string{
		"config.yaml":          hostSideRealConfig,
		"prompts/concierge.md": "# persona",
		"skills/web/SKILL.md":  "# skill",
	})

	w := doListFiles(t, &TemplatesHandler{hostStateDir: base}, wsID, "root=/configs&depth=1")
	got := decodeList(t, w)
	if len(got) == 0 {
		t.Fatalf("listing is EMPTY — membership assertions below would pass vacuously; body=%s", w.Body.String())
	}

	for _, wantDir := range []string{"prompts", "skills"} {
		e, ok := findEntry(got, wantDir)
		if !ok {
			t.Errorf("directory %q missing from listing; got %s", wantDir, w.Body.String())
			continue
		}
		if !e.Dir {
			t.Errorf("entry %q must have dir:true, got dir:false", wantDir)
		}
	}
	if e, ok := findEntry(got, "config.yaml"); !ok || e.Dir {
		t.Errorf("config.yaml must appear as a FILE (dir:false); got %+v ok=%v", e, ok)
	}
}

// ---------------------------------------------------------------------------
// Defect 2 — subdirectory traversal, and absent vs empty
// ---------------------------------------------------------------------------

// TestListFiles_Subdir_ReturnsChildren: ?path=<subdir> lists that subdir's
// entries, addressed RELATIVE to the subdir.
func TestListFiles_Subdir_ReturnsChildren(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	const wsID = "ws-subdir"
	expectWorkspaceRow(mock, wsID, "Agent", "mol-ws-sd", "openclaw")

	base := t.TempDir()
	seedHostSideMirror(t, base, wsID, map[string]string{
		"config.yaml":          hostSideRealConfig,
		"prompts/concierge.md": "# persona",
		"prompts/greeting.md":  "# hello",
	})

	w := doListFiles(t, &TemplatesHandler{hostStateDir: base}, wsID, "root=/configs&path=prompts&depth=1")
	got := decodeList(t, w)
	if len(got) == 0 {
		t.Fatalf("subdir listing is EMPTY — this is the reported defect; body=%s", w.Body.String())
	}
	for _, want := range []string{"concierge.md", "greeting.md"} {
		if _, ok := findEntry(got, want); !ok {
			t.Errorf("expected %q in prompts/ listing; got %s", want, w.Body.String())
		}
	}
}

// TestListFiles_NestedSubdir_Resolves: a nested ?path=a/b resolves.
func TestListFiles_NestedSubdir_Resolves(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	const wsID = "ws-nested"
	expectWorkspaceRow(mock, wsID, "Agent", "mol-ws-n", "openclaw")

	base := t.TempDir()
	seedHostSideMirror(t, base, wsID, map[string]string{
		"config.yaml":         hostSideRealConfig,
		"skills/web/SKILL.md": "# skill",
	})

	w := doListFiles(t, &TemplatesHandler{hostStateDir: base}, wsID, "root=/configs&path=skills/web&depth=1")
	got := decodeList(t, w)
	if len(got) == 0 {
		t.Fatalf("nested subdir listing is EMPTY; body=%s", w.Body.String())
	}
	if _, ok := findEntry(got, "SKILL.md"); !ok {
		t.Errorf("expected SKILL.md in skills/web listing; got %s", w.Body.String())
	}
}

// TestListFiles_AbsentSubdir_Is404_NotEmptyList is THE regression test for the
// operator's wrong conclusion. The mirror is present and serves config.yaml,
// but carries no `plugins` dir. Pre-fix the handler answered `200 []`, which
// reads as "the directory is empty". It must instead say the path was not
// found, and name what was searched.
func TestListFiles_AbsentSubdir_Is404_NotEmptyList(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	const wsID = "ws-absent-subdir"
	expectWorkspaceRow(mock, wsID, "Agent", "mol-ws-as", "openclaw")

	base := t.TempDir()
	seedHostSideMirror(t, base, wsID, map[string]string{"config.yaml": hostSideRealConfig})

	w := doListFiles(t, &TemplatesHandler{hostStateDir: base}, wsID, "root=/configs&path=plugins&depth=1")

	if w.Code == http.StatusOK && strings.TrimSpace(w.Body.String()) == "[]" {
		t.Fatalf("REGRESSION: absent subpath returned `200 []`, indistinguishable from an empty directory")
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an absent subpath, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The message must name the root and the requested subpath so the caller
	// can tell "not found HERE" from "does not exist anywhere".
	for _, want := range []string{"/configs", "plugins"} {
		if !strings.Contains(body, want) {
			t.Errorf("404 body must name %q; got %s", want, body)
		}
	}
	// It must NOT leak the host-side absolute path of the mirror.
	if strings.Contains(body, base) {
		t.Errorf("404 body leaks host path %q: %s", base, body)
	}
}

// TestListFiles_AbsentSubdir_NoMirrorNoTemplate_Is404: same rule on the
// template-dir fallback branch, including when NO backend resolves at all.
// Pre-fix every one of these returned `200 []`.
func TestListFiles_AbsentSubdir_NoMirrorNoTemplate_Is404(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	const wsID = "ws-no-backend"
	expectWorkspaceRow(mock, wsID, "Unknown Agent", "", "")

	// tmpDir has no template matching "Unknown Agent", and no mirror is wired.
	h := NewTemplatesHandler(t.TempDir(), nil, nil)
	w := doListFiles(t, h, wsID, "root=/configs&path=plugins&depth=1")

	if w.Code == http.StatusOK && strings.TrimSpace(w.Body.String()) == "[]" {
		t.Fatalf("REGRESSION: absent subpath with no backend returned `200 []`")
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// TestListFiles_EmptySubdir_Is200EmptyList: the counterpart that keeps the 404
// honest. A directory that EXISTS and is EMPTY must still be `200 []` — if
// this and the test above ever agree, the distinction has been lost again.
func TestListFiles_EmptySubdir_Is200EmptyList(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	const wsID = "ws-empty-subdir"
	expectWorkspaceRow(mock, wsID, "Agent", "mol-ws-es", "openclaw")

	base := t.TempDir()
	mirror := seedHostSideMirror(t, base, wsID, map[string]string{"config.yaml": hostSideRealConfig})
	if err := os.MkdirAll(filepath.Join(mirror, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}

	w := doListFiles(t, &TemplatesHandler{hostStateDir: base}, wsID, "root=/configs&path=emptydir&depth=1")
	got := decodeList(t, w)
	if len(got) != 0 {
		t.Fatalf("expected an empty listing for a genuinely empty dir, got %s", w.Body.String())
	}
}

// TestListFiles_EmptyRoot_StillReturns200: a ROOT listing with no backend
// stays `200 []` — that contract is relied on by the canvas root load and by
// TestListFiles_FallbackToHost_NoTemplate. Only an explicit ?path= 404s.
func TestListFiles_EmptyRoot_StillReturns200(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	const wsID = "ws-empty-root"
	expectWorkspaceRow(mock, wsID, "Unknown Agent", "", "")

	h := NewTemplatesHandler(t.TempDir(), nil, nil)
	w := doListFiles(t, h, wsID, "root=/configs")
	got := decodeList(t, w)
	if len(got) != 0 {
		t.Fatalf("expected empty root listing, got %s", w.Body.String())
	}
}

// TestListFiles_SubPathIsFile_IsNotEmptyList: ?path=config.yaml names a FILE.
// Walking a file yields zero entries, so pre-fix this was a third flavour of
// the same false negative — `200 []`, reading as "that directory is empty".
func TestListFiles_SubPathIsFile_IsNotEmptyList(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	const wsID = "ws-path-is-file"
	expectWorkspaceRow(mock, wsID, "Agent", "mol-ws-pf", "openclaw")

	base := t.TempDir()
	seedHostSideMirror(t, base, wsID, map[string]string{"config.yaml": hostSideRealConfig})

	w := doListFiles(t, &TemplatesHandler{hostStateDir: base}, wsID, "root=/configs&path=config.yaml")

	if w.Code == http.StatusOK && strings.TrimSpace(w.Body.String()) == "[]" {
		t.Fatalf("REGRESSION: ?path= naming a file returned `200 []`")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a file ?path=, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "is a file") {
		t.Errorf("error should say the path is a file; got %s", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Defect 3 — absolute ?path= is rejected with an actionable message
// ---------------------------------------------------------------------------

// TestListFiles_AbsolutePath_MessageNamesRootAndRelativeForm: the decision is
// to KEEP rejecting absolute ?path= (see handler comment), but the bare
// "invalid path" told the caller nothing — in particular not that the API is
// already rooted at /configs, so `/configs/plugins` is a doubled root.
func TestListFiles_AbsolutePath_MessageNamesRootAndRelativeForm(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	const wsID = "ws-abs"
	expectWorkspaceRow(mock, wsID, "Agent", "mol-ws-abs", "openclaw")

	base := t.TempDir()
	seedHostSideMirror(t, base, wsID, map[string]string{"config.yaml": hostSideRealConfig})

	w := doListFiles(t, &TemplatesHandler{hostStateDir: base}, wsID, "root=/configs&path=/configs")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an absolute path, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.TrimSpace(body) == `{"error":"invalid path"}` {
		t.Fatalf("REGRESSION: opaque `invalid path` tells the caller nothing")
	}
	// Must name the root it is already rooted at, and say what form is wanted.
	if !strings.Contains(body, "/configs") {
		t.Errorf("error must name the root /configs; got %s", body)
	}
	if !strings.Contains(strings.ToLower(body), "relative") {
		t.Errorf("error must say the path is expected RELATIVE to the root; got %s", body)
	}
}

// ---------------------------------------------------------------------------
// CONTAINMENT — must not regress while fixing traversal
// ---------------------------------------------------------------------------

// TestListFiles_Containment_RejectsDotDot: `..` escape stays rejected, and is
// rejected as a BAD REQUEST (400), never silently downgraded to the new 404
// (which would make an escape attempt look like an ordinary missing path).
func TestListFiles_Containment_RejectsDotDot(t *testing.T) {
	setupTestRedis(t)
	base := t.TempDir()
	const wsID = "ws-dotdot"

	for _, bad := range []string{
		"../../etc",
		"..",
		"prompts/../../..",
		"prompts/../../../etc/passwd",
		"./../../etc",
	} {
		t.Run(bad, func(t *testing.T) {
			mock := setupTestDB(t)
			expectWorkspaceRow(mock, wsID, "Agent", "mol-ws-dd", "openclaw")
			seedHostSideMirror(t, base, wsID, map[string]string{"config.yaml": hostSideRealConfig})

			w := doListFiles(t, &TemplatesHandler{hostStateDir: base}, wsID, "root=/configs&path="+bad)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("traversal %q must be 400, got %d: %s", bad, w.Code, w.Body.String())
			}
		})
	}
}

// TestListFiles_Containment_RejectsAbsoluteEscape: an absolute path pointing
// clean outside the root is rejected, not resolved.
func TestListFiles_Containment_RejectsAbsoluteEscape(t *testing.T) {
	setupTestRedis(t)
	base := t.TempDir()
	const wsID = "ws-absesc"

	for _, bad := range []string{"/etc", "/etc/passwd", `C:\Windows`} {
		t.Run(bad, func(t *testing.T) {
			mock := setupTestDB(t)
			expectWorkspaceRow(mock, wsID, "Agent", "mol-ws-ae", "openclaw")
			seedHostSideMirror(t, base, wsID, map[string]string{"config.yaml": hostSideRealConfig})

			w := doListFiles(t, &TemplatesHandler{hostStateDir: base}, wsID, "root=/configs&path="+bad)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("absolute escape %q must be 400, got %d: %s", bad, w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "root:x:") {
				t.Fatalf("LEAK: response contains /etc/passwd content")
			}
		})
	}
}

// TestListFiles_Containment_SymlinkedSubdirNotFollowed is the escape the
// traversal fix could most easily introduce. `?path=` is validated as a
// relative path, so a symlink INSIDE the root whose target is outside passes
// validation lexically; the walk root then resolves through it and the walker
// lists the TARGET's contents. (The pre-existing OFFSEC-010 skip only drops
// symlink ENTRIES encountered during the walk — it does not stop the walk ROOT
// itself from being a symlink.) Template dirs are fetched from template repos,
// and git preserves symlinks, so this is reachable.
func TestListFiles_Containment_SymlinkedSubdirNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	mock := setupTestDB(t)
	setupTestRedis(t)
	const wsID = "ws-symdir"
	expectWorkspaceRow(mock, wsID, "Agent", "mol-ws-sy", "openclaw")

	base := t.TempDir()
	mirror := seedHostSideMirror(t, base, wsID, map[string]string{"config.yaml": hostSideRealConfig})

	// A directory OUTSIDE the mirror holding a file that must never be listed.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "stolen.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(mirror, "escape")); err != nil {
		t.Fatal(err)
	}

	w := doListFiles(t, &TemplatesHandler{hostStateDir: base}, wsID, "root=/configs&path=escape&depth=2")

	if strings.Contains(w.Body.String(), "stolen.txt") {
		t.Fatalf("SYMLINK ESCAPE: listing served content outside the root: %s", w.Body.String())
	}
	if w.Code == http.StatusOK {
		var got []listEntry
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if len(got) != 0 {
			t.Fatalf("SYMLINK ESCAPE: expected no entries through a symlinked walk root, got %s", w.Body.String())
		}
	}
}

// TestListFiles_Containment_SymlinkedNestedComponent: the same escape via an
// INTERMEDIATE component (?path=escape/sub), which a check that only Lstat's
// the final element would miss.
func TestListFiles_Containment_SymlinkedNestedComponent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	mock := setupTestDB(t)
	setupTestRedis(t)
	const wsID = "ws-symnest"
	expectWorkspaceRow(mock, wsID, "Agent", "mol-ws-sn", "openclaw")

	base := t.TempDir()
	mirror := seedHostSideMirror(t, base, wsID, map[string]string{"config.yaml": hostSideRealConfig})

	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "sub", "stolen.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(mirror, "escape")); err != nil {
		t.Fatal(err)
	}

	w := doListFiles(t, &TemplatesHandler{hostStateDir: base}, wsID, "root=/configs&path=escape/sub&depth=2")
	if strings.Contains(w.Body.String(), "stolen.txt") {
		t.Fatalf("SYMLINK ESCAPE via intermediate component: %s", w.Body.String())
	}
}

// TestListFiles_HermesPrivateState_StillDenied: `.hermes` holds the rendered
// .env (plaintext provider keys) and the API_SERVER_KEY. Hiding it is a
// deliberate POLICY choice (ssrf.go validateRelPath), unlike the directory
// omission — it must survive the traversal fix.
func TestListFiles_HermesPrivateState_StillDenied(t *testing.T) {
	setupTestRedis(t)
	base := t.TempDir()
	const wsID = "ws-hermes"

	for _, bad := range []string{".hermes", ".hermes/sessions"} {
		t.Run(bad, func(t *testing.T) {
			mock := setupTestDB(t)
			expectWorkspaceRow(mock, wsID, "Agent", "mol-ws-h", "openclaw")
			mirror := seedHostSideMirror(t, base, wsID, map[string]string{"config.yaml": hostSideRealConfig})
			if err := os.MkdirAll(filepath.Join(mirror, ".hermes", "sessions"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(mirror, ".hermes", "sessions", "k.env"), []byte("API_SERVER_KEY=x"), 0o600); err != nil {
				t.Fatal(err)
			}

			w := doListFiles(t, &TemplatesHandler{hostStateDir: base}, wsID, "root=/configs&path="+bad)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%q must stay denied with 400, got %d: %s", bad, w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "API_SERVER_KEY") {
				t.Fatalf("LEAK: %s", w.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Provenance — which backend answered
// ---------------------------------------------------------------------------

// TestListFiles_ProvenanceHeader_NamesBackend: the root cause of the operator's
// wrong conclusion is that a listing served from the PARTIAL host-side mirror
// is wire-identical to one served from the live container. The response now
// names the backend that answered so a caller can tell a complete listing from
// a config-bundle-only one.
func TestListFiles_ProvenanceHeader_NamesBackend(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	const wsID = "ws-prov"
	expectWorkspaceRow(mock, wsID, "Agent", "mol-ws-p", "openclaw")

	base := t.TempDir()
	seedHostSideMirror(t, base, wsID, map[string]string{"config.yaml": hostSideRealConfig})

	w := doListFiles(t, &TemplatesHandler{hostStateDir: base}, wsID, "root=/configs")
	decodeList(t, w)
	if got := w.Header().Get(filesSourceHeader); got != filesSourceHostMirror {
		t.Fatalf("expected %s: %q, got %q", filesSourceHeader, filesSourceHostMirror, got)
	}
}

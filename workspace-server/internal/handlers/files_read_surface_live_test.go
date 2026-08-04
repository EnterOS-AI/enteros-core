package handlers

// The READ leg of the Files API, proven against a REAL running container.
//
// Why this file exists
// --------------------
// plugin_settings_writer_live_test.go proves a WRITE lands on the box. Nothing
// proved the converse: that a LISTING describes the box. On 2026-08-02 an
// operator called `GET /workspaces/<id>/files`, got `config.yaml` back with a
// size that matched the container byte-for-byte — and got `[]` for
// `?path=plugins` while `/configs/plugins` held 8 real entries. They concluded
// "the files API cannot see hermes working directories, so no filesystem check
// exists" and moved to external artifacts only.
//
// The API reported ABSENCE when it meant COULD-NOT-LOOK, and nothing in CI
// noticed. That is the same class as TestLiveBox_MissingManifestIsAbsentNotUnreachable
// (below in the writer file): a read surface asserting a clean state it had not
// verified. The manifest read got a live proof; the LISTING never did.
//
// So this suite drives the REAL gin handler (TemplatesHandler.ListFiles) against
// a REAL container whose contents we seeded and can independently read back, and
// asserts POSITIVELY — named entries, a byte size cross-checked against the
// container itself — rather than "no error" or "the response is a list". Both of
// those were TRUE throughout the incident.
//
// Anti-vacuity contract (this repo has shipped several guards that covered
// nothing — a lint whose regex had decayed to literal 0x08 bytes, a paths-filter
// that emits a literal "No-op pass" SUCCESS):
//
//  1. EVERY listing assertion hard-fails on an EMPTY result BEFORE it checks
//     membership. `[]` is the exact payload the incident produced, and a
//     membership loop over an empty slice passes silently.
//  2. Each test counts the comparisons it actually executed and fails on zero,
//     mirroring the `--- PASS:` floor handlers-postgres-integration.yml already
//     enforces at the workflow level.
//  3. Ground truth is read out of the container with a SEPARATE mechanism
//     (`docker exec stat` / `ls`), never from the handler's own answer, and is
//     ALSO checked against the literal bytes we wrote — so a `stat` that
//     returned 0 could not make both sides agree at zero.
//
// Skipped unless MOLECULE_LIVE_DOCKER=1 and a daemon is reachable, and wired
// into the `Run live-container tests` step of
// .gitea/workflows/handlers-postgres-integration.yml, which SETS that variable
// and treats a SKIP as a failure. Run locally with:
//
//	MOLECULE_LIVE_DOCKER=1 go test ./internal/handlers/ -run LiveBox -v

import (
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// listedEntry mirrors the Files-API wire shape (the anonymous `fileEntry`
// declared inside ListFiles). Decoding into a typed struct rather than
// map[string]any is deliberate: a renamed json tag becomes a zero value here
// and fails an assertion, instead of silently missing from a map lookup.
type listedEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Dir  bool   `json:"dir"`
}

// wireMockDBForListFiles stands up the two queries ListFiles issues on the
// local-docker leg: its own workspaces lookup (backend selection) and
// findContainer's name lookup.
//
// instance_id carries the container NAME, not an `i-*` EC2 id — that is what a
// molecules-server (local-docker) workspace persists, and it MUST route to the
// docker-exec path rather than the AWS-only EIC tunnel (isEC2InstanceID).
func wireMockDBForListFiles(t *testing.T, workspaceID string) {
	t.Helper()
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "instance_id", "runtime"}).
			AddRow("proof-ws", "ws-"+workspaceID, "openclaw"))
	mock.ExpectQuery(`SELECT LOWER\(REPLACE\(name`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("proof-ws"))
}

// callListFiles drives the REAL handler — not listFilesViaEIC, not the find
// shell — so the assertions cover backend dispatch, query parsing, and the
// JSON projection, which is where the incident actually lived.
func callListFiles(t *testing.T, h *TemplatesHandler, wsID, rawQuery string) (int, []byte) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/files?"+rawQuery, nil)
	h.ListFiles(c)
	return w.Code, w.Body.Bytes()
}

// listOrFatalOnEmpty decodes a 200 listing and FAILS if it is empty.
//
// This is the load-bearing guard of the whole file. `200 []` is precisely what
// the incident returned for a directory holding 8 entries, and every
// "is X in the list" loop below would pass vacuously over an empty slice. So
// emptiness is rejected here, once, before any membership check runs.
func listOrFatalOnEmpty(t *testing.T, code int, body []byte, what string) []listedEntry {
	t.Helper()
	if code != 200 {
		t.Fatalf("%s: expected 200, got %d: %s", what, code, body)
	}
	var entries []listedEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("%s: response is not a file listing: %v\n%s", what, err, body)
	}
	if len(entries) == 0 {
		t.Fatalf("%s: the Files API returned an EMPTY listing for a directory this test "+
			"just populated on the box. That is the false-negative class this "+
			"suite exists to catch: the API reported ABSENCE when it meant COULD-NOT-LOOK. "+
			"Check the docker-exec leg in TemplatesHandler.ListFiles (templates.go) and the "+
			"host-side-mirror fallback below it.", what)
	}
	return entries
}

// find returns the entry with the given path, or nil.
func (e entries) find(p string) *listedEntry {
	for i := range e {
		if e[i].Path == p {
			return &e[i]
		}
	}
	return nil
}

type entries []listedEntry

// boxStatSize reads a file's size out of the container with a mechanism
// INDEPENDENT of the handler under test. If ground truth came from the same
// code path as the answer, the comparison would be a tautology.
func boxStatSize(t *testing.T, box, absPath string) int64 {
	t.Helper()
	out, err := exec.Command("docker", "exec", box, "stat", "-c", "%s", absPath).CombinedOutput()
	if err != nil {
		t.Fatalf("stat %s on %s: %v: %s", absPath, box, err, out)
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if perr != nil {
		t.Fatalf("stat %s returned %q, not a size: %v", absPath, out, perr)
	}
	return n
}

// seedReadSurfaceBox puts a KNOWN tree on the box:
//
//	/configs/config.yaml            file, 18 bytes
//	/configs/plugins/               dir  — 3 entries, the shape that returned []
//	/configs/plugins/alpha/plugin.yaml
//	/configs/plugins/beta/plugin.yaml
//	/configs/plugins/registry.json
//	/configs/prompts/system.md      file in a second dir
//
// The plugin tree is not incidental: `?path=plugins` returning [] against a
// populated /configs/plugins is the literal incident.
func seedReadSurfaceBox(t *testing.T, box string) {
	t.Helper()
	script := strings.Join([]string{
		`set -e`,
		`mkdir -p /configs/plugins/alpha /configs/plugins/beta /configs/prompts`,
		`printf 'runtime: openclaw\n' > /configs/config.yaml`,
		`printf 'name: alpha\n' > /configs/plugins/alpha/plugin.yaml`,
		`printf 'name: beta\n' > /configs/plugins/beta/plugin.yaml`,
		`printf '{"installed":2}\n' > /configs/plugins/registry.json`,
		`printf '# system\n' > /configs/prompts/system.md`,
	}, " && ")
	if out, err := exec.Command("docker", "exec", box, "sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("seed the box: %v: %s", err, out)
	}
}

// TestLiveBox_FilesAPIListingSeesDirectoriesOnTheBox is the direct regression
// guard for the reported defect's first half: the root listing OMITTED every
// directory, so an operator reading it concluded the workspace had no plugin
// tree at all.
//
// Asserts positively — the two directories we created must both be present AND
// carry dir:true. A response that merely "contains config.yaml" (which the
// incident's did) fails here.
func TestLiveBox_FilesAPIListingSeesDirectoriesOnTheBox(t *testing.T) {
	cli := liveDockerOrSkip(t)
	box := startLiveBox(t)
	seedReadSurfaceBox(t, box)
	wireMockDBForListFiles(t, liveWorkspaceID)
	h := &TemplatesHandler{docker: cli}

	code, body := callListFiles(t, h, liveWorkspaceID, "")
	got := entries(listOrFatalOnEmpty(t, code, body, "root listing of /configs"))

	checks := 0
	for _, dir := range []string{"plugins", "prompts"} {
		e := got.find(dir)
		if e == nil {
			t.Errorf("root listing OMITTED the directory %q — this is the reported defect: "+
				"the API described a workspace as having no %s tree while the container had one. "+
				"Listing was: %s", dir, dir, body)
			continue
		}
		checks++
		if !e.Dir {
			t.Errorf("%q was listed with dir:false — the canvas cannot expand it, so its "+
				"contents are unreachable and the tree reads as empty: %+v", dir, *e)
		}
		checks++
	}
	if e := got.find("config.yaml"); e == nil {
		t.Errorf("root listing lost config.yaml entirely: %s", body)
	} else {
		checks++
		if e.Dir {
			t.Errorf("config.yaml was listed as a directory: %+v", *e)
		}
		checks++
	}

	// Anti-vacuity: a rename or a wire-shape change that made every lookup
	// return nil would leave the loops above silent. Require that comparisons
	// actually ran.
	if checks == 0 {
		t.Fatal("ZERO assertions executed against the listing — every expected entry was " +
			"missing, so this test proved nothing. Do not weaken the expectations; fix the listing.")
	}
	t.Logf("root listing of a real container: %d entries, %d assertions executed", len(got), checks)
}

// TestLiveBox_FilesAPISubdirectoryListingIsNotEmpty is the second half of the
// defect: `?path=plugins` returned [] while /configs/plugins held 8 entries.
//
// A subdirectory listing must describe THAT subdirectory. The empty-result
// hard-fail in listOrFatalOnEmpty is what makes this test bite — without it a
// membership loop over [] would pass.
func TestLiveBox_FilesAPISubdirectoryListingIsNotEmpty(t *testing.T) {
	cli := liveDockerOrSkip(t)
	box := startLiveBox(t)
	seedReadSurfaceBox(t, box)
	wireMockDBForListFiles(t, liveWorkspaceID)
	h := &TemplatesHandler{docker: cli}

	code, body := callListFiles(t, h, liveWorkspaceID, "path=plugins")
	got := entries(listOrFatalOnEmpty(t, code, body, "?path=plugins"))

	// Independent ground truth: ask the container what is in there.
	out, err := exec.Command("docker", "exec", box, "ls", "/configs/plugins").CombinedOutput()
	if err != nil {
		t.Fatalf("ls /configs/plugins on the box: %v: %s", err, out)
	}
	onBox := strings.Fields(string(out))
	if len(onBox) == 0 {
		t.Fatal("the box itself reports /configs/plugins as empty — the seed did not take, " +
			"so a matching empty API answer would be a FALSE PASS")
	}

	checks := 0
	for _, name := range onBox {
		e := got.find(name)
		if e == nil {
			t.Errorf("?path=plugins omitted %q, which `ls` finds on the box. The API is "+
				"describing something other than the workspace filesystem. API said: %s", name, body)
			continue
		}
		checks++
	}
	if e := got.find("registry.json"); e != nil {
		checks++
		if e.Dir {
			t.Errorf("registry.json listed as a directory: %+v", *e)
		}
	}
	if e := got.find("alpha"); e != nil {
		checks++
		if !e.Dir {
			t.Errorf("the alpha plugin directory was listed with dir:false: %+v", *e)
		}
	}
	if checks == 0 {
		t.Fatal("ZERO assertions executed against ?path=plugins — nothing the box reports " +
			"was found in the API answer, so this test proved nothing.")
	}
	t.Logf("?path=plugins: box has %v, API returned %d entries, %d assertions executed",
		onBox, len(got), checks)
}

// TestLiveBox_FilesAPIReportedSizeMatchesTheContainer pins the one signal the
// operator DID trust during the incident — the byte size — to the container
// itself rather than to the handler's own bookkeeping.
//
// Cross-checked twice: against `stat` inside the container, and against the
// literal bytes this test wrote. A `stat` that returned 0 (or a listing that
// hard-coded 0, which is exactly what the find shell emits for directories)
// cannot satisfy both.
func TestLiveBox_FilesAPIReportedSizeMatchesTheContainer(t *testing.T) {
	cli := liveDockerOrSkip(t)
	box := startLiveBox(t)
	seedReadSurfaceBox(t, box)
	wireMockDBForListFiles(t, liveWorkspaceID)
	h := &TemplatesHandler{docker: cli}

	const wrote = "runtime: openclaw\n" // 18 bytes
	wantLiteral := int64(len(wrote))
	wantStat := boxStatSize(t, box, "/configs/config.yaml")
	if wantStat != wantLiteral {
		t.Fatalf("the box disagrees with what this test wrote (stat=%d, literal=%d) — "+
			"ground truth is broken, so any comparison below would be meaningless",
			wantStat, wantLiteral)
	}
	if wantStat == 0 {
		t.Fatal("ground-truth size is 0; a listing that reports 0 for everything would " +
			"pass vacuously")
	}

	code, body := callListFiles(t, h, liveWorkspaceID, "")
	got := entries(listOrFatalOnEmpty(t, code, body, "root listing for size check"))

	e := got.find("config.yaml")
	if e == nil {
		t.Fatalf("config.yaml absent from the listing, so its size cannot be checked: %s", body)
	}
	checks := 0
	if e.Size != wantStat {
		t.Errorf("size mismatch: API says %d, `stat` inside the container says %d", e.Size, wantStat)
	}
	checks++
	if e.Size != wantLiteral {
		t.Errorf("size mismatch: API says %d, this test wrote %d bytes", e.Size, wantLiteral)
	}
	checks++
	if checks != 2 {
		t.Fatalf("expected 2 size comparisons, executed %d", checks)
	}
	t.Logf("config.yaml: API %d B == stat %d B == %d B written", e.Size, wantStat, wantLiteral)
}

// TestLiveBox_FilesAPIContainmentHoldsOnARunningBox proves the listing cannot be
// walked out of its root — on a REAL filesystem, where a symlink is a real
// symlink rather than a test fixture's idea of one.
//
// The containment cases and the "must see real content" cases belong in the same
// suite on purpose: a handler that answered [] for EVERYTHING would satisfy every
// containment assertion here while being catastrophically broken, and the
// positive tests above are what forbid that resolution.
func TestLiveBox_FilesAPIContainmentHoldsOnARunningBox(t *testing.T) {
	cli := liveDockerOrSkip(t)
	box := startLiveBox(t)
	seedReadSurfaceBox(t, box)

	// A real symlink out of /configs, plus a sentinel outside it.
	if out, err := exec.Command("docker", "exec", box, "sh", "-c",
		`printf 'SENTINEL-MUST-NOT-BE-LISTED\n' > /etc/molecule-escape-sentinel && `+
			`ln -sfn /etc /configs/escape-dir`).CombinedOutput(); err != nil {
		t.Fatalf("stage the escape fixtures: %v: %s", err, out)
	}

	checks := 0

	// (a) Traversal and absolute paths must be REFUSED outright.
	for _, bad := range []string{"..", "../..", "plugins/../../etc", "../../../etc/shadow"} {
		wireMockDBForListFiles(t, liveWorkspaceID)
		h := &TemplatesHandler{docker: cli}
		code, body := callListFiles(t, h, liveWorkspaceID, "path="+bad)
		if code != 400 {
			t.Errorf("?path=%s was not refused: %d %s", bad, code, body)
		}
		checks++
	}

	// (b) A root outside the allow-list must be refused. `/etc` is not a
	// listable root no matter how the sub-path is spelled.
	{
		wireMockDBForListFiles(t, liveWorkspaceID)
		h := &TemplatesHandler{docker: cli}
		code, body := callListFiles(t, h, liveWorkspaceID, "root=/etc")
		if code != 400 {
			t.Errorf("?root=/etc was not refused: %d %s", code, body)
		}
		checks++
	}

	// (c) A symlink pointing out of /configs must not be TRAVERSED. Asserted on
	// content, not on status: the property that matters is that /etc's contents
	// never reach the caller, however the handler chooses to say so.
	{
		wireMockDBForListFiles(t, liveWorkspaceID)
		h := &TemplatesHandler{docker: cli}
		code, body := callListFiles(t, h, liveWorkspaceID, "path=escape-dir&depth=2")
		if code == 200 {
			var leaked []listedEntry
			_ = json.Unmarshal(body, &leaked)
			for _, e := range leaked {
				if strings.Contains(e.Path, "molecule-escape-sentinel") || e.Path == "passwd" {
					t.Errorf("listing followed a symlink out of /configs and exposed %q: %s", e.Path, body)
				}
			}
		}
		if strings.Contains(string(body), "molecule-escape-sentinel") {
			t.Errorf("the sentinel outside /configs reached the caller: %s", body)
		}
		checks++
	}

	// (d) An absolute sub-path must not resolve to the host root. Tolerant of
	// HOW it is refused (400 on Linux; on a Windows dev box filepath.IsAbs
	// disagrees about "/etc") but NOT of a leak.
	{
		wireMockDBForListFiles(t, liveWorkspaceID)
		h := &TemplatesHandler{docker: cli}
		code, body := callListFiles(t, h, liveWorkspaceID, "path=/etc")
		if code == 200 && strings.Contains(string(body), "molecule-escape-sentinel") {
			t.Errorf("?path=/etc escaped the configs root: %s", body)
		}
		checks++
	}

	if checks != 7 {
		t.Fatalf("containment suite executed %d assertions, expected 7 — a case was skipped, "+
			"which is how a containment guard silently stops guarding", checks)
	}
	t.Logf("containment: %d escape attempts, all contained", checks)
}

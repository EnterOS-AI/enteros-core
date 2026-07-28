package handlers

// The proof M4 hinges on: a settings file CHANGES ON A RUNNING BOX.
//
// Four previous attempts at layer 6 failed by designing storage and an API
// around this and never executing it. So this test does not mock the write —
// it starts a real container, writes through the real copyFilesToContainer
// path, and reads the bytes back out of the running container's filesystem.
//
// Skipped unless MOLECULE_LIVE_DOCKER=1 and a Docker daemon is reachable, so
// it never blocks CI on a machine without a socket. Run it with:
//
//   MOLECULE_LIVE_DOCKER=1 go test ./internal/handlers/ -run LiveBox -v

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/docker/docker/client"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

func liveDockerOrSkip(t *testing.T) *client.Client {
	t.Helper()
	if os.Getenv("MOLECULE_LIVE_DOCKER") != "1" {
		t.Skip("set MOLECULE_LIVE_DOCKER=1 to run the live-container writer proof")
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	if _, err := cli.Ping(context.Background()); err != nil {
		t.Skipf("no docker daemon: %v", err)
	}
	return cli
}

// startLiveBox runs a container that just sleeps, standing in for a workspace
// with a /configs tree. Returns its name; it is removed on cleanup.
func startLiveBox(t *testing.T) string {
	t.Helper()
	return startLiveBoxFor(t, liveWorkspaceID)
}

// liveWorkspaceID is the id the writer resolves; the container is named
// ws-<id> so the REAL findContainer finds it.
//
// It MUST be unique per CI run. This workflow's concurrency group is keyed on
// the head SHA, so two handlers-touching PRs run their jobs concurrently against
// ONE shared Docker daemon — the same reason Postgres is named
// pg-handlers-${RUN_ID}-${RUN_ATTEMPT}. With a fixed name, startLiveBoxFor's
// `docker rm -f` would evict the OTHER run's box mid-test; worse than a flaky
// red, the file-count assertions could then read a box that belongs to a
// different run and report a FALSE GREEN.
var liveWorkspaceID = func() string {
	// MOLECULE_LIVE_BOX_SUFFIX is set from ${RUN_ID}-${RUN_ATTEMPT} in CI and is
	// empty for a local run, where a stable name is convenient and there is no
	// concurrent peer to collide with.
	if sfx := strings.TrimSpace(os.Getenv("MOLECULE_LIVE_BOX_SUFFIX")); sfx != "" {
		return "settings-writer-proof-" + sfx
	}
	return "settings-writer-proof"
}()

func startLiveBoxFor(t *testing.T, workspaceID string) string {
	t.Helper()
	name := "ws-" + workspaceID
	_ = exec.Command("docker", "rm", "-f", name).Run()
	out, err := exec.Command("docker", "run", "-d", "--name", name,
		"alpine:3.20", "sh", "-c", "mkdir -p /configs/plugins && sleep 600").CombinedOutput()
	if err != nil {
		t.Skipf("could not start live box (%v): %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	// Wait for it to actually be running before writing into it.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name).Output()
		if strings.TrimSpace(string(st)) == "true" {
			return name
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("live box never reached running state")
	return ""
}

func readFromBox(t *testing.T, container, path string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", container, "cat", path).CombinedOutput()
	if err != nil {
		t.Fatalf("read %s from %s: %v: %s", path, container, err, out)
	}
	return string(out)
}

// TestLiveBox_SettingsFileLandsInARunningContainer is the load-bearing one.
func TestLiveBox_SettingsFileLandsInARunningContainer(t *testing.T) {
	cli := liveDockerOrSkip(t)
	box := startLiveBox(t)
	h := &TemplatesHandler{docker: cli}

	rel, err := pluginSettingsRelPath("molecule-ai-plugin-scheduler")
	if err != nil {
		t.Fatal(err)
	}
	body, err := renderPluginSettingsJSON(map[string]any{
		"poll_seconds": 15,
		"timezone":     "America/Vancouver",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The exact call the writer makes on the container-up path.
	if err := h.copyFilesToContainer(context.Background(), box, "/configs",
		map[string]string{rel: string(body)}); err != nil {
		t.Fatalf("copyFilesToContainer into a RUNNING container: %v", err)
	}

	got := readFromBox(t, box, "/configs/"+rel)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("what landed on the box is not valid JSON: %v\n%s", err, got)
	}
	if parsed["timezone"] != "America/Vancouver" {
		t.Errorf("timezone did not land: %v", parsed)
	}
	if parsed["poll_seconds"] != float64(15) {
		t.Errorf("poll_seconds did not land: %v", parsed)
	}
	t.Logf("wrote %s into a running container; box now reads: %s", rel, strings.TrimSpace(got))
}

// A settings CHANGE must overwrite, not append or duplicate — this is the
// property a live edit depends on, and tar-based copies can get it wrong.
func TestLiveBox_SecondWriteReplacesTheFirst(t *testing.T) {
	cli := liveDockerOrSkip(t)
	box := startLiveBox(t)
	h := &TemplatesHandler{docker: cli}
	rel, _ := pluginSettingsRelPath("molecule-ai-plugin-scheduler")

	for _, tz := range []string{"UTC", "Asia/Tokyo"} {
		body, err := renderPluginSettingsJSON(map[string]any{"timezone": tz})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.copyFilesToContainer(context.Background(), box, "/configs",
			map[string]string{rel: string(body)}); err != nil {
			t.Fatalf("write %s: %v", tz, err)
		}
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(readFromBox(t, box, "/configs/"+rel)), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["timezone"] != "Asia/Tokyo" {
		t.Errorf("second write did not replace the first: %v", parsed)
	}
	// Exactly one file — a duplicate would mean the runtime could read either.
	out, _ := exec.Command("docker", "exec", box, "sh", "-c",
		"ls /configs/"+pluginSettingsDirName+" | wc -l").Output()
	if strings.TrimSpace(string(out)) != "1" {
		t.Errorf("expected exactly one settings file, got %q", strings.TrimSpace(string(out)))
	}
}

// The writer must not be able to escape /configs via the install name — the
// name comes from a plugin source and is not fully trusted.
func TestLiveBox_UnsafeInstallNameNeverReachesTheBox(t *testing.T) {
	for _, bad := range []string{
		"../escape", "a/b", "..", ".", "", "/abs", "plugins/../../etc/passwd",
	} {
		if _, err := pluginSettingsRelPath(bad); err == nil {
			t.Errorf("install name %q was accepted — it must be refused before any write", bad)
		}
	}
	// ...and a legitimate name still resolves under the settings dir.
	got, err := pluginSettingsRelPath("molecule-ai-plugin-scheduler")
	if err != nil {
		t.Fatal(err)
	}
	if got != pluginSettingsDirName+"/molecule-ai-plugin-scheduler.json" {
		t.Errorf("unexpected path %q", got)
	}
}

// Byte-for-byte agreement with the provision-time renderer. If a live write and
// a re-provision produce different bytes for the same values, every
// re-provision churns the file and any content-hash provenance stamp with it.
func TestLiveBox_LiveWriteMatchesProvisionRenderByteForByte(t *testing.T) {
	settings := map[string]any{"poll_seconds": 15, "timezone": "America/Vancouver"}

	live, err := renderPluginSettingsJSON(settings)
	if err != nil {
		t.Fatal(err)
	}

	// The provision path renders from a template's config.yaml.
	rendered, err := renderPluginSettingsFiles([]byte(`
plugins:
  - source: gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0
    config:
      poll_seconds: 15
      timezone: America/Vancouver
`), "ws")
	if err != nil {
		t.Fatal(err)
	}
	provision, ok := rendered[pluginSettingsDirName+"/molecule-ai-plugin-scheduler.json"]
	if !ok {
		t.Fatalf("provision render produced nothing; got %v", settingsFileNames(rendered))
	}
	if string(live) != string(provision) {
		t.Errorf("live write and provision render disagree:\n live:     %q\n provision: %q", live, provision)
	}
}

// TestLiveBox_FilesAPISaveActuallyLandsOnTheBox is the regression guard for a
// LIVE bug this milestone uncovered.
//
// buildContainerFilesTar used to prefix every entry name with destPath. But
// CopyToContainer extracts the archive WITH destPath as its root, so
// `/configs/config.yaml` extracted at `/configs` landed at
// `/configs/configs/config.yaml` — while the call returned nil and the Files
// API answered {"status":"saved"}. On a running local-docker workspace, Canvas
// "Save" silently did nothing: the response was 200 and the file was untouched.
//
// The pre-existing unit test could not catch it because it asserted the TAR
// SHAPE (entries rooted under /configs/) and never the EXTRACTION RESULT — it
// pinned the bug as the contract. This one writes into a real container and
// reads the bytes back, which is the only assertion that distinguishes them.
func TestLiveBox_FilesAPISaveActuallyLandsOnTheBox(t *testing.T) {
	cli := liveDockerOrSkip(t)
	box := startLiveBox(t)
	h := &TemplatesHandler{docker: cli}

	// Seed a file, then overwrite it exactly as WriteFile's container leg does.
	if out, err := exec.Command("docker", "exec", box, "sh", "-c",
		`printf 'tier: 3\n' > /configs/config.yaml`).CombinedOutput(); err != nil {
		t.Fatalf("seed: %v: %s", err, out)
	}
	if err := h.copyFilesToContainer(context.Background(), box, "/configs",
		map[string]string{"config.yaml": "tier: 99\n"}); err != nil {
		t.Fatalf("copyFilesToContainer: %v", err)
	}

	if got := readFromBox(t, box, "/configs/config.yaml"); got != "tier: 99\n" {
		t.Errorf("the save did not take effect: /configs/config.yaml = %q", got)
	}
	// And nothing was written to the doubled path.
	out, _ := exec.Command("docker", "exec", box, "sh", "-c",
		"[ -e /configs/configs ] && echo EXISTS || echo absent").Output()
	if strings.TrimSpace(string(out)) != "absent" {
		t.Errorf("destPath was doubled — /configs/configs exists")
	}
}

// wireMockDBForWriter stands up the two queries the REAL writer path issues:
// its own workspaces lookup (backend selection) and findContainer's name
// lookup. Without this, exercising writePluginSettingsToWorkspace would only
// ever test copyFilesToContainer directly — which is what let the destPath
// doubling hide in the first place.
func wireMockDBForWriter(t *testing.T, workspaceID string) {
	t.Helper()
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	prev := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prev; mockDB.Close() })

	// Backend selection: a local-docker workspace carries a container NAME in
	// instance_id, which must NOT route to the EC2/EIC leg.
	mock.ExpectQuery(`SELECT COALESCE\(instance_id`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"instance_id", "runtime"}).
			AddRow("ws-"+workspaceID, "openclaw"))
	// findContainer's workspace-name lookup.
	mock.ExpectQuery(`SELECT LOWER\(REPLACE\(name`).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("proof-ws"))
}

// TestLiveBox_RealWriterLandsSettingsOnARunningWorkspace drives the ACTUAL
// entry point — backend dispatch included — not just the copy primitive.
func TestLiveBox_RealWriterLandsSettingsOnARunningWorkspace(t *testing.T) {
	cli := liveDockerOrSkip(t)
	box := startLiveBox(t)
	wireMockDBForWriter(t, liveWorkspaceID)
	h := &TemplatesHandler{docker: cli}

	if err := h.writePluginSettingsToWorkspace(context.Background(), liveWorkspaceID,
		"molecule-ai-plugin-scheduler",
		map[string]any{"poll_seconds": 15, "timezone": "America/Vancouver"}); err != nil {
		t.Fatalf("writePluginSettingsToWorkspace: %v", err)
	}

	got := readFromBox(t, box, "/configs/"+pluginSettingsDirName+"/molecule-ai-plugin-scheduler.json")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("not valid JSON on the box: %v\n%s", err, got)
	}
	if parsed["timezone"] != "America/Vancouver" || parsed["poll_seconds"] != float64(15) {
		t.Errorf("settings did not land: %v", parsed)
	}
	t.Logf("REAL writer put %s on a running workspace", strings.TrimSpace(got))
}

// deliverPluginSettings = write + best-effort daemon reload. The box here has
// no runtime serving /internal/daemons/reload, so the reload must NOT confirm —
// and the writer must still report the durable write as successful. That
// distinction is the point: "saved and live" is a stronger claim than "saved".
func TestLiveBox_DeliverReportsWhenTheReloadDidNotConfirm(t *testing.T) {
	cli := liveDockerOrSkip(t)
	box := startLiveBox(t)
	wireMockDBForWriter(t, liveWorkspaceID)
	// armSchedulerPlugin issues its own workspaces lookup for the callback URL.
	h := &TemplatesHandler{docker: cli}

	reloaded, err := h.deliverPluginSettings(context.Background(), liveWorkspaceID,
		"molecule-ai-plugin-scheduler", map[string]any{"timezone": "UTC"})
	if err != nil {
		t.Fatalf("deliverPluginSettings returned an error for a durable write: %v", err)
	}
	if reloaded {
		t.Error("reload should NOT have confirmed — this box runs no workspace runtime")
	}
	// The file is on the box regardless: the change applies on next boot.
	if got := readFromBox(t, box, "/configs/"+pluginSettingsDirName+"/molecule-ai-plugin-scheduler.json"); !strings.Contains(got, "UTC") {
		t.Errorf("durable write did not land: %q", got)
	}
}

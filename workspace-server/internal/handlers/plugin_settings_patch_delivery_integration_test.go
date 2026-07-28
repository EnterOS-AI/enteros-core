//go:build integration

package handlers

// B1 — THE PATCH DELIVERY PATH MUST NOT DESTROY TEMPLATE-SUPPLIED KEYS.
//
// The two-column split guarantees an override survives a re-provision. It says
// nothing about the LIVE EDIT path, and that is where the loss was:
//
//	writeTemplateConfig (the only writer of `config`) is called ONLY from
//	applyPluginSettingsLayers, which is gated on pluginSettingsLayersEnabled().
//	The flag ships OFF. So on the default configuration — and on every
//	workspace not yet re-provisioned after a flip — `config` is EMPTY.
//	PatchPluginSettings then computes effectiveSettings(config={}, overrides)
//	= the overrides ALONE and delivers that file WHOLESALE. Delivery is not
//	flag-gated. Every template-supplied key the operator did not name in the
//	PATCH is deleted from the box, and the API answers 200.
//
// These tests drive the REAL handler over the REAL DB and read the bytes that
// actually land on the workspace's /configs, through the docker-less host-side
// mirror leg of writePluginSettingsToWorkspace (the CP tenant shape).
//
// Each is paired with the reproduction of the unfixed behaviour, so a green
// here is evidence rather than an accident of the test's shape.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// patchDeliveryFixture stands up the docker-less CP shape: a real workspace row,
// a declared plugin so resolvePluginParam passes, and a host-side /configs
// mirror carrying the settings file the provision delivered.
type patchDeliveryFixture struct {
	ws      string
	handler *TemplatesHandler
	mirror  string
	rel     string
}

func newPatchDeliveryFixture(t *testing.T, delivered map[string]any) patchDeliveryFixture {
	t.Helper()
	conn := settingsTestDB(t)
	ws := seedSettingsWorkspace(t, conn)
	declarePlugin(t, conn, ws, testPlugin)

	prev := db.DB
	db.DB = conn
	t.Cleanup(func() { db.DB = prev })

	base := t.TempDir()
	h := &TemplatesHandler{hostStateDir: base} // docker == nil: the CP tenant shape
	mirror := provisioner.HostSideConfigsDir(base, ws)

	rel, err := pluginSettingsRelPath(testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	if delivered != nil {
		body, rerr := renderPluginSettingsJSON(delivered)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(mirror, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mirror, rel), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return patchDeliveryFixture{ws: ws, handler: h, mirror: mirror, rel: rel}
}

// patch drives the real gin handler and returns status + decoded body.
func (f patchDeliveryFixture) patch(t *testing.T, payload string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.PATCH("/workspaces/:id/plugin-settings/:plugin", f.handler.PatchPluginSettings)

	req := httptest.NewRequest(http.MethodPatch,
		"/workspaces/"+f.ws+"/plugin-settings/"+testPlugin, bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// onBox reads what the workspace's /configs now holds for this plugin.
func (f patchDeliveryFixture) onBox(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.mirror, f.rel))
	if err != nil {
		t.Fatalf("reading the delivered settings file back off the box: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("what landed on the box is not valid JSON: %v\n%s", err, raw)
	}
	return out
}

// THE ASSERTION. The default configuration (layers flag OFF, so `config` was
// never populated) must not lose a key the operator did not touch.
func TestIntegration_PluginSettings_PatchKeepsTemplateKeysWithConfigUnpopulated(t *testing.T) {
	t.Setenv("MOLECULE_PLUGIN_SETTINGS_LAYERS", "") // the SHIPPING default
	f := newPatchDeliveryFixture(t, map[string]any{
		"poll_seconds": 30,
		"timezone":     "UTC",
		"schedules":    []any{map[string]any{"name": "nightly"}},
	})

	code, body := f.patch(t, `{"set":{"poll_seconds":5}}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH returned %d: %v", code, body)
	}

	got := f.onBox(t)
	if got["poll_seconds"] != float64(5) {
		t.Errorf("the edit did not reach the box: poll_seconds = %v", got["poll_seconds"])
	}
	if got["timezone"] != "UTC" {
		t.Errorf("SILENT DATA LOSS: timezone was %q before the PATCH and is %v after — "+
			"the PATCH delivered the override layer wholesale over a file it never read. "+
			"full delivered file: %v", "UTC", got["timezone"], got)
	}
	scheds, ok := got["schedules"].([]any)
	if !ok || len(scheds) != 1 {
		t.Errorf("SILENT DATA LOSS: schedules was [{name:nightly}] before the PATCH and is %v after. "+
			"full delivered file: %v", got["schedules"], got)
	}
}

// ...and the DB now carries the provenance to explain it, so GET can answer
// "why is timezone UTC?" for a workspace that never ran with the flag on.
func TestIntegration_PluginSettings_PatchSeedsConfigProvenanceFromTheBox(t *testing.T) {
	t.Setenv("MOLECULE_PLUGIN_SETTINGS_LAYERS", "")
	f := newPatchDeliveryFixture(t, map[string]any{"poll_seconds": 30, "timezone": "UTC"})

	if code, body := f.patch(t, `{"set":{"poll_seconds":5}}`); code != http.StatusOK {
		t.Fatalf("PATCH returned %d: %v", code, body)
	}

	config, overrides, _, err := loadPluginSettings(context.Background(), db.DB, f.ws, testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := config["timezone"]; !ok {
		t.Fatalf("`config` was not seeded from the delivered file — the un-overridden "+
			"keys have no recorded layer and GET can say nothing about them: %v", config)
	}
	resolved := resolveSettings(config, overrides)
	if resolved["timezone"].Layer != layerTemplate {
		t.Errorf("timezone layer = %q, want %q", resolved["timezone"].Layer, layerTemplate)
	}
	if resolved["poll_seconds"].Layer != layerOverride {
		t.Errorf("poll_seconds layer = %q, want %q", resolved["poll_seconds"].Layer, layerOverride)
	}
	// The seed runs BEFORE the patch commits, so the file it reads is still pure
	// layers 2-5: the overridden key keeps a real template baseline and the UI
	// can offer "revert to template" without a second query.
	if resolved["poll_seconds"].OverriddenFrom != float64(30) {
		t.Errorf("overridden_from = %v, want the masked template value 30 recovered from the box",
			resolved["poll_seconds"].OverriddenFrom)
	}
}

// The repair case: a row created by a PRE-FIX PATCH already carries overrides
// with an empty config, so the file on the box holds the override's own value
// for those keys. Seeding them as "template" would fabricate provenance, so
// they are skipped while the untouched keys are still recovered.
func TestIntegration_PluginSettings_SeedDoesNotCreditOverriddenKeysToTheTemplate(t *testing.T) {
	t.Setenv("MOLECULE_PLUGIN_SETTINGS_LAYERS", "")
	f := newPatchDeliveryFixture(t, map[string]any{"poll_seconds": 5, "timezone": "UTC"})
	ctx := context.Background()

	// Stand up exactly the pre-fix residue: overrides set, config empty.
	if _, err := patchOverrides(ctx, db.DB, f.ws, testPlugin,
		map[string]any{"poll_seconds": 5}, nil, "pre-fix", -1); err != nil {
		t.Fatal(err)
	}
	config, _, _, err := loadPluginSettings(ctx, db.DB, f.ws, testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	if len(config) != 0 {
		t.Fatalf("fixture is wrong: config should still be empty, got %v", config)
	}

	if code, body := f.patch(t, `{"set":{"timezone":"Asia/Tokyo"}}`); code != http.StatusOK {
		t.Fatalf("PATCH returned %d: %v", code, body)
	}

	config, _, _, err = loadPluginSettings(ctx, db.DB, f.ws, testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	if _, lied := config["poll_seconds"]; lied {
		t.Errorf("seed invented a template baseline for a key that was already overridden: %v",
			config["poll_seconds"])
	}
	if config["timezone"].Value != "UTC" {
		t.Errorf("the un-overridden key should still be recovered from the box: %v", config["timezone"])
	}
	// ...and the override still reaches the box without losing the other key.
	got := f.onBox(t)
	if got["timezone"] != "Asia/Tokyo" || got["poll_seconds"] != float64(5) {
		t.Errorf("delivered file lost a key: %v", got)
	}
}

// The box is unreachable and `config` is empty: we do NOT know what the file
// holds, so a wholesale delivery could destroy it. The write is saved and
// delivery is SKIPPED, and the response says so — rather than shipping an
// override-only projection and calling it applied.
func TestIntegration_PluginSettings_PatchRefusesToDeliverWhenItCannotReadTheBox(t *testing.T) {
	t.Setenv("MOLECULE_PLUGIN_SETTINGS_LAYERS", "")
	conn := settingsTestDB(t)
	ws := seedSettingsWorkspace(t, conn)
	declarePlugin(t, conn, ws, testPlugin)
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() { db.DB = prev })

	// hostStateDir empty AND docker nil → no backend at all.
	f := patchDeliveryFixture{ws: ws, handler: &TemplatesHandler{}}
	code, body := f.patch(t, `{"set":{"poll_seconds":5}}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH returned %d: %v", code, body)
	}
	if body["applied"] != false {
		t.Errorf("an undeliverable edit must not report applied=true: %v", body)
	}
	if body["status"] != "saved" {
		t.Errorf("the edit is still durable; status = %v", body["status"])
	}
	// The response must name the REASON. Without this assertion the test passes
	// on the pre-fix code too — there the delivery merely failed for its own
	// reasons, which is not the guard being exercised.
	detail, _ := body["detail"].(string)
	if !strings.Contains(detail, "could not be read") {
		t.Errorf("the response must say the delivery was skipped because the current file could "+
			"not be read, not merely that something failed: detail = %q", detail)
	}
	// The durable half landed.
	_, overrides, _, err := loadPluginSettings(context.Background(), conn, ws, testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := overrides["poll_seconds"]; !ok {
		t.Errorf("the override was not saved: %v", overrides)
	}
}

// No settings file on the box at all: there is nothing to lose, so the override
// is delivered normally. This is the control that keeps the guard above from
// being an unconditional "never deliver".
func TestIntegration_PluginSettings_PatchStillDeliversWhenTheBoxHasNoFileYet(t *testing.T) {
	t.Setenv("MOLECULE_PLUGIN_SETTINGS_LAYERS", "")
	f := newPatchDeliveryFixture(t, nil) // mirror dir exists, file absent
	if err := os.MkdirAll(f.mirror, 0o755); err != nil {
		t.Fatal(err)
	}

	if code, body := f.patch(t, `{"set":{"poll_seconds":5}}`); code != http.StatusOK {
		t.Fatalf("PATCH returned %d: %v", code, body)
	}
	got := f.onBox(t)
	if got["poll_seconds"] != float64(5) {
		t.Errorf("first-ever settings file was not delivered: %v", got)
	}
}

// An oversized value must be refused BEFORE it is committed. The 64 KiB cap
// lives in renderPluginSettingsJSON, which the pre-fix handler reached only
// AFTER patchOverrides had already committed — so the row persisted a value
// that could never be delivered or overlaid again, behind a 200.
func TestIntegration_PluginSettings_OversizedValueIsRefusedBeforeTheCommit(t *testing.T) {
	t.Setenv("MOLECULE_PLUGIN_SETTINGS_LAYERS", "")
	f := newPatchDeliveryFixture(t, map[string]any{"poll_seconds": 30})

	huge := make([]byte, maxPluginSettingsBytes+1024)
	for i := range huge {
		huge[i] = 'x'
	}
	payload, err := json.Marshal(map[string]any{"set": map[string]any{"blob": string(huge)}})
	if err != nil {
		t.Fatal(err)
	}

	code, body := f.patch(t, string(payload))
	if code != http.StatusBadRequest {
		t.Errorf("an over-cap value must be refused with 400, got %d: %v", code, body)
	}
	// ...and nothing was persisted.
	_, overrides, version, err := loadPluginSettings(context.Background(), db.DB, f.ws, testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	if _, stored := overrides["blob"]; stored {
		t.Errorf("the over-cap value was COMMITTED before the cap was checked — "+
			"it can never be delivered or overlaid again: version=%d", version)
	}
	// ...and the box still holds the template value, untouched.
	if got := f.onBox(t); got["poll_seconds"] != float64(30) {
		t.Errorf("a refused PATCH must not touch the box: %v", got)
	}
}

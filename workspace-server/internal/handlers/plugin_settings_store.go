package handlers

// Layer-6 storage: the two-column split that makes an operator edit survive a
// re-provision.
//
//   config     layers 2-5, derived from the template. Core REWRITES it wholesale
//              on every (re-)provision.
//   overrides  layer 6, the operator's live edit. Core NEVER writes it during
//              provisioning.
//
// Effective value = overrides[key] if present, else config[key]. Keeping both
// in one column is what sank the four previous attempts: the next provision
// re-derived the column from the template and the edit was gone.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path"
	"strings"
)

// settingLayer names where a value came from, in precedence order.
const (
	layerTemplate = "template" // layers 2-5, collapsed: core rewrites these together
	layerOverride = "override" // layer 6
)

// settingValue is one key's stored form. Provenance travels WITH the value on
// both columns — `config` needs it too, or a GET can explain only the
// overridden keys and has nothing to say about the rest, which is precisely
// what an operator wants to know.
type settingValue struct {
	Value any    `json:"value"`
	Layer string `json:"layer"`
	SetBy string `json:"set_by,omitempty"`
	// SetAt is a CONTENT HASH of the value, not a wall-clock time. A
	// re-provision that produces the same value must not churn it — otherwise
	// every provision looks like an edit and the field is worthless for
	// answering "when did this last actually change?".
	SetAt string `json:"set_at"`
}

// contentStamp is the stable identity of a value.
func contentStamp(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(fmt.Sprintf("%v", v))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func newSettingValue(v any, layer, setBy string) settingValue {
	return settingValue{Value: v, Layer: layer, SetBy: setBy, SetAt: contentStamp(v)}
}

type settingMap map[string]settingValue

// resolvedSetting is what a GET returns per key: the winning value and the
// layer that supplied it.
type resolvedSetting struct {
	Value any    `json:"value"`
	Layer string `json:"layer"`
	SetBy string `json:"set_by,omitempty"`
	SetAt string `json:"set_at,omitempty"`
	// OverriddenFrom is the template value an override is masking, so the UI
	// can offer "revert to template" without a second query.
	OverriddenFrom any `json:"overridden_from,omitempty"`
}

// resolveSettings applies the precedence rule and records where each winner
// came from. This is the single place the layering is expressed.
func resolveSettings(config, overrides settingMap) map[string]resolvedSetting {
	out := make(map[string]resolvedSetting, len(config)+len(overrides))
	for k, v := range config {
		out[k] = resolvedSetting{Value: v.Value, Layer: v.Layer, SetBy: v.SetBy, SetAt: v.SetAt}
	}
	for k, v := range overrides {
		r := resolvedSetting{Value: v.Value, Layer: layerOverride, SetBy: v.SetBy, SetAt: v.SetAt}
		if base, ok := config[k]; ok {
			r.OverriddenFrom = base.Value
		}
		out[k] = r
	}
	return out
}

// effectiveSettings flattens the resolved view to the plain map the writer
// delivers to the box.
func effectiveSettings(config, overrides settingMap) map[string]any {
	resolved := resolveSettings(config, overrides)
	out := make(map[string]any, len(resolved))
	for k, r := range resolved {
		out[k] = r.Value
	}
	return out
}

func scanSettingMap(raw []byte) (settingMap, error) {
	if len(raw) == 0 {
		return settingMap{}, nil
	}
	var m settingMap
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode plugin settings column: %w", err)
	}
	if m == nil {
		m = settingMap{}
	}
	return m, nil
}

// loadPluginSettings reads one plugin's row. A missing row is not an error —
// every workspace predates this table.
func loadPluginSettings(ctx context.Context, db *sql.DB, workspaceID, pluginName string) (config, overrides settingMap, version int64, err error) {
	var cfgRaw, ovrRaw []byte
	row := db.QueryRowContext(ctx,
		`SELECT config, overrides, overrides_version
		   FROM workspace_plugin_settings
		  WHERE workspace_id = $1 AND plugin_name = $2`,
		workspaceID, pluginName)
	switch err = row.Scan(&cfgRaw, &ovrRaw, &version); {
	case err == sql.ErrNoRows:
		return settingMap{}, settingMap{}, 0, nil
	case err != nil:
		return nil, nil, 0, fmt.Errorf("load plugin settings for %s/%s: %w", workspaceID, pluginName, err)
	}
	if config, err = scanSettingMap(cfgRaw); err != nil {
		return nil, nil, 0, err
	}
	if overrides, err = scanSettingMap(ovrRaw); err != nil {
		return nil, nil, 0, err
	}
	return config, overrides, version, nil
}

// writeTemplateConfig replaces the `config` column for one plugin — the
// (re-)provision path.
//
// THE LOAD-BEARING PROPERTY: it never touches `overrides` or
// `overrides_version`. An operator edit survives every re-provision precisely
// because this statement cannot reach it.
func writeTemplateConfig(ctx context.Context, db *sql.DB, workspaceID, pluginName string, values map[string]any) error {
	cfg := make(settingMap, len(values))
	for k, v := range values {
		cfg[k] = newSettingValue(v, layerTemplate, "template")
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode template config: %w", err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO workspace_plugin_settings (workspace_id, plugin_name, config)
		      VALUES ($1, $2, $3::jsonb)
		 ON CONFLICT (workspace_id, plugin_name)
		 DO UPDATE SET config = EXCLUDED.config, updated_at = NOW()`,
		workspaceID, pluginName, raw)
	if err != nil {
		return fmt.Errorf("write template config for %s/%s: %w", workspaceID, pluginName, err)
	}
	return nil
}

// errOverridesVersionConflict signals a lost-update race: the caller edited a
// version that is no longer current.
var errOverridesVersionConflict = fmt.Errorf("overrides_version conflict")

// patchOverrides merges keys into the `overrides` column under compare-and-set.
//
// A nil value DELETES that key's override, reverting it to the template layer —
// distinct from setting it to JSON null, which is a real value an operator may
// legitimately want.
//
// expectedVersion < 0 skips the check (first-write / admin path). Otherwise a
// mismatch returns errOverridesVersionConflict and nothing is written, so two
// concurrent operators cannot silently clobber one another.
func patchOverrides(
	ctx context.Context, db *sql.DB, workspaceID, pluginName string,
	patch map[string]any, deletes []string, setBy string, expectedVersion int64,
) (newVersion int64, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin overrides patch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var ovrRaw []byte
	var current int64
	row := tx.QueryRowContext(ctx,
		`SELECT overrides, overrides_version
		   FROM workspace_plugin_settings
		  WHERE workspace_id = $1 AND plugin_name = $2
		    FOR UPDATE`,
		workspaceID, pluginName)
	switch err = row.Scan(&ovrRaw, &current); {
	case err == sql.ErrNoRows:
		ovrRaw, current = nil, 0
	case err != nil:
		return 0, fmt.Errorf("read overrides for %s/%s: %w", workspaceID, pluginName, err)
	}
	if expectedVersion >= 0 && expectedVersion != current {
		return current, errOverridesVersionConflict
	}

	overrides, err := scanSettingMap(ovrRaw)
	if err != nil {
		return 0, err
	}
	for k, v := range patch {
		overrides[k] = newSettingValue(v, layerOverride, setBy)
	}
	for _, k := range deletes {
		delete(overrides, k)
	}

	raw, err := json.Marshal(overrides)
	if err != nil {
		return 0, fmt.Errorf("encode overrides: %w", err)
	}
	newVersion = current + 1
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO workspace_plugin_settings (workspace_id, plugin_name, overrides, overrides_version)
		      VALUES ($1, $2, $3::jsonb, $4)
		 ON CONFLICT (workspace_id, plugin_name)
		 DO UPDATE SET overrides = EXCLUDED.overrides,
		               overrides_version = EXCLUDED.overrides_version,
		               updated_at = NOW()`,
		workspaceID, pluginName, raw, newVersion); err != nil {
		return 0, fmt.Errorf("write overrides for %s/%s: %w", workspaceID, pluginName, err)
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit overrides patch: %w", err)
	}
	return newVersion, nil
}

// applyPluginSettingsLayers is the provision-time seam between the rendered
// template settings and the stored layers.
//
// For each plugin-settings file the render produced it:
//  1. records the template values as the `config` layer (layers 2-5), and
//  2. re-renders the file from the EFFECTIVE settings, so any operator
//     override is present in the bytes the workspace actually receives.
//
// Step 2 is what makes "an edit survives a re-provision" true ON THE BOX and
// not merely in the database. Without it the DB would remember the override
// while the freshly provisioned workspace quietly ran the template value —
// which is the same silent-divergence failure this milestone keeps finding.
//
// Non-fatal by contract: every error is logged and skipped. A broken settings
// row must never block a workspace create.
func applyPluginSettingsLayers(
	ctx context.Context, database *sql.DB, workspaceID string, files map[string][]byte,
) (map[string][]byte, int) {
	if database == nil || len(files) == 0 {
		return files, 0
	}
	applied := 0
	for name, body := range files {
		pluginName, ok := pluginNameFromSettingsFile(name)
		if !ok {
			continue
		}
		var templateValues map[string]any
		if err := json.Unmarshal(body, &templateValues); err != nil {
			log.Printf("plugin settings layers: %s/%s is not decodable: %v (skipping)", workspaceID, pluginName, err)
			continue
		}
		if err := writeTemplateConfig(ctx, database, workspaceID, pluginName, templateValues); err != nil {
			log.Printf("plugin settings layers: %s/%s persist failed: %v (delivering template values)", workspaceID, pluginName, err)
			continue
		}
		config, overrides, _, err := loadPluginSettings(ctx, database, workspaceID, pluginName)
		if err != nil {
			log.Printf("plugin settings layers: %s/%s re-read failed: %v (delivering template values)", workspaceID, pluginName, err)
			continue
		}
		if len(overrides) == 0 {
			continue // nothing to overlay; the rendered bytes already stand
		}
		merged, err := renderPluginSettingsJSON(effectiveSettings(config, overrides))
		if err != nil {
			log.Printf("plugin settings layers: %s/%s re-render failed: %v (delivering template values)", workspaceID, pluginName, err)
			continue
		}
		files[name] = merged
		applied++
	}
	return files, applied
}

// pluginNameFromSettingsFile reverses pluginSettingsRelPath.
func pluginNameFromSettingsFile(name string) (string, bool) {
	dir, file := path.Split(name)
	if path.Clean(dir) != pluginSettingsDirName || !strings.HasSuffix(file, ".json") {
		return "", false
	}
	return strings.TrimSuffix(file, ".json"), true
}

// pluginSettingsLayersEnabled gates the provision-time overlay.
//
// DEFAULT OFF. This is new behaviour on the Create path — the single most
// blast-radius-sensitive code in the server — so it ships dark and is turned on
// deliberately, rather than changing every provision the moment it merges.
// With it off, provisioning is byte-identical to before: the rendered template
// settings are delivered exactly as they were.
//
// Turning it on is what makes an operator override survive a re-provision ON
// THE BOX (the DB half works either way).
//
// THE BISECT THIS FLAG WAS ALSO INTRODUCED FOR HAS CONCLUDED — and it cleared
// this code. template-delivery-e2e was red for a reason with no connection to
// the overlay: the seo-agent template declares runtime_config.required_env
// (TENANT_*), the harness tenant supplied none of them, so preflight #5 aborted
// the provision before cpProv.Start and the host-side /configs mirror was never
// written. The gate read 0B and reported a "delivery REGRESSION" that never
// happened. Fixed in tests/harness/template-asset-delivery-gate.sh; the tenant
// log naming the abort is quoted on core#4923.
//
// So what remains here is a shipping-posture choice, not an open question: the
// overlay is proven by TestIntegration_PluginSettings_ReprovisionDeliversThe
// OverrideToTheBox against a real Postgres, and it stays dark only because
// changing the Create path deserves a deliberate flip rather than an automatic
// one at merge. Until it is flipped, layer 6 survives in the DATABASE but a
// re-provisioned box still runs the template value — M4's done-condition is
// not met on the box with the flag off. Flipping it is owner-gated.
//
//	MOLECULE_PLUGIN_SETTINGS_LAYERS=1|true|yes   → on
//	unset / anything else                        → off
func pluginSettingsLayersEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MOLECULE_PLUGIN_SETTINGS_LAYERS"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

package handlers

// The layer-6 operator surface: read the resolved settings for one installed
// plugin, and patch the override layer.
//
// Deliberately NOT the Files API. `PUT /workspaces/:id/files/*path` can already
// put bytes in /configs, but its side effect is maybeRestartAfterFileWrite → a
// workspace RESTART. A settings edit wants the opposite: the file changes and
// the daemon re-reads it, with no restart and no lost session. So PATCH here
// writes the DB, delivers the effective file, and asks the runtime to reload.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

type patchPluginSettingsRequest struct {
	// Set carries the keys to override. A JSON null here is a real value the
	// operator chose; use Unset to REMOVE an override and fall back to the
	// template layer.
	Set   map[string]any `json:"set"`
	Unset []string       `json:"unset"`
	// Version is the overrides_version the caller read. Omit (or -1) to skip
	// the compare-and-set — accepted only because the very first edit has no
	// version to quote.
	Version *int64 `json:"version"`
}

// resolvePluginParam pins the request's `:plugin` to the workspace's real
// INSTALL name and writes the error response itself when it cannot.
//
// Every layer-6 route goes through it, so no handler can accidentally use the
// raw parameter: passing a plugin's MANIFEST name would otherwise address
// plugin-settings/<manifest-name>.json — a file nothing reads — and return 200.
// See plugin_settings_identity.go for why the two names diverge.
//
// Returns ok=false when a response has already been written.
func resolvePluginParam(c *gin.Context, workspaceID string) (string, bool) {
	requested := c.Param("plugin")
	if db.DB == nil {
		// Without the DB there is no install-name set to pin against. Say so —
		// a 404 here would read as "no such plugin", which is a different and
		// wrong answer. (GET/PATCH already check this; the declaration route
		// did not need a DB before the pin, so the check belongs here.)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database unavailable"})
		return "", false
	}
	resolved, err := resolvePluginInstallName(c.Request.Context(), db.DB, workspaceID, requested)
	if err == nil {
		return resolved, true
	}
	if errors.Is(err, errPluginNotOnWorkspace) {
		// Name the install names that WOULD have worked. A caller who passed
		// the manifest name is one response away from the right one, instead
		// of getting a silent success against a phantom file.
		known, lerr := workspacePluginInstallNames(c.Request.Context(), db.DB, workspaceID)
		if lerr != nil {
			log.Printf("resolvePluginParam %s/%s: listing install names: %v", workspaceID, requested, lerr)
		}
		c.JSON(http.StatusNotFound, gin.H{
			"error":     "plugin is not declared or installed on this workspace",
			"requested": requested,
			"hint": "plugin settings are keyed on the INSTALL name (the plugins/<name>/ directory " +
				"derived from the source repo), not the manifest's own `name:`",
			"install_names": sortedNames(known),
		})
		return "", false
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	return "", false
}

// GetPluginSettings handles GET /workspaces/:id/plugin-settings/:plugin.
//
// Returns every key with the layer that supplied it, so the plugin tab can
// answer "why is it this?" for un-overridden keys too — which is the reason
// provenance is stored on `config` and not only on `overrides`.
func (h *TemplatesHandler) GetPluginSettings(c *gin.Context) {
	workspaceID := c.Param("id")
	if db.DB == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database unavailable"})
		return
	}
	pluginName, ok := resolvePluginParam(c, workspaceID)
	if !ok {
		return
	}

	config, overrides, version, err := loadPluginSettings(c.Request.Context(), db.DB, workspaceID, pluginName)
	if err != nil {
		log.Printf("GetPluginSettings %s/%s: %v", workspaceID, pluginName, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load plugin settings"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"workspace_id": workspaceID,
		"plugin":       pluginName,
		"version":      version,
		"settings":     resolveSettings(config, overrides),
	})
}

// PatchPluginSettings handles PATCH /workspaces/:id/plugin-settings/:plugin.
//
// DB write → deliver the effective file → daemon reload. The response
// distinguishes "applied" from "saved, applies on next boot": conflating them
// is how an operator ends up believing a setting is live when it is not.
func (h *TemplatesHandler) PatchPluginSettings(c *gin.Context) {
	workspaceID := c.Param("id")
	if db.DB == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database unavailable"})
		return
	}
	// Resolve BEFORE reading the body: a write keyed on the manifest name must
	// be refused, not saved somewhere nothing reads. See plugin_settings_identity.go.
	pluginName, ok := resolvePluginParam(c, workspaceID)
	if !ok {
		return
	}

	var body patchPluginSettingsRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if len(body.Set) == 0 && len(body.Unset) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "nothing to change: provide `set` and/or `unset`"})
		return
	}

	expected := int64(-1)
	if body.Version != nil {
		expected = *body.Version
	}
	setBy := settingsActor(c)

	ctx := c.Request.Context()

	// SEED THE `config` LAYER FROM WHAT THE BOX IS ACTUALLY RUNNING, before
	// anything is computed from it.
	//
	// `config` is written only by writeTemplateConfig, which is reached only
	// through the flag-gated provision-time overlay. With the flag off — the
	// shipping default, and the state of every workspace not yet re-provisioned
	// after a flip — `config` is empty, so effectiveSettings() below would return
	// the overrides ALONE and the wholesale delivery would delete every
	// template-supplied key from the box behind a 200.
	//
	// The currently-delivered file IS the record of layers 2-5 for such a
	// workspace, so it is what we seed from. `haveBaseline` carries forward
	// whether the effective map can be trusted to be complete.
	haveBaseline, seedErr := h.seedConfigFromDeliveredSettings(ctx, workspaceID, pluginName)
	if seedErr != nil {
		log.Printf("PatchPluginSettings %s/%s: could not establish the delivered baseline: %v",
			workspaceID, pluginName, seedErr)
	}

	// VALIDATE BEFORE THE COMMIT. renderPluginSettingsJSON enforces the 64 KiB
	// per-plugin cap and JSON-encodability; reaching it only on the delivery leg
	// meant an over-cap value was already COMMITTED, answered 200 with
	// applied:false, and could never be delivered or overlaid again. Run the
	// exact same function over the prospective result first.
	if err := validatePatchPayload(ctx, workspaceID, pluginName, body.Set, body.Unset); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	newVersion, err := patchOverrides(ctx, db.DB, workspaceID, pluginName, body.Set, body.Unset, setBy, expected)
	if errors.Is(err, errOverridesVersionConflict) {
		// A lost update is refused, not silently applied. The current version
		// comes back so the caller can re-read and retry.
		c.JSON(http.StatusConflict, gin.H{
			"error":   "overrides_version conflict: another edit landed first",
			"version": newVersion,
		})
		return
	}
	if err != nil {
		log.Printf("PatchPluginSettings %s/%s: %v", workspaceID, pluginName, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save plugin settings"})
		return
	}

	// Deliver the EFFECTIVE settings — the same merge the box would get from a
	// fresh provision, so a live edit and a re-provision converge.
	config, overrides, _, err := loadPluginSettings(ctx, db.DB, workspaceID, pluginName)
	if err != nil {
		log.Printf("PatchPluginSettings %s/%s: saved but could not re-read: %v", workspaceID, pluginName, err)
		c.JSON(http.StatusOK, gin.H{
			"status": "saved", "applied": false, "version": newVersion,
			"detail": "settings saved; delivery skipped (re-read failed) — applies on next boot",
		})
		return
	}
	effective := effectiveSettings(config, overrides)

	// NEVER DELIVER AN OVERRIDE-ONLY PROJECTION OVER A FILE WE COULD NOT READ.
	// writePluginSettingsToWorkspace replaces the file WHOLESALE. If `config` is
	// still empty and the seed above could not establish what the box holds, the
	// effective map is not the full picture and delivering it would silently
	// destroy whatever template-supplied keys are on the box. The durable half is
	// already saved; say plainly that the live half was skipped and why.
	if len(config) == 0 && !haveBaseline {
		log.Printf("PatchPluginSettings %s/%s: saved but delivery SKIPPED — the currently delivered "+
			"settings could not be read, so a wholesale write could have destroyed template-supplied keys",
			workspaceID, pluginName)
		c.JSON(http.StatusOK, gin.H{
			"status": "saved", "applied": false, "version": newVersion,
			"detail": "settings saved; delivery skipped — the workspace's current plugin-settings file " +
				"could not be read, so writing the override layer over it could have destroyed " +
				"template-supplied keys. The change applies on the next provision.",
			"settings": resolveSettings(config, overrides),
		})
		return
	}

	reloaded, derr := h.deliverPluginSettings(ctx, workspaceID, pluginName, effective)
	if derr != nil {
		// The DB is the durable record; a delivery failure is recoverable on
		// the next boot, so this is not a 500 — but it must not read as "live".
		log.Printf("PatchPluginSettings %s/%s: saved but delivery failed: %v", workspaceID, pluginName, derr)
		c.JSON(http.StatusOK, gin.H{
			"status": "saved", "applied": false, "version": newVersion,
			"detail": "settings saved; could not write to the workspace — applies on next boot",
		})
		return
	}
	detail := "settings saved and delivered; the daemon will pick them up on next boot"
	if reloaded {
		detail = "settings saved, delivered, and reloaded into the running daemons"
	}
	c.JSON(http.StatusOK, gin.H{
		"status": "saved", "applied": reloaded, "version": newVersion,
		"settings": resolveSettings(config, overrides), "detail": detail,
	})
}

// seedConfigFromDeliveredSettings establishes the `config` (layers 2-5) baseline
// for a plugin whose row has none, by reading the settings file the workspace is
// CURRENTLY running.
//
// Returns haveBaseline: whether the effective map can now be trusted to be a
// complete picture of what the box should hold. It is true when a config layer
// already existed or was just seeded, and ALSO when the box has no settings file
// at all — there is nothing to lose in that case, so the first delivery is safe.
// It is false only when we could not determine what the box holds.
//
// Keys already carried by `overrides` are NOT seeded into `config`: the
// delivered file's value for an overridden key is the OVERRIDE's own value, so
// recording it as a template baseline would fabricate provenance. The cost is
// that such a key has no "overridden_from" to revert to, which is information
// that was never available anyway.
func (h *TemplatesHandler) seedConfigFromDeliveredSettings(
	ctx context.Context, workspaceID, pluginName string,
) (haveBaseline bool, err error) {
	config, overrides, _, lerr := loadPluginSettings(ctx, db.DB, workspaceID, pluginName)
	if lerr != nil {
		return false, lerr
	}
	if len(config) > 0 {
		return true, nil // a template layer is already recorded; nothing to do
	}

	rel, rerr := pluginSettingsRelPath(pluginName)
	if rerr != nil {
		return false, rerr
	}
	raw, readErr := h.readWorkspaceConfigFile(ctx, workspaceID, rel)
	if errors.Is(readErr, errWorkspaceFileAbsent) {
		// The box has no settings file for this plugin, so a wholesale write
		// cannot destroy anything. Safe to deliver; nothing to seed.
		return true, nil
	}
	if readErr != nil {
		return false, readErr
	}

	var delivered map[string]any
	if jerr := json.Unmarshal(raw, &delivered); jerr != nil {
		// A file we cannot parse is a file whose keys we do not know. Refuse to
		// claim a baseline rather than overwrite it blind.
		return false, fmt.Errorf("the delivered %s is not decodable JSON: %w", rel, jerr)
	}
	for k := range overrides {
		delete(delivered, k)
	}
	if len(delivered) == 0 {
		// Every delivered key is already overridden — nothing to record, and
		// nothing the effective map can lose.
		return true, nil
	}
	if _, serr := seedTemplateConfigIfEmpty(ctx, db.DB, workspaceID, pluginName, delivered); serr != nil {
		return false, serr
	}
	return true, nil
}

// validatePatchPayload runs the prospective post-patch effective settings
// through the EXACT function the delivery leg uses, so the 64 KiB per-plugin cap
// and the JSON-encodability requirement are enforced BEFORE the DB commit
// instead of after it.
func validatePatchPayload(
	ctx context.Context, workspaceID, pluginName string, set map[string]any, unset []string,
) error {
	config, overrides, _, err := loadPluginSettings(ctx, db.DB, workspaceID, pluginName)
	if err != nil {
		return nil // a load failure is not the caller's fault; patchOverrides reports it
	}
	prospective := effectiveSettings(config, overrides)
	for k, v := range set {
		prospective[k] = v
	}
	for _, k := range unset {
		if base, ok := config[k]; ok {
			prospective[k] = base.Value
		} else {
			delete(prospective, k)
		}
	}
	if _, err := renderPluginSettingsJSON(prospective); err != nil {
		return err
	}
	return nil
}

// settingsActor derives WHO made an edit from the credential the auth middleware
// actually verified.
//
// It previously read c.GetString("actor") — a key NOTHING in the server ever
// sets — so every override was attributed to the literal "operator" and the
// per-key provenance answered "who?" with a constant. These keys are all set by
// middleware.WorkspaceAuth, the only middleware these routes are mounted behind.
//
// Never a secret value: an org token contributes its PREFIX (the same
// non-sensitive identifier the audit log uses), never the token.
func settingsActor(c *gin.Context) string {
	if id := c.GetString("cp_session_user_id"); id != "" {
		return "user:" + id
	}
	if actor := c.GetString("cp_session_actor"); actor != "" {
		return actor // already of the form "session:<hash>"
	}
	if prefix := c.GetString("org_token_prefix"); prefix != "" {
		return "org-token:" + prefix
	}
	if c.GetBool("caller_is_admin_token") {
		return "admin-token"
	}
	if ws := c.GetString("authenticated_workspace_id"); ws != "" {
		return "workspace:" + ws
	}
	if class := c.GetString("caller_credential_class"); class != "" {
		return class
	}
	return "unknown"
}

// GetPluginDeclaration handles
// GET /workspaces/:id/plugin-settings/:plugin/declaration.
//
// Returns the plugin's OWN `contributes.configuration`, read live off the
// workspace, so the plugin tab can render a form for a plugin the frontend has
// never seen — the M5 done condition. `?format=example` returns the generated
// `.example` instead, as text.
func (h *TemplatesHandler) GetPluginDeclaration(c *gin.Context) {
	workspaceID := c.Param("id")
	// Same pin as GET/PATCH. readPluginManifestFromWorkspace addresses
	// /configs/plugins/<name>/ — the install directory — so the manifest name
	// would not find it either; resolving here makes that a named 404 instead
	// of an opaque "manifest not readable".
	pluginName, ok := resolvePluginParam(c, workspaceID)
	if !ok {
		return
	}

	manifest, err := h.readPluginManifestFromWorkspace(c.Request.Context(), workspaceID, pluginName)
	if err != nil {
		// THREE FAILURE MODES, THREE STATUSES. They previously all collapsed to
		// 404 "plugin manifest not readable", which told an operator that a
		// perfectly installed plugin did not exist whenever core simply had no
		// way to look at the box.
		switch {
		case errors.Is(err, errUnsafeInstallName):
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plugin name", "detail": err.Error()})
		case errors.Is(err, errWorkspaceFileAbsent):
			// A backend WAS reached and the manifest is not there: the plugin is
			// declared but not installed on the box. This is the honest 404.
			c.JSON(http.StatusNotFound, gin.H{
				"error":  "plugin is declared on this workspace but not installed on the box",
				"detail": err.Error(),
				"hint":   "the plugin installs post-online via the reconcile; retry once the workspace reports it installed",
			})
		case errors.Is(err, errWorkspaceReadFailed):
			c.JSON(http.StatusBadGateway, gin.H{
				"error":  "the workspace's /configs backend failed while reading the plugin manifest",
				"detail": err.Error(),
			})
		default: // errWorkspaceUnreachable and anything unclassified
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "cannot read the box: no reachable /configs backend for this workspace, " +
					"so whether the plugin is installed is unknown",
				"detail": err.Error(),
			})
		}
		return
	}
	decl, perr := parsePluginDeclaration(manifest)
	if perr != nil {
		// Tolerant by contract, matching the manifest schema's open anyOf: a
		// malformed block yields no form rather than making the plugin look
		// broken. Surfaced as a warning so an author can still see it.
		log.Printf("GetPluginDeclaration %s/%s: %v", workspaceID, pluginName, perr)
		c.JSON(http.StatusOK, gin.H{
			"workspace_id": workspaceID, "plugin": pluginName,
			"declaration": decl, "warning": "configuration block could not be parsed; no form rendered",
		})
		return
	}

	if c.Query("format") == "example" {
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.String(http.StatusOK, renderSettingsExample(pluginName, decl))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"workspace_id": workspaceID, "plugin": pluginName, "declaration": decl,
	})
}

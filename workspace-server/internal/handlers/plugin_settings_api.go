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
	"errors"
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
	setBy := c.GetString("actor")
	if setBy == "" {
		setBy = "operator"
	}

	ctx := c.Request.Context()
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
		// Not installed / box unreachable is a 404 for this resource, not a 500:
		// the tab asks about plugins the workspace may not have.
		c.JSON(http.StatusNotFound, gin.H{
			"error":  "plugin manifest not readable on this workspace",
			"detail": err.Error(),
		})
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

package handlers

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/wirepath"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"github.com/docker/docker/client"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// allowedRoots are the container paths that the Files API can browse.
//
// `/agent-home` (added 2026-05-15, internal#425 RFC) is the container's
// own $HOME — `/root` for openclaw, `/home/agent` for claude-code/hermes
// — browsed via `docker exec` rather than host-side `find`. The
// dispatch is stubbed today (returns 501); full implementation lands in
// Phase 2b of the RFC. The allowedRoots key is added now so the canvas
// can design its root-selector UI against the final shape and the
// stub-vs-full transition is server-side only.
var allowedRoots = map[string]bool{
	"/configs":    true,
	"/workspace":  true,
	"/home":       true,
	"/plugins":    true,
	"/agent-home": true,
}

// agentHomeStubMessage is the body returned by every Files API verb
// when `?root=/agent-home` is requested before Phase 2b lands. Keep the
// status code 501 (Not Implemented) — the route exists, the verb is
// understood, but the handler is unimplemented. Distinguishes from
// 400/404 so a canvas behind a less-current server can render a clean
// "feature pending" state instead of a generic error.
const agentHomeStubMessage = "/agent-home not implemented yet (internal#425 RFC Phase 2b — docker-exec backend pending)"

// isAgentHomeStubRequest returns true when the request targets the
// stubbed /agent-home root. Centralised so every verb in this file
// short-circuits with the same response shape.
func isAgentHomeStubRequest(rootPath string) bool {
	return rootPath == "/agent-home"
}

// maxUploadFiles limits the number of files in a single import/replace.
const maxUploadFiles = 200

type TemplatesHandler struct {
	configsDir string
	cacheDir   string
	docker     *client.Client
	// wh is used by Import and ReplaceFiles to call DefaultTier() so a
	// generated config.yaml's tier matches the SaaS-vs-self-hosted
	// boundary (#2910 PR-B). nil-tolerant — the field is unused when
	// the caller doesn't import templates that need a fresh config
	// generated.
	wh *WorkspaceHandler
	// refreshCache is nil unless main wires a manifest-backed template
	// cache refresher. POST /admin/templates/refresh uses this hook so a
	// template repo merge can update the tenant catalog without rebuilding
	// the full tenant image.
	refreshCache func(ctx *gin.Context) (any, error)
	// hostStateDir is the base dir of the per-workspace host-side /configs
	// mirror the CPProvisioner persists at (re)provision (#206 molecules-server:
	// the tenant has no docker.sock into the runtime container, so the Files API
	// cannot docker-exec into it — it serves reads from this mirror instead).
	// MUST be the SAME value handed to CPProvisioner.WithHostStateDir so writer
	// and reader never drift. Empty disables the mirror (reads fall through to
	// the legacy container/template-dir path). Resolved once in main via
	// provisioner.ResolveWorkspaceStateBaseDir.
	hostStateDir string
}

// NewTemplatesHandler constructs a TemplatesHandler. wh may be nil for
// callers that only use the read-only template surfaces (List,
// ReadFile, ListFiles). Import + ReplaceFiles need wh non-nil so the
// generated config.yaml picks the SaaS-aware default tier.
func NewTemplatesHandler(configsDir string, dockerCli *client.Client, wh *WorkspaceHandler) *TemplatesHandler {
	return &TemplatesHandler{configsDir: configsDir, docker: dockerCli, wh: wh}
}

func (h *TemplatesHandler) WithCacheDir(cacheDir string) *TemplatesHandler {
	h.cacheDir = cacheDir
	return h
}

func (h *TemplatesHandler) WithRefreshFunc(fn func(ctx *gin.Context) (any, error)) *TemplatesHandler {
	h.refreshCache = fn
	return h
}

// WithHostStateDir wires the base dir of the per-workspace host-side /configs
// mirror the Files API serves docker-less reads from (#206 molecules-server).
// MUST match the value handed to CPProvisioner.WithHostStateDir.
func (h *TemplatesHandler) WithHostStateDir(dir string) *TemplatesHandler {
	h.hostStateDir = dir
	return h
}

// hostSideConfigsRoot returns the host-side /configs mirror dir for workspaceID
// when the ?root= is /configs and the mirror feature is enabled, else "". The
// mirror only carries the /configs tree (config.yaml + prompts/* + secret config
// files) — the exact bundle the runtime container's /configs is provisioned
// from — so it is authoritative ONLY for root=/configs. Other roots (/workspace,
// /home, /plugins) have no host-side mirror and fall through to the legacy path.
func (h *TemplatesHandler) hostSideConfigsRoot(rootPath, workspaceID string) string {
	if h.hostStateDir == "" || rootPath != "/configs" {
		return ""
	}
	return provisioner.HostSideConfigsDir(h.hostStateDir, workspaceID)
}

// modelSpec describes a single supported model on a template: its id (sent
// to the runtime), a human-readable label, and the env vars that must be
// present for that model to work (e.g. API keys).
type modelSpec struct {
	ID          string   `json:"id" yaml:"id"`
	Name        string   `json:"name,omitempty" yaml:"name"`
	Provider    string   `json:"provider,omitempty" yaml:"provider"`
	RequiredEnv []string `json:"required_env,omitempty" yaml:"required_env"`
}

// registryProviderView is the canvas-facing projection of a single registry
// Provider entry for a registry-known runtime: the stable name, the dropdown
// display label, and the auth-env-var NAMES (never values). Sourced from the
// provider registry (internal/providers) so the canvas drops its hardcoded
// VENDOR_LABELS map (internal#718 P3, retire-list #4).
type registryProviderView struct {
	// Name is the registry provider key (e.g. "anthropic-oauth", "platform").
	Name string `json:"name"`
	// DisplayName is the canvas dropdown label (registry Provider.DisplayName).
	DisplayName string `json:"display_name,omitempty"`
	// AuthEnv is the env-var NAMES any one of which satisfies auth for this
	// provider (registry Provider.AuthEnv). Names only, never secret values.
	AuthEnv []string `json:"auth_env,omitempty"`
	// Deprecated mirrors the registry's deprecated flag so the canvas can
	// grey the provider out without breaking saved configs.
	Deprecated bool `json:"deprecated,omitempty"`
}

// providerRegistryEntry mirrors a row from a template's top-level
// `providers:` registry block (claude-code, hermes, etc.). Each entry
// fully describes one provider: its name, auth flow, the model id
// prefixes/aliases that route to it, an optional base_url override, and
// the env vars required to authenticate.
//
// This is the structured taxonomy the canvas's ProviderModelSelector
// comment anticipates ("Templates that ship explicit vendor metadata
// (future) should override the heuristic.") — surfacing it here lets
// the canvas drop its prefix-inference fallback for templates that ship
// an explicit registry. Templates without the block omit the field
// (omitempty); the canvas falls back to its current per-model
// required_env derivation.
type providerRegistryEntry struct {
	Name          string   `json:"name" yaml:"name"`
	AuthMode      string   `json:"auth_mode,omitempty" yaml:"auth_mode"`
	ModelPrefixes []string `json:"model_prefixes,omitempty" yaml:"model_prefixes"`
	ModelAliases  []string `json:"model_aliases,omitempty" yaml:"model_aliases"`
	BaseURL       string   `json:"base_url,omitempty" yaml:"base_url"`
	AuthEnv       []string `json:"auth_env,omitempty" yaml:"auth_env"`
}

type templateSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Tier        int    `json:"tier"`
	Runtime     string `json:"runtime"`
	// RuntimeDisplayName is the runtime's human label from the provider
	// registry (providers.yaml runtimes.<rt>.display_name) — the SSOT for the
	// onboarding scene's runtime-picker label, so it never has to borrow this
	// template's Name. Empty when the runtime is not registry-known; the
	// canvas falls back to the runtime slug.
	RuntimeDisplayName string      `json:"runtime_display_name,omitempty"`
	Model              string      `json:"model"`
	Models             []modelSpec `json:"models,omitempty"`
	// RequiredEnv mirrors runtime_config.required_env from the template's
	// config.yaml — the AND-required env vars the template declares at the
	// runtime level (separate from per-model required_env). The canvas
	// preflight uses this as the fallback provider when `models` is empty
	// so provider picker stays data-driven instead of hardcoded in the UI.
	RequiredEnv []string `json:"required_env,omitempty"`
	// RecommendedEnv mirrors runtime_config.recommended_env from the
	// template's config.yaml. Canvas prompts for these as non-blocking
	// optional secrets during template deploy.
	RecommendedEnv []string `json:"recommended_env,omitempty"`
	// Providers is the runtime's own list of supported provider slugs,
	// sourced from runtime_config.providers in the template's config.yaml.
	// The canvas Config tab surfaces this as the Provider override
	// dropdown (Option B PR-5). Data-driven so each runtime owns its own
	// taxonomy — hermes-agent supports 20+ providers; claude-code only
	// "anthropic" — and a future runtime with
	// a different vendor list doesn't need a canvas edit. Empty list →
	// canvas falls back to deriving suggestions from `models[].id` slug
	// prefixes (still adapter-driven, just inferred).
	Providers []string `json:"providers,omitempty"`
	// ProviderRegistry is the structured provider taxonomy from the
	// template's TOP-LEVEL `providers:` block (separate from the
	// runtime_config.providers slug list above). Each entry carries
	// auth_env / model_prefixes / model_aliases / base_url so the canvas
	// can render an authoritative Provider→Model cascade without
	// re-deriving vendor metadata from per-model required_env tuples.
	//
	// Closes #235 (server-side enrichment): the `Providers []string`
	// field shipped a name list but never the structured payload the
	// canvas's ProviderModelSelector comment block anticipates as the
	// override for its prefix-inference heuristic. Pre-existing
	// templates without the top-level block omit the field
	// (omitempty); the canvas's existing per-model fallback continues
	// to work for them.
	ProviderRegistry []providerRegistryEntry `json:"provider_registry,omitempty"`
	// RegistryBacked is true when this template's runtime is known to the
	// provider registry (internal/providers runtimes: block) and the
	// RegistryProviders / RegistryModels fields below were populated from it.
	// The canvas treats a registry-backed payload as AUTHORITATIVE for the
	// selectable provider+model list (it drops its prefix-inference fallback)
	// — "only registered selectable" follows because the canvas can render
	// no option the registry did not serve. False = the runtime is not in the
	// registry (federation / external / mock); the canvas keeps using the
	// template-served Models/Providers + its heuristic. internal#718 P3.
	RegistryBacked bool `json:"registry_backed,omitempty"`
	// RegistryProviders is the runtime's NATIVE provider set from the
	// registry (ProvidersForRuntime), each with its display label, auth-env
	// names, and billing mode. Empty when !RegistryBacked. This is the SSOT
	// the canvas Provider dropdown consumes instead of VENDOR_LABELS.
	RegistryProviders []registryProviderView `json:"registry_providers,omitempty"`
	// RegistryModels is the runtime's NATIVE model set from the registry
	// (ModelsForRuntime), each annotated with its DERIVED provider and the
	// billing mode that provider implies. Empty when !RegistryBacked. This is
	// the SSOT the canvas Model dropdown consumes — a template can no longer
	// surface a model the registry does not list for the runtime.
	RegistryModels []modelSpec `json:"registry_models,omitempty"`
	Skills         []string    `json:"skills"`
	SkillCount     int         `json:"skill_count"`
	// ProvisionTimeoutSeconds lets a slow runtime declare its expected
	// cold-boot duration in its template manifest. Canvas's
	// ProvisioningTimeout banner respects this per-workspace via the
	// `provision_timeout_ms` field in the workspace API response (#2054).
	// 0 = template hasn't declared one, falls through to canvas's
	// runtime-profile default.
	ProvisionTimeoutSeconds int `json:"provision_timeout_seconds,omitempty"`
	// Displayable lets a template opt OUT of the canvas runtime picker
	// declaratively (config.yaml `displayable: false`) while still being a
	// provisionable runtime. nil/absent or true → shown; only an explicit
	// false hides it. The canvas runtime dropdown is SSOT-driven off this
	// list (no hardcoded frontend allowlist), so this is the single place a
	// runtime is hidden from the picker. Pointer so "unset" is distinct from
	// "false" and omitempty keeps the payload unchanged for existing
	// templates that never declare it.
	Displayable *bool `json:"displayable,omitempty"`
}

// resolveTemplateDir finds the template directory for a workspace on the host.
// Only resolves to actual templates (not ws-* dirs since those are now Docker volumes).
// Returns empty string if no matching template is found.
func (h *TemplatesHandler) resolveTemplateDir(wsName string) string {
	if h.cacheDir != "" {
		nameDir := filepath.Join(h.cacheDir, normalizeName(wsName))
		if _, err := os.Stat(nameDir); err == nil {
			return nameDir
		}
		if tmpl := findTemplateByName(h.cacheDir, wsName); tmpl != "" {
			return filepath.Join(h.cacheDir, tmpl)
		}
	}
	nameDir := filepath.Join(h.configsDir, normalizeName(wsName))
	if _, err := os.Stat(nameDir); err == nil {
		return nameDir
	}
	// Search templates by config.yaml name field (e.g., org-pm has name: "PM")
	if tmpl := findTemplateByName(h.configsDir, wsName); tmpl != "" {
		return filepath.Join(h.configsDir, tmpl)
	}
	return ""
}

// List handles GET /templates
func (h *TemplatesHandler) List(c *gin.Context) {
	templates := make([]templateSummary, 0)
	seen := map[string]struct{}{}
	walk := func(root string) {
		if root == "" {
			return
		}
		walkTemplateConfigs(root, func(id string, data []byte) {
			if _, ok := seen[id]; ok {
				return
			}
			seen[id] = struct{}{}
			var raw struct {
				Name        string   `yaml:"name"`
				Description string   `yaml:"description"`
				Tier        int      `yaml:"tier"`
				Runtime     string   `yaml:"runtime"`
				Model       string   `yaml:"model"`
				Skills      []string `yaml:"skills"`
				Displayable *bool    `yaml:"displayable"`
				// Top-level `providers:` block — structured registry. Distinct
				// from runtime_config.providers (slug list) below. Both shapes
				// coexist in production: claude-code ships the structured
				// registry, hermes still uses the slug list. /templates surfaces
				// both verbatim so each runtime owns its taxonomy.
				Providers     []providerRegistryEntry `yaml:"providers"`
				RuntimeConfig struct {
					Model                   string      `yaml:"model"`
					Models                  []modelSpec `yaml:"models"`
					RequiredEnv             []string    `yaml:"required_env"`
					RecommendedEnv          []string    `yaml:"recommended_env"`
					Providers               []string    `yaml:"providers"`
					ProvisionTimeoutSeconds int         `yaml:"provision_timeout_seconds"`
				} `yaml:"runtime_config"`
			}
			if err := yaml.Unmarshal(data, &raw); err != nil {
				// Without this log a malformed config.yaml causes the
				// template to silently disappear from /templates with no
				// trace — the operator can't tell "excluded due to parse
				// error" from "never existed." That matters more now that
				// templates ship richer YAML shapes (top-level providers
				// registry, models[] with required_env, etc.) where a
				// type-shape mismatch on one field drops the whole entry.
				log.Printf("templates list: skip %s: yaml.Unmarshal: %v", id, err)
				return
			}
			// normalizedRuntime strips the "-default" vanilla-variant suffix
			// (claude-code-default → claude-code). Hoisted out of the
			// known-runtime guard so the registry enrichment below can key off
			// the same normalised name the guard validated.
			normalizedRuntime := strings.TrimSuffix(strings.TrimSpace(raw.Runtime), "-default")
			if raw.Runtime != "" {
				if _, ok := knownRuntimes[normalizedRuntime]; !ok {
					log.Printf("templates list: skip %s: unsupported runtime %q", id, raw.Runtime)
					return
				}
			}

			// Model comes from either top-level (legacy) or runtime_config.model (current).
			model := raw.Model
			if model == "" {
				model = raw.RuntimeConfig.Model
			}

			tier := raw.Tier
			if h.wh != nil && h.wh.IsSaaS() {
				tier = h.wh.DefaultTier()
			}

			summary := templateSummary{
				ID:                      id,
				Name:                    raw.Name,
				Description:             raw.Description,
				Tier:                    tier,
				Runtime:                 raw.Runtime,
				Model:                   model,
				Models:                  raw.RuntimeConfig.Models,
				RequiredEnv:             raw.RuntimeConfig.RequiredEnv,
				RecommendedEnv:          raw.RuntimeConfig.RecommendedEnv,
				Providers:               raw.RuntimeConfig.Providers,
				ProviderRegistry:        raw.Providers,
				Skills:                  raw.Skills,
				SkillCount:              len(raw.Skills),
				ProvisionTimeoutSeconds: raw.RuntimeConfig.ProvisionTimeoutSeconds,
				Displayable:             raw.Displayable,
			}

			// internal#718 P3: serve the SELECTABLE provider/model list from
			// the provider registry for a registry-known runtime. Additive —
			// the template-served Models/Providers above stay for non-registry
			// runtimes + older canvases; this adds the authoritative
			// registry_backed/registry_providers/registry_models block the
			// current canvas prefers. Fail-open for unknown runtimes.
			enrichFromRegistry(&summary, normalizedRuntime)

			templates = append(templates, summary)
		})
	}
	walk(h.cacheDir)
	walk(h.configsDir)

	c.JSON(http.StatusOK, templates)
}

// RefreshCache handles POST /admin/templates/refresh.
func (h *TemplatesHandler) RefreshCache(c *gin.Context) {
	if h.refreshCache == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "template cache refresh is not configured"})
		return
	}
	result, err := h.refreshCache(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

// ListFiles handles GET /workspaces/:id/files
// Lists files inside the running container's /configs directory (or /workspace, etc.).
// Falls back to host-side config templates directory when container isn't running.
func (h *TemplatesHandler) ListFiles(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	// Query params:
	//   ?root=  — base path in container (default: /configs)
	//   ?path=  — subdirectory to list (relative to root, default: "")
	//   ?depth= — max depth to recurse (default: 1, max: 5)
	rootPath := c.DefaultQuery("root", "/configs")
	if !allowedRoots[rootPath] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "root must be one of: /configs, /workspace, /home, /plugins, /agent-home"})
		return
	}
	// /agent-home dispatch is stubbed pre-Phase-2b. Short-circuit before
	// the DB lookup + EIC dance so a canvas exercising the new root key
	// gets a clean 501 instead of a half-effort response.
	if isAgentHomeStubRequest(rootPath) {
		c.JSON(http.StatusNotImplemented, gin.H{"error": agentHomeStubMessage})
		return
	}
	subPath := c.DefaultQuery("path", "")
	if subPath != "" {
		if err := validateRelPath(subPath); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
			return
		}
	}
	depth := 1
	if d := c.Query("depth"); d != "" {
		n, err := strconv.Atoi(d)
		if err != nil || n < 1 || n > 5 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "depth must be 1-5"})
			return
		}
		depth = n
	}
	listPath := rootPath
	if subPath != "" {
		listPath = rootPath + "/" + subPath
	}

	var wsName, instanceID, runtime string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT name, COALESCE(instance_id, ''), COALESCE(runtime, '') FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&wsName, &instanceID, &runtime); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	type fileEntry struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
		Dir  bool   `json:"dir"`
	}

	// SaaS workspace (EC2-per-workspace) — no Docker on this tenant. List
	// via SSH through the EIC endpoint, mirroring ReadFile/WriteFile's
	// dispatch. Pre-fix this branch was missing and SaaS workspaces
	// always fell through to local-Docker check (finds nothing on a SaaS
	// tenant) + template-dir fallback (returns the seed template, not
	// the persisted state, and almost never matches on user-named
	// workspaces). Net effect: the canvas Files tab always rendered "0
	// files / No config files yet" for SaaS workspaces, regardless of
	// what was actually on disk. See issue #2999.
	//
	// isEC2InstanceID gates this on a REAL EC2 id: a molecules-server
	// (local-docker) workspace persists its container NAME in instance_id,
	// which must route to the docker-exec path below, not the AWS-only EIC
	// tunnel. See files_backend_dispatch.go.
	if isEC2InstanceID(instanceID) {
		entries, err := listFilesViaEIC(ctx, instanceID, runtime, rootPath, subPath, depth)
		if err != nil {
			log.Printf("ListFiles EIC for %s root=%s sub=%s: %v", workspaceID, rootPath, subPath, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to list files: %v", err)})
			return
		}
		// Translate to the handler's wire shape (the field names match
		// 1:1, so we can use a direct type conversion).
		out := make([]fileEntry, 0, len(entries))
		for _, e := range entries {
			out = append(out, fileEntry(e))
		}
		c.JSON(http.StatusOK, out)
		return
	}

	// Try container filesystem first
	if containerName := h.findContainer(ctx, workspaceID); containerName != "" {
		// Portable file listing: works on both GNU and BusyBox/Alpine.
		// Uses find + sh -c stat to output TYPE|SIZE|PATH per line.
		output, err := h.execInContainer(ctx, containerName, []string{
			"sh", "-c",
			fmt.Sprintf(`find '%s' -maxdepth %d -not -path '*/.git/*' -not -path '*/__pycache__/*' -not -path '*/node_modules/*' -not -path '*/.hermes' -not -path '*/.hermes/*' -not -name .DS_Store | while IFS= read -r f; do
				rel="${f#'%s'/}"; [ "$rel" = '%s' ] && continue; [ -z "$rel" ] && continue
				if [ -d "$f" ]; then echo "d|0|$rel"; else s=$(stat -c %%s "$f" 2>/dev/null || stat -f %%z "$f" 2>/dev/null || echo 0); echo "f|$s|$rel"; fi
			done`, listPath, depth, listPath, listPath),
		})
		if err != nil {
			log.Printf("Container file list failed, falling back to host: %v", err)
		} else {
			var files []fileEntry
			for _, line := range strings.Split(output, "\n") {
				parts := strings.SplitN(line, "|", 3)
				if len(parts) != 3 || parts[2] == "" {
					continue
				}
				size, _ := strconv.ParseInt(parts[1], 10, 64)
				files = append(files, fileEntry{
					Path: parts[2],
					Size: size,
					Dir:  parts[0] == "d",
				})
			}
			if files == nil {
				files = []fileEntry{}
			}
			c.JSON(http.StatusOK, files)
			return
		}
	}

	// walkConfigTree lists a host-side config directory (the /configs mirror or a
	// template dir) into the Files-API wire shape, applying the same depth limit,
	// symlink skip (OFFSEC-010), and noise-file filter. Shared by the
	// molecules-server host-side mirror and the legacy template-dir fallback so
	// the two never diverge.
	walkConfigTree := func(walkRoot string) []fileEntry {
		var files []fileEntry
		filepath.Walk(walkRoot, func(path string, info os.FileInfo, err error) error {
			if err != nil || path == walkRoot {
				return nil
			}
			// Skip symlinks to prevent path traversal via malicious symlinks
			// inside the workspace config directory (OFFSEC-010).
			if info.Mode()&os.ModeSymlink != 0 {
				return nil
			}
			rel, _ := filepath.Rel(walkRoot, path)
			// Enforce depth limit
			if strings.Count(rel, string(filepath.Separator))+1 > depth {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			base := filepath.Base(rel)
			if base == ".git" || base == ".DS_Store" || base == "__pycache__" || base == "node_modules" {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			files = append(files, fileEntry{
				Path: rel,
				Size: info.Size(),
				Dir:  info.IsDir(),
			})
			return nil
		})
		if files == nil {
			files = []fileEntry{}
		}
		return files
	}

	// Docker-less molecules-server (#206): list /configs from the host-side
	// mirror the CPProvisioner persisted at provision. Fixes the empty "[]"
	// listing a molecules-server tenant returned for /configs even though config
	// was delivered. Only for root=/configs; other roots have no host-side
	// mirror and fall through to the template dir.
	if mirror := h.hostSideConfigsRoot(rootPath, workspaceID); mirror != "" {
		if fi, statErr := os.Stat(mirror); statErr == nil && fi.IsDir() {
			walkRoot := mirror
			if subPath != "" {
				walkRoot = filepath.Join(mirror, subPath)
			}
			if _, err := os.Stat(walkRoot); err == nil {
				c.JSON(http.StatusOK, walkConfigTree(walkRoot))
				return
			}
			// Mirror present but the requested subpath isn't in it — return empty
			// rather than falling through to the (unrelated) template dir.
			c.JSON(http.StatusOK, []fileEntry{})
			return
		}
	}

	// Fallback: host-side template dir (only for templates, not ws-* workspace volumes)
	configDir := h.resolveTemplateDir(wsName)
	if configDir == "" {
		c.JSON(http.StatusOK, []fileEntry{})
		return
	}

	walkRoot := configDir
	if subPath != "" {
		walkRoot = filepath.Join(configDir, subPath)
	}
	if _, err := os.Stat(walkRoot); os.IsNotExist(err) {
		c.JSON(http.StatusOK, []fileEntry{})
		return
	}
	c.JSON(http.StatusOK, walkConfigTree(walkRoot))
}

// ReadFile handles GET /workspaces/:id/files/*path
func (h *TemplatesHandler) ReadFile(c *gin.Context) {
	workspaceID := c.Param("id")
	filePath := c.Param("path")
	filePath = strings.TrimPrefix(filePath, "/")

	if err := validateRelPath(filePath); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
		return
	}

	ctx := c.Request.Context()
	rootPath := c.DefaultQuery("root", "/configs")
	if !allowedRoots[rootPath] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "root must be one of: /configs, /workspace, /home, /plugins, /agent-home"})
		return
	}
	if isAgentHomeStubRequest(rootPath) {
		c.JSON(http.StatusNotImplemented, gin.H{"error": agentHomeStubMessage})
		return
	}

	var wsName, instanceID, runtime string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT name, COALESCE(instance_id, ''), COALESCE(runtime, '') FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&wsName, &instanceID, &runtime); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	// SaaS workspace (EC2-per-workspace) — no Docker on this tenant. Read
	// via SSH through the EIC endpoint, mirroring WriteFile's dispatch
	// in this same file. Pre-fix this branch was missing and SaaS
	// workspaces always fell through to the local-Docker container check
	// (finds nothing on a SaaS tenant) + template-dir fallback (returns
	// the seed template, not the persisted state). Net effect: the
	// canvas Config tab always 404'd for SaaS workspaces — visible to
	// users after #2781 added the "no config.yaml" error UX.
	//
	// `?root=` flows through resolveWorkspaceFilePath: "/configs" stays
	// the per-runtime managed-config indirection (claude-code → /configs,
	// hermes → /home/ubuntu/.hermes); other allow-listed roots
	// (`/home`, `/workspace`, `/plugins`) pass through literally so
	// list/read/write/delete agree on what file a tree row points to.
	//
	// isEC2InstanceID gates this on a REAL EC2 id: a molecules-server
	// (local-docker) workspace persists its container NAME in instance_id,
	// which must route to the docker-exec path below, not the AWS-only EIC
	// tunnel. See files_backend_dispatch.go.
	if isEC2InstanceID(instanceID) {
		content, err := readFileViaEIC(ctx, instanceID, runtime, rootPath, filePath)
		if err == nil {
			c.JSON(http.StatusOK, gin.H{
				"path":    filePath,
				"content": string(content),
				"size":    len(content),
			})
			return
		}
		if errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found on workspace"})
			return
		}
		log.Printf("ReadFile EIC for %s path=%s: %v", workspaceID, filePath, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to read file: %v", err)})
		return
	}

	// Local Docker path: try the workspace container first. `cat` wants a
	// single path argument — passing rootPath and filePath as two args
	// would make `cat` try to read the rootPath directory (error) and
	// then resolve filePath relative to the container's cwd, which
	// isn't guaranteed to equal rootPath.
	if containerName := h.findContainer(ctx, workspaceID); containerName != "" {
		fullPath := strings.TrimRight(rootPath, "/") + "/" + filePath
		content, err := h.execInContainer(ctx, containerName, []string{"cat", fullPath})
		if err == nil {
			c.JSON(http.StatusOK, gin.H{
				"path":    filePath,
				"content": content,
				"size":    len(content),
			})
			return
		}
	}

	// Docker-less molecules-server (#206): the tenant has no docker.sock into
	// the runtime container, so the container `cat` above found nothing and the
	// legacy path returned a misleading 59-byte "container offline, no template"
	// 404 even though the config was delivered. Serve the file from the
	// per-workspace host-side /configs mirror the CPProvisioner persisted at
	// (re)provision — the SAME rendered bundle the container's /configs volume
	// is built from (config.yaml + prompts/* + secret config files). Only for
	// root=/configs; other roots have no host-side mirror. This is the read-back
	// half of the OSS-clean, R2-free config path (config → volume mount into the
	// box; read-API → this mirror).
	if mirror := h.hostSideConfigsRoot(rootPath, workspaceID); mirror != "" {
		if fi, statErr := os.Stat(mirror); statErr == nil && fi.IsDir() {
			// validateRelPath already ran at the top of ReadFile; re-validate as
			// defense-in-depth before joining into the mirror dir.
			if err := validateRelPath(filePath); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
				return
			}
			data, rerr := os.ReadFile(filepath.Join(mirror, filePath))
			if rerr == nil {
				c.JSON(http.StatusOK, gin.H{
					"path":    filePath,
					"content": string(data),
					"size":    len(data),
				})
				return
			}
			// The mirror EXISTS for this workspace (config WAS delivered) but the
			// requested file genuinely isn't in the /configs bundle. Fail loud +
			// clear — NOT the misleading "container offline, no template" stub.
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found in workspace /configs"})
			return
		}
	}

	// Fallback: host-side template dir
	templateDir := h.resolveTemplateDir(wsName)
	if templateDir == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found (container offline, no template)"})
		return
	}
	// validateRelPath is already called above (line 260) for the container path,
	// but the fallback below uses filePath directly in filepath.Join without
	// any sanitization. Re-validate before the host-side read to close the gap.
	if err := validateRelPath(filePath); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
		return
	}
	fullPath := filepath.Join(templateDir, filePath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"path":    filePath,
		"content": string(data),
		"size":    len(data),
	})
}

// WriteFile handles PUT /workspaces/:id/files/*path
func (h *TemplatesHandler) WriteFile(c *gin.Context) {
	workspaceID := c.Param("id")
	filePath := c.Param("path")
	filePath = strings.TrimPrefix(filePath, "/")

	if err := validateRelPath(filePath); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
		return
	}

	var body struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	ctx := c.Request.Context()
	rootPath := c.DefaultQuery("root", "/configs")
	if !allowedRoots[rootPath] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "root must be one of: /configs, /workspace, /home, /plugins, /agent-home"})
		return
	}
	if isAgentHomeStubRequest(rootPath) {
		c.JSON(http.StatusNotImplemented, gin.H{"error": agentHomeStubMessage})
		return
	}
	var wsName, instanceID, runtime string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT name, COALESCE(instance_id, ''), COALESCE(runtime, '') FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&wsName, &instanceID, &runtime); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	// SaaS workspace (EC2-per-workspace) — no Docker on this tenant. Write
	// via SSH through the EIC endpoint to the runtime-specific path.
	// `?root=` flows through the same per-runtime / literal indirection
	// as ReadFile so list/read/write/delete agree on what file a tree
	// row points to.
	//
	// isEC2InstanceID gates this on a REAL EC2 id: a molecules-server
	// (local-docker) workspace persists its container NAME in instance_id,
	// which must route to the docker-exec path below, not the AWS-only EIC
	// tunnel. See files_backend_dispatch.go.
	if isEC2InstanceID(instanceID) {
		if err := writeFileViaEIC(ctx, instanceID, runtime, rootPath, filePath, []byte(body.Content)); err != nil {
			log.Printf("WriteFile EIC for %s path=%s: %v", workspaceID, filePath, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to write file: %v", err)})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "saved", "path": filePath})
		if h.wh != nil {
			// internal#624: 15s per-workspace debounce around the file-write
			// → RestartByID trigger. Canvas Save fires N PUTs in a burst;
			// without this each PUT chains into the coalesceRestart drain
			// loop and produces back-to-back EC2 recreate cycles. The
			// helper still uses goAsync internally (drains via
			// h.wh.waitAsyncForTest), preserving RFC internal#524 Layer 1.
			h.wh.maybeRestartAfterFileWrite(workspaceID)
		}
		return
	}

	// Local Docker path — write via CopyToContainer when container is running
	if containerName := h.findContainer(ctx, workspaceID); containerName != "" {
		singleFile := map[string]string{filePath: body.Content}
		if err := h.copyFilesToContainer(ctx, containerName, "/configs", singleFile); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to write file: %v", err)})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "saved", "path": filePath})
		if h.wh != nil {
			// internal#624: 15s per-workspace debounce around the file-write
			// → RestartByID trigger. Canvas Save fires N PUTs in a burst;
			// without this each PUT chains into the coalesceRestart drain
			// loop and produces back-to-back EC2 recreate cycles. The
			// helper still uses goAsync internally (drains via
			// h.wh.waitAsyncForTest), preserving RFC internal#524 Layer 1.
			h.wh.maybeRestartAfterFileWrite(workspaceID)
		}
		return
	}

	// Container offline — write via ephemeral container mounting the config volume
	volName := provisioner.ConfigVolumeName(workspaceID)
	singleFile := map[string]string{filePath: body.Content}
	if err := h.writeViaEphemeral(ctx, volName, workspaceID, singleFile); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to write file: %v", err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "saved", "path": filePath})
	if h.wh != nil {
		// internal#624: 15s per-workspace debounce around the file-write
		// → RestartByID trigger. Canvas Save fires N PUTs in a burst;
		// without this each PUT chains into the coalesceRestart drain
		// loop and produces back-to-back EC2 recreate cycles. The
		// helper still uses goAsync internally (drains via
		// h.wh.waitAsyncForTest), preserving RFC internal#524 Layer 1.
		h.wh.maybeRestartAfterFileWrite(workspaceID)
	}
}

// DeleteFile handles DELETE /workspaces/:id/files/*path
func (h *TemplatesHandler) DeleteFile(c *gin.Context) {
	workspaceID := c.Param("id")
	filePath := c.Param("path")
	// Reject absolute paths before stripping the leading slash — this check
	// must come before the strip so that "/etc/passwd" is not silently accepted.
	if filepath.IsAbs(filePath) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "absolute paths not permitted"})
		return
	}
	filePath = strings.TrimPrefix(filePath, "/")

	if err := validateRelPath(filePath); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid path"})
		return
	}

	ctx := c.Request.Context()
	rootPath := c.DefaultQuery("root", "/configs")
	if !allowedRoots[rootPath] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "root must be one of: /configs, /workspace, /home, /plugins, /agent-home"})
		return
	}
	if isAgentHomeStubRequest(rootPath) {
		c.JSON(http.StatusNotImplemented, gin.H{"error": agentHomeStubMessage})
		return
	}
	var wsName, instanceID, runtime string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT name, COALESCE(instance_id, ''), COALESCE(runtime, '') FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&wsName, &instanceID, &runtime); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	// SaaS workspace (EC2-per-workspace) — no Docker on this tenant. Delete
	// via SSH through the EIC endpoint, mirroring ReadFile/WriteFile's
	// dispatch. Pre-fix this branch was missing — DeleteFile fell through
	// to local-Docker (no container) + ephemeral-volume (no Docker) and
	// silently 500'd. See issue #2999.
	//
	// isEC2InstanceID gates this on a REAL EC2 id: a molecules-server
	// (local-docker) workspace persists its container NAME in instance_id,
	// which must route to the docker-exec path below, not the AWS-only EIC
	// tunnel. See files_backend_dispatch.go.
	if isEC2InstanceID(instanceID) {
		if err := deleteFileViaEIC(ctx, instanceID, runtime, rootPath, filePath); err != nil {
			log.Printf("DeleteFile EIC for %s root=%s path=%s: %v", workspaceID, rootPath, filePath, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to delete file: %v", err)})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "deleted", "path": filePath})
		if h.wh != nil {
			// internal#624: 15s per-workspace debounce around the file-write
			// → RestartByID trigger. Canvas Save fires N PUTs in a burst;
			// without this each PUT chains into the coalesceRestart drain
			// loop and produces back-to-back EC2 recreate cycles. The
			// helper still uses goAsync internally (drains via
			// h.wh.waitAsyncForTest), preserving RFC internal#524 Layer 1.
			h.wh.maybeRestartAfterFileWrite(workspaceID)
		}
		return
	}

	// Delete via docker exec when container is running
	if containerName := h.findContainer(ctx, workspaceID); containerName != "" {
		// CWE-78: use path.Join instead of string concat to prevent path
		// injection into the exec argument. validateRelPath above is the primary
		// guard; path.Join is defence-in-depth. It must be path.Join, NOT
		// filepath.Join: this is a container (Linux) path, and on a Windows
		// host filepath.Join yields `\configs\...`, making rm -f a silent
		// no-op. Use -f (not -rf) to avoid recursive deletion of an entire
		// directory via traversal.
		_, err := h.execInContainer(ctx, containerName, []string{"rm", "-f", wirepath.Join("/configs", filePath)})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to delete: %v", err)})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "deleted", "path": filePath})
		if h.wh != nil {
			// internal#624: 15s per-workspace debounce around the file-write
			// → RestartByID trigger. Canvas Save fires N PUTs in a burst;
			// without this each PUT chains into the coalesceRestart drain
			// loop and produces back-to-back EC2 recreate cycles. The
			// helper still uses goAsync internally (drains via
			// h.wh.waitAsyncForTest), preserving RFC internal#524 Layer 1.
			h.wh.maybeRestartAfterFileWrite(workspaceID)
		}
		return
	}

	// Container offline — delete via ephemeral container
	volName := provisioner.ConfigVolumeName(workspaceID)
	if err := h.deleteViaEphemeral(ctx, volName, workspaceID, filePath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to delete: %v", err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted", "path": filePath})
	if h.wh != nil {
		// internal#624: 15s per-workspace debounce around the file-write
		// → RestartByID trigger. Canvas Save fires N PUTs in a burst;
		// without this each PUT chains into the coalesceRestart drain
		// loop and produces back-to-back EC2 recreate cycles. The
		// helper still uses goAsync internally (drains via
		// h.wh.waitAsyncForTest), preserving RFC internal#524 Layer 1.
		h.wh.maybeRestartAfterFileWrite(workspaceID)
	}
}

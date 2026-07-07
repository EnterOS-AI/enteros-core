package handlers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

// ListRegistry handles GET /plugins — lists all available plugins from the registry.
// Supports optional ?runtime=<name> query param to filter to plugins that
// declare support for the given runtime. Plugins with no declared
// `runtimes` field are treated as "unspecified, try it" and included.
func (h *PluginsHandler) ListRegistry(c *gin.Context) {
	runtime := c.Query("runtime")
	c.JSON(http.StatusOK, h.listRegistryFiltered(runtime))
}

// listRegistryFiltered is the shared read-plus-filter path used by both
// /plugins and /workspaces/:id/plugins/available.
func (h *PluginsHandler) listRegistryFiltered(runtime string) []pluginInfo {
	plugins := []pluginInfo{}
	entries, err := os.ReadDir(h.pluginsDir)
	if err != nil {
		return plugins
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info := h.readPluginManifest(filepath.Join(h.pluginsDir, e.Name()), e.Name())
		if runtime != "" && !info.supportsRuntime(runtime) {
			continue
		}
		// The installable handle for a registry entry is its REAL provider
		// source (e.g. gitea://<owner>/<repo>#<pinned-ref>), derived from the
		// manifest.json SSOT that also seeds this registry dir. A catalog
		// consumer (canvas, mgmt-MCP list_available_plugins) passes this
		// straight to install_plugin, which records it as the workspace's
		// declared source — and the on-box runtime boot-installer can only
		// FETCH gitea:// / presign://. The old local://<dir-name> handle was
		// un-fetchable there ("[plugins] skip unsupported source"), so a
		// catalog-installed plugin recorded but never loaded. Fall back to
		// local://<name> for a plugin dir with no manifest entry (a curated
		// local drop), preserving that escape hatch.
		if src, ok := pluginInstallSource(e.Name()); ok {
			info.Source = src
		} else {
			info.Source = "local://" + e.Name()
		}
		plugins = append(plugins, info)
	}
	return plugins
}

// ListAvailableForWorkspace handles GET /workspaces/:id/plugins/available —
// returns plugins from the registry filtered to those supported by the
// workspace's runtime. If no runtime lookup is wired, falls back to the
// full registry.
func (h *PluginsHandler) ListAvailableForWorkspace(c *gin.Context) {
	workspaceID := c.Param("id")
	runtime := ""
	if h.runtimeLookup != nil {
		if r, err := h.runtimeLookup(workspaceID); err == nil {
			runtime = r
		}
	}
	c.JSON(http.StatusOK, h.listRegistryFiltered(runtime))
}

// ListInstalled handles GET /workspaces/:id/plugins — lists plugins installed in the workspace.
func (h *PluginsHandler) ListInstalled(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()
	plugins := []pluginInfo{}

	// Dispatch order mirrors Install/Uninstall/deliverToContainer
	// (plugins_install.go): a local Docker container wins; otherwise the
	// instance_id is SHAPE-routed (isEC2InstanceID, files_backend_dispatch.go)
	// — a real "i-<hex>" EC2 id lists over EIC SSH, a local-docker CONTAINER
	// NAME lists via docker exec. Without the SaaS branch ListInstalled
	// returned [] for EVERY SaaS tenant (no LOCAL container), so an installed
	// plugin read back as not-installed — the "[] readback after a successful
	// install" bug.
	if containerName := h.findRunningContainer(ctx, workspaceID); containerName != "" {
		plugins = h.listInstalledViaDocker(ctx, containerName)
		h.annotateRuntimeSupport(workspaceID, plugins)
		c.JSON(http.StatusOK, plugins)
		return
	}

	if instanceID, runtime := h.lookupSaaSDispatch(workspaceID); instanceID != "" {
		// SHAPE-routed exactly like Uninstall/deliverToContainer + the Files API
		// (files_backend_dispatch.go). A real "i-<hex>" EC2 id → EIC SSH list;
		// a local-docker CONTAINER NAME ("mol-ws-*") → docker exec into that
		// container; NEVER route a local-docker tenant to the AWS-only EIC path
		// — with no AWS creds that ssh dance hangs 90-120s, and the canvas
		// Plugins tab / concierge config_tab_sweep then fails "context deadline
		// exceeded" (core#182 / BUG-A). Install/Uninstall already shape-route;
		// the listing path was the last EIC-unconditional caller.
		if isEC2InstanceID(instanceID) {
			plugins = h.listInstalledViaEIC(ctx, workspaceID, instanceID, runtime)
			h.annotateRuntimeSupport(workspaceID, plugins)
			c.JSON(http.StatusOK, plugins)
			return
		}
		// molecules-server / local-docker backend: instance_id IS the running
		// container name on the local docker daemon. List via the SAME docker
		// primitive the findRunningContainer branch uses (findRunningContainer
		// looks for "ws-<id>" and never matches the CP's "mol-ws-*" name, so we
		// key the read on the instance_id container name).
		if h.docker != nil {
			plugins = h.listInstalledViaDocker(ctx, instanceID)
			h.annotateRuntimeSupport(workspaceID, plugins)
			c.JSON(http.StatusOK, plugins)
			return
		}
		// Non-EC2 instance id (local-docker) but no docker client wired — the
		// hardened, docker-less tenant /platform (#206: no docker.sock, so we
		// can't exec into mol-ws). We must NOT ride the 90-120s AWS EIC hang.
		// The install path for this posture is now PULL-mode (deliverToContainer
		// → errNoPushTarget → declare + record + re-materialize), and the
		// workspace_plugins DB row is the backend-agnostic install SSOT (written
		// by the docker, EIC and pull paths alike — see plugins_tracking.go /
		// listInstalledPlugins). List from there: fast, no EIC, and it reflects
		// exactly what the boot materializer pulls into /configs/plugins. This
		// is what makes the readback after a pull-mode install (canvas Plugins
		// tab / plugin-install-lifecycle e2e) see the plugin.
		installed, dberr := listInstalledPlugins(ctx, workspaceID)
		if dberr != nil {
			log.Printf("ListInstalled: workspace %s docker-less, workspace_plugins read failed: %v (returning empty, not riding AWS EIC)", workspaceID, dberr)
			c.JSON(http.StatusOK, plugins)
			return
		}
		for _, p := range installed {
			if validatePluginName(p.PluginName) != nil {
				continue
			}
			plugins = append(plugins, pluginInfo{Name: p.PluginName})
		}
		h.annotateRuntimeSupport(workspaceID, plugins)
		c.JSON(http.StatusOK, plugins)
		return
	}

	// Neither backend reachable — empty list (fail-soft, same as before).
	c.JSON(http.StatusOK, plugins)
}

// listInstalledViaDocker lists plugins under /configs/plugins/ inside
// containerName via docker exec, reading each plugin.yaml. Shared by the
// findRunningContainer branch and the local-docker instance_id branch — the
// docker read is identical once we know which container to exec into.
func (h *PluginsHandler) listInstalledViaDocker(ctx context.Context, containerName string) []pluginInfo {
	plugins := []pluginInfo{}
	output, err := h.execInContainer(ctx, containerName, []string{
		"sh", "-c", "ls -1 /configs/plugins/ 2>/dev/null || true",
	})
	if err != nil {
		return plugins
	}
	for _, name := range strings.Split(output, "\n") {
		name = strings.TrimSpace(name)
		if name == "" || validatePluginName(name) != nil {
			continue
		}
		// Try to read manifest from container (safe: name is validated)
		manifestOutput, err := h.execInContainer(ctx, containerName, []string{
			"cat", fmt.Sprintf("/configs/plugins/%s/plugin.yaml", name),
		})
		if err != nil || manifestOutput == "" {
			plugins = append(plugins, pluginInfo{Name: name})
			continue
		}
		plugins = append(plugins, parseManifestYAML(name, []byte(manifestOutput)))
	}
	return plugins
}

// listInstalledViaEIC lists plugins on a SaaS workspace EC2 over the EIC SSH
// tunnel, reading each plugin.yaml. Returns [] on any tunnel/list error
// (fail-soft — the canvas treats an empty list as "none installed / try
// again", never a hard error). Only reached for a real "i-<hex>" EC2 id.
func (h *PluginsHandler) listInstalledViaEIC(ctx context.Context, workspaceID, instanceID, runtime string) []pluginInfo {
	plugins := []pluginInfo{}
	names, err := listPluginsViaEIC(ctx, instanceID, runtime)
	if err != nil {
		log.Printf("ListInstalled: EIC list failed for %s: %v", workspaceID, err)
		return plugins
	}
	for _, name := range names {
		if validatePluginName(name) != nil {
			continue
		}
		manifest, mErr := readPluginManifestViaEIC(ctx, instanceID, runtime, name)
		if mErr != nil || len(manifest) == 0 {
			plugins = append(plugins, pluginInfo{Name: name})
			continue
		}
		plugins = append(plugins, parseManifestYAML(name, manifest))
	}
	return plugins
}

// annotateRuntimeSupport stamps each plugin with whether it still supports the
// workspace's current runtime. Lets the canvas grey out plugins that went inert
// after a runtime change. Shared by the Docker and SaaS ListInstalled branches.
func (h *PluginsHandler) annotateRuntimeSupport(workspaceID string, plugins []pluginInfo) {
	if h.runtimeLookup == nil {
		return
	}
	runtime, err := h.runtimeLookup(workspaceID)
	if err != nil || runtime == "" {
		return
	}
	for i := range plugins {
		ok := plugins[i].supportsRuntime(runtime)
		plugins[i].SupportedOnRuntime = &ok
	}
}

// CheckRuntimeCompatibility handles GET /workspaces/:id/plugins/compatibility?runtime=<name>
// — preflight for runtime changes. Reports which installed plugins would
// become inert if the workspace switched to <runtime>. Canvas uses this
// to show a confirm dialog before applying the change.
func (h *PluginsHandler) CheckRuntimeCompatibility(c *gin.Context) {
	workspaceID := c.Param("id")
	targetRuntime := c.Query("runtime")
	ctx := c.Request.Context()

	if targetRuntime == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "runtime query parameter is required"})
		return
	}

	containerName := h.findRunningContainer(ctx, workspaceID)
	if containerName == "" {
		// Workspace not running — nothing installed yet, trivially compatible.
		c.JSON(http.StatusOK, gin.H{
			"target_runtime": targetRuntime,
			"compatible":     []pluginInfo{},
			"incompatible":   []pluginInfo{},
			"all_compatible": true,
		})
		return
	}

	output, err := h.execInContainer(ctx, containerName, []string{
		"sh", "-c", "ls -1 /configs/plugins/ 2>/dev/null || true",
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list installed plugins"})
		return
	}

	compatible := []pluginInfo{}
	incompatible := []pluginInfo{}
	for _, name := range strings.Split(output, "\n") {
		name = strings.TrimSpace(name)
		if name == "" || validatePluginName(name) != nil {
			continue
		}
		manifestOutput, err := h.execInContainer(ctx, containerName, []string{
			"cat", fmt.Sprintf("/configs/plugins/%s/plugin.yaml", name),
		})
		var info pluginInfo
		if err != nil || manifestOutput == "" {
			info = pluginInfo{Name: name}
		} else {
			info = parseManifestYAML(name, []byte(manifestOutput))
		}
		if info.supportsRuntime(targetRuntime) {
			compatible = append(compatible, info)
		} else {
			incompatible = append(incompatible, info)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"target_runtime": targetRuntime,
		"compatible":     compatible,
		"incompatible":   incompatible,
		"all_compatible": len(incompatible) == 0,
	})
}

package handlers

import (
	"net/http"
	"strings"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"github.com/gin-gonic/gin"
)

// BootConfigHandler serves the CORE-side boot-config fetch endpoint:
//
//	GET /internal/workspaces/boot-config
//	Authorization: Bearer <one-time boot token>
//
// This is the config-INTO-container half of the FINAL, platform-agnostic config
// delivery (see provisioner/bootconfig_token.go). At (re)provision the tenant
// minted a one-time boot token bound to the workspace and injected it into the
// runtime container's env (MOLECULE_CONFIG_BOOT_TOKEN) via the Env map the CP
// forwards verbatim. At boot the container fetches HERE, on the tenant-server it
// already reaches at PLATFORM_URL (localhost/shared-net on local-docker,
// <slug>.moleculesai.app remote) — NEVER the CP platform API. We validate the
// token, serve the rendered bundle from the host-side /configs mirror ONCE as a
// {relpath: base64(content)} JSON object (the SAME wire shape the R2 relay used,
// so the runtime unpack is transport-agnostic), then invalidate the token.
//
// OSS-clean: this path has NO R2 and NO CP dependency. A self-host provisions a
// workspace and its own tenant-server delivers config to the box.
//
// Dark by default: when the store is nil (feature flag off) the endpoint 404s,
// exactly as if unrouted, so a flag-off deployment is byte-identical.
type BootConfigHandler struct {
	store        *provisioner.BootConfigTokenStore
	hostStateDir string
}

// NewBootConfigHandler wires the shared token store (mint side: CPProvisioner)
// and the host-side mirror base dir (the same value the Files API reads). A nil
// store or empty dir disables the endpoint (returns 404).
func NewBootConfigHandler(store *provisioner.BootConfigTokenStore, hostStateDir string) *BootConfigHandler {
	return &BootConfigHandler{store: store, hostStateDir: hostStateDir}
}

// Enabled reports whether the boot-config fetch endpoint is live.
func (h *BootConfigHandler) Enabled() bool {
	return h != nil && h.store != nil && strings.TrimSpace(h.hostStateDir) != ""
}

func bearerToken(authHeader string) string {
	const prefix = "Bearer "
	if len(authHeader) > len(prefix) && strings.EqualFold(authHeader[:len(prefix)], prefix) {
		return strings.TrimSpace(authHeader[len(prefix):])
	}
	return ""
}

// Serve handles GET /internal/workspaces/boot-config.
func (h *BootConfigHandler) Serve(c *gin.Context) {
	if !h.Enabled() {
		// Feature off → behave as if the route does not exist (no capability leak).
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	token := bearerToken(c.GetHeader("Authorization"))
	if token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing boot token"})
		return
	}
	workspaceID, ok := h.store.Lookup(token)
	if !ok {
		// Unknown, expired, or already-consumed token. Do not distinguish (no
		// oracle for a brute-force / replay attempt).
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired boot token"})
		return
	}

	mirror := provisioner.HostSideConfigsDir(h.hostStateDir, workspaceID)
	bundle, err := provisioner.BuildConfigBundleJSON(mirror)
	if err != nil {
		// Transient server-side read failure — do NOT consume the token so the
		// container's boot retry can succeed. Fail loud.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read workspace config bundle"})
		return
	}
	if len(bundle) == 0 {
		// The token is valid but the mirror is genuinely empty/absent. The mirror
		// is written BEFORE the token is minted (CPProvisioner.Start ordering), so
		// this is a real missing-config condition, not a race — fail loud, do not
		// consume (a reprovision re-persists the mirror + re-mints).
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace config bundle is empty"})
		return
	}

	// Committed to serving a real bundle → invalidate the one-time token now.
	h.store.Consume(token)
	// Body is the {relpath: base64(content)} object the runtime unpacks directly.
	c.JSON(http.StatusOK, bundle)
}

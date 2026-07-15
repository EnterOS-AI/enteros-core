package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockerimage "github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/providers"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// WorkspaceImageService is an explicit, single-host maintenance utility. It
// pulls workspace template images from the configured registry and can remove
// matching ws-* containers so the local provisioner recreates them later.
//
// It is not the managed runtime rollout path: managed launches are selected by
// control-plane image pins, and a template publish does not prove that an
// already-running workspace changed image.
type WorkspaceImageService struct {
	docker *dockerclient.Client
}

func NewWorkspaceImageService(docker *dockerclient.Client) *WorkspaceImageService {
	return &WorkspaceImageService{docker: docker}
}

// AllRuntimes is the canonical set of workspace runtimes this tenant will
// pull/recreate template images for. It is DERIVED from the same providers
// manifest SSOT (internal/providers/providers.yaml `runtimes:` block, mirrored
// from CP's providers.yaml) that the rest of the platform routes against —
// NOT a second hand-maintained list.
//
// Why derive instead of hardcode (controlplane#578): the old hardcoded slice
// here silently drifted from CP's runtime pin-promote/redeploy allowlist. A
// runtime pin could be accepted CP-side, then this tenant's
// POST /admin/workspace-images/refresh?runtime=<name> rejected it 400
// ("unknown runtime"), so image fixes never deployed. Deriving from the
// manifest makes the tenant allowlist and the CP allowlist provably the same
// set — they can't drift again.
//
// imageRefreshFallbackRuntimes is used ONLY if the embedded providers manifest
// fails to load (which would be a build/CI failure caught by the providers
// package's own tests, never a healthy prod). It preserves the historical
// behavior so a manifest regression can never take the refresh endpoint fully
// offline. Kept in lockstep with the providers.yaml
// `runtimes:` keys; the drift guard in admin_workspace_images_test.go asserts
// the two match.
var imageRefreshFallbackRuntimes = []string{
	"claude-code", "codex", "hermes", "openclaw",
}

// AllRuntimes is computed once at package init from the providers SSOT.
var AllRuntimes = loadImageRefreshRuntimes()

// loadImageRefreshRuntimes returns the sorted runtime names declared in the
// providers manifest, falling back to imageRefreshFallbackRuntimes if the
// manifest can't be loaded.
func loadImageRefreshRuntimes() []string {
	m, err := providers.LoadManifest()
	if err != nil || len(m.Runtimes) == 0 {
		if err != nil {
			log.Printf("workspace-images: providers.LoadManifest failed (%v); falling back to static runtime allowlist", err)
		}
		out := append([]string(nil), imageRefreshFallbackRuntimes...)
		sort.Strings(out)
		return out
	}
	out := make([]string, 0, len(m.Runtimes))
	for rt := range m.Runtimes {
		out = append(out, rt)
	}
	sort.Strings(out)
	return out
}

// RefreshResult is the per-call outcome surfaced to the admin HTTP caller.
type RefreshResult struct {
	Pulled    []string `json:"pulled"`
	Failed    []string `json:"failed"`
	Recreated []string `json:"recreated"`
}

// TemplateImageRef returns the canonical image ref for a runtime's template,
// using the configured registry (provisioner.RegistryPrefix()) and the
// moving `:latest` tag.
//
// Defaults to registry.moleculesai.app/molecule-ai/workspace-template-
// <runtime>:latest. MOLECULE_IMAGE_REGISTRY may select an operator-controlled
// mirror for a registry-backed self-host.
func TemplateImageRef(runtime string) string {
	return fmt.Sprintf("%s/workspace-template-%s:latest", provisioner.RegistryPrefix(), runtime)
}

// registryAuthHeader returns the base64-encoded JSON auth payload Docker's
// ImagePull expects in PullOptions.RegistryAuth, or empty string when no
// legacy GHCR_USER/GHCR_TOKEN pair is set. The names are retained only for
// compatibility; the payload is sent to the host selected by
// MOLECULE_IMAGE_REGISTRY.
//
// The Docker SDK doesn't read ~/.docker/config.json — every authenticated
// pull needs an explicit RegistryAuth string. The serveraddress field is
// resolved from provisioner.RegistryHost() so it tracks MOLECULE_IMAGE_REGISTRY
// when the operator points the platform at a private mirror.
func registryAuthHeader() string {
	user := strings.TrimSpace(os.Getenv("GHCR_USER"))
	token := strings.TrimSpace(os.Getenv("GHCR_TOKEN"))
	if user == "" || token == "" {
		return ""
	}
	payload := map[string]string{
		"username":      user,
		"password":      token,
		"serveraddress": provisioner.RegistryHost(),
	}
	js, err := json.Marshal(payload)
	if err != nil {
		log.Printf("workspace-images: failed to marshal registry auth: %v", err)
		return ""
	}
	return base64.StdEncoding.EncodeToString(js)
}

// Refresh pulls the requested runtimes' template images from the configured
// registry and (if
// recreate) force-removes any matching ws-* containers so the platform
// re-provisions them on next interaction.
//
// Soft-fails per runtime: one missing image (e.g. unpublished template)
// doesn't abort the others. Per-runtime failures are in RefreshResult.Failed.
// Returns a non-nil error only when the recreate phase couldn't enumerate
// containers at all (caller should surface that as 500).
func (s *WorkspaceImageService) Refresh(ctx context.Context, runtimes []string, recreate bool) (RefreshResult, error) {
	res := RefreshResult{Pulled: []string{}, Failed: []string{}, Recreated: []string{}}
	auth := registryAuthHeader()

	pullCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for _, rt := range runtimes {
		image := TemplateImageRef(rt)
		opts := dockerimage.PullOptions{Platform: provisioner.DefaultImagePlatform()}
		if auth != "" {
			opts.RegistryAuth = auth
		}
		rc, err := s.docker.ImagePull(pullCtx, image, opts)
		if err != nil {
			log.Printf("workspace-images/refresh: pull %s failed: %v", rt, err)
			res.Failed = append(res.Failed, rt)
			continue
		}
		// Drain to completion. The engine treats early-close as "abandon",
		// leaving partial layers around with no reference.
		if _, err := io.Copy(io.Discard, rc); err != nil {
			rc.Close()
			log.Printf("workspace-images/refresh: drain %s failed: %v", rt, err)
			res.Failed = append(res.Failed, rt)
			continue
		}
		rc.Close()
		res.Pulled = append(res.Pulled, rt)
	}

	if !recreate {
		return res, nil
	}

	listCtx, listCancel := context.WithTimeout(ctx, 30*time.Second)
	defer listCancel()
	containers, err := s.docker.ContainerList(listCtx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("name", "ws-")),
	})
	if err != nil {
		log.Printf("workspace-images/refresh: container list failed: %v", err)
		return res, fmt.Errorf("container list: %w", err)
	}

	pulledSet := map[string]struct{}{}
	for _, rt := range res.Pulled {
		pulledSet[rt] = struct{}{}
	}
	for _, ctr := range containers {
		// ContainerList's ctr.Image is the *resolved digest* (sha256:…),
		// not the human-readable tag. Inspect to get Config.Image so we
		// can match against the pulled-runtime set.
		inspectCtx, inspectCancel := context.WithTimeout(ctx, 10*time.Second)
		full, err := s.docker.ContainerInspect(inspectCtx, ctr.ID)
		inspectCancel()
		if err != nil {
			log.Printf("workspace-images/refresh: inspect %s failed: %v", ctr.ID[:12], err)
			continue
		}
		imageRef := ""
		if full.Config != nil {
			imageRef = full.Config.Image
		}
		matched := ""
		for rt := range pulledSet {
			if strings.Contains(imageRef, "workspace-template-"+rt) {
				matched = rt
				break
			}
		}
		if matched == "" {
			continue
		}
		name := strings.TrimPrefix(ctr.Names[0], "/")
		rmCtx, rmCancel := context.WithTimeout(ctx, 30*time.Second)
		err = s.docker.ContainerRemove(rmCtx, ctr.ID, container.RemoveOptions{Force: true})
		rmCancel()
		if err != nil {
			log.Printf("workspace-images/refresh: remove %s failed: %v", name, err)
			continue
		}
		res.Recreated = append(res.Recreated, name)
	}
	return res, nil
}

// AdminWorkspaceImagesHandler serves POST /admin/workspace-images/refresh.
//
//	?runtime=claude-code   (optional; default = all runtimes in AllRuntimes)
//	&recreate=true|false   (default true; false = pull only)
//
// The endpoint is available only when MOLECULE_IMAGE_REGISTRY selects a real
// registry. Unset-registry self-hosts use the Gitea clone-and-build path, so a
// remote pull here would update a different image source than provisioning.
//
// Returns JSON {pulled: [...], failed: [...], recreated: [...]}
type AdminWorkspaceImagesHandler struct {
	svc *WorkspaceImageService
}

func NewAdminWorkspaceImagesHandler(docker *dockerclient.Client) *AdminWorkspaceImagesHandler {
	return &AdminWorkspaceImagesHandler{svc: NewWorkspaceImageService(docker)}
}

func (h *AdminWorkspaceImagesHandler) Refresh(c *gin.Context) {
	runtimes := AllRuntimes
	if r := c.Query("runtime"); r != "" {
		found := false
		for _, known := range AllRuntimes {
			if known == r {
				found = true
				break
			}
		}
		if !found {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":          fmt.Sprintf("unknown runtime: %s", r),
				"known_runtimes": AllRuntimes,
			})
			return
		}
		runtimes = []string{r}
	}
	if provisioner.Resolve().Mode != provisioner.RegistryModeSaaS {
		c.JSON(http.StatusConflict, gin.H{
			"error": "workspace image refresh is unavailable in local-build mode; set MOLECULE_IMAGE_REGISTRY to use a registry-backed self-host",
		})
		return
	}
	recreate := c.DefaultQuery("recreate", "true") == "true"

	res, err := h.svc.Refresh(c.Request.Context(), runtimes, recreate)
	authStatus := "anonymous registry pull"
	if registryAuthHeader() != "" {
		authStatus = "configured registry basic auth"
	}
	log.Printf("workspace-images/refresh: pulled=%d failed=%d recreated=%d (%s)",
		len(res.Pulled), len(res.Failed), len(res.Recreated), authStatus)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "partial_result": res})
		return
	}
	c.JSON(http.StatusOK, res)
}

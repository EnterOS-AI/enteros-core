package handlers

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/wirepath"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/envx"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/plugins"
	"github.com/gin-gonic/gin"
)

// Install-layer defaults. Overridable via env for deployments whose
// plugin sources are fast (or slow) enough to warrant different caps.
const (
	defaultInstallBodyMaxBytes = 64 * 1024         // 64 KiB JSON body cap
	defaultInstallFetchTimeout = 5 * time.Minute   // per-fetch deadline
	defaultInstallMaxDirBytes  = 100 * 1024 * 1024 // 100 MiB staged tree
)

// errNoPushTarget is returned by deliverToContainer when there is NO docker-push
// target for the workspace — a molecules-server / local-docker tenant whose
// workspace-server runs INSIDE a container with no docker.sock mounted (#206), so
// h.docker == nil and the instance_id is a container name (not an i-<hex> EC2 id).
//
// The docker-PUSH into the container is RETIRED for these tenants (the operator
// pull-model target): the old path 503'd (or, before that, hung on AWS EIC ->
// 502; core#182). Callers now treat this sentinel as "deliver by PULL instead" —
// they declare the plugin and RE-MATERIALIZE (restart), so the runtime's boot
// materializer (molecule_runtime.plugin_sources) pulls the declared plugins into
// /configs/plugins/<name>/ on the next boot. The AGENT never fetches; the box does.
var errNoPushTarget = errors.New("no docker-push target: deliver by pull (re-materialize)")

// httpErr is the typed error returned by Install helpers. The handler
// matches it with errors.As and emits the attached status + body. Using
// a typed error instead of a 5-value tuple keeps helper signatures Go-
// idiomatic and makes them testable without a gin.Context.
type httpErr struct {
	Status int
	Body   gin.H
}

func (e *httpErr) Error() string {
	return fmt.Sprintf("%d: %v", e.Status, e.Body)
}

// newHTTPErr constructs an *httpErr without the caller worrying about
// pointer receivers. Keeps call sites terse.
func newHTTPErr(status int, body gin.H) *httpErr { return &httpErr{Status: status, Body: body} }

// installLimitsLogOnce gates the single operator-facing log line
// describing the effective install caps + timeout. sync.Once guarantees
// exactly one emission per process lifetime, regardless of how many
// PluginsHandler instances are constructed. Safe to call from any
// goroutine.
var installLimitsLogOnce sync.Once

// logInstallLimitsOnce writes the effective install limits to `w`,
// exactly once per process. Taking the writer as a parameter (instead
// of a package-level var) removes the last piece of mutable global
// state from this file — production passes os.Stderr, tests pass a
// bytes.Buffer with no t.Cleanup dance.
func logInstallLimitsOnce(w io.Writer) {
	installLimitsLogOnce.Do(func() {
		fmt.Fprintf(w,
			"Plugin install limits: body=%d bytes  timeout=%s  staged=%d bytes\n",
			envx.Int64("PLUGIN_INSTALL_BODY_MAX_BYTES", defaultInstallBodyMaxBytes),
			envx.Duration("PLUGIN_INSTALL_FETCH_TIMEOUT", defaultInstallFetchTimeout),
			envx.Int64("PLUGIN_INSTALL_MAX_DIR_BYTES", defaultInstallMaxDirBytes),
		)
	})
}

// validatePluginName ensures the name is safe (no path traversal).
func validatePluginName(name string) error {
	if name == "" {
		return fmt.Errorf("plugin name is required")
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		return fmt.Errorf("invalid plugin name: must not contain path separators or '..'")
	}
	if name != filepath.Base(name) {
		return fmt.Errorf("invalid plugin name")
	}
	return nil
}

// dirSize returns the total bytes of files under dir. Short-circuits
// as soon as the byte limit is exceeded so pathological inputs don't
// run the full walk.
func dirSize(dir string, limit int64) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() {
			total += info.Size()
			if total > limit {
				return fmt.Errorf("staged plugin exceeds cap of %d bytes", limit)
			}
		}
		return nil
	})
	return total, err
}

// advisoryManifestMaxBytes caps how much of a staged plugin.yaml the
// advisory SSOT check will read. Real manifests are a few KiB; anything
// past this is reported as not-validatable rather than read.
const advisoryManifestMaxBytes = 1 << 20 // 1 MiB

// readStagedManifestForAdvisory reads stagedDir/plugin.yaml for the
// advisory SSOT check without trusting staged content: a hostile archive
// could ship plugin.yaml as a symlink (e.g. to /dev/zero or a host file),
// so symlinks and other non-regular files are rejected via Lstat and the
// read is size-capped before it happens.
func readStagedManifestForAdvisory(stagedDir string) ([]byte, error) {
	p := filepath.Join(stagedDir, "plugin.yaml")
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("plugin.yaml is not a regular file (mode %v)", fi.Mode())
	}
	if fi.Size() > advisoryManifestMaxBytes {
		return nil, fmt.Errorf("plugin.yaml is %d bytes, over the %d-byte advisory cap", fi.Size(), advisoryManifestMaxBytes)
	}
	return os.ReadFile(p)
}

// sanitizeSourceForLog strips credential-bearing URL parts (userinfo,
// query) from a plugin source before it reaches a log line — sources can
// be user-provided URLs with embedded tokens.
func sanitizeSourceForLog(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable-source>"
	}
	u.User = nil
	u.RawQuery = ""
	return u.String()
}

// manifestSSOTEnforceDisabledLogOnce gates the single operator-facing
// notice that the SSOT manifest kill-switch is engaged.
var manifestSSOTEnforceDisabledLogOnce sync.Once

// manifestSSOTEnforcementEnabled reports whether install-time SSOT
// manifest validation is fail-closed (core#3383 PR-4). Default ON; the
// operator escape hatch is MOLECULE_MANIFEST_SSOT_ENFORCE=off (case-
// insensitive), which reverts violations to the advisory log line.
// envx.Bool is deliberately not used: its ParseBool vocabulary does not
// accept "off", and the mental model here is "set off to disable", not
// a truthy flag. Read per call — the install path is not hot.
func manifestSSOTEnforcementEnabled() bool {
	if strings.EqualFold(os.Getenv("MOLECULE_MANIFEST_SSOT_ENFORCE"), "off") {
		manifestSSOTEnforceDisabledLogOnce.Do(func() {
			log.Printf("Plugin install: SSOT manifest enforcement DISABLED via MOLECULE_MANIFEST_SSOT_ENFORCE=off — advisory mode")
		})
		return false
	}
	return true
}

// installRequest is the decoded, validated payload a caller submits.
// Held out as its own type so resolveAndStage is testable without a
// gin.Context; the handler just decodes into this shape.
type installRequest struct {
	Source string `json:"source"`
	// SHA256 is an optional hex-encoded SHA-256 of the plugin's plugin.yaml.
	// When present, resolveAndStage verifies the fetched content matches
	// before allowing the install to proceed (SAFE-T1102 supply-chain hardening).
	SHA256 string `json:"sha256,omitempty"`
	// Track is the version-subscription mode for this install (core#113):
	//   "none"        — no auto-update tracking (default)
	//   "tag:vX.Y.Z"  — track a specific version tag
	//   "tag:latest"  — track latest tag, drift on every new tag
	//   "sha:<full>"  — pinned, no drift ever
	// The drift detector (separate component, follow-up) reads
	// workspace_plugins rows where tracked_ref != 'none' and queues
	// updates when upstream resolves to a different SHA.
	Track string `json:"track,omitempty"`
	// Restart controls the post-install auto-restart (self-reprovision,
	// design §5.2). nil or true → existing behavior: the workspace restarts
	// so boot-install re-establishes /configs/plugins from the desired-set
	// (declared ∪ installed). false → deliver + record ONLY; the caller
	// owns triggering the restart later (e.g. batching several installs
	// into one reprovision). A *bool so an absent field is distinguishable
	// from an explicit false — absent must keep the historical default.
	Restart *bool `json:"restart,omitempty"`
}

// stageResult bundles the outputs of resolveAndStage for the caller.
// Avoids a 5-value tuple return.
type stageResult struct {
	StagedDir    string
	PluginName   string
	Source       plugins.Source
	InstalledSHA string // empty for local:// sources (no meaningful upstream)
	// SuppressRestart carries the caller's restart=false request into the
	// delivery step (deliverViaDocker / EIC), which owns the restart
	// decision. Set by Install from installRequest.Restart; never set by
	// resolveAndStage itself, so the reconcile / drift-apply paths keep
	// their existing restart behavior (zero value = restart as before).
	SuppressRestart bool
}

// resolveAndStage parses a validated request, dispatches to the right
// SourceResolver, fetches the plugin into a temp dir, and validates the
// returned name + staged size.
//
// On any error the staging tempdir (if created) is removed before return,
// and the returned *stageResult is nil. Callers own cleanup of
// result.StagedDir on success via defer os.RemoveAll.
func (h *PluginsHandler) resolveAndStage(ctx context.Context, req installRequest) (*stageResult, error) {
	if req.Source == "" {
		return nil, newHTTPErr(http.StatusBadRequest, gin.H{
			"error": "'source' is required (e.g. \"local://my-plugin\" or \"github://owner/repo\")",
		})
	}

	source, err := plugins.ParseSource(req.Source)
	if err != nil {
		return nil, newHTTPErr(http.StatusBadRequest, gin.H{"error": "invalid plugin source"})
	}
	resolver, err := h.sources.Resolve(source)
	if err != nil {
		// F1086 / #1206: include schemes so the caller can self-diagnose
		// the fix, but never the raw error message.
		return nil, newHTTPErr(http.StatusBadRequest, gin.H{
			"error":             "failed to resolve plugin source",
			"available_schemes": h.sources.Schemes(),
		})
	}
	// Front-run obvious input validation for local sources so path-
	// traversal attempts yield 400 rather than a resolver-level 502.
	if source.Scheme == "local" {
		if err := validatePluginName(source.Spec); err != nil {
			return nil, newHTTPErr(http.StatusBadRequest, gin.H{"error": "invalid plugin name"})
		}
	}

	// Pinned-ref enforcement for git-backed sources (SAFE-T1102).
	// An unpinned spec (no #<tag/sha> suffix) installs from a mutable
	// default-branch tip whose content can change silently between an
	// audit and the actual install. Require explicit pinning unless the
	// operator opts in via PLUGIN_ALLOW_UNPINNED=true.
	if (source.Scheme == "github" || source.Scheme == "gitea") && !strings.Contains(source.Spec, "#") {
		if os.Getenv("PLUGIN_ALLOW_UNPINNED") != "true" {
			return nil, newHTTPErr(http.StatusUnprocessableEntity, gin.H{
				"error":  `unpinned plugin source: append a tag or commit SHA (e.g. "` + source.Scheme + `://owner/repo#v1.2.0"). Set PLUGIN_ALLOW_UNPINNED=true to override`,
				"source": source.Raw(),
			})
		}
	}

	stagedDir, err := os.MkdirTemp("", "molecule-plugin-fetch-*")
	if err != nil {
		return nil, newHTTPErr(http.StatusInternalServerError, gin.H{"error": "failed to create staging dir"})
	}
	// From here, we own stagedDir. Every error path below removes it
	// before returning; the caller's defer takes over on success.
	cleanup := func() { _ = os.RemoveAll(stagedDir) }

	pluginName, err := resolver.Fetch(ctx, source.Spec, stagedDir)
	if err != nil {
		cleanup()
		log.Printf("Plugin install: resolver %s failed for %s: %v", source.Scheme, source.Spec, err)
		status := http.StatusBadGateway
		if errors.Is(err, plugins.ErrPluginNotFound) {
			status = http.StatusNotFound
		} else if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		// F1086 / #1206: do NOT interpolate err into the response — a
		// resolver failure (github API rate-limit text, raw HTTP body,
		// file system path from a local-fs resolver) routinely contains
		// internal detail that has no business landing in the user's
		// browser. The status code already differentiates the failure
		// shape (404 not found vs 504 timeout vs 502 generic) for the
		// caller; full detail stays in the log line above.
		return nil, newHTTPErr(status, gin.H{
			"error":  fmt.Sprintf("failed to fetch plugin from %s", source.Scheme),
			"source": source.Raw(),
		})
	}

	// Capture the installed SHA from git-backed sources for drift detection.
	// The resolver sets LastSHA() during Fetch after a successful clone. Both
	// GithubResolver and GiteaResolver expose it; type-assert against a small
	// interface so adding a third git-backed scheme later needs no change here.
	var installedSHA string
	if sha, ok := resolver.(interface{ LastSHA() string }); ok {
		installedSHA = sha.LastSHA()
	}

	if err := validatePluginName(pluginName); err != nil {
		cleanup()
		return nil, newHTTPErr(http.StatusBadRequest, gin.H{
			"error":  "resolver returned invalid plugin name",
			"source": source.Raw(),
		})
	}
	limit := envx.Int64("PLUGIN_INSTALL_MAX_DIR_BYTES", defaultInstallMaxDirBytes)
	if _, err := dirSize(stagedDir, limit); err != nil {
		cleanup()
		return nil, newHTTPErr(http.StatusRequestEntityTooLarge, gin.H{
			"error":  "staged plugin exceeds size limit",
			"source": source.Raw(),
		})
	}

	// Manifest-declared SHA-256 content integrity check.
	// If the staged plugin ships a manifest.json with a "sha256" field, verify
	// the declared hash matches the actual staged tree contents.
	if err := plugins.VerifyManifestIntegrity(stagedDir); err != nil {
		cleanup()
		return nil, newHTTPErr(http.StatusUnprocessableEntity, gin.H{
			"error":  "plugin manifest integrity check failed",
			"source": source.Raw(),
		})
	}

	// Caller-pinned SHA-256 content integrity check (SAFE-T1102).
	// If the caller pinned a hash, verify it against the staged plugin.yaml.
	// A mismatch means the fetched content differs from what was audited —
	// abort rather than silently install an unexpected plugin.
	if req.SHA256 != "" {
		manifestPath := filepath.Join(stagedDir, "plugin.yaml")
		manifestData, readErr := os.ReadFile(manifestPath)
		if readErr != nil {
			cleanup()
			return nil, newHTTPErr(http.StatusUnprocessableEntity, gin.H{
				"error":  "sha256 check failed: plugin.yaml not found in staged plugin",
				"source": source.Raw(),
			})
		}
		sum := sha256.Sum256(manifestData)
		got := hex.EncodeToString(sum[:])
		if !strings.EqualFold(got, req.SHA256) {
			cleanup()
			return nil, newHTTPErr(http.StatusUnprocessableEntity, gin.H{
				"error":  fmt.Sprintf("sha256 mismatch: expected %s, got %s", req.SHA256, got),
				"source": source.Raw(),
			})
		}
	}

	// SSOT manifest validation — FAIL-CLOSED phase of core#3383 (PR-4,
	// post-soak: 25/25 real org manifests validated clean). A present,
	// readable plugin.yaml that violates the SDK-owned plugin-manifest
	// schema now aborts the install with 422, mirroring the
	// VerifyManifestIntegrity posture above; the kill-switch
	// MOLECULE_MANIFEST_SSOT_ENFORCE=off reverts to the advisory line.
	// A MISSING or not-validatable plugin.yaml (absent file, symlink /
	// non-regular, over the read cap) stays ADVISORY: staged trees with
	// no plugin.yaml are legal today. The staged tree is untrusted: the
	// manifest read rejects symlinks / non-regular files and is size-
	// capped, and the source is credential-stripped in BOTH the log line
	// and the 422 body (responses can land in client logs / proxies /
	// support bundles — no new secret-echo path).
	if data, readErr := readStagedManifestForAdvisory(stagedDir); readErr != nil {
		log.Printf("Plugin install: SSOT manifest validation (advisory): plugin=%s source=%s: no validatable plugin.yaml in staged tree: %v",
			pluginName, sanitizeSourceForLog(source.Raw()), readErr)
	} else if v := plugins.ValidateManifestSSOT(data); len(v) > 0 {
		if manifestSSOTEnforcementEnabled() {
			cleanup()
			log.Printf("Plugin install: SSOT manifest validation FAILED (fail-closed): plugin=%s source=%s %d violation(s): %s",
				pluginName, sanitizeSourceForLog(source.Raw()), len(v), strings.Join(v, "; "))
			return nil, newHTTPErr(http.StatusUnprocessableEntity, gin.H{
				"error":      "plugin manifest violates the plugin-manifest SSOT schema (core#3383)",
				"violations": v,
				"source":     sanitizeSourceForLog(source.Raw()),
			})
		}
		log.Printf("Plugin install: SSOT manifest validation (advisory): plugin=%s source=%s %d violation(s): %s",
			pluginName, sanitizeSourceForLog(source.Raw()), len(v), strings.Join(v, "; "))
	}

	return &stageResult{StagedDir: stagedDir, PluginName: pluginName, Source: source, InstalledSHA: installedSHA}, nil
}

// deliverToContainer copies the staged plugin dir into the workspace
// container, chowns it for the agent user, and triggers a restart.
// Returns a typed *httpErr on failure; nil on success.
//
// Dispatch order:
//
//  1. Local Docker container is up (self-host ws-<id>) → tar+CopyToContainer.
//  2. instance_id set → SHAPE-routed (isEC2InstanceID, mirrors
//     files_backend_dispatch.go):
//     2a. real "i-<hex>" EC2 id (AWS SaaS) → push via EIC SSH to the EC2's
//     bind-mounted /configs/plugins/<name>/ (template_files_eic.go).
//     2b. anything else = a local-docker / molecules-server CONTAINER NAME
//     ("mol-ws-<slug>-<hex>", which the CP local-docker provisioner
//     persists into instance_id) → docker CopyToContainer straight into
//     THAT container, when a docker client is wired. NEVER the AWS EIC
//     path (that 90-120s-times-out with no AWS creds → 502; core#182).
//     2c. local-docker id but no docker client wired → fail LOUD (503),
//     never a silent 90s AWS EIC timeout.
//  3. Neither wired → 503. True "no backend" case.
//
// The SaaS branch is gated on h.instanceIDLookup so unit tests can keep
// using NewPluginsHandler without a DB; production wires it in router.go.
// The boolean return reports whether a restart was ACTUALLY scheduled —
// false when the caller suppressed it (restart=false), when the change
// classified as skill-content-only, or when no restartFunc is wired. The
// Install handler echoes it as the response's "restarting" field so a
// SELF-installing agent is never told its session is ending when the
// delivery deliberately skipped the restart.
func (h *PluginsHandler) deliverToContainer(ctx context.Context, workspaceID string, r *stageResult) (bool, error) {
	if containerName := h.findRunningContainer(ctx, workspaceID); containerName != "" {
		return h.deliverViaDocker(ctx, workspaceID, containerName, r)
	}

	if instanceID, runtime := h.lookupSaaSDispatch(workspaceID); instanceID != "" {
		// AWS (EC2-per-workspace) SaaS backend — real "i-<hex>" id → EIC SSH.
		if isEC2InstanceID(instanceID) {
			if err := installPluginViaEIC(ctx, instanceID, runtime, r.PluginName, r.StagedDir); err != nil {
				log.Printf("Plugin install: EIC push failed for %s → %s: %v", r.PluginName, workspaceID, err)
				return false, newHTTPErr(http.StatusBadGateway, gin.H{
					"error": "failed to deliver plugin to workspace EC2",
				})
			}
			if h.restartFunc != nil && !r.SuppressRestart {
				// RFC internal#524 Layer 1: see Docker path above.
				wsID := workspaceID
				globalGoAsync(func() { h.restartFunc(wsID) })
				return true, nil
			}
			return false, nil
		}

		// molecules-server / local-docker backend: instance_id IS the running
		// container name on the local docker daemon. Deliver straight into it
		// via the SAME docker primitive the findRunningContainer branch uses —
		// findRunningContainer looked for "ws-<id>" and never matches the CP's
		// "mol-ws-*" name, so we key delivery on the instance_id container name.
		if h.docker != nil {
			return h.deliverViaDocker(ctx, workspaceID, instanceID, r)
		}

		// Non-EC2 instance id (local-docker) but no docker client wired — the
		// docker-less tenant (#206: no docker.sock mounted). The docker-PUSH is
		// RETIRED here: signal the caller to deliver by PULL (re-materialize) so
		// the runtime's boot materializer pulls the declared plugins. No more 503
		// dead-end, no AWS EIC 90s→502.
		log.Printf("Plugin install: workspace %s is docker-less (instance_id=%s, no docker client) — retiring the docker-push, delivering by pull (re-materialize)", workspaceID, instanceID)
		return false, errNoPushTarget
	}

	// No running container and no instance id — deliver by pull. The boot
	// materializer installs the declared plugins on the next (re-)provision.
	return false, errNoPushTarget
}

// reMaterialize delivers a plugin by PULL (the docker-less path that replaces the
// retired docker-push): it records the plugin in the workspace's DECLARED set
// (workspace_declared_plugins) so the runtime's boot materializer
// (molecule_runtime.plugin_sources) installs it into /configs/plugins/<name>/,
// then triggers a restart so that boot runs. The AGENT never fetches; the box
// pulls its declared plugins itself (runtime-agnostic contract). Returns whether
// a restart was scheduled. recordDeclaredPlugin no-ops when the DB is unset (unit
// tests) and enforces the privileged-plugin kind gate (a user can't declare the
// platform MCP on a non-platform workspace).
func (h *PluginsHandler) reMaterialize(ctx context.Context, workspaceID, pluginName, source string, suppressRestart bool) (bool, error) {
	if err := recordDeclaredPlugin(ctx, workspaceID, pluginName, source); err != nil {
		return false, err
	}
	if suppressRestart {
		log.Printf("Plugin install (pull): %s → workspace %s — restart suppressed by caller (restart=false); boot-install picks it up on the next reprovision", pluginName, workspaceID)
		return false, nil
	}
	if h.restartFunc != nil {
		wsID := workspaceID
		globalGoAsync(func() { h.restartFunc(wsID) })
		return true, nil
	}
	return false, nil
}

// deliverViaDocker copies the staged plugin dir into containerName via docker
// CopyToContainer, chowns it for the agent user (uid 1000), and triggers a
// restart unless the change is skill-content-only or the caller suppressed
// it. Shared by the self-host ws-<id> branch and the molecules-server
// mol-ws-* (instance_id) branch — docker delivery is identical once we know
// which container to write into. The boolean reports whether a restart was
// actually scheduled (see deliverToContainer).
func (h *PluginsHandler) deliverViaDocker(ctx context.Context, workspaceID, containerName string, r *stageResult) (bool, error) {
	// Hot-reload classifier (molecule-core#112) — decide BEFORE the
	// install whether this update can skip restartFunc. SKILL.md
	// content changes are filesystem-visible to Claude Code on the
	// next Skill invocation; hooks / settings.json / plugin.yaml /
	// added-or-removed files need a container restart.
	// Classifier reads live tree from container; on any read error
	// it returns kindCold so we never hot-reload speculatively.
	kind, _ := h.classifyInstallChanges(ctx, containerName, r.StagedDir, r.PluginName)

	// Atomic stage→snapshot→swap→marker (molecule-core#114).
	// Replaces the prior single docker.CopyToContainer write that
	// left a partially-extracted tree on mid-install failure with
	// no rollback path. atomicCopyToContainer writes a .complete
	// marker as the last step; workspace-side plugin loaders should
	// refuse to load a plugin dir without it.
	if err := h.atomicCopyToContainer(ctx, containerName, r.StagedDir, r.PluginName); err != nil {
		log.Printf("Plugin install: failed to copy %s to %s (container %s): %v", r.PluginName, workspaceID, containerName, err)
		return false, newHTTPErr(http.StatusInternalServerError, gin.H{"error": "failed to copy plugin to container"})
	}
	// POST-DELIVERY VERIFICATION: read the manifest back from the live path
	// before declaring success. The atomic pipeline's own error paths cover
	// most failures, but a delivery whose extraction lands in the WRONG
	// place with exit 0 (the 2026-07-19 Windows backslash-tar incident:
	// every step "succeeded" while the plugin materialized as garbage in
	// the container root) is only caught by verifying the artifact where
	// the loader will look for it. Host-OS independent — this guard fires
	// on the FIRST bad install, not on a later reconcile cycle.
	//
	// Two review-driven constraints (2026-07-19):
	//   - Only verify plugin.yaml when the STAGED tree actually shipped one:
	//     manifest-less plugins are legal (stagePlugin's SSOT block — missing
	//     manifest is ADVISORY), so requiring it would 500 every such install.
	//   - Hold the same per-(container,plugin) mutex as atomicCopyToContainer:
	//     a concurrent install of the same plugin has a live-path-absent
	//     window between its SNAPSHOT and SWAP steps; taking the lock means we
	//     verify either before it starts or after it completes, never inside
	//     that window.
	if _, statErr := os.Stat(filepath.Join(r.StagedDir, "plugin.yaml")); statErr == nil {
		verify := func() (string, error) {
			lockKey := containerName + "\x00" + r.PluginName
			mu, _ := atomicInstallLocks.LoadOrStore(lockKey, &sync.Mutex{})
			mu.(*sync.Mutex).Lock()
			defer mu.(*sync.Mutex).Unlock()
			return h.execInContainer(ctx, containerName, []string{
				"cat", "/configs/plugins/" + r.PluginName + "/plugin.yaml",
			})
		}
		if out, verr := verify(); verr != nil || len(out) == 0 {
			log.Printf("Plugin install: post-delivery verification FAILED for %s → %s (container %s): manifest unreadable at live path (err=%v)", r.PluginName, workspaceID, containerName, verr)
			return false, newHTTPErr(http.StatusInternalServerError, gin.H{"error": "plugin delivered but not present at live path — delivery corrupt"})
		}
	}
	h.execAsRoot(ctx, containerName, []string{
		"chown", "-R", "1000:1000", "/configs/plugins/" + r.PluginName,
	})
	if h.restartFunc != nil {
		if r.SuppressRestart {
			log.Printf("Plugin install: %s → workspace %s — restart suppressed by caller (restart=false); boot-install picks it up on the next reprovision", r.PluginName, workspaceID)
		} else if pluginInstallCanSkipRestart(r.PluginName, kind) {
			log.Printf("Plugin install: %s → workspace %s — SKILL-content-only update, SKIPPING restart", r.PluginName, workspaceID)
		} else {
			// RFC internal#524 Layer 1: drain via globalGoAsync (see
			// workspace.go:globalGoAsync).
			wsID := workspaceID
			globalGoAsync(func() { h.restartFunc(wsID) })
			return true, nil
		}
	}
	return false, nil
}

func pluginInstallCanSkipRestart(pluginName, kind string) bool {
	if pluginName == conciergePlatformMCPName {
		return false
	}
	return kind == classifyKindSkillContentOnly
}

// lookupSaaSDispatch returns (instance_id, runtime) for SaaS dispatch, or
// ("", "") when the lookups aren't wired or the workspace isn't on the
// EC2 backend. Errors from the lookups are logged-and-swallowed: failing
// open here just means the caller falls through to the 503 path it would
// have returned without us, never to a wrong action against the wrong
// instance.
func (h *PluginsHandler) lookupSaaSDispatch(workspaceID string) (instanceID, runtime string) {
	if h.instanceIDLookup == nil {
		return "", ""
	}
	id, err := h.instanceIDLookup(workspaceID)
	if err != nil {
		log.Printf("Plugin install: instance_id lookup failed for %s: %v", workspaceID, err)
		return "", ""
	}
	if id == "" {
		return "", ""
	}
	if h.runtimeLookup != nil {
		if rt, rterr := h.runtimeLookup(workspaceID); rterr == nil {
			runtime = rt
		}
	}
	return id, runtime
}

// readPluginSkillsFromContainer reads /configs/plugins/<name>/plugin.yaml
// from the running container and returns the `skills:` list. Returns an
// empty slice if the file is missing or unparseable — uninstall must keep
// running even if the manifest is gone (already half-deleted, etc.).
func (h *PluginsHandler) readPluginSkillsFromContainer(ctx context.Context, containerName, pluginName string) []string {
	out, err := h.execInContainer(ctx, containerName, []string{
		"cat", "/configs/plugins/" + pluginName + "/plugin.yaml",
	})
	if err != nil || len(out) == 0 {
		return nil
	}
	info := parseManifestYAML(pluginName, []byte(out))
	return info.Skills
}

// stripPluginMarkersFromMemory rewrites /configs/CLAUDE.md (the runtime's
// memory file) in-place, removing any block whose marker line starts with
// `# Plugin: <name> /` — mirrors AgentskillsAdaptor.uninstall's stripping
// logic so install/uninstall are symmetric. Best-effort: silent on read or
// write failure, since the rest of uninstall must still succeed.
func (h *PluginsHandler) stripPluginMarkersFromMemory(ctx context.Context, workspaceID, containerName, pluginName string) {
	// Use sed via bash -c for atomic in-place delete: drop the marker line
	// and the blank line that follows it (install adds a leading blank line
	// before the marker via append_to_memory). Three sed passes mirror the
	// install layout: leading blank, marker line, then we also strip empty
	// trailing markers from older installs that didn't add the prefix blank.
	// Falls through silently if CLAUDE.md doesn't exist (fresh workspace).
	marker := "# Plugin: " + pluginName + " /"
	// AgentskillsAdaptor.append_to_memory writes blocks of the shape:
	//   # Plugin: <name> / rule: foo.md
	//   <blank>
	//   <content lines…>
	// separated from the next block by a single blank line. We strip from
	// our marker up to (but not including) the next `# Plugin:` line of
	// any plugin (which marks the boundary), or EOF. Other plugins'
	// blocks and surrounding user content stay intact.
	// Block layout per AgentskillsAdaptor: marker line, one blank, content
	// lines, then a terminating blank (or EOF, or the next plugin's marker).
	// We track blanks-seen-since-marker: the 2nd blank ends our skip; any
	// `# Plugin: ` line also ends our skip (handles back-to-back blocks).
	script := fmt.Sprintf(
		`awk 'BEGIN{skip=0; blanks=0} /^%s/{skip=1; blanks=0; next} skip==1 && /^[[:space:]]*$/{blanks++; if(blanks>=2){skip=0; print; next} next} /^# Plugin: /{if(skip==1)skip=0} skip==1{next} {print}' /configs/CLAUDE.md > /tmp/claude.new && mv /tmp/claude.new /configs/CLAUDE.md`,
		regexpEscapeForAwk(marker),
	)
	if _, awkErr := h.execAsRoot(ctx, containerName, []string{"bash", "-c", script}); awkErr != nil {
		log.Printf("Plugin uninstall: failed to strip markers from CLAUDE.md for %s in %s: %v", pluginName, workspaceID, awkErr)
	}
}

// regexpEscapeForAwk escapes characters that have special meaning inside an
// awk ERE pattern. Plugin names go through validatePluginName so the input
// is already restricted to [A-Za-z0-9_-], but the literal `# Plugin: …/`
// prefix and a future relaxation of validatePluginName both motivate
// escaping defensively.
func regexpEscapeForAwk(s string) string {
	// `/` is the regex delimiter in awk's /.../ syntax — must be escaped
	// alongside the standard regex specials.
	specials := `\^$.|?*+()[]{}/`
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(specials, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// streamDirAsTar writes every regular file + dir under `root` to the tar
// writer, using paths relative to root so the caller's unpack produces
// `<name>/<original-layout>` without any leading tempdir components.
// Symlinks are skipped intentionally — they would usually point outside
// the staged tree and we don't want to expose platform filesystem paths.
func streamDirAsTar(root string, tw *tar.Writer) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil // skip symlinks — see doc comment
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		// Forward slashes always — filepath.Rel yields backslashes on a
		// Windows host and the Linux-side unpack would create flat
		// literal-backslash filenames (same defect as tarWalk; see its
		// comment).
		hdr.Name = wirepath.Normalize(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

// ResolveAndStageForApply is the context-based equivalent of resolveAndStage,
// exposed for the admin plugin drift apply endpoint (core#123). It bypasses
// the gin.Context dependency so the apply path can re-trigger a plugin install
// programmatically.
func (h *PluginsHandler) ResolveAndStageForApply(ctx context.Context, req installRequest) (*stageResult, error) {
	return h.resolveAndStage(ctx, req)
}

// DeliverForApply is the context-based equivalent of deliverToContainer,
// exposed for the admin plugin drift apply endpoint (core#123). The
// restart-scheduled signal is not consumed here — the drift apply path
// manages restarts itself via GetRestartFunc.
func (h *PluginsHandler) DeliverForApply(ctx context.Context, workspaceID string, r *stageResult) error {
	_, err := h.deliverToContainer(ctx, workspaceID, r)
	return err
}

// GetRestartFunc returns the pluginsHandler's restartFunc, or nil if not set.
// Used by the admin drift apply endpoint to trigger a workspace restart after
// a plugin update is applied.
func (h *PluginsHandler) GetRestartFunc() func(string) {
	return h.restartFunc
}

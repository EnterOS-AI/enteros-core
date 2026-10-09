package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provlog"
	"github.com/gin-gonic/gin"
)

// restartState coalesces concurrent RestartByID calls for one workspace.
//
// The naive "one mutex per workspace, TryLock+drop" pattern caused a real
// data-loss bug: SetSecret + SetModel both fire `go restartFunc(...)` from
// the HTTP handler, and both writes commit before either restart goroutine
// gets to load workspace_secrets. If the second goroutine arrives while the
// first holds the mutex, TryLock returns false and the second is silently
// skipped. The first goroutine's loadWorkspaceSecrets ran before the second
// write committed, so the new container boots without that env var. Surfaced
// as the "No LLM provider configured" hermes error when MODEL_PROVIDER landed
// after the API-key write but lost its restart to the mutex.
//
// The fix is the pending-flag / coalescing pattern: any restart request that
// arrives while one is in flight sets the pending flag and returns. The
// in-flight runner, on completion, checks the flag and runs another cycle.
// This collapses N concurrent requests into at most 2 sequential restarts
// (the current one + one more that picks up everything written during it),
// while guaranteeing the final container always sees the latest secrets.
type restartState struct {
	mu      sync.Mutex
	running bool // true while a restart cycle is in flight
	pending bool // set by any caller that arrived during the in-flight cycle
	// restartStartedAt records the wall-clock when the most recent cycle
	// flipped running=true. Used by the self-fire debounce (internal#544,
	// the ws-server self-fire restart feedback loop seen in prod-Reviewer/
	// Researcher 2026-05-19 ~00:05Z 4x reprov thrash): any RestartByID
	// arriving within RestartDebounceWindow of this timestamp is silently
	// dropped so a probe firing during the EC2-pending window can't
	// re-trigger a fresh full cycle on the just-launched instance.
	restartStartedAt time.Time
}

// restartStates is a per-workspace map of *restartState. Each workspace gets
// its own entry so unrelated workspaces don't serialize on each other.
var restartStates sync.Map // map[workspaceID]*restartState

// RestartDebounceWindow is the silent-drop window for successive RestartByID
// calls. Sized to cover the typical EC2 pending → online interval (20-30s)
// with a margin so a probe firing during the just-after-online but still-
// flaky heartbeat window also gets dropped. Bigger than that would block
// legitimate "Restart failed, retry" recoveries; smaller would let the
// 4x thrash class through. Package-level so tests can shrink it.
//
// COUPLING: this window MUST be >= the CP instance reconciler interval
// (cmd/server/main.go:355, currently 60s). If the interval ever shrinks
// below this window, a workspace flipped offline by one reconcile tick can
// be reprovisioned again by the next tick before the debounce drops it,
// reopening the double-reprovision thrash class (internal#544).
var RestartDebounceWindow = 60 * time.Second

// restartByIDDropCounter is incremented every time RestartByID drops a call
// inside the debounce window. Exposed as a package-level atomic counter so
// (a) tests can assert the drop fired, (b) ops can grep logs for the drop
// log line + the counter snapshot in a future /admin/metrics endpoint.
// Not a Prometheus metric because the platform doesn't pull metrics from
// workspace-server yet — that's a separate RFC.
var restartByIDDropCounter atomic.Uint64

// restartProvisionGates is a per-workspace mutex that serializes the
// Stop+Start cycle for a given ws-<id>. Closes the race where manual
// POST /workspaces/:id/restart (RestartWorkspaceAutoOpts) and programmatic
// RestartByID (runRestartCycle) BOTH async-dispatch Stop→Start and
// reached provisioner.Start twice for the same ws-<id>: the first call
// created ws-{id} and started writing tokens, the second call raced in
// and either (a) hit a Docker name conflict and markProvisionFailed
// wedged the workspace to "failed", or (b) silently rotated/wrote
// the bearer a second time and the second container start overlapped
// the first. Both surfaced as the "401 invalid auth token" /
// Docker-name-conflict symptom in #2659 Local Provision Lifecycle stub,
// run 353677/job 478450.
//
// Each workspace gets its own *sync.Mutex on first use (sync.Map
// LoadOrStore is the standard "map of locks" pattern — every workspace
// that ever restarted keeps a tiny mutex in this map for the process
// lifetime; if memory ever becomes a concern we can prune on workspace
// removal, but workspace counts are small enough that it's a non-issue).
//
// The gate is intentionally separate from the existing `restartState`
// (coalesceRestart) and the RestartDebounceWindow self-fire debounce:
//   - restartState coalesces: collapses N rapid concurrent RestartByID
//     calls into ≤2 sequential cycles (the in-flight one + one drain).
//   - RestartDebounceWindow self-fire: drops successive RestartByID
//     calls within 60s of the most recent cycle start (closes the
//     probe-during-EC2-pending self-fire loop).
//   - restartProvisionGate (THIS): the load-bearing exclusion that
//     makes "only ONE Docker create per ws-<id> at a time" hold across
//     the TWO different entry points (manual Restart HTTP + programmatic
//     RestartByID). The existing two gates only cover the RestartByID
//     path; the manual Restart HTTP handler bypassed both and called
//     RestartWorkspaceAutoOpts directly.
//
// Both RestartWorkspaceAutoOpts and runRestartCycle acquire this gate
// around their Stop+Start pair. A concurrent caller blocks (Go mutex
// semantics) until the in-flight cycle completes — so the second
// caller's Stop+Start runs AFTER the first's Start is fully done, and
// the second's provisioner.Start is the only one in flight at a time.
// The HTTP UX cost: a user double-clicking Restart gets a delayed
// response on the second click (the second Stop+Start waits). That's
// strictly better than the pre-fix behavior where the second click
// wedged the workspace to "failed".
var restartProvisionGates sync.Map // map[workspaceID]*sync.Mutex

// acquireRestartProvisionGate returns the per-workspace mutex, creating
// it on first use. Caller MUST defer Unlock after acquisition. The
// mutex is intended to wrap the entire Stop+Start cycle, not just
// provisioner.Start — the race is in the full sequence, not only Start.
func acquireRestartProvisionGate(workspaceID string) *sync.Mutex {
	sv, _ := restartProvisionGates.LoadOrStore(workspaceID, &sync.Mutex{})
	return sv.(*sync.Mutex)
}

// fileWriteRestartDebounceWindow is the per-workspace coalescing window for
// the file-write → RestartByID trigger fired by templates.go's WriteFile,
// DeleteFile, and ReplaceFiles handlers (and template_import.go's variants).
//
// Background (internal#624 2026-05-20): canvas Save fires N PUT /files
// requests in a 30-60s burst (claude-code SEO agent observed 10-17 files in
// 60s). Each successful write previously fired `goAsync(RestartByID)`. The
// 60s self-fire debounce in RestartByID itself catches calls 1-60s, but
// writes at T+65s+ pass the debounce, set pending=true on a still-running
// coalesceRestart cycle, and drain immediately into cycle 2 — which DELETEs
// + recreates EC2 mid-burst, returning 500 EC2InstanceStateInvalidException
// on the in-flight user PUTs.
//
// 15s is sized to absorb a canvas Save burst (writes typically land within
// a 5-10s window) while still letting a deliberate "edit, wait, edit again"
// pattern restart twice. Bigger than that would silently swallow legitimate
// rapid-iteration edits; smaller would let burst tails leak through.
var fileWriteRestartDebounceWindow = 15 * time.Second

// fileWriteRestartLastFireAt records the last time `maybeRestartAfterFileWrite`
// actually fired a restart for each workspace. sync.Map (not RWMutex+map)
// because writes happen on every successful file-write handler, reads on
// every subsequent file-write handler call — both per-workspace — and the
// keys are sparse + long-lived. Stored as int64 unix-nano so the load/store
// path can stay lock-free (atomic.Int64 inside sync.Map.Value is fine, but
// time.Time itself isn't atomically loadable).
var fileWriteRestartLastFireAt sync.Map // map[workspaceID]*atomic.Int64

// fileWriteRestartDropCounter counts how many file-write restart triggers
// were silently coalesced. Same observability rationale as
// restartByIDDropCounter — package-level atomic so tests can assert the
// drop fired and ops can correlate with "user clicked Save 10 times,
// only saw 1 restart cycle".
var fileWriteRestartDropCounter atomic.Uint64

// maybeRestartAfterFileWrite is the call-site debounce wrapper for the 9
// file-write trigger sites in templates.go + template_import.go. Replaces
// the direct `goAsync(func() { wh.RestartByID(wsID) })` pattern with a
// 15s per-workspace coalescing window:
//
//   - First call (no prior fire OR last fire >15s ago): records the
//     current timestamp and fires goAsync(RestartByID).
//   - Subsequent calls within 15s of the last fire: silently dropped,
//     drop counter incremented.
//
// This is the call-site-layer protection (internal#624 Path A). The drain-
// loop layer in coalesceRestart (Path B, re-stamping restartStartedAt per
// iteration) is the platform-layer defense in depth — together they close
// the file-write tight-loop class regardless of which entry point fires.
//
// Stateless on the handler so any handler with access to a WorkspaceHandler
// can use it; the per-workspace state lives in the package-level sync.Map.
func (h *WorkspaceHandler) maybeRestartAfterFileWrite(workspaceID string) {
	now := time.Now().UnixNano()

	// LoadOrStore the per-workspace last-fire stamp. First write for a
	// brand-new workspace falls through the CompareAndSwap below because
	// the zero-init value (0) is far enough in the past to satisfy the
	// "last fire >15s ago" predicate.
	sv, _ := fileWriteRestartLastFireAt.LoadOrStore(workspaceID, new(atomic.Int64))
	stamp := sv.(*atomic.Int64)

	// CAS loop: read last, decide, swap. We use CAS instead of Lock/Unlock
	// because the typical case is "thousands of writes, one restart per
	// 15s" — uncontended atomic is ~5ns vs ~30ns mutex. Bounded retry
	// because in the rare contended case (two writes finishing nanoseconds
	// apart) one will win the swap and the other will see the new stamp,
	// drop, and bail.
	for retry := 0; retry < 4; retry++ {
		last := stamp.Load()
		elapsed := time.Duration(now - last)
		if last != 0 && elapsed < fileWriteRestartDebounceWindow {
			// Within debounce window — drop silently.
			fileWriteRestartDropCounter.Add(1)
			log.Printf("maybeRestartAfterFileWrite: %s — coalesced "+
				"(last fire %s ago < %s window; total dropped=%d)",
				workspaceID, elapsed.Round(time.Millisecond),
				fileWriteRestartDebounceWindow,
				fileWriteRestartDropCounter.Load())
			return
		}
		if stamp.CompareAndSwap(last, now) {
			break
		}
		// Another writer beat us to the stamp update. Re-read and retry;
		// the retry will almost certainly see the new value and drop.
	}

	h.goAsync(func() { h.RestartByID(workspaceID) })
}

// isRestarting reports whether a restart cycle is currently in flight for
// the workspace. Callers that have their own "container looks dead" probe
// MUST consult this before triggering a restart, because during the
// 20-30s EC2-pending window the workspace's url=” and IsRunning()=false
// looks identical to a dead container — and any restart-triggering probe
// (maybeMarkContainerDead from canvas /delegations poll, or the trailing
// restart-context probe at the end of runRestartCycle) will set
// pending=true and the outer coalesceRestart loop will drain by running
// ANOTHER full cycle, ec2_stopped of the just-booted instance →
// re-provision. That's the self-fire loop closed by this gate.
// restartSettleWindow is the post-restart window during which a single
// IsRunning=false probe is NOT trusted. A workspace that just had its
// config.yaml PUT and was restarted can report IsRunning=false while the
// agent is still registering its first heartbeat / settling the container.
// Widening the self-fire guard to cover this window prevents a lone flaky
// probe from clearing the workspace URL and re-triggering a destructive
// restart. core#2929.
const restartSettleWindow = 30 * time.Second

func isRestarting(workspaceID string) bool {
	sv, ok := restartStates.Load(workspaceID)
	if !ok {
		return false
	}
	state := sv.(*restartState)
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.running
}

// inRestartSettleWindow reports whether workspaceID is within
// restartSettleWindow of its most recent restart start. This widens the
// self-fire guard beyond the in-flight restart flag to cover the settle
// window right after a config-PUT restart. core#2929.
func inRestartSettleWindow(workspaceID string) bool {
	sv, ok := restartStates.Load(workspaceID)
	if !ok {
		return false
	}
	state := sv.(*restartState)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.restartStartedAt.IsZero() {
		return false
	}
	return time.Since(state.restartStartedAt) < restartSettleWindow
}

// lastRestartStartedAt returns the timestamp recorded when the most recent
// restart cycle started for workspaceID, if any. Used by the settle-window
// heartbeat freshness check in maybeMarkContainerDead. core#2929.
func lastRestartStartedAt(workspaceID string) (time.Time, bool) {
	sv, ok := restartStates.Load(workspaceID)
	if !ok {
		return time.Time{}, false
	}
	state := sv.(*restartState)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.restartStartedAt.IsZero() {
		return time.Time{}, false
	}
	return state.restartStartedAt, true
}

// isParentPaused checks if any ancestor of the workspace is paused.
func isParentPaused(ctx context.Context, workspaceID string) (bool, string) {
	var parentID *string
	db.DB.QueryRowContext(ctx, `SELECT parent_id FROM workspaces WHERE id = $1`, workspaceID).Scan(&parentID)
	if parentID == nil {
		return false, ""
	}
	var parentStatus, parentName string
	err := db.DB.QueryRowContext(ctx,
		`SELECT status, name FROM workspaces WHERE id = $1`, *parentID,
	).Scan(&parentStatus, &parentName)
	if err != nil {
		return false, ""
	}
	if parentStatus == "paused" {
		return true, parentName
	}
	// Check grandparent recursively
	return isParentPaused(ctx, *parentID)
}

// Restart handles POST /workspaces/:id/restart
// Works for offline, failed, or degraded workspaces. Stops any existing container, then re-provisions.
func (h *WorkspaceHandler) Restart(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()

	var status, wsName, dbRuntime, dbTemplate string
	var tier int
	err := db.DB.QueryRowContext(ctx,
		`SELECT status, name, tier, COALESCE(runtime, 'claude-code'), COALESCE(template, '') FROM workspaces WHERE id = $1`, id,
	).Scan(&status, &wsName, &tier, &dbRuntime, &dbTemplate)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}
	// Block restart if workspace is removed — same 404 as not-found so we don't
	// leak that the row ever existed, and to prevent resurrecting a removed
	// workspace to 'provisioning' before the async runRestartCycle guard fires.
	if status == string(models.StatusRemoved) {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	// Block restart if any ancestor is paused — must resume parent first
	if paused, parentName := isParentPaused(ctx, id); paused {
		c.JSON(http.StatusConflict, gin.H{"error": "parent workspace \"" + parentName + "\" is paused — resume it first"})
		return
	}

	// runtime=external: the workspace has no Docker container or EC2 — its
	// lifecycle is operator-driven (a remote poller heartbeats from outside
	// the platform). Pre-fix, this handler still ran the full re-provision
	// pipeline, which calls issueAndInjectToken → RevokeAllForWorkspace.
	// That silently destroyed the operator's local bearer token on every
	// "Restart" click, leaving them with a 401-spamming poller and no
	// platform-side recovery path short of minting a replacement bearer under
	// Settings → Workspace Tokens. Auto-restart already short-circuits external (see
	// runRestartCycle below). Mirror that here so manual + automatic
	// behavior agree, and surface a clear message instead of silently
	// no-op'ing — the canvas can show the operator that the fix is on
	// their side.
	if isExternalLikeRuntime(dbRuntime) {
		c.JSON(http.StatusOK, gin.H{
			"status":  "noop",
			"runtime": dbRuntime,
			"message": dbRuntime + " workspaces are operator-driven — restart your local agent; platform has nothing to restart",
		})
		return
	}

	// runtime=mock: virtual workspace with canned A2A replies. No
	// container, no provider compute, no provisioning state to recycle. Mirror
	// the external no-op so the canvas's Restart button doesn't
	// silently fail or leak through to the (template-less) provisioner.
	if dbRuntime == "mock" {
		c.JSON(http.StatusOK, gin.H{
			"status":  "noop",
			"runtime": "mock",
			"message": "mock workspaces have no container — restart is a no-op",
		})
		return
	}

	// SaaS mode: cpProv handles provider-managed workspace lifecycle. Self-hosted mode:
	// provisioner handles local Docker containers. At least one must be
	// available — previously only `provisioner` was checked, which broke
	// restart entirely on every SaaS tenant (the workspace compute could not
	// be terminated + relaunched, the endpoint 503'd on every try).
	if !h.HasProvisioner() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "provisioner not available"})
		return
	}

	// Read template from request body before consulting the existing
	// config volume. apply_template means the caller just changed the
	// runtime in the DB and wants the runtime-default template to be
	// authoritative; in that case, reading the old container config would
	// roll the DB runtime back before restart.
	var body struct {
		Template      string `json:"template"`
		ApplyTemplate bool   `json:"apply_template"` // force re-apply runtime-default template (e.g. after runtime change)
		Reset         bool   `json:"reset"`          // #12: discard claude-sessions volume before restart
		RebuildConfig bool   `json:"rebuild_config"` // #239: re-render config volume from org-template source (recovery path when volume was destroyed)
	}
	if err := c.ShouldBindJSON(&body); err != nil && err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}

	// Read runtime from container's config.yaml before stopping. Docker-
	// only: in SaaS mode the workspace runs on remote provider compute and we cannot
	// exec into it, so we trust the DB value (user updates runtime via
	// the Config tab which writes through to both the DB and the container).
	containerRuntime := h.restartRuntimeFromConfig(ctx, id, wsName, dbRuntime, body.ApplyTemplate)

	// Reset to provisioning
	markProvisioningForRestart(ctx, id)
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceProvisioning), id, map[string]interface{}{
		"name":    wsName,
		"tier":    tier,
		"runtime": containerRuntime,
	})

	templatePath, configLabel := resolveRestartTemplate(h.configsDir, wsName, dbRuntime, dbTemplate, restartTemplateInput{
		Template:      body.Template,
		ApplyTemplate: body.ApplyTemplate,
		RebuildConfig: body.RebuildConfig,
	})

	// #239: rebuild_config=true — try org-templates as last-resort source so a
	// workspace with a destroyed config volume can self-recover without admin
	// intervention. Only fires when no other template was resolved above.
	if templatePath == "" && body.RebuildConfig {
		if p, label := resolveOrgTemplate(h.configsDir, wsName); p != "" {
			templatePath = p
			configLabel = label
			log.Printf("Restart: rebuild_config — using org-template %s for %s (%s)", label, wsName, id)
		}
	}

	if templatePath == "" {
		log.Printf("Restart: reusing existing config volume for %s (%s)", wsName, id)
	} else {
		log.Printf("Restart: using template %s for %s (%s)", templatePath, wsName, id)
	}

	var configFiles map[string][]byte
	payload := withStoredCompute(ctx, id, models.CreateWorkspacePayload{Name: wsName, Tier: tier, Runtime: containerRuntime, Template: dbTemplate})
	log.Printf("Restart: workspace %s (%s) runtime=%q template=%q", wsName, id, containerRuntime, dbTemplate)

	// #12: ?reset=true (or body.Reset) discards the claude-sessions volume
	// before restart, giving the agent a clean /root/.claude/sessions dir.
	resetClaudeSession := c.Query("reset") == "true" || body.Reset
	if resetClaudeSession {
		log.Printf("Restart: reset=true — will discard claude-sessions volume for %s (%s)", wsName, id)
	}

	// Capture restart-context data BEFORE provisionWorkspaceOpts flips
	// last_heartbeat_at with the new session. Issue #19 Layer 1.
	restartData := loadRestartContextData(ctx, id)

	// Dispatch through the SoT restart dispatcher. RestartWorkspaceAutoOpts
	// owns "which backend for stop" + "which backend for provision" and
	// keeps the two halves in sync. resetClaudeSession is the one
	// Docker-only per-invocation knob the dispatcher carries through.
	//
	// Stop runs inside the dispatcher's stop leg (synchronous), then the
	// provision leg fires in a goroutine — NOT before the response —
	// because CPProvisioner.Stop is synchronous DELETE /cp/workspaces/:id
	// → CP → AWS EC2 terminate, which can exceed the canvas's 15s default
	// HTTP timeout when the platform has just redeployed (every tenant's
	// CP request queues at once). Pre-fix (2026-04-30) the user saw a
	// misleading "signal timed out" on the canvas even though the
	// restart actually succeeded — caught on hongmingwang hermes
	// workspace 32993ee7-…cb9d75d112a5 right after the heartbeat-fix
	// platform redeploy. context.Background() detaches the dispatch
	// from the request lifecycle so an aborted client connection
	// doesn't cancel the in-flight Stop/provision pair.
	//
	// Pre-2026-05-05 this site inlined the manual if-cpProv-else
	// dispatch with Docker-FIRST ordering (a different drift class from
	// the silent-drop bugs PRs #2811/#2824 closed). RestartWorkspaceAuto
	// enforces CP-FIRST ordering matching the other dispatchers — see
	// docs/architecture/backends.md.
	h.goAsync(func() {
		h.RestartWorkspaceAutoOpts(context.Background(), id, templatePath, configFiles, payload, resetClaudeSession)
	})
	// First-boot-vs-restart arbitration (RFC concierge rule 2): fire
	// restart-context ONLY if this workspace has already been greeted (a real
	// restart). A genuine first boot is skipped — the first-boot greeting owns
	// that edge. Claims the per-restart concurrency gate BEFORE spawning the
	// sender when it does fire (see fireRestartContextIfBooted / the drain-race
	// note in restart_context.go).
	h.fireRestartContextIfBooted(ctx, id, restartData)

	c.JSON(http.StatusOK, gin.H{"status": "provisioning", "config_dir": configLabel, "reset_session": resetClaudeSession})
}

// markProvisioningForRestart moves a workspace into the provisioning state at
// the START of a restart, before anything has been stopped.
//
// It deliberately does NOT clear `url` (core#5025 finding 2). The manual restart
// handler runs this, answers 200, and only then dispatches — so at this point
// nobody knows yet whether a stop will actually happen. The core#5019 pre-flight
// can refuse, in which case the container is never touched and keeps serving on
// exactly the address in that column. Clearing it here made a DECLINED restart
// strictly worse than the outage it prevented: the heartbeat path writes status
// and not url, so the workspace flipped back to 'online' with no address at all
// — healthy-looking and unroutable, with no recovery short of another restart.
//
// The url is cleared where it becomes true — clearWorkspaceRouting, called from
// the stop legs once the container is actually gone.
//
// Extracted so the restart entry points and the tests that pin this share ONE
// statement rather than three copies that can drift a column apart.
func markProvisioningForRestart(ctx context.Context, workspaceID string) {
	if _, err := db.DB.ExecContext(ctx,
		// Clear mcp_unloaded_since so the concierge warming clock (EV2 warm-fail +
		// online not-ready grace, registry.go) starts fresh for this new provisioning
		// episode — a stale stamp from a prior online-degrade would instantly re-degrade
		// the box on its first post-restart beat (core#4457 cluster-2).
		`UPDATE workspaces SET status = $1, mcp_unloaded_since = NULL, updated_at = now() WHERE id = $2`,
		models.StatusProvisioning, workspaceID); err != nil {
		log.Printf("Restart: failed to set provisioning status for %s: %v", workspaceID, err)
	}
}

// clearWorkspaceRouting drops everything that points callers at the container
// that has JUST been destroyed: the persisted url and the cached A2A routing
// keys. Called from the stop legs at the moment the stop has been issued —
// never speculatively, because a workspace that was not stopped is still
// reachable and must keep its address (core#5025 finding 2).
func (h *WorkspaceHandler) clearWorkspaceRouting(ctx context.Context, workspaceID string) {
	if db.DB == nil {
		return
	}
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET url = '', updated_at = now() WHERE id = $1`, workspaceID); err != nil {
		log.Printf("Restart: failed to clear url for %s: %v", workspaceID, err)
	}
	// core#3220: the old container is gone; clear any cached A2A routing keys
	// so concurrent probes do not resolve to the dead URL while the workspace
	// is reprovisioning.
	db.ClearWorkspaceKeys(ctx, workspaceID)
}

// markRestartDeclined records the terminal outcome of a restart the platform
// REFUSED to perform (core#5025 finding 3).
//
// A decline is not "nothing happened": the platform tried to recover a
// workspace and could not obtain the image it would have needed. Before this,
// the auto-restart cycle simply returned — no status write, no event, no row
// change — so an unrecoverable restart was indistinguishable from one that was
// never attempted, and the only trace was a log line on a box nobody tails.
//
// Deliberately NOT markProvisionFailed. Nothing failed to provision, and the
// container is still running and still heartbeating: writing status='failed'
// would misreport a live workspace and is the same class of confident lie as
// finding 2's empty url. The signal is a durable event plus the
// operator-visible error column the canvas already renders — visible, durable,
// and true.
func (h *WorkspaceHandler) markRestartDeclined(ctx context.Context, workspaceID, wsName, runtime, template string) {
	msg := "restart declined — the pinned image for runtime=" + runtime +
		" could not be made available, so the running container was left untouched (core#5019). " +
		"The workspace is still serving its previous version; it will retry on the next restart."

	// Nil-tolerant on both sinks: recording WHY a restart was refused must never
	// itself panic the restart path. A handler wired without a broadcaster (or
	// running before db.DB is set) still gets the log line above the call site.
	if h.broadcaster != nil {
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceRestartDeclined), workspaceID, map[string]interface{}{
			"name":     wsName,
			"runtime":  runtime,
			"template": template,
			"error":    msg,
			"reason":   "image_prewarm_declined",
		})
	}
	if db.DB == nil {
		return
	}

	// Status is deliberately untouched — see the doc comment. last_sample_error
	// is the column the canvas surfaces for "what went wrong here", and it is
	// what makes this distinguishable from a restart nobody ever ran.
	if _, err := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET last_sample_error = $2, updated_at = now() WHERE id = $1`,
		workspaceID, msg); err != nil {
		log.Printf("markRestartDeclined: db update failed for %s: %v", workspaceID, err)
	}
}

func (h *WorkspaceHandler) restartRuntimeFromConfig(ctx context.Context, id, wsName, dbRuntime string, applyTemplate bool) string {
	// The workspaces.runtime DB column is the SSOT for the workspace's runtime
	// — it is the column the runtime-switch PATCH (workspace_crud.go Update)
	// writes, and ONLY that column; the PATCH does NOT write through to the
	// running container's /configs/config.yaml. So on restart we must always
	// re-provision with the DB runtime, never the runtime baked into the
	// container's (possibly stale, template-default) config.yaml.
	//
	// Pre-fix this function let a config.yaml runtime that differed from the DB
	// WIN, and even stomped the DB column back to the config value. That
	// silently reverted a switched runtime (e.g. hermes -> claude-code) on
	// every plain "Restart" click, so a switched-runtime box was never built.
	if h.provisioner == nil || applyTemplate {
		return dbRuntime
	}
	// Best-effort drift detection only: if the container's config.yaml disagrees
	// with the DB, log it (the config volume will be re-rendered from the
	// runtime-default template on re-provision), but the DB value remains
	// authoritative and is never overwritten from the stale config.
	containerName := provisioner.ContainerName(id) // ws-{id} (KI-013 full UUID)
	if cfgBytes, readErr := h.provisioner.ExecRead(ctx, containerName, "/configs/config.yaml"); readErr == nil {
		for _, line := range strings.Split(string(cfgBytes), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "runtime:") {
				parsed := strings.TrimSpace(strings.TrimPrefix(line, "runtime:"))
				if parsed != "" && parsed != dbRuntime {
					log.Printf("Restart: config.yaml runtime %q differs from DB runtime %q for %s — using DB (SSOT); config volume will be re-rendered", parsed, dbRuntime, wsName)
				}
				break
			}
		}
	}
	return dbRuntime
}

// errHibernateNotClaimed reports that the atomic claim matched no row: the
// workspace left online/degraded before the claim landed, or (without ?force=true)
// it had tasks in flight. Nothing was stopped and nothing was written.
var errHibernateNotClaimed = errors.New("workspace not claimable for hibernation")

// Hibernate handles POST /workspaces/:id/hibernate
// Manually puts a running workspace into hibernation — useful for immediate
// cost savings without waiting for the idle timer. The workspace auto-wakes
// on the next incoming A2A message/send.
func (h *WorkspaceHandler) Hibernate(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()

	var wsName string
	var tier, activeTasks int
	err := db.DB.QueryRowContext(ctx,
		`SELECT name, tier, active_tasks FROM workspaces WHERE id = $1 AND status IN ('online', 'degraded')`, id,
	).Scan(&wsName, &tier, &activeTasks)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found or not in a hibernatable state (must be online or degraded)"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	// #822: reject hibernation when active tasks are in flight unless caller
	// passes ?force=true. Prevents operator from accidentally killing a
	// mid-task agent.
	force := c.Query("force") == "true"
	if activeTasks > 0 && !force {
		c.JSON(http.StatusConflict, gin.H{
			"error":        "workspace has active tasks; use ?force=true to terminate them",
			"active_tasks": activeTasks,
		})
		return
	}
	if activeTasks > 0 {
		log.Printf("[WARN] force-hibernating workspace %s (%s) with %d active tasks", id, wsName, activeTasks)
	}

	// Report what actually happened. Pre-fix this called HibernateWorkspace (which
	// returns nothing) and then answered 200 {"status":"hibernated"} unconditionally
	// — so every no-op arm of the atomic claim (active tasks under ?force=true, or a
	// concurrent pause/restart/hibernate that moved the row out of online/degraded)
	// reported success while the container kept running and billing.
	if err := h.hibernateWorkspace(ctx, id, force); err != nil {
		// Only errHibernateNotClaimed means "nothing happened". Every other error
		// is a genuine failure, and at least one of them (a Step 3 UPDATE error)
		// fires AFTER the container has already been stopped — reporting that as a
		// 409 "it left the online state before the claim landed" would be a fresh
		// lie in the opposite direction, and would misdirect whoever debugs it next.
		if !errors.Is(err, errHibernateNotClaimed) {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":        "hibernation failed partway — the workspace may be stopped with its row left mid-transition; re-check its status before retrying",
				"workspace_id": id,
			})
			return
		}
		c.JSON(http.StatusConflict, gin.H{
			"error":        "workspace could not be hibernated — it was no longer online/degraded-and-idle when the claim landed (a concurrent pause, restart, or hibernation, or a heartbeat that raised active_tasks since the eligibility check)",
			"workspace_id": id,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "hibernated"})
}

// HibernateWorkspace stops the container and sets the workspace status to
// 'hibernated'. Called by the hibernation monitor when a workspace has had
// active_tasks == 0 for longer than its configured hibernation_idle_minutes.
// Hibernated workspaces auto-wake on the next incoming A2A message.
//
// TOCTOU safety (#819): the three-step pattern below is atomic at the DB level.
//
//  1. Atomic claim: a single UPDATE WHERE locks the row by transitioning
//     status → 'hibernating', gated on status IN ('online','degraded') AND
//     active_tasks = 0.  If any concurrent caller (another goroutine, the
//     idle-timer, or a manual API call) already claimed the row, or if tasks
//     arrived since the caller decided to hibernate, rowsAffected == 0 and
//     this function returns immediately without stopping anything.
//
//  2. provisioner.Stop: safe to call now because status == 'hibernating';
//     the routing layer rejects new tasks for non-online/degraded workspaces,
//     so no new task can be dispatched between step 1 and step 2.
//
//  3. Final UPDATE to 'hibernated': records the completed hibernation.
func (h *WorkspaceHandler) HibernateWorkspace(ctx context.Context, workspaceID string) {
	// Idle-timer entry point (registry.HibernateHandler). Never forces: the monitor
	// only ever selects rows that are already active_tasks == 0.
	_ = h.hibernateWorkspace(ctx, workspaceID, false)
}

// hibernateWorkspace is HibernateWorkspace with an explicit force flag and a real
// error return.
//
// force=true drops the `active_tasks = 0` predicate from the atomic claim. Without
// that, ?force=true was a lie: the HTTP handler skipped its own 409 and called in,
// but the claim below still demanded active_tasks = 0, matched no row, and returned
// silently — leaving the workspace online and running while the API answered 200
// {"status":"hibernated"}. Caught live on staging (e2e 10b, run 494525): hibernate
// on a just-resumed workspace whose boot turn was still in flight sat at
// status=online for 120s with no error anywhere.
//
// errHibernateNotClaimed lets the caller answer truthfully instead of guessing.
func (h *WorkspaceHandler) hibernateWorkspace(ctx context.Context, workspaceID string, force bool) error {
	// ── Step 1: Atomic claim ──────────────────────────────────────────────────
	// The UPDATE acts as a DB-level advisory lock: only one concurrent caller
	// can transition the row from online/degraded → hibernating.  The
	// active_tasks = 0 predicate ensures we never interrupt a running task —
	// which is exactly what an explicit ?force=true asks us to do, so force
	// drops it (the container stop in Step 2 is what "terminates them" means).
	claim := `
		UPDATE workspaces
		SET    status = $2, updated_at = now()
		WHERE  id = $1
		  AND  status IN ('online', 'degraded')
		  AND  active_tasks = 0`
	if force {
		claim = `
		UPDATE workspaces
		SET    status = $2, updated_at = now()
		WHERE  id = $1
		  AND  status IN ('online', 'degraded')`
	}
	result, err := db.DB.ExecContext(ctx, claim, workspaceID, models.StatusHibernating)
	if err != nil {
		log.Printf("Hibernate: atomic claim failed for %s: %v", workspaceID, err)
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("Hibernate: RowsAffected error for %s: %v", workspaceID, err)
		return err
	}
	if rowsAffected == 0 {
		// Either already hibernating/hibernated/paused/removed, or (non-force)
		// active_tasks > 0 — safe to abort without side-effects.
		return errHibernateNotClaimed
	}

	// Fetch name/tier for logging and event broadcast (after the claim, so we
	// can use a simple SELECT without a status guard).
	var wsName string
	var tier int
	if scanErr := db.DB.QueryRowContext(ctx,
		`SELECT name, tier FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&wsName, &tier); scanErr != nil {
		wsName = workspaceID // fallback for log messages
	}

	// ── Step 2: Stop the container ────────────────────────────────────────────
	// Status is now 'hibernating'; the router rejects new task routing here, so
	// there is no race window between claiming the row and stopping the container.
	//
	// Route through StopWorkspaceAuto — the single source of truth that dispatches
	// to whichever backend is wired (CP for SaaS/molecules-server, Docker for
	// self-hosted). Pre-fix this site inlined `else if h.provisioner != nil { Stop }`,
	// which silently NO-OP'd on a cpProv/molecules-server tenant (h.provisioner==nil):
	// the row flipped to 'hibernated' + url='' while the container KEPT RUNNING and
	// serving A2A — the exact local-docker-vs-EC2 gap the Pause path already closed
	// (see Pause's StopWorkspaceAuto comment) and the same drift class as the
	// Collapse/Delete EC2 leaks (#2813/#2814). Verified live on staging: hibernate
	// left the workspace container "Up" while GET reported status=hibernated.
	// StopWorkspaceAuto returns nil on no-backend (no-op), so Step 3's mark-hibernated
	// + url-clear bookkeeping still fires regardless — matching Pause's fail-open shape.
	log.Printf("Hibernate: stopping container for %s (%s)", wsName, workspaceID)
	if h.stopFnOverride != nil {
		h.stopFnOverride(ctx, workspaceID)
	} else if err := h.StopWorkspaceAuto(ctx, workspaceID); err != nil {
		log.Printf("Hibernate: stop %s failed: %v — orphan sweeper will reconcile", workspaceID, err)
	}

	// ── Step 3: Mark fully hibernated ─────────────────────────────────────────
	// active_tasks is zeroed here, not just on the force path: Step 2 stopped the
	// container, so whatever it was running is gone. Leaving a force-hibernated row
	// at active_tasks > 0 would strand it — the idle monitor only ever selects
	// active_tasks = 0, so the workspace could never be auto-hibernated again after
	// its next wake, silently forfeiting the cost saving hibernation exists for. On
	// the non-force path the claim already proved active_tasks = 0, so this is a no-op.
	if _, err = db.DB.ExecContext(ctx,
		`UPDATE workspaces SET status = $1, url = '', active_tasks = 0, updated_at = now() WHERE id = $2`,
		models.StatusHibernated, workspaceID); err != nil {
		log.Printf("Hibernate: failed to mark hibernated for %s: %v", workspaceID, err)
		return err
	}

	db.ClearWorkspaceKeys(ctx, workspaceID)
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceHibernated), workspaceID, map[string]interface{}{
		"name": wsName,
		"tier": tier,
	})
	log.Printf("Hibernate: workspace %s (%s) is now hibernated", wsName, workspaceID)
	return nil
}

// WakeWorkspace re-provisions a HIBERNATED workspace on the auto-wake-on-A2A
// path (ProxyA2A calls this when an inbound message hits a hibernated ws).
//
// Why this exists separately from RestartByID: a hibernated workspace's
// container is now GENUINELY STOPPED (Hibernate Step 2 routes through
// StopWorkspaceAuto), so waking MUST re-provision a fresh container — it can no
// longer piggy-back on a still-running box. But RestartByID → runRestartCycle
// deliberately SELECTs `status NOT IN ('removed','paused','hibernated')` and
// returns early for dormant states, so the reactive dead-agent auto-restart
// never resurrects a deliberately-parked workspace. Pointing the wake path at
// RestartByID therefore no-op'd: the workspace stayed hibernated forever
// (verified live — A2A logged "waking" + 503 but the box never came back). The
// EXPLICIT A2A wake is a different intent than reactive auto-restart, so it gets
// its own re-provision, mirroring Resume's provision-only relaunch (Resume
// handles 'paused'; this handles 'hibernated').
//
// Atomic claim (status hibernated→provisioning, gated on status='hibernated')
// dedupes concurrent wake A2As: only the first transitions + provisions; the
// rest see rowsAffected=0 and no-op (the caller already returned 503 + retry).
func (h *WorkspaceHandler) WakeWorkspace(workspaceID string) {
	// Symmetric with RestartByID: at least one backend must be wired. On a
	// no-backend handler (unit fixtures) this returns before any DB touch.
	if !h.HasProvisioner() {
		return
	}
	ctx := context.Background()

	// Atomic claim — only one concurrent wake transitions hibernated→provisioning.
	res, err := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET status = $1, url = '', updated_at = now() WHERE id = $2 AND status = 'hibernated'`,
		models.StatusProvisioning, workspaceID)
	if err != nil {
		log.Printf("Wake: claim failed for %s: %v", workspaceID, err)
		return
	}
	if n, raErr := res.RowsAffected(); raErr != nil || n == 0 {
		// Already woken / not hibernated (a concurrent A2A won the claim, or the
		// ws was resumed/removed meanwhile) — nothing to do.
		return
	}

	// Load the stored provision inputs — same shape Resume uses.
	var wsName, dbRuntime, dbTemplate string
	var tier int
	if err := db.DB.QueryRowContext(ctx,
		`SELECT name, tier, COALESCE(runtime, 'claude-code'), COALESCE(template, '') FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&wsName, &tier, &dbRuntime, &dbTemplate); err != nil {
		log.Printf("Wake: load workspace %s failed: %v", workspaceID, err)
		return
	}

	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceProvisioning), workspaceID, map[string]interface{}{
		"name": wsName, "tier": tier, "runtime": dbRuntime, "template": dbTemplate,
	})

	// Carry stored compute + template through so the re-provisioned box
	// re-delivers config.yaml + prompts (identical to Resume's path).
	payload := withStoredCompute(ctx, workspaceID, models.CreateWorkspacePayload{Name: wsName, Tier: tier, Runtime: dbRuntime, Template: dbTemplate})
	if payload.Template == "" && h.cpProv != nil {
		if storedTmpl := storedWorkspaceTemplate(ctx, workspaceID); storedTmpl != "" {
			payload.Template = storedTmpl
		}
	}

	log.Printf("Wake: re-provisioning hibernated workspace %s (%s) runtime=%q template=%q", wsName, workspaceID, dbRuntime, dbTemplate)
	h.provisionWorkspaceAuto(workspaceID, "", nil, payload)
}

// RestartByID restarts a workspace by ID — for programmatic use (e.g.,
// auto-restart after secret change). Calls that arrive while one is in flight
// are coalesced via the pending-flag pattern (see restartState above): the
// in-flight runner picks up the pending request after its current cycle
// completes, so writes that committed mid-restart are guaranteed to land.
func (h *WorkspaceHandler) RestartByID(workspaceID string) {
	h.restartByID(workspaceID, true)
}

// RestartByIDAfterMutation restarts a workspace after the platform has made an
// explicit state change that the running container cannot observe without a
// reprovision, such as plugin delivery into /configs/plugins.
//
// It intentionally bypasses RestartByID's self-fire debounce while preserving
// coalescing and the provision gate. Probe/reactive callers must keep using
// RestartByID so container-health self-fire loops still drop inside the
// debounce window.
func (h *WorkspaceHandler) RestartByIDAfterMutation(workspaceID string) {
	h.restartByID(workspaceID, false)
}

func (h *WorkspaceHandler) restartByID(workspaceID string, useSelfFireDebounce bool) {
	restartByIDWithCycle(workspaceID, h.HasProvisioner(), useSelfFireDebounce, func() {
		h.runRestartCycle(workspaceID)
	})
}

func restartByIDWithCycle(workspaceID string, hasProvisioner bool, useSelfFireDebounce bool, cycle func()) {
	// At least one of the two provisioners must be wired. Pre-fix this
	// short-circuited on h.provisioner==nil alone, which silently disabled
	// reactive auto-restart on every SaaS tenant (where the local Docker
	// provisioner is intentionally nil). The runRestartCycle below now
	// branches on which one is set for the Stop call.
	if !hasProvisioner {
		return
	}
	// Self-fire debounce: drop (not coalesce) successive RestartByID calls
	// within RestartDebounceWindow of the most recent cycle's start. This
	// is the load-bearing protection against the 4x reprov thrash class —
	// coalesceRestart's pending-flag would otherwise drain by running
	// ANOTHER full cycle of stop+provision on the just-launched EC2 (still
	// in the pending state), which is the self-fire we're closing.
	//
	// Only applies to RestartByID (programmatic — secrets handler,
	// maybeMarkContainerDead, preflightContainerHealth). The HTTP Restart
	// handler in workspace_restart.go's Restart() bypasses this path and
	// calls RestartWorkspaceAutoOpts directly, so user-initiated restart
	// clicks are unaffected.
	if useSelfFireDebounce && shouldDebounceRestart(workspaceID) {
		restartByIDDropCounter.Add(1)
		log.Printf("RestartByID: %s — dropped (within %s self-fire debounce window; total dropped=%d)",
			workspaceID, RestartDebounceWindow, restartByIDDropCounter.Load())
		return
	}
	coalesceRestart(workspaceID, cycle)
}

// shouldDebounceRestart reports whether the most recent cycle for this
// workspace started within RestartDebounceWindow. Read-only on
// restartState; the actual restartStartedAt stamp is written in
// coalesceRestart when running flips false→true.
func shouldDebounceRestart(workspaceID string) bool {
	sv, ok := restartStates.Load(workspaceID)
	if !ok {
		return false
	}
	state := sv.(*restartState)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.restartStartedAt.IsZero() {
		return false
	}
	return time.Since(state.restartStartedAt) < RestartDebounceWindow
}

// coalesceRestart implements the pending-flag gate around an arbitrary cycle
// function. Extracted from RestartByID for direct unit testing — the cycle
// function in production is `runRestartCycle`, but tests pass a counter to
// verify the coalescing math (N concurrent requests → ≤2 cycles).
func coalesceRestart(workspaceID string, cycle func()) {
	sv, _ := restartStates.LoadOrStore(workspaceID, &restartState{})
	state := sv.(*restartState)

	// Mark a restart as wanted. If one is already running, return — that
	// runner will see pending=true on its next loop iteration and run
	// another cycle that picks up our request's effects. NOT dropped.
	state.mu.Lock()
	state.pending = true
	if state.running {
		state.mu.Unlock()
		log.Printf("Auto-restart: %s — coalescing with in-flight cycle (pending=true)", workspaceID)
		return
	}
	state.running = true
	// Stamp the start time so the RestartByID debounce can drop any
	// self-fire probe that hits within RestartDebounceWindow. Only the
	// false→true edge stamps; the drain-loop's inner cycles re-use the
	// same start (they're effectively one "restart event" from the
	// debounce's POV).
	state.restartStartedAt = time.Now()
	state.mu.Unlock()

	// Always clear running on exit — including panic — so a panicking
	// cycle (e.g. a future provisionWorkspace nil-deref) doesn't leave
	// the workspace permanently locked out of restarts.
	//
	// recover()-and-DON'T-re-raise on purpose: this runs in a goroutine
	// (callers are `go h.RestartByID(...)`); an unrecovered panic in a
	// goroutine crashes the whole platform process, taking down every
	// workspace served by this binary because of one bug in cycle for
	// one workspace. Log the panic with stack trace for debuggability,
	// then recover and let the goroutine exit cleanly. The next restart
	// request for this workspace will see running=false and proceed.
	defer func() {
		state.mu.Lock()
		state.running = false
		state.mu.Unlock()
		if r := recover(); r != nil {
			log.Printf("Auto-restart: %s — cycle panicked, restart-state cleared: %v\n%s",
				workspaceID, r, debug.Stack())
		}
	}()

	// Drain pending requests. Each iteration re-loads workspace_secrets
	// inside provisionWorkspace, so any writes that committed since the
	// last cycle are picked up. Continues until no pending request was
	// observed at the top of an iteration.
	//
	// internal#624 Path B (defense in depth for the file-write tight-loop
	// class): re-stamp restartStartedAt at the top of every drain iteration
	// past the first. The original design (stamp only on false→true edge)
	// treated all drained pending as "one event from the debounce's POV",
	// which is correct for the secrets-batch use case but lets a file-write
	// burst at T+65s of a 60s drain pipe straight into another full cycle.
	// Re-stamping closes that hole — each drained cycle gets its own fresh
	// debounce window, so any RestartByID arriving during cycle N is
	// dropped by shouldDebounceRestart instead of accumulating into
	// pending=true for cycle N+1.
	//
	// The original "one cycle picks up everyone who arrived during it"
	// semantic still holds for the secrets-write path: callers that hit
	// coalesceRestart during cycle 1 still set pending=true and still get
	// their effects landed in cycle 2. What changes is that callers
	// arriving during cycle 2 (via RestartByID) now hit the re-stamped
	// debounce and are dropped instead of being chained into cycle 3,
	// which is exactly the chain that produced the 22:08-22:10 thrash on
	// 3fe84b89.
	iteration := 0
	for {
		state.mu.Lock()
		if !state.pending {
			state.mu.Unlock()
			return // defer clears running
		}
		state.pending = false
		if iteration > 0 {
			// Re-stamp for drained iterations only; the false→true edge
			// already stamped at the top of coalesceRestart.
			state.restartStartedAt = time.Now()
		}
		state.mu.Unlock()
		iteration++

		cycle()
	}
}

// stopForRestart dispatches Stop to whichever provisioner is wired (Docker or
// control plane, mutually exclusive in production). Docker provisioner.Stop
// kills the local container; CP provisioner.Stop calls
// DELETE /cp/workspaces/:id, which terminates the selected provider compute.
// Pre-fix runRestartCycle only called the
// Docker path, so on SaaS (h.provisioner=nil) the auto-restart cycle silently
// NPE'd before reaching the reprovision step — which is why every SaaS dead-
// agent incident pre-this-fix required manual restart from canvas.
// It takes the PAYLOAD rather than loose runtime/template strings (core#5025):
// the pre-flight below must resolve the same box the provision that follows will
// build, and the payload is that single object. Passing the identity fields
// separately is how the provider drifted away from the provision — the payload's
// Compute.Provider was simply never read, so the wire named no backend at all
// and the control plane resolved its default instead.
func (h *WorkspaceHandler) stopForRestart(ctx context.Context, workspaceID string, payload models.CreateWorkspacePayload) bool {
	backend := "none"
	if h.provisioner != nil {
		backend = "docker"
	} else if h.cpProv != nil {
		backend = "cp"
	}
	// core#5019 (structural half): PULL BEFORE STOPPING. Ask the control plane
	// to make the pinned image obtainable BEFORE destroying the container. A
	// refusal means we keep what the user still has — a running workspace —
	// rather than trading it for an image that may never arrive.
	//
	// Placed above the pre_stop emit on purpose: a declined restart must not
	// leave a restart.pre_stop marker in the wire log for a stop that never
	// happened. Ops read that sequence to reconstruct incidents.
	if !h.ensurePinnedImageBeforeStop(ctx, workspaceID, payload) {
		return false
	}
	provlog.Event("restart.pre_stop", map[string]any{
		"workspace_id": workspaceID,
		"backend":      backend,
	})
	if h.provisioner != nil {
		h.provisioner.Stop(ctx, workspaceID)
	} else if h.cpProv != nil {
		h.cpStopWithRetry(ctx, workspaceID, "Auto-restart")
	}

	// core#3220 + core#5025: the old container is gone, so drop everything that
	// still points at it — the persisted url AND the cached A2A routing keys.
	// Cleared here (rather than in the caller) because this is the earliest
	// point where the backend Stop has been issued and both are guaranteed
	// stale, and the LATEST point at which clearing them is still a lie: on the
	// declined path above we returned without reaching this line, and the
	// workspace kept its address because it kept its container.
	h.clearWorkspaceRouting(ctx, workspaceID)
	return true
}

// restartPrewarmBudget bounds the ENTIRE pre-flight — every retry and every
// backoff between them — for one restart.
//
// It exists because of what the pre-flight HOLDS, not because of what it does.
// Without a deadline the call inherits the ensure-image HTTP client's 20-minute
// budget (deliberately generous: a cold ~7GB pull is the design point), and on
// the auto-restart path that budget is spent holding the per-workspace
// restart/provision gate. A registry that hangs would then look exactly like a
// workspace whose lifecycle has stopped.
//
// It is bounded from BOTH sides, and the lower bound is the load-bearing one:
//
//   - Below provisioner.EnsureImageClientTimeout(), or it bounds nothing — the
//     HTTP client would give up first and the gate would be held for the full
//     client timeout anyway.
//   - Comfortably above the measured cold-pull design point (the 7.05GB pull
//     core#5019 was root-caused on). Too SMALL is not the safe direction it
//     looks like: every restart would decline while a legitimate pull was still
//     in progress, and a workspace could never adopt a newly promoted pin at
//     all — the pre-flight would have turned "slow adoption" into "NO
//     adoption", a worse failure than the one it prevents.
//
// Exceeding it declines, which is the cheap outcome: the container is untouched
// and the next restart cycle asks again, by which time the pull has usually
// landed. Package-level so tests can shrink it.
var restartPrewarmBudget = 15 * time.Minute

// ensurePinnedImageBeforeStop reports whether it is safe to destroy this
// workspace's container — core#5019's structural fix.
//
// A "restart" is a full RE-PROVISION: the control plane reads runtime_image_pins
// at provision time, so adopting a newly promoted pin means obtaining that image
// (~7GB). Pre-fix the tenant stopped first and discovered the image was missing
// second, which is why an unobtainable image cost the workspace its container
// for ~10 minutes. #5020 widened the provision budget so a cold pull usually
// FINISHES; it does nothing for a pull that cannot succeed at all — a bad
// digest, a registry outage, a full disk. This is that half.
//
// Decision table, and which way each branch fails:
//
//	no CP provisioner              -> ALLOW  (self-host/Docker: the local
//	                                  provisioner owns its own image, there is
//	                                  no CP seam to ask and nothing to guard)
//	CP confirms                    -> ALLOW
//	CP has no such endpoint (404)  -> ALLOW  (deliberate fail-OPEN, see below)
//	anything else                  -> DECLINE (fail-CLOSED)
//
// The 404 branch is the single deliberate fail-open. During a rollout a tenant
// can run ahead of its control plane; failing closed there would wedge EVERY
// restart on the fleet, which is a much larger outage than the one being fixed.
// It is also self-limiting: the moment CP ships the endpoint the branch stops
// being reachable. Every other outcome fails closed.
//
// Not a substitute for the CP-side pre-warm on promote — it is the backstop for
// when that has not happened. The two are complementary: pre-warm makes the
// common path fast, this makes the uncommon path non-destructive.
func (h *WorkspaceHandler) ensurePinnedImageBeforeStop(ctx context.Context, workspaceID string, payload models.CreateWorkspacePayload) bool {
	if h.cpProv == nil {
		return true
	}
	runtime, template := payload.Runtime, payload.Template

	// core#5025 finding 7: the pre-flight gets its OWN deadline. It used to run
	// on context.Background() and lean on the 20-minute ensure-image HTTP client
	// timeout, while holding the per-workspace restart/provision gate — so a
	// hung pull froze every restart, heal and provision path for that workspace
	// for twenty minutes. A slow pull is allowed to be slow; it is not allowed
	// to be indistinguishable from a stopped lifecycle. Exceeding the budget
	// DECLINES, which costs the user nothing: the container is still running and
	// the next cycle retries, by which time the pull has usually landed.
	ctx, cancel := context.WithTimeout(ctx, restartPrewarmBudget)
	defer cancel()

	req := provisioner.EnsureImageRequest{
		WorkspaceID: workspaceID,
		Runtime:     runtime,
		Template:    template,
		// core#5025: the provider the PROVISION will use, read off the same
		// payload that provision receives. Left unset, the wire named no
		// backend, the control plane resolved the SSOT default (aws), and it
		// answered "not_applicable" — a 200 — for every workspace on the
		// local-docker substrate. The tenant treats any 2xx as permission to
		// stop, so the guard destroyed containers while logging success.
		Provider: payload.Compute.Provider,
	}

	// core#5025 finding 4: bounded retry, on the SAME budget as the stop leg
	// this pre-flight precedes. cpStopWithRetryErr retries because refusing to
	// act strands the user with a workspace nobody can recover; refusing to
	// pre-warm strands them identically, and harder — it refuses the restart
	// outright. One-shot fail-closed meant a ~60s control-plane redeploy
	// declined every restart on the fleet, and this deployment redeploys the
	// control plane routinely.
	//
	// The two retry knobs are READ from the stop leg rather than re-declared, so
	// the policies cannot drift into two different numbers that both look
	// deliberate.
	var err error
	var res provisioner.EnsureImageResult
	delay := cpStopRetryBaseDelay
	for attempt := 1; attempt <= cpStopRetryAttempts; attempt++ {
		res, err = h.cpProv.EnsureImage(ctx, req)
		if err == nil {
			log.Printf("Restart: %s pinned image ready before stop (runtime=%q template=%q status=%q ref=%q attempt=%d) — core#5019 pull-before-stop",
				workspaceID, runtime, template, res.Status, res.ImageRef, attempt)
			return true
		}
		if errors.Is(err, provisioner.ErrEnsureImageUnsupported) {
			log.Printf("Restart: %s control plane has no ensure-image endpoint — proceeding with the pre-core#5019 stop-then-provision ordering (runtime=%q). Upgrade the control plane to close the cold-adoption window.",
				workspaceID, runtime)
			return true
		}
		// A refusal the control plane MEANT. Retrying returns the same answer
		// and spends the budget doing it — this is the core#5019 bad-digest
		// case, and it must decline fast, not slowly.
		if errors.Is(err, provisioner.ErrEnsureImagePermanent) {
			break
		}
		if attempt == cpStopRetryAttempts {
			break
		}
		log.Printf("Restart: %s pre-warm attempt %d/%d failed (%v) — retrying in %s; a control plane that is momentarily unreachable says nothing about the image",
			workspaceID, attempt, cpStopRetryAttempts, err, delay)
		// Sleep with ctx awareness, exactly as cpStopWithRetryErr does: a
		// budget that only bounds the CALLS and not the waits between them
		// would still hold the caller for the full backoff chain.
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			err = fmt.Errorf("pre-warm budget %s exhausted after %d attempt(s): %w", restartPrewarmBudget, attempt, err)
			return h.declineRestart(ctx, workspaceID, runtime, template, err)
		case <-timer.C:
		}
		delay *= 2
	}

	return h.declineRestart(ctx, workspaceID, runtime, template, err)
}

// declineRestart is the single exit for "we will not destroy this container".
//
// One function rather than a copy at each exit: the budget-exhausted path and
// the attempts-exhausted path must produce the SAME wire log, or an incident
// reconstructed from provlog would show a decline for one cause and silence for
// the other.
//
// LOUD by design. The prefix is stable so ops can grep it, and it is worded to
// be distinguishable from an ordinary provision failure — nothing was broken
// here, something was PREVENTED from breaking.
func (h *WorkspaceHandler) declineRestart(_ context.Context, workspaceID, runtime, template string, err error) bool {
	log.Printf("IMAGE-PREWARM-DECLINED restart workspace_id=%s runtime=%q template=%q err=%q — the workspace was NOT stopped; it keeps its running container (core#5019)",
		workspaceID, runtime, template, err.Error())
	provlog.Event("restart.image_prewarm_declined", map[string]any{
		"workspace_id": workspaceID,
		"runtime":      runtime,
		"template":     template,
		"error":        err.Error(),
	})
	return false
}

// cpStopRetryAttempts caps total Stop attempts (initial + retries). 3 catches
// transient CP/provider hiccups that produce most leaks without slowing
// recovery noticeably
// — worst-case wait is ~7s (1 + 2 + 4 backoff) and we run in a detached
// goroutine, so user UX is unaffected. Package-level so tests can shrink it.
var cpStopRetryAttempts = 3

// cpStopRetryBaseDelay is the first-retry backoff. Doubles each attempt:
// 1s, 2s, 4s for default attempts=3.
var cpStopRetryBaseDelay = 1 * time.Second

// cpStopWithRetry wraps cpProv.Stop with bounded exponential backoff for
// the restart paths. Different policy from workspace_crud.go's Delete:
// Delete returns 500 to the client on Stop failure (loud-fail-and-block,
// since the user asked to destroy and silent leak is unacceptable),
// whereas Restart's contract is "make the workspace alive again" — if we
// refuse to reprovision when Stop fails, we strand the user with a dead
// workspace. So this helper retries to absorb transient failures, then on
// final exhaustion emits a structured `LEAK-SUSPECT` log and returns —
// the caller proceeds with reprovision regardless. The leak signal is
// the bridge to the (forthcoming) CP-side workspace orphan reconciler;
// grep `LEAK-SUSPECT cpProv.Stop` to find affected workspace IDs.
//
// source tags the originating path ("Restart" / "Auto-restart") so the
// log line attributes leaks to the path that produced them.
//
// Returns nothing — caller's contract is unchanged.
func (h *WorkspaceHandler) cpStopWithRetry(ctx context.Context, workspaceID, source string) {
	// Restart's contract is "make the workspace alive again": it proceeds
	// with reprovision regardless of the Stop outcome, so it discards the
	// terminal error. The delete path needs the error (it must keep the
	// row recoverable for the orphan-sweeper + emit a durable event), so
	// the actual retry loop lives in cpStopWithRetryErr below.
	_ = h.cpStopWithRetryErr(ctx, workspaceID, source, false) // restart/hibernate never prunes
}

// cpStopWithRetryErr is the shared bounded-retry core for cpProv.Stop.
// It returns the terminal error so callers that need to react to a leak
// (the DELETE path's stopWorkspaceForDelete) can do so, while
// cpStopWithRetry keeps its void contract for the restart paths.
//
// Behaviour (unchanged from the original cpStopWithRetry loop):
//   - cpProv nil          → nil (no-op; nothing to stop).
//   - success on attempt N → nil; logs a retry-success line when N > 1.
//   - ctx cancelled mid-retry → returns ctx.Err(); logs an "abandoned"
//     line and deliberately does NOT emit LEAK-SUSPECT (operator-initiated
//     drain is a different signal than "we tried hard and failed").
//   - all attempts fail   → returns the LAST attempt's error and emits the
//     stable `LEAK-SUSPECT cpProv.Stop ...` log line so the CP-side orphan
//     reconciler can correlate by workspace_id.
//
// cpStopWithRetryErr terminates the workspace's CP-managed compute with bounded
// retry. prune=true (internal#734) additionally requests CP erase the durable
// data volume — set ONLY by the permanent-delete-with-erase path, NEVER by
// restart/hibernate (those pass false), so a recreate can never prune.
func (h *WorkspaceHandler) cpStopWithRetryErr(ctx context.Context, workspaceID, source string, prune bool) error {
	if h.cpProv == nil {
		return nil
	}
	var lastErr error
	delay := cpStopRetryBaseDelay
	for attempt := 1; attempt <= cpStopRetryAttempts; attempt++ {
		var err error
		if prune {
			err = h.cpProv.StopAndPrune(ctx, workspaceID)
		} else {
			err = h.cpProv.Stop(ctx, workspaceID)
		}
		if err == nil {
			if attempt > 1 {
				log.Printf("%s: cpProv.Stop(%s) succeeded on attempt %d", source, workspaceID, attempt)
			}
			return nil
		}
		lastErr = err
		if attempt == cpStopRetryAttempts {
			break
		}
		// Sleep with ctx awareness so a cancelled ctx exits early instead
		// of stalling the goroutine through the remaining backoff.
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Printf("%s: cpProv.Stop(%s) abandoned mid-retry: ctx cancelled (last_err=%v)",
				source, workspaceID, lastErr)
			return ctx.Err()
		case <-timer.C:
		}
		delay *= 2
	}
	// Exhausted. Loud-flag: stable prefix `LEAK-SUSPECT` + key=value pairs
	// so logs are greppable / parseable for the CP-side orphan reconciler.
	log.Printf("LEAK-SUSPECT cpProv.Stop workspace_id=%s source=%s attempts=%d last_err=%q",
		workspaceID, source, cpStopRetryAttempts, lastErr.Error())
	return lastErr
}

// isLiveWarmingPlatformConcierge is the pure decision for the core#3082 warming
// guard in runRestartCycle: refuse to auto-restart a kind=platform concierge
// that is ALREADY warming (status=provisioning with a LIVE container). Returns
// false — allow the restart — for the FIRST provision (no container running
// yet: running=false, so the concierge can boot), for non-platform workspaces,
// for any non-provisioning status, and (fail-open) on an IsRunning probe error.
func isLiveWarmingPlatformConcierge(kind, status string, running bool, probeErr error) bool {
	return probeErr == nil &&
		running &&
		kind == models.KindPlatform &&
		status == string(models.StatusProvisioning)
}

// workspaceIsRunning probes whichever lifecycle backend is active. Production
// wires exactly one backend, but prefer the local provisioner if a test fixture
// supplies both, matching the existing A2A health-probe behavior.
func (h *WorkspaceHandler) workspaceIsRunning(ctx context.Context, workspaceID string) (bool, error) {
	if h.provisioner != nil {
		return h.provisioner.IsRunning(ctx, workspaceID)
	}
	if h.cpProv != nil {
		return h.cpProv.IsRunning(ctx, workspaceID)
	}
	return false, nil
}

// runRestartCycle does the actual stop+provision work for one restart
// iteration. Synchronous (waits for provisionWorkspace to complete) so the
// outer pending-flag loop in RestartByID can correctly coalesce — if this
// returned before the new container was up, the loop would race the
// in-progress provision goroutine on the next iteration's Stop call.
//
// The cycle is wrapped in the per-workspace restart/provision GATE
// (acquireRestartProvisionGate) so concurrent programmatic RestartByID
// calls and the manual HTTP Restart handler (RestartWorkspaceAutoOpts)
// cannot overlap their provisioner.Start calls for the same ws-<id>.
// The outer coalesceRestart pending-flag already serializes N
// programmatic RestartByID calls into ≤2 sequential cycles; this gate
// closes the second class of race (manual + programmatic, or two
// distinct programmatic entry points firing near-simultaneously) that
// the pending-flag didn't cover.
func (h *WorkspaceHandler) runRestartCycle(workspaceID string) {
	ctx := context.Background()

	// Per-workspace restart/provision gate. The same gate is acquired
	// in RestartWorkspaceAutoOpts (the manual HTTP Restart path), so
	// the manual and programmatic paths mutually exclude on Stop+Start.
	// Held for the entire cycle (Stop → provision); the coalesceRestart
	// drain loop's inner cycles re-use the same gate because the
	// outer cycle holds it across the drain.
	gate := acquireRestartProvisionGate(workspaceID)
	gate.Lock()
	defer gate.Unlock()

	var wsName, status, dbRuntime, dbTemplate, dbKind string
	var tier int
	err := db.DB.QueryRowContext(ctx,
		`SELECT name, status, tier, COALESCE(runtime, 'claude-code'), COALESCE(template, ''), COALESCE(kind, 'workspace') FROM workspaces WHERE id = $1 AND status NOT IN ('removed', 'paused', 'hibernated')`, workspaceID,
	).Scan(&wsName, &status, &tier, &dbRuntime, &dbTemplate, &dbKind)
	if err != nil {
		return // includes paused/hibernated — don't auto-restart those
	}

	// Don't auto-restart external workspaces (no Docker container)
	// or mock workspaces (no container, every reply is canned —
	// see workspace-server/internal/handlers/mock_runtime.go).
	if isExternalLikeRuntime(dbRuntime) || dbRuntime == "mock" {
		return
	}

	// Don't auto-restart if any ancestor is paused
	if paused, _ := isParentPaused(ctx, workspaceID); paused {
		return
	}

	// core#3082 WARMING GUARD — the runtime-agnostic chokepoint that makes a
	// platform concierge actually reach `online`. A kind=platform concierge that
	// is ALREADY warming (status=provisioning with a LIVE container) must NOT be
	// auto-restarted: a restart interrupts its boot, and because the
	// restart-trigger paths (plugin-reconcile, coalesceRestart's pending-drain,
	// the trailing restart-context probe) re-fire on a cadence SHORTER than a
	// slow runtime's cold-boot (hermes ~60–90s vs the ~35s restart cadence), it
	// spins a SELF-PERPETUATING restart loop the concierge never escapes — it
	// never reaches the online flip. This single guard neutralizes EVERY
	// restart-trigger path at once (superseding the per-reconcile
	// SuppressRestart guard, which remains as defense-in-depth).
	//
	// Warming is then bounded by existing lifecycle signals, never a wall-clock
	// restart:
	//   * READY  → the verified-ready gate (registry.go Heartbeat) flips it
	//     online the instant a heartbeat reports provision_workspace loaded.
	//   * TIMEOUT → the provisioning timeout sweep marks a concierge failed
	//     when it does not become ready within the runtime-specific deadline.
	//
	// The FIRST provision is deliberately NOT guarded: no container is running
	// yet, so IsRunning=false and we fall through to create it — that is how the
	// concierge boots. Only a re-provision of an already-live warming concierge
	// is refused. Fail-open on a probe error (proceed as before) so a backend
	// blip never wedges a genuinely-needed restart.
	if status == string(models.StatusProvisioning) && dbKind == models.KindPlatform && h.HasProvisioner() {
		running, runErr := h.workspaceIsRunning(ctx, workspaceID)
		if isLiveWarmingPlatformConcierge(dbKind, status, running, runErr) {
			log.Printf("Auto-restart: SKIPPING %s (%s) — kind=platform still WARMING (provisioning, container live); not resetting the boot. Warming ends on the verified-ready gate or the provisioning timeout (core#3082).", wsName, workspaceID)
			return
		}
	}

	// If still provisioning, brief wait so container exists for Stop()
	if status == "provisioning" {
		log.Printf("Auto-restart: interrupting provisioning for %s (%s)", wsName, workspaceID)
		time.Sleep(10 * time.Second)
	}

	log.Printf("Auto-restart: restarting %s (%s) runtime=%q template=%q (was: %s)", wsName, workspaceID, dbRuntime, dbTemplate, status)

	// #125 Phase 1: send pre-restart drain signal to the workspace agent.
	// For native_session targets, A2A messages go directly to the SDK session
	// and bypass the platform's a2a_queue buffering. If the container dies
	// mid-request, those messages are lost. The pre-restart signal gives the
	// SDK a chance to drain in-flight work before the container stops.
	//
	// Fire-and-forget: gracefulPreRestart runs in a detached goroutine with its
	// own 10s timeout. If the workspace doesn't implement the handler (404) or
	// times out, we proceed with the stop anyway — identical to the pre-fix
	// behaviour.
	h.gracefulPreRestart(ctx, workspaceID)

	// Payload built BEFORE the stop (core#5025), not after it. The pre-flight
	// below has to name the box the provision will build, and that box is
	// payload.Compute.Provider — which this function used to resolve only after
	// the container was already gone. Constructing it here is what makes the two
	// halves provably the same question.
	payload := withStoredCompute(ctx, workspaceID, models.CreateWorkspacePayload{Name: wsName, Tier: tier, Runtime: dbRuntime, Template: dbTemplate})

	// core#5019: stopForRestart DECLINES when the pinned image cannot be made
	// obtainable. The container is still running and still heartbeating, so the
	// workspace stays exactly as the user left it — but a decline is not a
	// no-op either (core#5025 finding 3): it is an unrecoverable restart, and
	// leaving no status behind makes it indistinguishable from "nothing was
	// tried". markRestartDeclined records the terminal, operator-visible
	// outcome without pretending a provision failed.
	if !h.stopForRestart(ctx, workspaceID, payload) {
		log.Printf("Auto-restart: %s (%s) DECLINED — pinned image for runtime=%q template=%q could not be made available; the running container was left untouched (core#5019)",
			wsName, workspaceID, dbRuntime, dbTemplate)
		h.markRestartDeclined(ctx, workspaceID, wsName, dbRuntime, dbTemplate)
		return
	}

	if _, err := db.DB.ExecContext(ctx,
		// Clear mcp_unloaded_since — fresh warming clock for the new provisioning
		// episode (core#4457 cluster-2; see Restart path above).
		`UPDATE workspaces SET status = $1, url = '', mcp_unloaded_since = NULL, updated_at = now() WHERE id = $2`, models.StatusProvisioning, workspaceID); err != nil {
		log.Printf("Auto-restart: failed to set provisioning status for %s: %v", workspaceID, err)
	}
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceProvisioning), workspaceID, map[string]interface{}{
		"name": wsName, "tier": tier, "runtime": dbRuntime, "template": dbTemplate,
	})

	// RFC#2843 #33 + SaaS restart re-stub fix: restore the persisted template
	// and resolve its local template dir so the reprovision request carries both
	// current delivery representations: fetched assets in `template_assets` and
	// the compatibility copy in `config_files`. The pinned provision-request
	// contract marks both fields CP-consumed; keeping the local path here also
	// preserves compatibility across mixed-version rollouts.
	//
	// Historically this restart path passed templatePath="" and omitted the
	// persisted template, so a fresh provider instance could land on the stub
	// config. Re-applying only touches template-owned, allowlisted files
	// (config.yaml, prompts/**); user/agent paths (CLAUDE.md, MEMORY.md,
	// .claude/**, /workspace) are excluded by IsCPTemplateAssetPath, so a
	// persisted /configs is never clobbered. Docker keeps templatePath="" to
	// preserve its persistent config volume.
	restartTemplatePath := ""
	if h.cpProv != nil {
		if storedTmpl := storedWorkspaceTemplate(ctx, workspaceID); storedTmpl != "" {
			payload.Template = storedTmpl
			if p, resolveErr := resolveWorkspaceTemplatePath(h.configsDir, h.cacheDir, storedTmpl); resolveErr != nil {
				log.Printf("Auto-restart: %s template %q path resolution failed: %v — re-provision may land a stub config", workspaceID, storedTmpl, resolveErr)
			} else if _, statErr := os.Stat(p); statErr == nil {
				restartTemplatePath = p
			} else {
				log.Printf("Auto-restart: %s template %q dir absent at %q — re-provision may land a stub config", workspaceID, storedTmpl, p)
			}
		}
	}

	// RFC#2843 #33: TemplateIdentity is derived from payload.Template in
	// workspace_provision.go. Docker keeps its persistent config volume, so it
	// retains the "do not re-apply templates" behavior (template left empty).
	if h.cpProv != nil {
		if storedTmpl := storedWorkspaceTemplate(ctx, workspaceID); storedTmpl != "" {
			payload.Template = storedTmpl
		}
	}

	// Snapshot restart-context data before the new session overwrites
	// last_heartbeat_at. Issue #19 Layer 1.
	restartData := loadRestartContextData(ctx, workspaceID)

	// On auto-restart, do NOT re-apply templates — preserve existing config volume.
	// provisionWorkspaceAutoSync is the SYNCHRONOUS dispatcher (mirrors
	// provisionWorkspaceAuto but blocks instead of spawning a goroutine):
	// returns when the new container is up (or has failed). The outer
	// pending-flag loop in RestartByID relies on this to know when it's
	// safe to start another restart cycle without racing this one's
	// Stop call.
	//
	// core#2771: call the *Locked* variant — the per-workspace provision
	// gate is ALREADY held by this cycle (acquired at the top of
	// runRestartCycle, held for the entire Stop+Start). Calling the
	// unlocked variant would re-lock the same non-reentrant sync.Mutex
	// and deadlock the programmatic restart path.
	//
	// Pre-2026-05-05 this site inlined the if-cpProv-else dispatch. On
	// SaaS the cycle would NPE inside provisionWorkspace's
	// `h.provisioner.VolumeHasFile` call, get swallowed by
	// coalesceRestart's recover()-without-re-raise (a platform-stability
	// safeguard), and leave the workspace permanently stuck in
	// status='provisioning' (the UPDATE above already ran). User-
	// observable result on SaaS pre-fix: dead workspace → manual canvas
	// restart was the only recovery path.
	h.provisionWorkspaceAutoSyncLocked(workspaceID, restartTemplatePath, nil, payload)
	// sendRestartContext is a one-way notification to the new container; safe
	// to fire async — the next restart cycle won't depend on it completing.
	// Tracked via h.goAsync so tests can wait for it via h.asyncWG before
	// closing the sqlmock. Without this, untracked goroutines hit the restored
	// mock and cause "was not expected" errors in parallel CI execution (mc#1264).
	//
	// First-boot-vs-restart arbitration (RFC concierge rule 2): fire only for an
	// already-greeted workspace (has_greeted marker) — a genuine first boot is
	// skipped, the greeting owns that edge. Same session-claim ordering as the
	// Restart handler: fireRestartContextIfBooted sets the per-restart gate
	// before the spawn so the drain woken by this restart's first heartbeat
	// already sees it.
	h.fireRestartContextIfBooted(ctx, workspaceID, restartData)
}

// pauseSideEffectBudget is the HARD ceiling on Pause's detached side-effect
// phase — TOTAL for the whole cascade, not per workspace.
//
// Why a ceiling is mandatory: the request context used to be the only thing
// bounding this work. Detaching from it (see Pause) removes that bound, and a
// detached operation with no deadline is a different bug — a wedged CP Stop
// would pin the handler goroutine and its DB connections forever. The context
// built from this budget is threaded into StopWorkspaceAuto (which carries it
// into the CP HTTP round-trip) and into every ExecContext below, so the deadline
// is enforced by the transports themselves, not by a convention.
//
// TOTAL, not per-workspace, for the same reason CascadeDelete's
// captureCarryoverBudget is total (workspace_crud.go): a per-workspace timeout
// lets an N-descendant cascade run for N × budget, which is not a bound. When
// the budget is spent mid-cascade the remaining workspaces fast-fail on the
// expired context and land in the failure list — a truthful partial result
// rather than an unbounded handler.
//
// Sizing: the relevant neighbour is provisioner.cpAPITimeout = 120s, the
// http.Client timeout on the CP's small JSON calls — "status, stop, console",
// i.e. literally the Stop this budget has to contain. 60s sits below it
// deliberately: a single wedged Stop should surface as a reported failure on
// this budget rather than run the CP client all the way to its own ceiling and
// take the rest of the cascade with it. (An earlier draft of this comment cited
// the stop RETRY envelope; that is cpStopWithRetry on the restart/delete paths
// and is not on the Pause path at all.) A var, not a const, so tests can shrink it.
var pauseSideEffectBudget = 60 * time.Second

// errPauseClaimMissed reports that the guarded mark-paused UPDATE matched no
// row: the workspace left the pausable set between the eligibility SELECT and
// the claim (removed, or already paused by a concurrent caller). The container
// was stopped, but this Pause is not the operation that parked the row, so it
// must not report the row as its own work.
var errPauseClaimMissed = errors.New("mark-paused claim matched no row — the workspace left the pausable set (removed, or paused by a concurrent caller) after the eligibility check")

// Pause handles POST /workspaces/:id/pause
// Stops the container and sets status to 'paused'. The workspace remains on the canvas
// but won't receive heartbeats, won't be auto-restarted, and won't consume resources.
// Config volume is preserved — resume will re-provision with the same config.
func (h *WorkspaceHandler) Pause(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()

	var status, wsName string
	err := db.DB.QueryRowContext(ctx,
		`SELECT status, name FROM workspaces WHERE id = $1 AND status NOT IN ('removed', 'paused')`, id,
	).Scan(&status, &wsName)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found or already paused"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	// Collect this workspace + all descendants to pause
	toPause := []struct{ id, name string }{{id, wsName}}
	var descendantList []gin.H
	rows, err := db.DB.QueryContext(ctx,
		`WITH RECURSIVE descendants AS (
			SELECT id, name FROM workspaces WHERE parent_id = $1 AND status NOT IN ('removed', 'paused')
			UNION ALL
			SELECT w.id, w.name FROM workspaces w JOIN descendants d ON w.parent_id = d.id WHERE w.status NOT IN ('removed', 'paused')
		) SELECT id, name FROM descendants`, id)
	if err != nil {
		log.Printf("Pause: descendant query failed for %s: %v", id, err)
	}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var cid, cname string
			if rows.Scan(&cid, &cname) == nil {
				toPause = append(toPause, struct{ id, name string }{cid, cname})
				descendantList = append(descendantList, gin.H{"id": cid, "name": cname})
			}
		}
		if err := rows.Err(); err != nil {
			log.Printf("Pause: descendant query rows.Err: %v", err)
		}
	}

	// Default: single-workscope pause unless ?cascade=true
	if c.Query("cascade") != "true" && len(descendantList) > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error":       "workspace has descendants — use ?cascade=true to pause all",
			"descendants": descendantList,
		})
		return
	}

	// ── Detach the side effects from the request lifecycle ────────────────────
	//
	// Everything above this line is a READ: it may safely die with the request,
	// because a dead read produces an honest 404/500 and changes nothing.
	// Everything below MUTATES, and must not be cancellable by the client.
	//
	// Pre-fix this whole loop ran on `ctx` — the request context — so an aborted
	// client connection cancelled the stop, the status write, the key clear and
	// the event, while the handler still answered 200 {"status":"paused"}.
	// Observed live on a tenant (core#5019-adjacent, prod exposure is the ~20s
	// CP-recreate window the deploy pipeline opens ~5×/day):
	//
	//	Pause: stop 35cf77b2-… failed: cp provisioner: stop: … context canceled
	//	Pause: failed to set paused status for 35cf77b2-…: context canceled
	//	RecordAndBroadcast: insert event error: context canceled
	//	[GIN] 200 | 4.21s | POST "/workspaces/35cf77b2-…/pause"
	//
	// …and eleven seconds later that workspace read status="online" routable=true.
	// Restart hit this hazard first and documented it (see the goAsync +
	// context.Background() dispatch above: "detaches the dispatch from the request
	// lifecycle so an aborted client connection doesn't cancel the in-flight
	// Stop/provision pair").
	//
	// Pause takes the OTHER documented shape — context.WithoutCancel, SYNCHRONOUS,
	// no goAsync — matching the a2a ingest writes (a2a_proxy_helpers.go: "a client
	// disconnect … must not lose the row; SYNCHRONOUS (no goAsync): the row must be
	// durable before the 200"). The distinction is what the response CLAIMS:
	// Restart answers {"status":"provisioning"}, an honest "queued", so
	// backgrounding it stays truthful. Pause answers {"status":"paused"}, a claim
	// about a COMPLETED state — moving the work behind goAsync would leave the body
	// reading "done" while meaning "queued", re-introducing the same lie through
	// the other door. Synchronous + WithoutCancel gives the only combination where
	// a 200 means what it says: the client may vanish, the work still completes,
	// and the status line below is computed from what actually happened.
	//
	// pauseSideEffectBudget is the ceiling this detach must not exceed.
	opCtx, cancelOp := context.WithTimeout(context.WithoutCancel(ctx), pauseSideEffectBudget)
	defer cancelOp()

	// StopWorkspaceAuto routes to whichever backend is wired (CP for SaaS, Docker
	// for self-hosted) — pre-2026-05-05 this site inlined `if h.provisioner != nil
	// { Stop }`, which silently leaked EC2s on every SaaS Pause (same drift class
	// as the team-collapse leak #2813 and the workspace-delete leak #2814, both
	// closed by PR #2824). StopWorkspaceAuto returns nil on no-backend (no-op), so
	// the Pause bookkeeping still fires on a misconfigured deployment exactly as
	// before — a no-op is not a failure.
	//
	// What CHANGED, beyond the context: a failed stop no longer writes
	// status='paused' anyway. The old code did, under the comment "orphan sweeper
	// will reconcile" — which no sweeper does. Precisely:
	//
	//   - registry/cp_orphan_sweeper.go (the SaaS/CP reaper) selects ONLY
	//     `status = 'removed'`.
	//   - registry/orphan_sweeper.go's container-reaping passes likewise key on
	//     `status = 'removed'`. Its one pass with a wider predicate
	//     (`status NOT IN ('removed','provisioning')`, which does include
	//     'paused') is the STALE-TOKEN REVOCATION pass — it revokes auth tokens,
	//     it never stops compute, so it could not reconcile this even in
	//     principle.
	//   - and that whole file is moot on the plane that matters: StartOrphanSweeper
	//     is wired under `if prov != nil` (cmd/server/main.go), so in CP/SaaS prod
	//     it does not run at all.
	//
	// So a 'paused' row over a still-running container has no backstop, and would
	// sit there forever, invisible. The truthful row is the one we leave untouched
	// (it still says online, which it is), and the caller's retry — now that the
	// caller is TOLD — is the recovery path. This is the same
	// loud-fail-instead-of-silent-leak choice the delete path makes
	// (workspace_crud.go), minus the false row, because delete's 'removed' write is
	// what its sweeper keys on and 'paused' is not.
	var failures []gin.H
	pausedCount := 0
	for _, ws := range toPause {
		note := func(stage string, err error) {
			log.Printf("Pause: %s failed for %s (%s): %v", stage, ws.name, ws.id, err)
			failures = append(failures, gin.H{
				"workspace_id": ws.id,
				"name":         ws.name,
				"stage":        stage,
				"error":        err.Error(),
			})
		}

		if err := h.StopWorkspaceAuto(opCtx, ws.id); err != nil {
			note("stop", err)
			continue // compute is still running — do not claim it is paused
		}

		// Guarded claim. Pre-fix this was `WHERE id = $2` with rowsAffected never
		// read, so a row that left the pausable set between the eligibility SELECT
		// and this write was silently reported as paused. The predicate + the
		// rowsAffected check turn that no-op into a reported failure — the same
		// property Hibernate's atomic claim buys, without inventing a 'pausing'
		// status (see the PR body for why that is deliberately out of scope).
		res, err := db.DB.ExecContext(opCtx,
			`UPDATE workspaces SET status = $1, url = '', updated_at = now() WHERE id = $2 AND status NOT IN ('removed', 'paused')`,
			models.StatusPaused, ws.id)
		// stage "mark_paused" = the container IS stopped and the row still says
		// online. That state does NOT sit still, and a reader who assumes it does
		// will predict the wrong behaviour: the row is now a stopped box claiming
		// to be online, which is exactly what the liveness/health sweeps hunt.
		// StartHealthSweep and StartLivenessMonitor (neither gated on prov, so
		// both run in CP/SaaS) and StartCPInstanceReconciler all call
		// onWorkspaceOffline, which ends in `go wh.RestartByID(workspaceID)` —
		// and RestartByID's dormant-state guard skips 'paused'/'hibernated' but
		// NOT 'online', so it proceeds. The platform therefore AUTO-REPROVISIONS
		// the workspace within a sweep interval. The user asked for a pause and
		// gets a running workspace back; that is a self-healing outcome rather
		// than a stranded row, but it is emphatically not "paused", which is why
		// this is a reported failure and not a silent continue.
		if err != nil {
			note("mark_paused", err)
			continue
		}
		claimed, err := res.RowsAffected()
		if err != nil {
			note("mark_paused", err)
			continue
		}
		if claimed == 0 {
			note("claim", errPauseClaimMissed)
			continue
		}

		// From here the workspace IS paused: stopped, and the row says so. A
		// failure below is real and reportable, but it does not un-pause anything,
		// so it counts toward pausedCount AND toward failures — the caller gets a
		// 207 telling them the canvas may be stale, not a 500 implying nothing
		// happened.
		pausedCount++
		db.ClearWorkspaceKeys(opCtx, ws.id)
		if err := h.broadcaster.RecordAndBroadcast(opCtx, string(events.EventWorkspacePaused), ws.id, map[string]interface{}{
			"name": ws.name,
		}); err != nil {
			note("broadcast", err)
		}
	}

	// ── Report what actually happened ─────────────────────────────────────────
	//
	// Pre-fix this was an UNCONDITIONAL c.JSON(200, {"status":"paused"}) with every
	// failure above reduced to a log line — the same lie 6d64c183f closed for
	// hibernate (#4293), in a different handler.
	//
	// The status is computed in memory and delivered on the HTTP response. That is
	// deliberate: the response needs no DB, no Redis and no broadcast, so it is
	// still reachable when the very subsystem that caused the failure is dead. A
	// failure reported through a write that the failure itself has broken is not a
	// report.
	if len(failures) == 0 {
		log.Printf("Paused workspace %s (%s) + %d children", wsName, id, len(toPause)-1)
		c.JSON(http.StatusOK, gin.H{"status": "paused", "paused_count": pausedCount})
		return
	}
	log.Printf("Pause: %s (%s) — %d/%d paused, %d failed", wsName, id, pausedCount, len(toPause), len(failures))
	if pausedCount == 0 {
		// Same discipline as the 207 below: say what is known, not a guess about
		// the workload. This branch covers BOTH a failed stop (workload still
		// running) and a post-stop failure (workload stopped, row never reached
		// 'paused'), so any fixed claim about whether it is running is false on one
		// of them — and wrong in the more expensive direction on the second, since
		// an operator told the box is up will not go looking for a stopped one.
		// failures[].stage distinguishes them.
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": "pause_failed",
			"error": fmt.Sprintf("no workspace reached the paused state (%d step(s) failed) — see failures[]: stage \"stop\" = the workload was never stopped; "+
				"stage \"mark_paused\"/\"claim\" = it WAS stopped but its row does not say paused. Re-check status before retrying",
				len(failures)),
			"paused_count": 0,
			"failed_count": len(failures),
			"failures":     failures,
		})
		return
	}
	// Partial: the cascade fans out over descendants, so "2 of 3 paused" is a real
	// outcome and must be representable. Collapsing it into 200 or 500 discards the
	// only information the caller can act on.
	//
	// The message states ONLY what is known at this point — a count, and a pointer
	// to failures[]. It deliberately does not say "cascade", "were not paused" or
	// "still running", because none of those hold on every path that reaches here:
	// a single-workspace pause whose stop and claim both succeeded and whose
	// telemetry INSERT then failed is a 207 in which there is no cascade, the
	// workspace WAS paused, and it is NOT running. Prose that contradicts the
	// payload beside it is the same defect as an unconditional 200 — a human reads
	// the sentence, not failures[].
	c.JSON(http.StatusMultiStatus, gin.H{
		"status": "partially_paused",
		"error": fmt.Sprintf("%d of %d workspace(s) paused; %d step(s) failed — see failures[] for which workspace and which stage",
			pausedCount, len(toPause), len(failures)),
		"paused_count": pausedCount,
		"failed_count": len(failures),
		"failures":     failures,
	})
}

// resumeSideEffectBudget is the HARD ceiling on the DATABASE work in Resume's
// detached side-effect phase — TOTAL for the whole cascade, not per workspace.
//
// Why a ceiling is mandatory: the request context used to be the only thing
// bounding this work. Detaching from it (see Resume) removes that bound, and a
// detached operation with no deadline is a different bug. The context built
// from this budget is threaded into every ExecContext/QueryContext below, so
// the deadline is enforced by database/sql itself, not by a convention.
//
// What it bounds, precisely: the statements. Per workspace that is one claim
// UPDATE, one event INSERT and two small SELECTs. 30s is scaled from
// provisionWorkspaceAuto's no-backend arm, the in-repo precedent for exactly
// that shape, which allots 10s to "the broadcast + single UPDATE inside
// markProvisionFailed".
//
// What it does NOT bound — say it plainly rather than let the name imply
// otherwise — is the handler's wall clock. Two things sit outside opCtx:
//
//   - provisionWorkspaceAuto takes acquireRestartProvisionGate(ws.id) and
//     gate.Lock()s it SYNCHRONOUSLY, before its goroutine spawn
//     (workspace_dispatchers.go). If another provision for the same ws-<id> is
//     in flight, this handler goroutine blocks on that mutex for as long as the
//     holder runs — up to provisioner.cpProvisionTimeout (20m). That is
//     pre-existing, deliberate (it is the core#2771 serialization), shared
//     verbatim with Create, and strands nothing; but it is not covered here and
//     no ceiling in this file can cover it.
//   - the provision itself, which runs on the dispatcher's own
//     context.WithTimeout(context.Background(), provisioner.ProvisionTimeout).
//     Already detached, already bounded, correctly not on this budget: a
//     provision legitimately outlives a DB budget.
//
// TOTAL, not per-workspace: a per-workspace timeout lets an N-descendant cascade
// run for N × budget, which is not a bound. When the budget is spent mid-cascade
// the remaining workspaces fast-fail on the expired context and land in the
// failure list — a truthful partial result rather than an unbounded handler.
// A var, not a const, so tests can shrink it.
//
// Sibling note: PR #5102 introduces pauseSideEffectBudget for the equivalent
// phase in Pause. That PR is still OPEN, so the symbol does not exist in this
// tree and nothing here may depend on it. Sizing differs anyway and the reason
// is worth recording for whoever merges second: Pause's detached phase CONTAINS
// a synchronous CP HTTP round-trip (StopWorkspaceAuto → DELETE
// /cp/workspaces/:id → provider terminate), so its budget is scaled against
// provisioner.cpAPITimeout (120s). Resume's phase contains no CP call at all,
// which is why it is scaled against a DB figure instead.
var resumeSideEffectBudget = 30 * time.Second

// errResumeClaimMissed reports that the guarded provisioning claim matched no
// row: the workspace left the paused set between the eligibility SELECT and the
// claim (removed, or already resumed/woken by a concurrent caller). Nothing was
// provisioned for it — see the claim-before-provision note in Resume for why
// that ordering is the whole point.
var errResumeClaimMissed = errors.New("provisioning claim matched no row — the workspace left the paused set (removed, or resumed by a concurrent caller) after the eligibility check")

// Resume handles POST /workspaces/:id/resume
// Re-provisions a paused workspace. Config volume is preserved from before the pause.
func (h *WorkspaceHandler) Resume(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()

	var wsName, dbRuntime, dbTemplate string
	var tier int
	err := db.DB.QueryRowContext(ctx,
		`SELECT name, tier, COALESCE(runtime, 'claude-code'), COALESCE(template, '') FROM workspaces WHERE id = $1 AND status = 'paused'`, id,
	).Scan(&wsName, &tier, &dbRuntime, &dbTemplate)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found or not paused"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	// Accept either provisioner (Docker self-hosted OR CP SaaS). See the
	// same guard in Restart above for context — Resume previously 503'd
	// on every SaaS tenant.
	if !h.HasProvisioner() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "provisioner not available"})
		return
	}

	// Block resume if any ancestor is still paused — must resume from the top down
	if paused, parentName := isParentPaused(ctx, id); paused {
		c.JSON(http.StatusConflict, gin.H{"error": "parent workspace \"" + parentName + "\" is paused — resume it first"})
		return
	}

	// Collect this workspace + all paused descendants to resume
	type wsInfo struct {
		id, name, runtime, template string
		tier                        int
	}
	toResume := []wsInfo{{id, wsName, dbRuntime, dbTemplate, tier}}
	var descendantList []gin.H
	rows, err := db.DB.QueryContext(ctx,
		`WITH RECURSIVE descendants AS (
			SELECT id, name, tier, COALESCE(runtime, 'claude-code') AS runtime, COALESCE(template, '') AS template FROM workspaces WHERE parent_id = $1 AND status = 'paused'
			UNION ALL
			SELECT w.id, w.name, w.tier, COALESCE(w.runtime, 'claude-code'), COALESCE(w.template, '') FROM workspaces w JOIN descendants d ON w.parent_id = d.id WHERE w.status = 'paused'
		) SELECT id, name, tier, runtime, template FROM descendants`, id)
	if err != nil {
		log.Printf("Resume: descendant query failed for %s: %v", id, err)
	}
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var ws wsInfo
			if rows.Scan(&ws.id, &ws.name, &ws.tier, &ws.runtime, &ws.template) == nil {
				toResume = append(toResume, ws)
				descendantList = append(descendantList, gin.H{"id": ws.id, "name": ws.name})
			}
		}
		if err := rows.Err(); err != nil {
			log.Printf("Resume: descendant query rows.Err: %v", err)
		}
	}

	// Default: single-workspace resume unless ?cascade=true
	if c.Query("cascade") != "true" && len(descendantList) > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error":       "workspace has descendants — use ?cascade=true to resume all",
			"descendants": descendantList,
		})
		return
	}

	// ── Detach the side effects from the request lifecycle ────────────────────
	//
	// Everything above this line is a READ: it may safely die with the request,
	// because a dead read produces an honest 404/409/500 and changes nothing.
	// Everything below MUTATES, and must not be cancellable by the client.
	//
	// Pre-fix this loop ran on `ctx` — the request context — so an aborted client
	// connection cancelled the status write and the event while the handler still
	// answered 200 {"status":"provisioning"}. That is the defect PR #5102 (open
	// at time of writing) addresses for Pause, in the same file; Resume is the
	// other copy and does not depend on that PR. Restart hit the
	// hazard first and documented it (see the goAsync + context.Background()
	// dispatch above: "detaches the dispatch from the request lifecycle so an
	// aborted client connection doesn't cancel the in-flight Stop/provision
	// pair"), and WakeWorkspace — Resume's hibernated-side twin, ~800 lines up —
	// already runs its whole claim+provision on context.Background().
	//
	// Resume's failure mode is NOT Pause's, and is worse. The provision dispatch
	// below does NOT take this context: provisionWorkspaceAuto spawns its own
	// goroutine on context.Background(), so it ran to completion whether or not
	// the status write survived. A cancelled request therefore produced a
	// workspace whose box was genuinely started and BILLED while its row still
	// read 'paused' — and 'paused' is precisely the value every recovery path
	// refuses to touch:
	//
	//   - /registry/register's upsert carries
	//     `WHERE workspaces.status NOT IN ('removed','paused','hibernated')`, so
	//     the booting box's registration matched no row, wrote no url, and
	//     returned 200. Silently. That guard's own comment names this handler as
	//     the thing that must have run first: "Resume/Wake set status=
	//     'provisioning' first, so their post-relaunch register still promotes
	//     provisioning→online normally."
	//   - the heartbeat's `recoverable` predicate lists provisioning/failed/
	//     offline/awaiting_agent/degraded — 'paused' is excluded by name as
	//     "terminal or operator-managed", so a live heartbeat returns early.
	//   - StartLivenessMonitor and StartHealthSweep both carry the same
	//     NOT IN (…,'paused',…) guard.
	//   - StartCPOrphanSweeper reaps `status = 'removed'` ONLY, so the running
	//     instance is never reclaimed.
	//   - Pause itself 404s ("not found or already paused") on that row, so the
	//     user cannot even stop the box they are paying for.
	//
	// The one door left open is another Resume — which, without the claim below,
	// provisions a SECOND box and overwrites instance_id with it, orphaning the
	// first beyond the reach of the sweeper that only looks at 'removed'.
	// Note the inversion this produces: a provision that FAILS is self-healing
	// (markProvisionFailed writes status='failed' unguarded, freeing the row),
	// while a provision that SUCCEEDS strands it permanently.
	//
	// Hence the shape below: WithoutCancel + a bounded budget, and the claim is
	// taken BEFORE the provision is dispatched, never after.
	opCtx, cancelOp := context.WithTimeout(context.WithoutCancel(ctx), resumeSideEffectBudget)
	defer cancelOp()

	var failures []gin.H
	resumedCount := 0
	for _, ws := range toResume {
		note := func(stage string, err error) {
			log.Printf("Resume: %s failed for %s (%s): %v", stage, ws.name, ws.id, err)
			failures = append(failures, gin.H{
				"workspace_id": ws.id,
				"name":         ws.name,
				"stage":        stage,
				"error":        err.Error(),
			})
		}

		// Atomic claim, paused→provisioning. Pre-fix this was `WHERE id = $2`
		// with rowsAffected never read — a blind write, the same shape Pause had
		// and the opposite of every sibling: WakeWorkspace claims
		// `WHERE id = $2 AND status = 'hibernated'`, HibernateWorkspace claims
		// `status='hibernating' WHERE status IN ('online','degraded')`.
		//
		// The predicate is load-bearing twice over. It makes a concurrent
		// double-Resume a no-op for the loser instead of a second provision, and
		// — because the dispatch below is gated on it — it makes "the row says
		// provisioning" a PRECONDITION of "a box gets started". That is what
		// closes the running-box/paused-row strand described above: there is no
		// longer any path that provisions compute the database does not know
		// about.
		//
		// Clear mcp_unloaded_since — this is the RESUME path (pause/hibernate →
		// provisioning); a stale warming stamp here is exactly the cluster-2
		// false-degrade the EV2 review flagged (core#4457).
		res, err := db.DB.ExecContext(opCtx,
			`UPDATE workspaces SET status = $1, mcp_unloaded_since = NULL, updated_at = now() WHERE id = $2 AND status = 'paused'`,
			models.StatusProvisioning, ws.id)
		if err != nil {
			note("mark_provisioning", err)
			continue // nothing was started — the row is untouched and still 'paused'
		}
		claimed, raErr := res.RowsAffected()
		if raErr != nil {
			note("mark_provisioning", raErr)
			continue
		}
		if claimed == 0 {
			note("claim", errResumeClaimMissed)
			continue
		}

		// From here this Resume OWNS the transition: the row says provisioning, so
		// the box it is about to start is one the database knows about, and the
		// register/heartbeat guards above will promote it to online. A failure
		// below is real and reportable but does not un-resume anything, so it
		// counts toward resumedCount AND toward failures — the caller gets a 207
		// telling them the canvas may be stale, not a 500 implying nothing
		// happened.
		resumedCount++
		if err := h.broadcaster.RecordAndBroadcast(opCtx, string(events.EventWorkspaceProvisioning), ws.id, map[string]interface{}{
			"name": ws.name, "tier": ws.tier, "runtime": ws.runtime, "template": ws.template,
		}); err != nil {
			note("broadcast", err)
		}
		// Phase 1 template decoupling: the workspace row stores the template
		// explicitly, so resume carries it through CreateWorkspacePayload.
		payload := withStoredCompute(opCtx, ws.id, models.CreateWorkspacePayload{Name: ws.name, Tier: ws.tier, Runtime: ws.runtime, Template: ws.template})
		// RFC#2843 #33: if the row template is empty (legacy row), restore the
		// persisted template on SaaS resume so config + prompts re-deliver.
		if payload.Template == "" && h.cpProv != nil {
			if storedTmpl := storedWorkspaceTemplate(opCtx, ws.id); storedTmpl != "" {
				payload.Template = storedTmpl
			}
		}
		// Resume is provision-only (workspace is paused, no live container
		// to stop). provisionWorkspaceAuto handles backend routing and the
		// no-backend mark-failed fallback identically to Create. Pre-
		// 2026-05-05 this site inlined the if-cpProv-else dispatch; the
		// dispatcher is the SoT now.
		//
		// It takes no context by design — it builds its own from
		// context.Background() with provisioner.ProvisionTimeout — so this
		// dispatch is NOT bounded by resumeSideEffectBudget and must not be
		// (a provision legitimately outlives a 30s DB budget).
		h.provisionWorkspaceAuto(ws.id, "", nil, payload)
	}

	// ── Report what actually happened ─────────────────────────────────────────
	//
	// Pre-fix this was an UNCONDITIONAL c.JSON(200, {"status":"provisioning"})
	// with every failure above reduced to a log line.
	//
	// The status is computed in memory and delivered on the HTTP response. That
	// is deliberate: the response needs no DB, no Redis and no broadcast, so it
	// is still reachable when the very subsystem that caused the failure is dead.
	// A failure reported through a write that the failure itself has broken is
	// not a report.
	//
	// Note what "provisioning" keeps meaning on the success arm, and why the
	// async dispatch above does not make it a lie. It asserts a state that is
	// DURABLY TRUE AT RESPONSE TIME: the claim committed before this line runs.
	// Pause's {"status":"paused"} could not make that trade — it asserts a
	// COMPLETED TERMINAL state, which is why PR #5102 keeps Pause synchronous.
	// "provisioning" is an honest "queued", and the provision's own outcome
	// arrives asynchronously as a WORKSPACE_PROVISION_FAILED event plus
	// status='failed' via markProvisionFailed, exactly as it does for Restart and
	// Create.
	//
	// The residual races are bounded too, which is the property that actually
	// makes this safe. Post-fix there are exactly two:
	//
	//   - claim lands, dispatch never happens (process dies between them). The
	//     row sits at 'provisioning', and StartProvisioningTimeoutSweep
	//     (registry/provisiontimeout.go, wired in cmd/server/main.go) flips
	//     'provisioning' rows older than DefaultProvisioningTimeout — 12m, 30m
	//     for hermes — to 'failed'. Recoverable: 'failed' is in the heartbeat's
	//     `recoverable` set and Restart accepts it.
	//   - claim fails, nothing is dispatched. The row stays 'paused', which is
	//     the truth, and Resume can be retried.
	//
	// So the invariant this handler now holds is: EVERY residual race lands in a
	// status something can recover from. Pre-fix the SUCCESS path landed in
	// 'paused'-with-a-running-box, the one status nothing can.
	if len(failures) == 0 {
		log.Printf("Resuming workspace %s (%s) + %d children", wsName, id, len(toResume)-1)
		c.JSON(http.StatusOK, gin.H{"status": "provisioning", "resumed_count": resumedCount})
		return
	}
	log.Printf("Resume: %s (%s) — %d/%d resuming, %d failed", wsName, id, resumedCount, len(toResume), len(failures))
	if resumedCount == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": "resume_failed",
			"error": fmt.Sprintf("no workspace entered the provisioning state (%d step(s) failed) — see failures[]: stage \"mark_provisioning\" = the status write failed; "+
				"stage \"claim\" = the workspace was no longer paused. Nothing was provisioned in either case; re-check status before retrying",
				len(failures)),
			"resumed_count": 0,
			"failed_count":  len(failures),
			"failures":      failures,
		})
		return
	}
	// Partial: the cascade fans out over descendants, so "2 of 3 resuming" is a
	// real outcome and must be representable. Collapsing it into 200 or 500
	// discards the only information the caller can act on.
	//
	// The message states ONLY what is known here — a count and a pointer to
	// failures[] — because no stronger claim holds on every path that reaches
	// this line: a single-workspace resume whose claim succeeded and whose
	// telemetry INSERT then failed is a 207 in which there is no cascade and the
	// workspace IS provisioning. Prose that contradicts the payload beside it is
	// the same defect as an unconditional 200 — a human reads the sentence, not
	// failures[].
	c.JSON(http.StatusMultiStatus, gin.H{
		"status": "partially_resumed",
		"error": fmt.Sprintf("%d of %d workspace(s) entered provisioning; %d step(s) failed — see failures[] for which workspace and which stage",
			resumedCount, len(toResume), len(failures)),
		"resumed_count": resumedCount,
		"failed_count":  len(failures),
		"failures":      failures,
	})
}

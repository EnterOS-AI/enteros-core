package handlers

// scheduler_plugin.go — per-workspace delivery of the default scheduler.
//
// Scheduler-as-trigger-plugin RFC. core#4399 (P4) retired the central cron
// loop; scheduling is now a per-workspace `kind:trigger` plugin
// (molecule-ai-plugin-scheduler). A workspace runs the scheduler daemon ONLY if
// that plugin is installed — so a workspace that has (or gets) schedules must
// have it DECLARED (workspace_declared_plugins → MOLECULE_DECLARED_PLUGINS →
// boot-install / online-reconcile), and, for an already-running workspace, its
// daemon ARMED without a restart via the runtime hot-start endpoint
// (POST /internal/daemons/reload, molecule-ai-workspace-runtime#308).
//
// This is additive: it only ever adds a declaration + best-effort reload for
// workspaces that touch schedules. Schedule storage is volume-authoritative
// (the legacy core-DB schedule backend was retired in P4b).

import (
	"bytes"
	"context"
	"database/sql"
	"log"
	"net/http"
	"strings"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// SchedulerPluginName is the declared-plugin name (matches plugin.yaml) and the
// registry key core looks the source up by.
const SchedulerPluginName = "molecule-scheduler"

// SchedulerPluginSource is the git-native source the boot-installer / reconcile
// fetches. It is NO LONGER a hand-written literal: it is sourced from the SDK
// native-plugins registry SSOT (plugin_registry.go), so bumping the plugin's
// pinned tag happens once, in the registry, and reaches core via a molcontracts
// bump — core can't drift. A registry that drops molecule-scheduler panics at
// startup (mustNativePluginSource) rather than recording an empty source.
var SchedulerPluginSource = mustNativePluginSource(SchedulerPluginName)

// schedulerArmTimeout bounds the best-effort reload forward. Short: arming is a
// fire-and-forget optimization over the reconcile-on-online safety net.
const schedulerArmTimeout = 8 * time.Second

// ensureSchedulerPluginDeclared records molecule-scheduler in the workspace's
// declared-plugin set (idempotent upsert). After this, the plugin is installed
// on the next provision/boot (MOLECULE_DECLARED_PLUGINS) or the next
// transition-to-online reconcile — so scheduling survives a restart with no
// further action. Safe to call on every schedule create/seed.
func ensureSchedulerPluginDeclared(ctx context.Context, workspaceID string) error {
	return recordDeclaredPlugin(ctx, workspaceID, SchedulerPluginName, SchedulerPluginSource)
}

// armSchedulerPlugin asks a RUNNING workspace to start its scheduler daemon now
// (no restart), via the runtime hot-start endpoint. Non-fatal: any failure is
// logged, because the daemon is also armed by the reconcile-on-online path (the
// durable safety net). A runtime too old to expose the endpoint simply 404s here
// and arms on its next restart instead.
//
// Returns true iff the reload returned 2xx. Because the runtime mounts
// /internal/schedules BEFORE /internal/daemons/reload (and both before it
// accepts connections), a 2xx here PROVES the schedule grid API is already
// serving — the synchronous Create path (ensureAndArmSchedulerPluginSync) uses
// that as a readiness signal to avoid forwarding a create at a still-booting
// runtime. Statement-callers that don't care (the carryover restore path) simply
// discard the bool.
func armSchedulerPlugin(ctx context.Context, workspaceID string) bool {
	if db.DB == nil {
		return false
	}
	var wsURL string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(url, '') FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&wsURL); err != nil {
		if err != sql.ErrNoRows {
			log.Printf("scheduler-arm: workspace lookup failed for %s: %v", workspaceID, err)
		}
		return false
	}
	if wsURL == "" {
		return false // no callback URL (poll-mode / not registered) — reconcile arms it
	}
	if err := isSafeURL(wsURL); err != nil {
		log.Printf("scheduler-arm: unsafe workspace URL for %s rejected: %v", workspaceID, err)
		return false
	}
	secret, _, err := readOrLazyHealInboundSecret(ctx, workspaceID, "scheduler-arm")
	if err != nil {
		log.Printf("scheduler-arm: inbound secret unavailable for %s: %v", workspaceID, err)
		return false
	}

	target := strings.TrimRight(wsURL, "/") + "/internal/daemons/reload"
	fctx, cancel := context.WithTimeout(ctx, schedulerArmTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodPost, target, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Timeout: schedulerArmTimeout,
		Transport: &http.Transport{
			DialContext:         safeDialer().DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("scheduler-arm: reload forward to %s failed (workspace will arm on next reconcile): %v", workspaceID, err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("scheduler-arm: reload for %s returned %d (arms on next reconcile)", workspaceID, resp.StatusCode)
		return false
	}
	log.Printf("scheduler-arm: hot-armed scheduler daemon on %s", workspaceID)
	return true
}

// ensureAndArmSchedulerPluginSync declares the plugin (blocking, must persist so
// the plugin survives a restart) and then arms the running daemon
// SYNCHRONOUSLY, returning whether the arm reached a serving runtime.
//
// This is the Create path. Post-P4b the schedule store is volume-only (no DB
// fallback), so a create MUST NOT forward to a runtime that isn't serving
// /internal/schedules yet — the daemon is often declared+armed on this very
// request (first schedule on a fresh workspace). Arming here BEFORE createVolume
// closes that race: because the runtime mounts /internal/schedules before the
// reload route, a true return (2xx reload) proves the grid API is up. A false
// return is non-fatal — createVolume still tries, and its bounded transient-dial
// retry + retryable 503 cover a runtime that is still coming up. Bounded by
// schedulerArmTimeout (8s), so the create response is delayed at most that long
// and only while the daemon is actually starting.
func ensureAndArmSchedulerPluginSync(ctx context.Context, workspaceID string) (armed bool, err error) {
	// The declaration MUST persist even if the caller's ctx is cancelled (client
	// disconnect) or the create deadline fires mid-write — otherwise a schedule
	// can land on the volume with no declared plugin and silently never fire after
	// the next restart (the reconcile-on-online net installs only what's declared).
	// Detach it from cancellation; it is a fast idempotent upsert.
	if err := ensureSchedulerPluginDeclared(context.WithoutCancel(ctx), workspaceID); err != nil {
		return false, err
	}
	actx, cancel := context.WithTimeout(ctx, schedulerArmTimeout)
	defer cancel()
	return armSchedulerPlugin(actx, workspaceID), nil
}

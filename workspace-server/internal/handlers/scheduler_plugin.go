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

// armSchedulerReload asks a RUNNING workspace to start its scheduler daemon now
// (no restart), via the runtime hot-start endpoint, using ALREADY-RESOLVED
// forward creds. Non-fatal: any failure is logged, because the daemon is also
// armed by the reconcile-on-online path (the durable safety net). A runtime too
// old to expose the endpoint simply 404s here and arms on its next restart.
//
// Returns true iff the reload returned 2xx. Because the runtime mounts
// /internal/schedules BEFORE /internal/daemons/reload (and both before it accepts
// connections), a 2xx here PROVES the schedule grid API is already serving — the
// Create path uses that as the readiness signal to avoid forwarding a create at a
// still-booting runtime. Create resolves url+secret ONCE and shares them with the
// create forward (SSOT — no second workspaces.url query).
func armSchedulerReload(ctx context.Context, workspaceID, wsURL, secret string) bool {
	if wsURL == "" {
		return false // no callback URL (poll-mode / not registered) — reconcile arms it
	}
	if err := isSafeURL(wsURL); err != nil {
		log.Printf("scheduler-arm: unsafe workspace URL for %s rejected: %v", workspaceID, err)
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

// armSchedulerPlugin is the SELF-RESOLVING arm for callers that don't already
// hold the workspace's forward creds (the carryover restore path). It resolves
// url + secret then delegates to armSchedulerReload. The Create path does NOT use
// this — it resolves once via resolveWorkspaceForwardCreds and calls
// armSchedulerReload directly, so there is no second workspaces.url query.
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
		return false
	}
	secret, _, err := readOrLazyHealInboundSecret(ctx, workspaceID, "scheduler-arm")
	if err != nil {
		log.Printf("scheduler-arm: inbound secret unavailable for %s: %v", workspaceID, err)
		return false
	}
	return armSchedulerReload(ctx, workspaceID, wsURL, secret)
}

// schedulerDeclareTimeout bounds the DETACHED plugin-declaration upsert (see
// ensureSchedulerPluginDeclaredBounded). Detaching from the request/deadline
// keeps the durable declaration from being aborted by a client disconnect, but
// the write must still be bounded so a contended/slow workspace_declared_plugins
// upsert can't hang the request — the bug a raw context.WithoutCancel had.
const schedulerDeclareTimeout = 5 * time.Second

// ensureSchedulerPluginDeclaredBounded records the molecule-scheduler declaration
// on a context DETACHED from the caller's cancellation (so a client disconnect or
// the create deadline can't abort the must-persist upsert) yet BOUNDED by
// schedulerDeclareTimeout (so it can't hang unbounded). Post-P4b the schedule
// store is volume-only, so a missing declaration means the daemon is never
// installed on the next restart and the schedule silently never fires — hence the
// declaration is the durable net and must persist, but only within a bound.
func ensureSchedulerPluginDeclaredBounded(ctx context.Context, workspaceID string) error {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), schedulerDeclareTimeout)
	defer cancel()
	return ensureSchedulerPluginDeclared(dctx, workspaceID)
}

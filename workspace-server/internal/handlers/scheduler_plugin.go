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
// workspaces that touch schedules. It does not change the schedule storage
// routing (volume vs the legacy DB table) — that cutover is P4b.

import (
	"bytes"
	"context"
	"database/sql"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

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
// (no restart), via the runtime hot-start endpoint. Best-effort and
// non-blocking: any failure is logged and swallowed, because the daemon is also
// armed by the reconcile-on-online path (the durable safety net) — this call
// just makes a freshly-scheduled workspace fire without waiting for a restart.
// A runtime too old to expose the endpoint simply 404s here and arms on its
// next restart instead.
func armSchedulerPlugin(ctx context.Context, workspaceID string) {
	if db.DB == nil {
		return
	}
	var wsURL string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(url, '') FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&wsURL); err != nil {
		if err != sql.ErrNoRows {
			log.Printf("scheduler-arm: workspace lookup failed for %s: %v", workspaceID, err)
		}
		return
	}
	if wsURL == "" {
		return // no callback URL (poll-mode / not registered) — reconcile arms it
	}
	if err := isSafeURL(wsURL); err != nil {
		log.Printf("scheduler-arm: unsafe workspace URL for %s rejected: %v", workspaceID, err)
		return
	}
	secret, _, err := readOrLazyHealInboundSecret(ctx, workspaceID, "scheduler-arm")
	if err != nil {
		log.Printf("scheduler-arm: inbound secret unavailable for %s: %v", workspaceID, err)
		return
	}

	target := strings.TrimRight(wsURL, "/") + "/internal/daemons/reload"
	fctx, cancel := context.WithTimeout(ctx, schedulerArmTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodPost, target, bytes.NewReader([]byte("{}")))
	if err != nil {
		return
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
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("scheduler-arm: reload for %s returned %d (arms on next reconcile)", workspaceID, resp.StatusCode)
		return
	}
	log.Printf("scheduler-arm: hot-armed scheduler daemon on %s", workspaceID)
}

// ensureAndArmSchedulerPlugin declares the plugin (blocking, must persist so
// the plugin survives a restart) and then arms the running daemon
// asynchronously (best-effort, off the request path). Call it whenever a
// workspace gains a schedule. A declaration failure is returned so callers that
// care (create) can log it; arming never blocks or errors.
func ensureAndArmSchedulerPlugin(ctx context.Context, workspaceID string) error {
	if err := ensureSchedulerPluginDeclared(ctx, workspaceID); err != nil {
		return err
	}
	// Detach from the request context so a slow reload never delays the create
	// response, and use a background context so request cancellation after the
	// (already-committed) declaration does not abort the arm.
	wsID := workspaceID
	globalGoAsync(func() {
		actx, cancel := context.WithTimeout(context.Background(), schedulerArmTimeout+2*time.Second)
		defer cancel()
		armSchedulerPlugin(actx, wsID)
	})
	return nil
}

// BackfillSchedulerPlugin remediates the P4 gap for EXISTING scheduled
// workspaces: those with rows in workspace_schedules created before per-workspace
// delivery existed have no scheduler plugin, so post-P4 their schedules fire
// nowhere. This declares molecule-scheduler for each such workspace (and, when
// applying, best-effort arms it).
//
// DRY-RUN BY DEFAULT — returns the affected workspace list and counts WITHOUT
// mutating. Pass ?apply=true to actually declare + arm. This lets an operator
// review the exact blast radius (and hand it to the CTO) before touching prod.
//
//	@Router	/admin/schedules/backfill-plugin [post]
//	@Security	AdminAuth
func (h *ScheduleHandler) BackfillSchedulerPlugin(c *gin.Context) {
	ctx := c.Request.Context()
	apply := strings.EqualFold(strings.TrimSpace(c.Query("apply")), "true")

	rows, err := db.DB.QueryContext(ctx,
		`SELECT DISTINCT workspace_id FROM workspace_schedules ORDER BY workspace_id`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to enumerate scheduled workspaces"})
		return
	}
	defer rows.Close()
	var workspaceIDs []string
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to scan workspace id"})
			return
		}
		workspaceIDs = append(workspaceIDs, id)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "row iteration failed"})
		return
	}

	if !apply {
		c.JSON(http.StatusOK, gin.H{
			"dry_run":       true,
			"would_declare": len(workspaceIDs),
			"plugin":        SchedulerPluginName,
			"source":        SchedulerPluginSource,
			"workspace_ids": workspaceIDs,
			"note":          "no changes made — re-run with ?apply=true to declare + arm",
		})
		return
	}

	var declared, failed int
	failures := map[string]string{}
	for _, id := range workspaceIDs {
		if err := ensureAndArmSchedulerPlugin(ctx, id); err != nil {
			failed++
			failures[id] = err.Error()
			log.Printf("scheduler-backfill: declare failed for %s: %v", id, err)
			continue
		}
		declared++
	}
	log.Printf("scheduler-backfill: applied — declared=%d failed=%d of %d scheduled workspace(s)", declared, failed, len(workspaceIDs))
	c.JSON(http.StatusOK, gin.H{
		"dry_run":  false,
		"declared": declared,
		"failed":   failed,
		"total":    len(workspaceIDs),
		"plugin":   SchedulerPluginName,
		"failures": failures,
	})
}

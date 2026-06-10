// Package main runs the per-tenant workspace-server.
//
//	@title			Molecule AI Workspace Server API
//	@version		1.0
//	@description	The per-tenant workspace-server HTTP API. Single source of truth for workspace/schedule/agent/secrets/files/memory CRUD. Hand-written clients (canvas, molecule-mcp-server, molecule-cli, molecule-sdk-python) should be replaced by clients generated from this spec — see RFC #1706.
//	@host			api.moleculesai.app
//	@BasePath		/
//	@schemes		https
//
//	@securityDefinitions.apikey	BearerAuth
//	@in							header
//	@name						Authorization
//	@description				Bearer token issued by Gitea (org-admin or persona PAT) or by the platform's signup/SSO flow.
//
//	@securityDefinitions.apikey	OrgSlugAuth
//	@in							header
//	@name						X-Molecule-Org-Slug
//	@description				Tenant routing header — required on every /workspaces/{id}/* request so the platform edge can route to the correct per-tenant workspace-server. Either X-Molecule-Org-Slug (human-readable, e.g. "agents-team") or X-Molecule-Org-Id (UUID) must be sent; slug is preferred for client code.
//
//	@securityDefinitions.apikey	OrgIdAuth
//	@in							header
//	@name						X-Molecule-Org-Id
//	@description				Tenant routing header (UUID form). Alternative to X-Molecule-Org-Slug. At least one of OrgSlugAuth or OrgIdAuth must be sent alongside BearerAuth.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/channels"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/codexauth"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/crypto"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/handlers"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/imagewatch"
	memwiring "git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/memory/wiring"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/middleware"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/pendinguploads"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/plugins"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/registry"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/router"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/scheduler"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/supervised"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/templatecache"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/ws"

	// External plugins — each registers EnvMutator(s) that run at workspace
	// provision time. Loaded via soft-dep gates in main() so self-hosters
	// without per-agent identity configured keep working.
	ghidentity "go.moleculesai.app/plugin/gh-identity/pluginloader"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/pkg/provisionhook"
	"github.com/gin-gonic/gin"
)

func main() {
	// .env auto-load: in dev, the operator keeps MOLECULE_ENV /
	// DATABASE_URL / etc. in the monorepo's .env file. Loading it here
	// — before any code reads env — means a fresh `/tmp/molecule-server`
	// run picks up dev config without `set -a && source .env`. No-op
	// in production (Docker image doesn't ship a .env, and existing env
	// always wins over file values, so container env stays dominant).
	loadDotEnvIfPresent()

	// CP self-refresh: pull any operator-rotated config (e.g. a new
	// MOLECULE_CP_SHARED_SECRET) before any other code reads env.
	// Best-effort — if the CP is unreachable we keep booting with the
	// env we were provisioned with. Older SaaS tenants predate PR #53
	// and can arrive here with MOLECULE_CP_SHARED_SECRET unset; this
	// is how they heal without SSH.
	if err := refreshEnvFromCP(); err != nil {
		log.Printf("CP env refresh: %v (continuing with baked-in env)", err)
	}

	// Managed-tenant boot assertion (cp#469 — tenant proxy-env delivery).
	// If we're a managed SaaS tenant (orgID + adminToken set), all required
	// LLM proxy env vars must be present after refresh. Missing keys block
	// the tenant from booting with broken LLM creds — silent-fail is worse
	// than a loud refusal. Self-hosted (no orgID/adminToken) short-circuits
	// inside the assertion, so this never fires for dev.
	if err := assertManagedTenantHasLLMEnv(); err != nil {
		log.Fatalf("Managed tenant boot assertion: %v", err)
	}

	// Secrets encryption. In MOLECULE_ENV=prod, boot refuses to start
	// without a valid SECRETS_ENCRYPTION_KEY (fail-secure — Top-5 #5).
	// In any other environment, missing keys just log a warning and
	// continue with encryption disabled for dev ergonomics.
	if err := crypto.InitStrict(); err != nil {
		log.Fatalf("Secrets encryption: %v", err)
	}
	if crypto.IsEnabled() {
		log.Println("Secrets encryption: AES-256-GCM enabled")
	} else {
		log.Println("Secrets encryption: disabled (set SECRETS_ENCRYPTION_KEY — required when MOLECULE_ENV=prod)")
	}

	// Database
	databaseURL := envOr("DATABASE_URL", "postgres://dev:dev@localhost:5432/molecule?sslmode=disable")
	if err := db.InitPostgres(databaseURL); err != nil {
		log.Fatalf("Postgres init failed: %v", err)
	}

	// Run migrations
	migrationsDir := findMigrationsDir()
	if migrationsDir != "" {
		if err := db.RunMigrations(migrationsDir); err != nil {
			log.Fatalf("Migrations failed: %v", err)
		}
	}

	// Self-hosted platform-agent seed. With no control plane present to install
	// the org's concierge (SaaS leaves it to the CP at org-provision time), the
	// tenant server seeds it itself when MOLECULE_SEED_PLATFORM_AGENT is set —
	// the self-hosted docker-compose sets it, while CI harnesses + SaaS tenants
	// leave it unset (so e2e empty-DB assertions and the CP path are unaffected).
	// Idempotent + best-effort — never fatal.
	if v := os.Getenv("MOLECULE_SEED_PLATFORM_AGENT"); v == "true" || v == "1" {
		if err := handlers.EnsureSelfHostedPlatformAgent(context.Background(), db.DB); err != nil {
			log.Printf("boot: platform-agent self-seed failed (non-fatal): %v", err)
		}
	}

	// Redis
	redisURL := envOr("REDIS_URL", "redis://localhost:6379")
	if err := db.InitRedis(redisURL); err != nil {
		log.Fatalf("Redis init failed: %v", err)
	}

	// WebSocket Hub — inject CanCommunicate as a function to avoid import cycles
	hub := ws.NewHub(registry.CanCommunicate)
	go hub.Run()

	// Event Broadcaster
	broadcaster := events.NewBroadcaster(hub)

	// Start Redis pub/sub subscriber
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Every long-running subsystem below is wrapped by supervised.RunWithRecover:
	// a panic (e.g. from a single bad tenant row) is logged + the subsystem is
	// restarted with exponential backoff instead of silently dying forever.
	// Motivation: issue #85 (scheduler silent outage for 12+ hours) and #92
	// (systemic — affects every background goroutine).
	go supervised.RunWithRecover(ctx, "broadcaster", broadcaster.Subscribe)

	// Activity log retention — configurable via env vars
	retentionDays := envOr("ACTIVITY_RETENTION_DAYS", "7")
	cleanupHours := envOr("ACTIVITY_CLEANUP_INTERVAL_HOURS", "6")
	cleanupInterval, _ := time.ParseDuration(cleanupHours + "h")
	if cleanupInterval == 0 {
		cleanupInterval = 6 * time.Hour
	}
	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				result, err := db.DB.ExecContext(ctx, `DELETE FROM activity_logs WHERE created_at < now() - ($1 || ' days')::interval`, retentionDays)
				if err != nil {
					log.Printf("Activity log cleanup error: %v", err)
				} else {
					n, err := result.RowsAffected()
					if err != nil {
						log.Printf("Activity log cleanup RowsAffected error: %v", err)
					} else if n > 0 {
						log.Printf("Activity log cleanup: purged %d old entries", n)
					}
				}
			}
		}
	}()

	// Provisioner — auto-detect backend:
	//   1. MOLECULE_ORG_ID set → SaaS tenant → control plane provisioner
	//   2. Docker available     → self-hosted → Docker provisioner
	//   3. Neither              → provisioner disabled (external agents only)
	var prov *provisioner.Provisioner
	var cpProv *provisioner.CPProvisioner
	if os.Getenv("MOLECULE_ORG_ID") != "" {
		// SaaS tenant — provision via control plane (holds Fly token, manages billing)
		if cp, err := provisioner.NewCPProvisioner(); err != nil {
			log.Printf("Control plane provisioner unavailable: %v", err)
		} else {
			cpProv = cp
			defer cpProv.Close()
			log.Println("Provisioner: Control Plane (auto-detected SaaS tenant)")
		}
	} else {
		// Self-hosted — use local Docker daemon
		if p, err := provisioner.New(); err != nil {
			log.Printf("Provisioner disabled (Docker not available): %v", err)
		} else {
			prov = p
			defer prov.Close()
			log.Println("Provisioner: Docker")
		}
	}

	// Issue #831 bootstrap: if global_secrets has ADMIN_TOKEN=placeholder,
	// replace it with the real token from the environment. This fixes
	// workspaces provisioned before the correct value was seeded.
	// Only runs for SaaS tenants (cpProv != nil) where containers inherit
	// from global_secrets. Self-hosted deployments don't read ADMIN_TOKEN
	// from global_secrets for container env — the fix doesn't apply.
	if cpProv != nil {
		fixAdminTokenPlaceholder()
	}

	port := envOr("PORT", "8080")
	platformURL := envOr("PLATFORM_URL", fmt.Sprintf("http://host.docker.internal:%s", port))
	configsDir := envOr("CONFIGS_DIR", findConfigsDir())
	templateCacheDir := envOr("TEMPLATE_CACHE_DIR", filepath.Join(os.TempDir(), "molecule-template-cache"))
	manifestPath := findWorkspaceManifestPath()
	templateToken := templateCacheToken()
	refreshTemplates := func(ctx context.Context) (templatecache.RefreshReport, error) {
		return templatecache.RefreshWorkspaceTemplates(ctx, manifestPath, templateCacheDir, templateToken)
	}
	if shouldRefreshTemplateCache(templateToken, manifestPath) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		report, err := refreshTemplates(ctx)
		cancel()
		if err != nil {
			log.Printf("template cache refresh: %v (continuing with baked templates)", err)
		} else {
			log.Printf("template cache refresh: refreshed %d workspace templates into %s", len(report.Results), templateCacheDir)
		}
	}

	// Init order: wh → onWorkspaceOffline → liveness/healthSweep → router
	// WorkspaceHandler is created before the router so RestartByID can be wired into
	// the offline callbacks used by both the liveness monitor and the health sweep.
	wh := handlers.NewWorkspaceHandler(broadcaster, prov, platformURL, configsDir).
		WithTemplateCacheDir(templateCacheDir)
	if cpProv != nil {
		wh.SetCPProvisioner(cpProv)
	}

	// Self-hosted platform-agent boot-provision (Change 1). The line-128 seed
	// only creates the concierge DB ROW; on a fresh self-host that leaves it
	// with no container (status='failed'/'online' but nothing running). Now that
	// the local Docker provisioner (prov) and WorkspaceHandler (RestartByID)
	// exist, kick off a best-effort provision so a self-hosted concierge comes
	// online automatically once LLM creds exist.
	//
	// Guarded to self-host ONLY: same MOLECULE_SEED_PLATFORM_AGENT flag as the
	// seed AND prov != nil (local Docker active ⇒ MOLECULE_ORG_ID unset). The
	// SaaS path (cpProv != nil ⇒ prov == nil) never triggers — the CP owns
	// concierge provisioning there. Best-effort + non-fatal + runs once: on a
	// fresh self-host with no creds the provision fails and the agent stays
	// 'failed' until BYOK is configured via Settings; RestartByID is itself
	// debounced so this can't loop. Runs in a goroutine inside the helper so a
	// slow image pull never delays the HTTP server.
	if v := os.Getenv("MOLECULE_SEED_PLATFORM_AGENT"); (v == "true" || v == "1") && prov != nil {
		handlers.MaybeProvisionPlatformAgentOnBoot(context.Background(), db.DB, prov, wh.RestartByID)
	}

	// Memory v2 plugin (RFC #2728): build the dependency bundle once
	// here so all three handlers (MCPHandler, AdminMemoriesHandler,
	// WorkspaceHandler) get the same plugin/resolver pair. memBundle
	// is nil when MEMORY_PLUGIN_URL is unset — every consumer
	// nil-checks before using.
	memBundle := memwiring.Build(db.DB)
	if memBundle != nil {
		wh.WithNamespaceCleanup(memBundle.NamespaceCleanupFn())
		// Issue #1755: route workspace-create `initial_memories` through
		// the v2 plugin instead of the legacy `agent_memories` table.
		// Same plugin client the MCP tools use, same namespace
		// (`workspace:<id>`); writes are visible to subsequent
		// `recall_memory` calls on the same workspace.
		wh.WithSeedMemoryPlugin(memBundle.Plugin)
	}

	// External-plugin env mutators — each plugin contributes 0+ mutators
	// onto a shared registry. gh-identity populates MOLECULE_AGENT_ROLE-
	// derived attribution env vars that the workspace's install.sh can
	// then read.
	//
	// github-app-auth was dropped 2026-05-07 (closes #157): per-agent
	// Gitea identities (this gh-identity plugin's role-derived path)
	// replaced GitHub-App-installation tokens after the 2026-05-06
	// suspension. Workspaces now provision with a per-persona Gitea PAT
	// from .env instead of an App-rotated GITHUB_TOKEN.
	envReg := provisionhook.NewRegistry()

	// gh-identity plugin — per-agent attribution via env injection + gh
	// wrapper shipped as base64 env. Soft-dep: no config file is OK
	// (plugin no-ops when no role is set on the workspace).
	// Tracks molecule-core#1957.
	if res, err := ghidentity.BuildRegistry(); err != nil {
		log.Fatalf("gh-identity plugin: %v", err)
	} else {
		envReg.Register(res.Mutator)
		log.Printf("gh-identity: registered (config file=%q)", os.Getenv("MOLECULE_GH_IDENTITY_CONFIG_FILE"))
	}

	wh.SetEnvMutators(envReg)
	log.Printf("env-mutator chain: %v", envReg.Names())

	// Offline handler: broadcast event + auto-restart the dead workspace
	onWorkspaceOffline := func(innerCtx context.Context, workspaceID string) {
		if err := broadcaster.RecordAndBroadcast(innerCtx, "WORKSPACE_OFFLINE", workspaceID, map[string]interface{}{}); err != nil {
			log.Printf("Offline broadcast error for %s: %v", workspaceID, err)
		}
		// Auto-restart: bring the workspace back automatically
		go wh.RestartByID(workspaceID)
	}

	// Start Liveness Monitor — Redis TTL expiry-based offline detection + auto-restart
	go supervised.RunWithRecover(ctx, "liveness-monitor", func(c context.Context) {
		registry.StartLivenessMonitor(c, onWorkspaceOffline)
	})

	// Proactive health sweep — two passes per tick:
	//   1. Docker-side: checks "online" workspaces against the local Docker
	//      daemon (only runs when prov is non-nil, i.e. self-hosted mode).
	//   2. Remote-side: scans runtime='external' rows whose last_heartbeat_at
	//      is past REMOTE_LIVENESS_STALE_AFTER and flips them to
	//      awaiting_agent. Runs regardless of provisioner mode — SaaS
	//      tenants need this even though they don't run Docker locally,
	//      because external-runtime workspaces are operator-managed and
	//      the platform-side liveness sweep is the only thing that
	//      transitions them off 'online' when the operator's CLI dies.
	//
	// Pre-2026-04-30 this goroutine was gated on prov != nil, which silently
	// disabled the remote-side sweep on every SaaS tenant. The function in
	// healthsweep.go has always handled nil checker correctly; only the
	// orchestration was wrong. See #2392's CI failure for the trace.
	go supervised.RunWithRecover(ctx, "health-sweep", func(c context.Context) {
		registry.StartHealthSweep(c, prov, 15*time.Second, onWorkspaceOffline)
	})

	// Orphan-container reconcile sweep — finds running containers
	// whose workspace row is already status='removed' and stops
	// them. Defence in depth on top of the inline cleanup in
	// handlers/workspace_crud.go: any Docker hiccup that left a
	// container alive after the user clicked delete heals on the
	// next sweep instead of leaking forever.
	if prov != nil {
		go supervised.RunWithRecover(ctx, "orphan-sweeper", func(c context.Context) {
			registry.StartOrphanSweeper(c, prov)
		})
	}

	// CP-mode orphan sweeper — SaaS counterpart to the Docker sweeper
	// above. Re-issues cpProv.Stop for any workspace at status='removed'
	// with a non-NULL instance_id, healing the deprovision split-write
	// race documented in #2989: tenant marks status='removed' BEFORE
	// calling CP DELETE, so a transient CP failure leaves the EC2
	// running with no retry path. cpProv.Stop is idempotent against
	// already-terminated instances; on success we clear instance_id.
	if cpProv != nil {
		go supervised.RunWithRecover(ctx, "cp-orphan-sweeper", func(c context.Context) {
			registry.StartCPOrphanSweeper(c, cpProv)
		})
	}

	// CP-mode instance-state reconciler — authoritative EC2-liveness pass
	// for SaaS workspaces (core#2261). Every other liveness sweep keys off
	// a PROXY (Redis TTL, agent heartbeat, local Docker, or
	// runtime='external'); a SaaS claude-code workspace whose EC2 was
	// terminated/stopped falls through ALL of them and stays status='online'
	// pointing at a dead instance_id forever (root cause: core#2247). This
	// loop asks the ONE authoritative question the others lack —
	// cpProv.IsRunning (CP DescribeInstances-equivalent) — for each online
	// SaaS row, and on a CLEAN "not running" feeds it into the SAME
	// onWorkspaceOffline closure the other sweeps use (status flip +
	// RestartByID reprovision, existing volume). Fail-safe: IsRunning is
	// (true, err) on any transient error, so a CP blip never flips a healthy
	// workspace.
	if cpProv != nil {
		// Guard against double-reprovision thrash (internal#544): the restart
		// debounce window must cover the reconciler interval so a workspace
		// flipped offline by one reconcile tick isn't immediately reprovisioned
		// again by the next tick before the debounce drops it. If the interval
		// ever shrinks below the debounce window, the coupling silently breaks.
		reconcileInterval := 60 * time.Second
		if handlers.RestartDebounceWindow < reconcileInterval {
			log.Fatalf("RestartDebounceWindow (%s) must be >= CP instance reconciler interval (%s) to prevent double-reprovision thrash (internal#544)", handlers.RestartDebounceWindow, reconcileInterval)
		}
		go supervised.RunWithRecover(ctx, "cp-instance-reconciler", func(c context.Context) {
			registry.StartCPInstanceReconciler(c, cpProv, onWorkspaceOffline, reconcileInterval)
		})
	}

	// Pending-uploads GC sweep — deletes acked rows past their retention
	// window plus unacked rows past expires_at. Without this the
	// pending_uploads table grows unbounded; even with the 24h hard TTL,
	// nothing actually deletes a row, just makes it un-fetchable.
	go supervised.RunWithRecover(ctx, "pending-uploads-sweeper", func(c context.Context) {
		pendinguploads.StartSweeper(c, pendinguploads.NewPostgres(db.DB), 0)
	})

	// Codex shared-OAuth central refresher — the SINGLE owner of the rotating
	// refresh_token for the global codex (ChatGPT/Codex subscription) credential
	// (global_secrets key CODEX_AUTH_JSON). Multiple codex workspaces share ONE
	// ChatGPT-Pro OAuth token; OpenAI's refresh_token is single-use, so letting
	// each per-agent app-server refresh on its own 401 burned the seed within
	// seconds (a refresh storm). This goroutine is structurally single-flight
	// (one goroutine + a package mutex), refreshes only within a safety margin
	// of expiry, POSTs the refresh_token at most once per due cycle, and writes
	// the rotated blob back — workspaces now only GET the current token (see the
	// codex template's codex_auth_sync.sh). INERT when no CODEX_AUTH_JSON exists.
	go supervised.RunWithRecover(ctx, "codex-auth-refresher", func(c context.Context) {
		codexauth.StartCodexAuthRefresher(c, db.DB)
	})

	// RFC internal#742 Part 2: wire the boot-failure rescue capture into
	// the provision-timeout sweep's failure verdict. When the sweep flips
	// a stuck workspace to `failed`, this hook captures a forensic rescue
	// bundle off the still-running (but boot-failed) EC2 and ships it to
	// obs/Loki before the control plane reaps the instance. Best-effort +
	// non-blocking (handlers.BootFailureRescueHook dispatches on its own
	// goroutine + timeout). The handler-side boot-failure path
	// (WorkspaceHandler.BootstrapFailed) wires its own capture inline.
	registry.BootFailureRescueHook = handlers.BootFailureRescueHook

	// Provision-timeout sweep — flips workspaces that have been stuck in
	// status='provisioning' past the timeout window to 'failed' and emits
	// WORKSPACE_PROVISION_TIMEOUT. Without this the UI banner is cosmetic
	// and the state is incoherent (e.g. user sees "Retry" after 15min but
	// backend still thinks provisioning is in progress).
	go supervised.RunWithRecover(ctx, "provision-timeout-sweep", func(c context.Context) {
		// Pass the handler's per-runtime template-manifest lookup so the
		// sweeper honours `runtime_config.provision_timeout_seconds`
		// declared in any template's config.yaml — the same value the
		// canvas already reads via addProvisionTimeoutMs. Without this
		// the sweeper killed claude-code at the 10-min hardcoded floor
		// regardless of the manifest. See registry.RuntimeTimeoutLookup.
		registry.StartProvisioningTimeoutSweep(c, broadcaster, registry.DefaultProvisionSweepInterval, wh.ProvisionTimeoutSecondsForRuntime)
	})

	// Cron Scheduler — fires A2A messages to workspaces on user-defined schedules
	cronSched := scheduler.New(wh, broadcaster)
	// Wire the native-scheduler skip — when an adapter's heartbeat
	// declares provides_native_scheduler=true, the platform's polling
	// loop drops that workspace's schedules to avoid double-fire (the
	// SDK runs them itself). See project memory
	// `project_runtime_native_pluggable.md` and capability primitive #3.
	cronSched.SetNativeSchedulerCheck(handlers.ProvidesNativeScheduler)
	go supervised.RunWithRecover(ctx, "scheduler", cronSched.Start)

	// Hibernation Monitor — auto-pauses idle workspaces that have
	// hibernation_idle_minutes configured (#711). Wakeup is triggered
	// automatically on the next incoming A2A message.
	go supervised.RunWithRecover(ctx, "hibernation-monitor", func(c context.Context) {
		registry.StartHibernationMonitor(c, wh.HibernateWorkspace)
	})

	// RFC #2829 PR-3: stuck-task sweeper for the durable delegations
	// ledger. Marks deadline-exceeded rows as failed and heartbeat-stale
	// in-flight rows as stuck. Both transitions go through the ledger's
	// terminal forward-only protection so concurrent UpdateStatus calls
	// are not clobbered. Defaults: 5min interval, 10min stale threshold;
	// override via DELEGATION_SWEEPER_INTERVAL_S / DELEGATION_STUCK_THRESHOLD_S.
	delegSweeper := handlers.NewDelegationSweeper(nil, nil)
	go supervised.RunWithRecover(ctx, "delegation-sweeper", delegSweeper.Start)

	// RFC unified-requests-inbox P4: idle-agent inbox-nudge sweeper. Pokes
	// an IDLE online agent that has unhandled `requests` inbox items (stale
	// >10min) with one A2A nudge so it re-checks its inbox, rate-limited to
	// <=1 nudge per request per hour via requests.last_nudged_at. No-op until
	// the P1 `requests` table (#2525) + the last_nudged_at column have rolled
	// out. Disable via REQUEST_NUDGE_SWEEPER_DISABLED=true; tune cadence via
	// REQUEST_NUDGE_SWEEPER_INTERVAL_S.
	if !strings.EqualFold(os.Getenv("REQUEST_NUDGE_SWEEPER_DISABLED"), "true") {
		nudgeSweeper := handlers.NewRequestNudgeSweeper(nil)
		go supervised.RunWithRecover(ctx, "request-nudge-sweeper", nudgeSweeper.Start)
	}

	// Channel Manager — social channel integrations (Telegram, Slack, etc.)
	channelMgr := channels.NewManager(wh, broadcaster)
	go supervised.RunWithRecover(ctx, "channel-manager", channelMgr.Start)

	// Image auto-refresh — closes the runtime CD chain to "merge → containers
	// running new code" with no human in between. Polls GHCR for digest
	// changes on workspace-template-* :latest tags and invokes the same
	// refresh logic /admin/workspace-images/refresh exposes. Opt-in:
	// SaaS deploys whose pipeline already pulls every release should leave
	// it off (would be redundant work). Self-hosters get true zero-touch.
	if prov != nil && strings.EqualFold(os.Getenv("IMAGE_AUTO_REFRESH"), "true") {
		svc := handlers.NewWorkspaceImageService(prov.DockerClient())
		watcher := imagewatch.New(svc)
		go supervised.RunWithRecover(ctx, "image-auto-refresh", watcher.Run)
	}

	// Wire channel manager into scheduler for auto-posting cron output to Slack
	cronSched.SetChannels(channelMgr)

	// Router
	// Plugin registry — created before Setup so the same registry is shared
	// between the PluginsHandler (for installs) and the drift sweeper (for
	// drift detection). github:// sources always work; local:// sources
	// require a plugins/ dir on disk (nil in CP/SaaS mode).
	pluginRegistry := plugins.NewRegistry()
	pluginRegistry.Register(plugins.NewGithubResolver())
	refreshTemplatesHTTP := func(c *gin.Context) (any, error) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
		defer cancel()
		return refreshTemplates(ctx)
	}
	r := router.Setup(hub, broadcaster, prov, platformURL, configsDir, templateCacheDir, wh, channelMgr, memBundle, pluginRegistry, refreshTemplatesHTTP)

	// Plugin drift sweeper — periodic detection of upstream plugin version drift
	// (core#123). Scans workspace_plugins rows where tracked_ref != 'none',
	// resolves the current upstream SHA for each tracked ref, and queues drift
	// entries when the upstream has moved. Only runs when pluginResolver is
	// non-nil (CP/SaaS mode has no local git and the sweeper is a no-op there).
	// Nil prov: Docker not available (test harness / local dev without Docker).
	go supervised.RunWithRecover(ctx, "plugin-drift-sweeper", func(c context.Context) {
		plugins.StartPluginDriftSweeper(c, pluginRegistry)
	})

	// HTTP server with graceful shutdown.
	//
	// Bind host: in local dev (MOLECULE_ENV=dev|development) default the
	// listener to loopback as defense-in-depth — a dev box shouldn't be
	// reachable from the LAN. This is NOT an auth lever (auth is fail-closed
	// in every env now); it's strictly the safer default. Operators who need
	// LAN exposure set BIND_ADDR=0.0.0.0 explicitly. Production binds all
	// interfaces (existing shape). See molecule-core#7.
	bindHost := resolveBindHost()
	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%s", bindHost, port),
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Start server in goroutine
	go func() {
		log.Printf("Platform starting on %s:%s (local-dev-env=%v)", bindHost, port, middleware.IsLocalDevEnv())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down gracefully...")

	// Cancel background goroutines (liveness monitor, Redis subscriber)
	cancel()

	// Drain HTTP connections (30s timeout)
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Server forced shutdown: %v", err)
	}

	// Close WebSocket hub
	hub.Close()

	log.Println("Platform stopped")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// resolveBindHost picks the listener interface for the HTTP server.
//
// Precedence:
//  1. BIND_ADDR — explicit operator override (any value, including "0.0.0.0").
//  2. local dev (MOLECULE_ENV=dev|development) → "127.0.0.1" (loopback only).
//  3. otherwise → "" (Go binds every interface; existing prod/self-host shape).
//
// NOTE (harden/no-fail-open-auth): this is a defense-in-depth default, NOT an
// auth lever. Auth is fail-closed in every environment now, so the loopback
// default no longer compensates for a weak auth chain — it simply keeps a dev
// box off the LAN by default. It is keyed on MOLECULE_ENV alone (decoupled
// from ADMIN_TOKEN), because dev now provisions an ADMIN_TOKEN yet should
// still default to loopback. See molecule-core#7 for the original LAN finding.
func resolveBindHost() string {
	if v := os.Getenv("BIND_ADDR"); v != "" {
		return v
	}
	if middleware.IsLocalDevEnv() {
		return "127.0.0.1"
	}
	return ""
}

func findConfigsDir() string {
	candidates := []string{
		"workspace-configs-templates",
		"../workspace-configs-templates",
		"../../workspace-configs-templates",
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			// Verify the directory has at least one template with a config.yaml
			entries, _ := os.ReadDir(c)
			hasTemplate := false
			for _, e := range entries {
				if e.IsDir() {
					if _, err := os.Stat(filepath.Join(c, e.Name(), "config.yaml")); err == nil {
						hasTemplate = true
						break
					}
				}
			}
			if !hasTemplate {
				continue
			}
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return "workspace-configs-templates"
}

func findWorkspaceManifestPath() string {
	if v := os.Getenv("WORKSPACE_MANIFEST_PATH"); v != "" {
		return v
	}
	for _, p := range []string{"/app/manifest.json", "manifest.json", "../manifest.json", "../../manifest.json"} {
		if abs, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	return ""
}

func templateCacheToken() string {
	for _, key := range []string{"MOLECULE_TEMPLATE_GITEA_TOKEN", "MOLECULE_GITEA_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

func shouldRefreshTemplateCache(token, manifestPath string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TEMPLATE_CACHE_REFRESH"))) {
	case "0", "false", "off", "no":
		return false
	case "1", "true", "on", "yes":
		return token != "" && manifestPath != ""
	default:
		return token != "" && manifestPath != ""
	}
}

func findMigrationsDir() string {
	candidates := []string{
		"migrations",
		"platform/migrations",
		"../migrations",
		"../../migrations",
	}

	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "migrations"),
			filepath.Join(dir, "..", "migrations"),
			filepath.Join(dir, "..", "..", "migrations"),
		)
	}

	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			abs, _ := filepath.Abs(c)
			log.Printf("Found migrations at: %s", abs)
			return abs
		}
	}
	log.Println("No migrations directory found")
	return ""
}

// fixAdminTokenPlaceholder heals #831: workspaces provisioned with a placeholder
// ADMIN_TOKEN in global_secrets receive that placeholder as a container env var,
// breaking any code that calls platform APIs. This runs once at startup (SaaS only)
// and replaces the placeholder with the real token from the host environment.
//
// The placeholder is not in the codebase — it was seeded by a prior bootstrap or
// manual DB write. It should never be set by the platform itself. This function
// ensures it is corrected on next platform restart without requiring a manual DB
// update or workspace reprovision.
func fixAdminTokenPlaceholder() {
	realToken := os.Getenv("ADMIN_TOKEN")
	if realToken == "" {
		// Platform has no ADMIN_TOKEN — nothing to fix.
		return
	}

	// Read the current stored value. We only upsert when the placeholder is
	// present so we don't repeatedly write rows that are already correct.
	var storedValue []byte
	err := db.DB.QueryRow(`SELECT encrypted_value FROM global_secrets WHERE key = $1`, "ADMIN_TOKEN").Scan(&storedValue)
	if err != nil {
		// No row — nothing to fix. The control plane injects ADMIN_TOKEN via
		// Secrets Manager bootstrap; the global_secrets path is a legacy seed.
		return
	}

	// Decrypt to check the value. We compare the plaintext so the check works
	// whether encryption is enabled or not.
	storedPlaintext, decErr := crypto.DecryptVersioned(storedValue, crypto.CurrentEncryptionVersion())
	if decErr != nil {
		log.Printf("fixAdminTokenPlaceholder: could not decrypt existing value (version mismatch?): %v", decErr)
		return
	}

	if string(storedPlaintext) == realToken {
		// Already correct — nothing to do.
		return
	}

	if string(storedPlaintext) == "placeholder-will-ask-for-real" {
		log.Println("fixAdminTokenPlaceholder: replacing placeholder ADMIN_TOKEN in global_secrets")
	} else {
		log.Printf("fixAdminTokenPlaceholder: ADMIN_TOKEN in global_secrets differs from env; updating")
	}

	encrypted, err := crypto.Encrypt([]byte(realToken))
	if err != nil {
		log.Printf("fixAdminTokenPlaceholder: failed to encrypt: %v", err)
		return
	}

	_, err = db.DB.Exec(`
		INSERT INTO global_secrets (key, encrypted_value, encryption_version)
		VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE
			SET encrypted_value = $2, encryption_version = $3, updated_at = now()
	`, "ADMIN_TOKEN", encrypted, crypto.CurrentEncryptionVersion())
	if err != nil {
		log.Printf("fixAdminTokenPlaceholder: failed to upsert: %v", err)
		return
	}
	log.Println("fixAdminTokenPlaceholder: done")
}

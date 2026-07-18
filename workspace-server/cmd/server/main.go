// Package main runs the per-tenant workspace-server.
//
//	@title			Molecule AI Workspace Server API
//	@version		1.0
//	@description	The per-tenant workspace-server HTTP API. Single source of truth for workspace/schedule/agent/secrets/files/memory CRUD. Hand-written clients (canvas, molecule-mcp-server, molecule-cli, molecule-ai-sdk) should be replaced by clients generated from this spec — see RFC #1706.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/channels"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/codexauth"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/crypto"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/envx"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/handlers"
	memwiring "git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/memory/wiring"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/middleware"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/pendinguploads"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/plugins"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/registry"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/router"
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

// isSaaSDeployment reports whether this tenant platform is
// running in managed control-plane mode (mirrors handlers.saasMode;
// duplicated here because the helpers package is unexported
// and main.go is a separate package — would be a cycle).
//
// Resolution order:
//  1. MOLECULE_DEPLOY_MODE set — explicit operator flag is authoritative.
//     "saas" → true. "self-hosted"/"selfhosted"/"standalone" → false.
//     Unknown values log a warning + fall closed to false.
//  2. MOLECULE_DEPLOY_MODE unset — fall back to MOLECULE_ORG_ID presence.
func isSaaSDeployment() bool {
	raw := strings.TrimSpace(os.Getenv("MOLECULE_DEPLOY_MODE"))
	if raw != "" {
		switch strings.ToLower(raw) {
		case "saas":
			return true
		case "self-hosted", "selfhosted", "standalone":
			return false
		default:
			log.Printf("isSaaSDeployment: MOLECULE_DEPLOY_MODE=%q not recognised; falling back to strict (non-SaaS) mode. Valid values: saas | self-hosted.", raw)
			return false
		}
	}
	return strings.TrimSpace(os.Getenv("MOLECULE_ORG_ID")) != ""
}

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
	// Managed-tenant boot (cp#469 — tenant proxy-env delivery): fetch the
	// CP-delivered env (incl. the required LLM proxy creds) via refreshEnvFromCP,
	// retrying a transient startup 401 for a bounded window. A freshly-provisioned
	// tenant can call the CP BEFORE it commits our org_instances row (the token
	// lookup 401s and the LLM env is never delivered) — a race fast backends
	// can expose and slower provider boots can mask. A managed SaaS tenant that
	// still lacks the required LLM proxy vars after the retry window fatals loudly
	// — silent-fail is worse than a loud refusal. Self-hosted (no orgID/adminToken)
	// short-circuits inside and never retries — byte-identical to before. The
	// refresh also heals older tenants whose MOLECULE_CP_SHARED_SECRET is unset.
	// (core#4485) A Molecule-managed tenant (MOLECULE_ORG_ID set) MUST provide
	// ADMIN_TOKEN. Without it, AdminAuth's deprecated Tier-3 fallback
	// (wsauth_middleware.go:268-280) accepts any live workspace token as
	// org-admin — a workspace self-surface token could then manage the org /
	// create-delete-restart sibling workspaces. Assert BEFORE
	// ensureManagedTenantLLMEnv, whose managed-tenant detection (refreshEnvFromCP
	// / assertManagedTenantHasLLMEnv) silently no-ops when ADMIN_TOKEN is unset.
	// Self-hosted / local-dev (no MOLECULE_ORG_ID) is exempt and keeps Tier-3.
	if err := assertSaaSTenantHasAdminToken(); err != nil {
		log.Fatalf("Managed tenant boot refused: %v", err)
	}
	if err := ensureManagedTenantLLMEnv(); err != nil {
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

	// Self-hosted platform-agent seed — UNCONDITIONAL on self-host (core#3496,
	// operator ruling 2026-07-07: "the first agent and manager agent should
	// always be the concierge"). With no control plane present to install the
	// org's concierge, the tenant server seeds the row itself on EVERY boot —
	// the old MOLECULE_SEED_PLATFORM_AGENT opt-in flag is removed. The gate is
	// MOLECULE_ORG_ID: SaaS/CP tenants and CI harnesses always set it and never
	// self-seed (byte-identical to their old flag-unset behavior); a tombstoned
	// root is respected, never silently revived. Idempotent + best-effort —
	// never fatal. Row-only: the boot provision is phase 2 below, after the
	// provisioner exists.
	if handlers.SelfHostPlatformSeedEnabled() {
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

	// Activity log retention — configurable via env vars.
	//
	// MUST-FIX 3: the prune is now age-AND-acked with a hard ceiling instead
	// of age-only. softDays (ACTIVITY_RETENTION_DAYS, default 7) reclaims a
	// row only once the workspace's inbox poller has acked past it; hardDays
	// (ACTIVITY_HARD_RETENTION_DAYS, default 30) is the unconditional backstop
	// so a permanently-silent consumer can't pin rows forever. See
	// db.PruneActivityLogs — it is provably never less conservative than the
	// old age-only prune (and clamps hard >= soft to keep that guarantee).
	softDays := 7
	if n, err := strconv.Atoi(envOr("ACTIVITY_RETENTION_DAYS", "7")); err == nil && n > 0 {
		softDays = n
	}
	hardDays := 30
	if n, err := strconv.Atoi(envOr("ACTIVITY_HARD_RETENTION_DAYS", "30")); err == nil && n > 0 {
		hardDays = n
	}
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
				n, err := db.PruneActivityLogs(ctx, softDays, hardDays)
				if err != nil {
					log.Printf("Activity log cleanup error: %v", err)
				} else if n > 0 {
					log.Printf("Activity log cleanup: purged %d old entries", n)
				}
			}
		}
	}()

	// Provisioner — auto-detect backend:
	//   1. MOLECULE_ORG_ID set → SaaS tenant → control plane provisioner
	//   2. Docker available     → self-hosted → Docker provisioner
	//   3. Neither              → provisioner disabled (external agents only)
	// hostStateDir is the base dir for the per-workspace host-side /configs
	// mirror the Files API serves docker-less reads from (#206 molecules-server:
	// the tenant has no docker.sock into the runtime container). Resolved ONCE
	// here so the mirror WRITER (CPProvisioner.WithHostStateDir) and the READER
	// (TemplatesHandler, wired in router.Setup) are handed the SAME value and can
	// never drift. Local to the tenant — no CP, no R2 (core-OSS-clean).
	hostStateDir := provisioner.ResolveWorkspaceStateBaseDir()

	// CORE-served boot-config token delivery (the FINAL, platform-agnostic config
	// path — no R2, no CP dependency). Dark by default: create the ONE shared
	// token store only when MOLECULE_BOOT_CONFIG_ENABLE is truthy, and hand the
	// SAME instance to the mint side (CPProvisioner) and the serve side
	// (BootConfigHandler, wired in router.Setup) so they cannot drift. When nil,
	// no token is minted and the boot-config endpoint 404s — byte-identical to a
	// deployment that never had the feature.
	var bootTokens *provisioner.BootConfigTokenStore
	if envx.Bool("MOLECULE_BOOT_CONFIG_ENABLE", false) {
		ttl := provisioner.BootConfigTokenTTL
		if d, derr := time.ParseDuration(strings.TrimSpace(os.Getenv("MOLECULE_BOOT_CONFIG_TTL"))); derr == nil && d > 0 {
			ttl = d
		}
		bootTokens = provisioner.NewBootConfigTokenStore(ttl)
		log.Printf("Boot-config token delivery: ENABLED (ttl=%s) — runtime fetches config from the tenant-server at boot (no R2, no CP)", ttl)
	}

	var prov *provisioner.Provisioner
	var cpProv *provisioner.CPProvisioner
	if os.Getenv("MOLECULE_ORG_ID") != "" {
		// SaaS tenant — provision through the control plane's selected backend.
		if cp, err := provisioner.NewCPProvisioner(); err != nil {
			log.Printf("Control plane provisioner unavailable: %v", err)
		} else {
			cpProv = cp
			cpProv.WithHostStateDir(hostStateDir)
			cpProv.WithBootConfigTokenStore(bootTokens)
			defer cpProv.Close()
			log.Println("Provisioner: Control Plane (auto-detected SaaS tenant)")
		}
	} else {
		// Self-hosted — use local Docker daemon
		if p, err := provisioner.New(); err != nil {
			log.Printf("Provisioner disabled (Docker not available): %v", err)
		} else {
			// Provisioning-phase boot telemetry → BOOT_STEP broadcasts, as
			// step 1 of the boot family ("Provision compute" — the one step
			// the runtime can never emit because it is not running yet; see
			// events.EventBootStep). The runtime's own steps take the family
			// over once the container is up. Without this the canvas
			// watchdog is silent for the whole provisioning phase — a
			// first-boot local image build is 5+ minutes of "waiting for
			// boot telemetry" that reads as a hang.
			p.SetBootStepEmitter(func(workspaceID, status, message string) {
				// Logged as well as broadcast: BOOT_STEP is broadcast-only
				// (never persisted), so this line is the only durable
				// record that provisioning telemetry fired — operators
				// correlate it with local-build timings, and the lifecycle
				// e2e greps it to prove this wiring stays connected.
				log.Printf("boot-telemetry: workspace=%s step=1/8 key=PWR status=%s msg=%q", workspaceID, status, message)
				broadcaster.BroadcastOnly(workspaceID, string(events.EventBootStep), map[string]interface{}{
					"workspace_id": workspaceID,
					"step":         1,
					"total":        8,
					"key":          "PWR",
					"label":        "Provision compute",
					"status":       status,
					"message":      message,
				})
			})
			prov = p
			defer prov.Close()
			log.Println("Provisioner: Docker")
		}
	}

	// Workspace privilege model: tenant ADMIN_TOKEN is only for the concierge's
	// management MCP and CP request auth, never ordinary workspace env. Remove
	// legacy global_secrets rows so old bootstrap seeds do not keep re-injecting
	// the admin bearer through loadWorkspaceSecrets.
	if cpProv != nil {
		deleteLegacyAdminTokenGlobalSecrets()
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
		WithTemplateCacheDir(templateCacheDir).
		// internal#3211: wire the SAME refresh mechanism POST
		// /admin/templates/refresh uses so a create/provision for a KNOWN
		// non-claude-code runtime whose template is a cache MISS auto-refreshes
		// the cache before seeding, then fails LOUD if still missing — never a
		// silent claude-code substitution. The report is discarded; the handler
		// only needs success/failure.
		WithTemplateRefresh(func(ctx context.Context) error {
			_, err := refreshTemplates(ctx)
			return err
		})
	if cpProv != nil {
		wh.SetCPProvisioner(cpProv)
	}

	// #2930: independent A2A queue sweeper so queued requests drain even when a
	// workspace stops heartbeating (e.g., after a transient restart trigger).
	go wh.StartA2AQueueSweeper(ctx)

	// PR-B (RFC #2843 #24): wire the Gitea TemplateAssetFetcher.
	// nil-if-empty + WARN: if the token isn't set, log a warning
	// and stay in the "no fetcher wired" state. The SCAFFOLD gate
	// in collectCPConfigFiles treats nil fetcher as "skip the
	// fetcher" — pre-scaffold behavior preserved for self-host /
	// unconfigured tenants. baseURL has a production default
	// (https://git.moleculesai.app) but is overridable for staging
	// or per-deployment Gitea mirrors.
	// PR-B keystone (RFC #2843 #24): wire the template-asset fetcher
	// via the selection helper. SaaS deployments get the real
	// Gitea fetcher (public-fetch when MOLECULE_TEMPLATE_REPO_TOKEN
	// is empty per the CTO public-fetch GO; authenticated when set
	// for the future private-template / rate-limit CTO-grant item).
	// Self-host deployments get the no-op fetcher (self-host uses
	// the local TemplatePath + ConfigFiles path for /configs and
	// does not need an external asset channel). The token is
	// OPTIONAL for SaaS (the molecule-ai/* template repos are
	// PUBLIC — verified: GET /repos/.../archive/main.tar.gz returns
	// 200 with no Authorization header). baseURL has a production
	// default (https://git.moleculesai.app) but is overridable via
	// MOLECULE_GITEA_BASE_URL for staging or per-deployment Gitea
	// mirrors.
	token := templateRepoToken()
	baseURL := envOr("MOLECULE_GITEA_BASE_URL", "https://git.moleculesai.app")
	sel := provisioner.SelectTemplateAssetFetcher(isSaaSDeployment, baseURL, token)
	wh.SetGiteaTemplateFetcher(sel.Fetcher)
	switch sel.Mode {
	case "self-host-noop":
		log.Printf("template repo fetcher: wired (no-op — self-host default, no external asset channel)")
	default:
		if sel.Authenticated {
			log.Printf("template repo fetcher: wired (baseURL=%q, SaaS, token set — authenticated)", baseURL)
		} else {
			log.Printf("template repo fetcher: wired (baseURL=%q, SaaS, no token — public unauthenticated fetch)", baseURL)
		}
	}

	// Self-hosted platform-agent boot-provision (Change 1). The line-128 seed
	// only creates the concierge DB ROW; on a fresh self-host that leaves it
	// with no container (status='failed'/'online' but nothing running). Now that
	// the local Docker provisioner (prov) and WorkspaceHandler (RestartByID)
	// exist, kick off a best-effort provision so a self-hosted concierge comes
	// online automatically once LLM creds exist.
	//
	// Guarded to self-host ONLY: SelfHostPlatformSeedEnabled() (MOLECULE_ORG_ID
	// unset — same gate as the phase-1 seed; the old flag is removed, core#3496)
	// AND prov != nil (local Docker active). The SaaS path (cpProv != nil ⇒
	// prov == nil) never triggers — the CP owns concierge provisioning there.
	// Best-effort + non-fatal + runs once: an UNCONFIGURED root (no model
	// signal) is parked at 'offline' for the onboarding scene instead of
	// burning a guaranteed-failed provision (D2 posture, inside the helper);
	// a configured root with a missing/wrong key still fails loudly — a real
	// error state the user must see. RestartByID is itself debounced so this
	// can't loop. Runs in a goroutine inside the helper so a slow image pull
	// never delays the HTTP server.
	if handlers.SelfHostPlatformSeedEnabled() && prov != nil {
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

	// 2026-06-19 a2a RCA (#3057): wedged-agent handler. Initial
	// implementation is log + broadcast only — auto-restart is a
	// follow-up gated on ops review (a wedge can mask a busy agent
	// that's just slow; restarting such an agent loses in-flight
	// state). The broadcast event lets the canvas flag the wedge
	// status and operators inspect the tuple.
	onWorkspaceWedged := func(innerCtx context.Context, workspaceID string) {
		if err := broadcaster.RecordAndBroadcast(innerCtx, "WORKSPACE_WEDGED", workspaceID, map[string]interface{}{
			"reason": "active_tasks>0 with no outbound A2A and no heartbeat — alive-but-wedged",
		}); err != nil {
			log.Printf("Wedged broadcast error for %s: %v", workspaceID, err)
		}
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

	// 2026-06-19 a2a RCA (#3057): a separate monitor for the
	// alive-but-wedged case (active_tasks>0, no outbound, no heartbeat)
	// that the existing health-sweep misses because the Docker container
	// is still up (TCP connect succeeds) and the dead-origin HTTP-status
	// check isUpstreamDeadStatus is not triggered. Initial handler is
	// log-only; a gated auto-restart is a follow-up.
	go supervised.RunWithRecover(ctx, "wedged-agent-monitor", func(c context.Context) {
		registry.StartWedgedAgentMonitor(c, onWorkspaceWedged)
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
	// calling CP DELETE, so a transient CP failure leaves provider compute
	// running with no retry path. cpProv.Stop is idempotent against
	// already-terminated instances; on success we clear instance_id.
	if cpProv != nil {
		go supervised.RunWithRecover(ctx, "cp-orphan-sweeper", func(c context.Context) {
			registry.StartCPOrphanSweeper(c, cpProv)
		})
	}

	// CP-mode instance-state reconciler — authoritative provider-liveness pass
	// for SaaS workspaces (core#2261). Every other liveness sweep keys off
	// a PROXY (Redis TTL, agent heartbeat, local Docker, or
	// runtime='external'); a SaaS workspace whose provider host was
	// terminated/stopped falls through ALL of them and stays status='online'
	// pointing at a dead instance_id forever (root cause: core#2247). This
	// loop asks the ONE authoritative question the others lack —
	// cpProv.IsRunning (the CP provider-state query) — for each online
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
	// bundle off the still-running (but boot-failed) provider host and ships it to
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

	// Cron scheduling is no longer run by core (scheduler-as-trigger-plugin
	// RFC, P4). Every maintained runtime carries the kind:trigger scheduler
	// plugin, so schedules are volume-authoritative and fired by the runtime
	// daemon; core only proxies the Canvas/admin CRUD surface to the volume
	// (internal/handlers/schedules_proxy.go). The old poll-and-fire loop
	// (internal/scheduler) and the workspace_schedules table have been retired.
	// The native-scheduler capability check that used to gate double-fire is
	// therefore unnecessary — there is no core loop left to skip.
	//
	// The phantom-busy counter-drift sweep that used to ride the scheduler's
	// tick loop survives as its own worker: active_tasks > 0 gates A2A
	// dispatch / hibernation / discovery platform-wide, so a drifted counter
	// must still be reset even though no core cron loop reads it any more.
	// Disable via PHANTOM_BUSY_SWEEPER_DISABLED=true.
	if !strings.EqualFold(os.Getenv("PHANTOM_BUSY_SWEEPER_DISABLED"), "true") {
		phantomSweeper := handlers.NewPhantomBusySweeper(nil)
		go supervised.RunWithRecover(ctx, "phantom-busy-sweeper", phantomSweeper.Start)
	} else {
		log.Printf("PhantomBusySweeper: disabled via PHANTOM_BUSY_SWEEPER_DISABLED")
	}

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
	// Shout if the two delegation flags are half-flipped: LEDGER_WRITE on with
	// INBOX_PUSH off makes the sweeper terminalize delegations that then vanish
	// from the caller's "awaiting reply" list with no reply ever sent (#4314).
	handlers.WarnOnPartialDelegationRollout()
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

	// Agent-Liveness RFC, Layer 3 (A2): stall-watchdog. Catches the
	// "busy but silently hung" case the Redis TTL liveness monitor and the
	// status='failed' watchdog both miss — a workspace that is status='online'
	// with active_tasks>0 but has produced NO activity for too long (the case
	// that let JRS sit dead ~2.5h). Two-stage: probe via A2A, then soft-restart
	// (existing-volume, wh.RestartByID — the same path POST /workspaces/:id/
	// restart uses) if still silent past the grace window; anti-flap cooldown.
	// Defaults: 3min sweep, 12min stale, 5min probe-grace, 30min cooldown;
	// override via STALL_WATCHDOG_*_S. Disable via STALL_WATCHDOG_DISABLED=true.
	if !strings.EqualFold(os.Getenv("STALL_WATCHDOG_DISABLED"), "true") {
		stallWatchdog := handlers.NewStallWatchdog(nil, wh.RestartByID)
		go supervised.RunWithRecover(ctx, "stall-watchdog", stallWatchdog.Start)
	} else {
		log.Printf("StallWatchdog: disabled via STALL_WATCHDOG_DISABLED")
	}

	// Channel Manager — social channel integrations (Telegram, Slack, etc.)
	channelMgr := channels.NewManager(wh, broadcaster)
	go supervised.RunWithRecover(ctx, "channel-manager", channelMgr.Start)

	// Router
	// Plugin registry — created before Setup so the same registry is shared
	// between the PluginsHandler (for installs) and the drift sweeper (for
	// drift detection). github:// sources always work; local:// sources
	// require a plugins/ dir on disk (nil in CP/SaaS mode).
	pluginRegistry := plugins.NewRegistry()
	pluginRegistry.Register(plugins.NewGithubResolver())
	// gitea:// — private-repo-subpath resolver used by declared plugins
	// (RFC#2843). Shared with the drift sweeper so gitea-sourced plugins
	// get drift detection too.
	pluginRegistry.Register(plugins.NewGiteaResolver())
	refreshTemplatesHTTP := func(c *gin.Context) (any, error) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
		defer cancel()
		return refreshTemplates(ctx)
	}
	r := router.Setup(hub, broadcaster, prov, platformURL, configsDir, templateCacheDir, hostStateDir, bootTokens, wh, channelMgr, memBundle, pluginRegistry, refreshTemplatesHTTP)

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

// templateRepoToken returns the per-deployment READ-ONLY Gitea PAT
// used by the Gitea TemplateAssetFetcher (RFC #2843 #24 PR-B).
// Distinct from templateCacheToken (which is for the template cache,
// a different feature) so a tenant can rotate the fetcher token
// without touching the cache token. nil-if-empty + WARN: callers
// should treat empty as "fetcher disabled" (self-host default — the
// SCAFFOLD gate in collectCPConfigFiles treats nil fetcher as
// "skip the fetcher", pre-scaffold behavior preserved).
func templateRepoToken() string {
	return strings.TrimSpace(os.Getenv("MOLECULE_TEMPLATE_REPO_TOKEN"))
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

func deleteLegacyAdminTokenGlobalSecrets() {
	res, err := db.DB.Exec(`
		DELETE FROM global_secrets
		WHERE key IN ('ADMIN_TOKEN', 'MOLECULE_ADMIN_TOKEN')
	`)
	if err != nil {
		log.Printf("deleteLegacyAdminTokenGlobalSecrets: failed: %v", err)
		return
	}
	if rows, _ := res.RowsAffected(); rows > 0 {
		log.Printf("deleteLegacyAdminTokenGlobalSecrets: removed %d legacy admin-token global secret row(s)", rows)
	}
}


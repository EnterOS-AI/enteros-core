package handlers

// org_import.go — workspace tree creation during org template import.
// Contains createWorkspaceTree (recursive provisioning) and countWorkspaces.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/channels"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/crypto"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/plugins"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provlog"
	"github.com/google/uuid"
)

// createWorkspaceTree recursively materialises an OrgWorkspace (and its
// descendants) into the workspaces + canvas_layouts tables and kicks off
// Docker provisioning. absX/absY are THIS workspace's absolute canvas
// coordinates — roots inherit them from ws.Canvas, children receive
// parent.abs + childSlotInGrid(index, siblingSizes) computed by the
// caller. Storing already-absolute coords means a child that is itself
// a parent can simply compound the grid without any per-call math.
// relX / relY are THIS workspace's position RELATIVE to its parent's
// absolute origin (i.e. childSlotInGrid output for children; 0,0 for
// roots since a root's absolute IS its relative). The broadcast
// payload ships relative coords so the canvas can drop the node
// straight into the parent's child-coordinate space without doing a
// canvas-wide absolute-position walk.
func (h *OrgHandler) createWorkspaceTree(ws OrgWorkspace, parentID *string, absX, absY, relX, relY float64, defaults OrgDefaults, orgBaseDir string, results *[]map[string]interface{}, provisionSem chan struct{}) error {
	// spawning: false guard — skip this workspace AND all descendants.
	// Pointer-typed so we distinguish "explicitly false" from "unset"
	// (unset = default to spawn). The guard sits BEFORE any side effect
	// (no DB row, no docker provision, no children recursion) so a
	// false-spawning subtree is genuinely a no-op except for the log line.
	// Use case: dev-tree org template ships the full role taxonomy but a
	// developer's machine only has RAM for a subset; a per-workspace
	// `spawning: false` lets them narrow without editing the parent
	// template's structure.
	if ws.Spawning != nil && !*ws.Spawning {
		log.Printf("Org import: skipping workspace %q (spawning=false; %d descendant workspace(s) in subtree also skipped)", ws.Name, countWorkspaces(ws.Children))
		return nil
	}

	// Apply defaults. Explicit ws.Runtime wins, then the org template's
	// defaults.Runtime; only the final unset fallback FOLLOWS the platform
	// default SSOT (MOLECULE_DEFAULT_RUNTIME, KMS-injected) via
	// bareCreateDefaultRuntime instead of a baked runtime literal.
	runtime := ws.Runtime
	if runtime == "" {
		runtime = defaults.Runtime
	}
	if runtime == "" {
		runtime = bareCreateDefaultRuntime()
	}
	model := ws.Model
	if model == "" {
		model = defaults.Model
	}
	if model == "" {
		// SSOT (CTO 2026-05-22, feedback_workspace_model_required_no_platform_default_dynamic_credential_intake):
		// model is REQUIRED. The org-import template MUST declare a
		// model — either per-workspace (`ws.Model`) or via the org
		// defaults block (`defaults.Model`). If neither is present
		// the template is malformed and the import must fail-closed
		// rather than silently provisioning a workspace with a
		// runtime-incompatible default (the prior `anthropic:claude-opus-4-7`
		// fallback wedged every codex workspace at adapter init).
		return fmt.Errorf("org import: workspace %q has no model and the org defaults block does not provide one (runtime=%s) — model is a required field per the workspace-creation contract; either set `model:` on the workspace or under `defaults:`", ws.Name, runtime)
	}
	tier := ws.Tier
	if tier == 0 {
		tier = defaults.Tier
	}
	if tier == 0 {
		// Resolved via the same DefaultTier helper Create + Templates
		// use (#2910 PR-E). SaaS → T4 (dedicated managed compute),
		// self-hosted → T3. Pre-#2910
		// this path returned T2 on self-hosted, asymmetric with
		// workspace.go's T3 — undocumented drift. Lifting to
		// DefaultTier collapses both call sites onto one source of
		// truth so a future tier-default change sweeps every entry
		// point at once. Templates that want a different floor still
		// declare `tier:` in config.yaml or `defaults.tier` in
		// org.yaml.
		if h.workspace != nil {
			tier = h.workspace.DefaultTier()
		} else {
			tier = 3
		}
	}

	id := uuid.New().String()

	var role interface{}
	if ws.Role != "" {
		role = ws.Role
	}

	// Expand ${VAR} references in workspace_dir against the org's .env files
	// before validation. Without this, a template that ships
	// `workspace_dir: ${WORKSPACE_DIR}` (so each operator can pick the host
	// path to bind-mount) reaches validateWorkspaceDir as the literal
	// "${WORKSPACE_DIR}" string and fails with "must be an absolute path".
	// Other fields (channel config, prompts) already go through expandWithEnv;
	// workspace_dir was the last hold-out.
	if ws.WorkspaceDir != "" {
		ws.WorkspaceDir = expandWithEnv(ws.WorkspaceDir, loadWorkspaceEnv(orgBaseDir, ws.FilesDir))
	}

	// Validate and convert workspace_dir to NULL if empty
	var workspaceDir interface{}
	if ws.WorkspaceDir != "" {
		if err := validateWorkspaceDir(ws.WorkspaceDir); err != nil {
			return fmt.Errorf("workspace %s: %w", ws.Name, err)
		}
		workspaceDir = ws.WorkspaceDir
	}

	// #65: validate workspace_access (defaults to "none" when empty).
	workspaceAccess := ws.WorkspaceAccess
	if workspaceAccess == "" {
		workspaceAccess = provisioner.WorkspaceAccessNone
	}
	if err := provisioner.ValidateWorkspaceAccess(workspaceAccess, ws.WorkspaceDir); err != nil {
		return fmt.Errorf("workspace %s: %w", ws.Name, err)
	}

	// core#2129 write-path SSRF defense (CR2 RC 13399): validate the
	// external URL BEFORE any durable side effect (workspaces INSERT,
	// canvas_layouts INSERT, structure_events INSERT). Pre-#3170RC1
	// ordering put the validation after the INSERT, which left a
	// stranded provisioning workspace row + layout + event for a
	// rejected malicious leaf. Mirrors workspace.go's pre-BeginTx
	// pattern (workspace.go:624) — reject at the boundary, not after
	// the fact. The post-INSERT UPDATE that originally carried the URL
	// never runs on rejection, so no unsafe URL ever lands in the DB.
	if ws.External && ws.URL != "" {
		if err := validateAgentURL(ws.URL); err != nil {
			log.Printf("Org import: external workspace URL rejected for %s (pre-INSERT): %v — leaf rejected", ws.Name, err)
			return fmt.Errorf("external workspace %s URL rejected: %w", ws.Name, err)
		}
	}

	ctx := context.Background()

	// Org-template imports default to expanded so children render
	// visually nested inside their parent — matches the user's mental
	// model ("all children should be in front of its parent"). The
	// topology rescue heuristic lays any children whose YAML coords
	// fall outside the computed parent bbox into a tidy 2-column grid
	// (see canvas-topology.ts), so imports don't spray the viewport.
	initialCollapsed := false

	maxConcurrent := ws.MaxConcurrentTasks
	if maxConcurrent <= 0 {
		maxConcurrent = models.DefaultMaxConcurrentTasks
	}
	// TOCTOU-safe insert (#2872 Critical 1).
	//
	// `ON CONFLICT DO NOTHING` paired with the partial unique index
	// from migration 20260506000000_workspaces_unique_parent_name.up.sql
	// atomically resolves a race window that the prior
	// lookup-then-insert had: two concurrent /org/import POSTs both
	// saw "not found" in lookupExistingChild and both INSERT'd the
	// same (parent_id, name). After this swap the SECOND INSERT
	// silently no-ops, RETURNING returns 0 rows → sql.ErrNoRows, and
	// the skip-path runs.
	//
	// ON CONFLICT target uses (COALESCE(parent_id,...), name) WHERE
	// status != 'removed' — must match the partial-index predicate
	// EXACTLY for Postgres to consider the index applicable.
	var insertedID string
	err := db.DB.QueryRowContext(ctx, `
		INSERT INTO workspaces (id, name, role, tier, runtime, status, parent_id, workspace_dir, workspace_access, max_concurrent_tasks)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (COALESCE(parent_id, '00000000-0000-0000-0000-000000000000'::uuid), name)
		WHERE status != 'removed'
		DO NOTHING
		RETURNING id
	`, id, ws.Name, role, tier, runtime, "provisioning", parentID, workspaceDir, workspaceAccess, maxConcurrent).Scan(&insertedID)
	if errors.Is(err, sql.ErrNoRows) {
		// Skip path — a non-removed row already exists for
		// (parent_id, name). Re-select its id; idempotency-friendly
		// semantics match the original lookupExistingChild path
		// (parent_id IS NOT DISTINCT FROM matches NULL too,
		// status='removed' rows are ignored).
		ctxLookup, cancelLookup := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelLookup()
		existingID, found, selErr := h.lookupExistingChild(ctxLookup, ws.Name, parentID)
		if selErr != nil {
			return fmt.Errorf("post-conflict re-select for %s: %w", ws.Name, selErr)
		}
		if !found {
			// Index conflicted but row vanished between INSERT and
			// re-SELECT (status flipped to 'removed' concurrently).
			// Surface as an error rather than silently retrying —
			// the user can re-trigger /org/import safely.
			return fmt.Errorf("workspace %q conflicted on insert but not visible on re-select (concurrent status flip?)", ws.Name)
		}
		log.Printf("Org import: %q already exists (id=%s) — skipping create+provision, recursing into children for partial-match", ws.Name, existingID)
		parentRef := ""
		if parentID != nil {
			parentRef = *parentID
		}
		provlog.Event("provision.skip_existing", map[string]any{
			"name":        ws.Name,
			"existing_id": existingID,
			"parent_id":   parentRef,
			"tier":        tier,
		})
		*results = append(*results, map[string]interface{}{
			"id":      existingID,
			"name":    ws.Name,
			"tier":    tier,
			"skipped": true,
		})
		return h.recurseChildrenForImport(ws, existingID, absX, absY, defaults, orgBaseDir, results, provisionSem)
	}
	if err != nil {
		log.Printf("Org import: failed to create %s: %v", ws.Name, err)
		return fmt.Errorf("failed to create %s: %w", ws.Name, err)
	}

	// core#2594: persist the resolved template model as the MODEL workspace_secret
	// so it survives EVERY re-provision path (restart / resume / auto-recover /
	// re-import), not only the first import provision that carries it in-memory via
	// payload.Model. This is the org-import companion to WorkspaceHandler.Create's
	// identical setModelSecret write (workspace.go): both persist the picked model
	// into the ONE place prepareProvisionContext's loadWorkspaceSecrets reads.
	//
	// Without it, org-imported workspaces were the ONLY provision path that never
	// wrote MODEL. The first provision succeeded (payload.Model was populated from
	// ws.Model → defaults.Model), but any later provision rebuilds the payload from
	// the workspaces row — which has NO model column — so payload.Model was empty,
	// applyRuntimeModelEnv set neither MOLECULE_MODEL nor MODEL, and the universal
	// MISSING_MODEL gate aborted the workspace ("no resolved model (MISSING_MODEL,
	// core#2594); refusing the runtime's opaque default"). Persisting here closes
	// that gap for the whole imported tree, including a Marketing Manager's
	// nested-child sub-team.
	//
	// `model` is guaranteed non-empty at this point (required + resolved at the top
	// of this function). setModelSecret encrypts iff crypto is enabled (raw
	// otherwise), matching the workspace_secrets write loop below. Non-fatal: a
	// failure logs and continues so a transient secret-store blip does not abort the
	// entire import — the first provision still carries payload.Model, and a later
	// Save/Restart re-persists via the SecretsHandler endpoints.
	if err := setModelSecret(ctx, id, model); err != nil {
		log.Printf("Org import: failed to persist MODEL secret for %s (%q): %v (non-fatal)", ws.Name, model, err)
	}

	// Canvas layout — absX/absY were computed by the caller using the
	// subtree-aware grid (childSlotInGrid) so a nested-parent child
	// doesn't clip into its siblings. Raw YAML canvas coords are only
	// consulted at the root: many templates predate the nested-parent
	// model and author them as a flat horizontal row (y=180, x=100..1220),
	// which overlaps chaotically once the cards render inside a parent
	// container.
	//
	// `collapsed` lives on canvas_layouts (005_canvas_layouts.sql), not
	// on workspaces; the UI-only flag is intentionally decoupled from
	// the workspace row.
	if _, err := db.DB.ExecContext(ctx, `INSERT INTO canvas_layouts (workspace_id, x, y, collapsed) VALUES ($1, $2, $3, $4)`, id, absX, absY, initialCollapsed); err != nil {
		log.Printf("Org import: canvas layout insert failed for %s: %v", ws.Name, err)
	}

	// Broadcast — include runtime so the canvas pill renders the right
	// badge immediately instead of "unknown". parent_id + x/y let the
	// canvas's org-deploy animation spawn the child from the parent's
	// current coords and tween into its reserved slot, instead of
	// landing in a default grid position first and snapping on the
	// next hydrate.
	payload := map[string]interface{}{
		"name": ws.Name, "tier": tier, "runtime": runtime,
		// Parent-relative coords — the canvas's React Flow node uses
		// these as the node's position when parent_id is set (React
		// Flow treats node.position as parent-relative when the node
		// has a parentId). For roots, relX/relY == absX/absY.
		"x": relX, "y": relY,
	}
	if parentID != nil {
		payload["parent_id"] = *parentID
	}
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceProvisioning), id, payload)

	// Seed initial memories from workspace config or defaults (issue #1050).
	// Per-workspace initial_memories override defaults; if workspace has none,
	// fall back to defaults.initial_memories.
	wsMemories := ws.InitialMemories
	if len(wsMemories) == 0 {
		wsMemories = defaults.InitialMemories
	}
	h.workspace.seedInitialMemories(ctx, id, wsMemories)

	// Handle external workspaces
	//
	// NOTE: external URL validation moved to the top of this function
	// (CR2 RC 13399) — before the workspaces INSERT — so a rejected
	// malicious URL leaves no workspace-row / canvas-layout / structure-
	// event side effects. Only the UPDATE that lands `ws.URL` lives
	// here, and it runs unconditionally on the happy path.
	if ws.External {
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = $1, url = $2 WHERE id = $3`, models.StatusOnline, ws.URL, id); err != nil {
			log.Printf("Org import: external workspace status update failed for %s: %v", ws.Name, err)
		}
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceOnline), id, map[string]interface{}{
			"name": ws.Name, "external": true,
		})
	} else if IsMockRuntime(runtime) {
		// Mock-runtime workspaces have no container, provider compute, or URL —
		// the proxyA2ARequest short-circuit synthesises every reply
		// from a canned variant pool (see mock_runtime.go). Status
		// goes straight to 'online' so the canvas renders the node
		// as reachable + the chat tab's send button is enabled. No
		// URL is set; the proxy never tries to resolve one for mock
		// runtimes. Built for the funding-demo "200-workspace mock
		// org" template — visual scale without real backend cost.
		if _, err := db.DB.ExecContext(ctx, `UPDATE workspaces SET status = $1 WHERE id = $2`, models.StatusOnline, id); err != nil {
			log.Printf("Org import: mock workspace status update failed for %s: %v", ws.Name, err)
		}
		h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceOnline), id, map[string]interface{}{
			"name": ws.Name, "mock": true, "runtime": runtime,
		})
	} else if h.workspace.HasProvisioner() {
		// Provision container — either backend (CP for SaaS, local Docker
		// for self-hosted) is fine. Pre-2026-05-05 this gate was
		// `h.provisioner != nil`, which only checked the Docker pointer
		// and silently dropped every workspace on a SaaS tenant: the prep
		// block was skipped, no Auto call ever fired, and the row sat in
		// 'provisioning' until the 600s sweeper marked it failed with the
		// misleading "container started but never called /registry/register"
		// (incident: hongming tenant org-import 2026-05-05 01:14, 7-of-7
		// claude-code workspaces stuck). Routing to the right backend
		// happens inside provisionWorkspaceAuto — this gate just decides
		// whether to do prep at all.
		payload := models.CreateWorkspacePayload{
			Name: ws.Name, Tier: tier, Runtime: runtime, Model: model,
			Template:        ws.Template,
			WorkspaceDir:    ws.WorkspaceDir,
			WorkspaceAccess: workspaceAccess,
		}
		templatePath := ""
		if ws.Template != "" {
			// `template` comes from the uploaded YAML — treat as untrusted.
			// Only accept paths that stay inside h.configsDir.
			if tp, err := resolveInsideRoot(h.configsDir, ws.Template); err == nil {
				if _, statErr := os.Stat(tp); statErr == nil {
					templatePath = tp
				}
			}
		}
		if templatePath == "" {
			// #241: sanitizeRuntime() allowlists the runtime string so a
			// crafted org.yaml cannot use it as a path-traversal oracle.
			safeRuntime := sanitizeRuntime(runtime)
			runtimeDefault := filepath.Join(h.configsDir, safeRuntime+"-default")
			if _, err := os.Stat(runtimeDefault); err == nil {
				templatePath = runtimeDefault
			}
		}

		// Always generate default config.yaml (runtime, model, tier, etc.)
		configFiles, cfgErr := h.workspace.ensureDefaultConfig(id, payload)
		if cfgErr != nil {
			log.Printf("Org import: default config generation failed for %s: %v — marking workspace failed", ws.Name, cfgErr)
			// Fail-closed: the workspace row + layout + broadcast are already
			// persisted above (status='provisioning'). If we fall through,
			// the workspace stays stuck in provisioning silently. Mark it
			// failed so the canvas surfaces the failure card and the operator
			// sees the signal immediately, then skip the provisioning block.
			h.workspace.markProvisionFailed(ctx, id, fmt.Sprintf("default config generation failed: %v", cfgErr), nil)
			goto skipProvision
		}

		// Copy files_dir contents on top (system-prompt.md, CLAUDE.md, skills/, etc.)
		// Uses templatePath for CopyTemplateToContainer — runs AFTER configFiles are written
		if ws.FilesDir != "" && orgBaseDir != "" {
			// `files_dir` also comes from untrusted YAML. Join inside orgBaseDir
			// (already validated above) and reject anything that escapes.
			if filesPath, err := resolveInsideRoot(orgBaseDir, ws.FilesDir); err == nil {
				if info, statErr := os.Stat(filesPath); statErr == nil && info.IsDir() {
					templatePath = filesPath
				}
			}
		}

		// RFC#2843 #32: declared plugins are NOT bundled into configFiles (the
		// provisioning channel) anymore. The CTO ruling is that agent-skills
		// are PLUGINS and must install DYNAMICALLY after the workspace boots
		// online, via the existing plugin install pipeline — never through
		// the secret-bootstrap or template-asset transport. So instead of copying
		// the plugin tree into configFiles here, we PERSIST the declared set
		// (workspace_declared_plugins) and let the post-online reconcile
		// (registry heartbeat → ReconcileWorkspacePlugins) install them from
		// their source-contract specs once the box is reachable.
		//
		// Per-workspace plugins UNION with defaults.plugins (issue #68); a
		// leading "!" or "-" on a per-workspace entry opts that plugin out.
		// Each entry is a source-contract string (e.g.
		// "gitea://owner/repo/subpath#ref" or a bare local name); the install
		// name is derived from the source so the reconcile can diff declared
		// vs installed without fetching.
		seenPluginNames := map[string]string{} // name → first source that claimed it
		for _, pluginSource := range mergePlugins(defaults.Plugins, ws.Plugins) {
			pluginName, nameErr := plugins.PluginNameFromSource(pluginSource)
			if nameErr != nil {
				log.Printf("Org import: skipping plugin %q for %s — cannot derive install name: %v",
					pluginSource, ws.Name, nameErr)
				continue
			}
			// Two declared sources whose last segment collapses to the same
			// install name silently overwrite each other in
			// workspace_declared_plugins (ON CONFLICT). Warn so the operator
			// can spot the shadowing — the later source wins on re-import.
			if prevSource, dup := seenPluginNames[pluginName]; dup && prevSource != pluginSource {
				log.Printf("Org import: WARNING plugin name collision for %s — %q and %q both derive name %q; the latter overwrites the former",
					ws.Name, prevSource, pluginSource, pluginName)
			}
			seenPluginNames[pluginName] = pluginSource
			if recErr := recordDeclaredPlugin(ctx, id, pluginName, pluginSource); recErr != nil {
				log.Printf("Org import: failed to record declared plugin %s (%s) for %s: %v",
					pluginName, pluginSource, ws.Name, recErr)
			}
		}

		// Render category_routing into config.yaml so the agent can read its routing
		// table at runtime without hardcoded role names in prompts (issue #51).
		// Per-workspace keys replace defaults per-key (empty list drops the key);
		// see mergeCategoryRouting for exact semantics.
		routing := mergeCategoryRouting(defaults.CategoryRouting, ws.CategoryRouting)
		if len(routing) > 0 {
			if configFiles == nil {
				configFiles = map[string][]byte{}
			}
			block, err := renderCategoryRoutingYAML(routing)
			if err != nil {
				log.Printf("Org import: failed to render category_routing for %s: %v", ws.Name, err)
			} else {
				configFiles["config.yaml"] = appendYAMLBlock(configFiles["config.yaml"], block)
			}
		}

		// Resolve initial_prompt — inline wins, then file-ref, then defaults
		// (inline → file → defaults.inline → defaults.file). File refs are
		// rooted at <orgBaseDir>/<files_dir>/ per resolvePromptRef semantics.
		initialPrompt, err := resolvePromptRef(ws.InitialPrompt, ws.InitialPromptFile, orgBaseDir, ws.FilesDir)
		if err != nil {
			log.Printf("Org import: failed to resolve initial_prompt for %s: %v", ws.Name, err)
		}
		if initialPrompt == "" {
			// Fall back to defaults. Defaults live at the org root, so they
			// resolve with empty filesDir (relative to orgBaseDir).
			var defaultErr error
			initialPrompt, defaultErr = resolvePromptRef(defaults.InitialPrompt, defaults.InitialPromptFile, orgBaseDir, "")
			if defaultErr != nil {
				log.Printf("Org import: failed to resolve defaults.initial_prompt for %s: %v", ws.Name, defaultErr)
			}
		}
		if initialPrompt != "" {
			if configFiles == nil {
				configFiles = map[string][]byte{}
			}
			// Append initial_prompt to config.yaml using YAML block scalar.
			// Trim each line to avoid trailing whitespace issues.
			trimmed := strings.TrimSpace(initialPrompt)
			lines := strings.Split(trimmed, "\n")
			for i, line := range lines {
				lines[i] = strings.TrimRight(line, " \t")
			}
			indented := strings.Join(lines, "\n  ")
			configFiles["config.yaml"] = appendYAMLBlock(configFiles["config.yaml"], fmt.Sprintf("initial_prompt: |\n  %s\n", indented))
			log.Printf("Org import: injected initial_prompt (%d chars) into config.yaml for %s", len(trimmed), ws.Name)
		}

		// Resolve idle_prompt — same precedence (ws inline → ws file → defaults).
		// Inject into config.yaml alongside idle_interval_seconds so the
		// workspace's heartbeat loop picks up the idle-reflection cadence on
		// boot (see molecule-ai-workspace-runtime/molecule_runtime/
		// heartbeat.py and config.py).
		idlePrompt, err := resolvePromptRef(ws.IdlePrompt, ws.IdlePromptFile, orgBaseDir, ws.FilesDir)
		if err != nil {
			log.Printf("Org import: failed to resolve idle_prompt for %s: %v", ws.Name, err)
		}
		if idlePrompt == "" {
			var defaultErr error
			idlePrompt, defaultErr = resolvePromptRef(defaults.IdlePrompt, defaults.IdlePromptFile, orgBaseDir, "")
			if defaultErr != nil {
				log.Printf("Org import: failed to resolve defaults.idle_prompt for %s: %v", ws.Name, defaultErr)
			}
		}
		idleInterval := ws.IdleIntervalSeconds
		if idleInterval == 0 {
			idleInterval = defaults.IdleIntervalSeconds
		}
		if idlePrompt != "" {
			if configFiles == nil {
				configFiles = map[string][]byte{}
			}
			trimmed := strings.TrimSpace(idlePrompt)
			lines := strings.Split(trimmed, "\n")
			for i, line := range lines {
				lines[i] = strings.TrimRight(line, " \t")
			}
			indented := strings.Join(lines, "\n  ")
			// idle_interval_seconds belongs with idle_prompt — empty idle_prompt
			// means the idle loop never fires regardless of interval, so we
			// only emit interval when there's a body to go with it.
			if idleInterval <= 0 {
				idleInterval = 600 // same default as molecule_runtime/config.py
			}
			block := fmt.Sprintf("idle_interval_seconds: %d\nidle_prompt: |\n  %s\n", idleInterval, indented)
			configFiles["config.yaml"] = appendYAMLBlock(configFiles["config.yaml"], block)
			log.Printf("Org import: injected idle_prompt (%d chars, interval=%ds) into config.yaml for %s", len(trimmed), idleInterval, ws.Name)
		}

		// Scheduler-as-trigger-plugin RFC §8A P3 (template-seeding seam, core
		// leg — mechanism decided on issue #4411, 2026-07-17): render the
		// workspace's RESOLVED schedules into the DELIVERED config.yaml so the
		// runtime's boot/reload seeding (molecule-ai-workspace-runtime#318,
		// seed_schedules_from_workspace_config) reconciles them onto the
		// volume-authoritative grid. The config/asset channel lands the file
		// on the volume BEFORE boot, so there is no core→runtime API race on
		// first provision. This is now the ONLY schedule delivery path — the
		// legacy core-DB seed was retired in P4b. Must run BEFORE the provision
		// dispatch below, which captures configFiles for the provision goroutine.
		if len(ws.Schedules) > 0 {
			schedBlock, schedRendered, schedSkipped := renderTemplateSchedulesYAML(ws.Schedules, orgBaseDir, ws.FilesDir, ws.Name)
			if schedBlock != "" {
				if configFiles == nil {
					configFiles = map[string][]byte{}
				}
				// Checked append (belt+braces over the per-entry round-trip
				// guard): the ASSEMBLED config.yaml is re-parsed; if the combined
				// document fails to load, the block is dropped — an unparseable
				// config.yaml would brick workspace boot (runtime config.py
				// loads it with no try/except).
				combined, appended := appendYAMLBlockChecked(configFiles["config.yaml"], schedBlock, "schedules", ws.Name)
				configFiles["config.yaml"] = combined
				if appended {
					log.Printf("Org import: injected %d schedule(s) into config.yaml for %s (skipped=%d of %d)", schedRendered, ws.Name, schedSkipped, len(ws.Schedules))
				} else {
					schedRendered = 0
				}
			}
			// C2 ordering (RFC P5 per-workspace delivery): a workspace that
			// ships schedules needs the molecule-scheduler trigger plugin to
			// fire them (post-#4399 the plugin is the ONLY fire path), and the
			// declaration must land in workspace_declared_plugins BEFORE the
			// provision goroutine below assembles MOLECULE_DECLARED_PLUGINS
			// (buildProvisionerConfig → desiredPluginSources), so first boot
			// installs the daemon instead of waiting for the next online
			// reconcile. Pre-fix the org-import path never declared it at all
			// (only schedules.go Create and template_schedules.go did).
			// Non-fatal: a declare hiccup must not fail the import.
			if schedRendered > 0 {
				if declErr := ensureSchedulerPluginDeclared(ctx, id); declErr != nil {
					log.Printf("Org import: declare scheduler plugin for %s (non-fatal): %v", ws.Name, declErr)
				}
			}
		}

		// Inline system_prompt (only if no files_dir provides one)
		if ws.SystemPrompt != "" {
			if configFiles == nil {
				configFiles = map[string][]byte{}
			}
			configFiles["system-prompt.md"] = []byte(ws.SystemPrompt)
		}

		// Inject secrets from persona env + .env files as workspace secrets.
		// Resolution (later overrides earlier):
		//   0. Persona env (per-role bootstrap creds; only when ws.Role is set
		//      and the configured persona directory has a matching file)
		//   1. Org root .env (shared defaults)
		//   2. Workspace-specific .env (per-workspace overrides)
		// Each line: KEY=VALUE → stored as encrypted workspace secret.
		envVars := map[string]string{}
		// 0. Persona env (lowest precedence; injects the role's Gitea identity:
		//    GITEA_USER, GITEA_TOKEN, GITEA_TOKEN_SCOPES, GITEA_USER_EMAIL,
		//    GITEA_SSH_KEY_PATH, plus MODEL_PROVIDER/MODEL and the LLM auth
		//    token like CLAUDE_CODE_OAUTH_TOKEN or MINIMAX_API_KEY).
		//    Workspace and org .env can override.
		//
		// Use ws.FilesDir as the persona-dir lookup key, NOT ws.Role. In the
		// dev-tree org.yaml shape, `role:` carries the multi-line descriptive
		// text the agent reads from its prompt ("Engineering planning and
		// team coordination — leads Core Platform, Controlplane, ..."), while
		// `files_dir:` holds the short slug (`core-lead`, `dev-lead`, etc.)
		// matching `~/.molecule-ai/personas/<files_dir>/env`
		// (bind-mounted to `/etc/molecule-bootstrap/personas/<files_dir>/env`).
		//
		// Pre-fix, this passed `ws.Role` whose multi-word content failed
		// isSafeRoleName silently, so every imported workspace booted with
		// zero persona-env rows in workspace_secrets — no ANTHROPIC /
		// CLAUDE_CODE auth in the container env. The claude_agent_sdk
		// then wedged on `query.initialize()` with a 60s control-request
		// timeout (caught 2026-05-08 right after dev-only org/import).
		loadPersonaEnvFile(ws.FilesDir, envVars)
		if orgBaseDir != "" {
			// 1. Org root .env (shared defaults)
			parseEnvFile(filepath.Join(orgBaseDir, ".env"), envVars)
			// 2. Workspace-specific .env (overrides)
			// SECURITY: ws.FilesDir is untrusted YAML input — guard against CWE-22
			// traversal so a crafted filesDir like "../../../etc" cannot escape orgBaseDir.
			if ws.FilesDir != "" {
				if safeFilesDir, err := resolveInsideRoot(orgBaseDir, ws.FilesDir); err == nil {
					parseEnvFile(filepath.Join(safeFilesDir, ".env"), envVars)
				}
				// Traversal rejection: silently skip — callers expect partial env on failure.
			}
		}
		// Store as workspace secrets via DB (encrypted if key is set, raw otherwise)
		for key, value := range envVars {
			var encrypted []byte
			if crypto.IsEnabled() {
				var err error
				encrypted, err = crypto.Encrypt([]byte(value))
				if err != nil {
					log.Printf("Org import: failed to encrypt secret %s for %s: %v", key, ws.Name, err)
					continue
				}
			} else {
				encrypted = []byte(value) // store raw when encryption disabled
			}
			if _, err := db.DB.ExecContext(ctx, `
				INSERT INTO workspace_secrets (workspace_id, key, encrypted_value)
				VALUES ($1, $2, $3)
				ON CONFLICT (workspace_id, key) DO UPDATE SET encrypted_value = $3, updated_at = now()
			`, id, key, encrypted); err != nil {
				log.Printf("Org import: failed to insert secret %s for %s: %v", key, ws.Name, err)
			}
		}

		// #1084: limit concurrent provisioning via semaphore.
		// Use provisionWorkspaceAuto so SaaS deployments route through
		// the control-plane provider path — calling provisionWorkspace directly was
		// the same silent-drop bug that bit TeamHandler.Expand on
		// 2026-05-04 (see workspace.go:121-125 comment + #2486). Symptom:
		// every claude-code workspace from org-import on SaaS sat in
		// "provisioning" until the 600s sweeper marked it failed with
		// "container started but never called /registry/register" —
		// because there was no container, just a workspace row.
		// provisionWorkspaceAuto picks CP-mode when h.cpProv is wired,
		// Docker-mode otherwise; the org-import call site doesn't need
		// to know which.
		provisionSem <- struct{}{} // acquire
		// RFC internal#524 Layer 1: route through workspace.goAsync —
		// provisionWorkspaceAuto inserts/updates the workspaces row in
		// db.DB and must be drained before any test cleanup swap.
		wID, tPath, cFiles, p := id, templatePath, configFiles, payload
		h.workspace.goAsync(func() {
			defer func() { <-provisionSem }() // release
			h.workspace.provisionWorkspaceAuto(wID, tPath, cFiles, p)
		})
	}

skipProvision:
	// Schedules declared in the template were rendered into the delivered
	// config.yaml above (renderTemplateSchedulesYAML); the runtime seeds them
	// onto the volume-authoritative grid at boot/reload. Core no longer writes
	// a schedule DB table (retired in P4b).

	// Insert channels if defined (Telegram, Slack, etc.). Config values
	// support ${VAR} expansion from .env files. The manager is reloaded
	// once at the end of org import (in Import), not per-workspace.
	channelEnv := loadWorkspaceEnv(orgBaseDir, ws.FilesDir)
	wsChannelsCreated := []string{}
	wsChannelsSkipped := []map[string]string{}
	// skipChannel records a skipped channel with consistent shape across all reasons.
	skipChannel := func(channelType, reason string) {
		wsChannelsSkipped = append(wsChannelsSkipped, map[string]string{
			"workspace": ws.Name,
			"type":      channelType, // empty string when type field was missing
			"reason":    reason,
		})
	}

	for _, ch := range ws.Channels {
		if ch.Type == "" {
			skipChannel("", "empty type")
			log.Printf("Org import: skipping channel with empty type for %s", ws.Name)
			continue
		}
		// Validate adapter exists upfront — fail fast instead of inserting orphan rows
		adapter, ok := channels.GetAdapter(ch.Type)
		if !ok {
			skipChannel(ch.Type, "unknown adapter")
			log.Printf("Org import: skipping %s channel for %s — no adapter registered", ch.Type, ws.Name)
			continue
		}

		expandedConfig := make(map[string]interface{}, len(ch.Config))
		missing := []string{}
		for k, v := range ch.Config {
			expanded := expandWithEnv(v, channelEnv)
			if hasUnresolvedVarRef(v, expanded) {
				missing = append(missing, v)
			}
			expandedConfig[k] = expanded
		}
		if len(missing) > 0 {
			skipChannel(ch.Type, fmt.Sprintf("missing env: %v", missing))
			log.Printf("Org import: skipping %s channel for %s — env vars not set: %v", ch.Type, ws.Name, missing)
			continue
		}

		// Adapter-level config validation
		if err := adapter.ValidateConfig(expandedConfig); err != nil {
			skipChannel(ch.Type, err.Error())
			log.Printf("Org import: skipping %s channel for %s — invalid config: %v", ch.Type, ws.Name, err)
			continue
		}

		// Match ChannelHandler.Create/Update: production channel credentials
		// must be encrypted before channel_config reaches Postgres. Validate
		// first because adapters need the plaintext shape, then encrypt the
		// known secret fields in place while leaving routing identifiers usable.
		if err := channels.EncryptSensitiveFields(expandedConfig); err != nil {
			skipChannel(ch.Type, "channel config encryption failed")
			// Do not log expandedConfig: it still contains the imported secrets
			// when encryption fails.
			log.Printf("Org import: skipping %s channel for %s — config encryption failed: %v", ch.Type, ws.Name, err)
			continue
		}

		configJSON, err := json.Marshal(expandedConfig)
		if err != nil {
			log.Printf("Org import: failed to marshal config for %s channel: %v", ch.Type, err)
			continue
		}
		allowedJSON, err := json.Marshal(ch.AllowedUsers)
		if err != nil {
			log.Printf("Org import: failed to marshal allowed_users for %s channel: %v", ch.Type, err)
			continue
		}
		enabled := true
		if ch.Enabled != nil {
			enabled = *ch.Enabled
		}
		// Idempotent insert — if same workspace+type already exists, update config
		if _, err := db.DB.ExecContext(context.Background(), `
			INSERT INTO workspace_channels (workspace_id, channel_type, channel_config, enabled, allowed_users)
			VALUES ($1, $2, $3::jsonb, $4, $5::jsonb)
			ON CONFLICT (workspace_id, channel_type) DO UPDATE
			SET channel_config = EXCLUDED.channel_config,
			    enabled = EXCLUDED.enabled,
			    allowed_users = EXCLUDED.allowed_users,
			    updated_at = now()
		`, id, ch.Type, string(configJSON), enabled, string(allowedJSON)); err != nil {
			log.Printf("Org import: failed to create %s channel for %s: %v", ch.Type, ws.Name, err)
		} else {
			wsChannelsCreated = append(wsChannelsCreated, ch.Type)
			log.Printf("Org import: %s channel created for %s", ch.Type, ws.Name)
		}
	}

	resultEntry := map[string]interface{}{
		"id":   id,
		"name": ws.Name,
		"tier": tier,
	}
	if len(wsChannelsCreated) > 0 {
		resultEntry["channels"] = wsChannelsCreated
	}
	if len(wsChannelsSkipped) > 0 {
		resultEntry["channels_skipped"] = wsChannelsSkipped
	}
	*results = append(*results, resultEntry)

	// Recurse into children — both create-path and skip-path use the
	// same helper so partial-match (parent exists, some children missing)
	// backfills correctly without duplicating the recursion logic.
	return h.recurseChildrenForImport(ws, id, absX, absY, defaults, orgBaseDir, results, provisionSem)
}

// topLevelImportSlot describes where one imported top-level workspace is
// anchored: its parent (a pointer to the platform-agent id, or nil for the
// no-concierge fallback) and its absolute + parent-relative canvas
// coordinates. createWorkspaceTree consumes these exactly as it does for a
// recursed child: parentID → workspaces.parent_id, absX/absY → canvas_layouts,
// relX/relY → the broadcast payload's parent-relative x/y.
type topLevelImportSlot struct {
	parentID   *string
	absX, absY float64
	relX, relY float64
}

// planTopLevelImport decides how an org template's top-level workspaces are
// anchored during /org/import.
//
// The org's platform agent (the concierge) IS the org root — the single
// kind='platform' AND parent_id IS NULL row (see platform_agent.go +
// org_scope.go, which walks the parent_id chain to that one NULL-parent root
// to scope an org). When it is present, the imported top-level workspaces are
// nested UNDER it as children: parent_id = the platform-agent id, positioned in
// the SAME subtree-aware grid regular children use (childSlotInGrid over
// sizeOfSubtree), relative to the platform agent's own canvas position. A
// single shared *string is used for parentID across all top-level workspaces —
// they all have the same parent — mirroring recurseChildrenForImport's
// `&parentID` pattern.
//
// core#3510: the top-level loop previously always passed parent_id = nil, so an
// imported org landed as SIBLINGS of the platform agent (each imported root
// counted as its own org root under the parent_id-chain org scoping) instead of
// a child subtree of the concierge.
//
// When the org has NO platform agent (an edge-case org provisioned without a
// concierge) — or the lookup errors — it falls back to the historical behavior:
// each top-level workspace stays at ROOT (parent_id NULL) at its own template
// canvas coordinates. Imports on such orgs are never broken.
func (h *OrgHandler) planTopLevelImport(ctx context.Context, roots []OrgWorkspace) []topLevelImportSlot {
	slots := make([]topLevelImportSlot, len(roots))

	platformID, platX, platY, hasPlatform := lookupPlatformAgentAnchor(ctx)
	if !hasPlatform {
		// Fallback: no concierge to nest under. Roots keep their YAML canvas
		// coords and land at parent_id NULL (relX/relY == absX/absY, as a root
		// has no parent to be relative to).
		for i, ws := range roots {
			slots[i] = topLevelImportSlot{
				parentID: nil,
				absX:     ws.Canvas.X, absY: ws.Canvas.Y,
				relX: ws.Canvas.X, relY: ws.Canvas.Y,
			}
		}
		return slots
	}

	// Nest under the platform agent. Position the imported roots in the same
	// subtree-aware grid regular children use so a nested-parent root doesn't
	// clip into its siblings.
	siblingSizes := make([]nodeSize, len(roots))
	for i, ws := range roots {
		siblingSizes[i] = sizeOfSubtree(ws)
	}
	// One addressable copy shared by every top-level workspace (they share the
	// single platform-agent parent) — same shape as recurseChildrenForImport.
	pid := platformID
	for i := range roots {
		slotX, slotY := childSlotInGrid(i, siblingSizes)
		slots[i] = topLevelImportSlot{
			parentID: &pid,
			// abs = platform-agent origin + grid slot; rel = the slot itself
			// (parent-relative), exactly like childAbsX := absX + slotX with
			// slotX/slotY as the relative coords in recurseChildrenForImport.
			absX: platX + slotX, absY: platY + slotY,
			relX: slotX, relY: slotY,
		}
	}
	return slots
}

// lookupPlatformAgentAnchor returns the CALLER/import org's platform-agent root
// id and its canvas position (x, y), or found=false when the org has no platform
// agent to nest under.
//
// SCOPING (core#3510 review): the anchor is resolved deterministically by the
// canonical PlatformAgentID() id — DeterministicPlatformAgentID(MOLECULE_ORG_ID)
// on a CP-provisioned/SaaS tenant, else the fixed SelfHostedPlatformAgentID on
// self-host/local. That is the SAME id every real install path seeds the
// concierge under (ensurePlatformAgentFlow's derivedID := PlatformAgentID(), the
// CP install, EnsureSelfHostedPlatformAgent), so it targets THIS org's concierge
// precisely. A bare `WHERE kind='platform' AND parent_id IS NULL LIMIT 1` is
// multi-root-unsafe: workspaces has no org_id on that predicate, so in a DB with
// more than one platform/tenant root it could attach the imported org under an
// ARBITRARY platform root rather than the caller's. `status != 'removed'` mirrors
// the org-scope / defaultCreateParentID SSOT so a tombstoned concierge is never
// used as an anchor.
//
// FALLBACK (historical): when the deterministic id doesn't resolve to a live
// platform row — a legacy concierge seeded before the derived-id contract, or a
// hand-migrated DB — we fall back to the structural single platform root
// (kind='platform', parent_id NULL, not removed). A per-org tenant DB holds at
// most one such row (uniq_workspaces_one_platform_root), so LIMIT 1 is
// unambiguous there. If neither resolves (no concierge / any DB error) found is
// false and the caller places the import at root — a transient DB hiccup or a
// concierge-less org degrades to the historical behavior instead of breaking the
// import.
//
// Canvas coordinates live on canvas_layouts (LEFT JOIN + COALESCE(..., 0) so a
// platform agent without a layout row anchors the imported subtree at origin
// rather than failing).
func lookupPlatformAgentAnchor(ctx context.Context) (id string, x, y float64, found bool) {
	// Primary: org-scoped by the deterministic PlatformAgentID() anchor.
	if anchorID := PlatformAgentID(); anchorID != "" {
		if gotID, gx, gy, ok := scanPlatformAnchor(ctx, `
			SELECT w.id, COALESCE(cl.x, 0), COALESCE(cl.y, 0)
			FROM workspaces w
			LEFT JOIN canvas_layouts cl ON cl.workspace_id = w.id
			WHERE w.id = $1 AND w.kind = 'platform' AND w.status != 'removed'
			LIMIT 1
		`, anchorID); ok {
			return gotID, gx, gy, true
		}
	}
	// Fallback: the org's structural single platform root (historical behavior).
	return scanPlatformAnchor(ctx, `
		SELECT w.id, COALESCE(cl.x, 0), COALESCE(cl.y, 0)
		FROM workspaces w
		LEFT JOIN canvas_layouts cl ON cl.workspace_id = w.id
		WHERE w.kind = 'platform' AND w.parent_id IS NULL AND w.status != 'removed'
		LIMIT 1
	`)
}

// scanPlatformAnchor runs a platform-agent anchor SELECT (id + COALESCE'd canvas
// x,y) and returns found=false on sql.ErrNoRows AND on any query error, so a
// transient DB hiccup degrades to the caller's root-placement fallback instead
// of breaking the import. args carries the query's positional parameters (the
// primary lookup passes the PlatformAgentID() anchor; the structural fallback
// passes none).
func scanPlatformAnchor(ctx context.Context, query string, args ...interface{}) (id string, x, y float64, found bool) {
	err := db.DB.QueryRowContext(ctx, query, args...).Scan(&id, &x, &y)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("Org import: platform-agent anchor lookup failed: %v — falling back to root placement", err)
		}
		return "", 0, 0, false
	}
	return id, x, y, true
}

// lookupExistingChild returns the id of an existing workspace under
// (parent_id, name) if any, with idempotency-friendly semantics:
//   - parent_id IS NOT DISTINCT FROM matches NULL too (root workspaces)
//   - status='removed' rows are ignored — workspaces removed by deletion or
//     import reconciliation shouldn't block a re-import
//
// On sql.ErrNoRows: returns ("", false, nil) — caller should INSERT.
// On a real DB error: returns ("", false, err) — caller propagates.
//
// errors.Is is wrap-safe — a future caller wrapping the error
// (database/sql can wrap driver errors with %w in some setups) would
// silently break a `err == sql.ErrNoRows` equality check, causing the
// no-rows path to fall through to the "real DB error" branch and
// abort the import. errors.Is unwraps.
func (h *OrgHandler) lookupExistingChild(ctx context.Context, name string, parentID *string) (string, bool, error) {
	var existingID string
	err := db.DB.QueryRowContext(ctx, `
		SELECT id FROM workspaces
		WHERE name = $1
		  AND parent_id IS NOT DISTINCT FROM $2
		  AND status != 'removed'
		LIMIT 1
	`, name, parentID).Scan(&existingID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return existingID, true, nil
}

// recurseChildrenForImport walks ws.Children once, computing each child's
// absolute + parent-relative canvas coordinates from the subtree-aware
// grid (so nested-parent children don't clip into leaf siblings) and
// dispatching createWorkspaceTree for each. Pacing prevents Docker
// container-spam thundering on the self-hosted backend; SaaS dispatches
// managed provisioning in a goroutine so the main loop is not blocked.
func (h *OrgHandler) recurseChildrenForImport(ws OrgWorkspace, parentID string, absX, absY float64, defaults OrgDefaults, orgBaseDir string, results *[]map[string]interface{}, provisionSem chan struct{}) error {
	if len(ws.Children) == 0 {
		return nil
	}
	siblingSizes := make([]nodeSize, len(ws.Children))
	for i, c := range ws.Children {
		siblingSizes[i] = sizeOfSubtree(c)
	}
	for i, child := range ws.Children {
		slotX, slotY := childSlotInGrid(i, siblingSizes)
		childAbsX := absX + slotX
		childAbsY := absY + slotY
		// slotX/slotY are already parent-relative — that's
		// exactly what childSlotInGrid returns.
		if err := h.createWorkspaceTree(child, &parentID, childAbsX, childAbsY, slotX, slotY, defaults, orgBaseDir, results, provisionSem); err != nil {
			return err
		}
		// Pacing exists to throttle Docker container-spawn thundering
		// during a self-hosted import. Mock-runtime children spawn no
		// container — no Docker pressure, no LLM bursts, just DB
		// inserts + a broadcast. Skipping the 2s sleep collapses a
		// 200-workspace mock-org import from ~7min → ~5s, which is
		// the difference between a snappy demo and a "did it freeze?"
		// staring contest. Real (containerful) runtimes still pace.
		// Inheritance: if the child itself doesn't declare a runtime,
		// fall back to defaults.runtime — the org template sets
		// runtime: mock once at the org level, not on every IC node.
		childRuntime := child.Runtime
		if childRuntime == "" {
			childRuntime = defaults.Runtime
		}
		if !IsMockRuntime(childRuntime) {
			time.Sleep(workspaceCreatePacingMs * time.Millisecond)
		}
	}
	return nil
}

// envVarNamePattern guards template-supplied env var names against
// pathological inputs. A malicious template could ship
// required_env: ["'; DROP …"] or whitespace-only entries that would
// flow through collectOrgEnv → into the 412 response body and,
// worse, into the modal's PUT /settings/secrets input. Schema
// already has `key TEXT NOT NULL UNIQUE` and our queries are
// parameterised so SQL injection isn't the threat — the real risks
// are UI rendering weirdness (newlines, NUL bytes, zero-width chars)
// and downstream env-var semantics (POSIX requires uppercase +
// underscore + digit). A strict regex filters both classes of
// problem at a single choke point.
var envVarNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)

// sanitizeEnvMembers filters a requirement's member list through the
// name-validation regex, logging rejections. Returns the filtered
// list and a boolean indicating whether any valid members remain.
// Used so a group containing one valid + one bogus name is kept
// (valid member carries the group) rather than silently dropped.
func sanitizeEnvMembers(members []string, where string) ([]string, bool) {
	out := make([]string, 0, len(members))
	for _, k := range members {
		if !envVarNamePattern.MatchString(k) {
			if k != "" {
				log.Printf("collectOrgEnv: rejecting invalid env var name %q from %s (must match %s)", k, where, envVarNamePattern)
			}
			continue
		}
		out = append(out, k)
	}
	return out, len(out) > 0
}

// envRequirementKey canonicalises a requirement for dedup — sorted
// member list joined with NUL so `any_of: [A, B]` and `any_of: [B, A]`
// collapse to the same key. Single requirements are length-1 groups.
func envRequirementKey(members []string) string {
	cp := append([]string(nil), members...)
	sort.Strings(cp)
	return strings.Join(cp, "\x00")
}

// collectOrgEnv walks the whole template tree and returns the union of
// required_env and recommended_env declared anywhere — at the org
// level, on root workspaces, or on any nested child. Deduplicates by
// group membership (same set of members = same requirement) and
// sorts deterministically so the canvas sees a stable order.
//
// "Required wins" rules:
//
//   - A requirement that appears in BOTH required and recommended
//     (same members) surfaces only as required.
//   - A single-name requirement (e.g. "API_KEY") and a group that
//     contains that same name (e.g. {any_of: [API_KEY, OTHER]}) are
//     NOT deduplicated — they're semantically different (strict vs
//     satisfiable-by-alternative) and the stricter "single" one wins,
//     so the any-of group is dropped when its members overlap with a
//     strict requirement declared elsewhere.
//
// Invalid names fail envVarNamePattern; the filter is applied per
// group so a group with one bogus entry keeps the rest. A group
// whose ALL members are invalid is dropped entirely with a log.
func collectOrgEnv(tmpl *OrgTemplate) (required, recommended []EnvRequirement) {
	reqByKey := map[string]EnvRequirement{}
	recByKey := map[string]EnvRequirement{}
	// Names covered by strict (single) required entries. A group in
	// EITHER tier whose any-of contains ONE of these names is
	// dominated by the strict requirement and gets dropped on the
	// second pass.
	strictRequiredNames := map[string]struct{}{}

	accept := func(into map[string]EnvRequirement, src []EnvRequirement, where string, markStrict bool) {
		for _, req := range src {
			members, ok := sanitizeEnvMembers(req.Members(), where)
			if !ok {
				continue
			}
			key := envRequirementKey(members)
			if _, exists := into[key]; exists {
				continue
			}
			if req.Name != "" && len(members) == 1 {
				into[key] = EnvRequirement{Name: members[0]}
				if markStrict {
					strictRequiredNames[members[0]] = struct{}{}
				}
			} else {
				into[key] = EnvRequirement{AnyOf: members}
			}
		}
	}
	accept(reqByKey, tmpl.RequiredEnv, "template root", true)
	accept(recByKey, tmpl.RecommendedEnv, "template root", false)
	var walk func([]OrgWorkspace)
	walk = func(ws []OrgWorkspace) {
		for _, w := range ws {
			accept(reqByKey, w.RequiredEnv, "workspace "+w.Name, true)
			accept(recByKey, w.RecommendedEnv, "workspace "+w.Name, false)
			walk(w.Children)
		}
	}
	walk(tmpl.Workspaces)

	// Required wins across tiers: any requirement whose members
	// overlap with a strict required name gets dropped from
	// recommended. Keeps the canvas modal from showing the same
	// key in both sections.
	prune := func(from map[string]EnvRequirement) {
		for k, r := range from {
			for _, m := range r.Members() {
				if _, strict := strictRequiredNames[m]; strict {
					delete(from, k)
					break
				}
			}
		}
	}
	prune(recByKey)

	// Same-tier: a strict required X dominates any-of groups in
	// required that CONTAIN X (a group saying "any of X, Y" is
	// automatically satisfied when X is required anyway, so it's
	// redundant). Same logic applied to recommended.
	pruneSameTier := func(tier map[string]EnvRequirement) {
		strictInTier := map[string]struct{}{}
		for _, r := range tier {
			if r.Name != "" {
				strictInTier[r.Name] = struct{}{}
			}
		}
		for k, r := range tier {
			if len(r.AnyOf) == 0 {
				continue
			}
			for _, m := range r.AnyOf {
				if _, strict := strictInTier[m]; strict {
					delete(tier, k)
					break
				}
			}
		}
	}
	pruneSameTier(reqByKey)
	pruneSameTier(recByKey)

	required = flattenAndSortRequirements(reqByKey)
	recommended = flattenAndSortRequirements(recByKey)
	return required, recommended
}

func flattenAndSortRequirements(by map[string]EnvRequirement) []EnvRequirement {
	out := make([]EnvRequirement, 0, len(by))
	for _, r := range by {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		// Sort singles first by name; groups after, ordered by
		// joined-member string. Gives the canvas a deterministic
		// render order so the same template always produces the
		// same modal layout.
		iSingle := out[i].Name != ""
		jSingle := out[j].Name != ""
		if iSingle != jSingle {
			return iSingle
		}
		if iSingle {
			return out[i].Name < out[j].Name
		}
		return envRequirementKey(out[i].AnyOf) < envRequirementKey(out[j].AnyOf)
	})
	return out
}

// loadConfiguredGlobalSecretKeys returns the set of key names present
// in global_secrets WHERE the encrypted_value is non-empty. Filtering
// on the payload size catches the failure mode where a row was
// upserted with an empty value (historical rows predating the
// binding:"required" guard on SetGlobal, or a future direct SQL
// path that skips it) — the preflight would otherwise report the
// key as "configured" and the per-container preflight would still
// fail at start time, defeating the whole feature.
// The LIMIT is a sanity cap: at realistic tenant sizes (< 1k
// secrets) it's a no-op; at pathological sizes it stops one slow
// query from wedging org imports. A hit gets logged so operators
// can investigate.
const globalSecretsPreflightLimit = 10000

// PerWorkspaceUnsatisfied describes one per-workspace RequiredEnv that is
// not covered by either a global secret or a key present in the
// corresponding .env file.
type PerWorkspaceUnsatisfied struct {
	Workspace   string         `json:"workspace"`
	FilesDir    string         `json:"files_dir,omitempty"`
	Unsatisfied EnvRequirement `json:"unsatisfied_env"`
}

// collectPerWorkspaceUnsatisfied recursively walks workspaces and returns
// per-workspace RequiredEnv entries that are not covered by (a) a global
func loadConfiguredGlobalSecretKeys(ctx context.Context) (map[string]struct{}, error) {
	rows, err := db.DB.QueryContext(ctx,
		`SELECT key FROM global_secrets WHERE octet_length(encrypted_value) > 0 LIMIT $1`,
		globalSecretsPreflightLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var k string
		if scanErr := rows.Scan(&k); scanErr == nil && k != "" {
			out[k] = struct{}{}
		}
	}
	if len(out) == globalSecretsPreflightLimit {
		log.Printf("loadConfiguredGlobalSecretKeys: hit LIMIT %d — org-import preflight may be incomplete", globalSecretsPreflightLimit)
	}
	return out, rows.Err()
}

func countWorkspaces(workspaces []OrgWorkspace) int {
	count := len(workspaces)
	for _, ws := range workspaces {
		count += countWorkspaces(ws.Children)
	}
	return count
}

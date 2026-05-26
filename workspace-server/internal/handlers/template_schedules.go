package handlers

// template_schedules.go — read a workspace template's `schedules:`
// block and seed workspace_schedules with source='template'. Mirrors
// the org/import flow (org_import.go) so a workspace created directly
// from a workspace template (e.g. via WorkspaceHandler.Create) lands
// with the same schedule grid the org/import path would have produced.
//
// Issue #24 contract (also enforced by org_import + schedules.go):
//   - INSERT new rows with source='template'
//   - On (workspace_id, name) collision, only refresh template-source
//     rows; runtime-added rows survive re-provisioning untouched
//   - Never DELETE (additive only)
//
// The actual INSERT statement is the canonical orgImportScheduleSQL
// defined in org.go — reused here verbatim so the four guarantees
// stay in one place.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/scheduler"
)

// templateConfigSchedules is the minimal shape parsed from a workspace
// template's config.yaml. Only the `schedules:` block is modelled;
// the rest of the file (providers, runtime_config, …) is opaque to
// this loader and continues to flow through the existing pass-through
// in workspace_provision.go.
type templateConfigSchedules struct {
	Schedules []OrgSchedule `yaml:"schedules"`
}

// parseTemplateSchedules reads `<templatePath>/config.yaml` and
// returns its `schedules:` block (nil + nil error when the file is
// absent or the block is empty).
//
// Returns an error only when a present config.yaml fails to read or
// parse — callers should treat that as a template-author bug rather
// than a runtime fault. The Create handler logs the error and
// continues so a broken schedules block can never block workspace
// provisioning.
func parseTemplateSchedules(templatePath string) ([]OrgSchedule, error) {
	if templatePath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(templatePath, "config.yaml"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read template config.yaml: %w", err)
	}
	var cfg templateConfigSchedules
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse template config.yaml schedules: %w", err)
	}
	return cfg.Schedules, nil
}

// seedTemplateSchedules INSERTs (or refreshes) each schedule into
// workspace_schedules with source='template'. Returns the count of
// rows successfully upserted.
//
// Prompt body resolution mirrors org_import.go: inline `prompt:` wins,
// else `prompt_file:` is resolved relative to templatePath via
// resolvePromptRef. Per-schedule failures (bad cron, missing prompt
// file, DB error) are logged and skipped so one bad row never breaks
// the rest of the grid.
//
// Timezone defaults to "UTC" when unset. Env-var expansion in the
// timezone field is intentionally not performed — that mirrors the
// org/import behavior; template authors should pick a literal IANA
// zone (or rely on UTC + operator overrides per-tenant).
func seedTemplateSchedules(ctx context.Context, workspaceID, templatePath string, schedules []OrgSchedule) int {
	seeded := 0
	for _, sched := range schedules {
		tz := sched.Timezone
		if tz == "" {
			tz = "UTC"
		}
		enabled := true
		if sched.Enabled != nil {
			enabled = *sched.Enabled
		}
		prompt, promptErr := resolvePromptRef(sched.Prompt, sched.PromptFile, templatePath, "")
		if promptErr != nil {
			log.Printf("Template schedule seed: failed to resolve prompt for '%s' on %s: %v — skipping", sched.Name, workspaceID, promptErr)
			continue
		}
		if prompt == "" {
			log.Printf("Template schedule seed: schedule '%s' on %s has empty prompt — skipping", sched.Name, workspaceID)
			continue
		}
		nextRun, nextRunErr := scheduler.ComputeNextRun(sched.CronExpr, tz, time.Now())
		if nextRunErr != nil {
			log.Printf("Template schedule seed: invalid cron for '%s' on %s: %v — skipping", sched.Name, workspaceID, nextRunErr)
			continue
		}
		if _, err := db.DB.ExecContext(ctx, orgImportScheduleSQL,
			workspaceID, sched.Name, sched.CronExpr, tz, prompt, enabled, nextRun); err != nil {
			log.Printf("Template schedule seed: failed to upsert '%s' on %s: %v", sched.Name, workspaceID, err)
			continue
		}
		seeded++
		log.Printf("Template schedule seed: '%s' (%s, %d chars) upserted on %s (source=template)", sched.Name, sched.CronExpr, len(prompt), workspaceID)
	}
	return seeded
}

package handlers

// template_schedules.go — parse a workspace template's `schedules:`
// block. The parsed entries are rendered into the delivered config.yaml
// (renderTemplateSchedulesYAML) so the runtime seeds them onto the
// volume-authoritative grid at boot/reload; core no longer seeds a
// schedule DB table (retired in P4b).
//
// Hostile-template defense (a tenant can upload a config.yaml via
// POST /templates/import or webhook-sync a repo they control):
// config.yaml is loaded through a 1 MiB LimitReader so a YAML
// anchor-bomb / billion-laughs cannot pre-explode memory before
// unmarshal returns, and len(schedules) is bounded.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Bounds protecting the seeder against hostile or buggy templates.
// All chosen with generous headroom relative to legitimate use
// (reno-stars org template — the largest production schedule grid —
// runs ~10 entries per workspace, each prompt body well under 1 KiB).
const (
	maxTemplateConfigYAMLBytes int64 = 1 << 20  // 1 MiB — hard cap on config.yaml size
	maxTemplateSchedules             = 100      // 10x current largest grid
	maxScheduleCronExprLen           = 128      // cron-spec syntax is short by construction
	maxSchedulePromptBytes           = 16 << 10 // 16 KiB after prompt_file resolution
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
// The file is read through a 1 MiB LimitReader so a billion-laughs
// or anchor-explosion YAML cannot pre-explode memory before
// Unmarshal returns. Returns an error only when a present
// config.yaml fails to read or parse — callers should treat that as
// a template-author bug rather than a runtime fault. The Create
// handler logs the error and continues so a broken schedules block
// can never block workspace provisioning.
func parseTemplateSchedules(templatePath string) ([]OrgSchedule, error) {
	if templatePath == "" {
		return nil, nil
	}
	f, err := os.Open(filepath.Join(templatePath, "config.yaml"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open template config.yaml: %w", err)
	}
	defer f.Close()

	// Read maxTemplateConfigYAMLBytes+1 — if we filled the buffer the
	// underlying file exceeded the cap and we refuse to unmarshal.
	data, err := io.ReadAll(io.LimitReader(f, maxTemplateConfigYAMLBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read template config.yaml: %w", err)
	}
	if int64(len(data)) > maxTemplateConfigYAMLBytes {
		return nil, fmt.Errorf("template config.yaml exceeds %d-byte cap", maxTemplateConfigYAMLBytes)
	}
	var cfg templateConfigSchedules
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse template config.yaml schedules: %w", err)
	}
	if len(cfg.Schedules) > maxTemplateSchedules {
		return nil, fmt.Errorf("template declares %d schedules; cap is %d", len(cfg.Schedules), maxTemplateSchedules)
	}
	return cfg.Schedules, nil
}

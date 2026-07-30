package handlers

// plugin_install_report.go — core's half of the SDK plugin-install-report contract.
//
// WHAT WAS MISSING, AND WHY IT MATTERED
//
// The runtime already computed this on every boot
// (molecule_runtime.plugin_sources.InstallReport) and then printed it to stdout.
// On a locked-down prod box stdout is not readable — boot_step_emit.py says so in
// as many words — and the BOOT_STEP telemetry that would have carried it is
// CONCIERGE-GATED, "so an ordinary, non-concierge tenant boot doesn't spam the
// endpoint". POST /boot-event is BroadcastOnly on top of that: "presentation
// event, no structure_events row".
//
// Net effect, measured on both prod tenants 2026-07-30 (#4953): every
// kind=workspace workspace had loaded_mcp_tools:[] and an empty schedule grid, and
// core could not answer "did boot-install run, and what did it say" for ANY of
// them. The one class of box whose plugin install could silently produce nothing
// was also the one class core could not see. Three explanations for the symptom
// were proposed and retracted for want of this single fact.
//
// So this endpoint is deliberately the OPPOSITE of the boot-step feed on the two
// axes that caused the blind spot: it is NOT gated on kind, and it PERSISTS.
// Those are contract constants, not local choices —
// molcontracts.PluginInstallReportConciergeGated / …Durable.
//
// LIVENESS IS NOT `installed`. `installed` lists sources that reached the STAGING
// tree; `swapped` says whether that tree was promoted. A partial build is never
// promoted, so installed=[6] with swapped=false means nothing is live. The rule is
// molcontracts.PluginInstallReportOutcomeRule and it is evaluated in exactly one
// place (reportIsLive) so it cannot drift between writer, reader and operator.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.moleculesai.app/sdk/gen/go/molcontracts"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// maxInstallReportSources bounds each of installed/skipped/failed. A workspace
// declares a handful of plugins; a report with thousands of entries is a bug or an
// attack, and either way must not become an unbounded jsonb write. Rejected loudly
// rather than truncated — a silently trimmed report is a lying report.
const maxInstallReportSources = 256

// maxInstallReportSourceLen bounds one source string. Sources are gitea:// URLs or
// bare local names; the cap is generous for both.
const maxInstallReportSourceLen = 512

// pluginInstallReportBody is the wire shape. Field names come from the SDK
// contract (molcontracts.PluginInstallReportField*); the struct tags below MUST
// equal those constants and pluginInstallReportContractTest asserts it, so a
// contract rename cannot silently stop matching the producer.
//
// declared and swapped are POINTERS so "absent" is distinguishable from "false".
// A producer that forgot the field would otherwise be recorded as a definitive
// "nothing was declared", which is exactly the wrong conclusion to store.
type pluginInstallReportBody struct {
	Declared   *bool    `json:"declared"`
	PluginsDir string   `json:"plugins_dir"`
	Installed  []string `json:"installed"`
	Skipped    []string `json:"skipped"`
	Failed     []string `json:"failed"`
	Swapped    *bool    `json:"swapped"`
}

// PluginInstallReportHandler persists the runtime's boot-install outcome.
type PluginInstallReportHandler struct{}

// NewPluginInstallReportHandler constructs the handler.
func NewPluginInstallReportHandler() *PluginInstallReportHandler {
	return &PluginInstallReportHandler{}
}

// reportIsLive is the contract's outcome rule, in the one place it lives:
//
//	live iff declared && swapped && failed == []
//
// Keep it a function even though it is a one-liner — the value of having it named
// is that a caller cannot accidentally write `len(installed) > 0` instead.
func reportIsLive(declared, swapped bool, failed []string) bool {
	return declared && swapped && len(failed) == 0
}

// validateSources bounds one list, returning a reason when it is unusable.
func validateSources(name string, list []string) string {
	if len(list) > maxInstallReportSources {
		return name + " has too many entries"
	}
	for _, s := range list {
		if len(s) > maxInstallReportSourceLen {
			return name + " contains an over-long source"
		}
	}
	return ""
}

// Report handles POST /workspaces/:id/plugin-install-report.
//
// Mounted under wsAuth, so WorkspaceAuth has already bound the bearer to :id — a
// workspace can only report for itself. Returns 204 on success
// (molcontracts.PluginInstallReportSuccessStatus).
//
// The producer is fail-soft by contract: it swallows every error including this
// endpoint's 404 before it exists. That makes a LOUD 4xx here safe and useful —
// it cannot break a boot, and a malformed report is a runtime bug worth surfacing
// rather than persisting as junk.
func (h *PluginInstallReportHandler) Report(c *gin.Context) {
	workspaceID := c.Param("id")
	if _, err := uuid.Parse(workspaceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace id must be a UUID"})
		return
	}

	var body pluginInstallReportBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid plugin-install-report body"})
		return
	}
	if body.Declared == nil || body.Swapped == nil {
		// Not pedantry: absent-vs-false is the difference between "core never asked
		// for a plugin" and "core asked and nothing went live".
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "declared and swapped are required (absent is not the same as false)",
		})
		return
	}
	for _, chk := range []struct {
		name string
		list []string
	}{{"installed", body.Installed}, {"skipped", body.Skipped}, {"failed", body.Failed}} {
		if reason := validateSources(chk.name, chk.list); reason != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": reason})
			return
		}
	}
	if len(body.PluginsDir) > maxInstallReportSourceLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "plugins_dir is too long"})
		return
	}

	if err := persistPluginInstallReport(c.Request.Context(), db.DB, workspaceID, body); err != nil {
		log.Printf("plugin-install-report: persist for %s failed: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not record the report"})
		return
	}

	// One log line at the point of truth, because this is the record an operator
	// greps when a workspace has no plugins. NOT-LIVE is called out explicitly:
	// the counts alone read as success at a glance, which is the whole failure
	// mode this endpoint exists to end.
	if reportIsLive(*body.Declared, *body.Swapped, body.Failed) {
		log.Printf("plugin-install-report: %s LIVE — installed=%d skipped=%d dir=%s",
			workspaceID, len(body.Installed), len(body.Skipped), body.PluginsDir)
	} else {
		log.Printf("plugin-install-report: %s NOT LIVE — declared=%t swapped=%t installed=%d failed=%d (%s)",
			workspaceID, *body.Declared, *body.Swapped, len(body.Installed), len(body.Failed),
			molcontracts.PluginInstallReportOutcomeRule)
	}

	c.Status(molcontracts.PluginInstallReportSuccessStatus)
}

// persistPluginInstallReport upserts the LATEST report for a workspace.
//
// Latest-per-workspace rather than append-only: the question is "what happened on
// the last boot", and a wedged workspace reboots repeatedly, so a log would grow
// without bound and need a retention policy to answer a question the current state
// already answers.
func persistPluginInstallReport(ctx context.Context, database *sql.DB, workspaceID string, body pluginInstallReportBody) error {
	if database == nil {
		return nil // nil in unit tests; the row is test-only there
	}
	installed, err := json.Marshal(nonNilStrings(body.Installed))
	if err != nil {
		return err
	}
	skipped, err := json.Marshal(nonNilStrings(body.Skipped))
	if err != nil {
		return err
	}
	failed, err := json.Marshal(nonNilStrings(body.Failed))
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = database.ExecContext(wctx, `
		INSERT INTO workspace_plugin_install_reports
			(workspace_id, declared, plugins_dir, installed, skipped, failed, swapped, reported_at)
		VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6::jsonb, $7, NOW())
		ON CONFLICT (workspace_id) DO UPDATE SET
			declared    = EXCLUDED.declared,
			plugins_dir = EXCLUDED.plugins_dir,
			installed   = EXCLUDED.installed,
			skipped     = EXCLUDED.skipped,
			failed      = EXCLUDED.failed,
			swapped     = EXCLUDED.swapped,
			reported_at = NOW()
	`, workspaceID, *body.Declared, body.PluginsDir, string(installed), string(skipped), string(failed), *body.Swapped)
	return err
}

// nonNilStrings normalises a nil slice to an empty one so the stored jsonb is
// always `[]` rather than `null`. A reader should never have to distinguish
// "reported no failures" from "reported nothing about failures" — the producer
// always sends all three lists.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// pluginInstallReportRow is the read shape returned by Get and embedded in the
// workspace read.
type pluginInstallReportRow struct {
	Declared   bool      `json:"declared"`
	PluginsDir string    `json:"plugins_dir"`
	Installed  []string  `json:"installed"`
	Skipped    []string  `json:"skipped"`
	Failed     []string  `json:"failed"`
	Swapped    bool      `json:"swapped"`
	ReportedAt time.Time `json:"reported_at"`
	// Live is DERIVED on read via reportIsLive, never stored. A stored copy would
	// be a second place for the outcome rule to live, and the rule is the thing
	// most likely to be got wrong.
	Live bool `json:"live"`
	// OutcomeRule is echoed so a reader looking at live=false learns WHY without
	// having to find the contract.
	OutcomeRule string `json:"outcome_rule"`
}

// loadPluginInstallReport reads the latest report, or (nil, nil) when the
// workspace has never reported — which is itself informative: either it has not
// booted since the reporting runtime shipped, or its report never arrives.
//
// Takes the handle explicitly rather than reaching for db.DB so the round-trip is
// testable against a real Postgres; the handler passes db.DB.
func loadPluginInstallReport(ctx context.Context, database *sql.DB, workspaceID string) (*pluginInstallReportRow, error) {
	if database == nil {
		return nil, nil
	}
	var (
		row                                 pluginInstallReportRow
		installedRaw, skippedRaw, failedRaw []byte
	)
	err := database.QueryRowContext(ctx, `
		SELECT declared, plugins_dir, installed, skipped, failed, swapped, reported_at
		  FROM workspace_plugin_install_reports
		 WHERE workspace_id = $1
	`, workspaceID).Scan(&row.Declared, &row.PluginsDir, &installedRaw, &skippedRaw, &failedRaw,
		&row.Swapped, &row.ReportedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	for _, pair := range []struct {
		raw []byte
		dst *[]string
	}{{installedRaw, &row.Installed}, {skippedRaw, &row.Skipped}, {failedRaw, &row.Failed}} {
		*pair.dst = []string{}
		if len(pair.raw) > 0 {
			if uerr := json.Unmarshal(pair.raw, pair.dst); uerr != nil {
				return nil, uerr
			}
		}
	}
	row.Live = reportIsLive(row.Declared, row.Swapped, row.Failed)
	row.OutcomeRule = molcontracts.PluginInstallReportOutcomeRule
	return &row, nil
}

// Get handles GET /workspaces/:id/plugin-install-report. 404 when the workspace
// has never reported, so "never reported" is not silently rendered as a clean
// empty report — the two mean different things and only one of them is fine.
func (h *PluginInstallReportHandler) Get(c *gin.Context) {
	workspaceID := c.Param("id")
	if _, err := uuid.Parse(workspaceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace id must be a UUID"})
		return
	}
	row, err := loadPluginInstallReport(c.Request.Context(), db.DB, workspaceID)
	if err != nil {
		log.Printf("plugin-install-report: read for %s failed: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not read the report"})
		return
	}
	if row == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "this workspace has never reported a plugin boot-install",
			"hint":  "either it has not booted since the reporting runtime shipped, or its report is not arriving",
		})
		return
	}
	c.JSON(http.StatusOK, row)
}

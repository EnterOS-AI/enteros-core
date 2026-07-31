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
// tree; `swapped` says whether that tree was promoted. A tree that was never
// promoted is not live no matter how much reached staging, so installed=[6] with
// swapped=false means nothing is live. The rule is
// molcontracts.PluginInstallReportOutcomeRule and it is evaluated in exactly one
// place (reportIsLive) so it cannot drift between writer, reader and operator.
//
// LIVENESS IS ALSO NOT `failed == []`. The runtime PROMOTES PARTIAL BUILDS by
// design — molecule_runtime/plugin_sources.py, "A failed source fails THAT SOURCE
// — not the whole tree", added after the staging test5 incident of 2026-07-13
// where one unfetchable third-party plugin vetoed the swap and took the
// concierge's own management MCP down with it. On a partial failure the runtime
// carries every live dir the successful sources are not replacing into staging,
// logs "promoting the N that succeeded", swaps, and sets swapped=True with a
// NON-EMPTY failed list. Folding `failed == []` into liveness therefore reports a
// workspace with 5 of 6 plugins live and one flaky gitea fetch as live=false — a
// false alarm on a healthy box, which is the inverse of the lie this endpoint was
// built to end. A non-empty `failed` on a promoted tree is a SEPARATE, weaker
// signal: `degraded` (see reportIsDegraded).

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

// maxInstallReportBody caps the request body BEFORE the decoder sees it.
//
// The two bounds above are checked AFTER ShouldBindJSON has already materialised
// the whole body in memory, so on their own they bound what gets STORED and not
// what gets ALLOCATED. This group is mounted under wsAuth, which also accepts the
// ADMIN_TOKEN and an org key (wsauth_middleware.go), so "the caller is a
// workspace" is not a size bound either. Every sibling caps first —
// config.go's maxConfigBody, plugins_install.go's bodyMax.
//
// 512 KiB is DERIVED, not picked: the largest report the bounds above admit is
// three lists of maxInstallReportSources entries at maxInstallReportSourceLen
// bytes each, ~396 KiB of JSON with quoting and commas, plus a bounded
// plugins_dir. The cap must exceed that or it would reject a legal report with a
// 413 that the bounds would have accepted — TestPluginInstallReport_BodyCap*
// pins both directions of that inequality so tightening one bound cannot
// silently invalidate the other.
const maxInstallReportBody = 512 << 10

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
//	live iff declared && swapped
//
// `swapped` is the whole of it: the runtime sets it only after _atomic_swap_dir
// renamed the staging tree over plugins_dir, so swapped=true means the tree an
// agent loads from IS the tree this report describes. `failed` deliberately does
// NOT appear — the runtime promotes partial builds (see the file header), so a
// failed source subtracts from WHAT is live without making the promotion not have
// happened. That distinction is `degraded`, below.
//
// This is also what makes the migration's partial index — WHERE declared AND NOT
// swapped — the exact complement of live, rather than a near-miss that quietly
// disagrees with the API.
//
// Keep it a function even though it is a one-liner — the value of having it named
// is that a caller cannot accidentally write `len(installed) > 0` instead.
func reportIsLive(declared, swapped bool) bool {
	return declared && swapped
}

// reportIsDegraded is the signal `failed` actually carries once liveness stops
// swallowing it: the tree WAS promoted, and it is missing something that was asked
// for. Plugins are live; not all of them.
//
// Scoped to live reports on purpose. On a report that never promoted, `failed` is
// not a partial-coverage warning — it is one of the reasons nothing went live at
// all, and calling that "degraded" would soften a hard failure into a caveat.
//
// Derived on read like Live, never stored: a stored copy is a second place for a
// rule to drift, and this rule has now been got wrong once already.
func reportIsDegraded(declared, swapped bool, failed []string) bool {
	return reportIsLive(declared, swapped) && len(failed) > 0
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

	// Cap BEFORE the bind. maxInstallReportSources/…SourceLen below only fire once
	// the decoder has already built the whole body in memory, so they cannot bound
	// the allocation — only the write.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxInstallReportBody)

	var body pluginInstallReportBody
	if err := c.ShouldBindJSON(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			// Distinct from the 400 below on purpose: "too big" and "malformed" are
			// different runtime bugs, and a producer that hits this needs to know it
			// was cut off rather than mis-serialised.
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "plugin-install-report body is too large"})
			return
		}
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
	// mode this endpoint exists to end. DEGRADED is called out separately because
	// it is the opposite hazard — it must NOT read as an outage, or the operator
	// learns to ignore the line that one day means something.
	switch {
	case reportIsDegraded(*body.Declared, *body.Swapped, body.Failed):
		log.Printf("plugin-install-report: %s LIVE (DEGRADED) — installed=%d skipped=%d failed=%d dir=%s",
			workspaceID, len(body.Installed), len(body.Skipped), len(body.Failed), body.PluginsDir)
	case reportIsLive(*body.Declared, *body.Swapped):
		log.Printf("plugin-install-report: %s LIVE — installed=%d skipped=%d dir=%s",
			workspaceID, len(body.Installed), len(body.Skipped), body.PluginsDir)
	default:
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

// pluginInstallReportRow is the read shape returned by Get.
//
// It is NOT embedded in the workspace read. That claim used to be made here and
// in the SDK contract's `consumers` entry, and it was never true — nothing
// outside Get calls loadPluginInstallReport. Because the contract is the
// cross-repo SSOT, the false promise was what the next consumer would have
// built against: a canvas/CP workspace-detail panel wired to a
// `plugin_install_report` field on GET /workspaces/:id that does not exist, and
// which would therefore have shipped permanently empty. molecule-ai-sdk#190
// corrected the contract side; this corrects its twin here.
//
// If the workspace-read embed is wanted, it is a real feature to implement, not
// a comment to restore.
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
	// Degraded is DERIVED on read via reportIsDegraded, likewise never stored. It
	// is the signal that used to be folded into Live and lost there: the tree was
	// promoted AND something declared is missing from it. live=true degraded=true
	// is the runtime's partial-promotion outcome — plugins are running, and a
	// source needs looking at. It is not an outage and must not be paged on as one.
	Degraded bool `json:"degraded"`
	// OutcomeRule is echoed so a reader looking at live=false learns WHY without
	// having to find the contract.
	//
	// omitempty is for the FLEET read (admin_plugin_install_reports.go), which
	// embeds this struct and echoes the rule once on the envelope instead of
	// repeating it on every row. Get always sets it, so its response shape is
	// unchanged — there is no path where Get emits an empty outcome_rule.
	OutcomeRule string `json:"outcome_rule,omitempty"`
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
	row.Live = reportIsLive(row.Declared, row.Swapped)
	row.Degraded = reportIsDegraded(row.Declared, row.Swapped, row.Failed)
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

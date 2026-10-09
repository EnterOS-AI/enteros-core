package handlers

// admin_plugin_install_reports.go — the FLEET read over the plugin-install-report
// table (molecule-core#4981 §1).
//
// WHY THIS EXISTS
//
// The migration that created workspace_plugin_install_reports also created a
// partial index whose stated purpose, verbatim in the migration comment, is the
// fleet question:
//
//	"which workspaces booted with no live plugins"
//
//	CREATE INDEX … workspace_plugin_install_reports_not_live
//	  ON workspace_plugin_install_reports(reported_at DESC)
//	  WHERE declared AND NOT swapped;
//
// Nothing queried it. The only readers core shipped were POST (Report) and the
// single-workspace GET (Get) — both keyed by workspace id, both served by the
// primary key. So the operator question the whole chain was built to answer still
// required direct DB access, and the next #4953-class incident would have been
// diagnosed the same slow way it was the first time: by hand, against the tenant
// database, by the one person who can reach it.
//
// THE TWO CLASSES, AND WHY ONE IS NOT ENOUGH
//
// `not_live` is the index's predicate. Since core#4972 corrected liveness to
// `declared && swapped` (reportIsLive), `declared AND NOT swapped` is the EXACT
// complement of live over declared rows, so the index serves this arm precisely
// rather than approximately.
//
// `degraded` is the class that arm CANNOT see, by construction. The runtime
// deliberately promotes PARTIAL builds — molecule_runtime/plugin_sources.py, "A
// failed source fails THAT SOURCE — not the whole tree", carrying the previous
// dirs forward, logging "promoting the N that succeeded", then swapping. Such a
// box reports declared=true swapped=true failed=["gitea://…"]: it is LIVE, and it
// is missing a plugin somebody asked for. A fleet sweep that only asks "who is not
// live" reports that box as healthy and the missing plugin is never looked at.
// That is the same blind spot as the original, one level in.
//
// So the endpoint serves both, and keeps them SEPARATE rather than summing them
// into a "problems" count: live=false is an outage, degraded=true is a flaky
// source. Merging them re-creates exactly the conflation that #4972 removed from
// the liveness rule.
//
// WHAT IT IS NOT
//
// Not the workspace-read embed (#4981 §2). That is an open decision, and it is now
// honestly documented as absent in both the SDK contract and pluginInstallReportRow.
//
// AUTH — admin, not workspace. The POST/GET pair is mounted under wsAuth, where
// WorkspaceAuth binds the bearer to :id so a workspace can only see itself. This
// route returns rows for workspaces the caller never names, so a workspace-scoped
// token must not reach it: it is registered under middleware.AdminAuth alongside
// /admin/delegations, /admin/schedules/health and /admin/plugin-updates-pending —
// the existing operator-dashboard reads of exactly this shape.

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.moleculesai.app/sdk/gen/go/molcontracts"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// AdminPluginInstallReportsHandler serves the fleet read.
type AdminPluginInstallReportsHandler struct {
	db *sql.DB
}

// NewAdminPluginInstallReportsHandler constructs the handler. Takes the handle
// explicitly — like NewAdminDelegationsHandler — so the query is testable against
// a real Postgres rather than only through the package-global db.DB.
func NewAdminPluginInstallReportsHandler(handle *sql.DB) *AdminPluginInstallReportsHandler {
	if handle == nil {
		handle = db.DB
	}
	return &AdminPluginInstallReportsHandler{db: handle}
}

// fleetPluginInstallReportRow is one row of the fleet read: the single-workspace
// read shape, plus the identity a fleet caller does not already know.
//
// The embedded pluginInstallReportRow is deliberate — the fleet view and the
// single-workspace view must not be able to describe the same report differently,
// and a second row struct is how that starts. Live/Degraded are derived through
// the same reportIsLive/reportIsDegraded helpers here as there.
//
// WorkspaceName is joined in because "which workspaces" answered with a bare UUID
// makes the operator do a second lookup per row, which is most of the reason
// direct-DB triage felt faster than an API in the first place.
type fleetPluginInstallReportRow struct {
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name"`
	pluginInstallReportRow
}

// fleetReportFilters maps the query-string `status` value to its SQL predicate.
//
// An allowlist, not free-form SQL, and deliberately NOT extended with an
// "everything" option: an unfiltered fleet listing has no index behind it and
// answers no question an operator actually asked. Both predicates below are
// index-or-narrow by construction.
//
// notLiveFleetPredicate is written to MATCH THE PARTIAL INDEX CHARACTER FOR
// CHARACTER. A predicate that is merely equivalent (say `NOT (declared AND
// swapped) AND declared`) is not guaranteed to be recognised by the planner as
// implying the index's WHERE clause, and the whole point of this arm is to use
// that index.
const notLiveFleetPredicate = `declared AND NOT swapped`

// degradedFleetPredicate is the contract's degraded_rule in SQL: live (declared
// AND swapped) AND failed != [].
//
// `failed <> '[]'::jsonb` rather than `jsonb_array_length(failed) > 0` on purpose:
// jsonb_array_length ERRORS on a non-array, and SQL does not promise that a
// guarding jsonb_typeof() in the same AND chain is evaluated first. The writer
// always stores an array (nonNilStrings + a '[]' default), so the two agree on
// every real row — but one of them cannot throw on a row that got in another way.
//
// This predicate and reportIsDegraded are two expressions of one rule, which is a
// drift risk. TestIntegration_AdminPluginInstallReports_SQLFilterAgreesWithTheGoRule
// pins them to each other over the whole input space rather than trusting that
// they were written by the same person on the same day.
const degradedFleetPredicate = `declared AND swapped AND failed <> '[]'::jsonb`

var fleetReportFilters = map[string]string{
	"not_live": notLiveFleetPredicate,
	"degraded": degradedFleetPredicate,
}

// fleetReportFilterKeys returns the accepted `?status=` values, sorted so the
// error message is stable. DERIVED from the map — a hand-typed list is how an
// error message ends up disagreeing with what the endpoint accepts.
func fleetReportFilterKeys() []string {
	keys := make([]string, 0, len(fleetReportFilters))
	for k := range fleetReportFilters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// defaultListLimit / maxListLimit are the package's existing bounds (see
// admin_delegations.go). Reused rather than redeclared so the two operator reads
// cannot drift into different caps.

// fleetReportQuery builds the fleet SELECT for one allowlisted predicate.
//
// A function rather than an inline string so the EXPLAIN test can plan THE query
// the handler runs. An index-usage test that EXPLAINs a hand-retyped copy of the
// SQL proves the copy is served by the index, which is not the claim being made.
//
// `predicate` is always one of the fleetReportFilters constants — never a
// caller-supplied string. `limit` is bound as $1.
//
// ORDER BY r.reported_at DESC is not cosmetic: it is the not_live index's own sort
// order, so the planner can walk the partial index instead of sorting a filtered
// heap scan. It is also the order an operator wants — the most recent boot failure
// is the one being investigated.
// degradedFleetRelation is the SET OF ROWS every fleet-report question is asked
// about: install-report rows joined to a workspace that still EXISTS.
//
// It is a constant, and both readers are built from it, because the two used to
// be written out separately (core#5025 finding 8). The gauge counted
// workspace_plugin_install_reports alone while this endpoint counted the join,
// and the two answers diverged in the one way nobody checks: deletes here are
// SOFT (workspace_crud writes status='removed' and leaves the row), so the
// reports table's ON DELETE CASCADE never fires and a deleted workspace's report
// row satisfies the predicate forever. The gauge could not return to zero, so
// its alert could not clear, while the endpoint the alert sends an operator to
// listed fewer workspaces than the number that paged them.
//
// `w.status <> 'removed'` is what makes "Live workspaces" — the metric's own HELP
// text and this endpoint's whole purpose — true rather than aspirational. Every
// other status (online, degraded, provisioning, paused, hibernated, failed) is a
// workspace that still exists and whose missing plugin still matters.
const degradedFleetRelation = `workspace_plugin_install_reports r
		  JOIN workspaces w ON w.id = r.workspace_id AND w.status <> 'removed'`

func fleetReportQuery(predicate string) string {
	return `
		SELECT r.workspace_id::text, w.name, r.declared, r.plugins_dir,
		       r.installed, r.skipped, r.failed, r.swapped, r.reported_at
		  FROM ` + degradedFleetRelation + `
		 WHERE ` + predicate + `
		 ORDER BY r.reported_at DESC
		 LIMIT $1`
}

// List handles GET /admin/plugin-install-reports.
//
// Query params:
//   - status — `not_live` (default) or `degraded`
//   - limit  — int, 1..1000 (default 100)
//
// Returns 200 with {"reports": [...], "count": N, "status": …, "limit": …,
// "truncated": bool, "outcome_rule": …, "degraded_rule": …}.
//
// `truncated` is reported rather than left for the caller to infer: a fleet read
// that silently stops at the cap is a fleet read that tells an operator the
// outage is smaller than it is.
func (h *AdminPluginInstallReportsHandler) List(c *gin.Context) {
	statusKey := c.DefaultQuery("status", "not_live")
	predicate, ok := fleetReportFilters[statusKey]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":            "unknown status filter",
			"allowed":          fleetReportFilterKeys(),
			"requested_status": statusKey,
		})
		return
	}

	limit := defaultListLimit
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListLimit {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":     "limit must be 1..1000",
				"requested": v,
			})
			return
		}
		limit = n
	}

	if h.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database unavailable"})
		return
	}

	rows, err := h.db.QueryContext(c.Request.Context(), fleetReportQuery(predicate), limit)
	if err != nil {
		log.Printf("AdminPluginInstallReports.List: query failed (status=%s): %v", statusKey, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	out := make([]fleetPluginInstallReportRow, 0)
	for rows.Next() {
		var (
			r                                   fleetPluginInstallReportRow
			installedRaw, skippedRaw, failedRaw []byte
			reportedAt                          time.Time
		)
		if err := rows.Scan(&r.WorkspaceID, &r.WorkspaceName, &r.Declared, &r.PluginsDir,
			&installedRaw, &skippedRaw, &failedRaw, &r.Swapped, &reportedAt); err != nil {
			log.Printf("AdminPluginInstallReports.List: scan failed: %v", err)
			continue
		}
		r.ReportedAt = reportedAt
		decodeErr := false
		for _, pair := range []struct {
			raw []byte
			dst *[]string
		}{{installedRaw, &r.Installed}, {skippedRaw, &r.Skipped}, {failedRaw, &r.Failed}} {
			*pair.dst = []string{}
			if len(pair.raw) > 0 {
				if uerr := json.Unmarshal(pair.raw, pair.dst); uerr != nil {
					// Skip the row rather than serve it with an empty `failed`: an
					// unreadable failed list rendered as [] would report a degraded
					// box as clean, which is the lie this endpoint exists to end.
					log.Printf("AdminPluginInstallReports.List: %s has an undecodable list: %v",
						r.WorkspaceID, uerr)
					decodeErr = true
					break
				}
			}
		}
		if decodeErr {
			continue
		}
		// Derived on read through the SAME helpers as the single-workspace read.
		// Never stored, never recomputed inline here.
		r.Live = reportIsLive(r.Declared, r.Swapped)
		r.Degraded = reportIsDegraded(r.Declared, r.Swapped, r.Failed)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		log.Printf("AdminPluginInstallReports.List: rows.Err: %v", err)
	}

	c.JSON(http.StatusOK, gin.H{
		"reports": out,
		"count":   len(out),
		"status":  statusKey,
		"limit":   limit,
		// The cap was reached, so there may be more. Said out loud because the
		// alternative is an operator reading `count: 100` as "100 broken boxes".
		"truncated": len(out) == limit,
		// Echoed so a reader looking at live=false / degraded=true learns WHY
		// without having to go find the contract, exactly as the single-workspace
		// read does. Both come from the SDK gen module, never hand-written.
		"outcome_rule":  molcontracts.PluginInstallReportOutcomeRule,
		"degraded_rule": molcontracts.PluginInstallReportDegradedRule,
	})
}
